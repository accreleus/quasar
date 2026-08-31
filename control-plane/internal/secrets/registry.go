package secrets

import "sort"

// Descriptor declares one secret this build knows about: its stable name, how
// to describe it to an operator, and the environment variable that still works
// as a fallback.
//
// WHY A REGISTRY AT ALL. Without one, PUT /v1/admin/secrets/{name} is an
// arbitrary admin-writable key/value store — an admin could fill the table with
// names nothing reads, and the UI would have nothing to render. With one, the
// admin surface is derived: a future feature adds a Descriptor and gets an API
// row, a UI row, an env fallback and masking with no new endpoint and no new
// component.
type Descriptor struct {
	// Name is the stable identifier used in code, on the wire and as the primary
	// key. Convention is dotted and namespaced by feature:
	// "artwork.steamgriddb.api_key".
	Name string
	// Label is the operator-facing name ("SteamGridDB API key").
	Label string
	// Description explains what setting it enables, in one sentence.
	Description string
	// EnvVar is the environment variable that supplies the same credential, or
	// "" when there is none. It is a FALLBACK, not an alternative source of
	// truth: a stored secret takes precedence (see Store.Resolve).
	EnvVar string
	// DocsURL points at the operator documentation for this credential.
	DocsURL string
}

// Registry is the set of declared secrets. Immutable after construction, so it
// is safe to share across goroutines with no locking.
type Registry struct {
	byName map[string]Descriptor
	order  []string
}

// NewRegistry builds a registry. A duplicate name is a programming error and
// the last declaration wins; there is no runtime path that can produce one.
func NewRegistry(ds ...Descriptor) *Registry {
	r := &Registry{byName: make(map[string]Descriptor, len(ds))}
	for _, d := range ds {
		if _, seen := r.byName[d.Name]; !seen {
			r.order = append(r.order, d.Name)
		}
		r.byName[d.Name] = d
	}
	sort.Strings(r.order)
	return r
}

// Lookup returns the descriptor for a name.
func (r *Registry) Lookup(name string) (Descriptor, bool) {
	if r == nil {
		return Descriptor{}, false
	}
	d, ok := r.byName[name]
	return d, ok
}

// All returns every declared descriptor, name-ordered.
func (r *Registry) All() []Descriptor {
	if r == nil {
		return nil
	}
	out := make([]Descriptor, 0, len(r.order))
	for _, n := range r.order {
		out = append(out, r.byName[n])
	}
	return out
}

// Names for the artwork consumer. Declared here rather than in internal/artwork
// so the registry wiring in main has one import and the name string exists in
// exactly one place.
const (
	// NameArtworkAPIKey is the third-party cover-artwork provider credential
	// (SteamGridDB today). Env fallback: QUASAR_STEAMGRIDDB_API_KEY.
	NameArtworkAPIKey = "artwork.steamgriddb.api_key"
)

// DefaultRegistry declares every secret this build supports. Add a Descriptor
// here and the admin API + UI pick it up with no other change.
func DefaultRegistry() *Registry {
	return NewRegistry(Descriptor{
		Name:  NameArtworkAPIKey,
		Label: "SteamGridDB API key",
		Description: "Lets the control plane look up cover artwork for apps in your catalogue. " +
			"Without it, artwork can still be uploaded by hand and every app falls back to its gradient tile.",
		EnvVar:  "QUASAR_STEAMGRIDDB_API_KEY",
		DocsURL: "https://www.steamgriddb.com/profile/preferences/api",
	})
}
