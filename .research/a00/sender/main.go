// Throwaway A00 measurement sender (accreleus/quasar#1025). Never merges to the product.
//
// Listens for WebTransport on UDP, makes its own self-signed ECDSA P-256 cert (10 days,
// persisted so the hash survives a restart), and on request paces sequence-numbered
// datagrams or per-16ms-frame unidirectional streams at a given rate.
//
// Control: the client opens the first bidirectional stream and writes one JSON line:
//
//	{"rate":100,"carriage":"datagram","duration":10,"size":1150}
//
// The sender replies with one JSON summary line. Several runs may share a session.
package main

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"
	"github.com/quic-go/webtransport-go"
)

type request struct {
	Rate     float64 `json:"rate"` // Mbit/s
	Carriage string  `json:"carriage"`
	Duration float64 `json:"duration"` // s
	Size     int     `json:"size"`     // datagram payload bytes (<=1150)
}

type summary struct {
	Carriage       string  `json:"carriage"`
	TargetMbps     float64 `json:"target_mbps"`
	Packets        int64   `json:"packets"` // datagrams, or frames for stream carriage
	Bytes          int64   `json:"bytes"`
	ElapsedS       float64 `json:"elapsed_s"`
	AchievedMbps   float64 `json:"achieved_mbps"`
	PaceErrMeanUs  float64 `json:"pace_err_mean_us"` // actual send time minus scheduled time
	PaceErrMaxUs   float64 `json:"pace_err_max_us"`
	SendErrors     int64   `json:"send_errors"`
	FramesSkipped  int64   `json:"frames_skipped,omitempty"` // stream carriage: >64 frames in flight
	CPUSeconds     float64 `json:"cpu_s"`                    // whole process, user+sys, during the run
	CPUPercentCore float64 `json:"cpu_pct_of_one_core"`
	DatagramMax    int     `json:"datagram_max_payload,omitempty"`
}

func cpuNow() float64 {
	var ru syscall.Rusage
	_ = syscall.Getrusage(syscall.RUSAGE_SELF, &ru)
	return float64(ru.Utime.Sec) + float64(ru.Utime.Usec)/1e6 + float64(ru.Stime.Sec) + float64(ru.Stime.Usec)/1e6
}

func loadOrMakeCert(dir string, ips []net.IP) (tls.Certificate, [32]byte, time.Time) {
	certPath, keyPath := filepath.Join(dir, "cert.der"), filepath.Join(dir, "key.der")
	if cb, err := os.ReadFile(certPath); err == nil {
		if kb, err := os.ReadFile(keyPath); err == nil {
			if c, err := x509.ParseCertificate(cb); err == nil && time.Until(c.NotAfter) > 24*time.Hour {
				if k, err := x509.ParseECPrivateKey(kb); err == nil {
					return tls.Certificate{Certificate: [][]byte{cb}, PrivateKey: k}, sha256.Sum256(cb), c.NotAfter
				}
			}
		}
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		log.Fatal(err)
	}
	now := time.Now()
	tmpl := x509.Certificate{
		SerialNumber:          big.NewInt(now.UnixNano()),
		Subject:               pkix.Name{CommonName: "a00-wt-sender"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(10*24*time.Hour - time.Hour), // 10 days total validity, under the 14-day browser cap
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              []string{"localhost"},
		IPAddresses:           append([]net.IP{net.IPv4(127, 0, 0, 1)}, ips...),
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		log.Fatal(err)
	}
	kb, _ := x509.MarshalECPrivateKey(key)
	_ = os.MkdirAll(dir, 0o700)
	if err := os.WriteFile(certPath, der, 0o600); err != nil {
		log.Fatal(err)
	}
	if err := os.WriteFile(keyPath, kb, 0o600); err != nil {
		log.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, sha256.Sum256(der), tmpl.NotAfter
}

func lanIPs() []net.IP {
	var out []net.IP
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		if ipn, ok := a.(*net.IPNet); ok && ipn.IP.To4() != nil && !ipn.IP.IsLoopback() {
			out = append(out, ipn.IP)
		}
	}
	return out
}

// pace calls send(n) for n = 0,1,... at 'interval' spacing until 'dur' has elapsed, and
// returns how many sends happened plus the pacing error (actual minus scheduled).
func pace(interval, dur time.Duration, send func(n int64) bool) (sent int64, errMean, errMax time.Duration, elapsed time.Duration) {
	start := time.Now()
	var errSum time.Duration
	for {
		el := time.Since(start)
		if el >= dur {
			return sent, mean(errSum, sent), errMax, el
		}
		due := int64(el/interval) + 1
		for sent < due {
			sched := time.Duration(sent) * interval
			if !send(sent) {
				return sent, mean(errSum, sent), errMax, time.Since(start)
			}
			e := time.Since(start) - sched
			errSum += e
			if e > errMax {
				errMax = e
			}
			sent++
			if sent%64 == 0 {
				break // yield to the clock check rather than burst without bound
			}
		}
		if next := time.Duration(sent) * interval; next-time.Since(start) > 80*time.Microsecond {
			time.Sleep(next - time.Since(start) - 40*time.Microsecond)
		}
	}
}

func mean(sum time.Duration, n int64) time.Duration {
	if n == 0 {
		return 0
	}
	return sum / time.Duration(n)
}

func runDatagrams(sess *webtransport.Session, r request) summary {
	buf := make([]byte, r.Size)
	buf[0] = 0xA0
	bits := r.Rate * 1e6
	interval := time.Duration(float64(r.Size*8) / bits * float64(time.Second))
	var errs int64
	cpu0 := cpuNow()
	n, em, ex, el := pace(interval, time.Duration(r.Duration*float64(time.Second)), func(n int64) bool {
		binary.LittleEndian.PutUint32(buf[1:], uint32(n))
		if err := sess.SendDatagram(buf); err != nil {
			errs++
			return sess.Context().Err() == nil && errs < 1000
		}
		return true
	})
	cpu := cpuNow() - cpu0
	return summary{Carriage: "datagram", TargetMbps: r.Rate, Packets: n, Bytes: n * int64(r.Size), ElapsedS: el.Seconds(),
		AchievedMbps: float64(n*int64(r.Size)*8) / el.Seconds() / 1e6, PaceErrMeanUs: us(em), PaceErrMaxUs: us(ex),
		SendErrors: errs, CPUSeconds: cpu, CPUPercentCore: 100 * cpu / el.Seconds()}
}

func us(d time.Duration) float64 { return float64(d) / 1e3 }

func runStreams(sess *webtransport.Session, r request) summary {
	frame := time.Duration(16 * time.Millisecond)
	body := int(r.Rate * 1e6 / 8 * frame.Seconds())
	if body < 12 {
		body = 12
	}
	chunk := make([]byte, 64*1024)
	var inflight, skipped, errs, bytes atomic.Int64
	var wg sync.WaitGroup
	cpu0 := cpuNow()
	n, em, ex, el := pace(frame, time.Duration(r.Duration*float64(time.Second)), func(n int64) bool {
		if inflight.Load() >= 64 {
			skipped.Add(1)
			return true
		}
		inflight.Add(1)
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer inflight.Add(-1)
			ctx, cancel := context.WithTimeout(sess.Context(), 5*time.Second)
			defer cancel()
			s, err := sess.OpenUniStreamSync(ctx)
			if err != nil {
				errs.Add(1)
				return
			}
			var hdr [12]byte
			binary.LittleEndian.PutUint32(hdr[0:], uint32(n))
			binary.LittleEndian.PutUint32(hdr[4:], uint32(body))
			if _, err := s.Write(hdr[:]); err != nil {
				errs.Add(1)
				return
			}
			for left := body - 12; left > 0; {
				k := min(left, len(chunk))
				if _, err := s.Write(chunk[:k]); err != nil {
					errs.Add(1)
					s.CancelWrite(1)
					return
				}
				left -= k
			}
			bytes.Add(int64(body))
			_ = s.Close()
		}()
		return sess.Context().Err() == nil
	})
	wg.Wait()
	cpu := cpuNow() - cpu0
	return summary{Carriage: "stream", TargetMbps: r.Rate, Packets: n - skipped.Load(), Bytes: bytes.Load(), ElapsedS: el.Seconds(),
		AchievedMbps: float64(bytes.Load()*8) / el.Seconds() / 1e6, PaceErrMeanUs: us(em), PaceErrMaxUs: us(ex),
		SendErrors: errs.Load(), FramesSkipped: skipped.Load(), CPUSeconds: cpu, CPUPercentCore: 100 * cpu / el.Seconds()}
}

func handle(sess *webtransport.Session) {
	ctx := sess.Context()
	st, err := sess.AcceptStream(ctx)
	if err != nil {
		return
	}
	rd := bufio.NewReader(st)
	for {
		line, err := rd.ReadBytes('\n')
		if err != nil {
			break
		}
		var r request
		if err := json.Unmarshal(line, &r); err != nil || r.Rate <= 0 || r.Duration <= 0 || r.Duration > 120 || r.Rate > 2000 {
			fmt.Fprintf(st, "{\"error\":\"bad request\"}\n")
			continue
		}
		if r.Size <= 12 || r.Size > 1150 {
			r.Size = 1150
		}
		log.Printf("run: %s %.0f Mbit/s %.0fs size=%d from %s", r.Carriage, r.Rate, r.Duration, r.Size, sess.RemoteAddr())
		var s summary
		if r.Carriage == "stream" {
			s = runStreams(sess, r)
		} else {
			s = runDatagrams(sess, r)
		}
		b, _ := json.Marshal(s)
		log.Printf("done: %s", b)
		// Give the tail of the run a moment to drain before the summary races it on the control stream.
		time.Sleep(300 * time.Millisecond)
		_, _ = st.Write(append(b, '\n'))
	}
	_ = sess.CloseWithError(0, "bye")
}

func main() {
	addr := flag.String("addr", ":4433", "UDP listen address")
	state := flag.String("state", filepath.Join(os.Getenv("HOME"), ".local/state/a00-wt"), "cert state dir")
	flag.Parse()

	cert, hash, notAfter := loadOrMakeCert(*state, lanIPs())
	_, port, _ := net.SplitHostPort(*addr)
	fmt.Printf("cert-hash-base64 %s\ncert-hash-hex %s\ncert-not-after %s\n", base64.StdEncoding.EncodeToString(hash[:]), hex.EncodeToString(hash[:]), notAfter.UTC().Format(time.RFC3339))
	for _, ip := range lanIPs() {
		fmt.Printf("url https://%s:%s/wt\n", ip, port)
	}

	h3 := &http3.Server{
		Addr:       *addr,
		TLSConfig:  http3.ConfigureTLSConfig(&tls.Config{Certificates: []tls.Certificate{cert}}),
		QUICConfig: &quic.Config{EnableDatagrams: true, EnableStreamResetPartialDelivery: true, MaxIdleTimeout: 30 * time.Second, InitialStreamReceiveWindow: 4 << 20, MaxStreamReceiveWindow: 16 << 20, InitialConnectionReceiveWindow: 16 << 20, MaxConnectionReceiveWindow: 64 << 20},
	}
	webtransport.ConfigureHTTP3Server(h3)
	mux := http.NewServeMux()
	h3.Handler = mux
	s := webtransport.Server{H3: h3, CheckOrigin: func(*http.Request) bool { return true }} // file:// pages send Origin: null
	mux.HandleFunc("/wt", func(w http.ResponseWriter, r *http.Request) {
		sess, err := s.Upgrade(w, r)
		if err != nil {
			log.Printf("upgrade: %v", err)
			w.WriteHeader(500)
			return
		}
		go handle(sess)
	})
	log.Printf("listening udp %s", *addr)
	log.Fatal(s.ListenAndServe())
}
