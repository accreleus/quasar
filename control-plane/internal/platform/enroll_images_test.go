package platform

import (
	"context"
	"encoding/json"
	"testing"
)

// Add host installs the installed release's own recovery-actor (seed) and node-agent
// images by default.

func TestEnrollImagesOfAFormat2Manifest(t *testing.T) {
	m, err := ParseManifest([]byte(v2Fixture(t)))
	if err != nil {
		t.Fatal(err)
	}
	seed, agent, ok := EnrollImagesOf(m)
	if !ok ||
		seed != "ghcr.io/accreleus/quasar/quasar-recovery@sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd" ||
		agent != "ghcr.io/accreleus/quasar/quasar-node-agent@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb" {
		t.Fatalf("seed %q agent %q ok %v", seed, agent, ok)
	}
	v1, err := ParseManifest([]byte(goodManifest))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, ok := EnrollImagesOf(v1); ok {
		t.Error("a format-1 manifest names no recovery actor, so it names no seed")
	}
}

func TestInstalledEnrollImagesReadsTheReleaseTheControlPlaneRuns(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()
	store := NewStore(pool)
	m, err := ParseManifest([]byte(v2Fixture(t)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpsertRelease(ctx, Release{
		Channel: ChannelStable, Version: str(m.Version), SourceCommit: m.SourceCommit,
		BuiltAt: m.BuiltAtTime(), SchemaVersion: m.SchemaVersion, Manifest: json.RawMessage(v2Fixture(t)),
	}); err != nil {
		t.Fatalf("seed release: %v", err)
	}

	seed, agent, ok, err := store.InstalledEnrollImages(ctx, m.SourceCommit)
	if err != nil || !ok {
		t.Fatalf("installed release: %v, ok %v", err, ok)
	}
	wantSeed, wantAgent, _ := EnrollImagesOf(m)
	if seed != wantSeed || agent != wantAgent {
		t.Errorf("seed %q agent %q", seed, agent)
	}

	// A build this instance knows no release for (a branch build) has no default.
	if _, _, ok, err := store.InstalledEnrollImages(ctx, commitA); err != nil || ok {
		t.Errorf("unknown commit: ok %v err %v", ok, err)
	}
}
