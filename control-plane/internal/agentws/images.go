package agentws

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// Trust-boundary validation: authentication is not validation — a compromised
// or buggy agent must not write an unbounded string into Postgres or spam the
// read loop. Every bound matches agent-api.md's documented shape; nothing here
// rejects a compliant agent.

const (
	maxImageIDLen  = 128
	maxVersionLen  = 128
	maxImageErrLen = 300
)

// validateImageState checks m in place: a bad image_id/version drops the whole
// message (nothing safe to store); error/progress_pct/bytes are clamped so a
// partially-bad report is not thrown away wholesale. False ⇒ drop entirely.
func validateImageState(m *ImageStateMsg) bool {
	if m.ImageID == "" || len(m.ImageID) > maxImageIDLen {
		return false
	}
	if len(m.Version) > maxVersionLen {
		return false
	}
	if len(m.Error) > maxImageErrLen {
		// Truncate, don't drop: operator-facing context on an otherwise-valid
		// transition.
		m.Error = m.Error[:maxImageErrLen]
	}
	if m.ProgressPct < 0 {
		m.ProgressPct = 0
	} else if m.ProgressPct > 100 {
		m.ProgressPct = 100
	}
	if m.Bytes < 0 {
		m.Bytes = 0
	}
	return true
}

// register.images has no token bucket (it lands once per connect, one DB op
// per entry in ImageEvents.AgentImagesRegistered), so the snapshot must be
// bounded here before it reaches ImageEvents.

// Exceeding this drops the entire snapshot — treated as "not reported", never
// as a registration failure: an oversized report is a warn, not a reason to
// cost a host its session traffic.
const maxRegisterImages = 256

// Narrower than internal/images' hostImageStates on purpose: "building" (P4)
// is reachable only via image_state, never at register time.
var registerImageStates = map[string]bool{
	"absent":  true,
	"pulling": true,
	"ready":   true,
	"failed":  true,
}

// sanitizeRegisterImages bounds and validates a register.images snapshot.
// (nil, false) when the field was absent or over maxRegisterImages (over-limit
// is treated identically to absent). Otherwise the validated snapshot, deduped
// keeping first occurrence, with per-entry length/state violations dropped.
// Drops are logged once in aggregate — one line, not a flood.
func sanitizeRegisterImages(images []RegisterImage, log *slog.Logger, hostID string) ([]RegisterImage, bool) {
	if images == nil {
		return nil, false
	}
	if len(images) > maxRegisterImages {
		log.Warn("register images: snapshot exceeds max entries, dropping entire snapshot",
			"host_id", hostID, "count", len(images), "max", maxRegisterImages)
		return nil, false
	}
	seen := make(map[string]bool, len(images))
	out := make([]RegisterImage, 0, len(images))
	dropped := 0
	for _, img := range images {
		if img.ImageID == "" || len(img.ImageID) > maxImageIDLen {
			dropped++
			continue
		}
		if len(img.Version) > maxVersionLen {
			dropped++
			continue
		}
		if !registerImageStates[img.State] {
			dropped++
			continue
		}
		if seen[img.ImageID] {
			dropped++
			continue
		}
		seen[img.ImageID] = true
		out = append(out, img)
	}
	if dropped > 0 {
		log.Warn("register images: dropped invalid or duplicate entries",
			"host_id", hostID, "dropped", dropped, "kept", len(out))
	}
	return out, true
}

// Per-host image_state token bucket, far more generous (burst 30, refill 5/s)
// than agent-api.md's ~one-per-2s throttle so it only catches a runaway agent.
// Excess messages are dropped (logged once); the connection is never dropped —
// killing the WS would reap the host's live sessions (schema.md invariant #3).

const (
	imageStateBucketBurst  = 30.0
	imageStateBucketRefill = 5.0 // tokens/sec
)

type imageTokenBucket struct {
	tokens float64
	last   time.Time
}

// imageStateLimiter is bounded by the live connection set: buckets are created
// lazily and evicted on disconnect, so the map never grows past
// concurrently-connected hosts.
type imageStateLimiter struct {
	mu      sync.Mutex
	buckets map[string]*imageTokenBucket
	logged  map[string]bool
}

func newImageStateLimiter() *imageStateLimiter {
	return &imageStateLimiter{
		buckets: make(map[string]*imageTokenBucket),
		logged:  make(map[string]bool),
	}
}

// allow reports whether hostID may send one more image_state message right now,
// consuming a token if so.
func (l *imageStateLimiter) allow(hostID string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	b, ok := l.buckets[hostID]
	if !ok {
		b = &imageTokenBucket{tokens: imageStateBucketBurst - 1, last: now}
		l.buckets[hostID] = b
		return true
	}
	elapsed := now.Sub(b.last).Seconds()
	b.last = now
	b.tokens += elapsed * imageStateBucketRefill
	if b.tokens > imageStateBucketBurst {
		b.tokens = imageStateBucketBurst
	}
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	// Reset the log-once gate so a later, separate flood produces its own line.
	delete(l.logged, hostID)
	return true
}

// shouldLog reports whether this is the first drop since hostID's last allowed
// message — a sustained flood produces one line, not one per drop.
func (l *imageStateLimiter) shouldLog(hostID string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.logged[hostID] {
		return false
	}
	l.logged[hostID] = true
	return true
}

// evict drops hostID's bucket and log-once state on disconnect, keeping the
// map bounded by the live connection set.
func (l *imageStateLimiter) evict(hostID string) {
	l.mu.Lock()
	delete(l.buckets, hostID)
	delete(l.logged, hostID)
	l.mu.Unlock()
}

// Image management P2 (protocol/agent-api.md §image_ensure, §image_remove,
// §image_state, §register `images`). Additive throughout: an un-upgraded fleet
// behaves byte-identically.

// --- upstream (agent → control plane) ----------------------------------------

// ImageStateMsg is the agent's managed-image presence/progress callback.
// Fire-and-forget (no ack) and image-presence authority ONLY — it never touches
// sessions. progress_pct/bytes are best-effort and meaningful mainly while
// pulling; error is non-empty only for state="failed".
type ImageStateMsg struct {
	Type        string `json:"type"` // "image_state"
	ImageID     string `json:"image_id"`
	Version     string `json:"version"`
	State       string `json:"state"` // absent|pulling|ready|failed (building reserved for P4)
	ProgressPct int32  `json:"progress_pct"`
	Bytes       int64  `json:"bytes"`
	Error       string `json:"error"`
}

// RegisterImage is one entry of the optional `register.images` array: what the
// agent actually has, verified against its own docker daemon, for image ids it
// was previously told to ensure.
type RegisterImage struct {
	ImageID string `json:"image_id"`
	Version string `json:"version"`
	State   string `json:"state"`
}

// ImageEvents is a parallel callback surface, not folded into Events: image
// presence is internal/images' concern, and widening Events would force every
// implementation and fake to grow methods it has no business having. agentws
// imports neither package; both sides depend on this interface.
type ImageEvents interface {
	// Upserts host_images for (hostID, image_id); an image_id not in
	// image_catalog is dropped, not stored (agent-api.md).
	AgentImageState(ctx context.Context, hostID string, m ImageStateMsg)
	// Fires once per (re)connect. reported=false ⇒ no `images` field (older
	// agent): stored host_images rows must stay unchanged. reported=true ⇒
	// wholesale snapshot, explicit empty array included ("I have none").
	AgentImagesRegistered(ctx context.Context, hostID string, images []RegisterImage, reported bool)
}

// noopImageEvents is used until an ImageEvents implementation is wired (focused
// tests, and any build that does not run the ensure orchestrator).
type noopImageEvents struct{}

func (noopImageEvents) AgentImageState(context.Context, string, ImageStateMsg) {}
func (noopImageEvents) AgentImagesRegistered(context.Context, string, []RegisterImage, bool) {
}

// --- downstream (control plane → agent) --------------------------------------

// ImageEnsureCmd tells the agent to make a prebuilt catalog image present in its
// own docker daemon. Reserve/prepare semantics like session_assign: the agent
// acks acceptance immediately and reports the actual pull via image_state.
// RegistryRef is always a concrete immutable ref (a sha- tag or digest), never a
// floating tag, so an ensure is deterministic and re-runnable.
type ImageEnsureCmd struct {
	Type        string `json:"type"` // "image_ensure"
	ID          string `json:"id"`
	ImageID     string `json:"image_id"`
	RegistryRef string `json:"registry_ref"`
	Version     string `json:"version"`
}

// ImageRemoveCmd asks the agent to best-effort remove a managed image. The agent
// never force-removes an image backing a live container.
type ImageRemoveCmd struct {
	Type    string `json:"type"` // "image_remove"
	ID      string `json:"id"`
	ImageID string `json:"image_id"`
}

// ImageBuildCmd tells the agent to build a template catalog image locally onto
// its own docker daemon, tag it LocalTag, and report progress/terminal state
// via image_state — the template analogue of ImageEnsureCmd
// (image-management P4). Reserve/prepare semantics like image_ensure: the
// agent acks acceptance immediately and reports the actual build via
// image_state.
type ImageBuildCmd struct {
	Type          string            `json:"type"` // "image_build"
	ID            string            `json:"id"`
	ImageID       string            `json:"image_id"`
	ContextURL    string            `json:"context_url"`
	ContextSubdir string            `json:"context_subdir"`
	Dockerfile    string            `json:"dockerfile"`
	BuildArgs     map[string]string `json:"build_args,omitempty"`
	LocalTag      string            `json:"local_tag"`
	Version       string            `json:"version"`
}

// SendImageEnsure dispatches an image_ensure and waits for the agent's ack. The
// ack means ACCEPTED, not done — the pull's outcome arrives asynchronously as
// image_state. A returned error means the command could not be delivered (agent
// gone, queue full) or no ack arrived before ctx expired.
func (r *Registry) SendImageEnsure(ctx context.Context, hostID, id, imageID, registryRef, version string) (AckResult, error) {
	return r.SendWithAck(ctx, hostID, id, ImageEnsureCmd{
		Type:        "image_ensure",
		ID:          id,
		ImageID:     imageID,
		RegistryRef: registryRef,
		Version:     version,
	})
}

// SendImageBuild dispatches an image_build and waits for the agent's ack. The
// ack means ACCEPTED, not built — the build's outcome arrives asynchronously as
// image_state (reusing the `building` state P2 reserved). A returned error
// means the command could not be delivered or no ack arrived before ctx
// expired.
func (r *Registry) SendImageBuild(ctx context.Context, hostID, id, imageID, contextURL, contextSubdir, dockerfile string, buildArgs map[string]string, localTag, version string) (AckResult, error) {
	return r.SendWithAck(ctx, hostID, id, ImageBuildCmd{
		Type:          "image_build",
		ID:            id,
		ImageID:       imageID,
		ContextURL:    contextURL,
		ContextSubdir: contextSubdir,
		Dockerfile:    dockerfile,
		BuildArgs:     buildArgs,
		LocalTag:      localTag,
		Version:       version,
	})
}

// SendImageRemove dispatches an image_remove and waits for the agent's ack. An
// image_id the agent has no record of acks ok:true and reports absent
// (idempotent) — removal is best effort by contract.
func (r *Registry) SendImageRemove(ctx context.Context, hostID, id, imageID string) (AckResult, error) {
	return r.SendWithAck(ctx, hostID, id, ImageRemoveCmd{
		Type:    "image_remove",
		ID:      id,
		ImageID: imageID,
	})
}

// ConnectedHosts lists the hosts with a live agent connection right now. The
// ensure orchestration uses it as its fleet view: an image can only be ensured
// onto a host whose agent is actually reachable, and a host that connects later
// picks the ensure up on its own register (AgentImagesRegistered).
func (r *Registry) ConnectedHosts() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.conns))
	for hostID := range r.conns {
		out = append(out, hostID)
	}
	return out
}
