package secrets

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Origin names where the value in effect came from. Surfaced to the admin UI so
// an operator is never guessing which of two sources the server is actually
// using.
const (
	OriginNone        = "none"        // nothing configured anywhere
	OriginDatabase    = "database"    // an admin set it through the UI/API
	OriginEnvironment = "environment" // the legacy/deployment env var
)

// Status is everything an admin surface may know about a secret. It carries NO
// plaintext, by construction: Hint is at most the last four characters and is
// empty for a short value.
type Status struct {
	Name string `json:"name"`
	// Configured is true when a row exists, regardless of whether this control
	// plane can decrypt it. "Something is stored" and "we can read it" are
	// different facts and both matter.
	Configured bool `json:"configured"`
	// Readable is false when a row exists but the master key is missing or wrong.
	// This is what turns a silent feature outage into a diagnosable one.
	Readable bool `json:"readable"`
	// Hint is the masked tail of the stored value ("" when none is stored, or
	// when the value is too short to mask safely).
	Hint string `json:"hint"`
	// KeyVersion is the master-key version the row was written under (0 when no
	// row exists).
	KeyVersion int        `json:"key_version"`
	UpdatedBy  *string    `json:"updated_by"`
	UpdatedAt  *time.Time `json:"updated_at"`
	// Problem is a short operator-facing explanation when Readable is false, and
	// "" otherwise. Never contains any part of the value.
	Problem string `json:"problem,omitempty"`
}

// Value is the outcome of resolving a secret at the point of use.
//
// Secret carries `json:"-"` as belt-and-braces: this type is Go-internal and
// nothing serializes it today, but a future handler that accidentally wrote one
// into a response would be the one bug that matters here, and the tag makes
// that impossible rather than merely unlikely.
type Value struct {
	// Secret is the plaintext. Use it and drop it — do not stash it in a struct
	// that outlives the call, and never log it.
	Secret string `json:"-"`
	// Origin is OriginDatabase, OriginEnvironment or OriginNone.
	Origin string `json:"origin"`
}

// Configured reports whether any value is in effect.
func (v Value) Configured() bool { return v.Secret != "" }

// Store is the data-access layer for instance_secrets. A Store with a nil
// Keyring is fully constructed and legal: reads report "not available" and
// writes return ErrNoMasterKey, so the control plane boots and every other
// feature is unaffected.
type Store struct {
	db      *pgxpool.Pool
	keyring *Keyring
	reg     *Registry
}

// NewStore builds the store. keyring may be nil (no master key configured).
func NewStore(db *pgxpool.Pool, keyring *Keyring, reg *Registry) *Store {
	if reg == nil {
		reg = NewRegistry()
	}
	return &Store{db: db, keyring: keyring, reg: reg}
}

// Available reports whether a master key is configured, i.e. whether secrets
// can be stored or read at all. A secret-backed feature checks this to say
// "unavailable on this deployment" instead of failing obscurely later.
func (s *Store) Available() bool { return s != nil && s.keyring.Available() }

// BootWarning returns the operator-facing line to log at boot when no master
// key is configured, "" otherwise. Loud on purpose (#522): a one-line INFO is
// invisible and the facility could stay degraded for months. It names every
// declared secret by label and states the fix and the key-change consequences
// in the same line.
func (s *Store) BootWarning() string {
	if s.Available() {
		return ""
	}
	descs := s.reg.All()
	labels := make([]string, 0, len(descs))
	for _, d := range descs {
		labels = append(labels, d.Label)
	}
	disabled := "no admin-stored credential is declared in this build"
	if len(labels) > 0 {
		disabled = strings.Join(labels, ", ")
	}
	return "QUASAR_SECRET_KEY is not set: the encrypted-secrets store is UNAVAILABLE. " +
		"Disabled until it is set: " + disabled + ". Each falls back to its own " +
		"environment variable where one exists (docs/configuration.md), and otherwise " +
		"stays off — nothing else on this deployment is affected. To enable: generate a " +
		"key with `openssl rand -base64 32`, set it as QUASAR_SECRET_KEY in deploy/.env, " +
		"and restart. Back the key up with your deployment secrets: it is never stored " +
		"in the database. If you set a DIFFERENT key later without keeping the old one in " +
		"QUASAR_SECRET_KEY_PREVIOUS, anything already stored under the old key becomes " +
		"unreadable — reported specifically as a key mismatch, never silently — until you " +
		"restore the original key or re-enter the value."
}

// Registry exposes the declared secrets (the admin surface enumerates these).
func (s *Store) Registry() *Registry { return s.reg }

// KeyVersions lists the master-key versions this process can decrypt with.
func (s *Store) KeyVersions() []int { return s.keyring.Versions() }

// Set stores (or replaces) a secret. updatedBy is the acting admin's user id,
// or "" for a non-interactive caller.
//
// Encryption happens HERE, before the value reaches the database driver — the
// plaintext never appears in a query argument, so it cannot land in a
// statement log, a pg_stat_statements row or a query trace.
func (s *Store) Set(ctx context.Context, name, plaintext, updatedBy string) error {
	if _, ok := s.reg.Lookup(name); !ok {
		return fmt.Errorf("%w: %q", ErrUnknownSecret, name)
	}
	if plaintext == "" {
		return ErrEmptyValue
	}
	if !s.keyring.Available() {
		return ErrNoMasterKey
	}
	ct, nonce, version, err := seal(s.keyring, name, plaintext)
	if err != nil {
		return err
	}
	var by any
	if updatedBy != "" {
		by = updatedBy
	}
	_, err = s.db.Exec(ctx, `
		INSERT INTO instance_secrets (name, ciphertext, nonce, key_version, hint, updated_by, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6::uuid, now())
		ON CONFLICT (name) DO UPDATE
		    SET ciphertext  = EXCLUDED.ciphertext,
		        nonce       = EXCLUDED.nonce,
		        key_version = EXCLUDED.key_version,
		        hint        = EXCLUDED.hint,
		        updated_by  = EXCLUDED.updated_by
	`, name, ct, nonce, version, Hint(plaintext), by)
	if err != nil {
		// %w on the driver error is safe: the plaintext was never an argument.
		return fmt.Errorf("secrets: store %q: %w", name, err)
	}
	return nil
}

// Get returns the decrypted secret. THE ONLY method that yields a plaintext.
//
// Call it at the point of use and let the result go out of scope; do not cache
// it. Returns ErrNotFound when nothing is stored, ErrNoMasterKey when the
// facility is unavailable, and ErrKeyMismatch when a row exists but this
// control plane's key cannot open it.
func (s *Store) Get(ctx context.Context, name string) (string, error) {
	row, err := s.load(ctx, name)
	if err != nil {
		return "", err
	}
	if !s.keyring.Available() {
		return "", ErrNoMasterKey
	}
	plain, err := open(s.keyring, name, row.ciphertext, row.nonce, row.keyVersion)
	if err != nil {
		return "", err
	}
	// The returned string is a copy; clear the decrypt buffer rather than leave
	// a credential lying around for the GC. Defence in depth, not a guarantee.
	defer clear(plain)
	return string(plain), nil
}

// Delete removes a secret. Deleting one that is not there is not an error: the
// caller asked for "no secret stored under this name" and that is the outcome.
func (s *Store) Delete(ctx context.Context, name string) error {
	if _, ok := s.reg.Lookup(name); !ok {
		return fmt.Errorf("%w: %q", ErrUnknownSecret, name)
	}
	if _, err := s.db.Exec(ctx, `DELETE FROM instance_secrets WHERE name = $1`, name); err != nil {
		return fmt.Errorf("secrets: delete %q: %w", name, err)
	}
	return nil
}

// Status reports whether a secret is configured and readable, with a masked
// hint. NEVER returns the value.
//
// It reads the hint from the row rather than by decrypting, so it still answers
// usefully when the master key is missing or wrong — which is exactly when an
// operator needs the answer. Readability is then probed by an actual decrypt,
// whose plaintext is zeroed immediately and never leaves this function.
func (s *Store) Status(ctx context.Context, name string) (Status, error) {
	if _, ok := s.reg.Lookup(name); !ok {
		return Status{}, fmt.Errorf("%w: %q", ErrUnknownSecret, name)
	}
	st := Status{Name: name}
	row, err := s.load(ctx, name)
	if errors.Is(err, ErrNotFound) {
		return st, nil
	}
	if err != nil {
		return Status{}, err
	}
	st.Configured = true
	st.Hint = row.hint
	st.KeyVersion = row.keyVersion
	st.UpdatedBy = row.updatedBy
	st.UpdatedAt = &row.updatedAt

	if !s.keyring.Available() {
		st.Problem = "No master key is configured on this control plane (QUASAR_SECRET_KEY), so the stored value cannot be decrypted."
		return st, nil
	}
	plain, err := open(s.keyring, name, row.ciphertext, row.nonce, row.keyVersion)
	if err != nil {
		st.Problem = problemFor(err)
		return st, nil
	}
	clear(plain) // the probe is over; do not keep the bytes around
	st.Readable = true
	return st, nil
}

// problemFor is the operator-facing sentence for a failed decrypt, separating
// the same cases open() does — "master key does not match" is wrong advice for
// a malformed row. Fixed strings; never any part of a value.
func problemFor(err error) string {
	switch {
	case errors.Is(err, ErrCiphertextInvalid):
		return "The stored value is malformed and cannot be decrypted under any master key. Set this secret again."
	case errors.Is(err, ErrNoMasterKey):
		return "No master key is configured on this control plane (QUASAR_SECRET_KEY), so the stored value cannot be decrypted."
	default:
		return "The stored value did not decrypt: either the configured master key does not match it, or the stored row was modified or copied from another secret. Restore the original QUASAR_SECRET_KEY, or set this secret again."
	}
}

// Resolve is the point-of-use read: the stored secret when readable, else
// envFallback. The database wins and the environment is the fallback — a UI
// control that silently does nothing because a stale env var outranks it is
// the worst outcome, and env-as-fallback means upgrades and cleared secrets
// degrade gracefully. Value.Origin reports which source won. A
// stored-but-unreadable secret does not fall through to the env: silently
// using a different credential than the admin configured is the surprise this
// facility exists to prevent — it returns ErrKeyMismatch instead.
func (s *Store) Resolve(ctx context.Context, name, envFallback string) (Value, error) {
	if _, ok := s.reg.Lookup(name); !ok {
		return Value{Origin: OriginNone}, fmt.Errorf("%w: %q", ErrUnknownSecret, name)
	}
	row, err := s.load(ctx, name)
	switch {
	case errors.Is(err, ErrNotFound):
		if envFallback != "" {
			return Value{Secret: envFallback, Origin: OriginEnvironment}, nil
		}
		return Value{Origin: OriginNone}, nil
	case err != nil:
		return Value{Origin: OriginNone}, err
	}
	if !s.keyring.Available() {
		return Value{Origin: OriginNone}, ErrNoMasterKey
	}
	plain, err := open(s.keyring, name, row.ciphertext, row.nonce, row.keyVersion)
	if err != nil {
		return Value{Origin: OriginNone}, err
	}
	defer clear(plain) // as in Get: the Value carries a copy
	return Value{Secret: string(plain), Origin: OriginDatabase}, nil
}

// storedRow is one instance_secrets row. Unexported and never returned.
type storedRow struct {
	ciphertext []byte
	nonce      []byte
	keyVersion int
	hint       string
	updatedBy  *string
	updatedAt  time.Time
}

func (s *Store) load(ctx context.Context, name string) (storedRow, error) {
	var row storedRow
	err := s.db.QueryRow(ctx, `
		SELECT ciphertext, nonce, key_version, hint, updated_by::text, updated_at
		FROM instance_secrets WHERE name = $1
	`, name).Scan(&row.ciphertext, &row.nonce, &row.keyVersion, &row.hint, &row.updatedBy, &row.updatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return storedRow{}, ErrNotFound
	}
	if err != nil {
		return storedRow{}, fmt.Errorf("secrets: read %q: %w", name, err)
	}
	return row, nil
}
