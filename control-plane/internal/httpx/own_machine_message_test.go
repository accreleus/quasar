package httpx

import (
	"strings"
	"testing"
)

// The refusal is shown on the control plane's own machine's host, where no operator
// input can change the answer (docs/configuration.md: the machine shape is "not an
// operator input"), and where the identity is unknown. It must not tell the operator to
// set those variables, and must name the real remedy (#1395).
func TestHostIsOwnMachineMessageIsTrueOnTheOwnMachineAndWhenIdentityIsUnknown(t *testing.T) {
	m := HostIsOwnMachineMessage
	for _, bad := range []string{"set QUASAR_MACHINE", "QUASAR_MACHINE_ROLE", "QUASAR_MACHINE_NODE_NAME"} {
		if strings.Contains(m, bad) {
			t.Errorf("message tells the operator about %q, which is not theirs to set: %s", bad, m)
		}
	}
	for _, want := range []string{"own machine", "uninstall", "quasar-recovery reconfigure"} {
		if !strings.Contains(m, want) {
			t.Errorf("message lacks %q: %s", want, m)
		}
	}
}
