package crud

import "context"

// HostBody is one host exactly as `GET /v1/hosts/{id}` serves it, for another
// admin route that answers with the host (platform's host removal).
func (h *Handler) HostBody(ctx context.Context, id string) (any, error) {
	host, err := h.store.getHost(ctx, id)
	if err != nil {
		return nil, err
	}
	return hostToResp(host), nil
}
