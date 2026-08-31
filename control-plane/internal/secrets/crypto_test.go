package secrets

// Crypto-level tests. No database and no HTTP: these pin the construction
// itself, so a regression in nonce handling or AAD binding fails here rather
// than somewhere downstream where it would look like a storage bug.
//
// NO TEST IN THIS FILE PRINTS A PLAINTEXT. Comparisons use
// crypto/subtle.ConstantTimeCompare and failures report a boolean, never the
// value — a test failure message is a log line like any other.

import (
	"bytes"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
)

// testKey is a fixed 32-byte key. Test-only material; it protects nothing.
const testKeyB64 = "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8="

// altKeyB64 is a DIFFERENT fixed 32-byte key, used to prove the wrong-key path.
const altKeyB64 = "f39/f39/f39/f39/f39/f39/f39/f39/f39/f39/f38="

func testKeyring(t *testing.T) *Keyring {
	t.Helper()
	kr, err := ParseKeyring(testKeyB64, "")
	if err != nil {
		t.Fatalf("ParseKeyring: %v", err)
	}
	return kr
}

// sameSecret compares in constant time and returns only a boolean, so no
// failure message can ever carry the value.
func sameSecret(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

func TestParseKeyringUnsetIsNotAnError(t *testing.T) {
	// The load-bearing property: an unconfigured deployment BOOTS. A nil keyring
	// is a legal value, not a failure, and every operation on it is a clean
	// ErrNoMasterKey rather than a panic.
	kr, err := ParseKeyring("", "")
	if err != nil {
		t.Fatalf("an unset master key must not be an error, got %v", err)
	}
	if kr.Available() {
		t.Fatal("a nil keyring must report unavailable")
	}
	if kr.PrimaryVersion() != 0 || kr.Versions() != nil {
		t.Fatal("a nil keyring must report no versions")
	}
	if _, _, _, err := seal(kr, "n", "v"); !errors.Is(err, ErrNoMasterKey) {
		t.Fatalf("seal with no key: want ErrNoMasterKey, got %v", err)
	}
	if _, err := open(kr, "n", []byte("x"), []byte("y"), 1); !errors.Is(err, ErrNoMasterKey) {
		t.Fatalf("open with no key: want ErrNoMasterKey, got %v", err)
	}
}

func TestParseKeyringRejectsMalformedKeys(t *testing.T) {
	// A truncated or non-base64 key must fail LOUDLY at parse time. Accepting it
	// and failing later would mean an operator learns their key was wrong when a
	// secret they already saved turns out to be unreadable.
	short := base64.StdEncoding.EncodeToString(make([]byte, 16))
	for _, tc := range []struct{ name, primary, previous string }{
		{"not base64", "this is not base64 !!", ""},
		{"wrong length", short, ""},
		{"bad version prefix", "zero:" + testKeyB64, ""},
		{"previous without primary", "", "1:" + testKeyB64},
		{"duplicate version", "1:" + testKeyB64, "1:" + altKeyB64},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ParseKeyring(tc.primary, tc.previous); err == nil {
				t.Fatal("want an error, got nil")
			}
		})
	}
}

func TestParseKeyringAcceptsVersionedAndUnpaddedForms(t *testing.T) {
	raw := strings.TrimRight(testKeyB64, "=")
	for _, tc := range []struct {
		name    string
		primary string
		want    int
	}{
		{"bare is version 1", testKeyB64, 1},
		{"unpadded", raw, 1},
		{"explicit version", "7:" + testKeyB64, 7},
	} {
		t.Run(tc.name, func(t *testing.T) {
			kr, err := ParseKeyring(tc.primary, "")
			if err != nil {
				t.Fatalf("ParseKeyring: %v", err)
			}
			if got := kr.PrimaryVersion(); got != tc.want {
				t.Fatalf("PrimaryVersion = %d, want %d", got, tc.want)
			}
		})
	}
}

// TestSealUsesAFreshNonceEveryTime is the nonce-reuse guard. GCM nonce reuse
// under one key is catastrophic, so this asserts the observable consequence:
// encrypting the SAME plaintext twice must produce different nonces AND
// different ciphertexts.
func TestSealUsesAFreshNonceEveryTime(t *testing.T) {
	kr := testKeyring(t)
	const name = "test.secret"
	const plaintext = "the-same-value-twice"

	ct1, n1, _, err := seal(kr, name, plaintext)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	ct2, n2, _, err := seal(kr, name, plaintext)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	if bytes.Equal(n1, n2) {
		t.Fatal("two encryptions reused the same nonce")
	}
	if bytes.Equal(ct1, ct2) {
		t.Fatal("two encryptions of the same plaintext produced identical ciphertext")
	}
	// And both still decrypt.
	for i, pair := range [][2][]byte{{ct1, n1}, {ct2, n2}} {
		got, err := open(kr, name, pair[0], pair[1], kr.PrimaryVersion())
		if err != nil {
			t.Fatalf("open #%d: %v", i, err)
		}
		if !sameSecret(string(got), plaintext) {
			t.Fatalf("round trip #%d did not recover the value", i)
		}
	}
}

// TestCiphertextIsBoundToItsName is the AAD test: a ciphertext lifted from one
// row and pasted onto another must FAIL, not silently become that other secret.
func TestCiphertextIsBoundToItsName(t *testing.T) {
	kr := testKeyring(t)
	ct, nonce, version, err := seal(kr, "artwork.steamgriddb.api_key", "a-credential-value")
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	// Same key, same nonce, same bytes — only the name differs.
	_, err = open(kr, "smtp.password", ct, nonce, version)
	if !errors.Is(err, ErrKeyMismatch) {
		t.Fatalf("a ciphertext moved to another name must not decrypt, got err=%v", err)
	}
	// And it is reported as the AUTHENTICATION failure it is, not as a malformed
	// row: the bytes are perfectly well-formed, the AAD binding is what rejected
	// them, and "set the secret again" is not the right advice.
	if errors.Is(err, ErrCiphertextInvalid) {
		t.Fatalf("a well-formed relocated row must not be reported as malformed, got %v", err)
	}
	// The operator whose master key is FINE has to be told a copied row produces
	// this same error, or the message sends them hunting a key problem they do
	// not have.
	if !strings.Contains(err.Error(), "copied from another secret") {
		t.Fatalf("the error must name the copied-row cause, got %q", err.Error())
	}
	// Sanity: it decrypts under its own name.
	if _, err := open(kr, "artwork.steamgriddb.api_key", ct, nonce, version); err != nil {
		t.Fatalf("open under the correct name: %v", err)
	}
}

// TestMalformedRowIsNotReportedAsAKeyProblem is the other half of the same
// distinction: a nonce of the wrong length or a ciphertext too short to carry a
// GCM tag is detectable WITHOUT any key, so no restored QUASAR_SECRET_KEY can
// fix it. It still satisfies errors.Is(err, ErrKeyMismatch), because that
// sentinel is the umbrella every existing caller matches on.
func TestMalformedRowIsNotReportedAsAKeyProblem(t *testing.T) {
	kr := testKeyring(t)
	ct, nonce, version, err := seal(kr, "test.secret", "a-stored-value")
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	for _, tc := range []struct {
		name       string
		ciphertext []byte
		nonce      []byte
	}{
		{"nonce truncated", ct, nonce[:len(nonce)-1]},
		{"nonce padded", ct, append(append([]byte{}, nonce...), 0)},
		{"ciphertext shorter than the GCM tag", ct[:4], nonce},
		{"ciphertext empty", []byte{}, nonce},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := open(kr, "test.secret", tc.ciphertext, tc.nonce, version)
			if !errors.Is(err, ErrCiphertextInvalid) {
				t.Fatalf("want ErrCiphertextInvalid, got %v", err)
			}
			// Existing callers match the umbrella and must keep working.
			if !errors.Is(err, ErrKeyMismatch) {
				t.Fatalf("ErrCiphertextInvalid must still satisfy errors.Is(ErrKeyMismatch), got %v", err)
			}
			if errors.Is(err, ErrNoMasterKey) {
				t.Fatal("a malformed row must not read as 'no key configured'")
			}
		})
	}
}

// TestWrongMasterKeyIsDistinguishable pins the difference the whole facility
// hangs on: a wrong key must not look like "no key configured".
func TestWrongMasterKeyIsDistinguishable(t *testing.T) {
	good := testKeyring(t)
	bad, err := ParseKeyring(altKeyB64, "")
	if err != nil {
		t.Fatalf("ParseKeyring: %v", err)
	}
	ct, nonce, version, err := seal(good, "test.secret", "some-stored-value")
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	_, err = open(bad, "test.secret", ct, nonce, version)
	if !errors.Is(err, ErrKeyMismatch) {
		t.Fatalf("want ErrKeyMismatch, got %v", err)
	}
	if errors.Is(err, ErrNoMasterKey) {
		t.Fatal("a wrong key must NOT be reported as no key configured")
	}
	// Nor as a malformed row: the bytes are fine, the key is not.
	if errors.Is(err, ErrCiphertextInvalid) {
		t.Fatalf("a wrong key must NOT be reported as a malformed row, got %v", err)
	}
	// The message has to name the master key, or an operator has no lead.
	if !strings.Contains(err.Error(), "master key") {
		t.Fatalf("error must name the master key, got %q", err.Error())
	}
}

// TestUnknownKeyVersionIsAKeyProblem: a row written under a version this
// process does not hold is a key-management problem and must say so.
func TestUnknownKeyVersionIsAKeyProblem(t *testing.T) {
	kr := testKeyring(t) // holds version 1 only
	_, err := open(kr, "test.secret", []byte("irrelevant"), make([]byte, 12), 9)
	if !errors.Is(err, ErrKeyMismatch) {
		t.Fatalf("want ErrKeyMismatch for an unheld key version, got %v", err)
	}
}

// TestPreviousKeyDecryptsRotatedRows: rotation is not implemented, but the
// mechanism that makes it possible must work — a decrypt-only predecessor keeps
// old rows readable while new writes use the new key.
func TestPreviousKeyDecryptsRotatedRows(t *testing.T) {
	oldKr, err := ParseKeyring("1:"+testKeyB64, "")
	if err != nil {
		t.Fatalf("ParseKeyring: %v", err)
	}
	ct, nonce, version, err := seal(oldKr, "test.secret", "written-under-key-1")
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	if version != 1 {
		t.Fatalf("version = %d, want 1", version)
	}

	rotated, err := ParseKeyring("2:"+altKeyB64, "1:"+testKeyB64)
	if err != nil {
		t.Fatalf("ParseKeyring: %v", err)
	}
	if rotated.PrimaryVersion() != 2 {
		t.Fatalf("new writes must use version 2, got %d", rotated.PrimaryVersion())
	}
	got, err := open(rotated, "test.secret", ct, nonce, version)
	if err != nil {
		t.Fatalf("a version-1 row must still decrypt after rotation: %v", err)
	}
	if !sameSecret(string(got), "written-under-key-1") {
		t.Fatal("rotated read did not recover the original value")
	}
}

func TestHintMasksAndRefusesToLeakShortValues(t *testing.T) {
	for _, tc := range []struct {
		name, in, want string
	}{
		{"short values get no hint at all", "abc", ""},
		{"7 chars is still too short", "abcdefg", ""},
		// The old floor. An 8-character secret used to get "efgh" — half of it,
		// in cleartext, in a column readable without the master key.
		{"8 chars gets nothing now", "abcdefgh", ""},
		{"one below the floor", "abcdefghijk", ""},
		{"the floor itself reveals exactly a third", "abcdefghijkl", "ijkl"},
		{"long api key", "sgdb_live_0123456789abcdef", "cdef"},
		{"multi-byte is sliced by rune", "ключключключ", "ключ"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := Hint(tc.in); got != tc.want {
				t.Fatalf("Hint() = %q, want %q", got, tc.want)
			}
		})
	}
	// The masking rule itself: never more than hintTail characters, whatever the
	// input, and never more than a third of the value.
	if got := Hint(strings.Repeat("x", 512)); len([]rune(got)) != hintTail {
		t.Fatalf("Hint must be exactly %d characters for a long value, got %d", hintTail, len([]rune(got)))
	}
	for n := 1; n < 64; n++ {
		in := strings.Repeat("x", n)
		if got := len([]rune(Hint(in))); got*3 > n {
			t.Fatalf("Hint revealed %d of %d characters, more than a third", got, n)
		}
	}
}
