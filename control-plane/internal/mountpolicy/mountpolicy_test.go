package mountpolicy

import "testing"

// The escape the narrow docker.sock-only check let through, plus the trees that
// reach the host filesystem another way.
func TestRejectsHostEscapeSources(t *testing.T) {
	for _, m := range []string{
		"/var/run/docker.sock:/var/run/docker.sock",
		"/run/docker.sock:/run/docker.sock",
		"/var/run:/hostrun",
		"/run:/hostrun",
		"/var:/hostvar",
		"/var/lib/docker:/hostdocker",
		"/var/lib/containerd:/hostcd",
		"/proc/1/root:/host",
		"/proc:/hostproc",
		"/sys:/hostsys",
		"/dev:/hostdev",
		"/etc:/hostetc",
		"/root:/hostroot",
		"/boot:/hostboot",
		"/lib/modules:/lib/modules",
		"/:/host",
		"/var/run/../run/docker.sock:/s",
		"/var/lib/quasar/homes/../../../etc:/x",
	} {
		if err := Validate(m); err == nil {
			t.Errorf("Validate(%q) = nil, want a rejection", m)
		}
	}
}

func TestRejectsMalformedEntries(t *testing.T) {
	for _, m := range []string{
		"",
		"/opt/games",
		"relative:/dst",
		"/opt/games:relative",
		"/opt/games:/",
		":/dst",
		"/opt/games:",
		"/opt/games:/dst\nmore",
	} {
		if err := Validate(m); err == nil {
			t.Errorf("Validate(%q) = nil, want a rejection", m)
		}
	}
}

// A component-wise prefix: a sibling directory must not inherit the deny.
func TestAllowsOrdinaryHostPaths(t *testing.T) {
	for _, m := range []string{
		"/var/lib/quasar/homes/alice/steam:/home/quasar",
		"/opt/games:/games:ro",
		"/srv/media:/media",
		"/var/lib/dockerfoo:/x",
		"/etcetera/config:/cfg",
		"/devices/x:/x",
	} {
		if err := Validate(m); err != nil {
			t.Errorf("Validate(%q) = %v, want nil", m, err)
		}
	}
}

func TestValidateAllReturnsTheFirstRejection(t *testing.T) {
	if err := ValidateAll([]string{"/opt/a:/a", "/var/run:/hostrun", "/opt/b:/b"}); err == nil {
		t.Fatal("ValidateAll = nil, want a rejection")
	}
	if err := ValidateAll(nil); err != nil {
		t.Fatalf("ValidateAll(nil) = %v, want nil", err)
	}
}
