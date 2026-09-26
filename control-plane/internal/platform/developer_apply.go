package platform

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strings"

	"github.com/accreleus/quasar/control-plane/internal/audit"
	"github.com/accreleus/quasar/control-plane/internal/buildinfo"
	"github.com/accreleus/quasar/control-plane/internal/httpx"
	"github.com/accreleus/quasar/control-plane/internal/images"
	"github.com/accreleus/quasar/control-plane/internal/updater"
)

// Developer apply: an admin's arbitrary digest set on one owned target, as a
// `developer_apply` attempt driven by the per-host machine (apply_runner.go).
// semantics: control-api.md §"Developer apply" (amendment 14).

// Refusal codes new to this route.
const (
	CodeTargetNotOwned    = "target_not_owned"   // 409
	CodeImageUnresolvable = "image_unresolvable" // 409
	CodeNamespaceRejected = "namespace_rejected" // 409, the failure identifier reused
	ComponentRecovery     = "recovery-actor"
)

// DeveloperApplyRequest is `PlatformDeveloperApplyRequest`.
type DeveloperApplyRequest struct {
	Target                  string            `json:"target"`
	HostID                  *string           `json:"host_id,omitempty"`
	Components              []ComponentDigest `json:"components"`
	Force                   bool              `json:"force"`
	ExternalBackupConfirmed bool              `json:"external_backup_confirmed"`
}

// DeveloperImages reads the build identity the requested images carry.
type DeveloperImages interface {
	// Commit is the one `org.quasar.source.commit` every image carries; an error
	// is `image_unresolvable`.
	Commit(ctx context.Context, components []ComponentDigest) (string, error)
}

var (
	devDigestRe = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	// The distribution reference grammar's path component: no empty, `.` or `..`
	// segment, so a prefix match on the allowlist cannot be walked out of.
	pathComponentRe = regexp.MustCompile(`^[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*$`)
	registryHostRe  = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9.-]*[A-Za-z0-9])?(?::[0-9]+)?$`)
)

// RepositoryWellFormed: a registry host, then one or more path components.
func RepositoryWellFormed(image string) bool {
	parts := strings.Split(image, "/")
	if len(parts) < 2 || !registryHostRe.MatchString(parts[0]) {
		return false
	}
	for _, p := range parts[1:] {
		if !pathComponentRe.MatchString(p) {
			return false
		}
	}
	return true
}

// componentRank orders an apply's components: the recovery actor first
// (ADR 0008), whatever order the request used.
func componentRank(name string) int {
	switch name {
	case ComponentRecovery:
		return 0
	case ComponentControlPlane:
		return 1
	default:
		return 2
	}
}

// ValidateDeveloperApply is the `400 validation_failed` decision: the body's
// shape, before anything is read. It returns the ordered components.
func ValidateDeveloperApply(req DeveloperApplyRequest) ([]ComponentDigest, error) {
	allowed := map[string]bool{}
	switch req.Target {
	case TargetHost:
		if req.HostID == nil || !looksLikeUUID(*req.HostID) {
			return nil, errors.New("host_id must be a uuid when target is host")
		}
		allowed[ComponentNodeAgent], allowed[ComponentRecovery] = true, true
	case TargetControlPlane:
		if req.HostID != nil {
			return nil, errors.New("host_id must be absent when target is control_plane")
		}
		allowed[ComponentControlPlane], allowed[ComponentRecovery] = true, true
	default:
		return nil, errors.New("target must be control_plane or host")
	}
	if len(req.Components) == 0 || len(req.Components) > 2 {
		return nil, errors.New("components must name one or two images")
	}
	seen := map[string]bool{}
	for _, c := range req.Components {
		if !allowed[c.Name] {
			return nil, fmt.Errorf("component %q may not be applied to a %s target", c.Name, req.Target)
		}
		if seen[c.Name] {
			return nil, fmt.Errorf("component %q is named twice", c.Name)
		}
		seen[c.Name] = true
		if c.Image == "" || strings.ContainsAny(c.Image, " \t\n") || updater.ImageHasTagOrDigest(c.Image) {
			return nil, fmt.Errorf("component %q: image %q must be a repository reference with no tag and no digest", c.Name, c.Image)
		}
		if !RepositoryWellFormed(c.Image) {
			return nil, fmt.Errorf("component %q: image %q is not a registry host followed by lowercase path components", c.Name, c.Image)
		}
		if !devDigestRe.MatchString(c.Digest) {
			return nil, fmt.Errorf("component %q: digest %q is not sha256: + 64 lowercase hex", c.Name, c.Digest)
		}
	}
	// A1: the actor may lead the control plane only while a control-plane
	// replacement is in flight, so never on its own against that machine.
	if req.Target == TargetControlPlane && seen[ComponentRecovery] && !seen[ComponentControlPlane] {
		return nil, errors.New("a control_plane target names recovery-actor only together with control-plane")
	}
	out := append([]ComponentDigest(nil), req.Components...)
	sort.SliceStable(out, func(i, j int) bool { return componentRank(out[i].Name) < componentRank(out[j].Name) })
	return out, nil
}

// developerHostRefusal is the host-target decision on what the view already
// knows: "" when the host may take a developer apply, else the code and, for
// host_not_eligible, the EligibilityReason.
func developerHostRefusal(h HostIdentity, pre Preflight, attemptOpen, runActive bool) (code, reason string) {
	if !h.Known() {
		return CodeHostNotEligible, ReasonIdentityUnknown
	}
	if *h.InstallMode != InstallOwned {
		return CodeTargetNotOwned, ""
	}
	if !*h.UpdaterPresent {
		return CodeHostNotEligible, ReasonUpdaterAbsent
	}
	if h.Status == HostOffline || (h.AgentConnected != nil && !*h.AgentConnected) {
		return CodeHostNotEligible, ReasonHostOffline
	}
	if pre.Blocked() {
		return CodeHostNotEligible, ReasonPreflightBlocked
	}
	if attemptOpen {
		return CodeAttemptInFlight, ""
	}
	if runActive {
		return CodeRunActive, ""
	}
	return "", ""
}

// DeveloperCommitAllowed is ADR 0002 for a request that names no control-plane
// image: its commit is the installed control plane's own, or a known release's
// that orders at or below it. Anything else cannot be shown not to be ahead
// (fail closed).
func DeveloperCommitAllowed(commit string, cp View, known *Release) bool {
	if cp.Installed.ControlPlane.SourceCommit != nil && commitsMatch(*cp.Installed.ControlPlane.SourceCommit, commit) {
		return true
	}
	if known == nil {
		return false
	}
	var cpRelease *Release
	if c := cp.Installed.ControlPlane.SourceCommit; c != nil {
		cpRelease = matchRelease(cp.Available, rowChannel(cp.Channel), *c)
	}
	return !ordersAbove(*known, cpRelease, cp.Installed.ControlPlane, cp.Channel)
}

// WithDeveloperApply wires the route's registry reader and the namespace
// allowlist the control plane checks up front (QUASAR_UPDATER_ALLOWED_NAMESPACES).
func (h *ApplyHandler) WithDeveloperApply(dev DeveloperImages, allowed []string) *ApplyHandler {
	h.dev = dev
	h.allowedNamespaces = allowed
	return h
}

// OwnMachineSource is the control plane's own machine (OwnMachineReader).
type OwnMachineSource interface {
	Read(ctx context.Context) (OwnMachine, bool)
	Invalidate()
}

// WithOwnMachine wires the control plane's own machine; unwired is not owned.
func (h *ApplyHandler) WithOwnMachine(src OwnMachineSource) *ApplyHandler {
	h.ownMachine = src
	return h
}

// WithMachineShape wires the control plane's own machine shape (its configuration).
func (h *ApplyHandler) WithMachineShape(shape MachineShape) *ApplyHandler {
	h.machineShape = shape
	return h
}

func writeRefusal(w http.ResponseWriter, code, reason, message string) {
	if code == CodeHostNotEligible {
		var body notEligible
		body.Error.Code = CodeHostNotEligible
		body.Error.Message = message
		body.Reason = reason
		httpx.WriteJSON(w, http.StatusConflict, body)
		return
	}
	httpx.WriteError(w, http.StatusConflict, code, message)
}

// handleDeveloperApply serves POST /v1/admin/platform/developer-apply.
func (h *ApplyHandler) handleDeveloperApply(w http.ResponseWriter, r *http.Request) {
	if !h.ready(w) {
		return
	}
	var req DeveloperApplyRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed, "the request body is not a developer apply: "+err.Error())
		return
	}
	components, err := ValidateDeveloperApply(req)
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed, err.Error())
		return
	}
	ctx := r.Context()

	if req.Target == TargetControlPlane {
		// A failed read of the own machine is null, and null refuses (control-api.md
		// §"The control plane's own machine").
		var own OwnMachine
		ownOK := false
		if h.ownMachine != nil {
			h.ownMachine.Invalidate()
			own, ownOK = h.ownMachine.Read(ctx)
		}
		if !ownOK || own.Identity.InstallMode == nil || *own.Identity.InstallMode != InstallOwned {
			httpx.WriteError(w, http.StatusConflict, CodeTargetNotOwned,
				"this control plane does not report an owned install, so it takes no developer apply")
			return
		}
		h.developerApplyControlPlane(w, r, req, components)
		return
	}

	hostID := *req.HostID
	nodeName, err := h.store.HostNodeName(ctx, hostID)
	if err != nil {
		if errors.Is(err, ErrHostNotFound) {
			httpx.WriteError(w, http.StatusNotFound, httpx.CodeNotFound, "no such host")
			return
		}
		h.internal(w, "read host", err)
		return
	}
	// On a combined host the actor moves in the control-plane step. Decided from
	// this control plane's own configuration, so it holds (fail closed) whether or
	// not the recovery actor answers.
	if h.machineShape.SharesMachineWith(nodeName) {
		for _, c := range components {
			if c.Name != ComponentNodeAgent {
				httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed,
					"this host shares the control plane's machine, so a developer apply to it names only node-agent; its recovery actor moves with the control plane")
				return
			}
		}
	}
	view, err := h.freshView(ctx)
	if err != nil {
		h.internal(w, "build release view", err)
		return
	}
	active, err := h.store.ActiveRunExists(ctx)
	if err != nil {
		h.internal(w, "read active run", err)
		return
	}
	host, pre, open := developerHostFacts(view, hostID)
	if code, reason := developerHostRefusal(host, pre, open, active); code != "" {
		writeRefusal(w, code, reason, developerRefusalMessage(code, reason))
		return
	}

	// ADR 0001, up front: no registry outside the allowlist is ever contacted.
	for _, c := range components {
		if !updater.NamespaceAllowed(c.Image, h.allowedNamespaces) {
			httpx.WriteError(w, http.StatusConflict, CodeNamespaceRejected,
				fmt.Sprintf("component %s: image %s is outside the allowed platform-image namespaces (%s)",
					c.Name, c.Image, strings.Join(h.allowedNamespaces, ",")))
			return
		}
	}
	if h.dev == nil {
		httpx.WriteError(w, http.StatusConflict, CodeImageUnresolvable,
			"this control plane has no registry client to read the images' build identity")
		return
	}
	commit, err := h.dev.Commit(ctx, components)
	if err != nil {
		h.log.Warn("developer apply: image identity unreadable", "err", err)
		httpx.WriteError(w, http.StatusConflict, CodeImageUnresolvable, err.Error())
		return
	}
	known, err := h.store.ReleaseByCommit(ctx, commit)
	if err != nil && !errors.Is(err, ErrReleaseNotFound) {
		h.internal(w, "read release by commit", err)
		return
	}
	var knownRelease *Release
	if err == nil {
		knownRelease = &known
	}
	if !DeveloperCommitAllowed(commit, view, knownRelease) {
		writeRefusal(w, CodeHostNotEligible, ReasonReleaseAboveControlPlane,
			"the images' commit is neither this control plane's nor a release at or below it, so it cannot be shown not to be ahead of the control plane; apply the control plane first")
		return
	}
	if knownRelease != nil && releaseBelowFloor(*knownRelease, componentNames(components), buildinfo.DeclaredFloor()) {
		writeRefusal(w, CodeHostNotEligible, ReasonBelowFloor,
			"the images belong to a release below this control plane's floor, which it no longer manages")
		return
	}
	if !h.runner.Supported(hostID) {
		httpx.WriteError(w, http.StatusNotImplemented, CodeApplyUnsupported,
			"this host's agent did not answer a previous apply, so it predates the platform-release amendment; update it another way")
		return
	}

	previous, err := h.previousDigests(ctx, hostID, components)
	if err != nil {
		h.internal(w, "read previous digests", err)
		return
	}
	actor := actorID(r)
	attempt, err := h.store.CreateHostAttempt(ctx, NewHostAttempt{
		Kind:      KindDeveloperApply,
		HostID:    hostID,
		Requested: components,
		Previous:  previous,
		Force:     req.Force,
		Actor:     nilIfEmpty(actor),
	})
	if err != nil {
		if errors.Is(err, ErrAttemptInFlight) {
			httpx.WriteError(w, http.StatusConflict, CodeAttemptInFlight, "an update is already in flight on this host")
			return
		}
		h.internal(w, "create attempt", err)
		return
	}
	h.runner.RememberDeveloperCommit(attempt.ID, commit, host.SourceCommit)
	if n, err := h.store.NonTerminalSessions(ctx, hostID); err == nil {
		if err := h.store.SetWaitingSessions(ctx, attempt.ID, n); err == nil {
			attempt.State = AttemptWaitingSessions
			attempt.SessionsRemaining = &n
		}
	}

	digests := make([]map[string]string, 0, len(components))
	for _, c := range components {
		digests = append(digests, map[string]string{"name": c.Name, "digest": c.Digest})
	}
	audit.TryRecord(ctx, h.auditor, actor, "platform.apply.developer", "host", hostID, map[string]any{
		"attempt_id":                attempt.ID,
		"target":                    req.Target,
		"node_name":                 host.NodeName,
		"components":                digests,
		"source_commit":             commit,
		"force":                     req.Force,
		"external_backup_confirmed": req.ExternalBackupConfirmed,
	})
	h.runner.Start(attempt)
	httpx.WriteJSON(w, http.StatusAccepted, AttemptEnvelope{Attempt: attempt})
}

// developerHostFacts reads the host's identity, preflight and open-attempt
// flag from the view. A host the view does not list reads identity-unknown.
func developerHostFacts(v View, hostID string) (HostIdentity, Preflight, bool) {
	var host HostIdentity
	for _, h := range v.Installed.Hosts {
		if h.HostID == hostID {
			host = h
		}
	}
	var pre Preflight
	for _, t := range v.Targets {
		if t.Kind == TargetHost && t.HostID != nil && *t.HostID == hostID {
			pre = t.Preflight
		}
	}
	open := false
	if v.ActiveApply != nil {
		for _, a := range v.ActiveApply.Attempts {
			if a.HostID != nil && *a.HostID == hostID {
				open = true
			}
		}
	}
	return host, pre, open
}

func developerRefusalMessage(code, reason string) string {
	switch code {
	case CodeTargetNotOwned:
		return "this host is not a Quasar-owned install; a developer apply is only for owned machines"
	case CodeAttemptInFlight:
		return "an update is already in flight on this host"
	case CodeRunActive:
		return "a fleet update is running; a developer apply may not start while it owns the fleet"
	}
	return "this host cannot take a developer apply right now (" + reason + ")"
}

// ─── the registry reader ────────────────────────────────────────────────────

// RegistryDeveloperImages reads each image's config labels from its registry.
type RegistryDeveloperImages struct {
	inspect images.ImageInspector
}

func NewRegistryDeveloperImages(inspect images.ImageInspector) *RegistryDeveloperImages {
	return &RegistryDeveloperImages{inspect: inspect}
}

// Commit: every image must resolve, carry a full commit, and agree on it.
func (d *RegistryDeveloperImages) Commit(ctx context.Context, components []ComponentDigest) (string, error) {
	commit := ""
	for _, c := range components {
		ref := c.Image + "@" + c.Digest
		cfg, err := d.inspect.InspectConfig(ctx, ref)
		if err != nil {
			return "", fmt.Errorf("%s does not resolve at the registry as the control plane sees it: %w", ref, err)
		}
		// Digest-bound: the labels are those of the document that hashes to the
		// requested digest (the inspector verifies the manifest chain and blob).
		if cfg.ManifestDigest != c.Digest {
			return "", fmt.Errorf("%s: the registry answered a manifest whose digest is %q, not the requested one", ref, cfg.ManifestDigest)
		}
		got := strings.ToLower(cfg.Label(LabelSourceCommit))
		if !fullCommitRe.MatchString(got) {
			return "", fmt.Errorf("%s carries no readable %s label", ref, LabelSourceCommit)
		}
		if commit != "" && got != commit {
			return "", fmt.Errorf("the images disagree on their commit (%s and %s)", shortCommit(commit), shortCommit(got))
		}
		commit = got
	}
	return commit, nil
}

// RoutedInspector sends a reference to the plain-HTTP reader when its registry
// host is one the operator named, else to the hardened one.
type RoutedInspector struct {
	Plain      images.ImageInspector
	PlainHosts map[string]struct{}
	TLS        images.ImageInspector
}

func (r RoutedInspector) InspectConfig(ctx context.Context, ref string) (images.ImageConfig, error) {
	if _, ok := r.PlainHosts[registryHostOf(ref)]; ok && r.Plain != nil {
		return r.Plain.InspectConfig(ctx, ref)
	}
	return r.TLS.InspectConfig(ctx, ref)
}

// registryHostOf is the ref's first path element when it names a host.
func registryHostOf(ref string) string {
	first, _, found := strings.Cut(ref, "/")
	if !found || !(strings.ContainsAny(first, ".:") || first == "localhost") {
		return ""
	}
	return strings.ToLower(first)
}

// NamespaceHosts is the registry host of each allowed namespace: the hosts a
// developer apply's identity read must be able to reach.
func NamespaceHosts(namespaces []string) []string {
	out := make([]string, 0, len(namespaces))
	for _, ns := range namespaces {
		if h := registryHostOf(ns + "/x"); h != "" {
			out = append(out, h)
		}
	}
	return out
}

// ConfiguredInsecureRegistries reads QUASAR_PLATFORM_INSECURE_REGISTRIES.
func ConfiguredInsecureRegistries() map[string]struct{} {
	out := map[string]struct{}{}
	for _, part := range strings.Split(os.Getenv("QUASAR_PLATFORM_INSECURE_REGISTRIES"), ",") {
		if h := strings.ToLower(strings.TrimSpace(part)); h != "" {
			out[h] = struct{}{}
		}
	}
	return out
}

// ReleaseByCommit is the release row a full commit belongs to, on either
// channel; ErrReleaseNotFound when this instance knows none.
func (s *Store) ReleaseByCommit(ctx context.Context, commit string) (Release, error) {
	return s.releaseRow(ctx, `
		SELECT `+releaseColumns+` FROM platform_releases
		 WHERE lower(source_commit) = lower($1)
		 ORDER BY schema_version DESC, built_at DESC LIMIT 1`, commit)
}
