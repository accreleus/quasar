package crud

// GET / PATCH /v1/admin/hosts/{id}/webtransport — a host's reported WebTransport endpoint and
// the admin's public-URL override. semantics: control-api.md §WebTransport endpoint

import (
	"errors"
	"net/http"
	"time"

	"github.com/accreleus/quasar/control-plane/internal/httpx"
	"github.com/accreleus/quasar/control-plane/internal/wtendpoint"
)

type webTransportBody struct {
	Reported *wtendpoint.Reported `json:"reported"`
	Override *wtendpoint.Override `json:"override"`
	Endpoint *wtendpoint.Endpoint `json:"endpoint"`
	Usable   bool                 `json:"usable"`
	Reason   *string              `json:"reason"`
}

func (h *Handler) webTransportBody(r *http.Request, hostID string) (webTransportBody, error) {
	host, err := wtendpoint.Load(r.Context(), h.store.pool, hostID)
	if err != nil {
		return webTransportBody{}, err
	}
	res := wtendpoint.Resolve(host, time.Now())
	body := webTransportBody{Override: host.Override, Endpoint: res.Endpoint, Usable: res.Usable()}
	if host.Reported.Serves() {
		body.Reported = host.Reported
	}
	if res.Reason != "" {
		body.Reason = &res.Reason
	}
	return body, nil
}

func (h *Handler) writeWebTransport(w http.ResponseWriter, r *http.Request, hostID string) {
	body, err := h.webTransportBody(r, hostID)
	switch {
	case errors.Is(err, wtendpoint.ErrHostNotFound):
		httpx.WriteError(w, http.StatusNotFound, httpx.CodeNotFound, "host not found")
	case err != nil:
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not read the host's WebTransport endpoint")
	default:
		httpx.WriteJSON(w, http.StatusOK, body)
	}
}

func (h *Handler) handleGetHostWebTransport(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !isValidUUID(id) {
		httpx.WriteError(w, http.StatusNotFound, httpx.CodeNotFound, "host not found")
		return
	}
	h.writeWebTransport(w, r, id)
}

func (h *Handler) handlePatchHostWebTransport(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !isValidUUID(id) {
		httpx.WriteError(w, http.StatusNotFound, httpx.CodeNotFound, "host not found")
		return
	}
	var req struct {
		PublicURL   *string `json:"public_url"`
		Certificate *string `json:"certificate"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	certificate := wtendpoint.CertPublic
	if req.Certificate != nil {
		certificate = *req.Certificate
	}
	if certificate != wtendpoint.CertPublic && certificate != wtendpoint.CertAgent {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed, `certificate must be "public" or "agent"`)
		return
	}
	if req.PublicURL != nil {
		if err := wtendpoint.ValidatePublicURL(*req.PublicURL); err != nil {
			httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed, err.Error())
			return
		}
	}
	if err := wtendpoint.SetOverride(r.Context(), h.store.pool, id, req.PublicURL, certificate); err != nil {
		if errors.Is(err, wtendpoint.ErrHostNotFound) {
			httpx.WriteError(w, http.StatusNotFound, httpx.CodeNotFound, "host not found")
			return
		}
		httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "could not save the WebTransport override")
		return
	}
	details := map[string]any{"public_url": nil, "certificate": certificate}
	if req.PublicURL != nil {
		details["public_url"] = *req.PublicURL
	}
	h.recordActivity(r, "host.webtransport.update", "host", id, details)
	h.writeWebTransport(w, r, id)
}
