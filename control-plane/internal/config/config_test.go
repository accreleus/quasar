package config

import (
	"strings"
	"testing"
	"time"
)

func TestLoadTLSModeResolution(t *testing.T) {
	base := func(t *testing.T) {
		t.Setenv("DATABASE_URL", "postgres://test")
		t.Setenv("ENROLLMENT_TOKEN", "test-token")
	}

	t.Run("default is auto+self-signed", func(t *testing.T) {
		base(t)
		c, err := Load()
		if err != nil {
			t.Fatal(err)
		}
		if !c.TLSEnabled() {
			t.Fatal("TLS should default to enabled (auto)")
		}
		if c.TLSProvided() {
			t.Fatal("no cert/key set, TLSProvided should be false")
		}
		if c.TLSAddr != ":8443" {
			t.Fatalf("TLSAddr = %q, want :8443", c.TLSAddr)
		}
		if c.TLSDir != "/var/lib/quasar-control/tls" {
			t.Fatalf("TLSDir = %q", c.TLSDir)
		}
	})

	t.Run("off disables", func(t *testing.T) {
		base(t)
		t.Setenv("QUASAR_TLS", "off")
		c, err := Load()
		if err != nil {
			t.Fatal(err)
		}
		if c.TLSEnabled() {
			t.Fatal("QUASAR_TLS=off should disable TLS")
		}
	})

	t.Run("off is case-insensitive", func(t *testing.T) {
		base(t)
		t.Setenv("QUASAR_TLS", "OFF")
		c, err := Load()
		if err != nil {
			t.Fatal(err)
		}
		if c.TLSEnabled() {
			t.Fatal("QUASAR_TLS=OFF should disable TLS")
		}
	})

	t.Run("provided cert+key", func(t *testing.T) {
		base(t)
		t.Setenv("QUASAR_TLS_CERT", "/certs/c.pem")
		t.Setenv("QUASAR_TLS_KEY", "/certs/k.pem")
		c, err := Load()
		if err != nil {
			t.Fatal(err)
		}
		if !c.TLSProvided() {
			t.Fatal("both cert+key set, TLSProvided should be true")
		}
	})

	t.Run("only cert is an error", func(t *testing.T) {
		base(t)
		t.Setenv("QUASAR_TLS_CERT", "/certs/c.pem")
		if _, err := Load(); err == nil {
			t.Fatal("setting only QUASAR_TLS_CERT should fail")
		}
	})

	t.Run("only key is an error", func(t *testing.T) {
		base(t)
		t.Setenv("QUASAR_TLS_KEY", "/certs/k.pem")
		if _, err := Load(); err == nil {
			t.Fatal("setting only QUASAR_TLS_KEY should fail")
		}
	})

	t.Run("cert+key ignored when off", func(t *testing.T) {
		base(t)
		t.Setenv("QUASAR_TLS", "off")
		t.Setenv("QUASAR_TLS_CERT", "/certs/c.pem") // only one, but TLS off -> not validated
		c, err := Load()
		if err != nil {
			t.Fatal("half-set cert/key must not fail when TLS is off")
		}
		if c.TLSEnabled() {
			t.Fatal("should stay disabled")
		}
	})

	t.Run("unknown mode is an error", func(t *testing.T) {
		base(t)
		t.Setenv("QUASAR_TLS", "yes")
		if _, err := Load(); err == nil {
			t.Fatal("unknown QUASAR_TLS value should fail")
		}
	})

	t.Run("bad TLS addr is an error", func(t *testing.T) {
		base(t)
		t.Setenv("QUASAR_TLS_ADDR", "no-port")
		if _, err := Load(); err == nil {
			t.Fatal("QUASAR_TLS_ADDR without a port should fail")
		}
	})
}

func TestLoadHTTPRedirect(t *testing.T) {
	base := func(t *testing.T) {
		t.Setenv("DATABASE_URL", "postgres://test")
		t.Setenv("ENROLLMENT_TOKEN", "test-token")
	}

	t.Run("default: on, port from TLSAddr", func(t *testing.T) {
		base(t)
		c, err := Load()
		if err != nil {
			t.Fatal(err)
		}
		if !c.HTTPRedirectEnabled() {
			t.Fatal("redirect should default to enabled when TLS is on")
		}
		if c.TLSRedirectPort != "8443" {
			t.Fatalf("TLSRedirectPort = %q, want 8443 (derived from TLSAddr)", c.TLSRedirectPort)
		}
	})

	t.Run("explicit external port wins", func(t *testing.T) {
		base(t)
		t.Setenv("QUASAR_TLS_REDIRECT_PORT", "18443")
		c, err := Load()
		if err != nil {
			t.Fatal(err)
		}
		if c.TLSRedirectPort != "18443" {
			t.Fatalf("TLSRedirectPort = %q, want 18443", c.TLSRedirectPort)
		}
	})

	t.Run("off disables", func(t *testing.T) {
		base(t)
		t.Setenv("QUASAR_HTTP_REDIRECT", "off")
		c, err := Load()
		if err != nil {
			t.Fatal(err)
		}
		if c.HTTPRedirectEnabled() {
			t.Fatal("QUASAR_HTTP_REDIRECT=off should disable the redirect")
		}
	})

	t.Run("tls off implies redirect off", func(t *testing.T) {
		base(t)
		t.Setenv("QUASAR_TLS", "off")
		c, err := Load()
		if err != nil {
			t.Fatal(err)
		}
		if c.HTTPRedirectEnabled() {
			t.Fatal("no HTTPS listener → nothing to redirect to")
		}
	})

	t.Run("unknown mode is an error", func(t *testing.T) {
		base(t)
		t.Setenv("QUASAR_HTTP_REDIRECT", "yes")
		if _, err := Load(); err == nil {
			t.Fatal("unknown QUASAR_HTTP_REDIRECT value should fail")
		}
	})

	t.Run("non-numeric redirect port is an error", func(t *testing.T) {
		base(t)
		t.Setenv("QUASAR_TLS_REDIRECT_PORT", "https")
		if _, err := Load(); err == nil {
			t.Fatal("non-numeric QUASAR_TLS_REDIRECT_PORT should fail")
		}
	})
}

func TestLoadValidatesClientVersions(t *testing.T) {
	base := func(t *testing.T) {
		t.Setenv("DATABASE_URL", "postgres://test")
		t.Setenv("ENROLLMENT_TOKEN", "test-token")
	}

	for _, tc := range []struct {
		name, min, latest string
		wantErr           bool
	}{
		{"both unset — permissive", "", "", false},
		{"valid floor", "1.2.0", "", false},
		{"leading v accepted", "v1.2.0", "", false},
		{"valid floor + latest", "1.2.0", "1.5.0", false},
		{"bad floor", "1.2", "", true},
		{"non-numeric floor", "1.2.x", "", true},
		{"overflow floor — must NOT pass (same grammar as the gate)", "99999999999999999999.0.0", "", true},
		{"pre-release floor rejected", "1.2.0-rc1", "", true},
		{"bad latest", "1.2.0", "nope", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base(t)
			t.Setenv("QUASAR_MIN_CLIENT_VERSION", tc.min)
			t.Setenv("QUASAR_LATEST_CLIENT_VERSION", tc.latest)
			c, err := Load()
			if (err != nil) != tc.wantErr {
				t.Fatalf("Load error=%v wantErr=%v", err, tc.wantErr)
			}
			if !tc.wantErr {
				if c.MinClientVersion != tc.min {
					t.Fatalf("MinClientVersion=%q want %q", c.MinClientVersion, tc.min)
				}
				if c.LatestClientVersion != tc.latest {
					t.Fatalf("LatestClientVersion=%q want %q", c.LatestClientVersion, tc.latest)
				}
			}
		})
	}
}

// TestLoadVramAdmissionKnobs covers the #383 §4.3 knobs. These are interpolated
// into admission SQL (`make_interval(secs => $n)`), so a bad value must fail
// STARTUP: reaching the query, an empty or non-numeric value would either 500
// every launch or — worse — make the whole veto clause NULL, which HAVING treats
// as false and which silently fails CLOSED, inverting the design's central
// fail-open property (review finding #12).
func TestLoadVramAdmissionKnobs(t *testing.T) {
	base := func(t *testing.T) {
		t.Setenv("DATABASE_URL", "postgres://test")
		t.Setenv("ENROLLMENT_TOKEN", "test-token")
	}

	t.Run("defaults", func(t *testing.T) {
		base(t)
		c, err := Load()
		if err != nil {
			t.Fatal(err)
		}
		if c.VramMinFreeMB != 1024 {
			t.Fatalf("VramMinFreeMB = %d, want 1024", c.VramMinFreeMB)
		}
		// The debit defaults to the floor, not to a second hard-coded number.
		if c.VramInflightEstimateMB != 1024 {
			t.Fatalf("VramInflightEstimateMB = %d, want 1024 (= the floor)", c.VramInflightEstimateMB)
		}
		if c.VramStalenessSecs != 20 {
			t.Fatalf("VramStalenessSecs = %d, want 20", c.VramStalenessSecs)
		}
	})

	t.Run("inflight follows a raised floor", func(t *testing.T) {
		base(t)
		t.Setenv("QUASAR_VRAM_MIN_FREE_MB", "4096")
		c, err := Load()
		if err != nil {
			t.Fatal(err)
		}
		if c.VramInflightEstimateMB != 4096 {
			t.Fatalf("VramInflightEstimateMB = %d, want 4096", c.VramInflightEstimateMB)
		}
	})

	t.Run("inflight is independently settable", func(t *testing.T) {
		base(t)
		t.Setenv("QUASAR_VRAM_MIN_FREE_MB", "4096")
		t.Setenv("QUASAR_VRAM_INFLIGHT_ESTIMATE_MB", "512")
		c, err := Load()
		if err != nil {
			t.Fatal(err)
		}
		// Split knob (review finding #13): raising the floor for safety must not
		// also multiply the burst debit.
		if c.VramInflightEstimateMB != 512 {
			t.Fatalf("VramInflightEstimateMB = %d, want 512", c.VramInflightEstimateMB)
		}
	})

	t.Run("zero floor is the documented kill switch", func(t *testing.T) {
		base(t)
		t.Setenv("QUASAR_VRAM_MIN_FREE_MB", "0")
		c, err := Load()
		if err != nil {
			t.Fatalf("0 must be accepted (kill switch): %v", err)
		}
		if c.VramMinFreeMB != 0 {
			t.Fatalf("VramMinFreeMB = %d, want 0", c.VramMinFreeMB)
		}
	})

	for _, tc := range []struct {
		name, key, value string
	}{
		{"non-numeric floor", "QUASAR_VRAM_MIN_FREE_MB", "lots"},
		{"negative floor", "QUASAR_VRAM_MIN_FREE_MB", "-1"},
		{"float floor", "QUASAR_VRAM_MIN_FREE_MB", "1024.5"},
		{"non-numeric inflight", "QUASAR_VRAM_INFLIGHT_ESTIMATE_MB", "some"},
		{"negative inflight", "QUASAR_VRAM_INFLIGHT_ESTIMATE_MB", "-512"},
		{"non-numeric staleness", "QUASAR_VRAM_STALENESS_SECS", "soon"},
		{"zero staleness would mark every sample stale", "QUASAR_VRAM_STALENESS_SECS", "0"},
		{"negative staleness would make every sample eternal", "QUASAR_VRAM_STALENESS_SECS", "-20"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base(t)
			t.Setenv(tc.key, tc.value)
			if _, err := Load(); err == nil {
				t.Fatalf("%s=%q must fail startup, not reach the admission SQL", tc.key, tc.value)
			} else if !strings.Contains(err.Error(), tc.key) {
				t.Fatalf("error should name the offending knob: %v", err)
			}
		})
	}
}

func TestLoadValidatesAllowedOrigins(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://test")
	t.Setenv("ENROLLMENT_TOKEN", "test-token")

	for _, tc := range []struct {
		name, origins string
		wantErr       bool
	}{
		{"unset", "", false},
		{"valid comma list", " https://console.example, http://[::1]:8080 ", false},
		{"path", "https://console.example/path", true},
		{"query", "https://console.example?bad", true},
		{"credentials", "https://user@console.example", true},
		{"unsupported scheme", "file://console.example", true},
		{"opaque", "null", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("QUASAR_ALLOWED_ORIGINS", tc.origins)
			_, err := Load()
			if (err != nil) != tc.wantErr {
				t.Fatalf("Load error=%v wantErr=%v", err, tc.wantErr)
			}
			if tc.wantErr && !strings.Contains(err.Error(), "QUASAR_ALLOWED_ORIGINS contains invalid origin") {
				t.Fatalf("error=%q", err)
			}
		})
	}
}

// --- Cover artwork (UI-P7) ---------------------------------------------------

func TestLoadArtworkDefaultsToOff(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://test")
	t.Setenv("ENROLLMENT_TOKEN", "test-token")

	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	// THE acceptance property: an unconfigured deployment behaves exactly as it
	// did before UI-P7 — no credential anywhere, so no third-party request is
	// ever made. Whether a provider actually materialises is now decided per use
	// by artwork.SecretProviderSource (the key may also live in the encrypted
	// secrets store); this asserts the environment half of that input.
	if c.ArtworkAPIKey != "" {
		t.Fatal("artwork API key must be empty with none configured")
	}
	if c.ArtworkProviderDisabled() {
		t.Fatal("the provider should not read as explicitly disabled by default")
	}
	if c.ArtworkDir != "/var/lib/quasar-control/artwork" {
		t.Fatalf("ArtworkDir = %q", c.ArtworkDir)
	}
	if c.ArtworkMaxBytes != 8<<20 {
		t.Fatalf("ArtworkMaxBytes = %d, want %d", c.ArtworkMaxBytes, 8<<20)
	}
	if c.ArtworkInterval.String() != "15m0s" {
		t.Fatalf("ArtworkInterval = %v", c.ArtworkInterval)
	}
}

func TestLoadArtworkEnvKeyIsCarriedAsAFallback(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://test")
	t.Setenv("ENROLLMENT_TOKEN", "test-token")
	t.Setenv("QUASAR_STEAMGRIDDB_API_KEY", "an-env-key")

	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	// The env var must survive the move to the secrets store: an operator who
	// already set it must not lose artwork on upgrade.
	if c.ArtworkAPIKey == "" {
		t.Fatal("QUASAR_STEAMGRIDDB_API_KEY must still be read as the fallback credential")
	}
	if c.ArtworkProviderDisabled() {
		t.Fatal("a key in the environment must not read as disabled")
	}

	// "none" is the explicit off switch even with a key still in the environment
	// — an operator disabling the lookup should not have to delete their key,
	// and must not have it re-enabled by someone typing one into the admin UI.
	t.Setenv("QUASAR_ARTWORK_PROVIDER", "none")
	c, err = Load()
	if err != nil {
		t.Fatal(err)
	}
	if !c.ArtworkProviderDisabled() {
		t.Fatal("QUASAR_ARTWORK_PROVIDER=none must disable the provider")
	}
}

// A typo must FAIL STARTUP, not silently leave the feature dark: an operator who
// set a key and misspelled the provider would otherwise get no explanation.
func TestLoadRejectsUnknownArtworkProvider(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://test")
	t.Setenv("ENROLLMENT_TOKEN", "test-token")
	t.Setenv("QUASAR_ARTWORK_PROVIDER", "steamgridb") // one 'd' short

	_, err := Load()
	if err == nil {
		t.Fatal("an unknown provider must fail startup")
	}
	if !strings.Contains(err.Error(), "QUASAR_ARTWORK_PROVIDER") {
		t.Fatalf("error should name the variable, got %v", err)
	}
}

func TestLoadRejectsBadArtworkKnobs(t *testing.T) {
	for _, tc := range []struct{ key, val string }{
		{"QUASAR_ARTWORK_MAX_BYTES", "not-a-number"},
		{"QUASAR_ARTWORK_MAX_BYTES", "0"},
		{"QUASAR_ARTWORK_SWEEP_INTERVAL", "soon"},
		{"QUASAR_ARTWORK_SWEEP_INTERVAL", "0s"},
		{"QUASAR_ARTWORK_SWEEP_INTERVAL", "-5m"},
	} {
		t.Run(tc.key+"="+tc.val, func(t *testing.T) {
			t.Setenv("DATABASE_URL", "postgres://test")
			t.Setenv("ENROLLMENT_TOKEN", "test-token")
			t.Setenv(tc.key, tc.val)
			if _, err := Load(); err == nil {
				t.Fatalf("%s=%q must fail startup", tc.key, tc.val)
			}
		})
	}
}

// TestLoadPprofAddr covers the one knob in this file whose set-but-empty value
// means something different from unset (PROF-01, #388): "" is the documented
// off switch, so it must NOT collapse into the default the way every other
// envOr-backed knob does.
func TestLoadPprofAddr(t *testing.T) {
	base := func(t *testing.T) {
		t.Setenv("DATABASE_URL", "postgres://test")
		t.Setenv("ENROLLMENT_TOKEN", "test-token")
	}

	t.Run("defaults to loopback and enabled", func(t *testing.T) {
		base(t)
		c, err := Load()
		if err != nil {
			t.Fatal(err)
		}
		// The loopback default IS the security posture: pprof ships enabled, and
		// what keeps it off the internet is this address, not a middleware.
		if c.PprofAddr != "127.0.0.1:6060" {
			t.Fatalf("PprofAddr = %q, want 127.0.0.1:6060", c.PprofAddr)
		}
		if !c.PprofEnabled() {
			t.Fatal("pprof should default to enabled")
		}
	})

	t.Run("off disables", func(t *testing.T) {
		base(t)
		t.Setenv("QUASAR_PPROF_ADDR", "off")
		c, err := Load()
		if err != nil {
			t.Fatal(err)
		}
		if c.PprofEnabled() {
			t.Fatal("QUASAR_PPROF_ADDR=off should disable the listener")
		}
	})

	t.Run("OFF is case-insensitive", func(t *testing.T) {
		base(t)
		t.Setenv("QUASAR_PPROF_ADDR", "OFF")
		c, err := Load()
		if err != nil {
			t.Fatal(err)
		}
		if c.PprofEnabled() {
			t.Fatal("QUASAR_PPROF_ADDR=OFF should disable the listener")
		}
	})

	t.Run("explicitly empty disables", func(t *testing.T) {
		base(t)
		t.Setenv("QUASAR_PPROF_ADDR", "")
		c, err := Load()
		if err != nil {
			t.Fatal(err)
		}
		if c.PprofEnabled() {
			t.Fatal("QUASAR_PPROF_ADDR= (empty) should disable the listener, not fall back to the default")
		}
	})

	t.Run("custom address is honoured", func(t *testing.T) {
		base(t)
		t.Setenv("QUASAR_PPROF_ADDR", "127.0.0.1:7070")
		c, err := Load()
		if err != nil {
			t.Fatal(err)
		}
		if c.PprofAddr != "127.0.0.1:7070" || !c.PprofEnabled() {
			t.Fatalf("PprofAddr = %q enabled=%v", c.PprofAddr, c.PprofEnabled())
		}
	})

	t.Run("a portless address fails startup", func(t *testing.T) {
		base(t)
		t.Setenv("QUASAR_PPROF_ADDR", "127.0.0.1")
		_, err := Load()
		if err == nil {
			t.Fatal("a portless QUASAR_PPROF_ADDR must fail startup rather than silently leaving pprof off")
		}
		if !strings.Contains(err.Error(), "QUASAR_PPROF_ADDR") {
			t.Fatalf("error should name the variable, got %v", err)
		}
	})
}

// TestLoadLibraryScanIntervalOverride — admin-libraries amendment (2026-08-01):
// QUASAR_LIBRARY_SCAN_INTERVAL is now an OVERRIDE, distinguishable from
// "unset" via LibraryScanIntervalSet, not a default silently applied whether
// or not the operator wrote it.
func TestLoadLibraryScanIntervalOverride(t *testing.T) {
	base := func(t *testing.T) {
		t.Setenv("DATABASE_URL", "postgres://test")
		t.Setenv("ENROLLMENT_TOKEN", "test-token")
	}

	t.Run("unset leaves Set false", func(t *testing.T) {
		base(t)
		c, err := Load()
		if err != nil {
			t.Fatal(err)
		}
		if c.LibraryScanIntervalSet {
			t.Fatal("LibraryScanIntervalSet = true with the env var unset, want false — the database column must decide")
		}
	})

	t.Run("set records the value and Set=true, including the 0 kill switch", func(t *testing.T) {
		base(t)
		t.Setenv("QUASAR_LIBRARY_SCAN_INTERVAL", "0")
		c, err := Load()
		if err != nil {
			t.Fatal(err)
		}
		if !c.LibraryScanIntervalSet {
			t.Fatal("LibraryScanIntervalSet = false with the env var set to 0, want true")
		}
		if c.LibraryScanInterval != 0 {
			t.Fatalf("LibraryScanInterval = %v, want 0 (the kill switch)", c.LibraryScanInterval)
		}
	})

	t.Run("a negative value fails startup", func(t *testing.T) {
		base(t)
		t.Setenv("QUASAR_LIBRARY_SCAN_INTERVAL", "-1h")
		if _, err := Load(); err == nil {
			t.Fatal("a negative QUASAR_LIBRARY_SCAN_INTERVAL must fail startup")
		}
	})
}

// TestLoadSteamAppDetailsLookupOverride — same override-not-default rule for
// the appdetails switch.
func TestLoadSteamAppDetailsLookupOverride(t *testing.T) {
	base := func(t *testing.T) {
		t.Setenv("DATABASE_URL", "postgres://test")
		t.Setenv("ENROLLMENT_TOKEN", "test-token")
	}

	t.Run("unset leaves Set false", func(t *testing.T) {
		base(t)
		c, err := Load()
		if err != nil {
			t.Fatal(err)
		}
		if c.SteamAppDetailsLookupSet {
			t.Fatal("SteamAppDetailsLookupSet = true with the env var unset, want false — the database column must decide")
		}
	})

	t.Run("set records the value and Set=true", func(t *testing.T) {
		base(t)
		t.Setenv("QUASAR_STEAM_APPDETAILS_LOOKUP", "true")
		c, err := Load()
		if err != nil {
			t.Fatal(err)
		}
		if !c.SteamAppDetailsLookupSet || !c.SteamAppDetailsLookup {
			t.Fatalf("Set=%v Lookup=%v, want true/true", c.SteamAppDetailsLookupSet, c.SteamAppDetailsLookup)
		}
	})

	t.Run("an unparseable value fails startup", func(t *testing.T) {
		base(t)
		t.Setenv("QUASAR_STEAM_APPDETAILS_LOOKUP", "sorta")
		if _, err := Load(); err == nil {
			t.Fatal("an unparseable QUASAR_STEAM_APPDETAILS_LOOKUP must fail startup")
		}
	})
}

func TestTelemetryRetentionKnobs(t *testing.T) {
	base := func(t *testing.T) {
		t.Setenv("DATABASE_URL", "postgres://test")
		t.Setenv("ENROLLMENT_TOKEN", "test-token")
	}

	t.Run("defaults are 1h rolling / 24h post-mortem", func(t *testing.T) {
		base(t)
		c, err := Load()
		if err != nil {
			t.Fatal(err)
		}
		p := c.TelemetryPolicy()
		if p.Rolling != time.Hour || p.PostMortem != 24*time.Hour {
			t.Fatalf("policy = %+v, want 1h/24h", p)
		}
		if err := p.Validate(); err != nil {
			t.Fatalf("the shipped default must validate: %v", err)
		}
	})

	t.Run("both knobs are honoured", func(t *testing.T) {
		base(t)
		t.Setenv("QUASAR_TELEMETRY_ROLLING_WINDOW", "30m")
		t.Setenv("QUASAR_TELEMETRY_POSTMORTEM_RETENTION", "72h")
		c, err := Load()
		if err != nil {
			t.Fatal(err)
		}
		p := c.TelemetryPolicy()
		if p.Rolling != 30*time.Minute || p.PostMortem != 72*time.Hour {
			t.Fatalf("policy = %+v, want 30m/72h", p)
		}
	})

	// A post-mortem shorter than the rolling window would delete the evidence it
	// exists to keep, so it fails at BOOT rather than on the first sweep.
	t.Run("post-mortem shorter than the window fails startup", func(t *testing.T) {
		base(t)
		t.Setenv("QUASAR_TELEMETRY_ROLLING_WINDOW", "6h")
		t.Setenv("QUASAR_TELEMETRY_POSTMORTEM_RETENTION", "1h")
		_, err := Load()
		if err == nil {
			t.Fatal("expected a startup error")
		}
		if !strings.Contains(err.Error(), "POSTMORTEM") {
			t.Fatalf("the error must name the knobs: %v", err)
		}
	})

	t.Run("an unparseable duration fails startup", func(t *testing.T) {
		base(t)
		t.Setenv("QUASAR_TELEMETRY_ROLLING_WINDOW", "one hour")
		if _, err := Load(); err == nil {
			t.Fatal("expected a startup error for a bad duration")
		}
	})

	t.Run("a zero window fails startup rather than disabling retention", func(t *testing.T) {
		base(t)
		t.Setenv("QUASAR_TELEMETRY_ROLLING_WINDOW", "0")
		if _, err := Load(); err == nil {
			t.Fatal("0 is not a kill switch here; unbounded telemetry is not an option an operator gets")
		}
	})
}

// TestLoadAccessLog covers #517: QUASAR_ACCESS_LOG must default to "errors"
// (quiet unless something's actually wrong), accept the "off"/"errors"/"all"
// vocabulary, keep accepting the knob's original boolean form for backward
// compatibility, and reject anything else at startup rather than silently
// picking a default.
func TestLoadAccessLog(t *testing.T) {
	base := func(t *testing.T) {
		t.Setenv("DATABASE_URL", "postgres://test")
		t.Setenv("ENROLLMENT_TOKEN", "test-token")
	}

	t.Run("default is errors", func(t *testing.T) {
		base(t)
		c, err := Load()
		if err != nil {
			t.Fatal(err)
		}
		if c.AccessLog != "errors" {
			t.Fatalf("AccessLog = %q, want the default %q", c.AccessLog, "errors")
		}
	})

	for _, tc := range []struct{ in, want string }{
		{"off", "off"},
		{"OFF", "off"},
		{"errors", "errors"},
		{"Errors", "errors"},
		{"all", "all"},
		{"true", "all"},
		{"1", "all"},
		{"false", "off"},
		{"0", "off"},
	} {
		t.Run(tc.in+"->"+tc.want, func(t *testing.T) {
			base(t)
			t.Setenv("QUASAR_ACCESS_LOG", tc.in)
			c, err := Load()
			if err != nil {
				t.Fatal(err)
			}
			if c.AccessLog != tc.want {
				t.Fatalf("QUASAR_ACCESS_LOG=%q -> AccessLog = %q, want %q", tc.in, c.AccessLog, tc.want)
			}
		})
	}

	t.Run("unknown value fails startup", func(t *testing.T) {
		base(t)
		t.Setenv("QUASAR_ACCESS_LOG", "verbose")
		_, err := Load()
		if err == nil {
			t.Fatal("an unknown QUASAR_ACCESS_LOG value should fail startup")
		}
		if !strings.Contains(err.Error(), "QUASAR_ACCESS_LOG") {
			t.Fatalf("error should name the variable, got %v", err)
		}
	})
}

// TestLoadTrustedProxies covers #438: QUASAR_TRUSTED_PROXIES must default to
// EMPTY (headers never consulted — the pre-#438 behaviour, and the right one
// for a direct-LAN install), accept a comma-separated CIDR list plus bare IPs
// widened to single-host networks, and fail startup on a typo rather than
// silently leaving every client sharing one rate-limit budget.
func TestLoadTrustedProxies(t *testing.T) {
	base := func(t *testing.T) {
		t.Setenv("DATABASE_URL", "postgres://test")
		t.Setenv("ENROLLMENT_TOKEN", "test-token")
	}

	t.Run("default is empty", func(t *testing.T) {
		base(t)
		c, err := Load()
		if err != nil {
			t.Fatal(err)
		}
		if len(c.TrustedProxies) != 0 {
			t.Fatalf("TrustedProxies = %v, want empty by default", c.TrustedProxies)
		}
	})

	for _, tc := range []struct {
		in   string
		want []string
	}{
		{"172.18.0.0/16", []string{"172.18.0.0/16"}},
		{"172.18.0.0/16, 10.9.0.0/16", []string{"172.18.0.0/16", "10.9.0.0/16"}},
		{"fd00::/8", []string{"fd00::/8"}},
		{"192.0.2.7", []string{"192.0.2.7/32"}},
		{"2001:db8::1", []string{"2001:db8::1/128"}},
		{" 10.0.0.0/8 , ", []string{"10.0.0.0/8"}},
	} {
		t.Run(tc.in, func(t *testing.T) {
			base(t)
			t.Setenv("QUASAR_TRUSTED_PROXIES", tc.in)
			c, err := Load()
			if err != nil {
				t.Fatal(err)
			}
			if len(c.TrustedProxies) != len(tc.want) {
				t.Fatalf("TrustedProxies = %v, want %v", c.TrustedProxies, tc.want)
			}
			for i, w := range tc.want {
				if got := c.TrustedProxies[i].String(); got != w {
					t.Fatalf("TrustedProxies[%d] = %q, want %q", i, got, w)
				}
			}
		})
	}

	// A /0 trusts the whole internet, so every caller could choose its own
	// limiter key by writing a header — the security control would silently
	// stop existing. That can only be a mistake, so it must not boot.
	for _, wide := range []string{"0.0.0.0/0", "::/0", "172.18.0.0/16,0.0.0.0/0"} {
		t.Run("rejects "+wide, func(t *testing.T) {
			base(t)
			t.Setenv("QUASAR_TRUSTED_PROXIES", wide)
			_, err := Load()
			if err == nil {
				t.Fatalf("QUASAR_TRUSTED_PROXIES=%q should fail startup", wide)
			}
			if !strings.Contains(err.Error(), "QUASAR_TRUSTED_PROXIES") {
				t.Fatalf("error should name the variable, got %v", err)
			}
		})
	}

	// Shorter than /8 is legal but almost certainly wider than intended, so it
	// warns rather than failing.
	t.Run("warns on a prefix shorter than /8", func(t *testing.T) {
		base(t)
		t.Setenv("QUASAR_TRUSTED_PROXIES", "10.0.0.0/4")
		c, err := Load()
		if err != nil {
			t.Fatal(err)
		}
		if len(c.TrustedProxies) != 1 {
			t.Fatalf("TrustedProxies = %v, want the entry accepted", c.TrustedProxies)
		}
		if len(c.Warnings) != 1 || !strings.Contains(c.Warnings[0], "QUASAR_TRUSTED_PROXIES") {
			t.Fatalf("Warnings = %v, want one naming the variable", c.Warnings)
		}
	})

	t.Run("a /8 or narrower does not warn", func(t *testing.T) {
		base(t)
		t.Setenv("QUASAR_TRUSTED_PROXIES", "10.0.0.0/8, 172.18.0.0/16")
		c, err := Load()
		if err != nil {
			t.Fatal(err)
		}
		if len(c.Warnings) != 0 {
			t.Fatalf("Warnings = %v, want none", c.Warnings)
		}
	})

	for _, bad := range []string{"not-a-cidr", "10.0.0.0/8,garbage", "10.0.0.0/64"} {
		t.Run("rejects "+bad, func(t *testing.T) {
			base(t)
			t.Setenv("QUASAR_TRUSTED_PROXIES", bad)
			_, err := Load()
			if err == nil {
				t.Fatalf("QUASAR_TRUSTED_PROXIES=%q should fail startup", bad)
			}
			if !strings.Contains(err.Error(), "QUASAR_TRUSTED_PROXIES") {
				t.Fatalf("error should name the variable, got %v", err)
			}
		})
	}
}

// --- ICE servers (#509) ------------------------------------------------------

// TestLoadICEServersDefaultsToNone pins the default that keeps #509 a
// capability rather than a behaviour change: an existing LAN deployment that
// sets nothing gets no ICE servers, exactly as before the knob existed.
func TestLoadICEServersDefaultsToNone(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://test")
	t.Setenv("ENROLLMENT_TOKEN", "test-token")

	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(c.ICEServers) != 0 {
		t.Fatalf("ICEServers = %v, want none", c.ICEServers)
	}
}

func TestLoadICEServers(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://test")
	t.Setenv("ENROLLMENT_TOKEN", "test-token")

	for _, tc := range []struct {
		name, value string
		wantCount   int
		wantErr     bool
	}{
		{"explicit empty array", "[]", 0, false},
		{"stun only", `[{"urls":["stun:stun.example.net:3478"]}]`, 1, false},
		{"stun and turn", `[{"urls":["stun:s.example.net:3478"]},{"urls":["turn:t.example.net:3478"],"username":"u","credential":"c"}]`, 2, false},
		{"not json", "stun:stun.example.net:3478", 0, true},
		{"turn without credentials", `[{"urls":["turn:t.example.net:3478"]}]`, 0, true},
		{"non-ICE scheme", `[{"urls":["https://stun.example.net"]}]`, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("QUASAR_ICE_SERVERS", tc.value)
			c, err := Load()
			if (err != nil) != tc.wantErr {
				t.Fatalf("Load error=%v wantErr=%v", err, tc.wantErr)
			}
			if tc.wantErr {
				if !strings.Contains(err.Error(), "QUASAR_ICE_SERVERS") {
					t.Fatalf("error does not name the knob: %v", err)
				}
				return
			}
			if len(c.ICEServers) != tc.wantCount {
				t.Fatalf("got %d servers, want %d", len(c.ICEServers), tc.wantCount)
			}
		})
	}
}

// --- enrollment token (#12) --------------------------------------------------

// A deployment can enroll entirely with admin-minted per-host tokens, so the
// fleet-wide static one is optional. Empty must load, and must stay empty:
// agentws only compares a non-empty configured token, so "" is "the static path
// is off", never a wildcard that matches whatever an agent presents.
func TestLoadEnrollmentTokenIsOptional(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://test")
	t.Setenv("ENROLLMENT_TOKEN", "")

	c, err := Load()
	if err != nil {
		t.Fatalf("Load without ENROLLMENT_TOKEN: %v", err)
	}
	if c.EnrollmentToken != "" {
		t.Fatalf("EnrollmentToken = %q, want empty", c.EnrollmentToken)
	}
}
