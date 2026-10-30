package settings

import (
	"encoding/json"
	"testing"
)

// The shape rule is tested apart from LoadLocation: whether a host's zoneinfo
// has posix/ or right/ trees varies, the refusal must not.
func TestZoneNameShapeRefusesWhatIsNotAnIANAName(t *testing.T) {
	for tz, want := range map[string]bool{
		"Europe/London": true, "UTC": true, "Etc/GMT+5": true, "Asia/Kolkata": true,
		"Local": false, "localtime": false, "posixrules": false,
		"posix/Europe/London": false, "posix/UTC": false, "right/Europe/London": false, "right/UTC": false,
	} {
		if got := zoneNameShape(tz); got != want {
			t.Errorf("zoneNameShape(%q) = %v, want %v", tz, got, want)
		}
	}
}

func TestRegionInputValueRules(t *testing.T) {
	for tz, want := range map[string]bool{
		"Europe/London": true, "UTC": true, "Asia/Calcutta": true, "Asia/Kolkata": true,
		"America/Argentina/Buenos_Aires": true,
		"Local":                          false, "": false, "Mars/Olympus": false, "../etc/passwd": false, "europe/london": false,
		"posixrules": false, "localtime": false, "posix/Europe/London": false, "right/UTC": false,
	} {
		if got := ValidTimezone(tz); got != want {
			t.Errorf("ValidTimezone(%q) = %v, want %v", tz, got, want)
		}
	}
	for v, want := range map[string]bool{
		"de-DE": true, "fil-PH": true, "en-us": false, "de": false, "de_DE": false, "de-DE.UTF-8": false, "": false,
	} {
		if got := ValidLanguage(v); got != want {
			t.Errorf("ValidLanguage(%q) = %v, want %v", v, got, want)
		}
	}
	if !ValidXKBLayout("gb") || ValidXKBLayout("") || ValidXKBLayout("g b") || ValidXKBLayout("us,de") {
		t.Error("ValidXKBLayout accepts or refuses the wrong names")
	}
	if !ValidXKBVariant("") || !ValidXKBVariant("nodeadkeys") || ValidXKBVariant("$(x)") {
		t.Error("ValidXKBVariant accepts or refuses the wrong names")
	}
	for v, want := range map[float64]bool{0.25: true, 4: true, 1: true, 0.24: false, 4.01: false} {
		if got := ValidMouseSensitivity(v); got != want {
			t.Errorf("ValidMouseSensitivity(%v) = %v, want %v", v, got, want)
		}
	}
}

func TestAdminRegionInputDefaultsPatch(t *testing.T) {
	parse := func(body string) (RegionInputPatch, error) {
		var r regionInputRequest
		if err := json.Unmarshal([]byte(body), &r); err != nil {
			t.Fatalf("%s: %v", body, err)
		}
		return r.patch()
	}
	p, err := parse(`{"default_timezone":"Europe/Berlin","default_region_format":null,
		"default_keyboard":{"layout":"de","variant":"nodeadkeys"},"default_mouse_sensitivity":0.5,
		"default_raw_mouse":true}`)
	if err != nil {
		t.Fatal(err)
	}
	if *p.Timezone != "Europe/Berlin" || !p.RegionFormatSet || p.RegionFormat != nil ||
		*p.Keyboard != (Keyboard{"de", "nodeadkeys"}) || *p.MouseSensitivity != 0.5 || !*p.RawMouse ||
		p.Language != nil || p.FlipScroll != nil {
		t.Errorf("patch = %+v", p)
	}
	if p, _ := parse(`{}`); !p.empty() {
		t.Error("an empty body sets a default")
	}
	for _, bad := range []string{
		`{"default_timezone":null}`, `{"default_timezone":"Local"}`, `{"default_language":"de_DE"}`,
		`{"default_region_format":"x"}`, `{"default_keyboard":null}`, `{"default_keyboard":{"layout":"de"}}`,
		`{"default_keyboard":{"layout":"de","variant":"","extra":1}}`, `{"default_match_cursor":"sometimes"}`,
		`{"default_mouse_sensitivity":5}`, `{"default_mouse_sensitivity":null}`, `{"default_raw_mouse":null}`,
		`{"default_flip_scroll":"yes"}`, `{"default_nintendo_layout":1}`,
	} {
		if _, err := parse(bad); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
}
