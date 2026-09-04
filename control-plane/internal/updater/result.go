package updater

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// One result file per request id, in the shared volume, written atomically
// (tmp + rename) — prototype finding 2. The reader is a DIFFERENT container
// than the writer and is normally being replaced while the write happens, so a
// half-written file must be unrepresentable: rename is atomic within a
// filesystem, so a reader sees either the previous complete result or the next
// one, never a prefix.
//
// The field spellings are agent-api.md `release_state`'s, so the agent's relay
// is a re-frame and not a translation. That is a convenience, NOT a contract:
// schema.md declares this file explicitly un-frozen.

// OutputTailBytes bounds the failing step's captured output in a result. The
// wire caps `output` at 8192 bytes and truncates from the FRONT at a line
// boundary — the error is at the end — so the file carries what the wire can.
const OutputTailBytes = 8192

// Result is one apply's whole observable state.
type Result struct {
	RequestID  string              `json:"request_id"`
	State      string              `json:"state"`
	Reason     *string             `json:"reason"`
	Components []Component         `json:"components"`
	Previous   []PreviousComponent `json:"previous"`
	Output     string              `json:"output"`
	StartedAt  string              `json:"started_at"`
	UpdatedAt  string              `json:"updated_at"`
	FinishedAt *string             `json:"finished_at"`

	// Beyond the wire's fields, and local by definition:
	// Restored is true when a never-started control-plane apply was rolled back
	// to `.env.prev` by the updater itself (exec.go).
	Restored bool `json:"restored"`
	// Release is the provenance the request carried, so a human reading this
	// file knows what was being installed.
	Release Release `json:"release"`
	// Commands is exactly what was (or will be) run, so the manual recipe in a
	// failure is copy-paste rather than reconstructed.
	Commands [][]string `json:"commands"`
}

// resultIDRe guards the path built from a request id. The id is a uuid by the
// time it reaches here, but this is the one place a request field becomes a
// FILESYSTEM PATH, so it is re-checked rather than assumed: `../../etc/x` must
// never be openable through this door.
var resultIDRe = regexp.MustCompile(`^[0-9a-fA-F-]{36}$`)

// Store owns the results directory and the single-flight latch.
type Store struct {
	dir string

	mu       sync.Mutex
	inflight string
	// accepted caches the 202 body per request id so a re-post of an id that is
	// already in flight is idempotent rather than a second apply.
	accepted map[string]*Accepted
}

// Accepted is the 202 body: what the updater will do, answered at once so the
// requester can persist it and stop caring whether it survives (prototype
// finding 2 — the requester is often destroyed by the work).
type Accepted struct {
	RequestID string              `json:"request_id"`
	Previous  []PreviousComponent `json:"previous"`
	Commands  [][]string          `json:"commands"`
}

// NewStore creates the results directory (0755, root-owned: the control plane
// and the agent read it, only the updater writes it).
func NewStore(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("results dir %s: %w", dir, err)
	}
	// MkdirAll honours umask; the mode is asserted rather than hoped for.
	if err := os.Chmod(dir, 0o755); err != nil {
		return nil, fmt.Errorf("results dir %s: %w", dir, err)
	}
	return &Store{dir: dir, accepted: map[string]*Accepted{}}, nil
}

func (s *Store) path(requestID string) (string, error) {
	if !resultIDRe.MatchString(requestID) {
		return "", fmt.Errorf("request id %q is not a uuid", requestID)
	}
	return filepath.Join(s.dir, requestID+".json"), nil
}

// InFlight is the id of the non-terminal apply, or "".
func (s *Store) InFlight() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.inflight
}

// AcceptedFor returns the cached 202 body for an id, if it has one.
func (s *Store) AcceptedFor(requestID string) (*Accepted, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.accepted[requestID]
	return a, ok
}

// Claim latches the single-flight slot for requestID. It fails when another id
// holds it, so the latch and the check cannot race apart — Plan's `busy` is the
// friendly answer, this is the one that is actually authoritative.
func (s *Store) Claim(requestID string, a *Accepted) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.inflight != "" && s.inflight != requestID {
		return &Rejection{Reason: ReasonBusy, Message: "request " + s.inflight + " is still in flight"}
	}
	s.inflight = requestID
	s.accepted[requestID] = a
	return nil
}

// Release clears the latch if requestID still holds it.
func (s *Store) Release(requestID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.inflight == requestID {
		s.inflight = ""
	}
}

// Write persists a result atomically. `updated_at` is stamped here so no caller
// can forget it, and `finished_at` is filled exactly when the state is terminal.
func (s *Store) Write(r *Result) error {
	p, err := s.path(r.RequestID)
	if err != nil {
		return err
	}
	now := time.Now().UTC().Format(time.RFC3339)
	r.UpdatedAt = now
	if r.StartedAt == "" {
		r.StartedAt = now
	}
	if Terminal(r.State) {
		if r.FinishedAt == nil {
			r.FinishedAt = &now
		}
	} else {
		r.FinishedAt = nil
	}
	// Never a nil slice on the wire: `components` and `previous` are required
	// arrays, and `null` is not an empty array.
	if r.Components == nil {
		r.Components = []Component{}
	}
	if r.Previous == nil {
		r.Previous = []PreviousComponent{}
	}
	body, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	body = append(body, '\n')

	// tmp in the SAME directory: rename is only atomic within a filesystem.
	tmp, err := os.CreateTemp(s.dir, ".tmp-"+r.RequestID+"-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once the rename succeeded
	if _, err := tmp.Write(body); err != nil {
		tmp.Close()
		return err
	}
	// fsync before rename: a rename that lands before the data does would leave
	// a valid name over an empty file after a host crash.
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, 0o644); err != nil {
		return err
	}
	return os.Rename(tmpName, p)
}

// Read loads one result.
func (s *Store) Read(requestID string) (*Result, error) {
	p, err := s.path(requestID)
	if err != nil {
		return nil, err
	}
	body, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	var r Result
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

// TailOutput keeps the LAST cap bytes, cut forward to a line boundary. The
// failing step's error is at the end of its output; a prefix would reliably
// capture the banner and drop the cause.
func TailOutput(s string, cap int) string {
	if len(s) <= cap {
		return s
	}
	s = s[len(s)-cap:]
	if i := strings.IndexByte(s, '\n'); i >= 0 && i+1 < len(s) {
		s = s[i+1:]
	}
	return s
}
