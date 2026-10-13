package library

import (
	"encoding/json"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
)

type classCase struct{ Message, Class string }

func classFixture(t *testing.T) (classes []string, cases, hostile []classCase) {
	t.Helper()
	raw, err := os.ReadFile("../../../testdata/library/scan-failure-classes.json")
	must(t, err)
	var doc struct {
		Classes        []string
		Cases, Hostile []classCase
	}
	must(t, json.Unmarshal(raw, &doc))
	return doc.Classes, doc.Cases, doc.Hostile
}

func TestScanFailureClassCoversTheSharedFixture(t *testing.T) {
	classes, cases, _ := classFixture(t)
	for _, c := range cases {
		if got := scanFailureClass(c.Message); got != c.Class {
			t.Errorf("scanFailureClass(%q) = %q, want %q", c.Message, got, c.Class)
		}
	}
	have := []string{classUnclassified, classNoHomeRoot, classRootOutside, classRootNotDir, classTimedOut,
		classUnreadableOther, classOverCap, classNotUTF8, classUnparsable}
	for k := range unreadableKinds {
		have = append(have, "path unreadable ("+k+")")
	}
	slices.Sort(have)
	want := slices.Clone(classes)
	slices.Sort(want)
	if !slices.Equal(have, want) {
		t.Errorf("class set differs from the shared fixture:\n have %q\n want %q", have, want)
	}
}

// Nothing of a hostile input reaches the class: it equals a fixed class, and no path
// character or digit run of the input is in it.
func TestScanFailureClassLeaksNothingOfTheInput(t *testing.T) {
	classes, _, hostile := classFixture(t)
	digits := regexp.MustCompile(`[0-9]{3,}`)
	for _, h := range hostile {
		got := scanFailureClass(h.Message)
		if got != h.Class || !slices.Contains(classes, got) {
			t.Errorf("scanFailureClass(%q) = %q, want fixed class %q", h.Message, got, h.Class)
		}
		if strings.ContainsAny(got, "/\n") {
			t.Errorf("class %q has a path character", got)
		}
		for _, run := range digits.FindAllString(h.Message, -1) {
			if strings.Contains(got, run) {
				t.Errorf("class %q carries %q from the input", got, run)
			}
		}
	}
}
