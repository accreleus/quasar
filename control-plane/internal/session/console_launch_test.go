package session

import "testing"

func TestCheckConsoleTopology(t *testing.T) {
	for topology, wantErr := range map[string]bool{
		"local_only":  false,
		"dual_output": true,
		"stream_only": true,
		"":            true,
	} {
		if err := checkConsoleTopology(topology); (err != nil) != wantErr {
			t.Errorf("checkConsoleTopology(%q) = %v, want error %v", topology, err, wantErr)
		}
	}
}
