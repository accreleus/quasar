package main

// TestEnrollHostComposeMatchesBase — one definition of the node-agent runtime
// contract, guarded by a test (#100).
//
// deploy/enroll-host.sh embeds the compose text it writes on a second host so a
// single `curl | sh` fetch is self-contained. That text must be the
// quasar-node-agent service from deploy/docker-compose.yml with exactly the
// local-stack coupling removed — otherwise a device, mount or env passthrough
// added to the base file silently never reaches enrolled hosts. Same rule as
// deploy/image-contract.json and TestCertForRungMatchesPickCert: when two
// copies of a fact must agree, a test says so.
//
// Deltas the installer is ALLOWED to make (anything else is drift):
//   - no depends_on (there is no local control plane to wait for);
//   - no CONTROL_PLANE_URL / ENROLLMENT_TOKEN (the enrollment string carries both);
//   - QUASAR_ENROLLMENT added, compose-required.
// The NVIDIA overlay it embeds must equal deploy/docker-compose.nvidia.yml.

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"testing"

	"gopkg.in/yaml.v3"
)

const agentService = "quasar-node-agent"

func loadComposeFile(t *testing.T, rel string) map[string]any {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("..", "..", "..", "deploy", rel))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return parseCompose(t, rel, body)
}

func printedCompose(t *testing.T, flag string) map[string]any {
	t.Helper()
	script := filepath.Join("..", "..", "..", "deploy", "enroll-host.sh")
	out, err := exec.Command("sh", script, flag).Output()
	if err != nil {
		t.Fatalf("sh %s %s: %v", script, flag, err)
	}
	return parseCompose(t, "enroll-host.sh "+flag, out)
}

func parseCompose(t *testing.T, name string, body []byte) map[string]any {
	t.Helper()
	var doc map[string]any
	if err := yaml.Unmarshal(body, &doc); err != nil {
		t.Fatalf("parse %s: %v", name, err)
	}
	return doc
}

func service(t *testing.T, doc map[string]any, what string) map[string]any {
	t.Helper()
	services, _ := doc["services"].(map[string]any)
	svc, ok := services[agentService].(map[string]any)
	if !ok {
		t.Fatalf("%s: no services.%s", what, agentService)
	}
	return svc
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// diffMaps reports, per top-level key, what differs — so a failure names the
// env var or mount rather than dumping two 90-line maps.
func diffMaps(t *testing.T, what string, want, got map[string]any) {
	t.Helper()
	for _, k := range sortedKeys(want) {
		gv, ok := got[k]
		if !ok {
			t.Errorf("%s: %q missing from the installer's copy", what, k)
			continue
		}
		wm, wIsMap := want[k].(map[string]any)
		gm, gIsMap := gv.(map[string]any)
		if wIsMap && gIsMap {
			diffMaps(t, what+"."+k, wm, gm)
			continue
		}
		if !reflect.DeepEqual(want[k], gv) {
			t.Errorf("%s: %q differs\n  base:      %#v\n  installer: %#v", what, k, want[k], gv)
		}
	}
	for _, k := range sortedKeys(got) {
		if _, ok := want[k]; !ok {
			t.Errorf("%s: %q is in the installer's copy but not the base file", what, k)
		}
	}
}

func TestEnrollHostComposeMatchesBase(t *testing.T) {
	base := service(t, loadComposeFile(t, "docker-compose.yml"), "docker-compose.yml")
	got := service(t, printedCompose(t, "--print-compose"), "--print-compose")

	// Apply the allowed deltas to the base and demand equality with the rest.
	want := make(map[string]any, len(base))
	for k, v := range base {
		want[k] = v
	}
	delete(want, "depends_on")
	baseEnv, _ := base["environment"].(map[string]any)
	env := make(map[string]any, len(baseEnv))
	for k, v := range baseEnv {
		env[k] = v
	}
	for _, k := range []string{"CONTROL_PLANE_URL", "ENROLLMENT_TOKEN"} {
		if _, ok := env[k]; !ok {
			t.Fatalf("docker-compose.yml no longer sets %s on the agent — update the installer's allowed deltas", k)
		}
		delete(env, k)
	}
	env["QUASAR_ENROLLMENT"] = "${QUASAR_ENROLLMENT:?}"
	want["environment"] = env

	diffMaps(t, agentService, want, got)

	// Every named volume the service mounts must be declared in the printed file.
	printed := printedCompose(t, "--print-compose")
	vols, _ := printed["volumes"].(map[string]any)
	for _, v := range []string{"quasar-agent-data", "quasar-updater-run"} {
		if _, ok := vols[v]; !ok {
			t.Errorf("--print-compose: named volume %s is mounted but not declared", v)
		}
	}

	// An enrolled host with no updater has no actor for a platform-release apply
	// and reports updater_present=false forever (#115), so the installer must
	// write the service, not only the agent's mount of its volume.
	svcs, _ := printed["services"].(map[string]any)
	up, ok := svcs["quasar-updater"].(map[string]any)
	if !ok {
		t.Fatal("--print-compose: no services.quasar-updater")
	}
	baseUp, _ := loadComposeFile(t, "docker-compose.yml")["services"].(map[string]any)
	diffMaps(t, "quasar-updater", baseUp["quasar-updater"].(map[string]any), up)
}

func TestEnrollHostNvidiaOverlayMatchesBase(t *testing.T) {
	baseDoc := loadComposeFile(t, "docker-compose.nvidia.yml")
	gotDoc := printedCompose(t, "--print-nvidia-overlay")
	diffMaps(t, agentService+" (nvidia overlay)", service(t, baseDoc, "docker-compose.nvidia.yml"), service(t, gotDoc, "--print-nvidia-overlay"))

	baseVols, _ := baseDoc["volumes"].(map[string]any)
	gotVols, _ := gotDoc["volumes"].(map[string]any)
	if !reflect.DeepEqual(sortedKeys(baseVols), sortedKeys(gotVols)) {
		t.Errorf("nvidia overlay volumes differ: base %v, installer %v", sortedKeys(baseVols), sortedKeys(gotVols))
	}
}
