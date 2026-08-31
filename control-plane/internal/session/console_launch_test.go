package session

import "testing"

func TestConsoleTransportPlan(t *testing.T) {
	tests := []struct {
		name      string
		topology  string
		slots     int32
		signaling bool
		wantErr   bool
	}{
		{"local-only has no encoder or signaling", "local_only", 0, false, false},
		{"dual output keeps encoder and signaling", "dual_output", 2, true, false},
		{"invalid topology fails closed", "stream_only", 0, false, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			slots, signaling, err := consoleTransportPlan(tt.topology, 2)
			if (err != nil) != tt.wantErr || slots != tt.slots || signaling != tt.signaling {
				t.Fatalf("got slots=%d signaling=%v err=%v", slots, signaling, err)
			}
		})
	}
}
