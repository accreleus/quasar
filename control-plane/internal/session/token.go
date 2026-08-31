package session

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"
)

// SignalingTokenTTL is the lifetime of the single-use signaling token minted at
// launch (control-api.md / signaling.md default 60 s). The client must connect
// promptly; the token is consumed on first valid signaling connect (P1-7).
const SignalingTokenTTL = 60 * time.Second

// signalingToken is a freshly minted single-use token: the plaintext returned to
// the client exactly once, and the SHA-256 hash persisted on the session row.
type signalingToken struct {
	Plaintext string
	Hash      string
	ExpiresAt time.Time
}

// newSignalingToken mints a random opaque token (32 bytes hex) and its hash.
func newSignalingToken(now time.Time) (signalingToken, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return signalingToken{}, fmt.Errorf("generate signaling token: %w", err)
	}
	plain := hex.EncodeToString(b)
	h := sha256.Sum256([]byte(plain))
	return signalingToken{
		Plaintext: plain,
		Hash:      hex.EncodeToString(h[:]),
		ExpiresAt: now.Add(SignalingTokenTTL),
	}, nil
}
