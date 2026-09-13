// Outbound notification when a platform release appears (#123). A webhook
// rather than email: email needs SMTP credentials, a sender identity and a
// bounce story before it delivers anything.
//
// Delivery must never break detection, which is why Notify has no error return
// at all. semantics: control-api.md §"Release notifications"
package platform

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"
)

// The `event` field, and the two events a receiver can be sent.
const (
	NotifyEventRelease = "platform.release.detected"
	NotifyEventTest    = "platform.release.test"
)

const (
	// MaxNotifyAttempts bounds what a permanently broken URL costs: after this
	// many passes a release is left un-notified rather than retried forever.
	// Detection re-runs weekly and on demand, so "forever" is real.
	MaxNotifyAttempts = 5

	// notesExcerptMax bounds the release body carried in the payload. The notes
	// column allows 64 KiB and a webhook receiver is not a place to put it.
	notesExcerptMax = 1000
)

// Notification statuses, on the wire and in the status column.
const (
	NotifyDelivered = "delivered"
	NotifyFailed    = "failed"
	NotifySkipped   = "skipped"
)

// Why a pass sent nothing. Closed vocabulary: it lands in a job summary an
// operator reads, and prose there would drift.
const (
	SkipDisabled        = "disabled"
	SkipNotConfigured   = "not_configured"
	SkipNoUpdate        = "no_update"
	SkipAlreadyNotified = "already_notified"
	SkipAttemptCap      = "attempt_cap"
	SkipUnavailable     = "unavailable"
)

// WebhookConfig is one delivery's resolved target. Secret is a credential: it
// signs the body and is never logged, stored or echoed.
type WebhookConfig struct {
	Enabled bool
	URL     string
	Secret  string
}

// Configured reports whether there is somewhere to send.
func (c WebhookConfig) Configured() bool { return c.URL != "" }

// Delivery is one POST's outcome. Error must carry NO URL: a Slack or Discord
// webhook URL is itself the credential (sanitizeErr in notify_client.go).
type Delivery struct {
	OK         bool
	StatusCode *int
	Error      string
	DurationMS int
}

// WebhookStatus is the view's `release_webhook` object: how this instance
// announces a release, and how the last announcement went.
//
// URL and Enabled mirror instance_settings, as `channel` and `edge_branch`
// already do, so the console renders the whole Releases page from one read.
// SecretConfigured is a boolean and never the secret.
type WebhookStatus struct {
	Enabled          bool            `json:"enabled"`
	URL              string          `json:"url"`
	SecretConfigured bool            `json:"secret_configured"`
	LastDelivery     *DeliveryRecord `json:"last_delivery"`
}

// DeliveryRecord is the last attempt, as an admin needs to read it. Error is
// bounded prose and never carries the webhook URL.
type DeliveryRecord struct {
	ReleaseID      string  `json:"release_id"`
	ReleaseVersion *string `json:"release_version"`
	Status         string  `json:"status"`
	Attempts       int     `json:"attempts"`
	AttemptedAt    string  `json:"attempted_at"`
	StatusCode     *int    `json:"status_code"`
	Error          *string `json:"error"`
}

// NotifyDeps are the notifier's collaborators, as closures: internal/settings
// and internal/secrets sit above this package.
type NotifyDeps struct {
	// View is the same read the console gets, so "an update is available" means
	// one thing on both surfaces.
	View func(ctx context.Context) (View, error)
	// Config resolves enabled/URL/secret per pass — a webhook switched off must
	// stop sending with no restart.
	Config func(ctx context.Context) (WebhookConfig, error)
	// Send performs one delivery, retries included. Injectable so no test dials.
	Send func(ctx context.Context, cfg WebhookConfig, ev Event) Delivery
	Now  func() time.Time
}

// notifyStore is the notifier's slice of *Store, so the flow can be exercised
// without a database.
type notifyStore interface {
	ClaimNotification(ctx context.Context, releaseID string, maxAttempts int) (int, bool, error)
	RecordDelivery(ctx context.Context, releaseID string, d Delivery, now time.Time) error
	Notification(ctx context.Context, releaseID string) (Notification, bool, error)
}

// Notifier announces a newly-available release exactly once.
type Notifier struct {
	store notifyStore
	deps  NotifyDeps
	log   *slog.Logger
}

// NewNotifier builds a Notifier. A nil Send falls back to the production
// sender; a nil Now to time.Now.
func NewNotifier(store notifyStore, deps NotifyDeps, log *slog.Logger) *Notifier {
	if log == nil {
		log = slog.Default()
	}
	if deps.Send == nil {
		deps.Send = SendWebhook
	}
	if deps.Now == nil {
		deps.Now = time.Now
	}
	return &Notifier{store: store, deps: deps, log: log}
}

// NotifyOutcome is one pass's result, and the detection job's summary lines.
type NotifyOutcome struct {
	Status     string // NotifyDelivered | NotifyFailed | NotifySkipped
	Reason     string // a Skip* constant, or the bounded failure text
	ReleaseID  string
	StatusCode *int
	Attempts   int
}

// Summary folds the outcome into the detection job's summary map. Small on
// purpose: the summary column has a 4096-byte ceiling and detection's own
// counts share it.
func (o NotifyOutcome) Summary() map[string]any {
	if o.Status == "" {
		return nil
	}
	s := map[string]any{"notify": o.Status}
	if o.Reason != "" {
		s["notify_reason"] = o.Reason
	}
	if o.ReleaseID != "" {
		s["notify_release_id"] = o.ReleaseID
	}
	if o.StatusCode != nil {
		s["notify_status_code"] = *o.StatusCode
	}
	return s
}

// Notify runs one pass and returns no error: an unreachable or refused webhook
// must not fail the detection job that called it.
func (n *Notifier) Notify(ctx context.Context) NotifyOutcome {
	cfg, err := n.deps.Config(ctx)
	if err != nil {
		n.log.Warn("release notification: could not read the webhook setting", "err", err)
		return NotifyOutcome{Status: NotifySkipped, Reason: SkipUnavailable}
	}
	if !cfg.Enabled {
		return NotifyOutcome{Status: NotifySkipped, Reason: SkipDisabled}
	}
	if !cfg.Configured() {
		return NotifyOutcome{Status: NotifySkipped, Reason: SkipNotConfigured}
	}

	view, err := n.deps.View(ctx)
	if err != nil {
		n.log.Warn("release notification: could not read the release view", "err", err)
		return NotifyOutcome{Status: NotifySkipped, Reason: SkipUnavailable}
	}
	rel, ok := view.UpdateAvailable()
	if !ok {
		return NotifyOutcome{Status: NotifySkipped, Reason: SkipNoUpdate}
	}

	// Claim before sending, so a scheduled pass and a "Check now" racing on the
	// same release cannot both POST it.
	attempts, claimed, err := n.store.ClaimNotification(ctx, rel.ID, MaxNotifyAttempts)
	if err != nil {
		n.log.Warn("release notification: could not claim the release", "release_id", rel.ID, "err", err)
		return NotifyOutcome{Status: NotifySkipped, Reason: SkipUnavailable, ReleaseID: rel.ID}
	}
	if !claimed {
		return NotifyOutcome{
			Status:    NotifySkipped,
			Reason:    n.unclaimedReason(ctx, rel.ID),
			ReleaseID: rel.ID,
		}
	}

	del := n.deps.Send(ctx, cfg, releaseEvent(view, *rel, n.deps.Now()))
	// WithoutCancel: a shutdown between the POST and the record would leave the
	// row failed and re-send a release that was already delivered.
	recCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if err := n.store.RecordDelivery(recCtx, rel.ID, del, n.deps.Now()); err != nil {
		// The send happened; only the record of it did not. Logged rather than
		// reported as a failed delivery, which would be a lie.
		n.log.Error("release notification: could not record the delivery", "release_id", rel.ID, "err", err)
	}
	out := NotifyOutcome{ReleaseID: rel.ID, StatusCode: del.StatusCode, Attempts: attempts}
	if del.OK {
		out.Status = NotifyDelivered
		n.log.Info("release notification delivered", "release_id", rel.ID, "attempts", attempts)
		return out
	}
	out.Status = NotifyFailed
	out.Reason = del.Error
	n.log.Warn("release notification failed", "release_id", rel.ID, "attempts", attempts, "err", del.Error)
	return out
}

// unclaimedReason explains a refused claim. Reporting only: the claim itself
// already decided, and a read that fails here costs a summary word, nothing more.
func (n *Notifier) unclaimedReason(ctx context.Context, releaseID string) string {
	rec, found, err := n.store.Notification(ctx, releaseID)
	switch {
	case err != nil || !found:
		return SkipAlreadyNotified
	case rec.Status == NotifyDelivered:
		return SkipAlreadyNotified
	default:
		return SkipAttemptCap
	}
}

// Event is the JSON body POSTed to the webhook (control-api.md §Release
// notifications).
//
// Text and Content carry the same sentence under the two field names Slack and
// Discord read, so those two receivers render the notification with no adapter
// in between. Everything structured lives beside them.
type Event struct {
	Event    string        `json:"event"`
	SentAt   string        `json:"sent_at"`
	Text     string        `json:"text"`
	Content  string        `json:"content"`
	Instance EventInstance `json:"instance"`
	// Null on a test send: there may be no release to describe.
	Release *EventRelease `json:"release"`
}

// EventInstance is the announcing control plane.
type EventInstance struct {
	Version       string  `json:"version"`
	SourceCommit  *string `json:"source_commit"`
	SchemaVersion int     `json:"schema_version"`
	Channel       string  `json:"channel"`
}

// EventRelease is the release being announced.
type EventRelease struct {
	ID            string  `json:"id"`
	Channel       string  `json:"channel"`
	Version       *string `json:"version"`
	SourceCommit  string  `json:"source_commit"`
	BuiltAt       string  `json:"built_at"`
	SchemaVersion int     `json:"schema_version"`
	Prerelease    bool    `json:"prerelease"`
	CompareURL    *string `json:"compare_url"`
	NotesExcerpt  string  `json:"notes_excerpt"`
}

func releaseEvent(view View, rel Release, now time.Time) Event {
	cp := view.Installed.ControlPlane
	return Event{
		Event:   NotifyEventRelease,
		SentAt:  now.UTC().Format(time.RFC3339),
		Text:    releaseSentence(view, rel),
		Content: releaseSentence(view, rel),
		Instance: EventInstance{
			Version:       cp.Version,
			SourceCommit:  cp.SourceCommit,
			SchemaVersion: cp.SchemaVersion,
			Channel:       view.Channel,
		},
		Release: &EventRelease{
			ID:            rel.ID,
			Channel:       rel.Channel,
			Version:       rel.Version,
			SourceCommit:  rel.SourceCommit,
			BuiltAt:       rel.BuiltAt.UTC().Format(time.RFC3339),
			SchemaVersion: rel.SchemaVersion,
			Prerelease:    rel.Prerelease,
			CompareURL:    rel.CompareURL,
			NotesExcerpt:  excerpt(rel.Notes),
		},
	}
}

// TestEvent is what the admin test-send delivers: the real shape, the real
// signature, and a null release when nothing is available to describe.
func TestEvent(view View, now time.Time) Event {
	cp := view.Installed.ControlPlane
	return Event{
		Event:   NotifyEventTest,
		SentAt:  now.UTC().Format(time.RFC3339),
		Text:    "Test notification from Quasar " + versionLabel(cp.Version) + ". Release notifications are wired up.",
		Content: "Test notification from Quasar " + versionLabel(cp.Version) + ". Release notifications are wired up.",
		Instance: EventInstance{
			Version:       cp.Version,
			SourceCommit:  cp.SourceCommit,
			SchemaVersion: cp.SchemaVersion,
			Channel:       view.Channel,
		},
	}
}

func releaseSentence(view View, rel Release) string {
	installed := versionLabel(view.Installed.ControlPlane.Version)
	switch {
	case rel.Version != nil && *rel.Version != "":
		return fmt.Sprintf("Quasar %s is available. This instance is on %s — open Fleet ▸ Releases to apply it.",
			*rel.Version, installed)
	default:
		return fmt.Sprintf("A new Quasar %s build is available (%s). This instance is on %s — open Fleet ▸ Releases to apply it.",
			rel.Channel, shortCommit(rel.SourceCommit), installed)
	}
}

func versionLabel(v string) string {
	if v == "" {
		return "an unstamped build"
	}
	return v
}

// excerpt bounds the notes on a rune boundary — a cut mid-rune is not valid
// UTF-8 and a receiver parsing JSON is entitled to reject it.
func excerpt(notes string) string {
	notes = strings.TrimSpace(notes)
	if len(notes) <= notesExcerptMax {
		return notes
	}
	cut := notesExcerptMax
	for cut > 0 && !utf8.RuneStart(notes[cut]) {
		cut--
	}
	return strings.TrimSpace(notes[:cut]) + "\n\n…"
}
