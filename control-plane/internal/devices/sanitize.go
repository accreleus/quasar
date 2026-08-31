package devices

import (
	"encoding/json"
	"math"
)

// AS10-08 — capability payload sanitization.
//
// user_devices.capabilities is schema-free (schema.md §user_devices) but not
// unbounded: bound strings, validate the one modelled field (client_type),
// drop structural junk; unmodelled fields pass through verbatim within the
// generic bounds, preserving the opaque round-trip the AS10-02 reader depends
// on. Complements the handler's 8 KB MaxBytesReader: the cap stops a huge
// body, this stops a small-but-abusive one (a 7 KB string, a 200-deep nest).

const (
	// maxStringLen bounds any string value (and object key) in the blob. Generous
	// for a UA string / version / platform; small enough that no single field can
	// dominate the row.
	maxStringLen = 512
	// maxDepth bounds JSON nesting. The certification record is shallow
	// (capabilities → profiles → <id> → metrics); this leaves ample headroom while
	// rejecting pathological nesting.
	maxDepth = 8
	// maxArrayLen / maxObjectKeys bound fan-out at any level.
	maxArrayLen   = 64
	maxObjectKeys = 64
)

// knownClientTypes is the closed set for client_type. Unrecognised values are
// normalised to "web", never rejected — sanitization is corrective, so a
// client type the server has not learned yet still stores a clean blob.
var knownClientTypes = map[string]bool{
	"web":    true,
	"native": true,
}

// sanitizeCapabilities parses the raw capabilities blob and returns a cleaned copy.
// A nil/empty input yields an empty object. Invalid JSON yields an error so the
// handler can reject it (the handler already shape-checks, so this is defensive).
//
// The cleaned blob: every string ≤ maxStringLen, every object ≤ maxObjectKeys and
// every array ≤ maxArrayLen (excess dropped), nesting ≤ maxDepth (deeper subtrees
// dropped), non-finite numbers dropped, and a modelled client_type normalised to a
// known value. measured_at is NOT touched here — the store stamps it afterwards.
func sanitizeCapabilities(raw json.RawMessage) (json.RawMessage, error) {
	if len(raw) == 0 {
		return json.RawMessage("{}"), nil
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, err
	}
	cleaned, _ := sanitizeValue(v, 0)
	// The top level must be an object (the handler enforces this too).
	m, ok := cleaned.(map[string]any)
	if !ok {
		m = map[string]any{}
	}
	// Normalise the one field we model: client_type.
	if ct, present := m["client_type"]; present {
		s, isStr := ct.(string)
		if !isStr || !knownClientTypes[s] {
			m["client_type"] = "web"
		}
	}
	// AS10-12: report_version must be a whole number (how JSON integers decode);
	// anything else is dropped while the rest of the blob still stores. The other
	// native sub-objects flow through sanitizeValue with no special handling.
	if rv, present := m["report_version"]; present {
		f, isNum := rv.(float64)
		if !isNum || f != math.Trunc(f) {
			delete(m, "report_version")
		}
	}
	return json.Marshal(m)
}

// sanitizeValue recursively cleans one decoded JSON value. The bool reports
// whether the value survived (false → caller should drop it, e.g. a non-finite
// number or a subtree past maxDepth).
func sanitizeValue(v any, depth int) (any, bool) {
	if depth > maxDepth {
		return nil, false
	}
	switch t := v.(type) {
	case string:
		return clampString(t), true
	case float64:
		// json.Unmarshal decodes all JSON numbers to float64. Drop NaN/Inf, which
		// json.Marshal cannot represent anyway (it would error on the whole blob).
		if math.IsNaN(t) || math.IsInf(t, 0) {
			return nil, false
		}
		return t, true
	case bool:
		return t, true
	case nil:
		return nil, true
	case map[string]any:
		out := make(map[string]any, len(t))
		n := 0
		for _, k := range sortedKeys(t) {
			if n >= maxObjectKeys {
				break
			}
			cv, keep := sanitizeValue(t[k], depth+1)
			if !keep {
				continue
			}
			out[clampString(k)] = cv
			n++
		}
		return out, true
	case []any:
		out := make([]any, 0, len(t))
		for _, e := range t {
			if len(out) >= maxArrayLen {
				break
			}
			cv, keep := sanitizeValue(e, depth+1)
			if !keep {
				continue
			}
			out = append(out, cv)
		}
		return out, true
	default:
		// Unknown concrete type (shouldn't occur from encoding/json) — drop it.
		return nil, false
	}
}

// clampString truncates s to maxStringLen runes (not bytes) so a multi-byte
// truncation never splits a rune.
func clampString(s string) string {
	if len(s) <= maxStringLen {
		return s
	}
	r := []rune(s)
	if len(r) <= maxStringLen {
		return s
	}
	return string(r[:maxStringLen])
}

// sortedKeys returns the map's keys in a deterministic order so sanitization is
// reproducible (which key survives a maxObjectKeys truncation must not depend on
// Go's randomised map iteration).
func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sortStrings(keys)
	return keys
}

// sortStrings is a tiny insertion sort to avoid importing sort for one call site.
// (Slices here are bounded by the JSON object width, so this is fine.)
func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j-1] > s[j]; j-- {
			s[j-1], s[j] = s[j], s[j-1]
		}
	}
}
