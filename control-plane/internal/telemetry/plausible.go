package telemetry

import "time"

// ── ingest timestamp validation ──────────────────────────────────────────────
//
// A client sample or trace event carries the timestamp the CLIENT chose. Nothing
// checked it. A stamp in seconds, or a performance.now() reading (~1e5), or
// nanoseconds, was stored happily — and then vanished, because every read window
// is a few minutes wide and none of those values land in it. The row existed, the
// data did not, and no counter anywhere said so.
//
// So: an implausible stamp is DROPPED at ingest, counted, and named. Dropping is
// the honest outcome — storing it is indistinguishable from losing it, minus the
// counter. The batch still succeeds (202): telemetry never fails a client.
//
// The agent is NOT validated. It is the trusted host reporter and its stamps ARE
// the host clock — the thing everything else is aligned to.

// PlausibleWindow is how far from server-now an ingested client timestamp may
// sit. Deliberately enormous: this is a "is this a Unix-epoch-millisecond stamp
// at all" check, not a freshness check. Freshness is the read window's job, and a
// client with a badly wrong wall clock is exactly what the clock offset exists to
// correct — narrowing this would start rejecting real data.
const PlausibleWindow = 24 * time.Hour

// PlausibleTsUnixMs reports whether ts could be a Unix-epoch-millisecond stamp,
// and when it could not, names the domain it most likely came from. The reason is
// for the operator: "looks like seconds" turns a mystery into a one-line client
// fix.
func PlausibleTsUnixMs(ts int64, now time.Time) (ok bool, reason string) {
	nowMs := now.UnixMilli()
	within := func(v int64) bool {
		d := v - nowMs
		if d < 0 {
			d = -d
		}
		return d <= PlausibleWindow.Milliseconds()
	}
	if within(ts) {
		return true, ""
	}
	switch {
	case ts <= 0:
		return false, "not a timestamp (zero or negative)"
	case within(ts * 1000):
		return false, "looks like seconds, not milliseconds"
	case within(ts / 1_000_000):
		return false, "looks like nanoseconds, not milliseconds"
	case within(ts / 1000):
		return false, "looks like microseconds, not milliseconds"
	case ts < 1_000_000_000:
		// Anything under ~1e9 ms is 1970; performance.now() lives here (ms since
		// page load), and it is the single most likely mistake on the client.
		return false, "looks like performance.now (ms since page load), not a Unix epoch stamp"
	default:
		return false, "outside the ±24 h plausibility window"
	}
}
