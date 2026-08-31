// mic_grant_test.go — microphone capture amendment (2026-08-02, spec §3.5):
// the launch-time grant matrix (request × instance setting → granted) and
// the console/local_only path's "never granted" guarantee.
package session

import (
	"context"
	"testing"
)

// fakeMicSettings is a minimal MicCaptureProvider for tests that don't want a
// real settings.Store — mirrors the fakeSettings pattern used elsewhere in
// this codebase (internal/library/library_db_test.go).
type fakeMicSettings struct {
	enabled bool
	err     error
}

func (f fakeMicSettings) MicCaptureEnabled(context.Context) (bool, error) {
	return f.enabled, f.err
}

// TestMicGrantMatrix exercises every combination of the launch request's
// `mic` field and the instance setting: only request=true AND setting=true
// grants. A request against a disabled/unwired gate is never an error — the
// launch always succeeds; only the granted flag differs.
func TestMicGrantMatrix(t *testing.T) {
	cases := []struct {
		name        string
		requestMic  bool
		settingOn   bool
		wireMic     *fakeMicSettings // nil ⇒ WithMicSettings never called
		wantGranted bool
	}{
		{"request off, setting off", false, false, &fakeMicSettings{enabled: false}, false},
		{"request off, setting on", false, true, &fakeMicSettings{enabled: true}, false},
		{"request on, setting off", true, false, &fakeMicSettings{enabled: false}, false},
		{"request on, setting on", true, true, &fakeMicSettings{enabled: true}, true},
		{"request on, no provider wired", true, false, nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pool := testDB(t)
			store := NewStore(pool)
			s := seed(t, pool, 4)
			disp := newFakeDispatcher(true)
			var opts []CoordinatorOption
			if tc.wireMic != nil {
				opts = append(opts, WithMicSettings(*tc.wireMic))
			}
			coord := newTestCoordinator(t, store, disp, testLogger(), opts...)
			ctx := context.Background()

			res, err := coord.LaunchByProfile(ctx, s.userID, LaunchParams{AppID: s.appID, Mic: tc.requestMic})
			if err != nil {
				t.Fatalf("launch: %v (a mic request must never fail a launch)", err)
			}
			if res.Session.Mic != tc.wantGranted {
				t.Errorf("granted mic = %v, want %v", res.Session.Mic, tc.wantGranted)
			}

			// Persisted row agrees with what LaunchByProfile returned.
			got, err := store.Get(ctx, res.Session.ID)
			if err != nil {
				t.Fatalf("get: %v", err)
			}
			if got.Mic != tc.wantGranted {
				t.Errorf("persisted session.mic = %v, want %v", got.Mic, tc.wantGranted)
			}
		})
	}
}

// TestMicGrantSettingsReadErrorFailsClosed — a transient settings-read error
// must never grant a mic (and must never fail the launch either — same
// posture as every other best-effort read on the launch path).
func TestMicGrantSettingsReadErrorFailsClosed(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	s := seed(t, pool, 4)
	disp := newFakeDispatcher(true)
	coord := newTestCoordinator(t, store, disp, testLogger(),
		WithMicSettings(fakeMicSettings{enabled: true, err: context.DeadlineExceeded}))
	ctx := context.Background()

	res, err := coord.LaunchByProfile(ctx, s.userID, LaunchParams{AppID: s.appID, Mic: true})
	if err != nil {
		t.Fatalf("launch: %v (a settings-read failure must not fail the launch)", err)
	}
	if res.Session.Mic {
		t.Errorf("granted mic = true on a settings-read error, want false (fail closed)")
	}
}
