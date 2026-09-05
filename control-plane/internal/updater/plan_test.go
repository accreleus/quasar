package updater

import (
	"reflect"
	"strings"
	"testing"
)

const (
	goodDigest = "sha256:9f2c000000000000000000000000000000000000000000000000000000000abc"
	prevDigest = "sha256:1b7e000000000000000000000000000000000000000000000000000000000def"
	reqID      = "7a1f6f1e-2c33-4a58-9a5e-0b6b0f7a1c22"
	otherID    = "11111111-2222-3333-4444-555555555555"
)

func testCfg() Config {
	return Config{
		Project:     "deploy",
		WorkingDir:  "/opt/quasar/deploy",
		ConfigFiles: []string{"/opt/quasar/deploy/docker-compose.yml", "/opt/quasar/deploy/docker-compose.nvidia.yml"},
		// Non-default on purpose: the default must not be what makes a test pass.
		AllowedNamespaces: []string{"ghcr.io/accreleus/quasar"},
		WaitTimeoutS:      120,
	}
}

func agentReq() ApplyRequest {
	return ApplyRequest{
		RequestID: reqID,
		Components: []Component{
			{Name: "node-agent", Image: "ghcr.io/accreleus/quasar/quasar-node-agent", Digest: goodDigest},
		},
		Release: Release{ID: "r1", SourceCommit: strings.Repeat("a", 40)},
	}
}

func TestPlanAcceptsAnAgentApply(t *testing.T) {
	env := "POSTGRES_PASSWORD=hunter2\nQUASAR_AGENT_IMAGE=ghcr.io/accreleus/quasar/quasar-node-agent@" + prevDigest + "\n"
	p, rej := Plan(agentReq(), env, testCfg())
	if rej != nil {
		t.Fatalf("unexpected rejection: %v", rej)
	}
	if got := []string{"quasar-node-agent"}; !reflect.DeepEqual(p.Services, got) {
		t.Fatalf("services = %v, want %v", p.Services, got)
	}
	if p.Previous[0].Digest == nil || *p.Previous[0].Digest != prevDigest {
		t.Fatalf("previous = %+v, want %s", p.Previous[0], prevDigest)
	}
	wantEnv := "POSTGRES_PASSWORD=hunter2\nQUASAR_AGENT_IMAGE=ghcr.io/accreleus/quasar/quasar-node-agent@" + goodDigest + "\n"
	if p.EnvRewrite != wantEnv {
		t.Fatalf("env rewrite:\n got %q\nwant %q", p.EnvRewrite, wantEnv)
	}
}

// The command list is asserted verbatim: the flags are the contract with
// compose (--no-deps is what keeps postgres and the other component out of the
// recreate), and a silent drop would only show up on a live host.
func TestPlanProducesTheExactCommands(t *testing.T) {
	p, rej := Plan(agentReq(), "", testCfg())
	if rej != nil {
		t.Fatalf("unexpected rejection: %v", rej)
	}
	base := []string{
		"compose", "-p", "deploy",
		"--project-directory", "/opt/quasar/deploy",
		"-f", "/opt/quasar/deploy/docker-compose.yml",
		"-f", "/opt/quasar/deploy/docker-compose.nvidia.yml",
	}
	wantPull := append(append([]string{}, base...), "pull", "quasar-node-agent")
	wantUp := append(append([]string{}, base...),
		"up", "-d", "--force-recreate", "--no-deps", "--wait", "--wait-timeout", "120", "quasar-node-agent")
	if !reflect.DeepEqual(p.Commands, [][]string{wantPull, wantUp}) {
		t.Fatalf("commands =\n%v\nwant\n%v", p.Commands, [][]string{wantPull, wantUp})
	}
}

func TestPlanRejections(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*ApplyRequest)
		cfg    func(*Config)
		reason string
	}{
		{"foreign namespace", func(r *ApplyRequest) {
			r.Components[0].Image = "ghcr.io/someone-else/quasar/quasar-node-agent"
		}, nil, ReasonNamespaceRejected},
		{"namespace prefix is not a segment boundary", func(r *ApplyRequest) {
			r.Components[0].Image = "ghcr.io/accreleus/quasar-evil/quasar-node-agent"
		}, nil, ReasonNamespaceRejected},
		{"malformed digest", func(r *ApplyRequest) {
			r.Components[0].Digest = "sha256:notahexdigest"
		}, nil, ReasonDigestMalformed},
		{"uppercase digest", func(r *ApplyRequest) {
			r.Components[0].Digest = strings.ToUpper(goodDigest[7:])
		}, nil, ReasonDigestMalformed},
		{"image carries a tag", func(r *ApplyRequest) {
			r.Components[0].Image = "ghcr.io/accreleus/quasar/quasar-node-agent:latest"
		}, nil, ReasonInvalid},
		{"image carries a digest", func(r *ApplyRequest) {
			r.Components[0].Image = "ghcr.io/accreleus/quasar/quasar-node-agent@" + goodDigest
		}, nil, ReasonInvalid},
		{"unknown component", func(r *ApplyRequest) {
			r.Components[0].Name = "postgres"
		}, nil, ReasonInvalid},
		{"the updater naming itself", func(r *ApplyRequest) {
			r.Components[0].Name = "updater"
		}, nil, ReasonInvalid},
		{"the updater naming its service", func(r *ApplyRequest) {
			r.Components[0].Name = "quasar-updater"
		}, nil, ReasonInvalid},
		{"empty components", func(r *ApplyRequest) { r.Components = nil }, nil, ReasonInvalid},
		{"duplicate component", func(r *ApplyRequest) {
			r.Components = append(r.Components, r.Components[0])
		}, nil, ReasonInvalid},
		{"request id is not a uuid", func(r *ApplyRequest) { r.RequestID = "nope" }, nil, ReasonInvalid},
		{"busy", nil, func(c *Config) { c.InFlightRequestID = otherID }, ReasonBusy},
		{"undiscovered project", nil, func(c *Config) { c.Project = "" }, ReasonInvalid},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req, cfg := agentReq(), testCfg()
			if tc.mutate != nil {
				tc.mutate(&req)
			}
			if tc.cfg != nil {
				tc.cfg(&cfg)
			}
			p, rej := Plan(req, "", cfg)
			if rej == nil {
				t.Fatalf("expected %s, got a plan: %+v", tc.reason, p)
			}
			if rej.Reason != tc.reason {
				t.Fatalf("reason = %s, want %s (%s)", rej.Reason, tc.reason, rej.Message)
			}
		})
	}
}

// Re-posting the id that is already in flight is idempotent, not busy.
func TestPlanAcceptsTheInFlightIDAgain(t *testing.T) {
	cfg := testCfg()
	cfg.InFlightRequestID = reqID
	if _, rej := Plan(agentReq(), "", cfg); rej != nil {
		t.Fatalf("unexpected rejection: %v", rej)
	}
}

func TestPlanControlPlaneMapsToItsOwnVariable(t *testing.T) {
	req := agentReq()
	req.Components = []Component{
		{Name: "control-plane", Image: "ghcr.io/accreleus/quasar/quasar-control-plane", Digest: goodDigest},
	}
	p, rej := Plan(req, "", testCfg())
	if rej != nil {
		t.Fatalf("unexpected rejection: %v", rej)
	}
	if !strings.Contains(p.EnvRewrite, "QUASAR_CONTROL_IMAGE=ghcr.io/accreleus/quasar/quasar-control-plane@"+goodDigest) {
		t.Fatalf("env rewrite did not set QUASAR_CONTROL_IMAGE: %q", p.EnvRewrite)
	}
	if p.Services[0] != "quasar-control-plane" {
		t.Fatalf("service = %s", p.Services[0])
	}
}

func TestPlanPreviousDigestIsNullForALocalTag(t *testing.T) {
	p, rej := Plan(agentReq(), "QUASAR_AGENT_IMAGE=quasar-node-agent:latest\n", testCfg())
	if rej != nil {
		t.Fatalf("unexpected rejection: %v", rej)
	}
	if p.Previous[0].Digest != nil {
		t.Fatalf("previous digest = %v, want nil for a local tag", *p.Previous[0].Digest)
	}
}

// QUASAR_AGENT_IMAGE wins over the QUASAR_NODE_IMAGE alias by compose
// precedence, so only the former is written — but the alias is the honest
// previous value when the former is absent.
func TestPlanReadsThePreviousDigestFromTheAlias(t *testing.T) {
	env := "QUASAR_NODE_IMAGE=ghcr.io/accreleus/quasar/quasar-node-agent@" + prevDigest + "\n"
	p, rej := Plan(agentReq(), env, testCfg())
	if rej != nil {
		t.Fatalf("unexpected rejection: %v", rej)
	}
	if p.Previous[0].Digest == nil || *p.Previous[0].Digest != prevDigest {
		t.Fatalf("previous = %+v", p.Previous[0])
	}
	if !strings.Contains(p.EnvRewrite, "QUASAR_NODE_IMAGE=ghcr.io/accreleus/quasar/quasar-node-agent@"+prevDigest) {
		t.Fatalf("the alias line must be left untouched: %q", p.EnvRewrite)
	}
}

func TestPlanAppendsTheVariableWhenAbsent(t *testing.T) {
	env := "# a comment\nPOSTGRES_PASSWORD=hunter2\n"
	p, rej := Plan(agentReq(), env, testCfg())
	if rej != nil {
		t.Fatalf("unexpected rejection: %v", rej)
	}
	want := env + "QUASAR_AGENT_IMAGE=ghcr.io/accreleus/quasar/quasar-node-agent@" + goodDigest + "\n"
	if p.EnvRewrite != want {
		t.Fatalf("env rewrite:\n got %q\nwant %q", p.EnvRewrite, want)
	}
	if p.Previous[0].Digest != nil {
		t.Fatalf("previous digest = %v, want nil when the variable was absent", *p.Previous[0].Digest)
	}
}

// The .env is operator-owned: it carries the DB password, the enrollment token
// and the operator's comments. Everything but the one line must survive
// byte-for-byte.
func TestEnvRewritePreservesEveryOtherByte(t *testing.T) {
	env := "# header\r\n\r\nPOSTGRES_PASSWORD=p@ss=word#notacomment\n" +
		"QUASAR_AGENT_IMAGE=old:tag\n" +
		"  QUASAR_TLS=auto\n" +
		"TRAILING_NO_NEWLINE=1"
	p, rej := Plan(agentReq(), env, testCfg())
	if rej != nil {
		t.Fatalf("unexpected rejection: %v", rej)
	}
	want := "# header\r\n\r\nPOSTGRES_PASSWORD=p@ss=word#notacomment\n" +
		"QUASAR_AGENT_IMAGE=ghcr.io/accreleus/quasar/quasar-node-agent@" + goodDigest + "\n" +
		"  QUASAR_TLS=auto\n" +
		"TRAILING_NO_NEWLINE=1"
	if p.EnvRewrite != want {
		t.Fatalf("env rewrite:\n got %q\nwant %q", p.EnvRewrite, want)
	}
}

func TestEnvSetAppendsANewlineFirstWhenTheFileHasNone(t *testing.T) {
	got := envSet("A=1", "B", "2")
	if got != "A=1\nB=2\n" {
		t.Fatalf("got %q", got)
	}
}

// A duplicate definition is already pathological; leaving a stale one behind
// would make the file disagree with itself about which digest is installed.
func TestEnvSetReplacesEveryDefinition(t *testing.T) {
	got := envSet("X=1\nY=2\nX=3\n", "X", "9")
	if got != "X=9\nY=2\nX=9\n" {
		t.Fatalf("got %q", got)
	}
	if v, ok := envLookup("X=1\nX=3\n", "X"); !ok || v != "3" {
		t.Fatalf("envLookup should return the last definition, got %q %v", v, ok)
	}
}

func TestParseNamespaces(t *testing.T) {
	if got := ParseNamespaces(" ghcr.io/a/b/ , ghcr.io/c/d "); !reflect.DeepEqual(got, []string{"ghcr.io/a/b", "ghcr.io/c/d"}) {
		t.Fatalf("got %v", got)
	}
	if got := ParseNamespaces("  "); !reflect.DeepEqual(got, DefaultAllowedNamespaces) {
		t.Fatalf("blank must fall back to the default, got %v", got)
	}
}
