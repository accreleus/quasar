package audit

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// An admin audit row's details must render inside admin_activity's 4096-byte
// CHECK (migration 0028). The caps here are the cheap first pass for the fields
// known to grow (ids repeated per host, agent-reported text); they keep a row
// small in the ordinary case. They cannot be the GUARANTEE: they measure raw
// bytes while the CHECK measures the encoded row, and they see one field at a
// time. shrinkToFit, at the bottom of this file, is the guarantee: it measures
// the finished encoding and writes the row cut down rather than dropped
// (erratum 25 on quasar-protocol#29, #1209).
const (
	// MaxListIDs is the most entries one details list carries (erratum 25 on
	// quasar-protocol#29). The byte cost of the entries is shrinkToFit's job:
	// names are not fixed-width, so a count alone cannot bound the row.
	MaxListIDs = 40
	// MaxTextBytes bounds one free-text field. It counts BYTES, like the CHECK,
	// so a cut lands on a UTF-8 boundary rather than mid-rune.
	MaxTextBytes = 512
)

// CapIDs sets details[key] to ids, cut to MaxListIDs entries with
// key+"_truncated" past that. Only whole entries are kept: half a name would
// read as a different one.
//
// A nil list is written as [] rather than null, so a reader can index it
// without a guard. The caller records the true count itself, since only it
// knows what the list is.
func CapIDs(details map[string]any, key string, ids []string) {
	if ids == nil {
		ids = []string{}
	}
	if len(ids) > MaxListIDs {
		ids, details[key+"_truncated"] = ids[:MaxListIDs], true
	}
	details[key] = ids
}

// CapText sets details[key] to v, cut at MaxTextBytes with key+"_truncated"
// when it had to cut.
//
// Only a string is text. A value that arrived as an object, an array or a
// number is NOT written through: a structure can hide kilobytes of nested
// string the raw-byte cap never sees (#1209, reviewer A2), and an operator
// reading it as free text would be reading something the reporter did not
// say. It is dropped and marked key+"_invalid" instead.
func CapText(details map[string]any, key string, v any) {
	s, ok := v.(string)
	if !ok {
		if v != nil {
			details[key+"_invalid"] = true
		}
		return
	}
	if len(s) > MaxTextBytes {
		s, details[key+"_truncated"] = TruncateBytes(s, MaxTextBytes), true
	}
	details[key] = s
}

// detailsTruncatedKey marks a row the encoded-size step had to cut (#1209).
const detailsTruncatedKey = "details_truncated"

// detailsSanitizedKey and detailsSanitizedSuffix flag a row whose text carried
// a byte jsonb cannot store (see sanitizeDetails).
const (
	detailsSanitizedKey    = "details_sanitized"
	detailsSanitizedSuffix = "_sanitized"
)

// replacementRune stands in for a NUL byte in reported text.
//
// Postgres' jsonb refuses \u0000 outright, whatever the row weighs, so a single
// NUL in an agent's reason made the insert fail and TryRecord only log it: a
// second way for an audit row to be lost, independent of the size bound (#1209,
// reviewer A3). The byte is REPLACED rather than dropped, because dropping it
// would silently join the text either side of it; U+FFFD is what a reader should
// see where the reporter's text held something no text encoding has room for.
const replacementRune = '\uFFFD'

// sanitizeDetails makes details safe to store: every NUL byte, at any depth, is
// replaced with U+FFFD. It flags the row and each top-level field it touched, so
// a reader knows the text is not byte-for-byte what was reported.
//
// It returns the caller's map and false when there was nothing to sanitize, so
// the ordinary path allocates nothing and never mutates an emitter's map.
func sanitizeDetails(details map[string]any) (map[string]any, bool) {
	var (
		out     map[string]any
		touched bool
	)
	for k, v := range details {
		clean, changed := sanitizeValue(v)
		if !changed {
			continue
		}
		if out == nil {
			out = make(map[string]any, len(details)+1)
			for k2, v2 := range details {
				out[k2] = v2
			}
		}
		out[k] = clean
		out[k+detailsSanitizedSuffix] = true
		touched = true
	}
	if !touched {
		return details, false
	}
	out[detailsSanitizedKey] = true
	return out, true
}

// sanitizeValue replaces the NULs in one value: a string, a list of them, or a
// nested object. A shape it does not know is left exactly as the emitter wrote
// it — the size step is what bounds that.
func sanitizeValue(v any) (any, bool) {
	switch t := v.(type) {
	case string:
		if !strings.ContainsRune(t, 0) {
			return v, false
		}
		return strings.ReplaceAll(t, "\x00", string(replacementRune)), true
	case []string:
		out := make([]string, len(t))
		changed := false
		for i, s := range t {
			if clean, ok := sanitizeValue(s); ok {
				out[i], changed = clean.(string), true
			} else {
				out[i] = s
			}
		}
		if !changed {
			return v, false
		}
		return out, true
	case []any:
		out := make([]any, len(t))
		changed := false
		for i, item := range t {
			if clean, ok := sanitizeValue(item); ok {
				out[i], changed = clean, true
			} else {
				out[i] = item
			}
		}
		if !changed {
			return v, false
		}
		return out, true
	case map[string]any:
		out := make(map[string]any, len(t))
		changed := false
		for k, item := range t {
			if clean, ok := sanitizeValue(item); ok {
				out[k], changed = clean, true
			} else {
				out[k] = item
			}
		}
		if !changed {
			return v, false
		}
		return out, true
	}
	return v, false
}

// cutMarker stands in for a value too large to keep and not shrinkable in place.
const cutMarker = "[cut]"

// shrinkRounds bounds the cutting loop. Each round at least halves one field, so
// a megabyte of text converges in far fewer; this is only a guard.
const shrinkRounds = 128

// shrinkToFit returns details encoded small enough that Postgres' jsonb
// rendering stays inside maxDetailBytes, cutting free text and list entries
// until it does and marking the row details_truncated.
//
// The CHECK is on the ENCODED row, and encoding is not a byte-for-byte copy: Go
// writes &, <, >, U+2028, U+2029 and control characters as escape sequences of
// up to six bytes, and quotes and backslashes as two. Per-field caps measured in
// raw bytes therefore cannot guarantee the row, and independent caps ignore the
// fields (and the _truncated/count keys) beside them. So the guarantee is
// measured here, on the same marshalling and renderedSize the bound uses.
//
// The longest field gives way first, so a big free-text value is cut before a
// short id or a count ever is, and every field cut is marked
// key+"_truncated". The row is never refused: the action, the target and
// whatever detail remains still say what happened, and a lossy audit row beats
// no audit row at all.
func shrinkToFit(details map[string]any) ([]byte, error) {
	// Never mutate the caller's map: emitters reuse theirs, and the flags set
	// here belong to the copy being written.
	work := make(map[string]any, len(details)+1)
	for k, v := range details {
		work[k] = v
	}
	work[detailsTruncatedKey] = true

	for round := 0; round < shrinkRounds; round++ {
		b, err := json.Marshal(work)
		if err != nil {
			return nil, fmt.Errorf("marshal audit details: %w", err)
		}
		if renderedSize(b) <= maxDetailBytes {
			return b, nil
		}
		key, next, ok := biggestShrinkable(work)
		if !ok {
			// Nothing left that halves: replace the largest value of a type this
			// cannot split (a map, say) with a marker, so the rest of the row survives.
			if key, ok = biggestCollection(work); !ok {
				break
			}
			next = cutMarker
		}
		work[key] = next
		work[key+"_truncated"] = true
	}

	// Nothing left to give way and the row still does not fit. That is reachable
	// only if the row is made of many small fixed-width fields — every number and
	// boolean here is one — so keep those and drop only the content that could not
	// be cut in place. They are what still says what happened.
	minimal := map[string]any{detailsTruncatedKey: true}
	scalars := make([]string, 0, len(work))
	for k, v := range work {
		if !auxiliaryKey(k) && isScalar(v) {
			scalars = append(scalars, k)
		}
	}
	sort.Strings(scalars) // stable output for the same input
	for _, k := range scalars {
		minimal[k] = work[k]
		b, err := json.Marshal(minimal)
		if err != nil || renderedSize(b) > maxDetailBytes {
			delete(minimal, k)
		}
	}
	b, err := json.Marshal(minimal)
	if err != nil {
		return nil, fmt.Errorf("marshal audit details: %w", err)
	}
	return b, nil
}

// isScalar reports a value with no length to cut: a count, a flag, an id-like
// number. These survive to the last resort above.
func isScalar(v any) bool {
	switch v.(type) {
	case bool, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, float32, float64, json.Number:
		return true
	}
	return false
}

// biggestShrinkable picks the largest field that can still give way, so a cut
// always lands on the field costing the row the most bytes.
func biggestShrinkable(work map[string]any) (string, any, bool) {
	var (
		key  string
		next any
		size = -1
	)
	for k, v := range work {
		if auxiliaryKey(k) {
			continue
		}
		smaller, ok := shrinkValue(v)
		if !ok {
			continue
		}
		if n := encodedSize(v); n > size {
			key, next, size = k, smaller, n
		}
	}
	return key, next, size >= 0
}

// biggestCollection picks the largest field shrinkValue cannot split, for the
// last resort above.
func biggestCollection(work map[string]any) (string, bool) {
	var (
		key  string
		size = len(cutMarker) + 2 // replacing a value this small would only grow the row
	)
	for k, v := range work {
		if auxiliaryKey(k) {
			continue
		}
		if n := encodedSize(v); n > size {
			key, size = k, n
		}
	}
	return key, size > len(cutMarker)+2
}

// auxiliaryKey reports a key the auditor maintains itself, which must never be
// cut or counted as content.
func auxiliaryKey(k string) bool {
	return k == detailsTruncatedKey || k == detailsSanitizedKey ||
		strings.HasSuffix(k, "_truncated") || strings.HasSuffix(k, "_invalid") ||
		strings.HasSuffix(k, detailsSanitizedSuffix)
}

// encodedSize is what a value costs in the encoded row, which is how the CHECK
// measures it.
func encodedSize(v any) int {
	b, err := json.Marshal(v)
	if err != nil {
		return 0
	}
	return len(b)
}

// shrinkValue returns v cut roughly in half, and false when there is nothing
// left to take. A list never yields a fragment of an entry: an id cut in half
// is a different id, so a one-entry list is emptied instead (the flag and the
// caller's count still say it was cut).
func shrinkValue(v any) (any, bool) {
	switch t := v.(type) {
	case string:
		if t == "" {
			return nil, false
		}
		return TruncateBytes(t, len(t)/2), true
	case []string:
		if len(t) == 0 {
			return nil, false
		}
		if len(t) == 1 {
			return t[:0], true
		}
		return t[:len(t)/2], true
	case []any:
		if len(t) == 0 {
			return nil, false
		}
		if len(t) == 1 {
			return t[:0], true
		}
		return t[:len(t)/2], true
	}
	return nil, false
}
