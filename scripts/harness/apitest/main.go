// Command quasar-apitest drives a live Quasar control-plane through a realistic
// admin+user sequence and validates every /v1 response against protocol/openapi.yaml
// (real OpenAPI 3.1 conformance via libopenapi-validator), plus asserts status codes.
//
// It needs a running control-plane (QUASAR_URL) with a bootstrap admin. It mutates
// only throwaway data (a temp app, an invite, a temp registered user) and cleans up.
//
// Env:
//
//	QUASAR_URL       base origin, e.g. http://localhost:18099  (no /v1 suffix)
//	ADMIN_EMAIL      bootstrap admin email
//	ADMIN_PASSWORD   bootstrap admin password
//	SPEC             path to openapi.yaml (default ../../protocol/openapi.yaml)
//	RESULTS_JSON     optional path to write machine-readable results
//
// Exit code is non-zero if any step fails (bad status or schema-invalid response).
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/pb33f/libopenapi"
	validator "github.com/pb33f/libopenapi-validator"
)

type result struct {
	Name       string `json:"name"`
	Method     string `json:"method"`
	Path       string `json:"path"`
	WantStatus int    `json:"want_status"`
	GotStatus  int    `json:"got_status"`
	StatusOK   bool   `json:"status_ok"`
	SchemaOK   bool   `json:"schema_ok"`
	Notes      string `json:"notes"`
}

type harness struct {
	base    string
	client  *http.Client
	val     validator.Validator
	results []result
	failed  bool
}

func main() {
	base := strings.TrimRight(envOr("QUASAR_URL", "http://localhost:18099"), "/")
	adminEmail := envOr("ADMIN_EMAIL", "admin@quasar.local")
	adminPass := envOr("ADMIN_PASSWORD", "adminpassword123")
	specPath := envOr("SPEC", "../../protocol/openapi.yaml")

	specBytes, err := os.ReadFile(specPath)
	if err != nil {
		fatalf("read spec %s: %v", specPath, err)
	}
	doc, err := libopenapi.NewDocument(specBytes)
	if err != nil {
		fatalf("parse spec: %v", err)
	}
	val, valErrs := validator.NewValidator(doc)
	if len(valErrs) > 0 {
		for _, e := range valErrs {
			fmt.Printf("spec build error: %v\n", e)
		}
		fatalf("openapi.yaml did not build cleanly (%d errors)", len(valErrs))
	}
	fmt.Println("openapi.yaml parsed + model built OK")

	h := &harness{base: base, client: &http.Client{Timeout: 15 * time.Second}, val: val}

	// 1. login as bootstrap admin — the field is access_token, not token.
	loginBody := map[string]any{"email": adminEmail, "password": adminPass}
	adminTok := ""
	if body := h.step("admin login", "POST", "/v1/auth/login", "", loginBody, 200); body != nil {
		var lr struct {
			AccessToken string `json:"access_token"`
		}
		_ = json.Unmarshal(body, &lr)
		adminTok = lr.AccessToken
		if adminTok == "" {
			h.note("admin login", "FAIL: no access_token in login response")
		}
	}
	if adminTok == "" {
		h.finish("admin login failed — cannot continue authed steps")
	}

	// 2. identity + library reads
	h.step("get me", "GET", "/v1/me", adminTok, nil, 200)
	// UI-P1: GET /v1/apps requires auth. It used to be unauthenticated, which
	// handed the whole app catalogue to any caller — see the UI-P1 amendment in
	// control-api.md. Both arms are asserted so the gate cannot silently regress
	// back to a public catalogue.
	h.step("list apps (authed)", "GET", "/v1/apps", adminTok, nil, 200)
	h.step("list apps w/o token → 401", "GET", "/v1/apps", "", nil, 401)

	// 3. app CRUD — the runtime_spec pain point.
	appBody := map[string]any{
		"name":        "apitest-throwaway",
		"description": "created by quasar-apitest; safe to delete",
		"runtime_spec": map[string]any{
			"image": "ghcr.io/games-on-whales/xfce:edge",
			"args":  []string{},
			"env":   map[string]string{"FOO": "bar"},
			"gpu":   true,
		},
	}
	appID := ""
	if body := h.step("create app (admin)", "POST", "/v1/apps", adminTok, appBody, 201); body != nil {
		appID = extractID(body)
	}
	if appID != "" {
		h.step("get app (auth)", "GET", "/v1/apps/"+appID, adminTok, nil, 200)
	}
	h.step("list admin apps", "GET", "/v1/admin/apps", adminTok, nil, 200)

	// 4. hosts + admin settings
	h.step("list hosts (admin)", "GET", "/v1/hosts", adminTok, nil, 200)
	h.step("get admin settings", "GET", "/v1/admin/settings", adminTok, nil, 200)
	h.step("set registration_mode=invite_only", "PATCH", "/v1/admin/settings", adminTok, map[string]any{"registration_mode": "invite_only"}, 200)

	// 4b. encrypted secrets — reachability + the write-only property. A live
	// stack may or may not have QUASAR_SECRET_KEY set, so a PUT is deliberately
	// NOT asserted here (it is 409 without a master key, which is a documented
	// state, not a failure). The GET must always answer, and must never carry a
	// value: the shape has no field that could hold one.
	h.step("list admin secrets", "GET", "/v1/admin/secrets", adminTok, nil, 200)

	// 5. invite mint → list → register a throwaway user with it
	inviteCode := ""
	if body := h.step("mint invite (admin)", "POST", "/v1/admin/invites", adminTok, map[string]any{"role": "user", "max_uses": 1}, 201); body != nil {
		var ir struct {
			Invite struct {
				Code string `json:"code"`
			} `json:"invite"`
		}
		_ = json.Unmarshal(body, &ir)
		inviteCode = ir.Invite.Code
	}
	h.step("list invites (admin)", "GET", "/v1/admin/invites", adminTok, nil, 200)
	if inviteCode != "" {
		reg := map[string]any{"email": "apitest-user@quasar.local", "username": "apitestuser", "password": "userpassword123", "invite_code": inviteCode}
		// 201 fresh; 409 if a prior run on a persistent DB already registered this user.
		h.stepAny("register via invite", "POST", "/v1/auth/register", "", reg, []int{201, 409})
	} else {
		h.note("register via invite", "SKIP: no invite code captured")
	}

	// 6. users + sessions surface
	h.step("list users (admin)", "GET", "/v1/users", adminTok, nil, 200)
	h.step("list my sessions", "GET", "/v1/sessions", adminTok, nil, 200)
	// launch the still-enabled app with no node-agent online → expect 503 no_host_available
	// (a valid, documented error envelope). 409/404 also accepted defensively.
	if appID != "" {
		h.stepAny("launch (expect 503 no host)", "POST", "/v1/sessions", adminTok, map[string]any{"app_id": appID}, []int{503, 409, 404})
	}

	// 7. me sub-surface
	h.step("my devices", "GET", "/v1/me/devices", adminTok, nil, 200)
	h.step("my storage", "GET", "/v1/me/storage", adminTok, nil, 200)
	h.step("my profiles", "GET", "/v1/me/profiles", adminTok, nil, 200)

	// 7b. enriched admin read surfaces (validate the transcribed shapes against real bodies)
	h.step("config catalog", "GET", "/v1/admin/config/catalog", adminTok, nil, 200)
	h.step("stream profiles", "GET", "/v1/admin/stream-profiles", adminTok, nil, 200)
	h.step("profile policy", "GET", "/v1/admin/profile-policy", adminTok, nil, 200)
	h.step("storage homes", "GET", "/v1/admin/storage/homes", adminTok, nil, 200)

	// 7c. console-config (CM-01) — no host exists in the ephemeral stack (no agent), so
	// validate routing + admin-gating + the 404 path + error-envelope schema. The 200
	// resolved-config shape is covered by the drift test + gets live validation on Tower.
	randHost := "00000000-0000-0000-0000-0000000000aa"
	h.step("console-config unknown host → 404", "GET", "/v1/admin/hosts/"+randHost+"/console-config", adminTok, nil, 404)
	h.step("console-config PATCH unknown host → 404", "PATCH", "/v1/admin/hosts/"+randHost+"/console-config", adminTok, map[string]any{"enabled": true}, 404)
	h.step("console-config w/o token → 401", "GET", "/v1/admin/hosts/"+randHost+"/console-config", "", nil, 401)

	// 8. auth gating — the server-enforced admin gate + missing-token
	h.step("admin route w/o token → 401", "GET", "/v1/hosts", "", nil, 401)

	// 9. cleanup: restore registration mode + delete throwaway app
	h.step("restore registration_mode=open", "PATCH", "/v1/admin/settings", adminTok, map[string]any{"registration_mode": "open"}, 200)
	if appID != "" {
		// app is disabled, no live sessions → delete should succeed
		h.stepAny("delete throwaway app", "DELETE", "/v1/apps/"+appID, adminTok, nil, []int{204, 200})
	}

	h.finish("")
}

// step runs one request, expects a single status, validates the response schema.
func (h *harness) step(name, method, path, token string, body any, wantStatus int) []byte {
	return h.stepAny(name, method, path, token, body, []int{wantStatus})
}

func (h *harness) stepAny(name, method, path, token string, body any, wantStatuses []int) []byte {
	req, resp, respBody, err := h.do(method, path, token, body)
	r := result{Name: name, Method: method, Path: path, WantStatus: wantStatuses[0]}
	if err != nil {
		r.Notes = "transport error: " + err.Error()
		h.results = append(h.results, r)
		h.failed = true
		fmt.Printf("  ✗ %-34s %s %s — %s\n", name, method, path, r.Notes)
		return nil
	}
	r.GotStatus = resp.StatusCode
	r.StatusOK = containsInt(wantStatuses, resp.StatusCode)

	// schema validation against openapi.yaml
	resp.Body = io.NopCloser(bytes.NewReader(respBody))
	ok, valErrs := h.val.ValidateHttpResponse(req, resp)
	r.SchemaOK = ok
	if !ok {
		msgs := make([]string, 0, len(valErrs))
		for _, e := range valErrs {
			msgs = append(msgs, e.Message)
		}
		r.Notes = "schema: " + strings.Join(dedupe(msgs), "; ")
	}
	if !r.StatusOK {
		r.Notes = fmt.Sprintf("status want %v got %d; %s", wantStatuses, resp.StatusCode, r.Notes)
	}
	h.results = append(h.results, r)
	mark := "✓"
	if !r.StatusOK || !r.SchemaOK {
		mark = "✗"
		h.failed = true
	}
	fmt.Printf("  %s %-34s %s %-46s [%d] %s\n", mark, name, method, path, resp.StatusCode, r.Notes)
	return respBody
}

func (h *harness) do(method, path, token string, body any) (*http.Request, *http.Response, []byte, error) {
	var rdr io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, h.base+path, rdr)
	if err != nil {
		return nil, nil, nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	// keep a pristine copy of the request for the validator (its body may be needed)
	valReq := req.Clone(req.Context())
	if body != nil {
		b, _ := json.Marshal(body)
		valReq.Body = io.NopCloser(bytes.NewReader(b))
	}
	resp, err := h.client.Do(req)
	if err != nil {
		return nil, nil, nil, err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	return valReq, resp, respBody, nil
}

func (h *harness) note(name, msg string) {
	h.results = append(h.results, result{Name: name, Notes: msg})
	if strings.HasPrefix(msg, "FAIL") {
		h.failed = true
	}
	fmt.Printf("  · %-34s %s\n", name, msg)
}

func (h *harness) finish(fatal string) {
	pass, fail := 0, 0
	for _, r := range h.results {
		if r.Method == "" {
			continue
		}
		if r.StatusOK && r.SchemaOK {
			pass++
		} else {
			fail++
		}
	}
	fmt.Printf("\n=== apitest: %d passed, %d failed ===\n", pass, fail)
	if out := os.Getenv("RESULTS_JSON"); out != "" {
		b, _ := json.MarshalIndent(map[string]any{"passed": pass, "failed": fail, "results": h.results}, "", "  ")
		_ = os.WriteFile(out, b, 0o644)
	}
	if fatal != "" {
		fmt.Println("FATAL:", fatal)
	}
	if h.failed || fatal != "" {
		os.Exit(1)
	}
}

func extractID(body []byte) string {
	// accept {id:...} or {app:{id:...}} or {session:{id:...}}
	var m map[string]json.RawMessage
	if json.Unmarshal(body, &m) != nil {
		return ""
	}
	if raw, ok := m["id"]; ok {
		var s string
		if json.Unmarshal(raw, &s) == nil {
			return s
		}
	}
	for _, k := range []string{"app", "session"} {
		if raw, ok := m[k]; ok {
			if id := extractID(raw); id != "" {
				return id
			}
		}
	}
	return ""
}

func containsInt(xs []int, v int) bool {
	for _, x := range xs {
		if x == v {
			return true
		}
	}
	return false
}

func dedupe(xs []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, x := range xs {
		if !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	return out
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func fatalf(f string, a ...any) {
	fmt.Printf(f+"\n", a...)
	os.Exit(1)
}
