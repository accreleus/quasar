package updater

import (
	"bytes"
	"context"
	"encoding/json"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The handlers are exercised over a REAL unix socket, because the socket is the
// interface: its existence and its 0666 mode are what the agent and the control
// plane depend on, and neither is visible through an httptest server.
func serveOnSocket(t *testing.T, f *fakeEnv) *http.Client {
	t.Helper()
	// A socket path is capped at ~104 bytes on some platforms; t.TempDir() under
	// a long test name can exceed it, so the socket lives in its own short dir.
	dir, err := os.MkdirTemp("", "qu")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "u.sock")

	srv := &Server{Store: f.store, Docker: f.docker, Cfg: f.cfg(), EnvPath: f.envPath, Version: "test"}
	ln, err := Listen(sock)
	if err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(sock)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != fs.FileMode(0o666) {
		t.Fatalf("socket mode = %v, want 0666 (the control plane runs as uid 1000 and must be able to connect)", fi.Mode().Perm())
	}
	hs := &http.Server{Handler: srv.Handler()}
	go hs.Serve(ln)
	t.Cleanup(func() { hs.Close() })

	return &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", sock)
		},
	}}
}

func post(t *testing.T, c *http.Client, path string, body any) (int, map[string]any) {
	t.Helper()
	b, _ := json.Marshal(body)
	resp, err := c.Post("http://updater"+path, "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func get(t *testing.T, c *http.Client, path string) (int, map[string]any) {
	t.Helper()
	resp, err := c.Get("http://updater" + path)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func TestServerAcceptsAndReportsAnApply(t *testing.T) {
	f := newFakeEnv(t, "QUASAR_AGENT_IMAGE=ghcr.io/accreleus/quasar/quasar-node-agent@"+prevDigest+"\n")
	f.canned("ps.out", psJSON("quasar-node-agent", "na1", "running", ""))
	c := serveOnSocket(t, f)

	code, body := post(t, c, "/v1/apply", agentReq())
	if code != http.StatusAccepted {
		t.Fatalf("status = %d, body = %v", code, body)
	}
	prev := body["previous"].([]any)[0].(map[string]any)
	if prev["digest"] != prevDigest {
		t.Fatalf("the 202 must carry the previous digests: %v", body)
	}
	if len(body["commands"].([]any)) != 2 {
		t.Fatalf("the 202 must carry the exact commands: %v", body)
	}

	res := waitTerminal(t, c, reqID)
	if res["state"] != StateSucceeded {
		t.Fatalf("result = %v", res)
	}
}

func waitTerminal(t *testing.T, c *http.Client, id string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		code, body := get(t, c, "/v1/results/"+id)
		if code == http.StatusOK {
			if s, _ := body["state"].(string); Terminal(s) {
				return body
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("result for %s never reached a terminal state", id)
	return nil
}

func TestServerRejectionStatuses(t *testing.T) {
	f := newFakeEnv(t, "")
	c := serveOnSocket(t, f)

	foreign := agentReq()
	foreign.Components[0].Image = "ghcr.io/someone-else/quasar/quasar-node-agent"
	if code, body := post(t, c, "/v1/apply", foreign); code != http.StatusUnprocessableEntity || body["reason"] != ReasonNamespaceRejected {
		t.Fatalf("status=%d body=%v", code, body)
	}

	bad := agentReq()
	bad.RequestID = otherID
	bad.Components[0].Digest = "sha256:zz"
	if code, body := post(t, c, "/v1/apply", bad); code != http.StatusUnprocessableEntity || body["reason"] != ReasonDigestMalformed {
		t.Fatalf("status=%d body=%v", code, body)
	}

	self := agentReq()
	self.RequestID = otherID
	self.Components[0].Name = "quasar-updater"
	if code, body := post(t, c, "/v1/apply", self); code != http.StatusBadRequest || body["reason"] != ReasonInvalid {
		t.Fatalf("the updater must never accept a request naming itself: status=%d body=%v", code, body)
	}

	if code, body := get(t, c, "/v1/results/"+otherID); code != http.StatusNotFound {
		t.Fatalf("status=%d body=%v", code, body)
	}
	// A rejected apply leaves nothing behind: no result file, no latch.
	if f.store.InFlight() != "" {
		t.Fatalf("in flight = %q after rejections", f.store.InFlight())
	}
}

func TestServerBusyRefusesNeverQueues(t *testing.T) {
	f := newFakeEnv(t, "")
	c := serveOnSocket(t, f)
	if err := f.store.Claim(otherID, &Accepted{RequestID: otherID}); err != nil {
		t.Fatal(err)
	}
	code, body := post(t, c, "/v1/apply", agentReq())
	if code != http.StatusConflict || body["reason"] != ReasonBusy {
		t.Fatalf("status=%d body=%v", code, body)
	}
}

func TestServerSelfAndHealthz(t *testing.T) {
	f := newFakeEnv(t, "")
	f.canned("config.out", `{"services":{"quasar-control-plane":{"image":"quasar-control-plane:latest"}}}`)
	c := serveOnSocket(t, f)
	if code, _ := get(t, c, "/v1/healthz"); code != http.StatusOK {
		t.Fatalf("healthz status = %d", code)
	}
	code, body := get(t, c, "/v1/self")
	if code != http.StatusOK {
		t.Fatalf("self status = %d", code)
	}
	if body["project"] != "deploy" || body["env_path"] != f.envPath {
		t.Fatalf("self = %v", body)
	}
	if len(body["allowed_namespaces"].([]any)) != 1 {
		t.Fatalf("self must report the allowlist: %v", body)
	}
	// Exact key names: the control plane reads them to classify its own install
	// mode (#117).
	images, ok := body["images"].(map[string]any)
	if !ok {
		t.Fatalf("self must report images: %v", body)
	}
	for _, k := range []string{"control-plane", "node-agent"} {
		if _, present := images[k]; !present {
			t.Fatalf("images is missing the key %q: %v", k, images)
		}
	}
	if images["control-plane"] != "quasar-control-plane:latest" {
		t.Fatalf("images = %v", images)
	}
}

func TestServerApplyIsIdempotentForTheSameRequestID(t *testing.T) {
	f := newFakeEnv(t, "")
	f.canned("ps.out", psJSON("quasar-node-agent", "na1", "running", ""))
	c := serveOnSocket(t, f)

	if code, _ := post(t, c, "/v1/apply", agentReq()); code != http.StatusAccepted {
		t.Fatal("first apply not accepted")
	}
	waitTerminal(t, c, reqID)
	before := f.argv()
	if code, _ := post(t, c, "/v1/apply", agentReq()); code != http.StatusAccepted {
		t.Fatal("re-post of the same id must be accepted, not rejected")
	}
	time.Sleep(100 * time.Millisecond)
	if f.argv() != before {
		t.Fatalf("a re-post ran a second apply:\nbefore:\n%s\nafter:\n%s", before, f.argv())
	}
}
