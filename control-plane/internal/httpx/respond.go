// Package httpx holds small HTTP helpers shared across control-plane handlers:
// uniform JSON responses and the error envelope defined by protocol/control-api.md.
package httpx

import (
	"encoding/json"
	"log/slog"
	"net/http"
)

// Error codes. These are the only codes a client may see in an error envelope;
// semantics (remedy, retryability, which endpoint emits which) live in
// protocol/control-api.md §Errors. Add a code there before adding it here.
const (
	CodeValidationFailed       = "validation_failed"        // 400
	CodeUnauthorized           = "unauthorized"             // 401
	CodeInvalidCredentials     = "invalid_credentials"      // 401 (login)
	CodeForbidden              = "forbidden"                // 403
	CodeNotFound               = "not_found"                // 404
	CodeConflict               = "conflict"                 // 409
	CodeRateLimited            = "rate_limited"             // 429
	CodeNoHostAvailable        = "no_host_available"        // 503, retryable
	CodeCapacityExhausted      = "capacity_exhausted"       // 503, retryable
	CodeSessionQuota           = "session_quota_exceeded"   // 409
	CodeSessionNotSwappable    = "session_not_swappable"    // 409
	CodeSwapExceedsReservation = "swap_exceeds_reservation" // 409
	CodeSessionNotRunning      = "session_not_running"      // 409
	CodeDisplayUpdateRejected  = "display_update_rejected"  // 409
	// Refused before dispatch, unlike display_update_rejected (the agent nacking a
	// command it was sent): the host encoder cannot change the encoded size live.
	CodeExternalResizeUnsupported = "external_resize_unsupported" // 409
	CodeHomeInUse                 = "home_in_use"                 // 409
	// home_not_provisioned and parent_app_disabled are the only two refusals whose
	// remedy lies outside the caller's reach; never fold either into `conflict`.
	CodeHomeNotProvisioned   = "home_not_provisioned"           // 409
	CodeParentAppDisabled    = "parent_app_disabled"            // 409
	CodeProfileIneligible    = "profile_ineligible"             // 409
	CodeProfileNotLaunchable = "profile_not_launchable_for_app" // 409
	CodeRegistrationClosed   = "registration_closed"            // 403
	CodeInvalidInvite        = "invalid_invite"                 // 400, non-enumerating
	CodeClientTooOld         = "client_too_old"                 // 426
	CodeAlreadyInstalled     = "already_installed"              // 409
	CodeDigestUnresolved     = "digest_unresolved"              // 409
	CodeNotInstalled         = "not_installed"                  // 404
	CodeContextUnresolved    = "context_unresolved"             // 409
	CodeProviderEnabled      = "provider_enabled"               // 409
	// Refusal, never an auto-enable: library discovery walks every user's home, so
	// the switch is fail-closed by design (settings.Store.LibraryDiscoveryEnabled).
	CodeLibraryDiscoveryDisabled = "library_discovery_disabled" // 409
	CodeInternal                 = "internal"                   // 500
	CodeCapacityUnavailable      = "capacity_unavailable"       // 409, deprecated: never emit
	CodeCaptureBusy              = "capture_busy"               // 409
	CodeCaptureKindUnsupported   = "capture_kind_unsupported"   // 422
	CodeCaptureUnsupported       = "capture_unsupported"        // 501
	CodeAgentNotConnected        = "agent_not_connected"        // 503
	CodeJobDisabled              = "job_disabled"               // 409
	CodeJobAlreadyRunning        = "job_already_running"        // 409
	CodeJobUnmanaged             = "job_unmanaged"              // 409
	// schedule_locked: an env var is authoritative over the edited field; see
	// internal/jobs.EffectiveInterval.
	CodeScheduleLocked = "schedule_locked" // 409
	// Guards a managed runtime preset (managed_image_id, migration 0058) against an
	// `image` value the adoption pipeline could not have produced (#498).
	CodeManagedPresetImageInvalid = "managed_preset_image_invalid" // 422
	// invalid_state: an unrecognized ?state= filter value, never widened to the
	// default. semantics: control-api.md §UI v3 console
	CodeInvalidState = "invalid_state" // 400
)

// errorEnvelope is the wire shape: { "error": { "code", "message" } }.
type errorEnvelope struct {
	Error errorBody `json:"error"`
}

type errorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// WriteJSON serializes v as JSON with the given status code.
func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if v == nil {
		return
	}
	if err := json.NewEncoder(w).Encode(v); err != nil {
		// The header/status are already written; nothing left but to log.
		slog.Error("write json response", "err", err)
	}
}

// WriteError emits the uniform error envelope with the given status and code.
func WriteError(w http.ResponseWriter, status int, code, message string) {
	WriteJSON(w, status, errorEnvelope{Error: errorBody{Code: code, Message: message}})
}
