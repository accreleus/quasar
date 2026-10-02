package session

import "testing"

// Pure unit tests for gpuCodecSetSQL's two Go twins (#296 amendment 12). The
// SQL side is guarded against these by TestGPUCodecSetMatchesSQL
// (gpu_codecs_db_test.go); these pin down the inheritance rule itself:
// nil (never reported) inherits, a non-nil empty slice does not.

func strSliceEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestGpuCodecSetInheritance(t *testing.T) {
	cases := []struct {
		name       string
		gpuCodecs  []string
		hostCodecs []string
		want       []string
	}{
		{"gpu set wins over host", []string{"h264", "h265", "av1"}, []string{"h264"}, []string{"h264", "h265", "av1"}},
		{"gpu null inherits host", nil, []string{"h264", "av1"}, []string{"h264", "av1"}},
		{"both null falls back to h264", nil, nil, []string{"h264"}},
		{"gpu explicit empty is not null, does not inherit", []string{}, []string{"h264", "h265"}, []string{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := gpuCodecSet(tc.gpuCodecs, tc.hostCodecs)
			if !strSliceEqual(got, tc.want) {
				t.Errorf("gpuCodecSet(%v, %v) = %v, want %v", tc.gpuCodecs, tc.hostCodecs, got, tc.want)
			}
		})
	}
}

func TestGpuCodecSetNullableInheritance(t *testing.T) {
	cases := []struct {
		name       string
		gpuCodecs  []string
		hostCodecs []string
		want       []string
		wantNil    bool
	}{
		{name: "gpu set wins over host", gpuCodecs: []string{"h264", "h265"}, hostCodecs: []string{"h264"}, want: []string{"h264", "h265"}},
		{name: "gpu null inherits host", gpuCodecs: nil, hostCodecs: []string{"h264", "av1"}, want: []string{"h264", "av1"}},
		{name: "both null stays nil (never reported)", gpuCodecs: nil, hostCodecs: nil, wantNil: true},
		{name: "gpu explicit empty does not inherit", gpuCodecs: []string{}, hostCodecs: []string{"h264"}, want: []string{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := gpuCodecSetNullable(tc.gpuCodecs, tc.hostCodecs)
			if tc.wantNil {
				if got != nil {
					t.Errorf("gpuCodecSetNullable(%v, %v) = %v, want nil", tc.gpuCodecs, tc.hostCodecs, got)
				}
				return
			}
			if !strSliceEqual(got, tc.want) {
				t.Errorf("gpuCodecSetNullable(%v, %v) = %v, want %v", tc.gpuCodecs, tc.hostCodecs, got, tc.want)
			}
		})
	}
}
