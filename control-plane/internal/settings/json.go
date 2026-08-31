package settings

import (
	"encoding/json"
	"net/http"

	"github.com/accreleus/quasar/control-plane/internal/httpx"
)

// decodeJSON decodes a size-bounded, strict JSON body, writing a 400 and returning
// false on malformed or unknown-field input (mirrors auth.decodeJSON).
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed, "malformed JSON body")
		return false
	}
	return true
}
