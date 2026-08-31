package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/accreleus/quasar/control-plane/internal/storage"
)

// HomeProvider is the storage seam (P5-02): the per-(user, app) home mount at
// dispatch time, and usage stamped on session end. Implemented by
// internal/storage.Manager; an interface so lifecycle tests can run without one.
type HomeProvider interface {
	EnsureHome(ctx context.Context, userID, appID, hostID, containerPath string) (string, error)
	// RequireHome resolves an EXISTING home and refuses when there is none. It
	// must NOT create and must NOT un-tombstone — an implementation that falls
	// back to EnsureHome silently provisions an empty Steam library. Returns
	// storage.ErrHomeNotProvisioned on a miss.
	//
	// appID here is always the home-OWNING app (homeAppID), never a derived tile.
	RequireHome(ctx context.Context, userID, appID, hostID, containerPath string) (string, error)
	TouchUsed(ctx context.Context, userID, appID, hostID string) error
	// ReportBytesUsed updates user_homes.bytes_used, best-effort on the
	// pre-terminal metrics sample; a miss is silently ignored.
	ReportBytesUsed(ctx context.Context, sessionID string, bytesUsed int64) error
}

// CoordinatorOption customizes NewCoordinator without breaking existing callers.
type CoordinatorOption func(*Coordinator)

// MicCaptureProvider is the mic-capture instance gate (migration 0049),
// implemented by internal/settings.Store.MicCaptureEnabled. Read PER LAUNCH,
// never cached, as LibraryDiscoveryEnabled is.
type MicCaptureProvider interface {
	MicCaptureEnabled(ctx context.Context) (bool, error)
}

// SessionForgetter is the seam for "this session is over, drop what you held for
// it" (#402). Implemented by agentws.RelayBus but declared here as an interface:
// agentws already imports session, so session must never import it back.
type SessionForgetter interface {
	Forget(sessionID string)
}

// WithSessionForgetter wires a per-session buffer holder so the coordinator can
// evict it at every terminal transition. Optional; unwired is a no-op.
func WithSessionForgetter(f SessionForgetter) CoordinatorOption {
	return func(c *Coordinator) { c.forgetters = append(c.forgetters, f) }
}

// WithHomeProvider wires the storage provider. Without it a managed-home launch
// fails loudly: dropping the mount silently would run the app on an ephemeral
// home and lose saves.
func WithHomeProvider(p HomeProvider) CoordinatorOption {
	return func(c *Coordinator) { c.homes = p }
}

// WithMicSettings wires the mic-capture instance gate. Unwired, mic is never
// granted — a quiet default, not a loud failure, since "off" is already the
// posture of a fully-configured instance.
func WithMicSettings(p MicCaptureProvider) CoordinatorOption {
	return func(c *Coordinator) { c.micSettings = p }
}

// injectHomeMount appends mount to the runtime spec's `mounts` array, preserving
// every other field semantically (re-marshalled, so key order may change, which
// the agent's parser is indifferent to). The non-managed path never enters here.
func injectHomeMount(runtimeSpec []byte, mount string) ([]byte, error) {
	spec := map[string]any{}
	if len(runtimeSpec) > 0 {
		if err := json.Unmarshal(runtimeSpec, &spec); err != nil {
			return nil, fmt.Errorf("parse runtime_spec: %w", err)
		}
	}
	var mounts []any
	if raw, ok := spec["mounts"]; ok {
		if arr, ok := raw.([]any); ok {
			mounts = arr
		} else {
			return nil, fmt.Errorf("runtime_spec.mounts is not an array")
		}
	}
	spec["mounts"] = append(mounts, mount)
	out, err := json.Marshal(spec)
	if err != nil {
		return nil, fmt.Errorf("marshal runtime_spec: %w", err)
	}
	return out, nil
}

// resolveHomeSpec returns the dispatchable runtime spec, injecting the (user,
// HOME app, host) home mount when the effective runtime app opts into
// managed_home. Shared by the launch and swap dispatch paths; forgetting the
// swap site silently drops the home on swap.
//
// It is the single seam for derived tiles. GetLaunchApp already resolved the
// effective runtime app, so the fields below are the PARENT's for a tile; this
// adds the two things resolution cannot do without the launching user and host:
// RequireHome instead of EnsureHome, keyed on homeAppID(app), and the tile's
// STEAM_STARTUP_FLAGS merged over `env`. Flags first, so injectHomeMount stays
// the last writer of `mounts`.
//
// A non-derived, non-managed app still takes the untouched-bytes path — the
// byte-identical guarantee in runtime_preset.go.
func (c *Coordinator) resolveHomeSpec(ctx context.Context, app LaunchApp, userID, hostID string) ([]byte, error) {
	spec := app.RuntimeSpec

	// The tile's one contribution to execution (§1.2), applied whether or not the
	// effective app is managed-home: the flags are what make it launch a GAME
	// rather than the client. Every dispatch site must therefore guard on
	// `app.ManagedHome || app.IsDerived()`, never ManagedHome alone — through
	// LaunchConsoleSession that gate dispatched the parent's raw spec and Steam
	// launched the client instead of the game, with no error anywhere.
	if app.IsDerived() {
		flags, err := composeSteamFlags(app.ExternalID)
		if err != nil {
			return nil, fmt.Errorf("derived tile %s: %w", app.ID, err)
		}
		spec, err = injectSteamFlags(spec, flags)
		if err != nil {
			return nil, err
		}
	}

	if !app.ManagedHome {
		return spec, nil
	}
	if c.homes == nil {
		return nil, fmt.Errorf("app %s has managed_home but no storage provider is configured", app.ID)
	}

	var (
		mount string
		err   error
	)
	if app.IsDerived() {
		// Read-only, never EnsureHome (see HomeProvider.RequireHome): a tile that
		// created its own home would mount an empty directory and reach `running`
		// looking healthy, and would un-tombstone a home an admin marked for
		// reaping. The launch path resolved the pin from this same user_homes row
		// before scheduling, so no row here is a genuine race and refusing is right.
		mount, err = c.homes.RequireHome(ctx, userID, homeAppID(app), hostID, app.HomeContainerPath)
		if errors.Is(err, storage.ErrHomeNotProvisioned) {
			return nil, ErrHomeNotProvisioned
		}
		if err != nil {
			return nil, fmt.Errorf("require home: %w", err)
		}
	} else {
		mount, err = c.homes.EnsureHome(ctx, userID, homeAppID(app), hostID, app.HomeContainerPath)
		if err != nil {
			return nil, fmt.Errorf("ensure home: %w", err)
		}
	}
	return injectHomeMount(spec, mount)
}
