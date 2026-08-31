// Package secrets is the control plane's encrypted-secret facility: a named,
// versioned, AEAD-encrypted store for operator credentials (API keys,
// passwords, tokens) that must live in Postgres but must not be readable from a
// database dump.
//
// Generic by design — SteamGridDB's key is consumer #1, not the design centre:
// a new credential is one Descriptor registration plus a point-of-use
// store.Resolve, and gets the admin API, UI row, env fallback and masking free.
//
// The three rules this package enforces:
//  1. A plaintext is produced only at the point of use; Get is the only method
//     that yields one — Status, List and every HTTP response carry a masked
//     hint at most.
//  2. A plaintext is never logged and never put in an error or test-failure
//     message; errors name the secret, never its value.
//  3. "No master key" and "wrong master key" are different answers — the
//     operator's next action differs completely.
package secrets

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// MasterKeyLen is the required master-key length: AES-256 wants exactly 32 bytes.
const MasterKeyLen = 32

// Sentinel errors. Callers distinguish these; the HTTP layer maps them to
// distinct status codes and distinct operator-facing text.
var (
	// ErrNoMasterKey means QUASAR_SECRET_KEY is unset, so the facility is
	// UNAVAILABLE — not broken. Secret-backed features report themselves
	// unavailable and everything else keeps working. Deliberately NOT the same
	// as ErrKeyMismatch.
	ErrNoMasterKey = errors.New("secrets: no master key is configured (QUASAR_SECRET_KEY is unset)")

	// ErrKeyMismatch means a secret IS stored but the configured master key
	// cannot decrypt it — the key changed, was truncated, or belongs to another
	// deployment. This is loud and specific on purpose: the operator's fix is to
	// restore the original key or re-enter the secret, and neither is discoverable
	// from a "not configured" message.
	ErrKeyMismatch = errors.New("secrets: the master key does not match the stored secret (restore the original QUASAR_SECRET_KEY, or set the secret again)")

	// ErrCiphertextInvalid means the stored row is structurally malformed —
	// detectable without the key, so no restored QUASAR_SECRET_KEY fixes it; the
	// operator must set the secret again. It unwraps to ErrKeyMismatch so
	// existing errors.Is callers keep working; check this one first for the
	// distinction.
	ErrCiphertextInvalid error = ciphertextInvalidError{}

	// ErrNotFound means no secret is stored under that name.
	ErrNotFound = errors.New("secrets: no secret is stored under that name")

	// ErrUnknownSecret means the name is not one this build declares. Keeps the
	// admin API from becoming an arbitrary key/value store.
	ErrUnknownSecret = errors.New("secrets: unknown secret name")

	// ErrEmptyValue rejects storing an empty secret: "" is indistinguishable from
	// unset at every later read, so it would be a silent way to break a feature.
	ErrEmptyValue = errors.New("secrets: value must not be empty (delete the secret instead)")
)

// ciphertextInvalidError exists so the sentinel can unwrap to ErrKeyMismatch —
// a plain errors.New value cannot unwrap to another sentinel.
type ciphertextInvalidError struct{}

func (ciphertextInvalidError) Error() string {
	return "secrets: the stored secret is malformed and cannot be decrypted under any master key (set the secret again)"
}

func (ciphertextInvalidError) Unwrap() error { return ErrKeyMismatch }

// Keyring holds the master key(s) this process can decrypt with, keyed by
// version, plus the version new writes use. Rotation is not implemented but
// not designed out: give the new key a higher version, keep the old one in
// QUASAR_SECRET_KEY_PREVIOUS, and rows keep decrypting — a re-encrypt sweep
// needs no schema or wire change. A nil *Keyring is legal: "no master key
// configured"; every operation returns ErrNoMasterKey.
type Keyring struct {
	// primary is the version used for new encryptions.
	primary int
	aeads   map[int]cipher.AEAD
}

// ParseKeyring builds a Keyring from the raw env values.
//
// primary accepts either a bare base64 32-byte key (implicitly version 1) or an
// explicit "<version>:<base64>" form. previous is a comma-separated list of
// decrypt-only keys in the same forms. An empty primary yields a nil Keyring
// (the documented "unset" state), NOT an error and NOT a generated key:
// generating one and persisting it would silently diverge across a multi-node
// deployment and make a database backup unrestorable without the node that
// happened to invent it.
func ParseKeyring(primary, previous string) (*Keyring, error) {
	primary = strings.TrimSpace(primary)
	if primary == "" {
		if strings.TrimSpace(previous) != "" {
			return nil, fmt.Errorf("QUASAR_SECRET_KEY_PREVIOUS is set but QUASAR_SECRET_KEY is not: a decrypt-only key alone cannot store new secrets")
		}
		return nil, nil
	}
	pv, pkey, err := parseKeyEntry(primary, "QUASAR_SECRET_KEY")
	if err != nil {
		return nil, err
	}
	kr := &Keyring{primary: pv, aeads: map[int]cipher.AEAD{}}
	// Clear the decoded key once aes.NewCipher has copied it — the Store's rule
	// for plaintexts. Defence in depth: the base64 form still lives in Config.
	kr.aeads[pv], err = newAEAD(pkey)
	clear(pkey)
	if err != nil {
		return nil, fmt.Errorf("QUASAR_SECRET_KEY: %w", err)
	}
	for _, ent := range strings.Split(previous, ",") {
		if strings.TrimSpace(ent) == "" {
			continue
		}
		v, key, err := parseKeyEntry(ent, "QUASAR_SECRET_KEY_PREVIOUS")
		if err != nil {
			return nil, err
		}
		if _, dup := kr.aeads[v]; dup {
			clear(key)
			return nil, fmt.Errorf("QUASAR_SECRET_KEY_PREVIOUS: key version %d is already defined", v)
		}
		kr.aeads[v], err = newAEAD(key)
		clear(key)
		if err != nil {
			return nil, fmt.Errorf("QUASAR_SECRET_KEY_PREVIOUS: %w", err)
		}
	}
	return kr, nil
}

// parseKeyEntry decodes "<version>:<base64>" or a bare "<base64>" (version 1).
// The error text names the env var and the defect, never any part of the
// entry: echoing the pre-':' prefix would log a piece of a mis-pasted key
// containing a colon.
func parseKeyEntry(entry, envName string) (version int, key []byte, err error) {
	entry = strings.TrimSpace(entry)
	version = 1
	if i := strings.IndexByte(entry, ':'); i >= 0 {
		v, convErr := strconv.Atoi(strings.TrimSpace(entry[:i]))
		if convErr != nil || v < 1 {
			return 0, nil, fmt.Errorf("%s: the key version prefix before ':' must be a positive integer", envName)
		}
		version, entry = v, strings.TrimSpace(entry[i+1:])
	}
	// Accept both standard and URL-safe base64, with or without padding: an
	// operator pasting from `openssl rand -base64 32` and one pasting from a
	// secret manager should both work rather than hitting a cryptic length error.
	key, err = decodeBase64(entry)
	if err != nil {
		return 0, nil, fmt.Errorf("%s: value must be base64 (generate one with: openssl rand -base64 32)", envName)
	}
	if len(key) != MasterKeyLen {
		return 0, nil, fmt.Errorf("%s: decoded key is %d bytes, must be exactly %d (generate one with: openssl rand -base64 32)",
			envName, len(key), MasterKeyLen)
	}
	return version, key, nil
}

func decodeBase64(s string) ([]byte, error) {
	for _, enc := range []*base64.Encoding{
		base64.StdEncoding, base64.RawStdEncoding,
		base64.URLEncoding, base64.RawURLEncoding,
	} {
		if b, err := enc.DecodeString(s); err == nil {
			return b, nil
		}
	}
	return nil, errors.New("not base64")
}

// newAEAD builds AES-256-GCM. Standard-library construction only — no
// hand-rolled KDF, padding or mode. The master key is already 32 uniformly
// random bytes, so there is nothing for a KDF to do.
func newAEAD(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("aes: %w", err)
	}
	return cipher.NewGCM(block)
}

// Available reports whether a master key is configured. The one check a
// secret-backed feature makes before offering itself.
func (k *Keyring) Available() bool { return k != nil && len(k.aeads) > 0 }

// PrimaryVersion is the key version new writes are encrypted under.
func (k *Keyring) PrimaryVersion() int {
	if !k.Available() {
		return 0
	}
	return k.primary
}

// Versions lists every version this process can decrypt, ascending. Used by the
// admin surface to explain a mismatch ("this row was written under key version
// 1; this control plane holds 2").
func (k *Keyring) Versions() []int {
	if !k.Available() {
		return nil
	}
	out := make([]int, 0, len(k.aeads))
	for v := range k.aeads {
		out = append(out, v)
	}
	sort.Ints(out)
	return out
}

// aeadFor returns the AEAD for a version, or ErrKeyMismatch when this process
// holds no key that could have written the row. A row written under a version
// we do not hold is a KEY problem, not a corruption problem, and the operator
// needs to be told which.
func (k *Keyring) aeadFor(version int) (cipher.AEAD, error) {
	if !k.Available() {
		return nil, ErrNoMasterKey
	}
	a, ok := k.aeads[version]
	if !ok {
		return nil, fmt.Errorf("%w: the stored value was written under key version %d, which this control plane does not hold", ErrKeyMismatch, version)
	}
	return a, nil
}
