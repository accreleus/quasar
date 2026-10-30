package settings

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// Region and Input value rules, shared by GET/PATCH /v1/me/region-input and the
// admin defaults. semantics: control-api.md §Region and Input settings. The
// regexps mirror migration 0108's CHECKs.
var (
	languagePattern   = regexp.MustCompile(`^[a-z]{2,3}-[A-Z]{2}$`)
	xkbLayoutPattern  = regexp.MustCompile(`^[A-Za-z0-9_-]{1,32}$`)
	xkbVariantPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{0,64}$`)
)

const (
	MinMouseSensitivity = 0.25
	MaxMouseSensitivity = 4
)

// ValidTimezone accepts a zone the control plane's tz database knows, links
// included, other than Local. LoadLocation reads "" as UTC, so "" is refused here.
// The system zoneinfo directory also holds posixrules and the posix/ and right/
// trees, which LoadLocation finds first and which are not IANA zone names.
func ValidTimezone(tz string) bool {
	if tz == "" || tz == "Local" || len(tz) > 64 || tz == "posixrules" ||
		strings.HasPrefix(tz, "posix/") || strings.HasPrefix(tz, "right/") {
		return false
	}
	_, err := time.LoadLocation(tz)
	return err == nil
}

func ValidLanguage(v string) bool    { return languagePattern.MatchString(v) }
func ValidXKBLayout(v string) bool   { return xkbLayoutPattern.MatchString(v) }
func ValidXKBVariant(v string) bool  { return xkbVariantPattern.MatchString(v) }
func ValidMatchCursor(v string) bool { return v == "auto" || v == "on" || v == "off" }
func ValidMouseSensitivity(v float64) bool {
	return v >= MinMouseSensitivity && v <= MaxMouseSensitivity
}

// Keyboard is an xkeyboard-config layout and one of its variants ("" = basic).
type Keyboard struct {
	Layout  string `json:"layout"`
	Variant string `json:"variant"`
}

// DecodeKeyboard reads a keyboard object strictly: both keys, nothing else.
func DecodeKeyboard(raw json.RawMessage) (Keyboard, error) {
	var k struct {
		Layout  *string `json:"layout"`
		Variant *string `json:"variant"`
	}
	if err := strictUnmarshal(raw, &k); err != nil || k.Layout == nil || k.Variant == nil {
		return Keyboard{}, fmt.Errorf("keyboard must be {\"layout\", \"variant\"}")
	}
	if !ValidXKBLayout(*k.Layout) || !ValidXKBVariant(*k.Variant) {
		return Keyboard{}, fmt.Errorf("keyboard layout or variant is not an xkeyboard-config name")
	}
	return Keyboard{Layout: *k.Layout, Variant: *k.Variant}, nil
}

// RegionInputDefaults are the nine install defaults on instance_settings.
type RegionInputDefaults struct {
	Timezone         string   `json:"default_timezone"`
	Language         string   `json:"default_language"`
	RegionFormat     *string  `json:"default_region_format"`
	Keyboard         Keyboard `json:"default_keyboard"`
	MatchCursor      string   `json:"default_match_cursor"`
	MouseSensitivity float64  `json:"default_mouse_sensitivity"`
	RawMouse         bool     `json:"default_raw_mouse"`
	FlipScroll       bool     `json:"default_flip_scroll"`
	NintendoLayout   bool     `json:"default_nintendo_layout"`
}

// BuiltInRegionInputDefaults are the column defaults: a missing singleton row
// reads as these.
var BuiltInRegionInputDefaults = RegionInputDefaults{
	Timezone: "UTC", Language: "en-US", Keyboard: Keyboard{Layout: "us"},
	MatchCursor: "auto", MouseSensitivity: 1,
}

// RegionInputDefaultsColumns is the column list ScanRegionInputDefaults reads.
const RegionInputDefaultsColumns = `default_timezone, default_language, default_region_format,
	default_keyboard_layout, default_keyboard_variant, default_match_cursor,
	default_mouse_sensitivity, default_raw_mouse, default_flip_scroll, default_nintendo_layout`

func regionInputDefaultsDest(d *RegionInputDefaults) []any {
	return []any{&d.Timezone, &d.Language, &d.RegionFormat, &d.Keyboard.Layout, &d.Keyboard.Variant,
		&d.MatchCursor, &d.MouseSensitivity, &d.RawMouse, &d.FlipScroll, &d.NintendoLayout}
}

// ScanRegionInputDefaults scans a row selecting RegionInputDefaultsColumns.
func ScanRegionInputDefaults(row interface{ Scan(dest ...any) error }) (RegionInputDefaults, error) {
	var d RegionInputDefaults
	err := row.Scan(regionInputDefaultsDest(&d)...)
	return d, err
}

// RegionInputPatch is the admin PATCH of the defaults; nil = unchanged.
// RegionFormatSet with a nil RegionFormat clears it (the same as the language).
type RegionInputPatch struct {
	Timezone         *string
	Language         *string
	RegionFormatSet  bool
	RegionFormat     *string
	Keyboard         *Keyboard
	MatchCursor      *string
	MouseSensitivity *float64
	RawMouse         *bool
	FlipScroll       *bool
	NintendoLayout   *bool
}

func (p RegionInputPatch) empty() bool {
	return p.Timezone == nil && p.Language == nil && !p.RegionFormatSet && p.Keyboard == nil &&
		p.MatchCursor == nil && p.MouseSensitivity == nil && p.RawMouse == nil &&
		p.FlipScroll == nil && p.NintendoLayout == nil
}

func (p RegionInputPatch) changedKeys() []string {
	var keys []string
	for _, f := range []struct {
		name string
		set  bool
	}{
		{"default_timezone", p.Timezone != nil},
		{"default_language", p.Language != nil},
		{"default_region_format", p.RegionFormatSet},
		{"default_keyboard", p.Keyboard != nil},
		{"default_match_cursor", p.MatchCursor != nil},
		{"default_mouse_sensitivity", p.MouseSensitivity != nil},
		{"default_raw_mouse", p.RawMouse != nil},
		{"default_flip_scroll", p.FlipScroll != nil},
		{"default_nintendo_layout", p.NintendoLayout != nil},
	} {
		if f.set {
			keys = append(keys, f.name)
		}
	}
	return keys
}

// regionInputRequest is the admin PATCH body's nine fields, raw so that null is
// told apart from absence (null is accepted only for default_region_format).
type regionInputRequest struct {
	Timezone         json.RawMessage `json:"default_timezone"`
	Language         json.RawMessage `json:"default_language"`
	RegionFormat     json.RawMessage `json:"default_region_format"`
	Keyboard         json.RawMessage `json:"default_keyboard"`
	MatchCursor      json.RawMessage `json:"default_match_cursor"`
	MouseSensitivity json.RawMessage `json:"default_mouse_sensitivity"`
	RawMouse         json.RawMessage `json:"default_raw_mouse"`
	FlipScroll       json.RawMessage `json:"default_flip_scroll"`
	NintendoLayout   json.RawMessage `json:"default_nintendo_layout"`
}

func (r regionInputRequest) patch() (RegionInputPatch, error) {
	var p RegionInputPatch
	str := func(raw json.RawMessage, name string, ok func(string) bool) (*string, error) {
		if len(raw) == 0 {
			return nil, nil
		}
		var v string
		if json.Unmarshal(raw, &v) != nil || !ok(v) {
			return nil, fmt.Errorf("%s is not an accepted value", name)
		}
		return &v, nil
	}
	boolean := func(raw json.RawMessage, name string) (*bool, error) {
		if len(raw) == 0 {
			return nil, nil
		}
		var v bool
		if string(raw) == "null" || json.Unmarshal(raw, &v) != nil {
			return nil, fmt.Errorf("%s must be a boolean", name)
		}
		return &v, nil
	}
	var err error
	if p.Timezone, err = str(r.Timezone, "default_timezone", ValidTimezone); err != nil {
		return p, err
	}
	if p.Language, err = str(r.Language, "default_language", ValidLanguage); err != nil {
		return p, err
	}
	if len(r.RegionFormat) > 0 {
		p.RegionFormatSet = true
		if string(r.RegionFormat) != "null" {
			if p.RegionFormat, err = str(r.RegionFormat, "default_region_format", ValidLanguage); err != nil {
				return p, err
			}
		}
	}
	if len(r.Keyboard) > 0 {
		k, err := DecodeKeyboard(r.Keyboard)
		if err != nil {
			return p, fmt.Errorf("default_keyboard: %w", err)
		}
		p.Keyboard = &k
	}
	if p.MatchCursor, err = str(r.MatchCursor, "default_match_cursor", ValidMatchCursor); err != nil {
		return p, err
	}
	if len(r.MouseSensitivity) > 0 {
		var v float64
		if string(r.MouseSensitivity) == "null" || json.Unmarshal(r.MouseSensitivity, &v) != nil || !ValidMouseSensitivity(v) {
			return p, fmt.Errorf("default_mouse_sensitivity must be a number from 0.25 to 4")
		}
		p.MouseSensitivity = &v
	}
	if p.RawMouse, err = boolean(r.RawMouse, "default_raw_mouse"); err != nil {
		return p, err
	}
	if p.FlipScroll, err = boolean(r.FlipScroll, "default_flip_scroll"); err != nil {
		return p, err
	}
	if p.NintendoLayout, err = boolean(r.NintendoLayout, "default_nintendo_layout"); err != nil {
		return p, err
	}
	return p, nil
}

func strictUnmarshal(raw json.RawMessage, dst any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	return dec.Decode(dst)
}
