package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

// writeSecret writes a secret file the way the recovery actor's secrets volume
// holds one: a value and a trailing newline.
func writeSecret(t *testing.T, name, contents string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func ownedDatabaseEnv(t *testing.T) {
	t.Helper()
	t.Setenv("DATABASE_URL", "")
	t.Setenv("QUASAR_DATABASE_HOST", "postgres")
	t.Setenv("QUASAR_DATABASE_PASSWORD", "")
	t.Setenv("ENROLLMENT_TOKEN", "")
}

func TestDatabasePasswordFileCompletesTheHostForm(t *testing.T) {
	ownedDatabaseEnv(t)
	t.Setenv("QUASAR_DATABASE_PASSWORD_FILE", writeSecret(t, "db-password", "s3cret /?#@ and %\n"))
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	db, err := pgx.ParseConfig(c.DatabaseURL)
	if err != nil {
		t.Fatal("generated database connection is invalid")
	}
	if db.Password != "s3cret /?#@ and %" {
		t.Fatal("password file did not round trip with its trailing newline trimmed")
	}
}

func TestDatabasePasswordFileRefusals(t *testing.T) {
	const secret = "never-in-an-error"
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T)
		want  string
	}{
		{"both set", func(t *testing.T) {
			t.Setenv("QUASAR_DATABASE_PASSWORD", secret)
			t.Setenv("QUASAR_DATABASE_PASSWORD_FILE", writeSecret(t, "p", secret))
		}, "both set"},
		{"empty file", func(t *testing.T) {
			t.Setenv("QUASAR_DATABASE_PASSWORD_FILE", writeSecret(t, "p", " \n\n"))
		}, "empty"},
		{"unreadable file", func(t *testing.T) {
			t.Setenv("QUASAR_DATABASE_PASSWORD_FILE", filepath.Join(t.TempDir(), "missing"))
		}, "cannot read"},
		{"with DATABASE_URL", func(t *testing.T) {
			t.Setenv("DATABASE_URL", "postgres://u:"+secret+"@db/quasar")
			t.Setenv("QUASAR_DATABASE_PASSWORD_FILE", writeSecret(t, "p", secret))
		}, "DATABASE_URL"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ownedDatabaseEnv(t)
			tc.setup(t)
			_, err := Load()
			if err == nil {
				t.Fatal("Load succeeded, want a config error")
			}
			msg := err.Error()
			if !strings.Contains(msg, "QUASAR_DATABASE_PASSWORD_FILE") || !strings.Contains(msg, tc.want) {
				t.Fatalf("error %q does not name the variable and %q", msg, tc.want)
			}
			if strings.Contains(msg, secret) {
				t.Fatal("the error quotes the secret")
			}
		})
	}
}

func TestSecretKeyFile(t *testing.T) {
	const key = "1:AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8="
	t.Run("read and trimmed", func(t *testing.T) {
		ownedDatabaseEnv(t)
		t.Setenv("DATABASE_URL", "postgres://test")
		t.Setenv("QUASAR_SECRET_KEY", "")
		t.Setenv("QUASAR_SECRET_KEY_FILE", writeSecret(t, "k", key+"\n"))
		c, err := Load()
		if err != nil {
			t.Fatal(err)
		}
		if c.SecretKey != key {
			t.Fatal("secret key file not read verbatim, trailing newline trimmed")
		}
	})
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T)
	}{
		{"both set", func(t *testing.T) {
			t.Setenv("QUASAR_SECRET_KEY", key)
			t.Setenv("QUASAR_SECRET_KEY_FILE", writeSecret(t, "k", key))
		}},
		{"empty file", func(t *testing.T) {
			t.Setenv("QUASAR_SECRET_KEY", "")
			t.Setenv("QUASAR_SECRET_KEY_FILE", writeSecret(t, "k", "\n"))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ownedDatabaseEnv(t)
			t.Setenv("DATABASE_URL", "postgres://test")
			tc.setup(t)
			_, err := Load()
			if err == nil || !strings.Contains(err.Error(), "QUASAR_SECRET_KEY_FILE") || strings.Contains(err.Error(), key) {
				t.Fatalf("err = %v, want one naming QUASAR_SECRET_KEY_FILE without the key", err)
			}
		})
	}
}

func TestLocalEnrollment(t *testing.T) {
	base := func(t *testing.T) {
		ownedDatabaseEnv(t)
		t.Setenv("DATABASE_URL", "postgres://test")
	}
	t.Run("neither", func(t *testing.T) {
		base(t)
		c, err := Load()
		if err != nil {
			t.Fatal(err)
		}
		if c.LocalEnrollmentToken != "" || c.LocalEnrollmentNodeName != "" {
			t.Fatal("a local enrollment was configured from nothing")
		}
	})
	t.Run("both", func(t *testing.T) {
		base(t)
		t.Setenv("QUASAR_LOCAL_ENROLLMENT_FILE", writeSecret(t, "local", "tok-123\n"))
		t.Setenv("QUASAR_LOCAL_ENROLLMENT_NODE_NAME", "living-room-pc")
		c, err := Load()
		if err != nil {
			t.Fatal(err)
		}
		if c.LocalEnrollmentToken != "tok-123" || c.LocalEnrollmentNodeName != "living-room-pc" {
			t.Fatalf("local enrollment = %q for %q", c.LocalEnrollmentToken, c.LocalEnrollmentNodeName)
		}
	})
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T)
		want  string
	}{
		{"file only", func(t *testing.T) {
			t.Setenv("QUASAR_LOCAL_ENROLLMENT_FILE", writeSecret(t, "local", "tok"))
		}, "QUASAR_LOCAL_ENROLLMENT_NODE_NAME"},
		{"node name only", func(t *testing.T) {
			t.Setenv("QUASAR_LOCAL_ENROLLMENT_NODE_NAME", "living-room-pc")
		}, "QUASAR_LOCAL_ENROLLMENT_FILE"},
		{"empty file", func(t *testing.T) {
			t.Setenv("QUASAR_LOCAL_ENROLLMENT_FILE", writeSecret(t, "local", "\n"))
			t.Setenv("QUASAR_LOCAL_ENROLLMENT_NODE_NAME", "living-room-pc")
		}, "empty"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base(t)
			tc.setup(t)
			_, err := Load()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want one naming %q", err, tc.want)
			}
		})
	}
}

// control-api.md amendment 14 §"Enrollment": one WARN when a static value is
// set, nothing when it is unset.
func TestStaticEnrollmentTokenWarnsOnlyWhenSet(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://test")
	t.Setenv("ENROLLMENT_TOKEN", "")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Warnings) != 0 {
		t.Fatalf("unset ENROLLMENT_TOKEN warned: %v", c.Warnings)
	}

	t.Setenv("ENROLLMENT_TOKEN", "a-static-value")
	c, err = Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Warnings) != 1 {
		t.Fatalf("Warnings = %v, want exactly one", c.Warnings)
	}
	w := c.Warnings[0]
	for _, want := range []string{"ENROLLMENT_TOKEN", "deprecated", "RH06-15", "#367", "owned"} {
		if !strings.Contains(w, want) {
			t.Errorf("warning %q lacks %q", w, want)
		}
	}
	if strings.Contains(w, "a-static-value") {
		t.Error("the warning quotes the token")
	}
}

func TestRecoveryControlSocketIsOptional(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://test")
	t.Setenv("QUASAR_RECOVERY_CONTROL_SOCKET", "")
	c, err := Load()
	if err != nil || c.RecoveryControlSocket != "" {
		t.Fatalf("unset socket: %v %q", err, c.RecoveryControlSocket)
	}
	t.Setenv("QUASAR_RECOVERY_CONTROL_SOCKET", "/run/quasar-recovery/control.sock")
	c, err = Load()
	if err != nil || c.RecoveryControlSocket != "/run/quasar-recovery/control.sock" {
		t.Fatalf("set socket: %v %q", err, c.RecoveryControlSocket)
	}
}

func TestMachineShape(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://test")
	for _, tc := range []struct {
		role, node string
		ok         bool
	}{
		{"", "", true},
		{"combined", "living-room-pc", true},
		{"control_only", "attic-server", true},
		{"combined", "", false},
		{"", "living-room-pc", false},
		{"gpu", "gpu-host-2", false},
		{"control-only", "attic-server", false},
	} {
		t.Setenv("QUASAR_MACHINE_ROLE", tc.role)
		t.Setenv("QUASAR_MACHINE_NODE_NAME", tc.node)
		c, err := Load()
		if (err == nil) != tc.ok {
			t.Errorf("role=%q node=%q: err=%v, want ok=%v", tc.role, tc.node, err, tc.ok)
			continue
		}
		if err == nil && (c.MachineRole != tc.role || c.MachineNodeName != tc.node) {
			t.Errorf("role=%q node=%q: loaded %q %q", tc.role, tc.node, c.MachineRole, c.MachineNodeName)
		}
	}
}
