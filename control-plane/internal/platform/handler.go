// Package platform serves the platform-release admin surface (control-api.md
// §"Platform releases", amendment 1).
//
// Only `GET /v1/admin/platform/identity` lives here today: a pure read of the
// running binary's own stamps, touching no database row and never varying with
// the channel. `GET /v1/admin/platform/releases` — the release view over the
// `platform_releases` detector cache — is the detection ticket's, and lands
// beside this one.
package platform

import (
	"net/http"

	"github.com/accreleus/quasar/control-plane/internal/buildinfo"
	"github.com/accreleus/quasar/control-plane/internal/httpx"
)

// Handler serves the platform-release read surface. It holds no state: the
// identity it reports is linker-injected into internal/buildinfo.
type Handler struct{}

// NewHandler builds the platform HTTP handler.
func NewHandler() *Handler { return &Handler{} }

// Register wires the admin platform routes. admin must compose
// RequireAuth→RequireAdmin — the server-enforced gate, never UI hiding
// (control-api.md §Authorization).
func (h *Handler) Register(mux httpx.Router, admin func(http.Handler) http.Handler) {
	mux.Handle("GET /v1/admin/platform/identity", admin(http.HandlerFunc(h.handleIdentity)))
}

// identityResponse is the `{ "identity": … }` envelope openapi.yaml declares.
// The envelope, rather than a bare object, so the response can grow a sibling
// key without a shape change.
type identityResponse struct {
	Identity buildinfo.Identity `json:"identity"`
}

// handleIdentity answers "what is this control plane". There is no 404 shape:
// a control plane always has an identity to report, and an unstamped build
// reports `"dev"` with two nulls rather than failing.
func (h *Handler) handleIdentity(w http.ResponseWriter, _ *http.Request) {
	httpx.WriteJSON(w, http.StatusOK, identityResponse{Identity: buildinfo.Get()})
}
