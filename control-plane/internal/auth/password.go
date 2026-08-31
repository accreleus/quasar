package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

// Params are the argon2id cost parameters. They are stored inside each PHC
// hash string (schema.md: "params live in the string"), so the verifier reads
// them back from the stored hash — changing these defaults never invalidates
// existing hashes.
type Params struct {
	Memory  uint32 // KiB
	Time    uint32 // iterations
	Threads uint8  // parallelism
	SaltLen uint32 // bytes
	KeyLen  uint32 // bytes
}

// DefaultParams follows OWASP's argon2id guidance (64 MiB, t=3), tuned for a
// self-hosted single-node deploy.
func DefaultParams() Params {
	return Params{Memory: 64 * 1024, Time: 3, Threads: 2, SaltLen: 16, KeyLen: 32}
}

// ErrInvalidHash is returned when a stored hash string cannot be parsed.
var ErrInvalidHash = errors.New("invalid argon2id hash format")

// HashPassword returns a PHC-format argon2id string:
//
//	$argon2id$v=19$m=65536,t=3,p=2$<salt>$<hash>
func HashPassword(password string, p Params) (string, error) {
	salt := make([]byte, p.SaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("generate salt: %w", err)
	}
	key := argon2.IDKey([]byte(password), salt, p.Time, p.Memory, p.Threads, p.KeyLen)

	b64 := base64.RawStdEncoding
	return fmt.Sprintf(
		"$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, p.Memory, p.Time, p.Threads,
		b64.EncodeToString(salt), b64.EncodeToString(key),
	), nil
}

// VerifyPassword reports whether password matches the given PHC hash. The cost
// parameters are taken from the hash itself. Comparison is constant-time.
func VerifyPassword(password, phc string) (bool, error) {
	p, salt, key, err := decodeHash(phc)
	if err != nil {
		return false, err
	}
	candidate := argon2.IDKey([]byte(password), salt, p.Time, p.Memory, p.Threads, uint32(len(key)))
	return subtle.ConstantTimeCompare(key, candidate) == 1, nil
}

func decodeHash(phc string) (Params, []byte, []byte, error) {
	parts := strings.Split(phc, "$")
	// ["", "argon2id", "v=19", "m=..,t=..,p=..", "<salt>", "<hash>"]
	if len(parts) != 6 || parts[1] != "argon2id" {
		return Params{}, nil, nil, ErrInvalidHash
	}

	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil {
		return Params{}, nil, nil, ErrInvalidHash
	}
	if version != argon2.Version {
		return Params{}, nil, nil, fmt.Errorf("%w: unsupported version %d", ErrInvalidHash, version)
	}

	var p Params
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &p.Memory, &p.Time, &p.Threads); err != nil {
		return Params{}, nil, nil, ErrInvalidHash
	}

	b64 := base64.RawStdEncoding
	salt, err := b64.DecodeString(parts[4])
	if err != nil {
		return Params{}, nil, nil, ErrInvalidHash
	}
	key, err := b64.DecodeString(parts[5])
	if err != nil {
		return Params{}, nil, nil, ErrInvalidHash
	}
	p.SaltLen = uint32(len(salt))
	p.KeyLen = uint32(len(key))
	return p, salt, key, nil
}
