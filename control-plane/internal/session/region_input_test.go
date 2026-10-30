package session

import (
	"encoding/json"
	"testing"

	"github.com/accreleus/quasar/control-plane/internal/settings"
)

func strp(v string) *string { return &v }

func TestRegionInputEnvResolvesUserThenBrowserThenDefault(t *testing.T) {
	d := settings.BuiltInRegionInputDefaults
	none := DeviceScope{}
	browser := DeviceScope{Timezone: "America/New_York", Language: "fr-FR"}

	// Nothing set anywhere: the built-in defaults, region format = language.
	env := regionInputEnv(RegionInput{}, none, d)
	want := map[string]string{"TZ": "UTC", "LANG": "en_US.UTF-8", "LC_TIME": "en_US.UTF-8",
		"LC_NUMERIC": "en_US.UTF-8", "LC_MONETARY": "en_US.UTF-8", "LC_MEASUREMENT": "en_US.UTF-8",
		"LC_PAPER": "en_US.UTF-8", "XKB_DEFAULT_LAYOUT": "us", "XKB_DEFAULT_VARIANT": ""}
	if len(env) != len(want) {
		t.Fatalf("env = %v, want the nine keys", env)
	}
	for k, v := range want {
		if got, ok := env[k]; !ok || got != v {
			t.Errorf("%s = %q (present %v), want %q", k, got, ok, v)
		}
	}

	// The browser's values beat the install default; the region format follows them.
	env = regionInputEnv(RegionInput{}, browser, d)
	if env["TZ"] != "America/New_York" || env["LANG"] != "fr_FR.UTF-8" || env["LC_PAPER"] != "fr_FR.UTF-8" {
		t.Errorf("browser fallback: %v", env)
	}

	// An admin region format beats the language; the admin keyboard applies.
	d2 := d
	d2.Timezone, d2.RegionFormat, d2.Keyboard = "Europe/Paris", strp("en-GB"), settings.Keyboard{Layout: "fr", Variant: "azerty"}
	env = regionInputEnv(RegionInput{}, none, d2)
	if env["TZ"] != "Europe/Paris" || env["LANG"] != "en_US.UTF-8" || env["LC_TIME"] != "en_GB.UTF-8" ||
		env["XKB_DEFAULT_LAYOUT"] != "fr" || env["XKB_DEFAULT_VARIANT"] != "azerty" {
		t.Errorf("admin defaults: %v", env)
	}

	// The user's own values beat both.
	mine := RegionInput{
		Region: RegionSettings{Timezone: strp("Asia/Tokyo"), Language: strp("de-DE"), RegionFormat: strp("de-CH")},
		Input:  InputSettings{Keyboard: &settings.Keyboard{Layout: "de", Variant: "nodeadkeys"}},
	}
	env = regionInputEnv(mine, browser, d2)
	if env["TZ"] != "Asia/Tokyo" || env["LANG"] != "de_DE.UTF-8" || env["LC_MONETARY"] != "de_CH.UTF-8" ||
		env["XKB_DEFAULT_LAYOUT"] != "de" || env["XKB_DEFAULT_VARIANT"] != "nodeadkeys" {
		t.Errorf("user values: %v", env)
	}
}

func TestParseDeviceRegionDropsInvalidValues(t *testing.T) {
	tz, lang := parseDeviceRegion([]byte(`{"timezone":"Europe/Rome","language":"it-IT"}`))
	if tz != "Europe/Rome" || lang != "it-IT" {
		t.Errorf("valid = %q %q", tz, lang)
	}
	for _, caps := range []string{
		`{"timezone":"Local","language":"it"}`, `{"timezone":"$(id)","language":"it_IT"}`,
		`{"timezone":5,"language":true}`, `{}`, `not json`,
	} {
		if tz, lang := parseDeviceRegion([]byte(caps)); tz != "" || lang != "" {
			t.Errorf("%s read as %q %q, want absent", caps, tz, lang)
		}
	}
}

func TestRegionInputPatchRules(t *testing.T) {
	apply := func(region, input string, cur RegionInput) (RegionInput, error) {
		var r, i json.RawMessage
		if region != "" {
			r = json.RawMessage(region)
		}
		if input != "" {
			i = json.RawMessage(input)
		}
		patch, err := parseRegionInputPatch(r, i)
		if err != nil {
			return cur, err
		}
		patch(&cur)
		return cur, nil
	}
	cur := RegionInput{Region: RegionSettings{Timezone: strp("UTC"), Language: strp("en-GB")},
		Input: InputSettings{Keyboard: &settings.Keyboard{Layout: "gb", Variant: "extd"}, FlipScroll: new(bool)}}

	// Absent unchanged, null clears, keyboard replaced whole.
	got, err := apply(`{"timezone":null,"region_format":"de-DE"}`,
		`{"keyboard":{"layout":"de","variant":""},"mouse_sensitivity":1.5}`, cur)
	if err != nil {
		t.Fatal(err)
	}
	if got.Region.Timezone != nil || *got.Region.Language != "en-GB" || *got.Region.RegionFormat != "de-DE" ||
		*got.Input.Keyboard != (settings.Keyboard{Layout: "de"}) || *got.Input.MouseSensitivity != 1.5 ||
		got.Input.FlipScroll == nil {
		t.Errorf("patched = %+v / %+v", got.Region, got.Input)
	}
	if got, _ := apply("", `{"keyboard":null}`, cur); got.Input.Keyboard != nil {
		t.Error("keyboard null did not clear the pair")
	}

	for _, bad := range [][2]string{
		{`{"timezone":"Local"}`, ""}, {`{"timezone":"Nowhere/City"}`, ""}, {`{"timezone":""}`, ""},
		{`{"language":"de"}`, ""}, {`{"language":"de-de"}`, ""}, {`{"region_format":"DE-DE"}`, ""},
		{`{"zone":"UTC"}`, ""}, {`null`, ""}, {`[]`, ""},
		{"", `{"keyboard":{"layout":"de"}}`}, {"", `{"keyboard":{"layout":"de","variant":"x y"}}`},
		{"", `{"keyboard":{"layout":"","variant":""}}`}, {"", `{"keyboard":"de"}`},
		{"", `{"match_cursor":"always"}`}, {"", `{"mouse_sensitivity":0.2}`}, {"", `{"mouse_sensitivity":4.5}`},
		{"", `{"mouse_sensitivity":"1"}`}, {"", `{"raw_mouse":"true"}`}, {"", `{"flip_scroll":1}`},
		{"", `{"nintendo_layout":"no"}`}, {"", `{"cursor":"on"}`},
	} {
		if _, err := apply(bad[0], bad[1], cur); err == nil {
			t.Errorf("region %s input %s accepted", bad[0], bad[1])
		}
	}
	// Accepted edges.
	for _, ok := range [][2]string{
		{`{"timezone":"Asia/Calcutta"}`, ""}, {"", `{"mouse_sensitivity":0.25}`}, {"", `{"mouse_sensitivity":4}`},
		{"", `{"match_cursor":null,"raw_mouse":false,"nintendo_layout":true}`},
	} {
		if _, err := apply(ok[0], ok[1], cur); err != nil {
			t.Errorf("region %s input %s refused: %v", ok[0], ok[1], err)
		}
	}
}

func TestSameLaunchValuesIgnoresClientFields(t *testing.T) {
	a := RegionInput{Region: RegionSettings{Timezone: strp("UTC")}}
	b := a
	b.Input.MouseSensitivity, b.Input.RawMouse = new(float64), new(bool)
	if !sameLaunchValues(a, b) {
		t.Error("a client field counted as a launch value")
	}
	for _, c := range []RegionInput{
		{Region: RegionSettings{Timezone: strp("Europe/London")}},
		{},
		{Region: RegionSettings{Timezone: strp("UTC"), Language: strp("de-DE")}},
		{Region: RegionSettings{Timezone: strp("UTC"), RegionFormat: strp("de-DE")}},
		{Region: a.Region, Input: InputSettings{Keyboard: &settings.Keyboard{Layout: "us"}}},
	} {
		if sameLaunchValues(a, c) {
			t.Errorf("%+v read as unchanged", c)
		}
	}
}
