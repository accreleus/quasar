package audit

// limits_test.go — #1209. The caps count raw bytes; admin_activity's CHECK counts
// the ENCODED row, where Go writes &, <, >, control characters and U+2028/29 as
// escapes of up to six bytes. These pin the part that actually guarantees the
// bound: shrinkToFit measures the finished encoding and cuts the row to fit
// instead of letting it be dropped. The emitters are exercised in their own
// packages (compat, session, crud); this file is the shared rule.

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"
)

// escapeHeavy is every byte Go's encoder rewrites, plus a rune a byte cut must
// not split: & < > " \ a control character and a two-byte é.
const escapeHeavy = "&<>\"\\\x01é"

// decoded marshals b back into a map, as Postgres and every reader will see it.
func decoded(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("details are not a JSON object: %v (%s)", err, b)
	}
	return m
}

func TestShrinkToFitKeepsAnEscapeHeavyRowUnderTheEncodedBound(t *testing.T) {
	// 1600 raw bytes each, about 6000 encoded each: far past the bound even
	// though a raw-byte cap of 512 per field would have looked safe.
	long := strings.Repeat(escapeHeavy, 200)
	details := map[string]any{
		"name":   long,
		"reason": long,
		"tag":    "GE-Proton11-12",
		"count":  float64(3),
	}

	b, err := boundedDetails(details)
	if err != nil {
		t.Fatalf("an oversized row must be written, not refused: %v", err)
	}
	if n := renderedSize(b); n > maxDetailBytes {
		t.Fatalf("row renders to %d bytes, over the %d-byte bound", n, maxDetailBytes)
	}
	got := decoded(t, b)
	if got[detailsTruncatedKey] != true {
		t.Errorf("row is not marked %s: %#v", detailsTruncatedKey, got)
	}
	// The short, load-bearing fields survive untouched.
	if got["tag"] != "GE-Proton11-12" || got["count"] != float64(3) {
		t.Errorf("short fields were cut: %#v", got)
	}
	// The long ones give way, say so, and remain valid text.
	for _, key := range []string{"name", "reason"} {
		s, ok := got[key].(string)
		if !ok {
			t.Fatalf("%s is not text after the cut: %#v", key, got[key])
		}
		if got[key+"_truncated"] != true {
			t.Errorf("%s was cut but not flagged: %#v", key, got)
		}
		if !utf8.ValidString(s) {
			t.Errorf("%s was cut mid-rune", key)
		}
		if len(s) >= len(long) {
			t.Errorf("%s is %d bytes, want it cut", key, len(s))
		}
	}
	// The caller's map is untouched: emitters reuse theirs.
	if len(details["name"].(string)) != len(long) {
		t.Error("shrinkToFit mutated the caller's details")
	}
}

func TestShrinkToFitCutsAnEscapeHeavyNameList(t *testing.T) {
	names := make([]string, 0, 45)
	for i := 0; i < 45; i++ {
		names = append(names, strings.Repeat(`"`, 100))
	}
	details := map[string]any{"app_count": 45}
	CapIDs(details, "apps", names)

	b, err := boundedDetails(details)
	if err != nil {
		t.Fatalf("an oversized list must not refuse the row: %v", err)
	}
	if n := renderedSize(b); n > maxDetailBytes {
		t.Fatalf("row renders to %d bytes, over the %d-byte bound", n, maxDetailBytes)
	}
	got := decoded(t, b)
	list, ok := got["apps"].([]any)
	if !ok || len(list) == 0 || len(list) >= 45 {
		t.Fatalf("apps = %#v, want a short non-empty list inside the bound", got["apps"])
	}
	if got["apps_truncated"] != true || got[detailsTruncatedKey] != true {
		t.Errorf("list was cut but not flagged: %#v", got)
	}
	if got["app_count"] != float64(45) {
		t.Errorf("the true count was lost: %#v", got)
	}
}

// A value that is not text must never be written through as free text: a
// structure can hide kilobytes of nested string that no raw-byte cap sees.
func TestCapTextDropsAStructuredValue(t *testing.T) {
	nested := map[string]any{"nested": strings.Repeat("&", 4000)}

	details := map[string]any{}
	CapText(details, "reason", nested)
	if _, written := details["reason"]; written {
		t.Error("a structured reason was written as text")
	}
	if details["reason_invalid"] != true {
		t.Errorf("structured reason not flagged: %#v", details)
	}

	// Absent is not invalid: nothing is written and nothing is flagged.
	details = map[string]any{}
	CapText(details, "reason", nil)
	if len(details) != 0 {
		t.Errorf("an absent reason produced %#v", details)
	}

	// A structure must also not survive because the caps left it alone.
	b, err := boundedDetails(map[string]any{"reason": nested})
	if err != nil {
		t.Fatalf("unexpected refusal: %v", err)
	}
	if n := renderedSize(b); n > maxDetailBytes {
		t.Fatalf("row renders to %d bytes, over the bound", n)
	}
}

// #1209 A3: jsonb refuses \u0000 outright, so one NUL anywhere in a detail made
// the insert fail and the row was lost — independently of its size. The byte is
// replaced, at any depth, and the row says so.
func TestSanitizeDetailsReplacesNULs(t *testing.T) {
	nested := map[string]any{"note": "a\x00b"}
	details := map[string]any{
		"reason": "said \x00 softly",
		"ids":    []string{"ok", "b\x00d"},
		"items":  []any{"fine", nested},
		"count":  4,
		"flag":   true,
	}

	clean, changed := sanitizeDetails(details)
	if !changed {
		t.Fatal("a NUL was not reported as sanitized")
	}
	if clean[detailsSanitizedKey] != true {
		t.Errorf("row is not marked %s", detailsSanitizedKey)
	}
	for _, key := range []string{"reason", "ids", "items"} {
		if clean[key+detailsSanitizedSuffix] != true {
			t.Errorf("%s was sanitized but not flagged", key)
		}
	}
	if clean["count"] != 4 || clean["flag"] != true {
		t.Errorf("clean fields changed: %#v", clean)
	}
	if s := clean["reason"].(string); strings.ContainsRune(s, 0) || !strings.ContainsRune(s, replacementRune) {
		t.Errorf("reason = %q, want the NUL replaced", s)
	}
	if s := clean["ids"].([]string)[1]; strings.ContainsRune(s, 0) || !strings.ContainsRune(s, replacementRune) {
		t.Errorf("list entry = %q, want the NUL replaced", s)
	}
	inner := clean["items"].([]any)[1].(map[string]any)["note"].(string)
	if strings.ContainsRune(inner, 0) || !strings.ContainsRune(inner, replacementRune) {
		t.Errorf("nested value = %q, want the NUL replaced", inner)
	}
	// The caller's map is untouched: emitters reuse theirs.
	if !strings.ContainsRune(details["reason"].(string), 0) {
		t.Error("sanitizeDetails mutated the caller's details")
	}

	// Nothing to sanitize is the identity, and allocates no new flags.
	other := map[string]any{"reason": "plain"}
	if same, changed := sanitizeDetails(other); changed || len(same) != 1 {
		t.Errorf("a clean row changed: %#v", same)
	}
}

// The last resort keeps the counts and flags: they are what still says what
// happened, and they are not what made the row big.
func TestShrinkToFitKeepsCountsWhenOnlyScalarsRemain(t *testing.T) {
	details := map[string]any{}
	for i := 0; i < 600; i++ {
		details[fmt.Sprintf("k%03d", i)] = i
	}

	b, err := boundedDetails(details)
	if err != nil {
		t.Fatalf("unexpected refusal: %v", err)
	}
	if n := renderedSize(b); n > maxDetailBytes {
		t.Fatalf("row renders to %d bytes, over the %d-byte bound", n, maxDetailBytes)
	}
	got := decoded(t, b)
	if got[detailsTruncatedKey] != true {
		t.Errorf("row is not marked %s", detailsTruncatedKey)
	}
	kept := 0
	for k := range got {
		if !auxiliaryKey(k) {
			kept++
		}
	}
	if kept < 1 {
		t.Errorf("the fallback kept no counts at all: %#v", got)
	}
}

// A one-entry list is an id or a name; half of one is a different id. The list
// empties instead, and the flag says so.
func TestShrinkValueNeverFragmentsAListEntry(t *testing.T) {
	one := []string{"a-fairly-long-identifier-that-would-halve"}
	got, ok := shrinkValue(one)
	if !ok || len(got.([]string)) != 0 {
		t.Fatalf("shrinkValue(%v) = %#v, want the list emptied", one, got)
	}

	many := []string{"one", "two", "three", "four"}
	got, ok = shrinkValue(many)
	if !ok {
		t.Fatal("a four-entry list did not shrink")
	}
	for _, s := range got.([]string) {
		if s != "one" && s != "two" {
			t.Errorf("shrinkValue fragmented an entry: %#v", got)
		}
	}
}
