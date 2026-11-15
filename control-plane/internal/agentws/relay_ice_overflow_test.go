package agentws

import (
	"bytes"
	"fmt"
	"log/slog"
	"strings"
	"testing"
)

func iceFrame(pc string, n int) []byte {
	return []byte(fmt.Sprintf(`{"type":"ice","pc":%q,"candidate":{"candidate":"candidate:%d 1 udp 1 10.0.0.1 %d typ host","sdpMid":"0","sdpMLineIndex":0}}`, pc, n, 10000+n))
}

// A WebTransport session's browser never attaches, and a host with many addresses
// trickles hundreds of fallback candidates. In the agent's order (each PC's offer,
// then its candidates) the session must survive, keep every non-ICE frame, stay
// bounded, and flush in order on a later attach.
func TestRelayICEFloodWithNoBrowserKeepsOffersAndStaysBounded(t *testing.T) {
	var logs bytes.Buffer
	bus := NewRelayBus(slog.New(slog.NewTextHandler(&logs, nil)))
	videoOffer := []byte(`{"type":"offer","pc":"video","sdp":"v"}`)
	audioOffer := []byte(`{"type":"offer","pc":"audio","sdp":"a"}`)
	bye := []byte(`{"type":"bye"}`)

	var sent [][]byte
	sent = append(sent, videoOffer)
	for i := 0; i < 900; i++ {
		sent = append(sent, iceFrame("video", i))
	}
	sent = append(sent, audioOffer) // arrives with the buffer full
	for i := 0; i < 10; i++ {
		sent = append(sent, iceFrame("audio", i))
	}
	sent = append(sent, bye)
	order := map[string]int{}
	for i, f := range sent {
		order[string(f)] = i
		if err := bus.Deliver("s", f); err != nil {
			t.Fatalf("deliver %d (%s): %v", i, f, err)
		}
		if n := len(bus.pending["s"]); n > relayBufMax {
			t.Fatalf("pending grew to %d frames, cap %d", n, relayBufMax)
		}
	}

	ch := make(chan []byte, relayBufMax)
	bus.Register("s", ch)
	close(ch)
	var got [][]byte
	for f := range ch {
		got = append(got, f)
	}
	if len(got) != relayBufMax {
		t.Fatalf("flushed %d frames, want %d", len(got), relayBufMax)
	}
	if string(got[0]) != string(videoOffer) {
		t.Fatalf("first flushed frame %s, want the video offer", got[0])
	}
	seen := map[string]bool{}
	last := -1
	for _, f := range got {
		seen[string(f)] = true
		if order[string(f)] <= last {
			t.Fatalf("flush out of order at %s", f)
		}
		last = order[string(f)]
	}
	for _, f := range [][]byte{videoOffer, audioOffer, bye} {
		if !seen[string(f)] {
			t.Fatalf("non-ICE frame %s was dropped", f)
		}
	}
	if c := strings.Count(logs.String(), "relay-ice-dropped"); c != 1 {
		t.Fatalf("relay-ice-dropped logged %d times, want once:\n%s", c, logs.String())
	}
	if want := fmt.Sprintf("count=%d", len(sent)-relayBufMax); !strings.Contains(logs.String(), want) {
		t.Fatalf("drop log missing %s:\n%s", want, logs.String())
	}
}

// With no `ice` left to shed, a full buffer still rejects a frame: the cap holds.
func TestRelayNonICEOverflowStillErrors(t *testing.T) {
	bus := NewRelayBus(slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)))
	for i := 0; i < relayBufMax; i++ {
		if err := bus.Deliver("s", []byte(fmt.Sprintf(`{"type":"offer","pc":"video","sdp":"%d"}`, i))); err != nil {
			t.Fatalf("deliver %d: %v", i, err)
		}
	}
	if err := bus.Deliver("s", []byte(`{"type":"bye"}`)); err == nil {
		t.Fatal("non-ICE overflow was accepted")
	}
	if err := bus.Deliver("s", iceFrame("video", 0)); err != nil {
		t.Fatalf("ice into a full non-ICE buffer must be dropped, not fail: %v", err)
	}
	if n := len(bus.pending["s"]); n != relayBufMax {
		t.Fatalf("pending %d, want %d", n, relayBufMax)
	}
}
