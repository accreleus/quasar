package main

import (
	"context"
	"math"
	"net"
	"testing"
	"time"
)

var t0 = time.Unix(1_000_000, 0)

func at(msec float64) time.Time { return t0.Add(ms(msec)) }

func shaper(t *testing.T, sh Shape) *Shaper {
	t.Helper()
	if err := sh.Validate(); err != nil {
		t.Fatal(err)
	}
	s := NewShaper(1)
	s.Apply(t0, sh)
	return s
}

// A 1200-byte packet every 1 ms is 9.6 Mbit/s offered; a 5 Mbit/s cap must pass about 5 Mbit/s once the burst is spent.
func TestRateCapPassesConfiguredRate(t *testing.T) {
	s := shaper(t, Shape{RateBps: 5_000_000, BurstBytes: 15360, LimitBytes: 1 << 30})
	var last time.Time
	var bytes int
	for i := 0; i < 10_000; i++ { // 10 s
		rel, why := s.Offer(at(float64(i)), 1200)
		if why != "" {
			t.Fatalf("pkt %d dropped (%s) with an unbounded queue", i, why)
		}
		last = rel
		bytes += 1200
	}
	bps := float64(bytes-15360) * 8 / last.Sub(t0).Seconds()
	if math.Abs(bps-5e6)/5e6 > 0.01 {
		t.Fatalf("drain rate %.0f bit/s, want 5e6 +-1%%", bps)
	}
}

// tc: limit = rate*latency + burst. A same-instant flood passes the burst, queues up to the limit and drops the rest.
func TestQueueLimitFromLatency(t *testing.T) {
	sh := Shape{RateBps: 5_000_000, BurstBytes: 15360, LatencyMs: 10}
	if got, want := sh.limit(), int64(625_000*0.01)+15360; got != want {
		t.Fatalf("limit %d, want %d", got, want)
	}
	s := shaper(t, sh)
	var passed, dropped int
	for i := 0; i < 100; i++ {
		if _, why := s.Offer(t0, 1200); why == "" {
			passed++
		} else if why == DropQueue {
			dropped++
		} else {
			t.Fatalf("unexpected drop %s", why)
		}
	}
	if tot := int(sh.limit() + sh.BurstBytes); passed*1200 > tot || (passed+1)*1200 <= tot { // burst leaves at once, then the queue fills
		t.Fatalf("passed %d pkts (%d B) against burst+limit of %d B", passed, passed*1200, tot)
	}
	if passed+dropped != 100 || dropped == 0 {
		t.Fatalf("passed %d dropped %d", passed, dropped)
	}
	c, qb, _ := s.Snapshot(t0)
	if c.Drop[DropQueue].Pkts != int64(dropped) || qb == 0 {
		t.Fatalf("counters %+v backlog %d", c.Drop, qb)
	}
}

// Under steady overload the queueing delay stays within latency + burst time, so a deep and a shallow buffer differ.
func TestQueueDelayBounded(t *testing.T) {
	for _, lat := range []float64{10, 500} {
		sh := Shape{RateBps: 5_000_000, BurstBytes: 15360, LatencyMs: lat}
		s := shaper(t, sh)
		var worst float64
		for i := 0; i < 5000; i++ {
			now := at(float64(i))
			if rel, why := s.Offer(now, 1200); why == "" {
				worst = max(worst, float64(rel.Sub(now))/float64(time.Millisecond))
			}
		}
		if bound := lat + 15360.0/625; worst > bound || worst < lat*0.8 {
			t.Fatalf("latency %v ms: worst queue delay %.1f ms, bound %.1f", lat, worst, bound)
		}
	}
}

// A policer never delays: 10 Mbit/s offered for 4 s against 5 Mbit/s + 15 kB burst passes ~ rate*4 s + burst.
func TestPolicerBurstThenRate(t *testing.T) {
	s := shaper(t, Shape{PoliceBps: 5_000_000, PoliceBurstBytes: 15360})
	var passed int
	for i := 0; i < 4000; i++ {
		now := at(float64(i))
		rel, why := s.Offer(now, 1250) // 10 Mbit/s
		if why == "" {
			passed += 1250
			if !rel.Equal(now) {
				t.Fatal("policer delayed a packet")
			}
		} else if why != DropPolice {
			t.Fatal(why)
		}
	}
	want := 625_000.0*4 + 15360
	if math.Abs(float64(passed)-want)/want > 0.02 {
		t.Fatalf("passed %d B, want ~%.0f", passed, want)
	}
	// the first burst's worth goes through at once
	s = shaper(t, Shape{PoliceBps: 5_000_000, PoliceBurstBytes: 15360})
	n := 0
	for i := 0; i < 100; i++ {
		if _, why := s.Offer(t0, 1200); why == "" {
			n++
		}
	}
	if n != 12 { // 15360 / 1200
		t.Fatalf("instant burst passed %d pkts, want 12", n)
	}
}

func TestDelayJitterNeverReordersUnlessAsked(t *testing.T) {
	for _, reorder := range []bool{false, true} {
		s := shaper(t, Shape{DelayMs: 20, JitterMs: 15, Reorder: reorder})
		var prev time.Time
		inv := 0
		for i := 0; i < 5000; i++ {
			now := at(float64(i) * 0.5)
			rel, why := s.Offer(now, 1200)
			if why != "" {
				t.Fatal(why)
			}
			if d := rel.Sub(now); !reorder && d < 0 || d < 0 || d > ms(35)+time.Microsecond {
				t.Fatalf("delay %v outside 5..35 ms", d)
			}
			if rel.Before(prev) {
				inv++
			}
			prev = rel
		}
		if !reorder && inv != 0 {
			t.Fatalf("%d inversions with reorder off", inv)
		}
		if reorder && inv < 100 {
			t.Fatalf("only %d inversions with reorder on", inv)
		}
	}
}

// R8: 20 -> 80 ms mid-stream and back. Down must not reorder (it compresses), and the steady delays are right.
func TestDelayStepKeepsOrder(t *testing.T) {
	s := shaper(t, Shape{DelayMs: 20})
	var prev time.Time
	for i := 0; i < 3000; i++ {
		now := at(float64(i))
		switch i {
		case 1000:
			s.Apply(now, Shape{DelayMs: 80})
		case 2000:
			s.Apply(now, Shape{DelayMs: 20})
		}
		rel, _ := s.Offer(now, 1200)
		if rel.Before(prev) {
			t.Fatalf("pkt %d reordered across a delay step", i)
		}
		prev = rel
		if d := rel.Sub(now); (i == 999 && d != ms(20)) || (i == 1999 && d != ms(80)) {
			t.Fatalf("pkt %d delay %v", i, d)
		}
	}
}

func TestRandomLossRate(t *testing.T) {
	for _, pct := range []float64{3, 6} {
		s := shaper(t, Shape{LossPct: pct})
		lost := 0
		const n = 200_000
		for i := 0; i < n; i++ {
			if _, why := s.Offer(at(float64(i)), 1200); why == DropLoss {
				lost++
			}
		}
		if got := float64(lost) / n * 100; math.Abs(got-pct) > 0.15 {
			t.Fatalf("loss %.3f%%, want %v%%", got, pct)
		}
	}
}

func TestBurstScheduleAndOutage(t *testing.T) {
	s := shaper(t, Shape{Bursts: &Bursts{DropMs: 150, Count: 3, EveryMs: 10_000}})
	cases := []struct {
		ms   float64
		drop bool
	}{{0, true}, {149, true}, {150, false}, {5000, false}, {9999, false}, {10_000, true}, {10_149, true}, {10_150, false}, {20_100, true}, {20_150, false}, {30_000, false}}
	for _, c := range cases {
		_, why := s.Offer(at(c.ms), 1200)
		if (why == DropBlackout) != c.drop {
			t.Fatalf("t=%vms: reason %q, want drop=%v", c.ms, why, c.drop)
		}
	}
	s = shaper(t, Shape{OutageMs: 2000})
	if _, why := s.Offer(at(1999), 10); why != DropBlackout {
		t.Fatal("inside outage")
	}
	if _, why := s.Offer(at(2000), 10); why != "" {
		t.Fatal("after outage")
	}
	// a new shape replaces the schedule
	s.Apply(at(2100), Shape{})
	if _, why := s.Offer(at(2100), 10); why != "" {
		t.Fatal(why)
	}
}

func TestValidate(t *testing.T) {
	for name, sh := range map[string]Shape{
		"rate without burst":   {RateBps: 1, LatencyMs: 1},
		"rate without limit":   {RateBps: 1, BurstBytes: 1500},
		"police without burst": {PoliceBps: 1},
		"loss over 100":        {LossPct: 101},
		"negative delay":       {DelayMs: -1},
		"bad bursts":           {Bursts: &Bursts{DropMs: 100, Count: 1, EveryMs: 50}},
	} {
		if sh.Validate() == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// Through real sockets: a client talks to an echo upstream via the relay, both ways, and a down outage drops.
func TestRelayEndToEnd(t *testing.T) {
	up, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer up.Close()
	go func() {
		b := make([]byte, 2048)
		for {
			n, a, err := up.ReadFromUDP(b)
			if err != nil {
				return
			}
			up.WriteToUDP(b[:n], a)
		}
	}()
	lc, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer lc.Close()
	r := &relay{lc: lc, upstream: up.LocalAddr().(*net.UDPAddr), clients: map[string]*client{}}
	r.down = newLine("down", 1, func(p *pkt) { lc.WriteToUDP(p.b, p.c.addr) })
	r.up = newLine("up", 2, func(p *pkt) { p.c.up.Write(p.b) })
	r.down.sh.Apply(time.Now(), Shape{DelayMs: 30})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.down.run(ctx)
	go r.up.run(ctx)
	go r.readClients()

	c, err := net.DialUDP("udp", nil, lc.LocalAddr().(*net.UDPAddr))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	buf := make([]byte, 2048)
	roundTrip := func(msg string) (time.Duration, bool) {
		start := time.Now()
		c.Write([]byte(msg))
		c.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		n, err := c.Read(buf)
		return time.Since(start), err == nil && string(buf[:n]) == msg
	}
	if d, ok := roundTrip("hello"); !ok || d < 30*time.Millisecond || d > 200*time.Millisecond {
		t.Fatalf("delayed echo: ok=%v after %v", ok, d)
	}
	r.down.sh.Apply(time.Now(), Shape{OutageMs: 60_000})
	if _, ok := roundTrip("lost"); ok {
		t.Fatal("echo arrived through an outage")
	}
	r.down.sh.Apply(time.Now(), Shape{})
	if _, ok := roundTrip("back"); !ok {
		t.Fatal("no echo after clear")
	}
	if c, _, _ := r.down.sh.Snapshot(time.Now()); c.Drop[DropBlackout].Pkts != 1 || c.Out.Pkts != 2 {
		t.Fatalf("counters %+v out %+v", c.Drop[DropBlackout], c.Out)
	}
}
