// console_launch.go — CM-06 auto-start/stop: a pinned console-mode session on
// one host, owned by console_config.default_user, running
// console_config.default_app. Same schedule -> assign -> start primitives as
// LaunchByProfile, but with no profile/tier resolution and no probe envelope:
// the app's plain defaults, pinned to the host whose display connected.
package session

import (
	"context"
	"fmt"
	"time"
)

// LaunchConsoleSession launches one pinned console session on hostID, owned by
// userID, running appID at the app's defaults. Called by the agentws capacity
// handler's auto-start diff when a display connector goes absent->present.
// Returns the session id; the assign->start handshake runs asynchronously, so
// the caller does not wait for `running`.
func (c *Coordinator) LaunchConsoleSession(ctx context.Context, hostID, userID, appID, videoTopology string, width, height, fps int32) (string, error) {
	app, err := c.store.GetLaunchApp(ctx, appID)
	if err != nil {
		return "", fmt.Errorf("console auto-start: load app: %w", err)
	}

	encodeSlots, needsSignaling, err := consoleTransportPlan(videoTopology, app.DefaultEncodeSlots)
	if err != nil {
		return "", fmt.Errorf("console auto-start: %w", err)
	}
	var tokenHash string
	var tokenExpires time.Time
	if needsSignaling {
		tok, err := newSignalingToken(time.Now())
		if err != nil {
			return "", fmt.Errorf("console auto-start: signaling token: %w", err)
		}
		tokenHash, tokenExpires = tok.Hash, tok.ExpiresAt
	}
	if width <= 0 {
		width = app.DefaultWidth
	}
	if height <= 0 {
		height = app.DefaultHeight
	}
	if fps <= 0 {
		fps = app.DefaultFPS
	}

	p := CreateParams{
		UserID:          userID,
		AppID:           app.ID,
		Width:           width,
		Height:          height,
		FPS:             fps,
		BitrateKbps:     app.DefaultBitrateKbps,
		H264Profile:     "constrained-baseline", // T8; irrelevant to a local-only leg
		NeedEncodeSlots: encodeSlots,
		// The console drives a physical display and a launch failure feeds the
		// handler's crash-loop backoff, so after consoleBackoffMaxRetries a
		// transient VRAM veto would kill it semi-permanently with only an error
		// log. Encode slots still gate this launch; only the advisory veto is off.
		SkipVramVeto: true,
		TokenHash:    tokenHash,
		TokenExpires: tokenExpires,
		ManagedHome:  app.ManagedHome,
		// Inert for a non-catalog image. It cannot re-place the console (already
		// pinned), but it does refuse to auto-start an app whose managed image is
		// not on that host yet.
		AppImage: app.Image(),
		// The console's default_app can be a derived tile, so the single-writer
		// guard must key on the home-owning app here too. No §5 pin resolution: the
		// pin below is not negotiable, so a console tile whose home is on another
		// host fails at resolveHomeSpec with home_not_provisioned rather than being
		// placed elsewhere.
		HomeAppID: homeAppID(app),
		PinHostID: hostID, // the display lives here; not scheduler-picked
		// Mic unset: local_only console launches have no WebRTC pipeline
		// (agent-api.md), so capture never applies regardless of the setting.
	}

	sess, err := c.store.ScheduleAndCreate(ctx, p)
	if err != nil {
		return "", fmt.Errorf("console auto-start: schedule: %w", err)
	}
	c.log.Info("console auto-start: session scheduled",
		"session_id", sess.ID, "host_id", hostID, "user_id", userID, "app_id", appID)

	// Resolve the dispatchable spec: the per-(user, app) home mount for a
	// managed-home app, plus the tile's STEAM_STARTUP_FLAGS for a derived tile.
	//
	// `|| app.IsDerived()` is load-bearing here, not symmetry. The flag injection
	// lives inside resolveHomeSpec, so gating on ManagedHome alone dispatched the
	// parent's raw spec for a tile whose parent is not managed-home — Steam
	// launching the client instead of the game, silently. This path has no §5 pin
	// to refuse it first, so nothing else stands in front. It is also the only
	// caller where RequireHome is the sole thing between a console-configured tile
	// and a silently-provisioned empty Steam home.
	dispatchSpec := app.RuntimeSpec
	if app.ManagedHome || app.IsDerived() {
		if sess.HostID == nil {
			c.failSession(sess.ID, "app scheduled without a host")
			return "", fmt.Errorf("console auto-start: app scheduled without a host")
		}
		dispatchSpec, err = c.resolveHomeSpec(ctx, app, userID, *sess.HostID)
		if err != nil {
			c.failSession(sess.ID, fmt.Sprintf("home mount: %v", err))
			return "", fmt.Errorf("console auto-start: home mount: %w", err)
		}
	}

	go c.dispatchAssignStartWithTopology(sess, dispatchSpec, videoTopology)

	return sess.ID, nil
}

func consoleTransportPlan(videoTopology string, defaultEncodeSlots int32) (encodeSlots int32, needsSignaling bool, err error) {
	switch videoTopology {
	case "local_only":
		return 0, false, nil
	case "dual_output":
		return defaultEncodeSlots, true, nil
	default:
		return 0, false, fmt.Errorf("invalid console video topology %q", videoTopology)
	}
}

// StopConsoleSession is a thin wrapper over the normal Stop teardown, so console
// auto-stop gets the same agent-dispatch and reservation-release behaviour as
// DELETE /v1/sessions/{id}. Satisfies agentws.Events.
func (c *Coordinator) StopConsoleSession(ctx context.Context, sessionID, reason string) error {
	_, err := c.Stop(ctx, sessionID, reason)
	return err
}

// ConsoleSessionActive reports whether a recorded auto-started console session is
// still non-terminal, so the auto-start tracker can clear a session that died on
// its own and let the level-triggered always-on relaunch. Unknown or deleted is
// not active. Satisfies agentws.Events.
func (c *Coordinator) ConsoleSessionActive(ctx context.Context, sessionID string) bool {
	hs, err := c.store.GetSessionHostState(ctx, sessionID)
	if err != nil {
		return false
	}
	return hs.State == StatePending || hs.State.HoldsReservation()
}
