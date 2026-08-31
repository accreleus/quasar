package images

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// CatalogImage is one entry of the served catalog (protocol/openapi.yaml
// CatalogImage).
type CatalogImage struct {
	ID             string  `json:"id"`
	DisplayName    string  `json:"display_name"`
	Description    string  `json:"description"`
	Kind           string  `json:"kind"`
	Version        string  `json:"version"`
	RegistryRef    *string `json:"registry_ref"`
	RegistryDigest *string `json:"registry_digest"`
	// ContextSHA is the template analogue of RegistryDigest: the commit sha a
	// template's build context is pinned to. Nil (never "") when unresolved;
	// always nil for a prebuilt entry.
	ContextSHA *string `json:"context_sha"`
	// LocalTag is the template analogue of the adopted registry ref
	// (installed_images.local_tag). Nil for a prebuilt or uninstalled image.
	LocalTag         *string         `json:"local_tag"`
	Artwork          json.RawMessage `json:"artwork"`
	LibraryProvider  *string         `json:"library_provider"`
	Installed        bool            `json:"installed"`
	InstalledVersion *string         `json:"installed_version"`
	Pinned           bool            `json:"pinned"`
	Lazy             bool            `json:"lazy"`
	// RuntimePresetID: the managed preset materialized at install; nil when not
	// installed or the image carries no runtime block.
	RuntimePresetID *string          `json:"runtime_preset_id"`
	UpdateAvailable bool             `json:"update_available"`
	Hosts           []ImageHostState `json:"hosts"`
}

// ImageHostState is per-host presence (protocol/openapi.yaml ImageHostState).
// Part of the frozen response contract, so CatalogImage.Hosts is always a
// non-nil empty slice, never omitted.
type ImageHostState struct {
	HostID   string  `json:"host_id"`
	NodeName string  `json:"node_name"`
	Version  *string `json:"version"`
	State    string  `json:"state"`
	Error    *string `json:"error"`
	Bytes    *int64  `json:"bytes"`
}

// ManifestProvenance is where the served catalog came from (#548). The manifest
// itself is unauthenticated — it is fetched by plain HTTPS GET at a MUTABLE ref
// — so this is the whole defence: not preventing a swap, making one impossible
// to miss. Backed by instance_settings.image_manifest_* (migration 0070).
type ManifestProvenance struct {
	// SHA256 is the digest of the manifest bytes the served catalog was parsed
	// from. Never empty here — the Store answers a nil *ManifestProvenance
	// rather than a zero one when nothing has been recorded.
	SHA256 string `json:"sha256"`
	// PreviousSHA256 is the digest before SHA256, nil until a change has ever
	// been observed.
	PreviousSHA256 *string `json:"previous_sha256"`
	// CommitSHA is the upstream commit the manifest was fetched at, nil when
	// ref resolution failed and the fetch fell back to the mutable ref.
	CommitSHA *string `json:"commit_sha"`
	Ref       string  `json:"ref"`
	URL       string  `json:"url"`
	// Changed reports that the LAST successful sync's digest differed from the
	// previously recorded one. Self-clears on the next unchanged sync;
	// ChangedAt/PreviousSHA256 are the durable record.
	Changed   bool       `json:"changed"`
	ChangedAt *time.Time `json:"changed_at"`
}

// Envelope is the GET /v1/admin/images and POST /v1/admin/images/sync
// response shape (protocol/openapi.yaml ImageCatalogEnvelope).
//
// manifest_provenance (#548) is an additive admin-gated extension: it adds a
// property and changes no existing one. protocol/openapi.yaml does not yet
// declare it — that amendment lands in quasar-protocol under the frozen-
// interface sign-off, and web/src/api/types.ts carries a hand-declared
// intersection until it does.
type Envelope struct {
	ManifestVersion    *int                `json:"manifest_version"`
	CatalogRef         string              `json:"catalog_ref"`
	FetchedAt          *time.Time          `json:"fetched_at"`
	SyncError          *string             `json:"sync_error"`
	ManifestProvenance *ManifestProvenance `json:"manifest_provenance"`
	Images             []CatalogImage      `json:"images"`
}

// Store is the image_catalog data-access layer plus the sync orchestration
// (fetch -> validate -> upsert -> digest resolution -> update-policy
// application).
//
// The last sync's error/timestamp live in instance_settings.image_sync_error /
// image_synced_at (migration 0056), not process memory — invariant #5, state
// is external — so a restart after a failed sync still reports why.
type Store struct {
	pool     *pgxpool.Pool
	fetch    Fetcher
	resolver DigestResolver
	// contextResolver is the template analogue of resolver (image_catalog
	// .context_sha), kept separate because it resolves a different kind of
	// reference (repo ref -> commit sha, not tag -> digest) against a
	// different API.
	contextResolver ContextResolver
	log             *slog.Logger
	path            string // manifest path override for the default fetcher; unused when fetch is injected

	// ensure is the seam onto the ensure orchestrator, used by install/update
	// and the auto update policy. nil is legitimate (no agent registry wired):
	// dispatch no-ops and adoption rows are still written, same as a lazy install.
	ensure Ensure

	// imageSourceHosts is the image-source host allowlist
	// (QUASAR_IMAGE_REGISTRY_HOSTS, allowedHostsFromEnv in digest.go), held here
	// so other paths (provider_app.go artworkCoverURL: catalog-supplied artwork
	// renders in an operator's browser) share the same trust boundary. nil means
	// unrestricted and is test-only; both constructors populate it.
	imageSourceHosts map[string]struct{}

	// providerAllowlist: locally-configured library_provider names
	// EnsureProviders may auto-install (QUASAR_LIBRARY_PROVIDERS), set by
	// SetProviderAllowlist. nil -> allowedProviders' default applies. The local
	// trust boundary stopping a compromised catalog from marking an arbitrary
	// image as a provider.
	providerAllowlist map[string]bool

	// onSyncSuccess, if set, runs after a successful Sync — the provider
	// reconciler seam: app.go kicks EnsureProviders on its own goroutine when
	// discovery is enabled, so a provider that becomes resolvable only on a
	// later sync still gets retried. Must not block; the closure returns
	// immediately and does its work off-thread.
	onSyncSuccess func()

	// syncMu serializes the whole Sync sequence: without it two concurrent
	// syncs against a mutable ref can fetch different revisions and commit in
	// reverse order, leaving the OLDER manifest as the final catalog. Cannot
	// order commits across multiple control-plane replicas (would need a pg
	// advisory lock) — unchanged limitation from P1.
	syncMu sync.Mutex
}

// NewStore builds a Store using the production HTTPFetcher and the real
// registry digest resolver.
func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{
		pool: pool, fetch: NewHTTPFetcher(), resolver: NewRegistryResolver(nil),
		contextResolver: NewGitHubContextResolver(nil), log: slog.Default(),
		imageSourceHosts: allowedHostsFromEnv(),
	}
}

// NewStoreWithFetcher builds a Store with an injected Fetcher — the test seam
// for serving a local fixture instead of a live network call.
//
// resolver defaults to NoopResolver deliberately, so tests don't reach ghcr.io
// for the fixture's refs unless they opt in with SetResolver. imageSourceHosts
// is still populated (unlike resolver): it's a security boundary, and a test
// Store that silently allowed every artwork host would make tests pinning it
// vacuous.
func NewStoreWithFetcher(pool *pgxpool.Pool, fetch Fetcher) *Store {
	return &Store{pool: pool, fetch: fetch, resolver: NoopResolver(), contextResolver: NoopContextResolver(),
		log: slog.Default(), imageSourceHosts: allowedHostsFromEnv()}
}

// artworkHosts falls back to the environment for a zero Store — must never
// degrade to "allow everything" on a security check.
func (s *Store) artworkHosts() map[string]struct{} {
	if s.imageSourceHosts == nil {
		return allowedHostsFromEnv()
	}
	return s.imageSourceHosts
}

// SetResolver overrides the digest resolver (test seam; also for a deployment
// needing a custom transport).
func (s *Store) SetResolver(r DigestResolver) {
	if r != nil {
		s.resolver = r
	}
}

// SetContextResolver overrides the template context-sha resolver (same seam).
func (s *Store) SetContextResolver(r ContextResolver) {
	if r != nil {
		s.contextResolver = r
	}
}

// SetLogger overrides the logger. A nil logger is ignored.
func (s *Store) SetLogger(l *slog.Logger) {
	if l != nil {
		s.log = l
	}
}

// SetEnsurer wires the ensure orchestrator, from main after both are built
// (the Ensurer needs the agent registry, the Store does not).
func (s *Store) SetEnsurer(e Ensure) { s.ensure = e }

// SetOnSyncSuccess wires the post-sync provider reconciler hook (see the
// onSyncSuccess field). nil is fine — Sync simply skips it.
func (s *Store) SetOnSyncSuccess(f func()) { s.onSyncSuccess = f }

// CatalogRef reads instance_settings.image_catalog_ref via raw SQL rather than
// an internal/settings import, to keep this package's dependency surface to
// internal/images + the migration. Returns "stable" if the singleton is unseeded.
func (s *Store) CatalogRef(ctx context.Context) (string, error) {
	var ref string
	err := s.pool.QueryRow(ctx, `SELECT image_catalog_ref FROM instance_settings WHERE id = true`).Scan(&ref)
	if err == pgx.ErrNoRows {
		return "stable", nil
	}
	if err != nil {
		return "", fmt.Errorf("read image_catalog_ref: %w", err)
	}
	return ref, nil
}

// rowQuerier is the common subset of *pgxpool.Pool and pgx.Tx, so
// libraryDiscoveryEnabledTx reads a consistent value whether the caller has an
// open transaction (Uninstall, #471) or not.
type rowQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// libraryDiscoveryEnabledTx reads instance_settings.library_discovery_enabled
// inside the caller's own transaction (#471 provider-uninstall guard needs the
// current value, not one stale by commit time). Missing singleton reads false.
func (s *Store) libraryDiscoveryEnabledTx(ctx context.Context, q rowQuerier) (bool, error) {
	var enabled bool
	err := q.QueryRow(ctx, `SELECT library_discovery_enabled FROM instance_settings WHERE id = true`).Scan(&enabled)
	if err == pgx.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read library_discovery_enabled: %w", err)
	}
	return enabled, nil
}

// Sync fetches the manifest at the configured catalog ref, validates it, and
// upserts image_catalog. A fetch or validation failure never surfaces as a
// 5xx: it records sync_error and Envelope serves the last-good cached catalog
// (protocol/control-api.md: "a fetch failure never affects launches").
func (s *Store) Sync(ctx context.Context) (Envelope, error) {
	s.syncMu.Lock() // one sync at a time — see the field doc
	defer s.syncMu.Unlock()

	ref, err := s.CatalogRef(ctx)
	if err != nil {
		// CatalogRef already maps the missing-singleton case to the default, so
		// any error here is a real DB failure. Must not fall back and fetch
		// anyway: a branch/commit-pinned instance would silently commit a
		// different catalog.
		return s.failSync(ctx, err.Error())
	}

	// Resolve the mutable ref to a commit sha FIRST, then fetch the manifest at
	// that sha, so the manifest and every template's context_sha come from the
	// same commit — resolving after fetch could pair a manifest from commit A
	// with a context_sha from commit B. Resolution failure never fails the
	// sync: falls back to the mutable ref (templates stay un-installable,
	// prebuilts unaffected).
	fetchRef := ref
	resolvedSHA, resolved := s.resolveCatalogRef(ctx, ref)
	if resolved {
		fetchRef = resolvedSHA
	}

	data, fetchErr := s.fetch.Fetch(ctx, fetchRef)
	if fetchErr != nil {
		return s.failSync(ctx, fetchErr.Error())
	}

	manifest, parseErr := ParseAndValidate(data)
	if parseErr != nil {
		return s.failSync(ctx, parseErr.Error())
	}

	// #548: digest the bytes that just validated. Recorded only on a SUCCESSFUL
	// sync, in upsert's own transaction, so the provenance can never describe a
	// manifest other than the one whose rows are stored.
	prov := ManifestProvenance{
		SHA256:    fmt.Sprintf("%x", sha256.Sum256(data)),
		Ref:       ref,
		URL:       s.manifestURL(fetchRef),
		CommitSHA: &resolvedSHA,
	}
	if !resolved {
		prov.CommitSHA = nil
	}

	// upsert records the successful sync state (error cleared, time stamped)
	// atomically with the catalog it wrote — see upsert.
	recorded, err := s.upsert(ctx, manifest, prov)
	if err != nil {
		return s.failSync(ctx, err.Error())
	}
	s.logProvenance(recorded)

	// Everything below is post-success, best effort by contract: a registry
	// that won't answer, or an update that can't apply, must never retroactively
	// turn a successful refresh into a failed sync (protocol/control-api.md
	// §Digest pinning).
	s.resolveDigests(ctx, manifest)
	s.stampTemplateContexts(ctx, resolvedSHA, manifest) // sha already resolved above (resolve-then-fetch)
	s.applyUpdatePolicy(ctx)

	// A provider unresolved at an earlier enable can become resolvable on this
	// sync, so re-run the idempotent provider ensure after every refresh. Must
	// return immediately (app.go runs it on its own goroutine) so it never
	// extends the syncMu hold.
	if s.onSyncSuccess != nil {
		s.onSyncSuccess()
	}

	return s.Envelope(ctx)
}

// manifestURL names the URL the manifest at fetchRef was retrieved from, for
// the #548 provenance record: the fetcher's own configured repo/path when it
// exposes them (production *HTTPFetcher), else the configured defaults — the
// same fallback shape catalogRepo uses for a test fetcher.
func (s *Store) manifestURL(fetchRef string) string {
	if un, ok := s.fetch.(URLNamer); ok {
		return un.ManifestURL(fetchRef)
	}
	return ManifestURL(s.catalogRepo(), ConfiguredCatalogPath(), fetchRef)
}

// logProvenance is the #548 log half: an INFO on every successful sync naming
// the digest, and a WARN when it moved. The catalog is fetched unauthenticated
// from a mutable ref, so a digest change is exactly the event an operator
// watching logs must not have to open the UI to notice.
func (s *Store) logProvenance(p ManifestProvenance) {
	commit := "unresolved"
	if p.CommitSHA != nil {
		commit = *p.CommitSHA
	}
	s.log.Info("image catalog manifest fetched",
		"token", "catalog-manifest-digest",
		"manifest_sha256", p.SHA256, "ref", p.Ref, "commit_sha", commit, "url", p.URL)
	if !p.Changed {
		return
	}
	prev := ""
	if p.PreviousSHA256 != nil {
		prev = *p.PreviousSHA256
	}
	s.log.Warn("image catalog manifest CHANGED: the upstream manifest at this ref is not the one previously synced; review the catalog before installing or updating",
		"token", "catalog-manifest-changed",
		"manifest_sha256", p.SHA256, "previous_manifest_sha256", prev,
		"ref", p.Ref, "commit_sha", commit, "url", p.URL)
}

// recordProvenance writes the #548 provenance for the manifest this transaction
// is committing and returns it as stored, with Changed/PreviousSHA256/ChangedAt
// filled in by the database. Change detection is done in SQL against the
// existing row (every ON CONFLICT SET expression reads the OLD row) rather than
// read-then-write in Go, so the comparison and the write cannot straddle a
// concurrent sync from another control-plane replica — the one case syncMu, an
// in-process mutex, cannot order.
//
// The first sync (old digest empty) is NOT a change: there is nothing it could
// have changed from, and reporting one would cry wolf on every fresh instance.
func recordProvenance(ctx context.Context, tx dbExecutor, p ManifestProvenance) (ManifestProvenance, error) {
	commit := ""
	if p.CommitSHA != nil {
		commit = *p.CommitSHA
	}
	out := p
	var prev string
	err := tx.QueryRow(ctx, `
		INSERT INTO instance_settings (
			id, image_sync_error, image_synced_at,
			image_manifest_sha256, image_manifest_prev_sha256, image_manifest_commit_sha,
			image_manifest_ref, image_manifest_url, image_manifest_changed, image_manifest_changed_at
		) VALUES (true, '', now(), $1, '', $2, $3, $4, false, now())
		ON CONFLICT (id) DO UPDATE SET
			image_sync_error = '',
			image_synced_at  = now(),
			image_manifest_prev_sha256 = CASE
				WHEN instance_settings.image_manifest_sha256 <> EXCLUDED.image_manifest_sha256
				THEN instance_settings.image_manifest_sha256
				ELSE instance_settings.image_manifest_prev_sha256 END,
			image_manifest_changed = instance_settings.image_manifest_sha256 <> ''
				AND instance_settings.image_manifest_sha256 <> EXCLUDED.image_manifest_sha256,
			image_manifest_changed_at = CASE
				WHEN instance_settings.image_manifest_sha256 <> EXCLUDED.image_manifest_sha256
				THEN now()
				ELSE instance_settings.image_manifest_changed_at END,
			image_manifest_sha256     = EXCLUDED.image_manifest_sha256,
			image_manifest_commit_sha = EXCLUDED.image_manifest_commit_sha,
			image_manifest_ref        = EXCLUDED.image_manifest_ref,
			image_manifest_url        = EXCLUDED.image_manifest_url
		RETURNING image_manifest_prev_sha256, image_manifest_changed, image_manifest_changed_at
	`, p.SHA256, commit, p.Ref, p.URL).Scan(&prev, &out.Changed, &out.ChangedAt)
	if err != nil {
		return out, fmt.Errorf("record image manifest provenance: %w", err)
	}
	if prev != "" {
		out.PreviousSHA256 = &prev
	}
	return out, nil
}

// manifestProvenance reads the stored #548 provenance. nil (never a zero
// struct) when no successful sync has ever recorded one — the wire null that
// tells the admin UI there is nothing to show yet rather than an empty digest.
func (s *Store) manifestProvenance(ctx context.Context) *ManifestProvenance {
	var (
		p            ManifestProvenance
		prev, commit string
	)
	err := s.pool.QueryRow(ctx, `
		SELECT image_manifest_sha256, image_manifest_prev_sha256, image_manifest_commit_sha,
		       image_manifest_ref, image_manifest_url, image_manifest_changed, image_manifest_changed_at
		FROM instance_settings WHERE id = true
	`).Scan(&p.SHA256, &prev, &commit, &p.Ref, &p.URL, &p.Changed, &p.ChangedAt)
	if err != nil {
		if err != pgx.ErrNoRows {
			s.log.Error("read image manifest provenance", "err", err)
		}
		return nil
	}
	if p.SHA256 == "" {
		return nil
	}
	if prev != "" {
		p.PreviousSHA256 = &prev
	}
	if commit != "" {
		p.CommitSHA = &commit
	}
	return &p
}

// maxSyncErrorLen bounds image_sync_error: the message can carry registry- or
// manifest-supplied text, and must not write unbounded into the settings singleton.
const maxSyncErrorLen = 500

// clampSyncError bounds a message to what image_sync_error stores.
func clampSyncError(msg string) string {
	if len(msg) > maxSyncErrorLen {
		return msg[:maxSyncErrorLen]
	}
	return msg
}

// setSyncError records a failed sync. image_synced_at is left alone — it names
// when the currently-served catalog was fetched, and this attempt didn't
// replace it. Returns the write error so failSync can still report the
// failure even when persisting it didn't stick.
func (s *Store) setSyncError(ctx context.Context, msg string) error {
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO instance_settings (id, image_sync_error) VALUES (true, $1)
		ON CONFLICT (id) DO UPDATE SET image_sync_error = EXCLUDED.image_sync_error
	`, clampSyncError(msg)); err != nil {
		return fmt.Errorf("record image sync error: %w", err)
	}
	return nil
}

// failSync records msg and returns the envelope to serve. SyncError is set
// regardless of whether the persist succeeded: a failed sync must never report
// success just because writing the error row also failed.
func (s *Store) failSync(ctx context.Context, msg string) (Envelope, error) {
	if err := s.setSyncError(ctx, msg); err != nil {
		s.log.Error("record image sync error", "err", err, "sync_error", msg)
	}
	env, err := s.Envelope(ctx)
	if err != nil {
		return env, err
	}
	m := clampSyncError(msg)
	env.SyncError = &m
	return env, nil
}

// syncState reads (sync_error, synced_at). Empty error is nil on the wire —
// sync_error is nullable, and "" would read as a failure with no message.
func (s *Store) syncState(ctx context.Context) (*string, *time.Time) {
	var (
		msg      string
		syncedAt *time.Time
	)
	err := s.pool.QueryRow(ctx,
		`SELECT image_sync_error, image_synced_at FROM instance_settings WHERE id = true`).Scan(&msg, &syncedAt)
	if err != nil {
		if err != pgx.ErrNoRows {
			s.log.Error("read image sync state", "err", err)
		}
		return nil, nil
	}
	if msg == "" {
		return nil, syncedAt
	}
	return &msg, syncedAt
}

// normalizeJSONObject returns b, or `{}` if b is empty/literal null: every
// JSONB column here is NOT NULL DEFAULT '{}', and object-typed OpenAPI fields
// (CatalogImage.artwork) must never receive SQL NULL or JSON null.
func normalizeJSONObject(b json.RawMessage) json.RawMessage {
	trimmed := bytes.TrimSpace(b)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return json.RawMessage(`{}`)
	}
	return b
}

// installedAdoptions reads, for every installed image, what
// reconcileRuntimeDrift (preset.go, #470) needs to detect and repair a
// same-version runtime-block drift. Read once, before upsert's loop
// overwrites image_catalog.runtime.
func installedAdoptions(ctx context.Context, tx dbExecutor) (map[string]installedAdoption, error) {
	rows, err := tx.Query(ctx, `
		SELECT ii.image_id, ii.version, ic.runtime, ii.registry_ref, ii.local_tag,
		       COALESCE(ii.runtime_preset_id::text, ''), COALESCE(ic.library_provider, '')
		FROM installed_images ii
		JOIN image_catalog ic ON ic.id = ii.image_id
	`)
	if err != nil {
		return nil, fmt.Errorf("query installed adoptions: %w", err)
	}
	defer rows.Close()
	out := map[string]installedAdoption{}
	for rows.Next() {
		var id string
		var a installedAdoption
		if err := rows.Scan(&id, &a.version, &a.runtime, &a.registryRef, &a.localTag, &a.presetID, &a.provider); err != nil {
			return nil, fmt.Errorf("scan installed adoption: %w", err)
		}
		out[id] = a
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate installed adoptions: %w", err)
	}
	return out, nil
}

// upsert writes every manifest entry into image_catalog and reconciles
// deletions, all in one transaction: never half-written on a dropped
// connection, and a withdrawn image must disappear only as part of the same
// successful sync, never a later failed one.
// It also records the #548 manifest provenance in the same transaction and
// returns it as stored, so what the log and the UI report about the manifest's
// origin can never describe a catalog that did not commit.
func (s *Store) upsert(ctx context.Context, m *Manifest, prov ManifestProvenance) (ManifestProvenance, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return prov, fmt.Errorf("begin image_catalog upsert: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Must read before the loop overwrites image_catalog.runtime: reconcileRuntimeDrift
	// needs the OLD block to diff against the new one.
	installed, err := installedAdoptions(ctx, tx)
	if err != nil {
		return prov, fmt.Errorf("read installed adoptions: %w", err)
	}

	now := time.Now().UTC()
	ids := make([]string, 0, len(m.Images))
	for _, img := range m.Images {
		ids = append(ids, img.ID)

		// img.Raw preserves fields this build doesn't declare (re-marshaling the
		// typed struct would drop them); the empty fallback only guards a
		// hand-built test Manifest that skipped Parse.
		raw := []byte(img.Raw)
		if len(raw) == 0 {
			raw, err = json.Marshal(img)
			if err != nil {
				return prov, fmt.Errorf("marshal manifest image %q: %w", img.ID, err)
			}
		}
		buildArgs := normalizeJSONObject(img.BuildArgs)
		artwork := normalizeJSONObject(img.Artwork)
		runtime := normalizeJSONObject(img.Runtime)
		_, err = tx.Exec(ctx, `
			INSERT INTO image_catalog (
				id, manifest_version, display_name, description, kind, version,
				registry_ref, dockerfile, build_args, artwork, runtime,
				library_provider, min_quasar_version, fetched_at, raw, updated_at
			) VALUES (
				$1, $2, $3, $4, $5, $6,
				NULLIF($7, ''), NULLIF($8, ''), $9, $10, $11,
				NULLIF($12, ''), NULLIF($13, ''), $14, $15, $14
			)
			ON CONFLICT (id) DO UPDATE SET
				manifest_version   = EXCLUDED.manifest_version,
				display_name       = EXCLUDED.display_name,
				description        = EXCLUDED.description,
				kind               = EXCLUDED.kind,
				version            = EXCLUDED.version,
				registry_ref       = EXCLUDED.registry_ref,
				dockerfile         = EXCLUDED.dockerfile,
				build_args         = EXCLUDED.build_args,
				artwork            = EXCLUDED.artwork,
				runtime            = EXCLUDED.runtime,
				library_provider   = EXCLUDED.library_provider,
				min_quasar_version = EXCLUDED.min_quasar_version,
				fetched_at         = EXCLUDED.fetched_at,
				raw                = EXCLUDED.raw,
				updated_at         = EXCLUDED.updated_at,
				-- P3 (#440): CLEAR the digest on EVERY upsert, then let this
				-- sync's resolution pass repopulate it only on a fresh, successful
				-- resolve. Retaining it when registry_ref is unchanged was a
				-- staleness bug: a tag can be RETARGETED to new bits under the
				-- same version label, the version can move, or this sync's
				-- resolution can fail — in each case a retained digest names the
				-- WRONG (old) bits, which a later install would adopt. Existing
				-- installs are unaffected: they froze their digest into
				-- installed_images at adoption time and never re-read this column.
				registry_digest    = '',
				-- P4: CLEAR the resolved context sha on EVERY upsert too, for the
				-- same staleness reason registry_digest is cleared above — the
				-- catalog ref (branch/tag) this sync fetched from can move, or this
				-- sync's own resolution can fail, and a retained sha would then name
				-- the WRONG build context for an entry that looks unchanged.
				-- resolveTemplateContexts repopulates it on a fresh, successful
				-- resolve, below.
				context_sha        = ''
		`, img.ID, m.ManifestVersion, img.DisplayName, img.Description, img.Kind, img.Version,
			img.RegistryRef, img.Dockerfile, buildArgs, artwork, runtime,
			img.LibraryProvider, img.MinQuasarVersion, now, []byte(raw))
		if err != nil {
			return prov, fmt.Errorf("upsert image_catalog id=%q: %w", img.ID, err)
		}

		// #470: if this image is installed and its version didn't move, the
		// runtime write above isn't cosmetic — it's the operator's live app
		// going stale. Detect and repair now, in this transaction.
		old, ok := installed[img.ID]
		if err := reconcileRuntimeDrift(ctx, tx, s.log, img.ID, img, old, ok); err != nil {
			return prov, err
		}
	}

	// An id the manifest no longer lists must not linger forever. `!= ALL($1)`
	// against an empty ids is vacuously true, so a zero-image manifest correctly
	// empties the catalog. Same transaction as the upserts, so it only ever
	// takes effect with a manifest that validated.
	if _, err := tx.Exec(ctx, `DELETE FROM image_catalog WHERE id != ALL($1::text[])`, ids); err != nil {
		return prov, fmt.Errorf("reconcile image_catalog deletions: %w", err)
	}

	// Sync state (error cleared, time stamped) and the #548 provenance are
	// written in the SAME transaction as the catalog they describe: a separate
	// post-commit write could fail independently and leave a successful sync
	// still reporting the previous failure, or vice versa.
	stored, err := recordProvenance(ctx, tx, prov)
	if err != nil {
		return prov, err
	}

	if err := tx.Commit(ctx); err != nil {
		return prov, fmt.Errorf("commit image_catalog upsert: %w", err)
	}
	return stored, nil
}

// hostStates reads every host_images row, keyed by image id. One query for the
// whole catalog, not one per image, so the admin page's cost doesn't scale
// with the catalog. version/error are pointers: NOT NULL columns store "" but
// the wire type is nullable.
func (s *Store) hostStates(ctx context.Context) (map[string][]ImageHostState, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT hi.image_id, hi.host_id::text, h.node_name, hi.version, hi.state, hi.error, hi.bytes
		FROM host_images hi
		JOIN hosts h ON h.id = hi.host_id
		ORDER BY h.node_name, hi.host_id
	`)
	if err != nil {
		return nil, fmt.Errorf("query host_images: %w", err)
	}
	defer rows.Close()
	out := make(map[string][]ImageHostState)
	for rows.Next() {
		var (
			imageID       string
			hs            ImageHostState
			version, eMsg string
			bytes         *int64
		)
		if err := rows.Scan(&imageID, &hs.HostID, &hs.NodeName, &version, &hs.State, &eMsg, &bytes); err != nil {
			return nil, fmt.Errorf("scan host_images row: %w", err)
		}
		if version != "" {
			v := version
			hs.Version = &v
		}
		if eMsg != "" {
			e := eMsg
			hs.Error = &e
		}
		hs.Bytes = bytes
		out[imageID] = append(out[imageID], hs)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate host_images: %w", err)
	}
	return out, nil
}

// adoption is one installed_images row as the envelope needs it.
type adoption struct {
	Version         string
	Pinned          bool
	Lazy            bool
	LocalTag        string  // non-empty for a template adoption, empty for a prebuilt one
	RuntimePresetID *string // managed preset materialized at install; nil if no runtime block
}

// installedAdoptions reads the adoption set as image id → adoption state.
func (s *Store) installedAdoptions(ctx context.Context) (map[string]adoption, error) {
	rows, err := s.pool.Query(ctx, `SELECT image_id, version, pinned, lazy, local_tag, runtime_preset_id::text FROM installed_images`)
	if err != nil {
		return nil, fmt.Errorf("query installed_images: %w", err)
	}
	defer rows.Close()
	out := make(map[string]adoption)
	for rows.Next() {
		var (
			id string
			a  adoption
		)
		if err := rows.Scan(&id, &a.Version, &a.Pinned, &a.Lazy, &a.LocalTag, &a.RuntimePresetID); err != nil {
			return nil, fmt.Errorf("scan installed_images row: %w", err)
		}
		out[id] = a
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate installed_images: %w", err)
	}
	return out, nil
}

// resolveDigests resolves every prebuilt entry's registry_ref tag to its
// content digest (protocol/control-api.md §Digest pinning, #440). Best effort:
// a per-image failure is a WARN and an empty digest, never a sync failure. An
// empty digest just makes install/update of that image 409 digest_unresolved
// until a later sync succeeds.
func (s *Store) resolveDigests(ctx context.Context, m *Manifest) {
	if s.resolver == nil {
		return
	}
	for _, img := range m.Images {
		if img.Kind != "prebuilt" || img.RegistryRef == "" {
			continue // a template has nothing in a registry to resolve
		}
		digest, err := s.resolver.Resolve(ctx, img.RegistryRef)
		if err != nil {
			s.log.Warn("image sync: digest unresolved; install of this image will be refused until a later sync resolves it",
				"image_id", img.ID, "registry_ref", img.RegistryRef, "err", err)
			continue
		}
		// Match id AND version AND registry_ref so a slow/overlapping sync can't
		// stamp a digest onto a row a newer sync already moved to a different
		// version/ref. RowsAffected==0 is a discarded stale resolution, not an error.
		tag, err := s.pool.Exec(ctx, `
			UPDATE image_catalog
			   SET registry_digest = $2
			 WHERE id = $1
			   AND version = $3
			   AND registry_ref IS NOT DISTINCT FROM $4
		`, img.ID, digest, img.Version, img.RegistryRef)
		if err != nil {
			s.log.Error("image sync: storing resolved digest failed", "image_id", img.ID, "err", err)
			continue
		}
		if tag.RowsAffected() == 0 {
			s.log.Warn("image sync: stale digest resolution discarded (row moved under a concurrent sync)",
				"image_id", img.ID, "version", img.Version, "registry_ref", img.RegistryRef)
			continue
		}
		s.log.Debug("image sync: digest resolved", "image_id", img.ID, "registry_digest", digest)
	}
}

// catalogRepo is the "owner/name" repo the manifest and every template's build
// context come from. Single source for both the context resolver and the
// per-adoption context_repo freeze (actions.go), so a frozen context_repo
// always matches the repo its context_sha was resolved against.
func (s *Store) catalogRepo() string {
	if rn, ok := s.fetch.(RepoNamer); ok {
		return rn.RepoName()
	}
	return ConfiguredCatalogRepo()
}

// resolveCatalogRef resolves the ref to a commit sha, returning ("", false) on
// failure — a WARN, never a sync failure (mirrors resolveDigests/#440): the
// caller falls back to the mutable ref, leaving template context_sha empty
// (409 context_unresolved until a later sync). Prebuilt-only catalogs are unaffected.
func (s *Store) resolveCatalogRef(ctx context.Context, ref string) (string, bool) {
	if s.contextResolver == nil {
		return "", false
	}
	repo := s.catalogRepo()
	sha, err := s.contextResolver.Resolve(ctx, repo, ref)
	if err != nil {
		s.log.Warn("image sync: catalog ref unresolved; fetching at the mutable ref, template context shas stay empty (install of template images refused until a later sync resolves it)",
			"repo", repo, "ref", ref, "err", err)
		return "", false
	}
	return sha, true
}

// stampTemplateContexts stamps the resolve-then-fetch sha onto every
// kind=template row this sync wrote — one sha for the whole manifest, since it
// was fetched at that sha. Empty sha (resolution failed) is a no-op: context_sha
// stays empty, a supported state (409 context_unresolved until a later sync).
func (s *Store) stampTemplateContexts(ctx context.Context, sha string, m *Manifest) {
	if sha == "" {
		return
	}
	for _, img := range m.Images {
		if img.Kind != KindTemplate {
			continue
		}
		// Match id AND version, same staleness reason as resolveDigests.
		tag, err := s.pool.Exec(ctx, `
			UPDATE image_catalog
			   SET context_sha = $2
			 WHERE id = $1
			   AND version = $3
		`, img.ID, sha, img.Version)
		if err != nil {
			s.log.Error("image sync: storing resolved context sha failed", "image_id", img.ID, "err", err)
			continue
		}
		if tag.RowsAffected() == 0 {
			s.log.Warn("image sync: stale context sha resolution discarded (row moved under a concurrent sync)",
				"image_id", img.ID, "version", img.Version)
			continue
		}
		s.log.Debug("image sync: template context sha resolved", "image_id", img.ID, "context_sha", sha)
	}
}

// updatePolicy reads instance_settings.image_update_policy; unseeded reads
// `notify` — never silently auto-update an unconfigured instance.
func (s *Store) updatePolicy(ctx context.Context) string {
	var policy string
	err := s.pool.QueryRow(ctx, `SELECT image_update_policy FROM instance_settings WHERE id = true`).Scan(&policy)
	if err != nil {
		if err != pgx.ErrNoRows {
			s.log.Error("read image_update_policy", "err", err)
		}
		return "notify"
	}
	return policy
}

// applyUpdatePolicy is the post-sync half of the update policy
// (protocol/control-api.md §Update-policy semantics): manual/notify does
// nothing; auto re-adopts and re-ensures every installed, unpinned image whose
// catalog version moved. A running session is never affected — re-adoption
// changes only what a future launch places.
func (s *Store) applyUpdatePolicy(ctx context.Context) {
	if s.updatePolicy(ctx) != "auto" {
		return
	}
	rows, err := s.pool.Query(ctx, `
		SELECT ii.image_id
		FROM installed_images ii
		JOIN image_catalog ic ON ic.id = ii.image_id
		WHERE ii.pinned = false AND ii.version <> ic.version
		ORDER BY ii.image_id
	`)
	if err != nil {
		s.log.Error("auto update policy: listing drifted images failed", "err", err)
		return
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			s.log.Error("auto update policy: scan failed", "err", err)
			return
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		s.log.Error("auto update policy: iterate failed", "err", err)
		return
	}

	for _, id := range ids {
		applied, _, err := s.Update(ctx, id)
		switch {
		case errors.Is(err, ErrDigestUnresolved):
			// Not an error worth alarming on: keeps the fleet on the last
			// known-good digest, safer than adopting a floating tag.
			s.log.Warn("auto update policy: skipping image with an unresolved digest", "image_id", id)
		case err != nil:
			s.log.Error("auto update policy: update failed", "image_id", id, "err", err)
		case applied:
			s.log.Info("auto update policy: image re-adopted and re-ensured", "image_id", id)
		}
	}
}

// Envelope builds the current served catalog: cached image_catalog rows, each
// image's per-host state and adoption state, the pinned catalog_ref, and the
// last sync's error/fetched_at.
func (s *Store) Envelope(ctx context.Context) (Envelope, error) {
	ref, err := s.CatalogRef(ctx)
	if err != nil {
		ref = "stable"
	}

	hostStates, err := s.hostStates(ctx)
	if err != nil {
		return Envelope{}, err
	}
	installed, err := s.installedAdoptions(ctx)
	if err != nil {
		return Envelope{}, err
	}

	rows, err := s.pool.Query(ctx, `
		SELECT id, manifest_version, display_name, description, kind, version,
		       registry_ref, registry_digest, context_sha, artwork, library_provider, fetched_at
		FROM image_catalog
		ORDER BY display_name, id
	`)
	if err != nil {
		return Envelope{}, fmt.Errorf("query image_catalog: %w", err)
	}
	defer rows.Close()

	var (
		images         []CatalogImage
		maxManifestVer *int
		maxFetchedAt   *time.Time
	)
	for rows.Next() {
		var (
			ci          CatalogImage
			manifestVer int
			fetchedAt   time.Time
			artwork     []byte
			digest      string
			contextSHA  string
		)
		if err := rows.Scan(&ci.ID, &manifestVer, &ci.DisplayName, &ci.Description, &ci.Kind, &ci.Version,
			&ci.RegistryRef, &digest, &contextSHA, &artwork, &ci.LibraryProvider, &fetchedAt); err != nil {
			return Envelope{}, fmt.Errorf("scan image_catalog row: %w", err)
		}
		// Empty is the NOT NULL default, meaning "not resolved" -> null on the
		// wire, which is what tells a client the install button must be disabled.
		if digest != "" {
			d := digest
			ci.RegistryDigest = &d
		}
		if contextSHA != "" { // same empty-means-unresolved convention as registry_digest
			c := contextSHA
			ci.ContextSHA = &c
		}
		ci.Artwork = normalizeJSONObject(artwork) // defensive: a row could be written some other way than upsert
		if a, ok := installed[ci.ID]; ok {
			ci.Installed = true
			iv := a.Version
			ci.InstalledVersion = &iv
			ci.Pinned = a.Pinned
			ci.Lazy = a.Lazy
			ci.RuntimePresetID = a.RuntimePresetID
			if a.LocalTag != "" {
				lt := a.LocalTag
				ci.LocalTag = &lt
			}
			// Reported regardless of pin state: a pinned image's update IS
			// available, just not taken — hiding that would look like "up to date".
			ci.UpdateAvailable = a.Version != ci.Version
		}
		ci.Hosts = hostStates[ci.ID]
		if ci.Hosts == nil {
			ci.Hosts = []ImageHostState{}
		}
		images = append(images, ci)

		if maxManifestVer == nil || manifestVer > *maxManifestVer {
			v := manifestVer
			maxManifestVer = &v
		}
		if maxFetchedAt == nil || fetchedAt.After(*maxFetchedAt) {
			t := fetchedAt
			maxFetchedAt = &t
		}
	}
	if err := rows.Err(); err != nil {
		return Envelope{}, fmt.Errorf("iterate image_catalog: %w", err)
	}
	if images == nil {
		images = []CatalogImage{}
	}

	// fetched_at/sync_error come from instance_settings (migration 0056), so
	// they survive a restart. Per-row max(fetched_at) is the fallback for a
	// catalog populated before 0056 (image_synced_at NULL there).
	syncErr, syncedAt := s.syncState(ctx)
	if syncedAt == nil {
		syncedAt = maxFetchedAt
	}

	return Envelope{
		ManifestVersion:    maxManifestVer,
		CatalogRef:         ref,
		FetchedAt:          syncedAt,
		SyncError:          syncErr,
		ManifestProvenance: s.manifestProvenance(ctx),
		Images:             images,
	}, nil
}
