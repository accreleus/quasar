package platform

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// The images Add host installs by default: the installed release's own (#359 review on
// #365), so a new host starts on the control plane's release. QUASAR_ENROLL_SEED_IMAGE /
// QUASAR_ENROLL_AGENT_IMAGE override them field by field (enrollscript.Pins.Or).

// EnrollImagesOf is the seed image (the seed is a mode of the recovery-actor image, ADR
// 0007) and the node-agent image a format-2 manifest names, each image@digest. ok=false
// for a format-1 manifest, which names no recovery actor.
func EnrollImagesOf(m Manifest) (seed, agent string, ok bool) {
	if m.FormatVersion != ManifestFormat2 {
		return "", "", false
	}
	for _, c := range m.Components {
		switch c.Name {
		case ComponentRecovery:
			seed = c.Image + "@" + c.Digest
		case ComponentNodeAgent:
			agent = c.Image + "@" + c.Digest
		}
	}
	return seed, agent, seed != "" && agent != ""
}

// InstalledEnrollImages reads the stable release this control plane was built from (by
// its commit) and returns its EnrollImagesOf. ok=false, with no error, when no such
// release has been detected with a format-2 manifest: a branch or edge build, or a
// format-1 release.
func (s *Store) InstalledEnrollImages(ctx context.Context, commit string) (seed, agent string, ok bool, err error) {
	var raw []byte
	err = s.pool.QueryRow(ctx, `
		SELECT manifest FROM platform_releases
		 WHERE channel = 'stable' AND source_commit = $1 AND manifest IS NOT NULL
		 ORDER BY discovered_at DESC LIMIT 1`, commit).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", false, nil
	}
	if err != nil {
		return "", "", false, fmt.Errorf("read installed release: %w", err)
	}
	m, err := ParseManifest(raw)
	if err != nil {
		// Stored manifests were validated on detection; one that no longer parses
		// names nothing Add host may install.
		return "", "", false, nil
	}
	seed, agent, ok = EnrollImagesOf(m)
	return seed, agent, ok, nil
}
