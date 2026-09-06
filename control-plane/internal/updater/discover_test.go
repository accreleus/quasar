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
	layer := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	id := "b2c3d4e5f60718293a4b5c6d7e8f90112233445566778899aabbccddeeff0011"
	root := "4592 4484 0:81 /btrfs/subvolumes/" + layer + " / rw - btrfs /dev/loop2 rw,subvol=/btrfs/subvolumes/" + layer + "\n"
	identity := "4600 4592 0:81 /var/lib/docker/containers/" + id + "/hosts /etc/hosts rw - btrfs /dev/loop2 rw\n"
	if got := containerIDFromMountinfo(root + identity); got != id {
		t.Fatalf("got %q, want container %q (not Btrfs filesystem id)", got, id)
	}
	if got := containerIDFromMountinfo(root); got != "" {
		t.Fatalf("a filesystem id is not a container id: %q", got)
	}
}
