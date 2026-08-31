package secrets

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"

	"github.com/accreleus/quasar/control-plane/internal/auth"
	"github.com/accreleus/quasar/control-plane/internal/httpx"
)

// Handler serves the admin secrets surface.
//
// THE WIRE IS WRITE-ONLY. No response body from any route below ever contains a
// secret value. GET returns declarations plus configured/readable/masked-hint;
// PUT accepts a value and returns the same status shape; DELETE returns 204.
// There is deliberately no "reveal" endpoint: a stored credential is for the
// server to use, and an admin who needs the value again re-issues it at the
// provider.
type Handler struct {
	store *Store
	log   *slog.Logger
	// auditor mirrors the interface the artwork/console handlers use.
	auditor interface {
		Record(context.Context, string, string, string, string, map[string]any) error
	}
}

// NewHandler builds the handler. store may be nil only in route-registration
// tests; every request path checks it.
func NewHandler(store *Store, log *slog.Logger, auditors ...interface {
	Record(context.Context, string, string, string, string, map[string]any) error
}) *Handler {
	h := &Handler{store: store, log: log}
	if len(auditors) > 0 {
		h.auditor = auditors[0]
	}
	return h
}

// Register wires the routes. admin must already compose
// RequireAuth → RequireAdmin — this is the server-enforced gate
// (CLAUDE.md invariant #6); hiding the panel in the UI is never the control.
// Every route here is admin-gated. There is no non-admin route in this package.
func (h *Handler) Register(mux httpx.Router, admin func(http.Handler) http.Handler) {
	mux.Handle("GET /v1/admin/secrets", admin(http.HandlerFunc(h.handleList)))
	mux.Handle("PUT /v1/admin/secrets/{name}", admin(http.HandlerFunc(h.handleSet)))
	mux.Handle("DELETE /v1/admin/secrets/{name}", admin(http.HandlerFunc(h.handleDelete)))
}

// --- wire shapes -------------------------------------------------------------

// secretResp is the ONLY shape a secret is ever rendered in. It has no field
// that could hold a value; adding one would be the bug this type exists to
// prevent.
type secretResp struct {
	Name        string `json:"name"`
	Label       string `json:"label"`
	Description string `json:"description"`
	// EnvVar is the fallback environment variable, "" when none.
	EnvVar  string `json:"env_var"`
	DocsURL string `json:"docs_url"`

	// Configured is true when a value is stored in the database.
	Configured bool `json:"configured"`
	// Readable is false when a value is stored but the master key cannot open it.
	Readable bool `json:"readable"`
	// Hint is the masked tail of the STORED value ("" when nothing is stored or
	// the value is too short to mask safely). Never a full value.
	Hint string `json:"hint"`
	// EnvSet reports whether the fallback env var is present on the server —
	// presence only, never the value.
	EnvSet bool `json:"env_set"`
	// Origin is which source is actually in effect: "database", "environment"
	// or "none". This is what stops an operator guessing why their new key did
	// or did not take.
	Origin string `json:"origin"`

	KeyVersion int     `json:"key_version"`
	UpdatedBy  *string `json:"updated_by"`
	UpdatedAt  *string `json:"updated_at"`
	Problem    string  `json:"problem,omitempty"`
}

type secretsEnvelope struct {
	Secrets []secretResp `json:"secrets"`
	// MasterKeyConfigured is false when QUASAR_SECRET_KEY is unset. The UI reads
	// this to explain that storing secrets is unavailable on this deployment
	// rather than offering a field whose save always fails.
	MasterKeyConfigured bool `json:"master_key_configured"`
	// KeyVersions lists the master-key versions this control plane holds, so a
	// version mismatch is diagnosable from the UI.
	KeyVersions []int `json:"key_versions"`
}

// describe renders one descriptor's current state. It reads os.Getenv only to
// learn PRESENCE of the fallback — the value is never copied anywhere.
func (h *Handler) describe(ctx context.Context, d Descriptor) secretResp {
	out := secretResp{
		Name:        d.Name,
		Label:       d.Label,
		Description: d.Description,
		EnvVar:      d.EnvVar,
		DocsURL:     d.DocsURL,
		Origin:      OriginNone,
	}
	if d.EnvVar != "" {
		out.EnvSet = os.Getenv(d.EnvVar) != ""
	}
	st, err := h.store.Status(ctx, d.Name)
	if err != nil {
		h.log.Warn("secrets: could not read status", "secret", d.Name, "err", err)
		out.Problem = "This secret's status could not be read."
		return out
	}
	out.Configured = st.Configured
	out.Readable = st.Readable
	out.Hint = st.Hint
	out.KeyVersion = st.KeyVersion
	out.UpdatedBy = st.UpdatedBy
	out.Problem = st.Problem
	if st.UpdatedAt != nil {
		s := st.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z07:00")
		out.UpdatedAt = &s
	}
	switch {
	case st.Configured && st.Readable:
		out.Origin = OriginDatabase
	case st.Configured:
		// Stored but unreadable: nothing is in effect. Falling back to the env
		// here would silently use a different credential than the one an admin
		// set, so the origin is "none" and Problem explains why.
		out.Origin = OriginNone
	case out.EnvSet:
		out.Origin = OriginEnvironment
	}
	return out
}

// --- handlers ----------------------------------------------------------------

func (h *Handler) handleList(w http.ResponseWriter, r *http.Request) {
	if !h.ready(w) {
		return
	}
	descs := h.store.Registry().All()
	env := secretsEnvelope{
		Secrets:             make([]secretResp, 0, len(descs)),
		MasterKeyConfigured: h.store.Available(),
		KeyVersions:         h.store.KeyVersions(),
	}
	if env.KeyVersions == nil {
		env.KeyVersions = []int{}
	}
	for _, d := range descs {
		env.Secrets = append(env.Secrets, h.describe(r.Context(), d))
	}
	httpx.WriteJSON(w, http.StatusOK, env)
}

func (h *Handler) handleSet(w http.ResponseWriter, r *http.Request) {
	if !h.ready(w) {
		return
	}
	name := r.PathValue("name")
	var req struct {
		Value string `json:"value"`
	}
	// Strict + bounded decode. On failure the body is NOT echoed: it contains a
	// credential, so the error names the defect and nothing else.
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed, "malformed JSON body")
		return
	}
	user, _ := auth.UserFromContext(r.Context())
	if err := h.store.Set(r.Context(), name, req.Value, user.ID); err != nil {
		h.writeErr(w, name, err)
		return
	}
	// Audited by NAME only. The value is never a detail on an audit row.
	h.record(r, "instance.secret.set", name)
	h.writeStatus(w, r, name)
}

func (h *Handler) handleDelete(w http.ResponseWriter, r *http.Request) {
	if !h.ready(w) {
		return
	}
	name := r.PathValue("name")
	if err := h.store.Delete(r.Context(), name); err != nil {
		h.writeErr(w, name, err)
		return
	}
	h.record(r, "instance.secret.cleared", name)
	w.WriteHeader(http.StatusNoContent)
}

// writeStatus answers with the single-secret status shape after a write.
func (h *Handler) writeStatus(w http.ResponseWriter, r *http.Request, name string) {
	d, ok := h.store.Registry().Lookup(name)
	if !ok {
		httpx.WriteError(w, http.StatusNotFound, httpx.CodeNotFound, "unknown secret")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"secret":                h.describe(r.Context(), d),
		"master_key_configured": h.store.Available(),
	})
}

// --- plumbing ----------------------------------------------------------------

func (h *Handler) ready(w http.ResponseWriter) bool {
	if h.store == nil {
		httpx.WriteError(w, http.StatusServiceUnavailable, httpx.CodeInternal,
			"the secrets service is not available on this deployment")
		return false
	}
	return true
}

// writeErr maps sentinels to distinct, actionable statuses.
//
// The two key-management failures are deliberately DIFFERENT responses:
// "no master key" is 409 with a setup instruction, "wrong master key" is 409
// with a recovery instruction. Collapsing them into one message is the exact
// confusion this facility is meant to remove.
func (h *Handler) writeErr(w http.ResponseWriter, name string, err error) {
	switch {
	case errors.Is(err, ErrUnknownSecret):
		httpx.WriteError(w, http.StatusNotFound, httpx.CodeNotFound,
			"this deployment does not define a secret by that name")
	case errors.Is(err, ErrEmptyValue):
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed,
			"value must not be empty — delete the secret instead")
	case errors.Is(err, ErrNoMasterKey):
		httpx.WriteError(w, http.StatusConflict, httpx.CodeConflict,
			"no master key is configured on this control plane, so secrets cannot be stored. "+
				"Set QUASAR_SECRET_KEY to a base64 32-byte key and restart.")
	case errors.Is(err, ErrKeyMismatch):
		httpx.WriteError(w, http.StatusConflict, httpx.CodeConflict,
			"the master key does not match the stored secret. Restore the original QUASAR_SECRET_KEY, "+
				"or set this secret again to re-encrypt it under the current key.")
	case errors.Is(err, ErrNotFound):
		httpx.WriteError(w, http.StatusNotFound, httpx.CodeNotFound, "no value is stored for that secret")
	default:
		// Log by NAME. The error chain here can only contain driver text, never a
		// value (encryption happens before the driver sees anything), but the
		// response stays generic regardless.
		h.log.Warn("secrets: request failed", "secret", name, "err", err)
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not update the secret")
	}
}

func (h *Handler) record(r *http.Request, action, name string) {
	if h.auditor == nil {
		return
	}
	user, _ := auth.UserFromContext(r.Context())
	if err := h.auditor.Record(r.Context(), user.ID, action, "secret", name, nil); err != nil {
		h.log.Warn("secrets: record admin activity failed", "action", action, "err", err)
	}
}
