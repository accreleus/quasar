package platform

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/accreleus/quasar/control-plane/internal/images"
)

// The one preflight collector that talks to the network: does every component
// manifest of a release resolve at the registry, as seen from the control
// plane (preflight `image_resolvable`)? A GET of the manifest by digest, no
// pull. Cached per release for TTL; Invalidate before an apply decision so a
// stale answer never authorises one.

// DefaultImageCheckTTL bounds how long a registry answer is reused.
const DefaultImageCheckTTL = 10 * time.Minute

// ImageResolver checks a release's digests at the registry.
type ImageResolver struct {
	inspect images.ImageInspector
	// edge resolves a manifest-less release's commit tag, exactly as the apply
	// would; nil refuses an edge release.
	edge ApplyComponentResolver
	ttl  time.Duration

	mu    sync.Mutex
	cache map[string]imageCheckEntry
}

type imageCheckEntry struct {
	fact ImageFact
	at   time.Time
}

// NewImageResolver builds the collector; ttl <= 0 means DefaultImageCheckTTL.
func NewImageResolver(inspect images.ImageInspector, edge ApplyComponentResolver, ttl time.Duration) *ImageResolver {
	if ttl <= 0 {
		ttl = DefaultImageCheckTTL
	}
	return &ImageResolver{inspect: inspect, edge: edge, ttl: ttl, cache: map[string]imageCheckEntry{}}
}

// Invalidate drops every cached answer: called by the detect job (a "Check
// now") and by the apply endpoints before they decide.
func (r *ImageResolver) Invalidate() {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.cache = map[string]imageCheckEntry{}
	r.mu.Unlock()
}

// Check is the fact for one release, from the cache when fresh.
func (r *ImageResolver) Check(ctx context.Context, rel Release) *ImageFact {
	if r == nil || r.inspect == nil {
		return nil
	}
	r.mu.Lock()
	if e, ok := r.cache[rel.ID]; ok && time.Since(e.at) < r.ttl {
		r.mu.Unlock()
		f := e.fact
		return &f
	}
	r.mu.Unlock()

	fact := ImageFact{Err: r.resolve(ctx, rel)}
	r.mu.Lock()
	r.cache[rel.ID] = imageCheckEntry{fact: fact, at: time.Now()}
	r.mu.Unlock()
	return &fact
}

func (r *ImageResolver) resolve(ctx context.Context, rel Release) string {
	var components []ComponentDigest
	if len(rel.Manifest) > 0 {
		m, err := ParseManifest(rel.Manifest)
		if err != nil {
			return "the release manifest does not parse: " + err.Error()
		}
		for _, c := range m.Components {
			components = append(components, ComponentDigest{Name: c.Name, Image: c.Image, Digest: c.Digest})
		}
	}
	if len(components) == 0 {
		// Edge: no manifest, so the commit tag is what the apply would resolve.
		if r.edge == nil {
			return "this release carries no manifest and this control plane cannot reach the registry to resolve one"
		}
		for _, f := range []func(context.Context, Release) (ComponentDigest, error){r.edge.ControlPlaneComponent, r.edge.NodeAgentComponent} {
			if _, err := f(ctx, rel); err != nil {
				return err.Error()
			}
		}
		return ""
	}
	for _, c := range components {
		ref := c.Image + "@" + c.Digest
		cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		_, err := r.inspect.InspectConfig(cctx, ref)
		cancel()
		if err != nil {
			return fmt.Sprintf("%s: %s does not resolve at the registry: %v", c.Name, ref, err)
		}
	}
	return ""
}
