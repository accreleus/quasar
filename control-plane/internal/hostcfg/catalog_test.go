package hostcfg

import "testing"

func TestCatalogCoversKnownKnobs(t *testing.T) {
	c := Catalog()
	want := []string{
		"abr_enabled", "abr_floor_kbps", "abr_floor_ratio", "gop", "slices",
		"target_usage", "queue_buffers", "zerocopy", "idle_timeout_secs",
		"app_boot_timeout_secs",
		"home_root", "nvidia_lib32_path", "encoder", "render_node", "cuda_device",
	}
	got := map[string]Knob{}
	for _, k := range c {
		got[k.Key] = k
	}
	for _, key := range want {
		if _, ok := got[key]; !ok {
			t.Fatalf("catalog missing knob %q", key)
		}
	}
	if got["encoder"].Class != ClassRestart {
		t.Errorf("encoder must be restart-class, got %q", got["encoder"].Class)
	}
	if got["abr_enabled"].Class != ClassLive {
		t.Errorf("abr_enabled must be live-class, got %q", got["abr_enabled"].Class)
	}
}

func TestDefaultsMatchEnvDefaults(t *testing.T) {
	d := Defaults()
	if d["gop"] != float64(60) {
		t.Errorf("gop default = %v, want 60", d["gop"])
	}
	if d["abr_floor_ratio"] != 0.3 {
		t.Errorf("abr_floor_ratio default = %v, want 0.3", d["abr_floor_ratio"])
	}
	if d["encoder"] != "openh264" {
		t.Errorf("encoder default = %v, want openh264", d["encoder"])
	}
	if d["app_boot_timeout_secs"] != float64(300) {
		t.Errorf("app_boot_timeout_secs default = %v, want 300", d["app_boot_timeout_secs"])
	}
	if _, present := d["abr_floor_kbps"]; present {
		t.Errorf("abr_floor_kbps is nullable and unset; must be absent from Defaults()")
	}
}
