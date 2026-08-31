package devauth

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
)

// secretBytes is the entropy of the per-boot dev key. 32 bytes = 256 bits, hex
// encoded (64 chars) so it survives a copy-paste out of a log line.
const secretBytes = 32

// MintSecret returns a fresh per-boot dev key. Never persisted across boots:
// restarting the control plane invalidates every key a tester is holding, which
// is the point.
func MintSecret() (string, error) {
	b := make([]byte, secretBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate dev agent key: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// WriteKeyFile writes the key to path (0600, dir 0700) and reports success.
// Failure is not fatal — a diagnostics convenience must never take down the
// control plane (the PROF-01 posture); the key is in the log too, and the
// failure is logged at Error so it cannot pass unnoticed.
func WriteKeyFile(path, secret string, log *slog.Logger) bool {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		log.Error("dev agent key file unavailable — the key is in the log only",
			"path", path, "err", err)
		return false
	}
	if err := os.WriteFile(path, []byte(secret+"\n"), 0o600); err != nil {
		log.Error("dev agent key file unavailable — the key is in the log only",
			"path", path, "err", err)
		return false
	}
	// A pre-existing file keeps its old mode through WriteFile, so restate it.
	if err := os.Chmod(path, 0o600); err != nil {
		log.Error("dev agent key file permissions could not be set",
			"path", path, "err", err)
		return false
	}
	return true
}
