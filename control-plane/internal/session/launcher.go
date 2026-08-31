// launcher.go — the assign→start launch handshake and stuck-start watchdog.
package session

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/accreleus/quasar/control-plane/internal/agentws"
	"github.com/accreleus/quasar/control-plane/internal/profile"
	"github.com/accreleus/quasar/control-plane/internal/tier"
)

// LaunchByProfile resolves the selected launch profile (or the legacy tier) to
// concrete stream parameters, gates eligibility, persists the launch profile id,
// schedules+creates, then resolves post-placement which rung the session gets.
//
// Errors: ErrNotFound (404), ErrProfileUnknown (400), ErrProfileIneligible
// (409), ErrRungCodecNotAvailable (400), ErrCodecUnsupportedByHost (409), plus
// the scheduler's admission errors.
func (c *Coordinator) LaunchByProfile(ctx context.Context, userID string, lp LaunchParams) (LaunchResult, error) {
	app, err := c.store.GetLaunchApp(ctx, lp.AppID)
	if err != nil {
		return LaunchResult{}, err
	}

	// Entitlement pre-check (§6.3). NOT the authorization boundary — that is the
	// same predicate with FOR SHARE inside scheduleAttempt — and must never
	// replace it. It exists for error ORDERING: two gates below return 409 before
	// ScheduleAndCreate, so the 409/403/400 split would otherwise be an oracle for
	// an app's launch-profile allow-list. After GetLaunchApp, so an unknown or
	// disabled app is still 404. No role arm, here or in the transaction (§6.5).
	entitled, err := c.store.IsEntitled(ctx, userID, app.ID)
	if err != nil {
		return LaunchResult{}, err
	}
	if !entitled {
		return LaunchResult{}, ErrNotEntitled
	}

	// The app's launchable-launch-profile allow-list; empty is unrestricted.
	// Resolved once for both branches: the explicit path enforces it, the implicit
	// path filters through it.
	restriction, err := c.store.AppProfileRestrictionFor(ctx, app)
	if err != nil {
		return LaunchResult{}, err
	}

	// The override-policy hatch: an explicit `stream` block means the operator is
	// forcing concrete values, so user_overrides_allowed / `force` does not apply.
	//
	// Carve-out: a `force` app pins one launch profile, so naming that same
	// profile_id overrides nothing — resolveLaunchProfile lands on the chain the
	// implicit path would. A different profile_id is a real override and 409s.
	if lp.ProfileID != "" && !lp.Override.any() {
		isForcedNoOp := app.ProfilePolicy == "force" && app.DefaultProfileID != nil && lp.ProfileID == *app.DefaultProfileID
		if !isForcedNoOp {
			allowed, err := c.store.ProfileOverrideAllowed(ctx, app, lp.IsAdmin)
			if err != nil {
				return LaunchResult{}, err
			}
			if !allowed {
				return LaunchResult{}, ErrProfileOverrideDisabled
			}
		}
	}
	// Allow-list enforcement; the client-side menu filter is only a convenience.
	// It must stay OUTSIDE the `!Override.any()` guard: `stream` on POST
	// /v1/sessions has no role gate, so nesting it there let any caller launch a
	// removed chain by adding one field, and an override also short-circuits
	// resolveLaunchProfile so the disallowed chain got persisted and dispatched.
	// The override hatch beats eligibility; it never beats operator configuration
	// of which chains an app offers. Runs after the override-policy block, fixing
	// the precedence of the two 409s. Admin's bypass is a server-verified
	// users.role read, never a client assertion.
	if lp.ProfileID != "" && !lp.IsAdmin && !restriction.Permits(lp.ProfileID) {
		c.log.Info("UI-P5: launch profile refused by the app's allow-list",
			"user_id", userID, "app_id", app.ID, "profile_id", lp.ProfileID,
			"had_stream_override", lp.Override.any())
		return LaunchResult{}, ErrProfileNotLaunchableForApp
	}
	if lp.ProfileID == "" && !lp.Override.any() {
		// No in-code-catalog fallback: a control plane that cannot read
		// launch_profiles cannot serve the request, and launching at settings
		// guessed from a compiled-in table is worse than a 500.
		catalog, err := c.store.ListLaunchProfiles(ctx, true)
		if err != nil {
			return LaunchResult{}, fmt.Errorf("load launch profiles: %w", err)
		}
		// Narrow BEFORE evaluating, or the implicit path resolves profiles the
		// explicit path is rejected for and the allow-list binds only clients that
		// bother to name a profile.
		ev := profile.EvaluateLaunchProfiles(restriction.Filter(catalog), c.probeEvalInput(ctx, userID))
		resolved, err := c.store.ResolveDefaultProfile(ctx, userID, app, ev.RecommendedID, restriction)
		if err != nil {
			return LaunchResult{}, err
		}
		lp.ProfileID = resolved
	}

	tok, err := newSignalingToken(time.Now())
	if err != nil {
		return LaunchResult{}, err
	}

	ov := lp.Override
	var (
		width, height, fps, bitrateKbps, playout0Ms int32
		h264                                        string
		profileID, source                           string
		// The resolved chain, empty on the legacy/tier path. Its rungs are walked
		// post-placement, once the host is known.
		launchProfile profile.LaunchProfile
	)

	if lp.ProfileID != "" {
		// Launch-profile path: resolve + gate, then base on the TOP rung, beaten
		// field-by-field by an explicit override. The top rung is the
		// highest-demand one, so admission below is evaluated against the worst
		// case; falling through post-placement only lowers demand.
		resolved, err := c.resolveLaunchProfile(ctx, userID, lp)
		if err != nil {
			return LaunchResult{}, err
		}
		launchProfile = resolved
		top, ok := resolved.TopRung()
		if !ok {
			return LaunchResult{}, ErrLaunchProfileEmpty
		}
		profileID, source = resolved.ID, "profile:"+resolved.ID
		width = pick(ov.Width, top.Width)
		height = pick(ov.Height, top.Height)
		fps = pick(ov.FPS, top.FPS)
		bitrateKbps = pick(ov.BitrateKbps, top.NominalBitrateKbps)
		// H.264 floor: Chrome's WebRTC receiver cannot decode `high` (T8, verified
		// on VA and NVENC), so the rung's preference is negotiated down for the
		// browser transport. An explicit override wins.
		//
		// The lift back needs BOTH the launching client declaring itself native AND
		// this account's latest native probe decoding it. Keying on the launching
		// client is the identity binding: LatestProbe is per-account and returns
		// whichever device was seen last, so a native session otherwise poisons a
		// later browser launch into a black stream. It sets no StreamOverride
		// field, so the envelope and cert-cap blocks below still run.
		h264 = pickProfile(ov.H264Profile)
		if !ov.any() && lp.isNativeClient() {
			dp, err := c.store.LatestProbe(ctx, userID)
			if err != nil {
				// A probe read failure must never block a launch; keep the floor.
				c.log.Warn("SPT Path-B: probe load failed, keeping H.264 floor", "user_id", userID, "err", err)
			} else if nativeHighEligible(dp, top.H264Profile) {
				h264 = top.H264Profile
				c.log.Info("SPT Path-B: native high-eligible, lifting H.264 profile",
					"user_id", userID, "profile_id", resolved.ID,
					"target_h264", top.H264Profile)
			}
		}
		playout0Ms = top.Playout0Ms
	} else {
		// Legacy path. Precedence: explicit override > tier > app default, with app
		// defaults as a CEILING (a 720p app never launches 1080p).
		//
		// apps.default_width/height/fps/bitrate_kbps are load-bearing here, not
		// dead columns: any app reaches this path via a stream override with no
		// profile_id, and it is in the frozen contract.
		selected := c.selectTier(ctx, userID)
		width = capAndPick(ov.Width, selected.Width, app.DefaultWidth)
		height = capAndPick(ov.Height, selected.Height, app.DefaultHeight)
		fps = capAndPick(ov.FPS, selected.FPS, app.DefaultFPS)
		bitrateKbps = capAndPick(ov.BitrateKbps, selected.BitrateKbps, app.DefaultBitrateKbps)
		h264 = pickProfile(ov.H264Profile) // P1-11: per-launch override, else the floor
		playout0Ms = selected.Playout0Ms
		source = "tier:" + selected.Name
	}

	// SPT-07 probe envelope, default path only (no admin, no explicit override).
	// It only ever lowers: a safe_ceiling and an RTT-class playout0 bump derived
	// from the device probe. Computed once and applied BEFORE insert, so admission
	// never sees an inflated number, then re-applied post-placement to the
	// resolved rung's bitrate.
	env := ProbeEnvelope{}
	if !lp.IsAdmin && !ov.any() {
		dp, err := c.store.LatestProbe(ctx, userID)
		if err != nil {
			// A probe read failure must never block a launch; proceed with defaults.
			c.log.Warn("SPT-07: probe load failed, launching without envelope", "user_id", userID, "err", err)
		} else {
			env = buildProbeEnvelope(dp)
			if env.SafeCeilingKbps > 0 {
				newBitrate := applyEnvelopeToBitrate(bitrateKbps, env)
				if newBitrate < bitrateKbps {
					c.log.Info("SPT-07: probe envelope safe_ceiling applied",
						"user_id", userID,
						"probe_bw_kbps", dp.BandwidthKbps,
						"safe_ceiling_kbps", env.SafeCeilingKbps,
						"resolved_kbps", bitrateKbps,
						"clamped_kbps", newBitrate)
					bitrateKbps = newBitrate
				}
			}
			if env.Playout0BumpMs > 0 {
				c.log.Info("SPT-07: probe envelope playout0 bump applied",
					"user_id", userID,
					"probe_rtt_ms", dp.RTTMs,
					"bump_ms", env.Playout0BumpMs,
					"original_playout0_ms", playout0Ms,
					"adjusted_playout0_ms", playout0Ms+env.Playout0BumpMs)
				playout0Ms = applyEnvelopeToPlayout0(playout0Ms, env)
			}
		}
	}

	// The GRANTED mic state is request AND instance gate, resolved once and
	// persisted; never the raw request. A request against a disabled gate is not
	// an error, it resolves to false.
	micGranted := c.resolveMicGrant(ctx, lp)

	p := CreateParams{
		UserID:      userID,
		AppID:       app.ID,
		Width:       width,
		Height:      height,
		FPS:         fps,
		BitrateKbps: bitrateKbps,
		H264Profile: h264,
		Mic:         micGranted,
		ProfileID:   profileID,
		Playout0Ms:  playout0Ms,
		// Encode slots are the only reserved dimension (#383); app.DefaultVramMB is
		// deprecated. For a derived tile this must be the PARENT's number (resolved
		// by GetLaunchApp): a tile's own resource columns are whatever the
		// background reconciler's INSERT left them at, and reading them here admits
		// a session onto a GPU with no free encode slot — the cb97bfb shape.
		NeedEncodeSlots: app.DefaultEncodeSlots,
		TokenHash:       tok.Hash,
		TokenExpires:    tok.ExpiresAt,
		// The PARENT's managed_home for a tile (a tile's own is false by CHECK), so
		// the single-writer guard fires. See CreateParams.ManagedHome.
		ManagedHome: app.ManagedHome,
		HomeAppID:   homeAppID(app),
		// The effective runtime app's image; engages the placement filter only when
		// it is an installed catalog entry.
		AppImage: app.Image(),
	}

	// §5: a derived tile is placed with a HARD host pin, not an affinity. Locality
	// is only a sort preference, and a tile provisions nothing (RequireHome
	// refuses), so a host with no home for (user, parent) has nothing to mount.
	// The pin works identically under both policies.
	//
	// Stricter than locality: a tile cannot fall back to a free host and 409s
	// where a normal app would run. A session that refuses to start is one click
	// from recovery; one that silently creates a second empty Steam library is a
	// support ticket.
	if app.IsDerived() {
		homeHost, err := c.store.HomeHostForApp(ctx, userID, homeAppID(app))
		if err != nil {
			return LaunchResult{}, err
		}
		if homeHost == "" {
			// Refused before placement: nothing is reserved, and no user_homes row
			// is created on the way past.
			return LaunchResult{}, ErrHomeNotProvisioned
		}
		p.PinHostID = homeHost
	}

	sess, err := c.store.ScheduleAndCreate(ctx, p)
	if err != nil {
		c.logVramVetoRejection(userID, app.ID, err)
		return LaunchResult{}, err
	}
	c.log.Info("session assigned", "session_id", sess.ID, "host_id", deref(sess.HostID), "gpu_index", derefI32(sess.GPUIndex),
		"reserved_encode_slots", sess.ReservedSlots,
		"stream_source", source, "playout0_ms", playout0Ms)
	c.health.logGPUUtilization(ctx, deref(sess.HostID), deref(sess.GPUID))

	// Post-placement: rung resolution, cert cap, re-resolve, one write. Placement
	// is codec-blind (§3.1), so the rung resolves here, where the host is known.
	if err := c.applyPostPlacement(ctx, &sess, launchProfile, lp, ov, env, source); err != nil {
		// A stream.codec override named a codec no rung uses, or one the placed
		// host cannot encode. Fail the session, releasing its reservation, rather
		// than dispatching a doomed assignment.
		c.failSession(sess.ID, fmt.Sprintf("rung resolution failed: %v", err))
		return LaunchResult{}, err
	}

	// Resolve the dispatchable spec; for a managed-home app this injects the
	// per-(user, app) home mount. Failure fails the session, because dispatching a
	// managed-home app without its home loses saves silently.
	dispatchSpec := app.RuntimeSpec
	// resolveHomeSpec must be entered when EITHER condition holds, on this caller
	// and the console one alike: a derived tile whose parent is not managed-home
	// would otherwise dispatch without its STEAM_STARTUP_FLAGS. Unreachable today
	// only because §5's pin refuses such a tile first.
	if app.ManagedHome || app.IsDerived() {
		if sess.HostID == nil {
			c.failSession(sess.ID, "app scheduled without a host")
			return LaunchResult{}, fmt.Errorf("app scheduled without a host")
		}
		dispatchSpec, err = c.resolveHomeSpec(ctx, app, userID, *sess.HostID)
		if err != nil {
			c.failSession(sess.ID, fmt.Sprintf("home mount: %v", err))
			return LaunchResult{}, fmt.Errorf("home mount: %w", err)
		}
	}

	// Async, so the HTTP response returns immediately with the assigned session.
	go c.dispatchAssignStart(sess, dispatchSpec)

	return LaunchResult{Session: sess, SignalingToken: tok.Plaintext, TokenExpiresAt: tok.ExpiresAt}, nil
}

// applyPostPlacement resolves which rung a placed session starts at and writes
// it back in ONE update, mutating sess in place so the dispatched assignment and
// the persisted row agree.
//
// Gather, plan, apply: reads first (gatherStreamInputs), a pure decision
// (planStream, stream_plan.go), then log, write once, apply. Hoisting the reads
// is load-bearing — when the cert cap fires the decision walks a second chain,
// and re-reading would let one launch's two walks see different LatestProbe rows.
//
// On the legacy/tier path there are no rungs; only a codec override can change
// anything, handled with the host clamp alone.
func (c *Coordinator) applyPostPlacement(
	ctx context.Context,
	sess *Session,
	launchProfile profile.LaunchProfile,
	lp LaunchParams,
	ov StreamOverride,
	env ProbeEnvelope,
	source string,
) error {
	if launchProfile.ID == "" {
		return c.applyLegacyCodecOverride(ctx, sess, ov.codecOverride(), source)
	}

	in := c.gatherStreamInputs(ctx, sess, launchProfile, lp, ov, env, source)
	plan, err := planStream(in)
	c.logStreamPlan(in, plan)
	if err != nil {
		return err
	}

	if err := c.store.UpdateSessionStream(ctx, sess.ID, plan.Update); err != nil {
		// Non-fatal: the session keeps the pre-schedule top-rung values it was
		// admitted against and the h264 floor it was inserted with. sess must NOT
		// be mutated here, or the dispatched spec and the persisted row diverge.
		c.log.Warn("post-placement stream update failed, dispatching the pre-schedule values",
			"session_id", sess.ID, "launch_profile", plan.ChainID, "rung", plan.RungID, "err", err)
		return nil
	}
	plan.applyTo(sess)
	return nil
}

// gatherStreamInputs performs every read the post-placement decision needs and
// nothing else. It never fails: each read degrades to the zero value meaning "do
// not clamp on this", because an already-admitted launch must not be refused
// over an unreadable diagnostic input.
//
// The lower chain and its failure history load unconditionally whenever a cap
// could run, which is what lets planStream stay pure: lowerProfileRung is a
// compiled-in ladder, so the hop target costs two point-reads here instead of
// I/O mid-decision.
func (c *Coordinator) gatherStreamInputs(
	ctx context.Context,
	sess *Session,
	chain profile.LaunchProfile,
	lp LaunchParams,
	ov StreamOverride,
	env ProbeEnvelope,
	source string,
) StreamInputs {
	in := StreamInputs{
		SessionID:   sess.ID,
		HostID:      sess.HostID,
		GPUIndex:    sess.GPUIndex,
		H264Profile: sess.H264Profile,
		Codec:       sess.Codec,
		Params:      lp,
		Override:    ov,
		Envelope:    env,
		Source:      source,
		Chain:       chain,
		CertMaxAge:  CertStaleness,
		Now:         time.Now(),
	}

	// Placed-host encoder capability. A missing host is an h264-only host; an
	// unreported encoder capability skips clamp 5 (unknown → allow).
	if sess.HostID != nil {
		hc, err := c.store.HostCodecs(ctx, *sess.HostID)
		if err != nil {
			c.log.Warn("rung: host codec set load failed, assuming h264-only",
				"host_id", *sess.HostID, "err", err)
			in.HostCodecs = []string{wireCodecH264}
		} else {
			in.HostCodecs = hc
		}
		known, hw, err := c.store.HostHardwareEncoder(ctx, *sess.HostID)
		if err != nil {
			c.log.Warn("rung: host encoder capability load failed, skipping the hardware-encoder clamp",
				"host_id", *sess.HostID, "err", err)
		} else {
			in.HostEncoder = hostEncoderCaps{Known: known, HardwareEncoder: hw}
		}
		// The per-codec throughput hint for clamp 6, written onto the same
		// HostEncoder struct clamp 5 reads. A read error leaves it nil, which is
		// "unknown" and clamps nothing — unlike the codec set there is no floor to
		// degrade to, and none is wanted (Store.HostCodecPixelRates).
		if rates, err := c.store.HostCodecPixelRates(ctx, *sess.HostID); err != nil {
			c.log.Warn("rung: host codec throughput load failed, skipping the encoder-throughput clamp",
				"host_id", *sess.HostID, "err", err)
		} else {
			in.HostEncoder.PixelRates = rates
		}
	}

	// Device decode probe. A read error or absent/stale probe is non-fatal: HEVC
	// and AV1 stay hard-gated off and the decode-height clamp is skipped.
	dp, err := c.store.LatestProbe(ctx, sess.UserID)
	if err != nil {
		c.log.Warn("rung: probe load failed, gating without decode capabilities",
			"user_id", sess.UserID, "err", err)
		dp = nil
	}
	in.Probe = dp

	// Clamp 4 at rung grain (§4.4). RungFailures unions rung-level rows with the
	// legacy launch-profile-level ones, keyed on the same device the probe above
	// came from (LatestProbe and LatestDeviceKey share the last_seen_at ordering).
	// A read error skips the clamp.
	deviceKey, _ := c.store.LatestDeviceKey(ctx, sess.UserID)
	if fr, err := c.store.RungFailures(ctx, sess.UserID, deviceKey, chain); err != nil {
		c.log.Warn("rung: decode-failure history load failed, resolving without it",
			"user_id", sess.UserID, "profile_id", chain.ID, "err", err)
	} else {
		in.FailedRungs = fr
	}

	if !in.capEligible() {
		return in
	}

	// The cap's hop target, and the certs both chains could be judged against.
	rungIDs := rungIDsOf(chain)
	in.LowerChainID = lowerProfileRung(chain.ID)
	if in.LowerChainID != "" {
		lower, err := c.store.GetLaunchProfile(ctx, in.LowerChainID)
		if err != nil {
			// Recorded, not warned: this read happens on every cap-eligible launch
			// but only matters when a cap hops, and logStreamPlan warns on those.
			in.LowerChainErr = err
		} else {
			in.LowerChain = lower
			rungIDs = append(rungIDs, rungIDsOf(lower)...)
			if fr, err := c.store.RungFailures(ctx, sess.UserID, deviceKey, lower); err != nil {
				c.log.Warn("rung: decode-failure history load failed for the cap target, resolving without it",
					"user_id", sess.UserID, "profile_id", lower.ID, "err", err)
			} else {
				in.LowerFailed = fr
			}
		}
	}

	certs, err := c.store.CertsForRungs(ctx, *sess.HostID, int(*sess.GPUIndex), rungIDs, CertStaleness)
	if err != nil {
		c.log.Warn("SPT-06: cert lookup error, proceeding uncapped",
			"host_id", *sess.HostID, "profile_id", chain.ID, "err", err)
	} else {
		in.Certs = certs
	}
	return in
}

// rungIDsOf lists a chain's rung ids, for the batch cert read.
func rungIDsOf(chain profile.LaunchProfile) []string {
	out := make([]string, 0, len(chain.Rungs))
	for _, r := range chain.Rungs {
		out = append(out, r.ID)
	}
	return out
}

// logStreamPlan emits the launch-path diagnostics: one "rung resolved" line per
// walk, the cert-cap line, the envelope clamp. Reconstructed from the plan
// rather than passed a logger, so planStream stays pure and tests assert on
// values rather than log output.
func (c *Coordinator) logStreamPlan(in StreamInputs, plan StreamPlan) {
	for _, w := range plan.Walks {
		// failed_rungs comes from the WALK, not from in: the cap target resolves
		// against its own failure set, and pairing one walk's verdicts with the
		// other's history makes the line contradict itself.
		c.log.Info("rung resolved",
			"session_source", in.Source,
			"launch_profile", w.ChainID,
			"override", w.Decision.Override,
			"considered", formatRungVerdicts(w.Decision.Considered),
			"host_codecs", in.HostCodecs,
			"host_hw_encoder_known", in.HostEncoder.Known,
			"host_hw_encoder", in.HostEncoder.HardwareEncoder,
			// Raw map, so a clamp-6 rejection can be read against the number that
			// caused it without a second query.
			"host_codec_pixel_rates", in.HostEncoder.PixelRates,
			"device_hevc", in.Probe != nil && in.Probe.HEVC,
			"device_av1", in.Probe != nil && in.Probe.AV1,
			"device_max_decode_height", probeDecodeHeight(in.Probe),
			"failed_rungs", keys(w.Failed),
			"floor_fired", w.Decision.Floor,
			"result_rung", w.Decision.ResultRung,
			"result_codec", w.Decision.Result,
			"err", w.Err)
	}

	switch plan.CapOutcome {
	case capApplied:
		c.log.Info("SPT-06: cert cap applied",
			"session_id", in.SessionID, "original_profile", in.Chain.ID,
			"original_rung", plan.Walks[0].Decision.ResultRung,
			"capped_profile", plan.ChainID, "capped_rung", plan.RungID)
	case capLowerUnreadable:
		c.log.Warn("SPT-06: cert cap target launch profile unreadable, proceeding uncapped",
			"session_id", in.SessionID, "profile_id", in.Chain.ID,
			"capped_profile_id", plan.CapTarget, "err", in.LowerChainErr)
	case capLowerEmpty:
		c.log.Warn("SPT-06: cert cap target launch profile has no rungs, proceeding uncapped",
			"session_id", in.SessionID, "capped_profile_id", plan.CapTarget)
	case capUnresolvable:
		c.log.Warn("SPT-06: cert cap target chain did not resolve, proceeding uncapped",
			"session_id", in.SessionID, "capped_profile_id", plan.CapTarget,
			"err", plan.Walks[len(plan.Walks)-1].Err)
	}

	if plan.RungBitrateKbps > 0 && plan.Update.BitrateKbps < plan.RungBitrateKbps {
		c.log.Info("SPT-07: probe envelope re-applied to the resolved rung",
			"session_id", in.SessionID, "rung", plan.RungID,
			"rung_kbps", plan.RungBitrateKbps, "clamped_kbps", plan.Update.BitrateKbps)
	}
}

// applyLegacyCodecOverride handles the profile-less path, where there are no
// rungs. Without an override the codec stays at the inserted h264 floor; with
// one, the host-encoder clamp still applies — that is never overridable.
func (c *Coordinator) applyLegacyCodecOverride(ctx context.Context, sess *Session, override, source string) error {
	if override == "" || override == sess.Codec {
		return nil
	}
	hostCodecs := []string{wireCodecH264}
	if sess.HostID != nil {
		if hc, err := c.store.HostCodecs(ctx, *sess.HostID); err != nil {
			c.log.Warn("codec: host codec set load failed, assuming h264-only",
				"host_id", *sess.HostID, "err", err)
		} else {
			hostCodecs = hc
		}
	}
	if !codecSet(hostCodecs)[override] {
		return ErrCodecUnsupportedByHost
	}
	c.log.Info("codec resolved", "session_source", source, "override", override, "result", override)
	if err := c.store.UpdateSessionCodec(ctx, sess.ID, override); err != nil {
		// Keep the persisted h264 floor, leaving sess.Codec unchanged, so
		// sessions.codec and the dispatched stream spec stay consistent.
		c.log.Warn("codec persist failed, keeping h264 floor",
			"session_id", sess.ID, "resolved_codec", override, "err", err)
		return nil
	}
	sess.Codec = override
	return nil
}

// logVramVetoRejection makes a veto visible (#383 §4.4): one line per refused
// GPU with every number behind the decision, since a veto is otherwise
// indistinguishable from slot exhaustion (same retryable 503).
//
// Spec deviation: §4.4 also wants a session_trace_event, but
// session_trace_events.session_id is NOT NULL REFERENCES sessions(id) and an
// admission rejection persists no session row. The log carries the same payload.
// A nil or non-veto error is a no-op.
func (c *Coordinator) logVramVetoRejection(userID, appID string, err error) {
	var veto *VramVetoRejection
	if !errors.As(err, &veto) {
		return
	}
	for _, g := range veto.Candidates {
		attrs := []any{
			"user_id", userID,
			"app_id", appID,
			"gpu_id", g.GPUID,
			"host_id", g.HostID,
			"gpu_index", g.GPUIndex,
			"vram_mb_total", g.VramMBTotal,
			"min_free_mb", veto.Veto.MinFreeMB,
			"inflight_estimate_mb", veto.Veto.InflightMB,
			"inflight_sessions", g.InflightN,
			"debit_mb", g.DebitMB,
			"staleness_secs", veto.Veto.StalenessSecs,
		}
		if g.VramMBFree != nil {
			attrs = append(attrs, "vram_mb_free", *g.VramMBFree)
		} else {
			attrs = append(attrs, "vram_mb_free", "unknown")
		}
		if g.SampledAt != nil {
			attrs = append(attrs, "vram_sampled_at", g.SampledAt.UTC().Format(time.RFC3339Nano))
		} else {
			attrs = append(attrs, "vram_sampled_at", "never")
		}
		if g.SampleAgeMs != nil {
			attrs = append(attrs, "sample_age_ms", *g.SampleAgeMs)
		}
		c.log.Warn("admission: live free-VRAM veto refused a GPU with free encode slots", attrs...)
	}
}

// dispatchAssignStart performs the two-step agent handshake: assign, then start.
// Either step failing (reject, timeout, agent not connected) fails the session
// and releases its reservation. starting→running then arrives via AgentState.
func (c *Coordinator) dispatchAssignStart(sess Session, runtimeSpec []byte) {
	c.dispatchAssignStartWithTopology(sess, runtimeSpec, "stream_only")
}

func (c *Coordinator) dispatchAssignStartWithTopology(sess Session, runtimeSpec []byte, videoTopology string) {
	if sess.HostID == nil || sess.GPUIndex == nil {
		c.failSession(sess.ID, "session missing host/gpu placement")
		return
	}
	hostID := *sess.HostID
	app := runtimeSpec
	if len(app) == 0 {
		app = []byte("{}")
	}

	// The resolved RUNG's ABR floor, for the agent's in-session governor. It must
	// read the rung, not the launch profile: a chain has no single floor, and one
	// that fell through to a lower rung has a lower floor. No rung resolved ⇒ 0,
	// and the agent keeps its env/ratio fallback.
	var abrFloorKbps int32
	if sess.StreamProfileID != nil {
		if floor, err := c.store.RungABRFloor(c.ctx, *sess.StreamProfileID); err == nil {
			abrFloorKbps = floor
		}
	}

	// Omit the default h264 so the wire stays byte-identical to the
	// pre-multi-codec shape for H.264 sessions; absent ⇒ h264 (agent-api.md).
	streamCodec := sess.Codec
	if streamCodec == wireCodecH264 {
		streamCodec = ""
	}
	assign := agentws.SessionAssignCmd{
		Type:      "session_assign",
		ID:        newCmdID(),
		SessionID: sess.ID,
		GPUIndex:  *sess.GPUIndex,
		App:       app,
		Stream: agentws.StreamSpec{
			Width: sess.Width, Height: sess.Height, FPS: sess.FPS,
			BitrateKbps: sess.BitrateKbps, H264Profile: sess.H264Profile,
			AbrFloorKbps: abrFloorKbps,
			Codec:        streamCodec,
			// Already the GRANTED state (resolved in LaunchByProfile); `omitempty`
			// keeps the wire byte-identical for every non-mic session.
			Mic: sess.Mic,
		},
		Resources:     agentws.ResourceSpec{VRAMMB: sess.ReservedVram, EncodeSlots: sess.ReservedSlots},
		VideoTopology: videoTopology,
	}
	if !c.commandOK(hostID, assign.ID, assign, assignAckTimeout, sess.ID, "assign") {
		return
	}

	start := agentws.SessionStartCmd{Type: "session_start", ID: newCmdID(), SessionID: sess.ID}
	if !c.commandOK(hostID, start.ID, start, startAckTimeout, sess.ID, "start") {
		return
	}
	c.log.Info("session start dispatched", "session_id", sess.ID)
	// `running` arrives via the agent's session_state callback. Arm the
	// stuck-start watchdog: the agent acked start, so it must reach running
	// within the window or be reaped.
	go c.watchStartToRunning(sess.ID, hostID)
}

// watchStartToRunning is the stuck-start watchdog (P2-06). A session still
// pre-running when startToRunningTimeout elapses means the agent accepted the
// work and wedged. Failing it releases its reservation in the same transaction
// (schema.md invariant #2), and the stop tells the agent to tear down what it
// did bring up so no container is orphaned. A session that reached running or a
// terminal state is left untouched.
//
// The wait selects on c.ctx so shutdown cancels it without reaping. The
// terminal-state re-check after the timer is the guard for the happy path.
func (c *Coordinator) watchStartToRunning(sessionID, hostID string) {
	timer := time.NewTimer(c.startToRunningTimeout)
	defer timer.Stop()
	select {
	case <-c.ctx.Done():
		return // coordinator shutting down; the watchdog's reap is moot
	case <-timer.C:
	}

	sess, err := c.store.Get(c.ctx, sessionID)
	if err != nil {
		return // gone already; nothing to reap
	}
	if sess.State != StateAssigned && sess.State != StateStarting {
		return // reached running, or already terminal/stopping — healthy
	}

	c.failSession(sessionID, fmt.Sprintf(
		"start timed out: agent acked start but never reported running within %s", c.startToRunningTimeout))

	// Best-effort teardown so the agent does not leak a half-built session; the
	// host-disconnect reaper is the backstop if the agent is already gone.
	cmd := agentws.SessionStopCmd{Type: "session_stop", ID: newCmdID(), SessionID: sessionID, Reason: "start_timed_out"}
	ctx, cancel := context.WithTimeout(c.ctx, stopAckTimeout)
	defer cancel()
	if _, err := c.dispatcher.SendWithAck(ctx, hostID, cmd.ID, cmd); err != nil {
		c.log.Warn("stuck-start stop dispatch failed", "session_id", sessionID, "err", err)
	}
}

// selectTier intersects the user's most-recent device probe against the tier
// ladder, falling back to Default() on any error or missing probe. A probe read
// failure must never be fatal to a launch, so errors are logged, not propagated.
func (c *Coordinator) selectTier(ctx context.Context, userID string) tier.Tier {
	dp, err := c.store.LatestProbe(ctx, userID)
	if err != nil {
		c.log.Warn("AS-02: probe load failed, using default tier", "user_id", userID, "err", err)
		return tier.Default()
	}
	if dp == nil {
		return tier.Default()
	}
	p := tier.Probe{
		BandwidthKbps:   dp.BandwidthKbps,
		RTTMs:           dp.RTTMs,
		MaxDecodeHeight: dp.MaxDecodeHeight,
	}
	selected := tier.Select(p)
	c.log.Info("AS-02: tier selected", "user_id", userID,
		"tier", selected.Name,
		"probe_bw", dp.BandwidthKbps,
		"probe_rtt", dp.RTTMs)
	return selected
}

// resolveMicGrant is the request AND the instance-wide gate, read fresh at
// launch, never cached. An ungranted request is never an error and never blocks
// a launch; every failure path is fail-CLOSED:
//   - request false ⇒ short-circuit, no settings read at all.
//   - c.micSettings nil (WithMicSettings never wired) ⇒ false.
//   - settings read errors ⇒ false. A transient DB blip must not fail an
//     otherwise-valid launch over a policy nicety, nor fail open.
func (c *Coordinator) resolveMicGrant(ctx context.Context, lp LaunchParams) bool {
	if !lp.Mic {
		return false
	}
	if c.micSettings == nil {
		return false
	}
	enabled, err := c.micSettings.MicCaptureEnabled(ctx)
	if err != nil {
		c.log.Warn("microphone capture: instance-setting read failed, not granting", "err", err)
		return false
	}
	return enabled
}
