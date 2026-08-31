package library

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrNotFound: a scan id / parent app did not resolve. Maps to 404 either way; a scan the
// caller's host doesn't own reads as nonexistent, deliberately.
var ErrNotFound = errors.New("not found")

// ErrScanNotOpen: a report arrived for a scan not in 'claimed' (e.g. a duplicate report).
var ErrScanNotOpen = errors.New("scan is not open")

// steamAppIDPattern is §10's appid grammar, validation point 2 of 4 (agent parse, ingest, DB
// CHECK, launch-time render in session.composeSteamFlags); only the DB CHECK survives a later
// admin edit.
var steamAppIDPattern = regexp.MustCompile(`^[1-9][0-9]{0,9}$`)

// maxAppID bounds the appid at 2^32 (§10 point 2); the regex alone admits up to 9999999999.
const maxAppID = 1 << 32

// validAppID reports whether an appid from a scan report is storable.
func validAppID(id string) bool {
	if !steamAppIDPattern.MatchString(id) {
		return false
	}
	n, err := strconv.ParseUint(id, 10, 64)
	return err == nil && n < maxAppID
}

// Scan-job constants handed to the agent (§7.3), control-plane-owned so a bound can tighten
// without an agent release.
const (
	scanMaxEntries       = 512
	scanMaxManifestBytes = 1 << 20 // 1 MiB
	claimBatch           = 50      // caps one pull so a backlog drains over several passes
	// ClaimTTL (§7.6): the only recovery for an agent that dies mid-walk or a control-plane
	// restart between claim and report.
	ClaimTTL = 30 * time.Minute
	// terminalRetention bounds the scan log: rows are never read again once superseded
	// (partial open-scan index), and a fleet sweep writes forever otherwise. Not in the spec.
	terminalRetention = 30 * 24 * time.Hour
)

// steamRelativeRoots (§7.4): flatpak/deb and classic paths, de-duplicated by resolved symlink.
// libraryfolders.vdf is not consulted: it records container-side paths meaningless to a
// host-side scanner, so libraries on other disks aren't seen (§16, accepted).
var steamRelativeRoots = []string{
	".local/share/Steam/steamapps",
	".steam/steam/steamapps",
}

// Store is the data-access layer for discovery over the pgx pool.
type Store struct {
	pool *pgxpool.Pool
}

// NewStore constructs a Store from the shared pool.
func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

// --- the pull channel --------------------------------------------------------

// PendingScan is one claimed scan job (§7.3 wire shape). No user id, username, or
// user-derived field: root_path is opaque, resolved to a person only on receipt.
type PendingScan struct {
	ScanID           string   `json:"scan_id"`
	RootPath         string   `json:"root_path"`
	RelativeRoots    []string `json:"relative_roots"`
	MaxEntries       int      `json:"max_entries"`
	MaxManifestBytes int64    `json:"max_manifest_bytes"`
}

// ReportEntry is one parsed `appmanifest_*.acf`. The field set is the PII containment
// mechanism (§9): every manifest carries `LastOwner`, a SteamID64, and the agent parses with a
// key allow-list (not a denylist, which leaks every future Steam key), so this struct has no
// field that could hold a sixth value even if the parser were wrong.
type ReportEntry struct {
	ExternalID string `json:"external_id"`
	Name       string `json:"name"`

	// InstallDir and SizeOnDisk are collected but never read below (launch verification,
	// §16.1, was dropped). Stay in the allow-list anyway: narrowing buys nothing.
	InstallDir string `json:"install_dir"`
	SizeOnDisk int64  `json:"size_on_disk"`
	// StateFlags is likewise unread: it was 4 for all five Valve tools on Tower and three of
	// four real games, so it distinguishes nothing (denylist.go).
	StateFlags int64 `json:"state_flags"`
}

// ScanReport is the POST /v1/agent/library/scan-report body (§7.3).
type ScanReport struct {
	ScanID  string        `json:"scan_id"`
	OK      bool          `json:"ok"`
	Error   string        `json:"error"`
	Entries []ReportEntry `json:"entries"`
}

// ClaimPending claims up to claimBatch pending scans for hostID. FOR UPDATE SKIP LOCKED
// (§7.6) gives concurrent agents disjoint sets. The user_homes join also filters: a
// tombstoned/relocated/hostless home (§7.5) is left unclaimed for the janitor to expire.
func (s *Store) ClaimPending(ctx context.Context, hostID string) ([]PendingScan, error) {
	rows, err := s.pool.Query(ctx, `
		WITH claimable AS (
			SELECT sc.id
			  FROM library_scans sc
			 WHERE sc.host_id = $1::uuid
			   AND sc.state = 'pending'
			   AND EXISTS (
			       SELECT 1 FROM user_homes uh
			        WHERE uh.user_id = sc.user_id AND uh.app_id = sc.app_id
			          AND uh.host_id = sc.host_id
			          AND uh.gc_after IS NULL AND uh.provider = 'local' AND uh.ref <> ''
			   )
			 ORDER BY sc.created_at
			 LIMIT $2
			 FOR UPDATE SKIP LOCKED
		), claimed AS (
			UPDATE library_scans sc
			   SET state = 'claimed', claimed_at = now()
			  FROM claimable c
			 WHERE sc.id = c.id
			RETURNING sc.id, sc.user_id, sc.app_id, sc.host_id
		)
		SELECT cl.id::text, uh.ref
		  FROM claimed cl
		  JOIN user_homes uh
		    ON uh.user_id = cl.user_id AND uh.app_id = cl.app_id AND uh.host_id = cl.host_id
	`, hostID, claimBatch)
	if err != nil {
		return nil, fmt.Errorf("claim pending scans: %w", err)
	}
	defer rows.Close()

	out := []PendingScan{}
	for rows.Next() {
		var p PendingScan
		if err := rows.Scan(&p.ScanID, &p.RootPath); err != nil {
			return nil, fmt.Errorf("scan pending scan: %w", err)
		}
		p.RelativeRoots = steamRelativeRoots
		p.MaxEntries = scanMaxEntries
		p.MaxManifestBytes = scanMaxManifestBytes
		out = append(out, p)
	}
	return out, rows.Err()
}

// scanTarget is the (user, provider app, host) triple a scan id resolves to —
// the mapping the agent never sees.
type scanTarget struct {
	UserID   string
	ParentID string
	HostID   string
	// ProfileID / Policy: the parent's launch defaults, copied onto any tile this scan
	// creates (§7.7 "copied once from the parent").
	ProfileID *string
	Policy    string
}

// resolveScan loads the target of a claimed scan, scoped to the calling host so
// an agent can never report against another host's scan. Returns ErrNotFound for
// an unknown id and ErrScanNotOpen for one that is not 'claimed'.
func resolveScan(ctx context.Context, q pgx.Tx, scanID, hostID string) (scanTarget, error) {
	var t scanTarget
	var state string
	err := q.QueryRow(ctx, `
		SELECT sc.user_id::text, sc.app_id::text, sc.host_id::text, sc.state,
		       a.default_profile_id::text, a.profile_policy
		FROM library_scans sc
		JOIN apps a ON a.id = sc.app_id
		WHERE sc.id::text = $1 AND sc.host_id::text = $2
		FOR UPDATE OF sc
	`, scanID, hostID).Scan(&t.UserID, &t.ParentID, &t.HostID, &state, &t.ProfileID, &t.Policy)
	if errors.Is(err, pgx.ErrNoRows) {
		return scanTarget{}, ErrNotFound
	}
	if err != nil {
		return scanTarget{}, fmt.Errorf("resolve scan: %w", err)
	}
	if state != "claimed" {
		return scanTarget{}, ErrScanNotOpen
	}
	if t.Policy == "" {
		t.Policy = "inherit"
	}
	return t, nil
}

// MarkFailed records an agent-reported failure (§7.3's {ok:false}) and changes nothing else
// (§7.7 step 1, §17): the step-5 revoke is driven by absence, and a transient error (a
// refused path, a torn read, a lost mount) looks exactly like absence, so writing or pruning
// observations from a failed scan would let one bad pass mass-revoke a fleet's libraries.
func (s *Store) MarkFailed(ctx context.Context, scanID, hostID, msg string) error {
	if len(msg) > 512 {
		msg = msg[:512]
	}
	tag, err := s.pool.Exec(ctx, `
		UPDATE library_scans
		   SET state = 'failed', reported_at = now(), error = $3
		 WHERE id::text = $1 AND host_id::text = $2 AND state = 'claimed'
	`, scanID, hostID, msg)
	if err != nil {
		return fmt.Errorf("mark scan failed: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// --- reconciliation (§7.7) ---------------------------------------------------

// ReconcileResult is what one successful scan changed. Counted because under auto-publish the
// only observable signal of a working scan is tiles appearing, so "nothing appeared" and
// "nothing ran" look identical without it (§7.5).
type ReconcileResult struct {
	Observed   int
	Suppressed int
	Created    int
	Disabled   int
	Granted    int
	Revoked    int
	Rejected   int // entries dropped by ingest appid validation (§10 point 2)
	// Backfilled: existing discovered tiles of this parent whose blank description this scan
	// filled in. See the backfill step at the end of Reconcile.
	Backfilled int
}

// Candidate is one observed appid plus the decision the ladder reached for it.
type Candidate struct {
	ExternalID string
	Name       string
	Decision   Decision
}

// Reconcile runs §7.7's five steps in one transaction per scan, atomically with marking the
// scan 'reported': §7.6's table has no `entries` column or `reconciled_at`, so there's no
// state where a report is accepted but not applied. Steps 3 and 5 (create tile, grant
// entitlement) commit together, so a tile never exists with no entitlement. On failure the
// scan stays 'claimed' and ClaimTTL's reaper returns it to 'pending'; the filesystem, not
// this table, is the source of truth.
//
// appDetails is the opt-in fifth rung's (§8.3) appid -> AppDetail map, resolved before this
// transaction opens (a third-party call inside a DB transaction holds locks across a network
// timeout). A missing entry means "not consulted": suppression degrades to "the denylist
// alone decided" and backfill is skipped for that id.
func (s *Store) Reconcile(ctx context.Context, scanID, hostID string, entries []ReportEntry, appDetails map[string]AppDetail) (ReconcileResult, error) {
	var res ReconcileResult

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return res, fmt.Errorf("begin reconcile: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	target, err := resolveScan(ctx, tx, scanID, hostID)
	if err != nil {
		return res, err
	}

	// Ingest validation (§10 point 2): a bad entry is rejected, not the whole report.
	// De-duplicated by appid: two manifest files can share one, and the upsert would
	// otherwise hit the same PK twice in one statement (Postgres refuses that).
	seen := map[string]ReportEntry{}
	order := make([]string, 0, len(entries))
	for _, e := range entries {
		id := strings.TrimSpace(e.ExternalID)
		if !validAppID(id) {
			// Never echoed to a log line: not worth risking arbitrary bytes there.
			res.Rejected++
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		e.ExternalID = id
		e.Name = strings.TrimSpace(e.Name)
		seen[id] = e
		order = append(order, id)
	}
	res.Observed = len(order)

	// --- STEP 1: observations ------------------------------------------------
	// Upsert one row per reported entry including suppressed ones (§7.6), then delete the
	// rows for this (user, parent, host) triple the scan did not list. Only reached from a
	// successful scan (MarkFailed doesn't call in here); scoped by host too, since a user
	// with homes on two hosts has independent observation sets.
	for _, id := range order {
		e := seen[id]
		if _, err := tx.Exec(ctx, `
			INSERT INTO library_observations
			    (user_id, parent_app_id, external_source, external_id, name, host_id, last_seen_at)
			VALUES ($1::uuid, $2::uuid, $3, $4, $5, $6::uuid, now())
			ON CONFLICT (user_id, parent_app_id, external_source, external_id, host_id)
			DO UPDATE SET name = EXCLUDED.name, last_seen_at = now()
		`, target.UserID, target.ParentID, SourceSteam, e.ExternalID, e.Name, target.HostID); err != nil {
			return res, fmt.Errorf("upsert observation: %w", err)
		}
	}
	if _, err := tx.Exec(ctx, `
		DELETE FROM library_observations
		 WHERE user_id = $1::uuid AND parent_app_id = $2::uuid AND host_id = $3::uuid
		   AND external_source = $4
		   AND NOT (external_id = ANY($5::text[]))
	`, target.UserID, target.ParentID, target.HostID, SourceSteam, order); err != nil {
		return res, fmt.Errorf("prune observations: %w", err)
	}

	// --- STEP 2: the suppression decision, computed once ---------------------
	// Steps 4 and 5 both consume this map and never recompute it: a re-derived answer in
	// step 5 would re-grant from the observation step 1 just wrote, undoing an Ignore inside
	// this same transaction.
	rules, err := rulesTx(ctx, tx, target.ParentID, SourceSteam, order)
	if err != nil {
		return res, err
	}
	publish := make([]string, 0, len(order))
	suppress := make([]string, 0, len(order))
	decisions := make(map[string]Decision, len(order))
	for _, id := range order {
		d := Decide(rules[id], id, seen[id].Name)
		// Opt-in fifth rung (§8.3): consulted only for appids the ladder would publish, never
		// one an operator's rule decided (DecidedByRule). A Valve outage leaves the map empty
		// and the ladder's own answer stands.
		if !d.Suppressed && !DecidedByRule(d) {
			if det, consulted := appDetails[id]; consulted && !det.IsGame {
				d = Decision{Suppressed: true, Layer: LayerAppDetails}
			}
		}
		decisions[id] = d
		if d.Suppressed {
			suppress = append(suppress, id)
		} else {
			publish = append(publish, id)
		}
	}
	res.Suppressed = len(suppress)

	// --- STEP 3: auto-publish ------------------------------------------------
	// Ensure the derived tile exists for each not-suppressed appid. An existing tile is never
	// modified here: ON CONFLICT DO NOTHING, not DO UPDATE, so `enabled` is never flipped back
	// to true by a scan even if the rule row that suppressed it were lost (§8.2, guarded by
	// TestIgnoreIsDurableAndIsNotADelete / TestScanNeverReEnablesADisabledTile).
	//
	// One insert per appid rather than a batched unnest(): N is bounded by scanMaxEntries
	// (512), and per-row makes res.Created a true count rather than an aggregate that can't
	// distinguish "created" from "already there".
	for _, id := range publish {
		tag, err := tx.Exec(ctx, `
			INSERT INTO apps (name, kind, parent_app_id, external_source, external_id,
			                  origin, enabled, default_profile_id, profile_policy,
			                  runtime_spec, managed_home, runtime_preset_id, library_provider)
			-- default_profile_id is TEXT and holds a launch-profile SLUG
			-- ("1440p120"), NOT a uuid. It is $5 with no cast for that reason.
			--
			-- It was written ::uuid, and that shipped, because every test fixture
			-- left the parent's default_profile_id NULL — and NULL casts to uuid
			-- happily. The first parent with a real launch profile set made every
			-- reconcile of that home fail with 22P02, which stranded the scan in
			-- 'claimed' and, through library_scans_open_uk, blocked every later
			-- enqueue for that triple. A cast is not free: it asserts a type, and
			-- asserting the wrong one is invisible until the column is non-empty.
			VALUES ($1, 'game', $2::uuid, $3, $4,
			        'discovered', true, $5, $6,
			        '{}'::jsonb, false, NULL, '')
			ON CONFLICT (parent_app_id, external_source, external_id)
			  WHERE parent_app_id IS NOT NULL
			DO NOTHING
		`, tileName(seen[id]), target.ParentID, SourceSteam, id, target.ProfileID, target.Policy)
		if err != nil {
			return res, fmt.Errorf("create derived tile: %w", err)
		}
		res.Created += int(tag.RowsAffected())
	}

	// --- STEP 4: suppressed tiles that already exist -------------------------
	// Disable the tile and revoke its provider entitlements fleet-wide, not just for the
	// scanning user: an Ignore is a fleet decision, so half-revoking it would leave the tile
	// live for everyone not scanned next.
	//
	// Never deletes the row (§8.2, operator requirement): `apps` cascades to
	// user_app_favourites and app_artwork, so deleting would destroy every user's favourite
	// and artwork irreversibly, and the next scan would just re-create a bare row anyway.
	if len(suppress) > 0 {
		tag, err := tx.Exec(ctx, `
			UPDATE apps SET enabled = false
			 WHERE parent_app_id = $1::uuid AND external_source = $2
			   AND external_id = ANY($3::text[]) AND enabled = true
		`, target.ParentID, SourceSteam, suppress)
		if err != nil {
			return res, fmt.Errorf("disable suppressed tiles: %w", err)
		}
		res.Disabled = int(tag.RowsAffected())

		// granted_by='admin' rows are never touched: the sync owns only what it wrote.
		revoked, err := tx.Exec(ctx, `
			DELETE FROM entitlements e
			 USING apps a
			 WHERE e.app_id = a.id
			   AND e.granted_by = 'provider'
			   AND a.parent_app_id = $1::uuid AND a.external_source = $2
			   AND a.external_id = ANY($3::text[])
		`, target.ParentID, SourceSteam, suppress)
		if err != nil {
			return res, fmt.Errorf("revoke suppressed entitlements: %w", err)
		}
		res.Revoked += int(revoked.RowsAffected())
	}

	// --- STEP 5: entitlements ------------------------------------------------
	// Grant ('user', granted_by='provider') for every not-suppressed appid with both an
	// observation and a tile.
	//
	// The suppression filter (`publish`, not the observations) is load-bearing: step 1 wrote
	// an observation for suppressed appids too ("Seen, not published"), so granting over
	// observations would re-grant what step 4 just revoked, undoing an Ignore inside this
	// same transaction.
	//
	// ON CONFLICT DO NOTHING is also how an admin grant wins: entitlements_user_uk is on
	// (subject_id, app_id), so an existing granted_by='admin' row absorbs the conflict and
	// the sync may not revoke it.
	if len(publish) > 0 {
		granted, err := tx.Exec(ctx, `
			INSERT INTO entitlements (subject_type, subject_id, app_id, granted_by, source_ref)
			SELECT 'user', $1::uuid, a.id, 'provider', 'library:' || $2 || ':' || a.external_id
			  FROM apps a
			 WHERE a.parent_app_id = $3::uuid AND a.external_source = $2
			   AND a.external_id = ANY($4::text[])
			ON CONFLICT DO NOTHING
		`, target.UserID, SourceSteam, target.ParentID, publish)
		if err != nil {
			return res, fmt.Errorf("grant provider entitlements: %w", err)
		}
		res.Granted = int(granted.RowsAffected())
	}

	// ...and revoke this user's provider entitlements where no observation remains on any
	// host. No host predicate in the NOT EXISTS is deliberate: a user who moved a game from
	// host A to B still has it; scoping to the scanned host would flap the tile in and out.
	// granted_by='provider' is the other load-bearing predicate: an admin grant survives an
	// uninstall.
	revoked, err := tx.Exec(ctx, `
		DELETE FROM entitlements e
		 USING apps a
		 WHERE e.app_id = a.id
		   AND e.granted_by = 'provider'
		   AND e.subject_type = 'user'
		   AND e.subject_id = $1::uuid
		   AND a.parent_app_id = $2::uuid
		   AND a.external_source = $3
		   AND NOT EXISTS (
		       SELECT 1 FROM library_observations o
		        WHERE o.user_id = $1::uuid
		          AND o.parent_app_id = a.parent_app_id
		          AND o.external_source = a.external_source
		          AND o.external_id = a.external_id
		   )
	`, target.UserID, target.ParentID, SourceSteam)
	if err != nil {
		return res, fmt.Errorf("revoke stale entitlements: %w", err)
	}
	res.Revoked += int(revoked.RowsAffected())

	// --- STEP 6: backfill -----------------------------------------------------
	// Existing discovered tiles with description still '' whose external_id this scan
	// observed — not filtered to `publish`, since a suppressed appid can carry a tile
	// published before an Ignore. Recomputed here from `order` (step 1's validated/deduped
	// set) rather than a pre-transaction read, so nothing is missed or double-counted.
	//
	// Fill-blanks-only is enforced by the UPDATE itself (`AND description = ''`), the same
	// durability guarantee step 3's ON CONFLICT DO NOTHING gives `enabled`.
	candidates, err := backfillCandidates(ctx, tx, target.ParentID, order)
	if err != nil {
		return res, err
	}
	for _, id := range candidates {
		det, ok := appDetails[id]
		if !ok || det.ShortDescription == "" {
			continue
		}
		tag, err := tx.Exec(ctx, `
			UPDATE apps SET description = $4
			 WHERE parent_app_id = $1::uuid AND external_source = $2 AND external_id = $3
			   AND description = ''
		`, target.ParentID, SourceSteam, id, det.ShortDescription)
		if err != nil {
			return res, fmt.Errorf("backfill tile description: %w", err)
		}
		res.Backfilled += int(tag.RowsAffected())
	}

	if _, err := tx.Exec(ctx, `
		UPDATE library_scans
		   SET state = 'reported', reported_at = now(), error = '',
		       observed = $2, suppressed = $3, created = $4, disabled = $5,
		       granted = $6, revoked = $7, rejected = $8, backfilled = $9
		 WHERE id::text = $1
	`, scanID, res.Observed, res.Suppressed, res.Created, res.Disabled,
		res.Granted, res.Revoked, res.Rejected, res.Backfilled); err != nil {
		return res, fmt.Errorf("mark scan reported: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return res, fmt.Errorf("commit reconcile: %w", err)
	}
	return res, nil
}

// tileName is the manifest name, falling back to the appid: `apps.name` is NOT NULL with no
// default and the admin list sorts by it, so an empty name would be unsearchable.
func tileName(e ReportEntry) string {
	if n := strings.TrimSpace(e.Name); n != "" {
		return n
	}
	return e.ExternalID
}

// --- layer 2: the rules table ------------------------------------------------

// rulesTx reads the operator rules for a set of appids inside a transaction.
func rulesTx(ctx context.Context, q pgx.Tx, parentID, source string, ids []string) (map[string]string, error) {
	out := map[string]string{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := q.Query(ctx, `
		SELECT external_id, rule FROM library_appid_rules
		 WHERE parent_app_id = $1::uuid AND external_source = $2 AND external_id = ANY($3::text[])
	`, parentID, source, ids)
	if err != nil {
		return nil, fmt.Errorf("read appid rules: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, rule string
		if err := rows.Scan(&id, &rule); err != nil {
			return nil, fmt.Errorf("scan appid rule: %w", err)
		}
		out[id] = rule
	}
	return out, rows.Err()
}

// Rule is one operator-written layer-2 rule, as the admin surface sees it.
type Rule struct {
	ExternalSource string    `json:"external_source"`
	ExternalID     string    `json:"external_id"`
	Rule           string    `json:"rule"`
	Note           string    `json:"note"`
	CreatedBy      *string   `json:"created_by"`
	CreatedAt      time.Time `json:"created_at"`
}

// ListRules returns every rule on one provider app, newest first.
func (s *Store) ListRules(ctx context.Context, parentID string) ([]Rule, error) {
	if err := s.requireProviderApp(ctx, parentID); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `
		SELECT external_source, external_id, rule, note, created_by::text, created_at
		  FROM library_appid_rules WHERE parent_app_id = $1::uuid
		 ORDER BY created_at DESC, external_id
	`, parentID)
	if err != nil {
		return nil, fmt.Errorf("list appid rules: %w", err)
	}
	defer rows.Close()
	out := []Rule{}
	for rows.Next() {
		var r Rule
		if err := rows.Scan(&r.ExternalSource, &r.ExternalID, &r.Rule, &r.Note, &r.CreatedBy, &r.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan appid rule: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// SetRuleResult reports what SetRule changed beyond the rule row itself.
type SetRuleResult struct {
	Rule     Rule
	Disabled bool
	Revoked  int
}

// SetRule writes (or replaces) one layer-2 rule; for 'ignore', also does the other two
// things §8.2 requires: writes the rule row, disables the tile if one exists, revokes its
// granted_by='provider' entitlements. Never deletes: `apps` cascades to user_app_favourites
// and app_artwork (irreversible), and the appid is still on disk, so the next scan would just
// re-create a bare tile. The rule row is what stops that resurrection loop.
//
// 'allow' writes only the rule. It does not re-enable a tile — that happens via the next
// scan's §7.7 step 3 — since re-enabling here could override an admin's own unrelated
// disable.
func (s *Store) SetRule(ctx context.Context, parentID, source, externalID, rule, note string, actorID *string) (SetRuleResult, error) {
	var res SetRuleResult
	if !ValidRule(rule) {
		return res, fmt.Errorf("invalid rule %q", rule)
	}
	if !validAppID(externalID) {
		return res, fmt.Errorf("invalid external_id")
	}
	if len(note) > 512 {
		note = note[:512]
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return res, fmt.Errorf("begin set rule: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Must be a library provider, not merely an app that exists: a rule on an ordinary app is
	// unreachable by the reconciler forever. Checked inside the transaction so a concurrent
	// un-marking of the provider can't slip a rule in behind it.
	var provider string
	err = tx.QueryRow(ctx, `SELECT library_provider FROM apps WHERE id::text = $1`, parentID).Scan(&provider)
	if errors.Is(err, pgx.ErrNoRows) {
		return res, ErrNotFound
	}
	if err != nil {
		return res, fmt.Errorf("check app: %w", err)
	}
	if provider != SourceSteam {
		return res, ErrNotAProvider
	}

	r := Rule{ExternalSource: source, ExternalID: externalID}
	// The primary key is the idempotency key: set, replaced, or deleted, never accumulated.
	if err := tx.QueryRow(ctx, `
		INSERT INTO library_appid_rules (parent_app_id, external_source, external_id, rule, note, created_by)
		VALUES ($1::uuid, $2, $3, $4, $5, $6::uuid)
		ON CONFLICT (parent_app_id, external_source, external_id)
		DO UPDATE SET rule = EXCLUDED.rule, note = EXCLUDED.note,
		              created_by = EXCLUDED.created_by, created_at = now()
		RETURNING rule, note, created_by::text, created_at
	`, parentID, source, externalID, rule, note, actorID).
		Scan(&r.Rule, &r.Note, &r.CreatedBy, &r.CreatedAt); err != nil {
		return res, fmt.Errorf("write appid rule: %w", err)
	}
	res.Rule = r

	if rule == RuleIgnore {
		tag, err := tx.Exec(ctx, `
			UPDATE apps SET enabled = false
			 WHERE parent_app_id = $1::uuid AND external_source = $2 AND external_id = $3
			   AND enabled = true
		`, parentID, source, externalID)
		if err != nil {
			return res, fmt.Errorf("disable ignored tile: %w", err)
		}
		res.Disabled = tag.RowsAffected() > 0

		revoked, err := tx.Exec(ctx, `
			DELETE FROM entitlements e
			 USING apps a
			 WHERE e.app_id = a.id AND e.granted_by = 'provider'
			   AND a.parent_app_id = $1::uuid AND a.external_source = $2 AND a.external_id = $3
		`, parentID, source, externalID)
		if err != nil {
			return res, fmt.Errorf("revoke ignored entitlements: %w", err)
		}
		res.Revoked = int(revoked.RowsAffected())
	}

	if err := tx.Commit(ctx); err != nil {
		return res, fmt.Errorf("commit set rule: %w", err)
	}
	return res, nil
}

// DeleteRule removes a rule, returning the appid to whatever layer 1 says about
// it. It does not re-enable or re-disable anything: the next scan applies the
// ladder afresh.
func (s *Store) DeleteRule(ctx context.Context, parentID, source, externalID string) error {
	if err := s.requireProviderApp(ctx, parentID); err != nil {
		return err
	}
	tag, err := s.pool.Exec(ctx, `
		DELETE FROM library_appid_rules
		 WHERE parent_app_id = $1::uuid AND external_source = $2 AND external_id = $3
	`, parentID, source, externalID)
	if err != nil {
		return fmt.Errorf("delete appid rule: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// --- "Seen, not published" (§8.2) --------------------------------------------

// Unpublished is one observed appid that has no live tile, plus which layer is
// responsible.
type Unpublished struct {
	ExternalSource string    `json:"external_source"`
	ExternalID     string    `json:"external_id"`
	Name           string    `json:"name"`
	SuppressedBy   string    `json:"suppressed_by"`
	Users          int       `json:"users"`
	LastSeenAt     time.Time `json:"last_seen_at"`
	// HasTile distinguishes "disabled tile" (an Ignore that took effect, favourites/artwork
	// to preserve) from "never created".
	HasTile bool `json:"has_tile"`
}

// Unpublished lists appids observed under one provider app with no visible enabled tile.
// This read is why library_observations records suppressed appids at all (§7.6): without it
// a wrongly-denylisted game would have no surface for an admin to find and `allow`.
func (s *Store) Unpublished(ctx context.Context, parentID string) ([]Unpublished, error) {
	if err := s.requireProviderApp(ctx, parentID); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `
		SELECT o.external_source, o.external_id,
		       -- One appid, many users, possibly divergent names (a torn read, a
		       -- localised client). max() is arbitrary but deterministic; the row
		       -- is a handle for an admin action, not a catalogue entry.
		       max(o.name)                    AS name,
		       count(DISTINCT o.user_id)      AS users,
		       max(o.last_seen_at)            AS last_seen_at,
		       coalesce(max(r.rule), '')      AS rule,
		       bool_or(a.id IS NOT NULL)      AS has_tile,
		       bool_or(a.id IS NOT NULL AND a.enabled) AS has_live_tile
		  FROM library_observations o
		  LEFT JOIN library_appid_rules r
		    ON r.parent_app_id = o.parent_app_id AND r.external_source = o.external_source
		   AND r.external_id = o.external_id
		  LEFT JOIN apps a
		    ON a.parent_app_id = o.parent_app_id AND a.external_source = o.external_source
		   AND a.external_id = o.external_id
		 WHERE o.parent_app_id = $1::uuid
		 GROUP BY o.external_source, o.external_id
		HAVING NOT bool_or(a.id IS NOT NULL AND a.enabled)
		 ORDER BY max(o.last_seen_at) DESC, o.external_id
	`, parentID)
	if err != nil {
		return nil, fmt.Errorf("list unpublished: %w", err)
	}
	defer rows.Close()

	out := []Unpublished{}
	for rows.Next() {
		var u Unpublished
		var rule string
		var hasLive bool
		if err := rows.Scan(&u.ExternalSource, &u.ExternalID, &u.Name, &u.Users,
			&u.LastSeenAt, &rule, &u.HasTile, &hasLive); err != nil {
			return nil, fmt.Errorf("scan unpublished: %w", err)
		}
		d := Decide(rule, u.ExternalID, u.Name)
		if d.Suppressed {
			u.SuppressedBy = d.Layer
		} else {
			// Layers 1-3 would have published this but no enabled tile exists: either the
			// opt-in appdetails rung suppressed it, or an admin disabled it by hand. Neither
			// is knowable from the database, so reported as "other" rather than guessed.
			u.SuppressedBy = "other"
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// ErrNotAProvider: a /library/* route named a real app that isn't a library provider.
// Callers map it to 400, not 404 and not 200 — a rule written against an ordinary app would
// otherwise store fine and be permanently inert, worse than a failure.
var ErrNotAProvider = errors.New("app is not a library provider")

// requireProviderApp resolves {id} on the /library/* admin routes: must exist and be marked
// as a library provider. Reads library_provider, not kind (§4.5.3 forbids branching on kind).
func (s *Store) requireProviderApp(ctx context.Context, appID string) error {
	var provider string
	err := s.pool.QueryRow(ctx,
		`SELECT library_provider FROM apps WHERE id::text = $1`, appID).Scan(&provider)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("check app: %w", err)
	}
	if provider != SourceSteam {
		return ErrNotAProvider
	}
	return nil
}

// HasProviderApp reports whether any app is marked as a library provider, the precondition
// Enqueue's `a.library_provider = 'steam'` join silently asserts. First-run state is "nobody
// marked the app": with none, enqueue matches zero rows, indistinguishable from "no games
// installed" (§7.5) without this as a reported reason.
//
// Reads library_provider, not kind (§4.5.3 forbids branching on kind) — the same predicate
// Enqueue joins on, so this can't claim discovery is live while enqueue matches nothing.
func (s *Store) HasProviderApp(ctx context.Context) (bool, error) {
	var exists bool
	err := s.pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM apps WHERE library_provider = $1)`, SourceSteam).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("check for a library-provider app: %w", err)
	}
	return exists, nil
}

// HostIDs returns every registered host's id, any status: #472's inertReason needs "could any
// host's storage driver resolve to local right now", and a draining/offline host still counts.
func (s *Store) HostIDs(ctx context.Context) ([]string, error) {
	rows, err := s.pool.Query(ctx, `SELECT id::text FROM hosts`)
	if err != nil {
		return nil, fmt.Errorf("list host ids: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan host id: %w", err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// --- janitor queries (§11.1) -------------------------------------------------

// ReapClaimed returns claimed scans older than ClaimTTL to 'pending'. The only recovery for an
// agent that died mid-walk or a control-plane restart between claim and report: the claim is a
// database row, not a socket, so a reconnect needs nothing correlated across the gap.
func (s *Store) ReapClaimed(ctx context.Context, ttl time.Duration) (int, error) {
	tag, err := s.pool.Exec(ctx, `
		UPDATE library_scans
		   SET state = 'pending', claimed_at = NULL
		 WHERE state = 'claimed'
		   AND claimed_at < now() - make_interval(secs => $1)
	`, ttl.Seconds())
	if err != nil {
		return 0, fmt.Errorf("reap claimed scans: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// ExpireStranded fails pending scans whose home is no longer claimable (tombstoned, moved
// host, or the app stopped being a library provider). Without this the open-scan unique index
// blocks that triple forever: ClaimPending's EXISTS predicate rejects the row, and the reaper
// only touches 'claimed'. Marked 'failed', not deleted, so the reason survives in the scan
// log for one retention window.
func (s *Store) ExpireStranded(ctx context.Context) (int, error) {
	tag, err := s.pool.Exec(ctx, `
		UPDATE library_scans sc
		   SET state = 'failed', reported_at = now(),
		       error = 'home is no longer scannable (tombstoned, relocated, or not a local home)'
		 WHERE sc.state = 'pending'
		   AND NOT EXISTS (
		       SELECT 1
		         FROM user_homes uh
		         JOIN apps a ON a.id = uh.app_id
		        WHERE uh.user_id = sc.user_id AND uh.app_id = sc.app_id
		          AND uh.host_id = sc.host_id
		          AND uh.gc_after IS NULL AND uh.provider = 'local' AND uh.ref <> ''
		          AND a.library_provider = 'steam'
		   )
	`)
	if err != nil {
		return 0, fmt.Errorf("expire stranded scans: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// Enqueue inserts a 'pending' scan for every (user, provider app, host) triple with a live
// home, no open scan, and no successful scan inside interval (§11.1 step 3). The predicate is
// the schedule: no timer, no queue service, so it's correct after a restart or a home move
// with no state to rebuild. Reads apps.library_provider, not kind (§4.5.3 forbids branching on
// kind) — a Kind dropdown must not silently stop this job.
func (s *Store) Enqueue(ctx context.Context, interval time.Duration) (int, error) {
	tag, err := s.pool.Exec(ctx, `
		INSERT INTO library_scans (user_id, app_id, host_id, state)
		SELECT uh.user_id, uh.app_id, uh.host_id, 'pending'
		  FROM user_homes uh
		  JOIN apps a ON a.id = uh.app_id
		 WHERE uh.gc_after IS NULL
		   AND uh.host_id IS NOT NULL
		   AND uh.user_id IS NOT NULL
		   AND uh.app_id  IS NOT NULL
		   -- §7.5, per home rather than only per instance: a volume-backed home
		   -- has no host path an agent can walk, so enqueueing one would leave a
		   -- pending row nothing can ever claim. The janitor ALSO refuses to run
		   -- at all when the instance provider is 'volume', because an operator
		   -- needs to be told why nothing appeared — with auto-publish, "nothing
		   -- appeared" and "nothing ran" look identical.
		   AND uh.provider = 'local'
		   AND uh.ref <> ''
		   AND a.library_provider = 'steam'
		   AND NOT EXISTS (
		       SELECT 1 FROM library_scans s
		        WHERE s.user_id = uh.user_id AND s.app_id = uh.app_id AND s.host_id = uh.host_id
		          AND s.state IN ('pending', 'claimed')
		   )
		   AND NOT EXISTS (
		       SELECT 1 FROM library_scans s
		        WHERE s.user_id = uh.user_id AND s.app_id = uh.app_id AND s.host_id = uh.host_id
		          AND s.state = 'reported'
		          AND s.reported_at > now() - make_interval(secs => $1)
		   )
		ON CONFLICT DO NOTHING
	`, interval.Seconds())
	if err != nil {
		return 0, fmt.Errorf("enqueue scans: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// ForceScanResult is what one operator-triggered scan actually did.
//
// Skipped is not an error count: library_scans_open_uk permits one open scan per triple, so
// an already-queued/walking triple is skipped, which is correct. Reported separately from
// Queued so pressing the button twice shows "queued 0, skipped 3" not a bare zero.
type ForceScanResult struct {
	Queued  int `json:"queued"`
	Skipped int `json:"skipped"`
	// Eligible is Queued+Skipped: zero means the scope matched nothing, distinct from
	// "everything was already queued".
	Eligible int `json:"eligible"`
}

// ForceEnqueue is Enqueue minus the recency predicate: it inserts a 'pending' scan for every
// eligible triple immediately, optionally scoped to one app/user. An operator pressing "scan
// now" has a reason the recency rule can't know about, so that pacing is what they override —
// every other gate (instance switch, inertness, env kill switch, checked by the caller;
// per-home `provider='local'` and the library_provider trigger, here unchanged) still applies.
//
// Open-scan uniqueness is ON CONFLICT DO NOTHING, not NOT EXISTS, so a double-click is
// idempotent: the second press inserts nothing and reports skipped.
func (s *Store) ForceEnqueue(ctx context.Context, appID, userID *string) (ForceScanResult, error) {
	var res ForceScanResult
	err := s.pool.QueryRow(ctx, `
		WITH eligible AS (
		    SELECT uh.user_id, uh.app_id, uh.host_id
		      FROM user_homes uh
		      JOIN apps a ON a.id = uh.app_id
		     WHERE uh.gc_after IS NULL
		       AND uh.host_id IS NOT NULL
		       AND uh.user_id IS NOT NULL
		       AND uh.app_id  IS NOT NULL
		       -- §7.5 per home, exactly as Enqueue: a volume-backed home has no
		       -- host path an agent could walk, so forcing one would only create a
		       -- pending row nothing can ever claim.
		       AND uh.provider = 'local'
		       AND uh.ref <> ''
		       AND a.library_provider = 'steam'
		       -- The optional scope. Compared as text so a malformed id is an empty
		       -- result rather than a 22P02 the caller sees as a 500; the handler
		       -- rejects malformed ids with a 400 before ever getting here.
		       AND ($1::text IS NULL OR uh.app_id::text  = $1::text)
		       AND ($2::text IS NULL OR uh.user_id::text = $2::text)
		), queued AS (
		    INSERT INTO library_scans (user_id, app_id, host_id, state)
		    SELECT user_id, app_id, host_id, 'pending' FROM eligible
		    ON CONFLICT DO NOTHING
		    RETURNING 1
		)
		SELECT (SELECT count(*) FROM queued), (SELECT count(*) FROM eligible)
	`, appID, userID).Scan(&res.Queued, &res.Eligible)
	if err != nil {
		return ForceScanResult{}, fmt.Errorf("force enqueue scans: %w", err)
	}
	res.Skipped = res.Eligible - res.Queued
	return res, nil
}

// PruneTerminal bounds the scan log. Completed and failed rows are read by
// nothing once a newer scan supersedes them — the open-scan unique index is
// partial, so they block no enqueue — and a 6-hourly sweep over a fleet would
// otherwise write rows forever.
func (s *Store) PruneTerminal(ctx context.Context) (int, error) {
	tag, err := s.pool.Exec(ctx, `
		DELETE FROM library_scans
		 WHERE state IN ('reported', 'failed')
		   AND created_at < now() - make_interval(secs => $1)
	`, terminalRetention.Seconds())
	if err != nil {
		return 0, fmt.Errorf("prune terminal scans: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// ScanCounts is the per-state census the admin status read reports.
type ScanCounts struct {
	Pending  int `json:"pending"`
	Claimed  int `json:"claimed"`
	Reported int `json:"reported"`
	Failed   int `json:"failed"`
}

// Counts returns the scan census.
func (s *Store) Counts(ctx context.Context) (ScanCounts, error) {
	var c ScanCounts
	err := s.pool.QueryRow(ctx, `
		SELECT count(*) FILTER (WHERE state = 'pending'),
		       count(*) FILTER (WHERE state = 'claimed'),
		       count(*) FILTER (WHERE state = 'reported'),
		       count(*) FILTER (WHERE state = 'failed')
		  FROM library_scans
	`).Scan(&c.Pending, &c.Claimed, &c.Reported, &c.Failed)
	if err != nil {
		return c, fmt.Errorf("scan counts: %w", err)
	}
	return c, nil
}

// LastScanCompletedAt: nil if no scan ever completed; reported_at is stamped on both terminal
// states (MarkFailed, Reconcile). One-glance companion to the per-outcome counters, since a
// healthy-looking count can be hours stale.
func (s *Store) LastScanCompletedAt(ctx context.Context) (*time.Time, error) {
	var t *time.Time
	err := s.pool.QueryRow(ctx, `
		SELECT max(reported_at) FROM library_scans WHERE state IN ('reported', 'failed')
	`).Scan(&t)
	if err != nil {
		return nil, fmt.Errorf("last scan completed at: %w", err)
	}
	return t, nil
}

// RecentScan is one terminal scan as LibraryStatus.recent_scans reports it.
type RecentScan struct {
	User        string    `json:"user"`
	Host        string    `json:"host"`
	State       string    `json:"state"`
	CompletedAt time.Time `json:"completed_at"`
	Observed    int       `json:"observed"`
	Suppressed  int       `json:"suppressed"`
	Created     int       `json:"created"`
	Disabled    int       `json:"disabled"`
	Granted     int       `json:"granted"`
	Revoked     int       `json:"revoked"`
	Rejected    int       `json:"rejected"`
	Backfilled  int       `json:"backfilled"`
	Error       string    `json:"error"`
}

// RecentScans returns the last 20 terminal scans (reported or failed), newest first, with
// username/node_name (never ids). completed_at is reported_at for both terminal states (see
// LastScanCompletedAt). LEFT JOIN so a since-orphaned scan row can never 500 this read.
func (s *Store) RecentScans(ctx context.Context) ([]RecentScan, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT coalesce(u.username, ''), coalesce(h.node_name, ''),
		       sc.state, sc.reported_at,
		       sc.observed, sc.suppressed, sc.created, sc.disabled,
		       sc.granted, sc.revoked, sc.rejected, sc.backfilled, sc.error
		  FROM library_scans sc
		  LEFT JOIN users u ON u.id = sc.user_id
		  LEFT JOIN hosts h ON h.id = sc.host_id
		 WHERE sc.state IN ('reported', 'failed')
		 ORDER BY sc.reported_at DESC
		 LIMIT 20
	`)
	if err != nil {
		return nil, fmt.Errorf("recent scans: %w", err)
	}
	defer rows.Close()
	out := []RecentScan{}
	for rows.Next() {
		var r RecentScan
		if err := rows.Scan(&r.User, &r.Host, &r.State, &r.CompletedAt,
			&r.Observed, &r.Suppressed, &r.Created, &r.Disabled,
			&r.Granted, &r.Revoked, &r.Rejected, &r.Backfilled, &r.Error); err != nil {
			return nil, fmt.Errorf("scan recent scan: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// PublishableAppIDs returns the appids that layers 1-3 would publish, the only set the opt-in
// appdetails lookup may ask about (§8.3). Runs outside the reconcile transaction since it
// needs the rules table and a third-party call must never happen while holding DB locks.
func (s *Store) PublishableAppIDs(ctx context.Context, parentID string, entries []ReportEntry) ([]string, error) {
	ids := make([]string, 0, len(entries))
	names := make(map[string]string, len(entries))
	for _, e := range entries {
		id := strings.TrimSpace(e.ExternalID)
		if !validAppID(id) {
			continue
		}
		if _, dup := names[id]; dup {
			continue
		}
		names[id] = strings.TrimSpace(e.Name)
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return nil, nil
	}
	rows, err := s.pool.Query(ctx, `
		SELECT external_id, rule FROM library_appid_rules
		 WHERE parent_app_id = $1::uuid AND external_source = $2 AND external_id = ANY($3::text[])
	`, parentID, SourceSteam, ids)
	if err != nil {
		return nil, fmt.Errorf("read appid rules: %w", err)
	}
	rules := map[string]string{}
	for rows.Next() {
		var id, rule string
		if err := rows.Scan(&id, &rule); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan appid rule: %w", err)
		}
		rules[id] = rule
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	out := make([]string, 0, len(ids))
	for _, id := range ids {
		d := Decide(rules[id], id, names[id])
		if !d.Suppressed && !DecidedByRule(d) {
			out = append(out, id)
		}
	}
	return out, nil
}

// pgxQueryer is the one method backfillCandidates needs, satisfied by both *pgxpool.Pool (the
// handler's pre-transaction pass) and pgx.Tx (Reconcile step 6), so the query is written once.
type pgxQueryer interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// backfillCandidates returns external_ids of this parent's existing discovered tiles with a
// blank description among ids (the scan's validated, deduplicated appids). origin='discovered'
// scopes to reconciler-created tiles (§7.7 step 3), belt-and-braces alongside the
// (parent_app_id, external_source, external_id) uniqueness that already makes that shape
// exclusive.
func backfillCandidates(ctx context.Context, q pgxQueryer, parentID string, ids []string) ([]string, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	rows, err := q.Query(ctx, `
		SELECT external_id FROM apps
		 WHERE parent_app_id = $1::uuid AND external_source = $2 AND origin = 'discovered'
		   AND description = '' AND external_id = ANY($3::text[])
	`, parentID, SourceSteam, ids)
	if err != nil {
		return nil, fmt.Errorf("read backfill candidates: %w", err)
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan backfill candidate: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// BackfillCandidates runs backfillCandidates over the pool for the handler's pre-reconcile
// pass, so backfill ids fold into the same bounded appdetails Fetch as PublishableAppIDs'
// set, one third-party round trip per id, not two.
func (s *Store) BackfillCandidates(ctx context.Context, parentID string, entries []ReportEntry) ([]string, error) {
	ids := make([]string, 0, len(entries))
	seen := map[string]bool{}
	for _, e := range entries {
		id := strings.TrimSpace(e.ExternalID)
		if !validAppID(id) || seen[id] {
			continue
		}
		seen[id] = true
		ids = append(ids, id)
	}
	return backfillCandidates(ctx, s.pool, parentID, ids)
}

// ScanParent resolves the provider app of a claimed scan on hostID, for the
// pre-transaction appdetails pass. Returns ErrNotFound if the scan is not the
// caller's, and ErrScanNotOpen if it is not claimed.
func (s *Store) ScanParent(ctx context.Context, scanID, hostID string) (string, error) {
	var parent, state string
	err := s.pool.QueryRow(ctx, `
		SELECT app_id::text, state FROM library_scans
		 WHERE id::text = $1 AND host_id::text = $2
	`, scanID, hostID).Scan(&parent, &state)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("resolve scan parent: %w", err)
	}
	if state != "claimed" {
		return "", ErrScanNotOpen
	}
	return parent, nil
}
