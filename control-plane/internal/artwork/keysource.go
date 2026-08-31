package artwork

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"log/slog"
	"net/http"
	"sync"

	"github.com/accreleus/quasar/control-plane/internal/secrets"
)

// SecretProviderSource resolves the artwork provider from the encrypted secrets
// store on every use, with the legacy environment variable as a fallback.
//
// THIS IS WHAT MAKES A UI-SET KEY TAKE EFFECT WITHOUT A RESTART. Nothing here
// is captured at construction except the plumbing; the credential itself is
// read (and decrypted) at the moment a provider is needed and is dropped again
// as soon as the client is built.
//
// The constructed *SteamGridDBClient IS cached, deliberately: it owns the
// outbound throttle, and rebuilding it per call would let a sweep burst past
// the rate limit. The cache is keyed on a SHA-256 FINGERPRINT of the credential
// rather than the credential itself, and fingerprints are compared in constant
// time — so the hot path never does a variable-time comparison on secret
// material.
type SecretProviderSource struct {
	store *secrets.Store
	// envFallback is the value of QUASAR_STEAMGRIDDB_API_KEY as read at startup.
	// A fallback, never an override: see secrets.Store.Resolve for the precedence
	// argument.
	envFallback string
	// disabled short-circuits everything when QUASAR_ARTWORK_PROVIDER=none. An
	// operator who turned the provider off must not have it turned back on by
	// someone typing a key into the admin UI.
	disabled bool
	baseURL  string
	http     *http.Client
	log      *slog.Logger

	mu     sync.Mutex
	fprint [sha256.Size]byte
	client *SteamGridDBClient
}

// NewSecretProviderSource builds the source. store may be nil (no database
// wiring), in which case only the env fallback is consulted.
func NewSecretProviderSource(store *secrets.Store, envFallback string, disabled bool, log *slog.Logger) *SecretProviderSource {
	return &SecretProviderSource{
		store:       store,
		envFallback: envFallback,
		disabled:    disabled,
		log:         log,
	}
}

// Provider resolves the credential and returns the provider in effect.
func (s *SecretProviderSource) Provider(ctx context.Context) (Provider, ProviderInfo) {
	if s.disabled {
		return nil, ProviderInfo{Origin: OriginNone,
			Problem: "The artwork provider is switched off on this deployment (QUASAR_ARTWORK_PROVIDER=none)."}
	}

	var val secrets.Value
	if s.store != nil {
		v, err := s.store.Resolve(ctx, secrets.NameArtworkAPIKey, s.envFallback)
		switch {
		case err == nil:
			val = v
		case errors.Is(err, secrets.ErrNoMasterKey):
			// A key IS stored but this control plane holds no master key. Falling
			// back to the env here would quietly use a different credential than
			// the one an admin configured, so it does not.
			return nil, ProviderInfo{Origin: OriginNone,
				Problem: "An artwork API key is stored but no master key is configured on this control plane (QUASAR_SECRET_KEY), so it cannot be decrypted."}
		case errors.Is(err, secrets.ErrKeyMismatch):
			return nil, ProviderInfo{Origin: OriginNone,
				Problem: "An artwork API key is stored but the configured master key does not match it. Restore the original QUASAR_SECRET_KEY, or set the key again."}
		default:
			// A database blip must not look like "no key configured", but it also
			// must not take the admin page down. Log and report unavailable.
			s.log.Warn("artwork: could not resolve the provider credential", "err", err)
			return nil, ProviderInfo{Origin: OriginNone,
				Problem: "The artwork API key could not be read from the database."}
		}
	} else if s.envFallback != "" {
		val = secrets.Value{Secret: s.envFallback, Origin: secrets.OriginEnvironment}
	}

	if !val.Configured() {
		return nil, ProviderInfo{Origin: OriginNone}
	}
	return s.clientFor(val.Secret), ProviderInfo{
		Configured: true,
		Name:       "steamgriddb",
		Origin:     val.Origin,
	}
}

// clientFor returns the cached client when the credential is unchanged, and
// builds a new one when it is not. The credential is fingerprinted rather than
// stored for comparison, and the comparison is constant-time.
func (s *SecretProviderSource) clientFor(apiKey string) *SteamGridDBClient {
	fp := sha256.Sum256([]byte(apiKey))
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.client != nil && subtle.ConstantTimeCompare(s.fprint[:], fp[:]) == 1 {
		return s.client
	}
	s.client = NewSteamGridDBClient(apiKey, s.baseURL, s.http)
	s.fprint = fp
	// The STATE changes, never the value. There is no log line in this package
	// that takes the key as an argument.
	s.log.Info("artwork: provider credential loaded")
	return s.client
}
