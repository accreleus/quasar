package auth

import (
	"strings"
	"testing"
)

// testParams are deliberately light so unit tests run fast; production uses
// DefaultParams().
func testParams() Params {
	return Params{Memory: 8 * 1024, Time: 1, Threads: 1, SaltLen: 16, KeyLen: 32}
}

func TestHashAndVerify(t *testing.T) {
	hash, err := HashPassword("correct horse battery staple", testParams())
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if !strings.HasPrefix(hash, "$argon2id$v=19$") {
		t.Fatalf("unexpected PHC prefix: %q", hash)
	}

	ok, err := VerifyPassword("correct horse battery staple", hash)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if !ok {
		t.Fatal("correct password failed to verify")
	}

	ok, err = VerifyPassword("wrong password", hash)
	if err != nil {
		t.Fatalf("verify wrong: %v", err)
	}
	if ok {
		t.Fatal("wrong password verified as correct")
	}
}

func TestHashIsSalted(t *testing.T) {
	h1, _ := HashPassword("samepassword", testParams())
	h2, _ := HashPassword("samepassword", testParams())
	if h1 == h2 {
		t.Fatal("two hashes of the same password are identical — salt not applied")
	}
}

func TestVerifyMalformedHash(t *testing.T) {
	for _, bad := range []string{
		"",
		"not-a-hash",
		"$argon2id$v=19$m=8192,t=1,p=1$onlysalt", // missing hash segment
		"$bcrypt$v=19$m=8192,t=1,p=1$c2FsdA$aGFzaA",   // wrong algorithm
		"$argon2id$v=99$m=8192,t=1,p=1$c2FsdA$aGFzaA", // unsupported version
	} {
		if _, err := VerifyPassword("x", bad); err == nil {
			t.Errorf("expected error for malformed hash %q, got nil", bad)
		}
	}
}
