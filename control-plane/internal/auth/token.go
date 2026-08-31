package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
)

// tokenBytes is the entropy of an opaque bearer token. 32 bytes = 256 bits.
const tokenBytes = 32

// generateToken returns a new opaque bearer token (URL-safe base64, no padding)
// and its SHA-256 hex digest. Only the digest is persisted (auth_tokens.token_hash);
// the plaintext is returned to the client exactly once.
func generateToken() (plaintext, hash string, err error) {
	b := make([]byte, tokenBytes)
	if _, err := rand.Read(b); err != nil {
		return "", "", fmt.Errorf("generate token: %w", err)
	}
	plaintext = base64.RawURLEncoding.EncodeToString(b)
	return plaintext, hashToken(plaintext), nil
}

// hashToken returns the SHA-256 hex digest used as the auth_tokens lookup key.
// A bearer token is high-entropy random, so a fast hash (not a password KDF) is
// the correct choice: there is nothing to brute-force.
func hashToken(plaintext string) string {
	sum := sha256.Sum256([]byte(plaintext))
	return hex.EncodeToString(sum[:])
}
