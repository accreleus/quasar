package platform

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/accreleus/quasar/control-plane/internal/buildinfo"
)

func quietLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

// fakeNotifyStore records what the notifier asked of it.
type fakeNotifyStore struct {
	claimAttempts int
	claimOK       bool
	claimErr      error
	existing      Notification
	existingFound bool

	recorded    []Delivery
	recordedIDs []string
	recordErr   error
}

func (f *fakeNotifyStore) ClaimNotification(context.Context, string, int) (int, bool, error) {
	if f.claimErr != nil {
		return 0, false, f.claimErr
	}
	return f.claimAttempts, f.claimOK, nil
}

func (f *fakeNotifyStore) RecordDelivery(_ context.Context, id string, d Delivery, _ time.Time) error {
	f.recorded = append(f.recorded, d)
	f.recordedIDs = append(f.recordedIDs, id)
	return f.recordErr
}

func (f *fakeNotifyStore) Notification(context.Context, string) (Notification, bool, error) {
	return f.existing, f.existingFound, nil
}

// viewWithUpdate: a control plane on `installed`, one listed release on `newest`.
func viewWithUpdate(installed, newest string) View {
	return View{
		Channel: ChannelStable,
		Installed: Installed{ControlPlane: buildinfo.Identity{
			Version: "0.2.3", SourceCommit: str(installed), SchemaVersion: 78,
		}},
		Available: []Release{{
			ID: "11111111-1111-1111-1111-111111111111", Channel: ChannelStable,
			Version: str("0.2.4"), SourceCommit: newest, BuiltAt: time.Unix(1_700_000_000, 0),
			SchemaVersion: 79, Notes: "### Added\n- a thing",
		}},
	}
}

func newTestNotifier(t *testing.T, store notifyStore, view View, cfg WebhookConfig, out Delivery) (*Notifier, *int) {
	t.Helper()
	sends := 0
	n := NewNotifier(store, NotifyDeps{
		View:   func(context.Context) (View, error) { return view, nil },
		Config: func(context.Context) (WebhookConfig, error) { return cfg, nil },
		Send: func(context.Context, WebhookConfig, Event) Delivery {
			sends++
			return out
		},
		Now: func() time.Time { return time.Unix(1_700_000_100, 0) },
	}, quietLog())
	return n, &sends
}

func enabledCfg() WebhookConfig {
	return WebhookConfig{Enabled: true, URL: "https://hooks.example.com/abc"}
}

// TestNotifySkipsWhenNothingToSay — the three configuration skips and the
// no-update skip all send nothing. A fresh install that is current must not POST.
func TestNotifySkipsWhenNothingToSay(t *testing.T) {
	cases := map[string]struct {
		cfg    WebhookConfig
		view   View
		reason string
	}{
		"disabled": {
			cfg:    WebhookConfig{Enabled: false, URL: "https://hooks.example.com/abc"},
			view:   viewWithUpdate("aaaaaaa", "bbbbbbbbbb"),
			reason: SkipDisabled,
		},
		"no url": {
			cfg:    WebhookConfig{Enabled: true},
			view:   viewWithUpdate("aaaaaaa", "bbbbbbbbbb"),
			reason: SkipNotConfigured,
		},
		"already on it": {
			cfg:    enabledCfg(),
			view:   viewWithUpdate("bbbbbbb", "bbbbbbbbbb"),
			reason: SkipNoUpdate,
		},
		"nothing listed": {
			cfg:    enabledCfg(),
			view:   View{Channel: ChannelStable},
			reason: SkipNoUpdate,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			store := &fakeNotifyStore{claimAttempts: 1, claimOK: true}
			n, sends := newTestNotifier(t, store, tc.view, tc.cfg, Delivery{OK: true})
			got := n.Notify(context.Background())
			if got.Status != NotifySkipped || got.Reason != tc.reason {
				t.Fatalf("Notify = %q/%q, want skipped/%s", got.Status, got.Reason, tc.reason)
			}
			if *sends != 0 {
				t.Fatalf("sent %d notifications, want 0", *sends)
			}
		})
	}
}

// TestNotifyDeliversOnceAndRecordsIt — the happy path, and the claim is what
// makes it once.
func TestNotifyDeliversOnceAndRecordsIt(t *testing.T) {
	store := &fakeNotifyStore{claimAttempts: 1, claimOK: true}
	view := viewWithUpdate("aaaaaaa", "bbbbbbbbbb")
	code := 204
	n, sends := newTestNotifier(t, store, view, enabledCfg(), Delivery{OK: true, StatusCode: &code})

	got := n.Notify(context.Background())
	if got.Status != NotifyDelivered {
		t.Fatalf("Notify = %q (%q), want delivered", got.Status, got.Reason)
	}
	if got.ReleaseID != view.Available[0].ID {
		t.Errorf("release id = %q, want %q", got.ReleaseID, view.Available[0].ID)
	}
	if *sends != 1 || len(store.recorded) != 1 || !store.recorded[0].OK {
		t.Fatalf("sends=%d recorded=%+v, want one delivered record", *sends, store.recorded)
	}
}

// TestNotifyDoesNotSendWhenTheClaimIsRefused — a refused claim means another
// pass already has it, or it is past the cap. Either way: no POST.
func TestNotifyDoesNotSendWhenTheClaimIsRefused(t *testing.T) {
	for name, existing := range map[string]Notification{
		"already delivered": {Status: NotifyDelivered, Attempts: 1},
		"at the cap":        {Status: NotifyFailed, Attempts: MaxNotifyAttempts},
	} {
		t.Run(name, func(t *testing.T) {
			store := &fakeNotifyStore{claimOK: false, existing: existing, existingFound: true}
			n, sends := newTestNotifier(t, store,
				viewWithUpdate("aaaaaaa", "bbbbbbbbbb"), enabledCfg(), Delivery{OK: true})
			got := n.Notify(context.Background())
			if got.Status != NotifySkipped {
				t.Fatalf("Notify = %q, want skipped", got.Status)
			}
			if *sends != 0 {
				t.Fatalf("sent %d notifications on a refused claim, want 0", *sends)
			}
		})
	}
}

// TestNotifyNeverFailsTheCaller — every collaborator failing still returns a
// skipped outcome. Notify has no error return by design; this proves it also
// has no panic path when a dep says no.
func TestNotifyNeverFailsTheCaller(t *testing.T) {
	boom := errors.New("boom")
	cases := map[string]NotifyDeps{
		"config unavailable": {
			Config: func(context.Context) (WebhookConfig, error) { return WebhookConfig{}, boom },
			View:   func(context.Context) (View, error) { return viewWithUpdate("a", "b"), nil },
		},
		"view unavailable": {
			Config: func(context.Context) (WebhookConfig, error) { return enabledCfg(), nil },
			View:   func(context.Context) (View, error) { return View{}, boom },
		},
	}
	for name, deps := range cases {
		t.Run(name, func(t *testing.T) {
			deps.Send = func(context.Context, WebhookConfig, Event) Delivery {
				t.Fatal("must not send when a dependency failed")
				return Delivery{}
			}
			n := NewNotifier(&fakeNotifyStore{}, deps, quietLog())
			if got := n.Notify(context.Background()); got.Status != NotifySkipped || got.Reason != SkipUnavailable {
				t.Fatalf("Notify = %q/%q, want skipped/%s", got.Status, got.Reason, SkipUnavailable)
			}
		})
	}
}

// TestNotifyReportsAFailedDeliveryWithoutFailing — a refused webhook is a
// summary line, and the row is left failed so the next pass retries.
func TestNotifyReportsAFailedDeliveryWithoutFailing(t *testing.T) {
	store := &fakeNotifyStore{claimAttempts: 2, claimOK: true}
	code := 500
	n, _ := newTestNotifier(t, store, viewWithUpdate("aaaaaaa", "bbbbbbbbbb"), enabledCfg(),
		Delivery{StatusCode: &code, Error: "the webhook receiver answered 500 Internal Server Error"})

	got := n.Notify(context.Background())
	if got.Status != NotifyFailed {
		t.Fatalf("Notify = %q, want failed", got.Status)
	}
	if got.StatusCode == nil || *got.StatusCode != 500 {
		t.Errorf("status code = %v, want 500", got.StatusCode)
	}
	if len(store.recorded) != 1 || store.recorded[0].OK {
		t.Fatalf("recorded = %+v, want one failed record", store.recorded)
	}
	s := got.Summary()
	if s["notify"] != NotifyFailed || s["notify_status_code"] != 500 {
		t.Errorf("summary = %v, want a failed notify with its status code", s)
	}
}

// TestUpdateAvailable is the server twin of the client's hasUpdate.
func TestUpdateAvailable(t *testing.T) {
	built := time.Unix(1_700_000_000, 0)
	cp := buildinfo.Identity{Version: "0.2.3", SourceCommit: str("abcdef0123456789"), SchemaVersion: 78,
		BuiltAt: str(built.UTC().Format(time.RFC3339))}

	t.Run("a short agent-style commit still matches by prefix", func(t *testing.T) {
		v := View{Installed: Installed{ControlPlane: cp}, Available: []Release{{
			ID: "x", Channel: ChannelStable, SourceCommit: "abcdef0123456789", SchemaVersion: 79,
		}}}
		v.Installed.ControlPlane.SourceCommit = str("abcdef0")
		if _, ok := v.UpdateAvailable(); ok {
			t.Fatal("a prefix-matching commit is the installed release, not an update")
		}
	})

	t.Run("a different commit is an update", func(t *testing.T) {
		v := View{Installed: Installed{ControlPlane: cp}, Available: []Release{{
			ID: "x", Channel: ChannelStable, SourceCommit: "999999999999", SchemaVersion: 79,
		}}}
		rel, ok := v.UpdateAvailable()
		if !ok || rel.ID != "x" {
			t.Fatalf("UpdateAvailable = %v/%v, want the listed release", rel, ok)
		}
	})

	t.Run("an unstamped control plane treats the listing as news", func(t *testing.T) {
		v := View{
			Installed: Installed{ControlPlane: buildinfo.Identity{Version: "dev"}},
			Available: []Release{{ID: "x", Channel: ChannelStable, SourceCommit: "999", SchemaVersion: 79}},
		}
		if _, ok := v.UpdateAvailable(); !ok {
			t.Fatal("an unstamped build has no commit to be up to date with")
		}
	})

	t.Run("an edge build older than installed is not an update", func(t *testing.T) {
		v := View{Installed: Installed{ControlPlane: cp}, Available: []Release{{
			ID: "x", Channel: ChannelEdge, SourceCommit: "999999999999",
			SchemaVersion: 78, BuiltAt: built.Add(-time.Hour),
		}}}
		if _, ok := v.UpdateAvailable(); ok {
			t.Fatal("a same-schema edge build predating the installed one is a downgrade")
		}
	})
}

// TestReleaseEventCarriesSlackAndDiscordFields — `text` and `content` are what
// make a raw Slack or Discord incoming webhook render this body with no adapter.
func TestReleaseEventCarriesSlackAndDiscordFields(t *testing.T) {
	view := viewWithUpdate("aaaaaaa", "bbbbbbbbbb")
	ev := releaseEvent(view, view.Available[0], time.Unix(1_700_000_100, 0))

	if ev.Text == "" || ev.Text != ev.Content {
		t.Fatalf("text=%q content=%q, want the same non-empty sentence in both", ev.Text, ev.Content)
	}
	if !strings.Contains(ev.Text, "0.2.4") || !strings.Contains(ev.Text, "0.2.3") {
		t.Errorf("text %q should name both the available and the installed version", ev.Text)
	}
	if ev.Event != NotifyEventRelease || ev.Release == nil || ev.Release.ID != view.Available[0].ID {
		t.Errorf("event = %+v, want the release described", ev)
	}
	// Round-trips as JSON with `release` present, and null on a test send.
	if _, err := json.Marshal(ev); err != nil {
		t.Fatalf("marshal: %v", err)
	}
	test := TestEvent(view, time.Unix(1_700_000_100, 0))
	if test.Event != NotifyEventTest || test.Release != nil {
		t.Errorf("test event = %+v, want a null release", test)
	}
}

// TestExcerptBoundsNotesOnARuneBoundary — a cut mid-rune is not valid UTF-8 and
// a receiver parsing JSON is entitled to reject it.
func TestExcerptBoundsNotesOnARuneBoundary(t *testing.T) {
	got := excerpt(strings.Repeat("é", notesExcerptMax))
	if len(got) > notesExcerptMax+16 {
		t.Fatalf("excerpt length %d, want it bounded near %d", len(got), notesExcerptMax)
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("a truncated excerpt should say so, got %q", got[len(got)-8:])
	}
	for _, r := range got {
		if r == '�' {
			t.Fatal("excerpt cut mid-rune")
		}
	}
}
