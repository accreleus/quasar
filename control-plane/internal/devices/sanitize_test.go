package devices

import (
	"encoding/json"
	"strings"
	"testing"
)

// AS10-08 — capability sanitization unit tests. Pure (no DB); always run.

func mustSanitize(t *testing.T, in string) map[string]any {
	t.Helper()
	out, err := sanitizeCapabilities(json.RawMessage(in))
	if err != nil {
		t.Fatalf("sanitizeCapabilities(%q): %v", in, err)
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("result not a JSON object: %v (%s)", err, out)
	}
	return m
}

// TestSanitizeEmptyInput: nil/empty input yields an empty object, not an error.
func TestSanitizeEmptyInput(t *testing.T) {
	for _, in := range []string{"", "{}", "null"} {
		out, err := sanitizeCapabilities(json.RawMessage(in))
		if err != nil {
			t.Fatalf("input %q: unexpected error %v", in, err)
		}
		var m map[string]any
		if err := json.Unmarshal(out, &m); err != nil {
			t.Fatalf("input %q: result not an object: %v", in, err)
		}
		if len(m) != 0 {
			t.Fatalf("input %q: expected empty object, got %v", in, m)
		}
	}
}

// TestSanitizeInvalidJSON: malformed JSON is an error (the handler rejects it).
func TestSanitizeInvalidJSON(t *testing.T) {
	if _, err := sanitizeCapabilities(json.RawMessage(`{not json`)); err == nil {
		t.Fatal("expected an error for malformed JSON")
	}
}

// TestSanitizeClampsOversizedStrings: a string longer than maxStringLen is
// truncated; modelled and unmodelled fields alike.
func TestSanitizeClampsOversizedStrings(t *testing.T) {
	big := strings.Repeat("a", maxStringLen+500)
	in := `{"browser":{"name":"` + big + `"},"junk":"` + big + `"}`
	m := mustSanitize(t, in)

	browser, ok := m["browser"].(map[string]any)
	if !ok {
		t.Fatalf("browser missing/not object: %v", m["browser"])
	}
	name, _ := browser["name"].(string)
	if len([]rune(name)) != maxStringLen {
		t.Fatalf("browser.name not clamped: len=%d want %d", len([]rune(name)), maxStringLen)
	}
	junk, _ := m["junk"].(string)
	if len([]rune(junk)) != maxStringLen {
		t.Fatalf("unmodelled string not clamped: len=%d want %d", len([]rune(junk)), maxStringLen)
	}
}

// TestSanitizeNormalizesClientType: a bad/unknown client_type is normalised to
// "web"; a known one is preserved.
func TestSanitizeNormalizesClientType(t *testing.T) {
	cases := map[string]string{
		`{"client_type":"web"}`:                               "web",
		`{"client_type":"native"}`:                            "native",
		`{"client_type":"bogus"}`:                             "web",
		`{"client_type":12345}`:                               "web",
		`{"client_type":{"a":1}}`:                             "web",
		`{"client_type":"` + strings.Repeat("x", 5000) + `"}`: "web",
	}
	for in, want := range cases {
		m := mustSanitize(t, in)
		if got, _ := m["client_type"].(string); got != want {
			t.Fatalf("input %q: client_type=%q want %q", in[:min(len(in), 40)], got, want)
		}
	}
}

// TestSanitizePreservesKnownFields: the fields we DON'T model (codecs, bandwidth,
// nested display/features) round-trip verbatim within bounds — the opaque
// contract the AS10-02 reader depends on.
func TestSanitizePreservesKnownFields(t *testing.T) {
	in := `{
		"client_type":"web",
		"codecs":{"h264":true,"hevc":false,"av1":false,"vp9":true},
		"max_decode_height":2160,
		"bandwidth_kbps":48000,
		"rtt_ms":12,
		"browser":{"name":"Chrome","version":"126.0"},
		"platform":"macOS",
		"display":{"width":2560,"height":1440,"refresh_hz":120},
		"features":{"jitter_buffer_target":true,"playout_delay_hint":true,
		            "pointer_lock":true,"coalesced_pointer_events":true,"gamepad":true},
		"profiles":{}
	}`
	m := mustSanitize(t, in)

	if m["bandwidth_kbps"] != float64(48000) {
		t.Fatalf("bandwidth_kbps lost: %v", m["bandwidth_kbps"])
	}
	disp, ok := m["display"].(map[string]any)
	if !ok || disp["refresh_hz"] != float64(120) {
		t.Fatalf("display not preserved: %v", m["display"])
	}
	feats, ok := m["features"].(map[string]any)
	if !ok || feats["gamepad"] != true {
		t.Fatalf("features not preserved: %v", m["features"])
	}
	codecs, ok := m["codecs"].(map[string]any)
	if !ok || codecs["h264"] != true || codecs["vp9"] != true {
		t.Fatalf("codecs not preserved: %v", m["codecs"])
	}
}

// TestSanitizeDropsNonFiniteNumbers: NaN/Inf cannot be marshalled — they are
// dropped so the blob stays serialisable. (We inject them via a Go value since
// JSON has no NaN literal.)
func TestSanitizeDropsNonFiniteNumbers(t *testing.T) {
	// sanitizeValue is the unit under test for the non-finite path.
	out, keep := sanitizeValue(map[string]any{
		"good": 12.0,
		"nan":  jsonNaN(),
		"inf":  jsonInf(),
	}, 0)
	if !keep {
		t.Fatal("top-level object should survive")
	}
	m := out.(map[string]any)
	if _, ok := m["nan"]; ok {
		t.Fatal("NaN should have been dropped")
	}
	if _, ok := m["inf"]; ok {
		t.Fatal("Inf should have been dropped")
	}
	if m["good"] != 12.0 {
		t.Fatalf("finite number dropped: %v", m["good"])
	}
}

// TestSanitizeBoundsDepth: nesting deeper than maxDepth is dropped rather than
// stored (a small-but-abusive body).
func TestSanitizeBoundsDepth(t *testing.T) {
	// Build {"a":{"a":{...}}} nested maxDepth+5 deep.
	in := strings.Repeat(`{"a":`, maxDepth+5) + `1` + strings.Repeat(`}`, maxDepth+5)
	m := mustSanitize(t, in)
	// Walk down: at some level the subtree must be gone (dropped past maxDepth).
	depth := 0
	cur := any(m)
	for {
		obj, ok := cur.(map[string]any)
		if !ok {
			break
		}
		next, present := obj["a"]
		if !present {
			break
		}
		cur = next
		depth++
		if depth > maxDepth+10 {
			t.Fatal("depth not bounded")
		}
	}
	if depth > maxDepth+1 {
		t.Fatalf("nesting not bounded: reached depth %d (max %d)", depth, maxDepth)
	}
}

// TestSanitizeBoundsArray: an array longer than maxArrayLen is truncated.
func TestSanitizeBoundsArray(t *testing.T) {
	elems := make([]string, maxArrayLen+50)
	for i := range elems {
		elems[i] = "1"
	}
	in := `{"arr":[` + strings.Join(elems, ",") + `]}`
	m := mustSanitize(t, in)
	arr, ok := m["arr"].([]any)
	if !ok {
		t.Fatalf("arr missing/not array: %v", m["arr"])
	}
	if len(arr) != maxArrayLen {
		t.Fatalf("array not bounded: len=%d want %d", len(arr), maxArrayLen)
	}
}

func jsonNaN() float64 { z := 0.0; return z / z }   // NaN
func jsonInf() float64 { z := 0.0; return 1.0 / z } // +Inf

// --- AS10-12 native-client report sanitization ------------------------------

// TestSanitizeNativeClientType: client_type "native" is a known value and is
// preserved (the sanitizer already whitelists it).
func TestSanitizeNativeClientType(t *testing.T) {
	m := mustSanitize(t, `{"client_type":"native"}`)
	if got, _ := m["client_type"].(string); got != "native" {
		t.Fatalf("native client_type not preserved: %q", got)
	}
}

// TestSanitizeReportVersionInteger: a present integer report_version is kept as-is.
func TestSanitizeReportVersionInteger(t *testing.T) {
	m := mustSanitize(t, `{"client_type":"native","report_version":1}`)
	if m["report_version"] != float64(1) {
		t.Fatalf("integer report_version dropped/altered: %v", m["report_version"])
	}
}

// TestSanitizeReportVersionNonIntegerDropped: a non-integer report_version (string,
// fractional, object) is dropped; the rest of the blob still stores.
func TestSanitizeReportVersionNonIntegerDropped(t *testing.T) {
	cases := []string{
		`{"report_version":"1","client_type":"native"}`,
		`{"report_version":1.5,"client_type":"native"}`,
		`{"report_version":{"a":1},"client_type":"native"}`,
	}
	for _, in := range cases {
		m := mustSanitize(t, in)
		if _, present := m["report_version"]; present {
			t.Fatalf("input %q: non-integer report_version not dropped: %v", in, m["report_version"])
		}
		// The rest of the blob must survive.
		if m["client_type"] != "native" {
			t.Fatalf("input %q: rest of blob not preserved (client_type=%v)", in, m["client_type"])
		}
	}
}

// TestSanitizeNativeSubObjectsPreserved: the native sub-objects (decode, audio,
// input, metrics, health, os) round-trip verbatim within the generic bounds —
// forward-data the contract depends on storing opaquely.
func TestSanitizeNativeSubObjectsPreserved(t *testing.T) {
	in := `{
		"client_type":"native",
		"report_version":1,
		"client_name":"quasar-native-macos",
		"os":{"name":"macOS","version":"15.5","arch":"arm64"},
		"display":{"width":3456,"height":2234,"refresh_hz":120,"hdr":true,"vrr":true},
		"codecs":{"h264":true,"hevc":true,"av1":true,"vp9":false},
		"decode":{"h264":{"hw":true,"profiles":["constrained-baseline","main","high"],
		                  "levels":["4.2","5.1"],"max_height":2160}},
		"audio":{"channels":2,"sample_rate":48000,"codecs":["opus"]},
		"input":{"raw_mouse":true,"keyboard":true,"high_rate_input":true,
		         "controllers":[{"type":"dualsense","rumble":true,"haptics":true,"player":0}]},
		"metrics":{"decode_ms":1.8,"present_fps":59.9,"present_interval_sd_ms":1.2,
		           "glass_to_glass_ms_p50":45,"glass_to_glass_ms_p95":104,
		           "interactive_ms_p50":54,"jitter_buffer_ms":20,
		           "render_path":"webrtcbin+videotoolbox"},
		"health":{"class":"smooth","reason":""}
	}`
	m := mustSanitize(t, in)

	// decode matrix (forward-data) preserved.
	dec, ok := m["decode"].(map[string]any)
	if !ok {
		t.Fatalf("decode dropped: %v", m["decode"])
	}
	h264, ok := dec["h264"].(map[string]any)
	if !ok || h264["hw"] != true {
		t.Fatalf("decode.h264 not preserved: %v", dec["h264"])
	}
	profs, ok := h264["profiles"].([]any)
	if !ok || len(profs) != 3 || profs[2] != "high" {
		t.Fatalf("decode.h264.profiles not preserved: %v", h264["profiles"])
	}
	// metrics (p50 + p95) preserved.
	met, ok := m["metrics"].(map[string]any)
	if !ok || met["glass_to_glass_ms_p95"] != float64(104) {
		t.Fatalf("metrics.glass_to_glass_ms_p95 not preserved: %v", m["metrics"])
	}
	if met["render_path"] != "webrtcbin+videotoolbox" {
		t.Fatalf("metrics.render_path not preserved: %v", met["render_path"])
	}
	// input.controllers preserved.
	inp, ok := m["input"].(map[string]any)
	if !ok {
		t.Fatalf("input dropped: %v", m["input"])
	}
	ctrls, ok := inp["controllers"].([]any)
	if !ok || len(ctrls) != 1 {
		t.Fatalf("input.controllers not preserved: %v", inp["controllers"])
	}
	// health (class) preserved.
	hlt, ok := m["health"].(map[string]any)
	if !ok || hlt["class"] != "smooth" {
		t.Fatalf("health not preserved: %v", m["health"])
	}
	// os preserved.
	os, ok := m["os"].(map[string]any)
	if !ok || os["arch"] != "arm64" {
		t.Fatalf("os not preserved: %v", m["os"])
	}
}

// TestNativeCapabilitiesViewUnmarshal: the optional Go view struct can decode a
// native report (read-side only; storage never routes through it).
func TestNativeCapabilitiesViewUnmarshal(t *testing.T) {
	in := `{
		"report_version":1,"client_type":"native","client_name":"quasar-native-macos",
		"os":{"name":"macOS","version":"15.5","arch":"arm64"},
		"display":{"width":3456,"height":2234,"device_pixel_ratio":2.0,"refresh_hz":120,"hdr":true,"vrr":true},
		"codecs":{"h264":true,"hevc":true,"av1":true,"vp9":false},
		"decode":{"h264":{"hw":true,"profiles":["high"],"levels":["5.1"],"max_height":2160}},
		"input":{"raw_mouse":true,"keyboard":true,"high_rate_input":true,
		         "controllers":[{"type":"xbox","rumble":true,"haptics":false,"player":0}]},
		"metrics":{"glass_to_glass_ms_p50":45,"glass_to_glass_ms_p95":104,"render_path":"webrtcbin+videotoolbox"},
		"profiles":{"1080p60":{"h264_profile_decoded":"high","decode_pass":true}}
	}`
	var nc NativeCapabilities
	if err := json.Unmarshal([]byte(in), &nc); err != nil {
		t.Fatalf("unmarshal native view: %v", err)
	}
	if nc.ReportVersion == nil || *nc.ReportVersion != 1 {
		t.Fatalf("report_version not decoded: %v", nc.ReportVersion)
	}
	if nc.ClientType != "native" {
		t.Fatalf("client_type not decoded: %q", nc.ClientType)
	}
	if nc.Codecs["h264"] != true {
		t.Fatalf("flat codecs (eligibility surface) not decoded: %v", nc.Codecs)
	}
	if nc.Decode == nil || nc.Decode.H264 == nil || !nc.Decode.H264.HW {
		t.Fatalf("decode matrix not decoded: %v", nc.Decode)
	}
	if nc.Metrics == nil || nc.Metrics.GlassToGlassP95Ms == nil || *nc.Metrics.GlassToGlassP95Ms != 104 {
		t.Fatalf("metrics not decoded: %v", nc.Metrics)
	}
	if nc.Input == nil || len(nc.Input.Controllers) != 1 || nc.Input.Controllers[0].Type != "xbox" {
		t.Fatalf("input.controllers not decoded: %v", nc.Input)
	}
	cert, ok := nc.Profiles["1080p60"]
	if !ok || cert.H264ProfileDecoded != "high" {
		t.Fatalf("profile cert not decoded: %v", nc.Profiles)
	}
}

// TestSanitizeBoundsObjectKeys: an object wider than maxObjectKeys is truncated
// deterministically (sorted keys, so the surviving set is stable).
func TestSanitizeBoundsObjectKeys(t *testing.T) {
	var b strings.Builder
	b.WriteString("{")
	for i := 0; i < maxObjectKeys+20; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(`"k`)
		b.WriteString(itoa(i))
		b.WriteString(`":1`)
	}
	b.WriteString("}")
	m := mustSanitize(t, b.String())
	if len(m) != maxObjectKeys {
		t.Fatalf("object keys not bounded: %d want %d", len(m), maxObjectKeys)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}
