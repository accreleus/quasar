package auth

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

func TestGenerateTokenUniqueAndHashed(t *testing.T) {
	p1, h1, err := generateToken()
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	p2, h2, err := generateToken()
	if err != nil {
		t.Fatalf("generate: %v", err)
	}

	if p1 == p2 {
		t.Fatal("two generated tokens are identical")
	}
	if h1 == h2 {
		t.Fatal("two token hashes are identical")
	}
	if p1 == h1 {
		t.Fatal("plaintext token equals its hash — hash not applied")
	}

	// hash must be the sha256 hex digest of the plaintext.
	want := sha256.Sum256([]byte(p1))
	if h1 != hex.EncodeToString(want[:]) {
		t.Fatal("hash is not sha256(plaintext)")
	}
	if hashToken(p1) != h1 {
		t.Fatal("hashToken not deterministic")
	}
}
