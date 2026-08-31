package images

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// Library-provider auto-ensure (protocol/control-api.md §"P5 side effect").
// Enabling library discovery must guarantee the provider's canonical image and
// runtime preset are present rather than failing later at launch;
// EnsureProviders installs (via the install path — adopt + ensure-everywhere
// + preset materialization) every catalog image declaring a library_provider
// in the local allowlist that isn't already installed.

// defaultLibraryProvider is the sole provider auto-installed when
// QUASAR_LIBRARY_PROVIDERS is unset.
const defaultLibraryProvider = "steam"

// SetProviderAllowlist configures which library_provider names EnsureProviders
// may auto-install, from QUASAR_LIBRARY_PROVIDERS (comma-separated); empty
// falls back to "steam".
//
// The local trust boundary for auto-install: without it, any catalog image
// marking itself a library_provider would be auto-installed on discovery
// enable, so a compromised catalog could mark an arbitrary image as a
// provider and have it pulled fleet-wide.
func (s *Store) SetProviderAllowlist(raw string) {
	s.providerAllowlist = parseProviderAllowlist(raw)
}

// parseProviderAllowlist splits a comma-separated env value into a set of
// trimmed, lower-cased, non-empty provider names, falling back to the default
// when nothing usable is present.
func parseProviderAllowlist(raw string) map[string]bool {
	set := map[string]bool{}
	for _, p := range strings.Split(raw, ",") {
		p = strings.ToLower(strings.TrimSpace(p))
		if p != "" {
			set[p] = true
		}
	}
	if len(set) == 0 {
		set[defaultLibraryProvider] = true
	}
	return set
}

// allowedProviders defaults to {steam} when SetProviderAllowlist was never
// called; always non-empty.
func (s *Store) allowedProviders() map[string]bool {
	if len(s.providerAllowlist) == 0 {
		return map[string]bool{defaultLibraryProvider: true}
	}
	return s.providerAllowlist
}

// EnsureProviders installs every catalog image with a library_provider in the
// local allowlist that has no installed_images row yet, then ensures each
// installed provider has a provider app (#456: the piece P5 shipped without,
// which left an operator enabling Steam discovery with an empty apps table).
// An image outside the allowlist is skipped and logged — the trust decision
// stays local.
//
// The app pass runs for already-installed providers too (an image can predate
// the app), and is itself idempotent (EnsureProviderApp leaves an existing
// app alone).
//
// Idempotent overall; never fatal per-image — an unresolved digest/context
// (e.g. a private GHCR package, #442) is logged and left uninstalled, visible
// as installed=false in GET /v1/admin/images.
//
// Returns an error only for a failure to list the providers at all;
// per-image failures are logged, not returned.
func (s *Store) EnsureProviders(ctx context.Context) error {
	allow := s.allowedProviders()

	// Every provider image, installed or not: the installed ones still need the
	// app pass. `installed` selects which half of the loop body runs.
	rows, err := s.pool.Query(ctx, `
		SELECT ic.id, ic.library_provider, (ii.image_id IS NOT NULL) AS installed
		FROM image_catalog ic
		LEFT JOIN installed_images ii ON ii.image_id = ic.id
		WHERE ic.library_provider IS NOT NULL
		  AND ic.library_provider <> ''
		ORDER BY ic.id
	`)
	if err != nil {
		return fmt.Errorf("list library-provider images: %w", err)
	}
	type candidate struct {
		id, provider string
		installed    bool
	}
	var cands []candidate
	for rows.Next() {
		var c candidate
		if err := rows.Scan(&c.id, &c.provider, &c.installed); err != nil {
			rows.Close()
			return fmt.Errorf("scan library-provider image id: %w", err)
		}
		cands = append(cands, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate library-provider images: %w", err)
	}

	for _, c := range cands {
		if !allow[strings.ToLower(c.provider)] { // local trust boundary: only an operator-listed provider auto-installs
			s.log.Warn("provider auto-ensure: image claims a library_provider not in the local allowlist; skipped (set QUASAR_LIBRARY_PROVIDERS to permit it)",
				"image_id", c.id, "library_provider", c.provider)
			continue
		}
		if !c.installed {
			// Eager (lazy=false): a provider image should land on the fleet on
			// enable, not first launch.
			_, err := s.Install(ctx, c.id, false)
			switch {
			case err == nil:
				s.log.Info("provider auto-ensure: installed library-provider image", "image_id", c.id, "library_provider", c.provider)
			case errors.Is(err, ErrAlreadyInstalled):
				s.log.Debug("provider auto-ensure: image already installed", "image_id", c.id) // concurrent enable/install won the race
			case errors.Is(err, ErrDigestUnresolved), errors.Is(err, ErrContextUnresolved):
				// Unresolved digest/context (private registry, #442) is never fatal
				// to the enable flip — stays uninstalled until a later sync resolves it.
				s.log.Warn("provider auto-ensure: image unresolved; left uninstalled, retry after a successful sync",
					"image_id", c.id, "err", err)
				continue // nothing adopted -> nothing for an app to launch
			default:
				s.log.Error("provider auto-ensure: install failed", "image_id", c.id, "err", err)
				continue
			}
		}

		// #456. Idempotent and non-destructive; never fatal, same reason as install.
		created, err := s.EnsureProviderApp(ctx, c.id, c.provider)
		switch {
		case err != nil:
			s.log.Error("provider auto-ensure: could not create the provider app", "image_id", c.id, "library_provider", c.provider, "err", err)
		case created:
			s.log.Info("provider auto-ensure: created the provider app", "image_id", c.id, "library_provider", c.provider)
		default:
			s.log.Debug("provider auto-ensure: provider app already present or image not yet installed", "image_id", c.id)
		}
	}
	return nil
}
