package setup

import (
	"encoding/json"
	"io"
	"net/http"

	"github.com/accreleus/quasar/control-plane/internal/httpx"
)

// maxBody bounds the claim request body — three small fields.
const maxBody = 4 << 10

// decodeJSON decodes the request body, writing a 400 validation_failed and
// returning false on malformed input or unknown fields. Mirrors internal/auth's
// decodeJSON so the setup surface enforces the same envelope conventions.
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed, "malformed JSON body")
		return false
	}
	// Exactly one top-level value: a valid object followed by trailing JSON is
	// malformed, not silently ignored.
	if dec.Decode(&struct{}{}) != io.EOF {
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeValidationFailed, "malformed JSON body")
		return false
	}
	return true
}
