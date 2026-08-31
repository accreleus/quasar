package setup

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/accreleus/quasar/control-plane/internal/auth"
)

// fakeClaimer records whether ClaimSetup was reached — the assertion for the
// wrong/missing-token tests is precisely that it was NOT (the token check must
// short-circuit before any minting work).
type fakeClaimer struct {
	adminExists    bool
	claimCalled    bool
	deviceKey      string
	tok            auth.Token
	claimErr       error
	adminExistsErr error
}

func (f *fakeClaimer) AdminExists(ctx context.Context) (bool, error) {
	return f.adminExists, f.adminExistsErr
}

func (f *fakeClaimer) ClaimSetup(ctx context.Context, email, username, password, userAgent, deviceKey string) (auth.Token, error) {
	f.claimCalled = true
	f.deviceKey = deviceKey
	if f.claimErr != nil {
		return auth.Token{}, f.claimErr
	}
	return f.tok, nil
}

type fakeState struct {
	completed    bool
	completedErr error
	markCalls    int
	markErr      error
}

func (f *fakeState) SetupCompleted(ctx context.Context) (bool, error) {
	return f.completed, f.completedErr
}

func (f *fakeState) MarkSetupComplete(ctx context.Context) (bool, error) {
	f.markCalls++
	if f.markErr != nil {
		return false, f.markErr
	}
	f.completed = true
	return true, nil
}

func quietLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func doClaim(t *testing.T, svc *Service, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/setup/claim", strings.NewReader(body))
	if token != "" {
		req.Header.Set(TokenHeader, token)
	}
	rr := httptest.NewRecorder()
	svc.handleClaim(rr, req)
	return rr
}

const goodBody = `{"email":"admin@example.com","username":"admin","password":"correct-horse"}`

// TestClaimWrongTokenIs401AndNeverReachesClaimer pins gate 2: a wrong token is
// rejected BEFORE the body is examined and BEFORE any minting work.
func TestClaimWrongTokenIs401AndNeverReachesClaimer(t *testing.T) {
	claimer := &fakeClaimer{}
	svc := NewService(claimer, &fakeState{}, "the-real-token", quietLog())

	rr := doClaim(t, svc, "the-wrong-token", goodBody)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rr.Code)
	}
	if claimer.claimCalled {
		t.Fatal("ClaimSetup was reached on a wrong token — the gate must short-circuit before minting")
	}
}

// TestClaimMissingTokenIs401ByteIdenticalToWrong pins gate 2's headline
// property: missing and wrong are indistinguishable — same status, same body.
func TestClaimMissingTokenIs401ByteIdenticalToWrong(t *testing.T) {
	svc := NewService(&fakeClaimer{}, &fakeState{}, "the-real-token", quietLog())

	wrong := doClaim(t, svc, "the-wrong-token", goodBody)
	missing := doClaim(t, svc, "", goodBody)

	if wrong.Code != http.StatusUnauthorized || missing.Code != http.StatusUnauthorized {
		t.Fatalf("wrong=%d missing=%d, want both 401", wrong.Code, missing.Code)
	}
	if wrong.Body.String() != missing.Body.String() {
		t.Fatalf("wrong-token body %q != missing-token body %q — the two must be indistinguishable",
			wrong.Body.String(), missing.Body.String())
	}
}

// TestClaimEmptySecretFailsClosed: when no token was minted this boot (an admin
// already existed), the correct-looking empty header must NOT authenticate.
func TestClaimEmptySecretFailsClosed(t *testing.T) {
	claimer := &fakeClaimer{}
	svc := NewService(claimer, &fakeState{}, "", quietLog())

	// Present an empty token against an empty secret — must not match.
	rr := doClaim(t, svc, "", goodBody)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (empty secret must fail closed)", rr.Code)
	}
	if claimer.claimCalled {
		t.Fatal("ClaimSetup reached with no token minted this boot")
	}
}

// TestClaimWeakPasswordIs400 pins gate 3: a valid token still gets a 400 when
// the password fails the shared strength rule.
func TestClaimWeakPasswordIs400(t *testing.T) {
	claimer := &fakeClaimer{claimErr: auth.ErrValidation{Msg: "password must be at least 12 characters"}}
	svc := NewService(claimer, &fakeState{}, "tok", quietLog())

	rr := doClaim(t, svc, "tok", `{"email":"a@b.com","username":"a","password":"short"}`)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "validation_failed") {
		t.Fatalf("body = %q, want validation_failed", rr.Body.String())
	}
}

// TestClaimAlreadyCompleteIs409 pins gate 1's client-facing mapping.
func TestClaimAlreadyCompleteIs409(t *testing.T) {
	claimer := &fakeClaimer{claimErr: auth.ErrSetupAlreadyComplete}
	svc := NewService(claimer, &fakeState{}, "tok", quietLog())

	rr := doClaim(t, svc, "tok", goodBody)
	if rr.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "setup_already_complete") {
		t.Fatalf("body = %q, want setup_already_complete", rr.Body.String())
	}
}

// TestClaimMalformedBodyIs400 pins gate: a valid token with a junk body is a 400.
func TestClaimMalformedBodyIs400(t *testing.T) {
	claimer := &fakeClaimer{}
	svc := NewService(claimer, &fakeState{}, "tok", quietLog())

	rr := doClaim(t, svc, "tok", `{not json`)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
	if claimer.claimCalled {
		t.Fatal("ClaimSetup reached on a malformed body")
	}
}

// TestClaimTrailingJSONIs400: a valid object followed by a second top-level
// JSON value is malformed, not silently accepted with the trailer ignored.
func TestClaimTrailingJSONIs400(t *testing.T) {
	claimer := &fakeClaimer{}
	svc := NewService(claimer, &fakeState{}, "tok", quietLog())

	rr := doClaim(t, svc, "tok", goodBody+`{"sneaky":true}`)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for trailing JSON", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "validation_failed") {
		t.Fatalf("body = %q, want validation_failed", rr.Body.String())
	}
	if claimer.claimCalled {
		t.Fatal("ClaimSetup reached on a body with trailing JSON")
	}
}

// TestClaimForwardsDeviceKey: an optional device_key in the claim body reaches
// ClaimSetup so the minted token can be device-bound (and absent stays "").
func TestClaimForwardsDeviceKey(t *testing.T) {
	claimer := &fakeClaimer{}
	svc := NewService(claimer, &fakeState{}, "tok", quietLog())

	body := `{"email":"a@b.com","username":"a","password":"correct-horse","device_key":"dk-1"}`
	if rr := doClaim(t, svc, "tok", body); rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201", rr.Code)
	}
	if claimer.deviceKey != "dk-1" {
		t.Fatalf("deviceKey forwarded = %q, want dk-1", claimer.deviceKey)
	}

	claimer2 := &fakeClaimer{}
	svc2 := NewService(claimer2, &fakeState{}, "tok", quietLog())
	if rr := doClaim(t, svc2, "tok", goodBody); rr.Code != http.StatusCreated {
		t.Fatalf("status without device_key = %d, want 201", rr.Code)
	}
	if claimer2.deviceKey != "" {
		t.Fatalf("deviceKey without field = %q, want empty", claimer2.deviceKey)
	}
}

// TestStatusReportsBooleans checks the status handler surfaces exactly the two
// booleans it is given, and nothing else.
func TestStatusReportsBooleans(t *testing.T) {
	cases := []struct {
		admin, completed bool
	}{
		{false, false},
		{true, false},
		{true, true},
	}
	for _, c := range cases {
		svc := NewService(&fakeClaimer{adminExists: c.admin}, &fakeState{completed: c.completed}, "tok", quietLog())
		req := httptest.NewRequest(http.MethodGet, "/v1/setup/status", nil)
		rr := httptest.NewRecorder()
		svc.handleStatus(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rr.Code)
		}
		body := rr.Body.String()
		wantAdmin := `"admin_exists":` + boolStr(c.admin)
		wantComplete := `"setup_completed":` + boolStr(c.completed)
		if !strings.Contains(body, wantAdmin) || !strings.Contains(body, wantComplete) {
			t.Fatalf("body = %q, want %s and %s", body, wantAdmin, wantComplete)
		}
		// It must expose NOTHING else — no token, no email, no extra keys.
		for _, forbidden := range []string{"token", "email", "password", "user"} {
			if strings.Contains(body, forbidden) {
				t.Fatalf("status body leaked %q: %s", forbidden, body)
			}
		}
	}
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// --- rate limiting (finding 6) ------------------------------------------------

// TestClaimFailuresAreRateLimited proves the log-flood bound exists: after
// claimFailureLimit rejected attempts from one source, further attempts are
// refused with 429 and never reach the token compare or the WARN log.
func TestClaimFailuresAreRateLimited(t *testing.T) {
	claimer := &fakeClaimer{}
	svc := NewService(claimer, &fakeState{}, "the-real-token", quietLog())

	for i := 0; i < claimFailureLimit; i++ {
		rr := doClaim(t, svc, "wrong", goodBody)
		if rr.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d: status = %d, want 401", i, rr.Code)
		}
	}
	rr := doClaim(t, svc, "wrong", goodBody)
	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("attempt %d: status = %d, want 429 after %d failures",
			claimFailureLimit, rr.Code, claimFailureLimit)
	}
	if !strings.Contains(rr.Body.String(), "rate_limited") {
		t.Fatalf("body = %q, want rate_limited", rr.Body.String())
	}
}

// TestRateLimitIsNotAWrongVsMissingOracle is the security property the limiter
// must not break: for a caller in the SAME failure state, a missing token and a
// wrong token must still be byte-identical. The limiter keys on source IP only,
// never on the header, so both must move in lockstep — including once limited.
func TestRateLimitIsNotAWrongVsMissingOracle(t *testing.T) {
	// Two independent services so each sees an identical, isolated failure
	// history; within each, drive it to the limit and compare at every step.
	wrongSvc := NewService(&fakeClaimer{}, &fakeState{}, "the-real-token", quietLog())
	missingSvc := NewService(&fakeClaimer{}, &fakeState{}, "the-real-token", quietLog())

	for i := 0; i <= claimFailureLimit; i++ {
		wrong := doClaim(t, wrongSvc, "the-wrong-token", goodBody)
		missing := doClaim(t, missingSvc, "", goodBody)
		if wrong.Code != missing.Code {
			t.Fatalf("attempt %d: wrong=%d missing=%d — the limiter created a wrong-vs-missing distinction",
				i, wrong.Code, missing.Code)
		}
		if wrong.Body.String() != missing.Body.String() {
			t.Fatalf("attempt %d: wrong body %q != missing body %q", i, wrong.Body.String(), missing.Body.String())
		}
	}
}

// TestSuccessfulClaimClearsRateLimitPenalty: an operator who fumbles the paste a
// few times and then gets it right must not be left rate-limited.
func TestSuccessfulClaimClearsRateLimitPenalty(t *testing.T) {
	claimer := &fakeClaimer{}
	svc := NewService(claimer, &fakeState{}, "the-real-token", quietLog())

	for i := 0; i < claimFailureLimit-1; i++ {
		if rr := doClaim(t, svc, "wrong", goodBody); rr.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d: status = %d, want 401", i, rr.Code)
		}
	}
	// Correct token succeeds and forgets the penalty.
	if rr := doClaim(t, svc, "the-real-token", goodBody); rr.Code != http.StatusCreated {
		t.Fatalf("correct-token claim = %d, want 201", rr.Code)
	}
	// The allowance is restored: a subsequent failure is a plain 401, not a 429.
	if rr := doClaim(t, svc, "wrong", goodBody); rr.Code != http.StatusUnauthorized {
		t.Fatalf("post-success failure = %d, want 401 (penalty should have been forgotten)", rr.Code)
	}
}

// --- POST /v1/setup/complete --------------------------------------------------

// TestCompleteMarksAndReturnsStatus checks the handler returns the SetupStatus
// shape after marking.
func TestCompleteMarksAndReturnsStatus(t *testing.T) {
	state := &fakeState{}
	svc := NewService(&fakeClaimer{adminExists: true}, state, "tok", quietLog())

	rr := httptest.NewRecorder()
	svc.handleComplete(rr, httptest.NewRequest(http.MethodPost, "/v1/setup/complete", nil))

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if state.markCalls != 1 {
		t.Fatalf("MarkSetupComplete called %d times, want 1", state.markCalls)
	}
	body := rr.Body.String()
	if !strings.Contains(body, `"setup_completed":true`) || !strings.Contains(body, `"admin_exists":true`) {
		t.Fatalf("body = %q, want both booleans true", body)
	}
}

// TestCompleteIsIdempotentInHandler: calling twice returns the same body.
func TestCompleteIsIdempotentInHandler(t *testing.T) {
	state := &fakeState{}
	svc := NewService(&fakeClaimer{adminExists: true}, state, "tok", quietLog())

	first := httptest.NewRecorder()
	svc.handleComplete(first, httptest.NewRequest(http.MethodPost, "/v1/setup/complete", nil))
	second := httptest.NewRecorder()
	svc.handleComplete(second, httptest.NewRequest(http.MethodPost, "/v1/setup/complete", nil))

	if first.Code != second.Code || first.Body.String() != second.Body.String() {
		t.Fatalf("not idempotent: first (%d, %q) != second (%d, %q)",
			first.Code, first.Body.String(), second.Code, second.Body.String())
	}
}
