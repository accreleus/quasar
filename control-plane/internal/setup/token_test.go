package setup

import (
	"encoding/hex"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

// TestMintTokenIs64HexChars pins the documented token format: 32 random bytes,
// hex-encoded to 64 characters. The file is the only place the token exists,
// so its shape is part of the operator contract (copy-paste out of the file).
func TestMintTokenIs64HexChars(t *testing.T) {
	tok, err := MintToken()
	if err != nil {
		t.Fatalf("MintToken: %v", err)
	}
	if len(tok) != tokenBytes*2 {
		t.Fatalf("token length = %d, want %d", len(tok), tokenBytes*2)
	}
	if _, err := hex.DecodeString(tok); err != nil {
		t.Fatalf("token %q is not valid hex: %v", tok, err)
	}

	// Two mints must not collide — the per-boot rotation property depends on it.
	tok2, err := MintToken()
	if err != nil {
		t.Fatalf("second MintToken: %v", err)
	}
	if tok == tok2 {
		t.Fatal("two minted tokens are identical")
	}
}

// TestWriteTokenFileContentAndMode pins the 0600 custody guarantee and the
// trailing-newline content (so `cat` output pastes cleanly).
func TestWriteTokenFileContentAndMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "setup-token")
	const tok = "deadbeef"

	if err := WriteTokenFile(path, tok); err != nil {
		t.Fatalf("WriteTokenFile: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(data) != tok+"\n" {
		t.Fatalf("file content = %q, want %q", data, tok+"\n")
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("file mode = %o, want 0600", perm)
	}
	dirInfo, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatalf("stat dir: %v", err)
	}
	if perm := dirInfo.Mode().Perm(); perm != 0o700 {
		t.Fatalf("dir mode = %o, want 0700", perm)
	}
}

// TestWriteTokenFileRestatesModeOnExistingFile: a pre-existing file with a
// looser mode must be tightened back to 0600 on rewrite.
func TestWriteTokenFileRestatesModeOnExistingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "setup-token")
	if err := os.WriteFile(path, []byte("old\n"), 0o644); err != nil {
		t.Fatalf("seed loose-mode file: %v", err)
	}

	if err := WriteTokenFile(path, "cafef00d"); err != nil {
		t.Fatalf("WriteTokenFile over existing: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("file mode after rewrite = %o, want 0600", perm)
	}
}

// TestRemoveTokenFile: removal succeeds both when the file exists and when it
// is already missing (a missing file is success, not an error path).
func TestRemoveTokenFile(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	path := filepath.Join(t.TempDir(), "setup-token")

	if err := WriteTokenFile(path, "deadbeef"); err != nil {
		t.Fatalf("WriteTokenFile: %v", err)
	}
	RemoveTokenFile(path, log)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("file still present after RemoveTokenFile: %v", err)
	}

	// Second removal of the now-missing file must be a silent no-op.
	RemoveTokenFile(path, log)
}
