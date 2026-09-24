package images

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/accreleus/quasar/control-plane/internal/agentws"
	"github.com/accreleus/quasar/control-plane/internal/preparation"
)

// Ensure orchestration (image-management P2). The control plane never pulls an
// image itself (invariant #1: no per-host state); it decides who should have
// what and dispatches image_ensure, and the agent reports back over
// image_state, the only writer of host_images.
//
// Two level-triggered (not edge-triggered) entry points: EnsureAll makes the
// adoption set true across the connected fleet without blocking on the pulls;
// AgentImagesRegistered reconciles a (re)connected host's report and ensures
// what it's missing, so an offline host converges on its own next connect with
// no queue to persist.

const (
	// Bounds the agent's acceptance ack only, not the pull itself (reported later
	// via image_state).
	defaultAckTimeout = 15 * time.Second
	// Modest on purpose: a failing pull is usually operator-actionable (disk
	// full, auth denied), and hammering the registry risks a rate-limit ban.
	defaultMaxAttempts = 3
	defaultRetryBase   = 30 * time.Second // doubles each further attempt
	// Bounds a post-commit lookup on the Ensurer's own lifecycle context (see
	// EnsureImage), so a wedged DB cannot pin a goroutine indefinitely.
	dbLookupTimeout              = 10 * time.Second
	requirementReconcileInterval = time.Minute
)

// Dispatcher is the seam onto live agent connections (*agentws.Registry in
// production) — a behaviour, not the websocket layer, so tests can drive a fake
// fleet with no sockets.
type Dispatcher interface {
	ConnectedHosts() []string
	SendImageEnsure(ctx context.Context, hostID, id, imageID, registryRef, version string) (agentws.AckResult, error)
	// SendImageBuild is the template analogue of image_ensure (P4).
	SendImageBuild(ctx context.Context, hostID, id, imageID, contextURL, contextSubdir, dockerfile string, buildArgs map[string]string, localTag, version string) (agentws.AckResult, error)
}

// warmupJobID is the jobs-framework id of the #488 golden-home warm-up; a
// literal (not an import) because this package must not depend on the jobs
// framework — the trigger is optional and best-effort (see enqueueWarmup).
const warmupJobID = "template.warmup"

// JobEnqueuer is the narrow seam onto the background-jobs dispatcher
// (*jobs.Dispatcher in production; app.go adapts it). The run row is
// deliberately not in the signature — this package has no use for it, and
// returning it would force an import for a trigger that must stay optional.
type JobEnqueuer interface {
	// EnqueueJob creates (or returns the already-open) event-triggered run for
	// jobID on hostID, carrying params verbatim.
	EnqueueJob(ctx context.Context, jobID, hostID string, params any) error
}

// EnsureOption configures an Ensurer at construction.
type EnsureOption func(*Ensurer)

// WithAckTimeout overrides how long an ensure waits for the agent's ack.
func WithAckTimeout(d time.Duration) EnsureOption {
	return func(e *Ensurer) {
		if d > 0 {
			e.ackTimeout = d
		}
	}
}

// WithRetry overrides the failure-retry budget: at most attempts re-dispatches
// per (host, image), the first after base and each subsequent one doubling.
// Tests use a tiny base; production takes the defaults.
func WithRetry(attempts int, base time.Duration) EnsureOption {
	return func(e *Ensurer) {
		if attempts >= 0 {
			e.maxAttempts = attempts
		}
		if base > 0 {
			e.retryBase = base
		}
	}
}

// dbPool: dbExecutor (pool and a transaction are interchangeable — see
// host_images.go) plus Begin, so reconcile can run in one transaction.
// *pgxpool.Pool satisfies it; tests use a wrapper that injects a
// mid-transaction failure to prove the rollback property.
type dbPool interface {
	dbExecutor
	Begin(ctx context.Context) (pgx.Tx, error)
}

// Ensurer implements agentws.ImageEvents and owns the ensure lifecycle.
type Ensurer struct {
	pool dbPool
	disp Dispatcher
	log  *slog.Logger

	ackTimeout  time.Duration
	maxAttempts int
	retryBase   time.Duration

	// ctx/cancel bound background dispatch to the Ensurer's own lifetime, not the
	// request/WS-read context that triggered it: an ensure must outlive the
	// admin's HTTP request and the agent's register message.
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu sync.Mutex
	// Set by SetJobEnqueuer (built after this Ensurer in app.go); nil makes the
	// #488 warm-up trigger absent, not broken. Guarded by mu — the WS read loop
	// reads it.
	warmup JobEnqueuer
	// closed guards wg.Add against racing wg.Wait() in Close() (a documented Go
	// WaitGroup misuse panic). Every path that calls wg.Add goes through
	// addWork(), which checks closed under mu.
	closed bool
	// Consecutive reported failures per (host, image); backoff is per target.
	// Reset on ready, on a reconcile reporting ready/absent, and when the image
	// leaves the adoption set — a recovered/uninstalled target must not carry a
	// stale retry budget.
	failures map[string]int
	// Epoch invalidates old automatic backoff timers when an operator retries,
	// the image succeeds, or a newer failure starts another timer.
	retryEpoch map[string]uint64
	// pending/active serialize ensures for one (host|image) target
	// through a single worker (`pending` = latest desired op, `active` = a
	// worker is draining it) so retries cannot overtake an in-flight ensure.
	pending map[string]*pendingOp
	active  map[string]bool
	// unsupported marks a host whose agent let an image_ensure ack time out.
	// agent-api.md: an unrecognized downstream message is silently ignored, so a
	// timeout means an older agent, not a transient failure — no further image
	// commands go to it until its next register, which clears the mark.
	unsupported map[string]bool
	// Suppresses repeat "marking host unsupported" log lines for an already-marked host.
	unsupportedLogged map[string]bool
	// An explicit Retry stays coalesced until the next authenticated state
	// observation. Concurrent HTTP requests cannot create separate budgets.
	retryOpen map[string]bool
	// Current-connection evidence is intentionally volatile. A stored ready or
	// progress row from a previous connection cannot claim present preparation.
	inventory map[string]*imageInventoryEvidence
}

type imageInventoryEvidence struct {
	snapshot bool
	observed map[string]bool
}

// SetJobEnqueuer wires the dispatcher so an image reaching `ready` enqueues a
// `template.warmup` run for that host. A setter, not an EnsureOption, only
// because the dispatcher is built after this Ensurer in app.go. nil disables
// the trigger.
func (e *Ensurer) SetJobEnqueuer(q JobEnqueuer) {
	e.mu.Lock()
	e.warmup = q
	e.mu.Unlock()
}

// addWork reserves one wg.Add(1) unit, refusing once closed. Every goroutine
// this package starts goes through this, never a bare wg.Add (see `closed`).
func (e *Ensurer) addWork() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return false
	}
	e.wg.Add(1)
	return true
}

// NewEnsurer builds an Ensurer. A nil logger becomes the default logger; a nil
// dispatcher makes every ensure a no-op (ingest still works) — the right
// behaviour for a control plane with no agent registry wired.
//
// Thin *pgxpool.Pool-typed wrapper over newEnsurer so production callers pass
// the concrete pool type while tests build against the narrower dbPool.
func NewEnsurer(pool *pgxpool.Pool, disp Dispatcher, log *slog.Logger, opts ...EnsureOption) *Ensurer {
	return newEnsurer(pool, disp, log, opts...)
}

// newEnsurer is the real constructor, built against dbPool so tests can pass a
// wrapper (e.g. one that fails mid-transaction, to prove the reconcile
// rollback property) without redeclaring NewEnsurer's defaults.
func newEnsurer(pool dbPool, disp Dispatcher, log *slog.Logger, opts ...EnsureOption) *Ensurer {
	if log == nil {
		log = slog.Default()
	}
	ctx, cancel := context.WithCancel(context.Background())
	e := &Ensurer{
		pool:              pool,
		disp:              disp,
		log:               log,
		ackTimeout:        defaultAckTimeout,
		maxAttempts:       defaultMaxAttempts,
		retryBase:         defaultRetryBase,
		ctx:               ctx,
		cancel:            cancel,
		failures:          make(map[string]int),
		retryEpoch:        make(map[string]uint64),
		pending:           make(map[string]*pendingOp),
		active:            make(map[string]bool),
		unsupported:       make(map[string]bool),
		unsupportedLogged: make(map[string]bool),
		retryOpen:         make(map[string]bool),
		inventory:         make(map[string]*imageInventoryEvidence),
	}
	for _, o := range opts {
		o(e)
	}
	return e
}

// CurrentImageEvidence tells operator reads whether the current authenticated
// connection has reported this image or a wholesale inventory. It never
// infers presence from a durable row alone after reconnect or restart.
func (e *Ensurer) CurrentImageEvidence(hostID, imageID string) (connected, observed, snapshot bool) {
	if e.disp == nil {
		return false, false, false
	}
	for _, id := range e.disp.ConnectedHosts() {
		if id == hostID {
			connected = true
			break
		}
	}
	if !connected {
		return false, false, false
	}
	e.mu.Lock()
	if inv := e.inventory[hostID]; inv != nil {
		observed, snapshot = inv.observed[imageID], inv.snapshot
	}
	e.mu.Unlock()
	return
}

func (e *Ensurer) observeImage(hostID, imageID string) {
	e.mu.Lock()
	inv := e.inventory[hostID]
	if inv == nil {
		inv = &imageInventoryEvidence{observed: make(map[string]bool)}
		e.inventory[hostID] = inv
	}
	inv.observed[imageID] = true
	e.mu.Unlock()
}

func (e *Ensurer) observeSnapshot(hostID string, imageIDs []string) {
	e.mu.Lock()
	inv := e.inventory[hostID]
	if inv == nil {
		inv = &imageInventoryEvidence{observed: make(map[string]bool)}
		e.inventory[hostID] = inv
	}
	inv.snapshot = true
	for _, imageID := range imageIDs {
		inv.observed[imageID] = true
	}
	e.mu.Unlock()
}

// Close stops accepting new work and cancels in-flight dispatches. Optional in
// production (one process, exits anyway); tests build many.
//
// closed must be set before cancel()/wg.Wait(): mu serializes it against any
// racing addWork(), so every call either completes its wg.Add() first or
// observes closed and skips it — wg.Add() can never run concurrently with
// wg.Wait().
func (e *Ensurer) Close() {
	e.mu.Lock()
	e.closed = true
	e.mu.Unlock()
	e.cancel()
	e.wg.Wait()
}

// Wait blocks until every dispatch goroutine started so far has finished. Test
// seam only — production never calls it.
func (e *Ensurer) Wait() { e.wg.Wait() }

// RunRequirementReconcile is the bounded level-triggered safety net for lost
// placement/adoption notifications and provider-created apps. Desired work is
// the committed Postgres selection, so no separate volatile queue is needed.
func (e *Ensurer) RunRequirementReconcile(ctx context.Context) {
	ticker := time.NewTicker(requirementReconcileInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			scanCtx, cancel := context.WithTimeout(ctx, dbLookupTimeout)
			if err := e.EnsureAll(scanCtx); err != nil {
				e.log.Warn("image requirement reconcile deferred", "err", err)
			}
			cancel()
		}
	}
}

// EnsureAll reconciles each connected host's selected, adopted requirements. It returns as soon
// as the work is SCHEDULED — the pulls themselves are asynchronous by contract,
// so no request thread ever waits on one. An error means the adoption set could
// not be read at all.
func (e *Ensurer) EnsureAll(ctx context.Context) error {
	if e.disp == nil {
		return nil
	}
	var failures []error
	for _, hostID := range e.disp.ConnectedHosts() {
		if err := e.EnsureHost(ctx, hostID); err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

// EnsureImage is EnsureAll narrowed to one image (the P3 install/update path).
// A lazy adoption resolves to nothing (installedNonLazy excludes it), so a lazy
// install and a lazy update need no separate code path.
func (e *Ensurer) EnsureImage(_ context.Context, imageID string) {
	if e.disp == nil || imageID == "" {
		return
	}
	// ctx is deliberately ignored: this runs after the install/update commit, so
	// a client disconnect canceling the request must not cancel the ensure too.
	// Bounded on the Ensurer's own lifecycle context instead.
	ctx, cancel := context.WithTimeout(e.ctx, dbLookupTimeout)
	defer cancel()
	for _, hostID := range e.disp.ConnectedHosts() {
		img, required, err := requiredAdoptionForHost(ctx, e.pool, hostID, imageID)
		if err != nil {
			e.log.Warn("ensure image: requirement lookup failed", "image_id", imageID, "err", err)
			continue
		}
		if required {
			e.ensureHostImages(ctx, hostID, []installedImage{img})
		}
	}
}

// pendingOp is the latest desired action for one (host|image) target.
type pendingOp struct {
	force bool // explicit or bounded retry may resume a current failed row
	img   installedImage
}

// enqueue records op as the latest desired action for (host|image) and starts a
// worker if one isn't already running. Latest-write-wins lets a retry
// supersede queued ordinary work; the worker re-reads `pending` after each op.
func (e *Ensurer) enqueue(hostID, imageID string, op pendingOp) {
	key := hostID + "|" + imageID
	e.mu.Lock()
	if e.unsupported[hostID] {
		e.mu.Unlock()
		return
	}
	e.pending[key] = &op
	if e.active[key] {
		e.mu.Unlock()
		return
	}
	e.active[key] = true
	e.mu.Unlock()

	if !e.addWork() {
		e.mu.Lock()
		delete(e.active, key)
		delete(e.pending, key)
		e.mu.Unlock()
		return
	}
	go e.drainTarget(hostID, imageID, key)
}

// drainTarget is the single worker for one (host|image) target: runs the
// pending op, re-checks for a newer one, until none remains — one worker per
// target serializes ensure and remove.
func (e *Ensurer) drainTarget(hostID, imageID, key string) {
	defer e.wg.Done()
	for {
		e.mu.Lock()
		op := e.pending[key]
		if op == nil {
			delete(e.active, key)
			e.mu.Unlock()
			return
		}
		delete(e.pending, key)
		e.mu.Unlock()

		e.runEnsure(hostID, imageID, op.img, op.force)
	}
}

// runEnsure re-reads the adoption immediately before dispatching. A placement
// removal or uninstall must not resurrect a queued pull.
func (e *Ensurer) runEnsure(hostID, imageID string, _ installedImage, force bool) {
	ctx, cancel := context.WithTimeout(e.ctx, dbLookupTimeout)
	cur, required, err := requiredAdoptionForHost(ctx, e.pool, hostID, imageID)
	cancel()
	if err != nil {
		// A queued snapshot is no longer authority after placement or adoption
		// changes. A DB failure cannot authorize a stale pull.
		e.log.Warn("ensure: requirement re-read failed; deferring dispatch", "host_id", hostID, "image_id", imageID, "err", err)
		e.closeRetry(hostID + "|" + imageID)
		return
	}
	if !required {
		e.clearFailures(hostID + "|" + imageID)
		e.closeRetry(hostID + "|" + imageID)
		return // placement removal leaves cache intact
	}
	checkCtx, checkCancel := context.WithTimeout(e.ctx, dbLookupTimeout)
	defer checkCancel()
	needed, err := hostNeedsImage(checkCtx, e.pool, hostID, imageID, cur.Version)
	if err != nil {
		e.log.Warn("ensure: current image state lookup failed; deferring dispatch", "host_id", hostID, "image_id", imageID, "err", err)
		e.closeRetry(hostID + "|" + imageID)
		return
	}
	if !needed && force {
		var state, version string
		err = e.pool.QueryRow(checkCtx, `SELECT state,version FROM host_images WHERE host_id=$1::uuid AND image_id=$2`, hostID, imageID).Scan(&state, &version)
		if err != nil {
			e.log.Warn("ensure: forced retry state lookup failed; deferring dispatch", "host_id", hostID, "image_id", imageID, "err", err)
		}
		force = err == nil && state == "failed" && (version == "" || version == cur.Version)
	}
	if !needed && !force {
		// The requirement can remain while another attempt reported a
		// current failure. Only explicit Retry may bypass that suppression.
		e.closeRetry(hostID + "|" + imageID)
		return
	}
	// A queued ensure may have waited behind another image operation. The
	// durable fence, rather than the queue's old snapshot, decides whether it
	// may be sent now. A failed lookup also defers the command.
	removing, fenceErr := imageRemoving(checkCtx, e.pool, hostID, imageID)
	if fenceErr != nil || removing {
		if fenceErr != nil {
			e.log.Warn("ensure: image fence lookup failed; deferring dispatch", "host_id", hostID, "image_id", imageID, "err", fenceErr)
		}
		e.closeRetry(hostID + "|" + imageID)
		return
	}
	if !e.sendEnsure(hostID, cur) {
		// A definite rejection or undeliverable command did not start work.
		// The operator may retry once that failed dispatch has drained.
		e.closeRetry(hostID + "|" + imageID)
	}
	// Acceptance starts one physical attempt. The failed DB row is still
	// current until the agent reports image_state, so keep Retry coalesced.
}

func (e *Ensurer) closeRetry(key string) {
	e.mu.Lock()
	delete(e.retryOpen, key)
	e.mu.Unlock()
}

// EnsureHost is EnsureAll narrowed to one host — the reconnect path, and the
// reason an image installed while a host was offline still lands on it.
func (e *Ensurer) EnsureHost(ctx context.Context, hostID string) error {
	if e.disp == nil {
		return nil
	}
	imgs, err := requiredImagesForHost(ctx, e.pool, hostID)
	if err != nil {
		return err
	}
	e.ensureHostImages(ctx, hostID, imgs)
	return nil
}

// ensureHostImages schedules the ensures this host is missing. The readiness
// check is a DB read on the caller's context (cheap, indexed by the PK); the
// dispatch is a goroutine on the Ensurer's context.
func (e *Ensurer) ensureHostImages(ctx context.Context, hostID string, imgs []installedImage) {
	for _, img := range imgs {
		needed, err := hostNeedsImage(ctx, e.pool, hostID, img.ImageID, img.Version)
		if err != nil {
			e.log.Warn("ensure: host image lookup failed", "host_id", hostID, "image_id", img.ImageID, "err", err)
			continue
		}
		if !needed {
			continue
		}
		e.dispatch(hostID, img)
	}
}

// dispatch enqueues an image_ensure for (host, image), serialized behind any
// in-flight op for the same target (see enqueue) so it can never race a remove.
func (e *Ensurer) dispatch(hostID string, img installedImage) {
	e.enqueue(hostID, img.ImageID, pendingOp{img: img})
}

// markUnsupported records that hostID's agent does not answer image_ensure and
// logs it exactly once per marking (cleared on the next register — see
// AgentImagesRegistered).
func (e *Ensurer) markUnsupported(hostID string) {
	e.mu.Lock()
	already := e.unsupported[hostID]
	e.unsupported[hostID] = true
	logNow := !already && !e.unsupportedLogged[hostID]
	if logNow {
		e.unsupportedLogged[hostID] = true
	}
	e.mu.Unlock()
	if logNow {
		e.log.Warn("ensure: host marked image-unsupported (ack timeout); no further image commands until next register", "host_id", hostID)
	}
}

// splitDockerfilePath renders image_catalog.dockerfile (one repo-root-relative
// path, e.g. "steam/Dockerfile") into the (context_subdir, dockerfile) pair
// image_build wants (protocol/agent-api.md §image_build): everything before
// the last slash / everything after. A bare "Dockerfile" splits to
// context_subdir="." (repo root is the build context).
func splitDockerfilePath(df string) (contextSubdir, dockerfile string) {
	df = strings.TrimSpace(df)
	if df == "" {
		df = "Dockerfile"
	}
	if i := strings.LastIndex(df, "/"); i >= 0 {
		return df[:i], df[i+1:]
	}
	return ".", df
}

// parseBuildArgs decodes the frozen installed_images.build_args (validated as
// strings at sync, manifest.go) into image_build's map[string]string. Errors
// rather than dropping to empty: a build_args that won't decode to
// string=>string would silently build the wrong image argless, so the caller
// must record a failed state instead. Absent/empty/null is valid "no args".
func parseBuildArgs(raw json.RawMessage) (map[string]string, error) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return nil, nil
	}
	var m map[string]string
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("decode build_args: %w", err)
	}
	if len(m) == 0 {
		return nil, nil
	}
	return m, nil
}

// contextURLFor renders image_build's context_url: a codeload tarball of the
// frozen img.ContextRepo pinned to the frozen img.ContextSHA (a commit sha,
// never a floating ref — protocol/agent-api.md §image_build), both read from
// installed_images, never a live env snapshot.
func (e *Ensurer) contextURLFor(img installedImage) string {
	return fmt.Sprintf("https://codeload.github.com/%s/tar.gz/%s", img.ContextRepo, img.ContextSHA)
}

// sendEnsure dispatches whichever downstream command img's adoption calls for:
// image_ensure for a prebuilt (RegistryRef set), image_build for a template
// (LocalTag set, P4) — the one place that decides ensure vs build.
func (e *Ensurer) sendEnsure(hostID string, img installedImage) bool {
	if img.LocalTag != "" && img.ContextSHA == "" {
		// Unresolved context sha (sync failure, or hasn't run since the
		// dockerfile changed) — un-actionable, same posture as Install/Update's
		// 409 context_unresolved. No host_images row written, as with an ensure
		// never dispatched.
		e.log.Warn("ensure: template context sha unresolved; skipping build dispatch",
			"host_id", hostID, "image_id", img.ImageID)
		return false
	}
	cmdID, err := newCmdID()
	if err != nil {
		// Must not fall back to a colliding zero id; skip and let the
		// reconnect/next-trigger path retry.
		e.log.Error("ensure: command id generation failed; skipping dispatch",
			"host_id", hostID, "image_id", img.ImageID, "err", err)
		return false
	}
	ctx, cancel := context.WithTimeout(e.ctx, e.ackTimeout)
	defer cancel()
	var res agentws.AckResult
	if img.LocalTag != "" {
		buildArgs, perr := parseBuildArgs(img.BuildArgs)
		if perr != nil {
			// build_args is validated at sync and frozen verbatim, so a decode
			// failure here is a genuine, operator-actionable defect: record
			// failed rather than silently dispatch a build with no args.
			e.log.Error("ensure: frozen build_args decode failed; recording failed instead of dispatching argless",
				"host_id", hostID, "image_id", img.ImageID, "err", perr)
			stored, uerr := upsertHostImage(e.ctx, e.pool, hostID, img.ImageID, img.Version, "failed",
				truncateImageError("build_args decode: "+perr.Error()), nil)
			if uerr != nil {
				e.log.Error("ensure: record build_args failure failed", "host_id", hostID, "image_id", img.ImageID, "err", uerr)
			} else if !stored {
				e.log.Warn("ensure: build_args failure for unknown image dropped", "host_id", hostID, "image_id", img.ImageID)
			}
			return false
		}
		contextSubdir, dockerfile := splitDockerfilePath(img.Dockerfile)
		res, err = e.disp.SendImageBuild(ctx, hostID, cmdID, img.ImageID,
			e.contextURLFor(img), contextSubdir, dockerfile, buildArgs, img.LocalTag, img.Version)
	} else {
		res, err = e.disp.SendImageEnsure(ctx, hostID, cmdID, img.ImageID, img.RegistryRef, img.Version)
	}
	if err != nil {
		// Undeliverable (agent gone, queue full, no ack) is NOT recorded as
		// failed: nothing was attempted, and the reconnect path re-ensures.
		e.log.Warn("ensure: dispatch failed", "host_id", hostID, "image_id", img.ImageID, "err", err)
		// A timeout specifically (agent alive but never acks image_ensure)
		// means an older agent predating image management (agent-api.md:
		// unrecognized message = silent ignore). Mark unsupported until its next
		// register. errors.Is, not ==: ctx.Err() is wrapped by SendWithAck.
		if errors.Is(err, context.DeadlineExceeded) {
			e.markUnsupported(hostID)
		}
		return false
	}
	if !res.OK {
		// ack{ok:false} is un-actionable on its face; agent-api.md reserves
		// runtime failures for image_state. Retrying changes nothing, so record
		// failed and leave it.
		e.log.Warn("ensure: agent rejected", "host_id", hostID, "image_id", img.ImageID, "err", res.Error)
		// res.Error is our own ack decode, not the validated image_state stream
		// (handler.go's validateImageState) — must bound it ourselves.
		stored, err := upsertHostImage(e.ctx, e.pool, hostID, img.ImageID, img.Version, "failed", truncateImageError(res.Error), nil)
		if err != nil {
			e.log.Error("ensure: record rejection failed", "host_id", hostID, "image_id", img.ImageID, "err", err)
		} else if !stored {
			e.log.Warn("ensure: rejection for unknown image dropped", "host_id", hostID, "image_id", img.ImageID)
		}
		return false
	}
	e.log.Info("ensure: accepted", "host_id", hostID, "image_id", img.ImageID, "version", img.Version)
	return true
}

// --- agentws.ImageEvents ------------------------------------------------------

// AgentImageState ingests one image_state report: upserts host_images and, on
// failure, schedules a bounded retry. An image_id not in image_catalog is
// dropped, not stored (agent-api.md) — an agent must not create catalog rows
// by reporting on them.
func (e *Ensurer) AgentImageState(ctx context.Context, hostID string, m agentws.ImageStateMsg) {
	if hostID == "" || m.ImageID == "" {
		return
	}
	if !hostImageStates[m.State] {
		e.log.Warn("image_state: unknown state dropped", "host_id", hostID, "image_id", m.ImageID, "state", m.State)
		return
	}
	errMsg := m.Error
	if m.State != "failed" {
		errMsg = "" // error is non-empty only for failed (agent-api.md)
	}
	var bytes *int64
	if m.Bytes > 0 {
		b := m.Bytes
		bytes = &b
	}
	tx, err := e.pool.Begin(ctx)
	if err != nil {
		e.log.Error("image_state: begin failed", "host_id", hostID, "image_id", m.ImageID, "err", err)
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()
	stored, err := upsertHostImage(ctx, tx, hostID, m.ImageID, m.Version, m.State, errMsg, bytes)
	if err != nil {
		e.log.Error("image_state: upsert failed", "host_id", hostID, "image_id", m.ImageID, "err", err)
		return
	}
	if !stored {
		e.log.Warn("image_state: unknown image_id dropped", "host_id", hostID, "image_id", m.ImageID)
		return
	}
	if m.State == "ready" {
		if err := recordSuccessfulVersion(ctx, tx, hostID, m.ImageID, m.Version); err != nil {
			e.log.Error("image_state: history failed", "host_id", hostID, "image_id", m.ImageID, "err", err)
			return
		}
	}
	if err := tx.Commit(ctx); err != nil {
		e.log.Error("image_state: commit failed", "host_id", hostID, "image_id", m.ImageID, "err", err)
		return
	}
	e.observeImage(hostID, m.ImageID)

	key := hostID + "|" + m.ImageID
	e.mu.Lock()
	delete(e.retryOpen, key)
	e.mu.Unlock()
	switch m.State {
	case "ready":
		e.clearFailures(key)
		e.enqueueWarmup(hostID, m.ImageID)
	case "failed":
		e.scheduleRetry(hostID, m.ImageID, key)
	}
}

// RetryHostImage re-arms one reported failure after the operator asks for it.
// The database is authoritative for identity, selection, adoption and failure;
// a 202 only schedules work and never claims preparation succeeded.
func (e *Ensurer) RetryHostImage(ctx context.Context, hostID, imageID string) error {
	var hostExists, catalogExists, installed, lazy bool
	if err := e.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM hosts WHERE id=$1::uuid),
		EXISTS(SELECT 1 FROM image_catalog WHERE id=$2),
		EXISTS(SELECT 1 FROM installed_images WHERE image_id=$2),
		COALESCE((SELECT lazy FROM installed_images WHERE image_id=$2),false)`, hostID, imageID).
		Scan(&hostExists, &catalogExists, &installed, &lazy); err != nil {
		return err
	}
	if !hostExists || !catalogExists {
		return ErrNotFound
	}
	if !installed {
		return ErrNotInstalled
	}
	if e.disp == nil {
		return ErrRetryOffline
	}
	online := false
	for _, id := range e.disp.ConnectedHosts() {
		if id == hostID {
			online = true
			break
		}
	}
	if !online {
		return ErrRetryOffline
	}
	if lazy {
		return ErrRetryLazy
	}
	img, required, err := requiredAdoptionForHost(ctx, e.pool, hostID, imageID)
	if err != nil {
		return err
	}
	if !required {
		return ErrRetryNotRequired
	}
	// Serialize Retry with a cleanup attempt's fence transition. An unlocked
	// read could accept a retry while another transaction has already changed
	// the fence to removing but has not committed yet. Dispatch rechecks again
	// after this short transaction, so no DB lock crosses the agent call.
	tx, err := e.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `INSERT INTO host_image_operation_fences(host_id,image_id,state)
		VALUES($1::uuid,$2,'idle') ON CONFLICT DO NOTHING`, hostID, imageID); err != nil {
		return err
	}
	var fenceState string
	if err := tx.QueryRow(ctx, `SELECT state FROM host_image_operation_fences
		WHERE host_id=$1::uuid AND image_id=$2 FOR SHARE`, hostID, imageID).Scan(&fenceState); err != nil {
		return err
	}
	if fenceState == "removing" {
		return ErrRetryRemoving
	}
	var state, version string
	err = tx.QueryRow(ctx, `SELECT state,version FROM host_images WHERE host_id=$1::uuid AND image_id=$2`, hostID, imageID).Scan(&state, &version)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrRetryNotFailed
	}
	if err != nil {
		return err
	}
	if state != "failed" || (version != "" && version != img.Version) {
		return ErrRetryNotFailed
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	key := hostID + "|" + imageID
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return context.Canceled
	}
	// The explicit request starts one immediate attempt and a fresh bounded
	// automatic retry budget for this host/image only. Even when it merges
	// into an automatic attempt already in flight, the new budget applies.
	delete(e.failures, key)
	e.retryEpoch[key]++
	// A prior acceptance timeout suppresses automatic work on this connection.
	// An explicit operator retry is itself one bounded new attempt.
	delete(e.unsupported, hostID)
	delete(e.unsupportedLogged, hostID)
	if e.retryOpen[key] {
		e.mu.Unlock()
		return nil
	}
	e.retryOpen[key] = true
	e.mu.Unlock()
	e.enqueue(hostID, imageID, pendingOp{img: img, force: true})
	return nil
}

var (
	ErrRetryOffline     = errors.New("image retry: host offline")
	ErrRetryNotRequired = errors.New("image retry: not required")
	ErrRetryLazy        = errors.New("image retry: lazy")
	ErrRetryNotFailed   = errors.New("image retry: not failed at adopted version")
	ErrRetryRemoving    = errors.New("image retry: cleanup in progress")
)

// WarmupParamsForHost resolves `template.warmup` params for a MANUAL run on
// hostID (jobs framework `Definition.ResolveParams` hook). Resolves off the
// same adoption columns as the event path (enqueueWarmup) so a manual and an
// event-triggered run send identical params. Warm-up ordering when a host has
// several candidates: managed-home images first, then most-recently-ready,
// then image id. Not best-effort like the event path: refuses with a reason
// rather than queue a run that can't succeed.
func (e *Ensurer) WarmupParamsForHost(ctx context.Context, hostID string) (any, error) {
	if strings.TrimSpace(hostID) == "" {
		return nil, fmt.Errorf("a golden-home warm-up is host-scoped and needs a host_id")
	}
	return preparation.Params(ctx, e.pool, hostID)
}

// enqueueWarmup is the #488 golden-home warm-up trigger. Lives here because
// this is the single point where the control plane learns an image reached
// `ready` on a host — before adoption the agent drove this from its own
// scheduler thread, with nowhere to keep the run window/schedule/record.
//
// Best-effort by design: the warm-up is a background optimization and must
// never affect whether an image is ready. Every failure mode (no enqueuer, an
// unregistered job, a DB hiccup, no dispatchable adoption) is just a log line.
func (e *Ensurer) enqueueWarmup(hostID, imageID string) {
	e.mu.Lock()
	q := e.warmup
	e.mu.Unlock()
	if q == nil {
		return
	}
	if !e.addWork() {
		return
	}
	go func() {
		defer e.wg.Done()
		ctx, cancel := context.WithTimeout(e.ctx, dbLookupTimeout)
		defer cancel()

		params, err := preparation.Params(ctx, e.pool, hostID)
		if err != nil || imageID != "steam" {
			return
		}
		var finished bool
		if err = e.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM hosts
         WHERE id=$1::uuid AND source_preparation->'steam'->>'policy_revision'=$2
         AND source_preparation->'steam'->'images'->0->>'registry_ref'=$3
         AND source_preparation->'steam'->'images'->0->>'version'=$4
         AND source_preparation->'steam'->'images'->0->>'state' IN ('ready','preparing'))`, hostID, params["policy_revision"], params["registry_ref"], params["version"]).Scan(&finished); err != nil || finished {
			return
		}

		if err = q.EnqueueJob(ctx, warmupJobID, hostID, params); err != nil {
			e.log.Debug("Steam preparation not enqueued", "host_id", hostID, "err", err)
		}

	}()
}

// scheduleRetry re-dispatches a failed ensure after exponential backoff, up to
// maxAttempts per (host, image); beyond that the row is left `failed` (visible
// in GET /v1/admin/images) for an operator-actionable failure.
func (e *Ensurer) scheduleRetry(hostID, imageID, key string) {
	if e.disp == nil || e.maxAttempts <= 0 {
		return
	}
	e.mu.Lock()
	e.failures[key]++
	n := e.failures[key]
	e.retryEpoch[key]++
	epoch := e.retryEpoch[key]
	e.mu.Unlock()
	if n > e.maxAttempts {
		e.log.Warn("ensure: retry budget exhausted; leaving image failed",
			"host_id", hostID, "image_id", imageID, "attempts", n-1)
		return
	}
	delay := e.retryBase << (n - 1)

	if !e.addWork() {
		return
	}
	go func() {
		defer e.wg.Done()
		t := time.NewTimer(delay)
		defer t.Stop()
		select {
		case <-e.ctx.Done():
			return
		case <-t.C:
		}
		e.mu.Lock()
		stale := e.retryEpoch[key] != epoch || e.retryOpen[key]
		e.mu.Unlock()
		if stale {
			return
		}
		// Re-read instead of caching the ref: the image may have been
		// uninstalled/re-pinned while this retry waited, and a retry must never
		// resurrect a withdrawn ensure.
		imgs, err := requiredImagesForHost(e.ctx, e.pool, hostID)
		if err != nil {
			e.log.Warn("ensure: retry lookup failed", "host_id", hostID, "image_id", imageID, "err", err)
			return
		}
		for _, img := range imgs {
			if img.ImageID != imageID {
				continue
			}
			ready, err := hostHasImage(e.ctx, e.pool, hostID, img.ImageID, img.Version)
			if err != nil {
				return
			}
			if ready {
				e.clearFailures(key) // became ready via another path; fresh budget next time
				return
			}
			// Claim the attempt before dispatch. RetryHostImage can then coalesce
			// with it even if the operator request arrives after the DB read.
			e.mu.Lock()
			if e.retryEpoch[key] != epoch || e.retryOpen[key] {
				e.mu.Unlock()
				return
			}
			if e.unsupported[hostID] {
				e.mu.Unlock()
				return // operator Retry may clear this connection-local mark
			}
			e.retryOpen[key] = true
			e.mu.Unlock()
			e.enqueue(hostID, imageID, pendingOp{img: img, force: true})
			return
		}
		e.clearFailures(key) // image left the adoption set; don't leave a poisoned counter
	}()
}

// clearFailures drops the retry-failure counter for key (host|image).
func (e *Ensurer) clearFailures(key string) {
	e.mu.Lock()
	delete(e.failures, key)
	e.retryEpoch[key]++
	e.mu.Unlock()
}

// AgentImagesRegistered reconciles this host's rows against the agent's
// wholesale report, then ensures whatever it's missing. Runs on a goroutine:
// this fires from the agent WS read loop during registration, and neither a
// slow DB nor an ack round-trip may delay a host coming online.
func (e *Ensurer) AgentImagesRegistered(_ context.Context, hostID string, imgs []agentws.RegisterImage, reported bool) {
	if hostID == "" {
		return
	}
	// A fresh register clears the unsupported mark: a redeployed/upgraded agent
	// must not stay branded forever over one pre-upgrade timeout.
	e.mu.Lock()
	delete(e.unsupported, hostID)
	delete(e.unsupportedLogged, hostID)
	delete(e.inventory, hostID)
	for key := range e.retryOpen {
		if strings.HasPrefix(key, hostID+"|") {
			delete(e.retryOpen, key)
		}
	}
	e.mu.Unlock()

	if !e.addWork() {
		return
	}
	go func() {
		defer e.wg.Done()
		if reported {
			if err := e.reconcile(e.ctx, hostID, imgs); err != nil {
				// A failed reconcile must not leave EnsureHost trusting stale
				// host_images rows (a host reporting absent, whose demote never
				// committed, would stay schedulable). Bypass the readiness check
				// and dispatch every adopted image directly — image_ensure is
				// idempotent (agent-api.md), so a redundant dispatch is harmless.
				e.log.Error("register images: reconciliation failed; dispatching all adopted images directly, bypassing stale host_images rows",
					"host_id", hostID, "err", err)
				e.dispatchAllAdopted(e.ctx, hostID)
				return
			}
		} else {
			// A legacy register has no wholesale image snapshot. It cannot
			// attest that a previous connection's pull/build is still active.
			if _, err := demoteUnreportedImages(e.ctx, e.pool, hostID, nil, []string{"pulling", "building"}); err != nil {
				e.log.Warn("register images: stale progress demotion failed", "host_id", hostID, "err", err)
			}
		}
		// Runs whether or not the agent reported: an older agent that can't
		// report still needs the image, and image_ensure is idempotent.
		if err := e.EnsureHost(e.ctx, hostID); err != nil {
			e.log.Warn("ensure on register failed", "host_id", hostID, "err", err)
		}
	}()
}

// dispatchAllAdopted dispatches image_ensure for every adopted non-lazy image
// to hostID unconditionally (no readiness check) — only used on the
// reconcile-failure fallback above, where host_images can't be trusted.
func (e *Ensurer) dispatchAllAdopted(ctx context.Context, hostID string) {
	if e.disp == nil {
		return
	}
	imgs, err := requiredImagesForHost(ctx, e.pool, hostID)
	if err != nil {
		e.log.Warn("ensure on register failed (reconcile-failure fallback)", "host_id", hostID, "err", err)
		return
	}
	for _, img := range imgs {
		e.dispatch(hostID, img)
	}
}

// reconcile applies the agent's wholesale `register.images` snapshot: reported
// ids are written as stated, and anything believed ready but omitted from the
// report is flipped to absent — trusting our own memory would let a host that
// lost an image keep attracting placements.
//
// The snapshot runs in one transaction so the demotion and successful writes
// commit together. A savepoint around each reported image lets an independent
// valid image complete even if another image's history or inventory write
// fails. The failed image is omitted from seen and cannot appear ready.
func (e *Ensurer) reconcile(ctx context.Context, hostID string, imgs []agentws.RegisterImage) error {
	tx, err := e.pool.Begin(ctx)
	if err != nil {
		e.log.Error("register images: begin transaction failed", "host_id", hostID, "err", err)
		return fmt.Errorf("begin transaction: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(ctx)
		}
	}()

	seen := make([]string, 0, len(imgs))
	// clearKeys: (host,image) retry counters to drop once committed (reported
	// ready/absent). Deferred past commit so a rolled-back reconcile never
	// clears a counter for state that was never applied.
	clearKeys := make([]string, 0, len(imgs))
	for _, img := range imgs {
		if img.ImageID == "" {
			continue
		}
		if !hostImageStates[img.State] {
			e.log.Warn("register images: unknown state dropped", "host_id", hostID, "image_id", img.ImageID, "state", img.State)
			continue
		}
		if _, err := tx.Exec(ctx, `SAVEPOINT image_report`); err != nil {
			return fmt.Errorf("savepoint registered image=%s: %w", img.ImageID, err)
		}
		stored, err := upsertHostImage(ctx, tx, hostID, img.ImageID, img.Version, img.State, "", nil)
		if err != nil {
			if _, rollbackErr := tx.Exec(ctx, `ROLLBACK TO SAVEPOINT image_report`); rollbackErr != nil {
				return fmt.Errorf("rollback failed image=%s: %w", img.ImageID, rollbackErr)
			}
			if _, releaseErr := tx.Exec(ctx, `RELEASE SAVEPOINT image_report`); releaseErr != nil {
				return fmt.Errorf("release failed image=%s: %w", img.ImageID, releaseErr)
			}
			e.log.Error("register images: upsert failed; retaining other image reports", "host_id", hostID, "image_id", img.ImageID, "err", err)
			continue
		}
		if !stored {
			e.log.Warn("register images: unknown image_id dropped", "host_id", hostID, "image_id", img.ImageID)
			if _, err := tx.Exec(ctx, `RELEASE SAVEPOINT image_report`); err != nil {
				return fmt.Errorf("release unknown registered image=%s: %w", img.ImageID, err)
			}
			continue
		}
		if img.State == "ready" {
			if err := recordSuccessfulVersion(ctx, tx, hostID, img.ImageID, img.Version); err != nil {
				if _, rollbackErr := tx.Exec(ctx, `ROLLBACK TO SAVEPOINT image_report`); rollbackErr != nil {
					return fmt.Errorf("rollback failed history image=%s: %w", img.ImageID, rollbackErr)
				}
				if _, releaseErr := tx.Exec(ctx, `RELEASE SAVEPOINT image_report`); releaseErr != nil {
					return fmt.Errorf("release failed history image=%s: %w", img.ImageID, releaseErr)
				}
				e.log.Error("register images: history failed; retaining other image reports", "host_id", hostID, "image_id", img.ImageID, "err", err)
				continue
			}
		}
		if _, err := tx.Exec(ctx, `RELEASE SAVEPOINT image_report`); err != nil {
			return fmt.Errorf("release registered image=%s: %w", img.ImageID, err)
		}
		seen = append(seen, img.ImageID)
		if img.State == "ready" || img.State == "absent" {
			clearKeys = append(clearKeys, hostID+"|"+img.ImageID)
		}
	}
	demoted, err := demoteUnreportedImages(ctx, tx, hostID, seen, []string{"ready", "pulling", "building"})
	if err != nil {
		e.log.Error("register images: demote failed; rolling back reconciliation", "host_id", hostID, "err", err)
		return fmt.Errorf("demote unreported ready: %w", err)
	}
	for _, imageID := range demoted {
		clearKeys = append(clearKeys, hostID+"|"+imageID)
	}
	if err := tx.Commit(ctx); err != nil {
		e.log.Error("register images: commit failed", "host_id", hostID, "err", err)
		return fmt.Errorf("commit reconciliation: %w", err)
	}
	committed = true
	e.observeSnapshot(hostID, seen)
	for _, key := range clearKeys {
		e.clearFailures(key)
	}
	return nil
}

// ReconcilePreparation retries admission after a policy acknowledgement.
func (e *Ensurer) ReconcilePreparation(hostID string) { e.enqueueWarmup(hostID, "steam") }
