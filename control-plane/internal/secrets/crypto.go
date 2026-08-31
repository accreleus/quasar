package secrets

import (
	"crypto/rand"
	"fmt"
)

// aadPrefix domain-separates this package's ciphertexts. Bound in as additional
// authenticated data together with the secret's name, so a ciphertext lifted
// from one row and pasted into another FAILS to decrypt instead of silently
// becoming that other secret. Without this, an attacker with UPDATE on the
// table could swap a low-value credential onto a high-value name and have the
// server use it.
//
// The prefix is versioned separately from key_version: key_version is "which
// master key", this is "which binding scheme". Changing it would invalidate
// every existing ciphertext, so it changes only alongside a re-encrypt path.
const aadPrefix = "quasar/instance_secrets/v1|"

func aad(name string) []byte { return []byte(aadPrefix + name) }

// seal encrypts plaintext for the named secret under the keyring's primary key.
//
// The nonce is 12 fresh random bytes from crypto/rand for EVERY call — never a
// counter, never derived from the name, never reused. GCM nonce reuse under the
// same key is catastrophic (it leaks the XOR of the plaintexts and the
// authentication subkey), and the only way to be sure it never happens here is
// to never keep any state that could be replayed.
func seal(kr *Keyring, name, plaintext string) (ciphertext, nonce []byte, version int, err error) {
	aead, err := kr.aeadFor(kr.PrimaryVersion())
	if err != nil {
		return nil, nil, 0, err
	}
	nonce = make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		// Never include plaintext in this (or any) error.
		return nil, nil, 0, fmt.Errorf("secrets: could not generate a nonce for %q: %w", name, err)
	}
	ciphertext = aead.Seal(nil, nonce, []byte(plaintext), aad(name))
	return ciphertext, nonce, kr.PrimaryVersion(), nil
}

// open decrypts a stored row. Failures are separated because the operator's
// next action differs: no key of that version (Keyring.aeadFor — a
// key-management problem), a structurally impossible row (ErrCiphertextInvalid
// — no key can fix it, set the secret again), or authentication failure on a
// well-formed row (ErrKeyMismatch — GCM cannot tell wrong key from tampered
// bytes from wrong AAD, and that indistinguishability is the security
// property, so the message names all three). Returns []byte, not string, so
// callers can zero it after the copy — and every caller does.
func open(kr *Keyring, name string, ciphertext, nonce []byte, version int) ([]byte, error) {
	aead, err := kr.aeadFor(version)
	if err != nil {
		return nil, err
	}
	if len(nonce) != aead.NonceSize() {
		return nil, fmt.Errorf("%w: the stored nonce for %q is %d bytes, expected %d",
			ErrCiphertextInvalid, name, len(nonce), aead.NonceSize())
	}
	if len(ciphertext) < aead.Overhead() {
		return nil, fmt.Errorf("%w: the stored ciphertext for %q is %d bytes, too short to carry a %d-byte authentication tag",
			ErrCiphertextInvalid, name, len(ciphertext), aead.Overhead())
	}
	out, err := aead.Open(nil, nonce, ciphertext, aad(name))
	if err != nil {
		return nil, fmt.Errorf("%w: secret %q did not authenticate — the master key does not match it, or the stored row was modified or copied from another secret", ErrKeyMismatch, name)
	}
	return out, nil
}

// hintTail is how much of a secret a hint reveals; hintFloor is the shortest
// secret that gets one. Four characters distinguishes "the key I just pasted"
// from "the one already there" and is useless for guessing; the floor keeps
// the revealed tail to at most a third of the value — the hint column is
// readable without the master key, and a short shared password is exactly the
// case where more is too much. Below the floor: no hint, the UI just says one
// is configured.
const (
	hintTail  = 4
	hintFloor = 3 * hintTail
)

// Hint is the masked identifier shown to an operator: the last hintTail
// characters, and nothing at all for a secret shorter than hintFloor.
//
// Runes, not bytes, so a multi-byte value cannot be sliced mid-character.
func Hint(plaintext string) string {
	r := []rune(plaintext)
	if len(r) < hintFloor {
		return ""
	}
	return string(r[len(r)-hintTail:])
}
