package setup

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
)

// tokenBytes is the entropy of the per-boot setup token. 32 bytes = 256 bits,
// hex-encoded (64 chars) so it survives a copy-paste out of the token file.
const tokenBytes = 32

// MintToken returns a fresh per-boot setup token. Never persisted across boots:
// restarting the control plane before the claim rotates it, which is the point —
// an unclaimed token must not outlive the process (spec Risks).
func MintToken() (string, error) {
	b := make([]byte, tokenBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate setup token: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// WriteTokenFile writes the token to path with 0600, creating the directory
// (0700) if needed.
//
// A failure here IS fatal to boot (the caller fails startup): the 0600 file is
// the ONLY place the token exists. It is deliberately never logged — unlike the
// dev-agent key (#399, a dev-only endpoint), this token creates the FIRST ADMIN
// on a production instance, and a token in the WARN stream is readable by every
// log-aggregation principal (monitoring, support, CI) that has no host access.
// Silently degrading to log-only would hand any of them a claimable instance.
func WriteTokenFile(path, token string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create setup token dir %s: %w", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(token+"\n"), 0o600); err != nil {
		return fmt.Errorf("write setup token file %s: %w", path, err)
	}
	// A pre-existing file keeps its old mode through WriteFile, so restate it.
	if err := os.Chmod(path, 0o600); err != nil {
		return fmt.Errorf("set setup token file mode on %s: %w", path, err)
	}
	return nil
}

// RemoveTokenFile deletes a stale setup-token file. Called at boot when an admin
// already exists (env-bootstrap or a prior claim), so a token from an earlier
// unclaimed boot never lingers on disk once setup can no longer be claimed. A
// missing file is success, not an error; any other failure is logged (non-fatal)
// so an operator can see a token file they may still need to scrub.
func RemoveTokenFile(path string, log *slog.Logger) {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		log.Warn("stale setup token file could not be removed", "path", path, "err", err)
	}
}
