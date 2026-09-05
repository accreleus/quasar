package images

import (
	"context"
	"strings"
	"testing"
)

// TestNewRegistryResolverRefusesThroughOutboundClient pins the production
// wiring (#105): NewRegistryResolver(nil) resolves through internal/outbound,
// so the allowlist is enforced on the host actually dialled, not only on the
// registry named in the ref. Both cases below are refused before any network
// I/O, which is what makes them safe to run offline.
func TestNewRegistryResolverRefusesThroughOutboundClient(t *testing.T) {
	ctx := context.Background()

	// The ref's registry is off the allowlist: refused by the resolver's own
	// pre-flight, exactly as before the extraction.
	t.Setenv("QUASAR_IMAGE_REGISTRY_HOSTS", "ghcr.io")
	_, err := NewRegistryResolver(nil).Resolve(ctx, "docker.io/library/alpine:3")
	if err == nil || !strings.Contains(err.Error(), "allowlist") {
		t.Fatalf("docker.io ref with a ghcr.io-only allowlist: got %v, want an allowlist refusal", err)
	}

	// The ref's registry IS allowlisted, but Docker Hub's API host is not: the
	// outbound client refuses the manifest HEAD to registry-1.docker.io. This is
	// the documented consequence of the allowlist naming hosts, not aliases
	// (docs/configuration.md, QUASAR_IMAGE_REGISTRY_HOSTS).
	t.Setenv("QUASAR_IMAGE_REGISTRY_HOSTS", "docker.io")
	_, err = NewRegistryResolver(nil).Resolve(ctx, "docker.io/library/alpine:3")
	if err == nil || !strings.Contains(err.Error(), "registry-1.docker.io") || !strings.Contains(err.Error(), "allowlist") {
		t.Fatalf("docker.io-only allowlist: got %v, want the registry-1.docker.io allowlist refusal from the outbound client", err)
	}
}
