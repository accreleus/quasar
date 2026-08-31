package agentws

import (
	"io"
	"log/slog"
	"testing"
)

func TestRelayPendingSaturationIsExplicit(t *testing.T) {
	bus := NewRelayBus(slog.New(slog.NewTextHandler(io.Discard, nil)))
	for i := 0; i < relayBufMax; i++ {
		if err := bus.Deliver("session", []byte("ice")); err != nil {
			t.Fatalf("deliver %d: %v", i, err)
		}
	}
	if err := bus.Deliver("session", []byte("overflow")); err == nil {
		t.Fatal("overflow signaling was silently accepted/dropped")
	}
}

func TestRelayRegisteredDeliveryIsLossless(t *testing.T) {
	bus := NewRelayBus(slog.New(slog.NewTextHandler(io.Discard, nil)))
	ch := make(chan []byte, 1)
	bus.Register("session", ch)
	want := []byte("offer")
	if err := bus.Deliver("session", want); err != nil {
		t.Fatalf("deliver: %v", err)
	}
	if got := <-ch; string(got) != string(want) {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestRelayNormalDualPCPrebrowserBurstDrainsLosslessly(t *testing.T) {
	bus := NewRelayBus(slog.New(slog.NewTextHandler(io.Discard, nil)))
	const observedDualPCBurst = 68
	for i := 0; i < observedDualPCBurst; i++ {
		if err := bus.Deliver("session", []byte{byte(i)}); err != nil {
			t.Fatalf("prebrowser deliver %d: %v", i, err)
		}
	}
	ch := make(chan []byte, relayBufMax)
	bus.Register("session", ch)
	for i := 0; i < observedDualPCBurst; i++ {
		select {
		case got := <-ch:
			if len(got) != 1 || got[0] != byte(i) {
				t.Fatalf("frame %d: got %v", i, got)
			}
		default:
			t.Fatalf("frame %d was dropped during registration drain", i)
		}
	}
}
