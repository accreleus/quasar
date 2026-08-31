package session

import "sync"

// externalState is the in-memory view of one running session's EXTERNAL
// (encoded) frame size and whether its encoder can change that size live.
// Nothing here is persisted: the session row keeps the LAUNCH size as the truth,
// and this only answers "what is it right now". A restart drops it and the next
// session_metrics sample refills it — the agent is the only thing that knows.
type externalState struct {
	// Width/Height are meaningful only when HasSize is set. HasSize false means
	// the external size is the launch size, either because nothing moved it or
	// because the sample omitted the keys (sent only when it differs from launch).
	Width   int32
	Height  int32
	HasSize bool
	// Supported is the agent's `external_resize_supported` readback. nil (an older
	// agent, or no sample yet) is PERMISSIVE: the request goes through and the
	// agent nacks it into display_update_rejected. Only an explicit false
	// short-circuits to external_resize_unsupported.
	Supported *bool
	// Owner is the agent's `external_owner` readback ("auto" | "pinned" | ""):
	// who owns the live external size, the ABR ladder or a manual PATCH. Only
	// meaningful alongside HasSize — the agent reports it in the same window it
	// reports stream_width/height (node-agent/src/session/metrics.rs), so it is
	// cleared in lockstep with the size and never left stale.
	Owner string
}

// displayState holds the per-session external-resolution cache. Like swapper and
// healthEvaluator it owns its own map and mutex, takes no coordinator lock and
// calls nothing out, so it can be locked on the metrics hot path with no
// ordering concerns.
type displayState struct {
	mu sync.Mutex
	m  map[string]externalState
}

func newDisplayState() *displayState {
	return &displayState{m: make(map[string]externalState)}
}

// setSize records an OPTIMISTIC external size, overwritten by the first
// session_metrics sample reporting the real one (see observe).
//
// A stream_width/height PATCH is a statement of ownership, not just a resize
// (control-api.md §Pin / release semantics): any non-launch size PINS the session
// so the ladder stops fighting the human who chose it, and the launch size
// releases it to "". Setting Owner here rather than only in observe is what makes
// a manual pin visible on the very next GET.
func (d *displayState) setSize(sessionID string, w, h, launchW, launchH int32) {
	d.mu.Lock()
	defer d.mu.Unlock()
	st := d.m[sessionID]
	st.Width, st.Height, st.HasSize = w, h, true
	if w != launchW || h != launchH {
		st.Owner = "pinned"
	} else {
		st.Owner = ""
	}
	d.m[sessionID] = st
}

// observe folds a session_metrics sample into the cache. It is the authoritative
// readback and overrides any optimistic value.
//
// stream_width/height are sent only when the external size differs from launch,
// so their ABSENCE from a display-aware sample means "back at launch size" and
// must clear the cached size — otherwise a session stepped 1080p, 720p, 1080p
// reports 720p forever. A sample with all four fields empty is not display-aware
// and is ignored entirely, so it cannot clobber an optimistic value.
//
// owner follows the same window and is cleared in lockstep with the size, never
// left stale from a prior sample.
func (d *displayState) observe(sessionID string, w, h *int32, supported *bool, owner string) {
	if w == nil && h == nil && supported == nil && owner == "" {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	st := d.m[sessionID]
	if w != nil && h != nil {
		st.Width, st.Height, st.HasSize = *w, *h, true
		st.Owner = owner
	} else {
		st.Width, st.Height, st.HasSize = 0, 0, false
		st.Owner = ""
	}
	if supported != nil {
		st.Supported = supported
	}
	d.m[sessionID] = st
}

// get returns the cached state; false when nothing has been recorded at all, so
// the caller falls back to the launch size and to supported-unknown-allows.
func (d *displayState) get(sessionID string) (externalState, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	st, ok := d.m[sessionID]
	return st, ok
}

// forget drops a session's entry, at the same terminal sites as
// healthEvaluator.forget: a session ending any way at all must not leak an entry
// here, and a recycled id must never inherit a stale external size.
func (d *displayState) forget(sessionID string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.m, sessionID)
}

// externalSizeOf is the cached external size when known, else the launch size.
func (d *displayState) externalSizeOf(sessionID string, launchW, launchH int32) (int32, int32) {
	if st, ok := d.get(sessionID); ok && st.HasSize {
		return st.Width, st.Height
	}
	return launchW, launchH
}
