package updater

import "testing"

// A compose stack that sets `hostname:` makes $HOSTNAME a DNS name, and
// `docker inspect -- quasar-dev.local` answers "No such object" — the #107
// defect, which this program would inherit by reading $HOSTNAME unconditionally.
func TestSelfContainerIDRejectsAHostnameThatIsNotAContainerID(t *testing.T) {
	t.Setenv("HOSTNAME", "quasar-dev.local")
	if got := SelfContainerID(); got == "quasar-dev.local" {
		t.Fatal("a DNS hostname must never be used as a container reference")
	}
}

func TestContainerIDFromMountinfo(t *testing.T) {
	id := "b2c3d4e5f60718293a4b5c6d7e8f90112233445566778899aabbccddeeff0011"
	body := "1234 1000 0:59 / / rw,relatime - overlay overlay rw,lowerdir=/var/lib/docker/overlay2/" + id + "/diff\n"
	if got := containerIDFromMountinfo(body); got != id {
		t.Fatalf("got %q, want %q", got, id)
	}
	if got := containerIDFromMountinfo("nothing here\n"); got != "" {
		t.Fatalf("got %q, want empty", got)
	}
}
