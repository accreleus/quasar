package platform

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/accreleus/quasar/control-plane/internal/audit"
	"github.com/accreleus/quasar/control-plane/internal/buildinfo"
	"github.com/accreleus/quasar/control-plane/internal/httpx"
	"github.com/accreleus/quasar/control-plane/internal/updater"
)

// The control-plane target of a developer apply on an owned machine.
// semantics: control-api.md §"Developer apply" (amendment 14).

// developerSchema reads the schema version a control-plane image declares.
type developerSchema interface {
	ControlPlaneSchema(ctx context.Context, c ComponentDigest) (int, error)
}

// controlPlaneDeveloper drives the attempt (SelfDeveloperRunner).
type controlPlaneDeveloper interface{ Start(a Attempt) }

// admittedSchemaNoter keeps what admission read of the control-plane image
// (SelfDeveloperRunner.NoteDeveloperSchema).
type admittedSchemaNoter interface {
	NoteDeveloperSchema(attemptID string, schema int, migrates bool)
}

// WithSelfDeveloper wires the driver of control-plane developer applies.
func (h *ApplyHandler) WithSelfDeveloper(d controlPlaneDeveloper) *ApplyHandler {
	h.selfDev = d
	return h
}

func (h *ApplyHandler) developerApplyControlPlane(w http.ResponseWriter, r *http.Request, req DeveloperApplyRequest, components []ComponentDigest) {
	ctx := r.Context()
	active, err := h.store.ActiveRunExists(ctx)
	if err != nil {
		h.internal(w, "read active run", err)
		return
	}
	if active {
		httpx.WriteError(w, http.StatusConflict, CodeRunActive,
			"a fleet update is running; a developer apply may not start while it owns the fleet")
		return
	}
	view, err := h.freshView(ctx)
	if err != nil {
		h.internal(w, "build release view", err)
		return
	}
	if view.ActiveApply != nil {
		for _, a := range view.ActiveApply.Attempts {
			if a.HostID == nil {
				httpx.WriteError(w, http.StatusConflict, CodeAttemptInFlight, "an update of the control plane is already in flight")
				return
			}
		}
	}
	for _, t := range view.Targets {
		if t.Kind == TargetControlPlane && t.Preflight.Blocked() {
			httpx.WriteError(w, http.StatusConflict, CodePreflightBlocked,
				"the control plane's preflight is blocked; fix what it names, then apply again")
			return
		}
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
	schemas, ok := h.dev.(developerSchema)
	if h.dev == nil || !ok {
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
	var cp ComponentDigest
	for _, c := range components {
		if c.Name == ComponentControlPlane {
			cp = c
		}
	}
	schema, err := schemas.ControlPlaneSchema(ctx, cp)
	if err != nil {
		httpx.WriteError(w, http.StatusConflict, CodeImageUnresolvable, err.Error())
		return
	}
	installed := view.Installed.ControlPlane.SchemaVersion
	if schema < installed {
		httpx.WriteError(w, http.StatusUnprocessableEntity, CodeReleaseBelowSchemaVersion,
			fmt.Sprintf("the control-plane image is at schema %d, below the database's %d (ADR 0002)", schema, installed))
		return
	}
	// A migrating digest follows #352 decision 14 exactly as a migrating release
	// does: the runner drains the instance, then the recovery actor dumps a
	// Quasar-owned database or requires the operator's confirmation for theirs.
	migrates := schema > installed
	if h.selfDev == nil {
		httpx.WriteError(w, http.StatusNotImplemented, CodeApplyUnsupported,
			"this control plane cannot drive a developer apply to itself")
		return
	}
	last, err := h.store.LastSucceededControlPlaneDigests(ctx)
	if err != nil {
		h.internal(w, "read previous digests", err)
		return
	}
	actor := actorID(r)
	attempt, err := h.store.CreateControlPlaneAttempt(ctx, NewControlPlaneAttempt{
		Kind:      KindDeveloperApply,
		Requested: components,
		Previous:  previousOrUnknown(last, components),
		Actor:     nilIfEmpty(actor),
		Force:     req.Force,
	})
	if err != nil {
		if errors.Is(err, ErrAttemptInFlight) {
			httpx.WriteError(w, http.StatusConflict, CodeAttemptInFlight, "an update of the control plane is already in flight")
			return
		}
		h.internal(w, "create attempt", err)
		return
	}
	digests := make([]map[string]string, 0, len(components))
	for _, c := range components {
		digests = append(digests, map[string]string{"name": c.Name, "digest": c.Digest})
	}
	audit.TryRecord(ctx, h.auditor, actor, "platform.apply.developer", "platform", attempt.ID, map[string]any{
		"attempt_id":                attempt.ID,
		"target":                    req.Target,
		"components":                digests,
		"source_commit":             commit,
		"force":                     req.Force,
		"external_backup_confirmed": req.ExternalBackupConfirmed,
	})
	if migrates && h.externalDatabase(ctx) && !req.ExternalBackupConfirmed {
		// Failed before anything is held, drained or sent: nothing changed.
		if err := h.store.FailAttempt(ctx, attempt.ID, ReasonBackupUnconfirmed,
			"this build changes the database, and no current backup of your own database was confirmed. "+
				"Quasar never dumps an operator's database: take a backup with your own tools, then apply again, confirming it. Nothing was changed"); err != nil {
			h.internal(w, "fail attempt", err)
			return
		}
		if failed, err := h.store.Attempt(ctx, attempt.ID); err == nil {
			attempt = failed
		}
		httpx.WriteJSON(w, http.StatusAccepted, AttemptEnvelope{Attempt: attempt})
		return
	}
	if c, ok := h.selfDev.(backupConfirmer); ok && migrates && req.ExternalBackupConfirmed {
		c.ConfirmExternalBackup(attempt.ID)
	}
	// The schema is read once, here: the drain and the send use this answer.
	if n, ok := h.selfDev.(admittedSchemaNoter); ok {
		n.NoteDeveloperSchema(attempt.ID, schema, migrates)
	}
	h.selfDev.Start(attempt)
	httpx.WriteJSON(w, http.StatusAccepted, AttemptEnvelope{Attempt: attempt})
}

// externalDatabase: this control plane's recovery actor says its database is the
// operator's own. Unknown reads as not: the actor refuses such a step itself.
func (h *ApplyHandler) externalDatabase(ctx context.Context) bool {
	if h.ownMachine == nil {
		return false
	}
	own, ok := h.ownMachine.Read(ctx)
	return ok && own.Identity.DatabaseMode != nil && *own.Identity.DatabaseMode == DatabaseModeExternal
}

// SchemaOf is the schema version a developer apply's control-plane image
// declares (SelfApplier.DeveloperSchema).
func (d *RegistryDeveloperImages) SchemaOf(ctx context.Context, components []ComponentDigest) (int, error) {
	for _, c := range components {
		if c.Name == ComponentControlPlane {
			return d.ControlPlaneSchema(ctx, c)
		}
	}
	return 0, errors.New("the request names no control-plane image")
}

// Migrates reports whether a developer apply's control-plane image moves the
// schema past this binary's (SelfDeveloperRunner.Migrates).
func (d *RegistryDeveloperImages) Migrates(ctx context.Context, components []ComponentDigest) (bool, error) {
	schema, err := d.SchemaOf(ctx, components)
	if err != nil {
		return false, err
	}
	return schema > buildinfo.SchemaVersion(), nil
}

// ControlPlaneSchema is the schema version the control-plane image at this
// digest declares (LabelSchemaVersion).
func (d *RegistryDeveloperImages) ControlPlaneSchema(ctx context.Context, c ComponentDigest) (int, error) {
	ref := c.Image + "@" + c.Digest
	cfg, err := d.inspect.InspectConfig(ctx, ref)
	if err != nil {
		return 0, fmt.Errorf("%s does not resolve at the registry as the control plane sees it: %w", ref, err)
	}
	if cfg.ManifestDigest != c.Digest {
		return 0, fmt.Errorf("%s: the registry answered a manifest whose digest is %q, not the requested one", ref, cfg.ManifestDigest)
	}
	schema, err := parseSchemaLabel(cfg.Label(LabelSchemaVersion))
	if err != nil {
		return 0, fmt.Errorf("%s: %w", ref, err)
	}
	return schema, nil
}
