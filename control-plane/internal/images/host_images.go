package images

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// dbExecutor is the narrow surface both *pgxpool.Pool and pgx.Tx satisfy, so
// register reconciliation (ensure.go) can run its upsert-loop-plus-demote
// sequence inside one transaction while every other caller keeps passing the
// pool directly.
type dbExecutor interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// host_images persistence, shared by the Ensurer (writer, from image_state and
// register reconciliation) and Store.Envelope (reader, hosts[]).
//
// Every write is guarded by `WHERE EXISTS (SELECT 1 FROM image_catalog …)`
// rather than catching the FK violation: agent-api.md says an unknown
// image_id is dropped, not stored, and an FK error would need string-matching
// to tell apart from a real DB failure.

// hostImageStates are the states host_images accepts (migration 0055 CHECK).
// Anything else is dropped rather than surfacing as a DB error. `building` is
// accepted for the P4 template-build amendment even though no P2 agent sends it.

// maxStoredImageError bounds any error message written into host_images.error,
// independent of any bound the agentws wire layer applies: sendEnsure's
// ack{ok:false} path reads res.Error from our own ack decode, which never
// passes through the handler's image_state validation.
const maxStoredImageError = 300

func truncateImageError(s string) string {
	if len(s) <= maxStoredImageError {
		return s
	}
	return s[:maxStoredImageError]
}

var hostImageStates = map[string]bool{
	"absent":   true,
	"pulling":  true,
	"building": true,
	"ready":    true,
	"failed":   true,
}

// upsertHostImage records one (host, image) presence report. bytes is a
// pointer so "unknown" stays NULL, never a fabricated 0. Returns whether a row
// was actually written — false means the image_id isn't in image_catalog and
// the report was dropped.
//
// bytes is state-dependent, not a flat COALESCE: 'absent' clears it to NULL (a
// prior download's size is now wrong, not merely unknown); 'ready' stores
// exactly what was reported, NULL if the agent sent none, rather than
// carrying forward a stale mid-pull figure; every other state COALESCEs so an
// omitted bytes on a progress report doesn't erase the last known figure.
func upsertHostImage(ctx context.Context, db dbExecutor, hostID, imageID, version, state, errMsg string, bytes *int64) (bool, error) {
	tag, err := db.Exec(ctx, `
		INSERT INTO host_images (host_id, image_id, version, state, error, bytes, updated_at)
		SELECT $1::uuid, $2, $3, $4, $5, $6, now()
		WHERE EXISTS (SELECT 1 FROM image_catalog WHERE id = $2)
		ON CONFLICT (host_id, image_id) DO UPDATE SET
			version    = EXCLUDED.version,
			state      = EXCLUDED.state,
			error      = EXCLUDED.error,
			bytes      = CASE
				WHEN EXCLUDED.state = 'absent' THEN NULL
				WHEN EXCLUDED.state = 'ready'  THEN EXCLUDED.bytes
				ELSE COALESCE(EXCLUDED.bytes, host_images.bytes)
			END,
			updated_at = now()
	`, hostID, imageID, version, state, errMsg, bytes)
	if err != nil {
		return false, fmt.Errorf("upsert host_images host=%s image=%s: %w", hostID, imageID, err)
	}
	return tag.RowsAffected() > 0, nil
}

// demoteUnreportedReady: an image this host was believed ready on, but its
// wholesale report omits, is flipped to absent — a reconnected agent that lost
// an image must never keep reading as ready, or the scheduler would place a
// session with no image.
//
// Only 'ready' rows are demoted; a 'pulling' row not yet re-reported is left
// alone (already not-ready, and clobbering would erase progress). Version is
// kept — every readiness test is `state='ready' AND version=…`, so a stale
// version on an absent row can't be mistaken for presence.
//
// Returns the demoted ids so the caller can also clear their retry-failure
// counters.
func demoteUnreportedReady(ctx context.Context, db dbExecutor, hostID string, reported []string) ([]string, error) {
	rows, err := db.Query(ctx, `
		UPDATE host_images
		   SET state = 'absent', error = '', updated_at = now()
		 WHERE host_id = $1::uuid
		   AND state = 'ready'
		   AND image_id <> ALL($2::text[])
		RETURNING image_id
	`, hostID, reported)
	if err != nil {
		return nil, fmt.Errorf("demote unreported host_images for host=%s: %w", hostID, err)
	}
	defer rows.Close()
	var demoted []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan demoted host_images row for host=%s: %w", hostID, err)
		}
		demoted = append(demoted, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate demoted host_images for host=%s: %w", hostID, err)
	}
	return demoted, nil
}

// hostHasImage: version equality is part of the test — a host holding the
// previous version must be re-ensured.
func hostHasImage(ctx context.Context, db dbExecutor, hostID, imageID, version string) (bool, error) {
	var ok bool
	if err := db.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM host_images
			WHERE host_id = $1::uuid AND image_id = $2 AND state = 'ready' AND version = $3
		)`, hostID, imageID, version).Scan(&ok); err != nil {
		return false, fmt.Errorf("read host_images host=%s image=%s: %w", hostID, imageID, err)
	}
	return ok, nil
}

// installedImage is one row of the ensure set: an adopted image plus what the
// agent needs to make it present — a registry ref (image_ensure) or a build
// context (image_build). Exactly one of RegistryRef/LocalTag is non-empty for
// any dispatched row; the ensure worker branches on LocalTag, not Kind (Kind
// is for logging only).
type installedImage struct {
	ImageID     string
	Version     string
	RegistryRef string // prebuilt: digest-form ref frozen at adoption
	Kind        string // "prebuilt" | "template", for logging
	LocalTag    string // template: CP-assigned build tag frozen at adoption
	ContextRepo string // template: "owner/name" build-context repo, frozen at adoption
	ContextSHA  string // template: commit sha the build context is pinned to, frozen at adoption
	Dockerfile  string // template: manifest dockerfile path, frozen at adoption
	BuildArgs   json.RawMessage
}

// installedNonLazyQuery is the SELECT installedNonLazy and adoptionFor share.
// Every build-defining input is read from installed_images (frozen at
// adoption), never live image_catalog — the template twin of the #440
// fleet-split bug: a later sync could otherwise rebuild different bits under
// the adopted version. The join supplies only ic.kind, for logging; the build
// never reads the catalog once an adoption exists. An empty frozen ContextSHA
// shouldn't normally occur (Install/Update refuse an unresolved one), but
// every dispatch site treats it as "skip and log", never a panic.
const installedNonLazyQuery = `
	SELECT ii.image_id, ii.version, ii.registry_ref, ic.kind, ii.local_tag,
	       ii.context_repo, ii.context_sha, ii.dockerfile, ii.build_args
	FROM installed_images ii
	JOIN image_catalog ic ON ic.id = ii.image_id`

// scanInstalledImage scans one row of installedNonLazyQuery's column list.
func scanInstalledImage(row pgx.Row) (installedImage, error) {
	var (
		ii        installedImage
		buildArgs []byte
	)
	if err := row.Scan(&ii.ImageID, &ii.Version, &ii.RegistryRef, &ii.Kind, &ii.LocalTag,
		&ii.ContextRepo, &ii.ContextSHA, &ii.Dockerfile, &buildArgs); err != nil {
		return installedImage{}, err
	}
	ii.BuildArgs = buildArgs
	return ii, nil
}

// installedNonLazy lists the images this instance has adopted eagerly. A lazy
// row is excluded (pulled/built on demand), and so is a row with neither a
// concrete registry_ref nor local_tag (a defensive floor — Install refuses an
// unresolved digest/context).
//
// registry_ref/local_tag come from installed_images, not image_catalog: both
// catalog columns move on every sync, but the version dispatched is the
// adopted version frozen at install. Reading the catalog's columns would let a
// sync move the ref out from under an unchanged version, splitting the fleet.
func installedNonLazy(ctx context.Context, db dbExecutor) ([]installedImage, error) {
	rows, err := db.Query(ctx, installedNonLazyQuery+`
		WHERE ii.lazy = false
		  AND (
		        (ii.registry_ref IS NOT NULL AND ii.registry_ref <> '')
		     OR (ii.local_tag IS NOT NULL AND ii.local_tag <> '')
		      )
		ORDER BY ii.image_id
	`)
	if err != nil {
		return nil, fmt.Errorf("query installed_images: %w", err)
	}
	defer rows.Close()
	var out []installedImage
	for rows.Next() {
		ii, err := scanInstalledImage(rows)
		if err != nil {
			return nil, fmt.Errorf("scan installed_images row: %w", err)
		}
		out = append(out, ii)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate installed_images: %w", err)
	}
	return out, nil
}

// adoptionState is what adoptionFor reports about one image's current adoption.
type adoptionState int

const (
	adoptionActive adoptionState = iota // adopted, non-lazy, with a concrete ref to pull
	adoptionLazy                        // adopted but not eagerly pushed (lazy, or no concrete ref)
	adoptionAbsent                      // no installed_images row: desired state is "not present"
)

// adoptionFor reads the current adoption for imageID. The ensure worker calls
// this immediately before dispatching, so an ensure queued behind another op
// reflects the adoption as it is now — an image uninstalled meanwhile
// resolves to adoptionAbsent (becomes a remove), never a resurrection.
func adoptionFor(ctx context.Context, db dbExecutor, imageID string) (installedImage, adoptionState, error) {
	// installedNonLazyQuery doesn't carry `lazy` (already filtered lazy=false);
	// read it separately so a lazy row reports as adoptionLazy with no third
	// query shape.
	var lazy bool
	err := db.QueryRow(ctx, `SELECT lazy FROM installed_images WHERE image_id = $1`, imageID).Scan(&lazy)
	if errors.Is(err, pgx.ErrNoRows) {
		return installedImage{}, adoptionAbsent, nil
	}
	if err != nil {
		return installedImage{}, adoptionAbsent, fmt.Errorf("read adoption lazy image=%s: %w", imageID, err)
	}
	if lazy {
		return installedImage{}, adoptionLazy, nil
	}
	ii, err := scanInstalledImage(db.QueryRow(ctx, installedNonLazyQuery+` WHERE ii.image_id = $1`, imageID))
	if errors.Is(err, pgx.ErrNoRows) {
		return installedImage{}, adoptionAbsent, nil
	}
	if err != nil {
		return installedImage{}, adoptionAbsent, fmt.Errorf("read adoption image=%s: %w", imageID, err)
	}
	if ii.RegistryRef == "" && ii.LocalTag == "" {
		return installedImage{}, adoptionLazy, nil // nothing concrete to dispatch; treat like lazy
	}
	return ii, adoptionActive, nil
}

// newCmdID generates a command id for ack correlation (agent-api.md requires
// only per-connection uniqueness). Returns an error rather than swallowing
// one: an ignored rand.Read failure would yield colliding all-zero ids and
// cross-command ack confusion.
func newCmdID() (string, error) {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate command id: %w", err)
	}
	return hex.EncodeToString(b), nil
}
