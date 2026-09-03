package agentws

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/accreleus/quasar/control-plane/internal/hostenroll"
)

// seedAdmin returns a user id to own minted tokens.
func seedAdmin(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	var id string
	err := pool.QueryRow(context.Background(), `
		INSERT INTO users (email, username, password_hash, role)
		VALUES ('enroll-admin@test', 'enroll-admin', 'x', 'admin')
		ON CONFLICT (username) DO UPDATE SET role = EXCLUDED.role
		RETURNING id::text`).Scan(&id)
	if err != nil {
		t.Fatalf("seed admin: %v", err)
	}
	return id
}

// storeWithMintedTokens wires the real redemption path, as NewHandler does.
func storeWithMintedTokens(pool *pgxpool.Pool, connected func(string) bool) *agentStore {
	return &agentStore{
		pool:             pool,
		isAgentConnected: connected,
		redeemEnrollment: hostenroll.Redeem,
	}
}

// A minted token enrolls a host, and is single-use: the replay that would let a second
// machine (or an attacker who saw the token) claim a host must fail.
func TestMintedEnrollmentTokenIsSingleUse(t *testing.T) {
	pool := testPool(t)
	admin := seedAdmin(t, pool)
	ctx := context.Background()
	st := storeWithMintedTokens(pool, func(string) bool { return false })
	mint := hostenroll.NewStore(pool)

	_, plaintext, err := mint.Mint(ctx, hostenroll.MintParams{CreatedBy: admin})
	if err != nil {
		t.Fatalf("mint: %v", err)
	}

	// The static config token is deliberately NOT the one being presented.
	res, err := st.enrollHost(ctx, "minted-1", "0.1.0", plaintext, "static-token")
	if err != nil {
		t.Fatalf("first enrollment with a minted token: %v", err)
	}
	if res.NodeSecret == "" {
		t.Fatal("enrollment returned no node secret")
	}

	if _, err := st.enrollHost(ctx, "minted-2", "0.1.0", plaintext, "static-token"); !errors.Is(err, ErrInvalidEnrollmentToken) {
		t.Fatalf("replay of a single-use token: got %v, want ErrInvalidEnrollmentToken", err)
	}
}

// Expired, revoked, and bound-to-another-node tokens are all refused, and all with the
// same error — a caller probing /agent/ws must not learn which.
func TestMintedEnrollmentTokenRefusals(t *testing.T) {
	pool := testPool(t)
	admin := seedAdmin(t, pool)
	ctx := context.Background()
	st := storeWithMintedTokens(pool, func(string) bool { return false })
	mint := hostenroll.NewStore(pool)

	past := time.Now().Add(-time.Minute)
	_, expired, err := mint.Mint(ctx, hostenroll.MintParams{CreatedBy: admin, ExpiresAt: &past})
	if err != nil {
		t.Fatalf("mint expired: %v", err)
	}
	if _, err := st.enrollHost(ctx, "n-exp", "0.1.0", expired, ""); !errors.Is(err, ErrInvalidEnrollmentToken) {
		t.Fatalf("expired token: got %v, want ErrInvalidEnrollmentToken", err)
	}

	row, revoked, err := mint.Mint(ctx, hostenroll.MintParams{CreatedBy: admin})
	if err != nil {
		t.Fatalf("mint revoked: %v", err)
	}
	if err := mint.Revoke(ctx, row.ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if _, err := st.enrollHost(ctx, "n-rev", "0.1.0", revoked, ""); !errors.Is(err, ErrInvalidEnrollmentToken) {
		t.Fatalf("revoked token: got %v, want ErrInvalidEnrollmentToken", err)
	}

	// Binding is what stops a leaked token becoming a host it was not minted for.
	_, bound, err := mint.Mint(ctx, hostenroll.MintParams{CreatedBy: admin, NodeName: "the-right-host"})
	if err != nil {
		t.Fatalf("mint bound: %v", err)
	}
	if _, err := st.enrollHost(ctx, "the-wrong-host", "0.1.0", bound, ""); !errors.Is(err, ErrInvalidEnrollmentToken) {
		t.Fatalf("bound token on another node: got %v, want ErrInvalidEnrollmentToken", err)
	}
	if _, err := st.enrollHost(ctx, "the-right-host", "0.1.0", bound, ""); err != nil {
		t.Fatalf("bound token on its own node: %v", err)
	}
}

// Existing deployments carry only the static ENROLLMENT_TOKEN. It must keep working
// across this upgrade, and a wrong value must still be refused.
func TestStaticEnrollmentTokenStillWorks(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	st := storeWithMintedTokens(pool, func(string) bool { return false })

	if _, err := st.enrollHost(ctx, "legacy-host", "0.1.0", "static-token", "static-token"); err != nil {
		t.Fatalf("static token enrollment: %v", err)
	}
	if _, err := st.enrollHost(ctx, "legacy-host-2", "0.1.0", "wrong", "static-token"); !errors.Is(err, ErrInvalidEnrollmentToken) {
		t.Fatalf("wrong static token: got %v, want ErrInvalidEnrollmentToken", err)
	}
}

// #96: enrollment rotates node_secret, so re-enrolling onto a node_name whose agent is
// LIVE is identity takeover — the incumbent's next reconnect would fail and the scheduler
// would keep placing work on the row the caller now authenticates as. Refuse it while the
// agent is connected; allow it once that host is genuinely gone.
func TestEnrollmentCannotTakeOverAConnectedHost(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	connected := true
	st := storeWithMintedTokens(pool, func(string) bool { return connected })

	// The incumbent enrolls while nothing is connected yet.
	connected = false
	first, err := st.enrollHost(ctx, "contested", "0.1.0", "static-token", "static-token")
	if err != nil {
		t.Fatalf("initial enrollment: %v", err)
	}

	// Its agent is now live: a second enrollment under the same name is refused, even
	// though the credential presented is perfectly valid.
	connected = true
	if _, err := st.enrollHost(ctx, "contested", "0.1.0", "static-token", "static-token"); !errors.Is(err, ErrHostAgentConnected) {
		t.Fatalf("takeover while connected: got %v, want ErrHostAgentConnected", err)
	}

	// And the incumbent's secret is untouched — the refusal must not have rotated it.
	res, err := st.reconnectHost(ctx, "contested", "0.1.0", first.NodeSecret)
	if err != nil {
		t.Fatalf("incumbent reconnect after a refused takeover: %v", err)
	}
	if res.HostID != first.HostID {
		t.Fatalf("incumbent host id changed: %s -> %s", first.HostID, res.HostID)
	}

	// A host whose agent is gone still re-enrolls: that is the legitimate case this
	// guard must not break. markOffline is what really runs on every path that ends
	// the read loop, and the guard's DB half now requires it.
	connected = false
	if err := st.markOffline(ctx, first.HostID); err != nil {
		t.Fatalf("mark offline: %v", err)
	}
	if _, err := st.enrollHost(ctx, "contested", "0.1.0", "static-token", "static-token"); err != nil {
		t.Fatalf("re-enrolling a disconnected host: %v", err)
	}
}

// usedCount reads a minted token's consume tally straight from the row.
func usedCount(t *testing.T, pool *pgxpool.Pool, id string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT used_count FROM host_enrollments WHERE id::text = $1`, id).Scan(&n); err != nil {
		t.Fatalf("read used_count: %v", err)
	}
	return n
}

// /agent/ws is reachable pre-auth, so the takeover guard must never answer before the
// credential does: a bad token gets the same plain refusal whether the node_name is live,
// dead, or unknown. Otherwise the distinct ErrHostAgentConnected is an existence oracle.
func TestBadCredentialNeverRevealsALiveHost(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	connected := false
	st := storeWithMintedTokens(pool, func(string) bool { return connected })

	if _, err := st.enrollHost(ctx, "oracle-host", "0.1.0", "static-token", "static-token"); err != nil {
		t.Fatalf("initial enrollment: %v", err)
	}
	connected = true

	if _, err := st.enrollHost(ctx, "oracle-host", "0.1.0", "wrong", "static-token"); !errors.Is(err, ErrInvalidEnrollmentToken) {
		t.Fatalf("bad credential on a live node_name: got %v, want ErrInvalidEnrollmentToken", err)
	}
	if _, err := st.enrollHost(ctx, "no-such-host", "0.1.0", "wrong", "static-token"); !errors.Is(err, ErrInvalidEnrollmentToken) {
		t.Fatalf("bad credential on an unknown node_name: got %v, want ErrInvalidEnrollmentToken", err)
	}
}

// Validating the credential before the takeover guard means a minted token is consumed
// first — so the refusal has to give the use back, or an operator's single-use token is
// destroyed by an attempt that was refused for a reason they can fix.
func TestRefusedTakeoverDoesNotBurnAMintedToken(t *testing.T) {
	pool := testPool(t)
	admin := seedAdmin(t, pool)
	ctx := context.Background()
	connected := false
	st := storeWithMintedTokens(pool, func(string) bool { return connected })
	mint := hostenroll.NewStore(pool)

	first, err := st.enrollHost(ctx, "burned", "0.1.0", "static-token", "static-token")
	if err != nil {
		t.Fatalf("initial enrollment: %v", err)
	}
	row, plaintext, err := mint.Mint(ctx, hostenroll.MintParams{CreatedBy: admin, NodeName: "burned"})
	if err != nil {
		t.Fatalf("mint: %v", err)
	}

	connected = true
	if _, err := st.enrollHost(ctx, "burned", "0.1.0", plaintext, ""); !errors.Is(err, ErrHostAgentConnected) {
		t.Fatalf("takeover while connected: got %v, want ErrHostAgentConnected", err)
	}
	if n := usedCount(t, pool, row.ID); n != 0 {
		t.Fatalf("a refused takeover burned the token: used_count = %d, want 0", n)
	}

	connected = false
	if err := st.markOffline(ctx, first.HostID); err != nil {
		t.Fatalf("mark offline: %v", err)
	}
	if _, err := st.enrollHost(ctx, "burned", "0.1.0", plaintext, ""); err != nil {
		t.Fatalf("the same token once the host is gone: %v", err)
	}
	if n := usedCount(t, pool, row.ID); n != 1 {
		t.Fatalf("used_count after the successful enrollment = %d, want 1", n)
	}
}

// The registry only sees connections on THIS process, so a host connected to a sibling
// replica is live in the database and invisible in memory. It must still refuse a takeover.
func TestEnrollmentRefusesAHostOnlyTheDatabaseCallsLive(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	st := storeWithMintedTokens(pool, func(string) bool { return false }) // nothing local

	res, err := st.enrollHost(ctx, "other-replica", "0.1.0", "static-token", "static-token")
	if err != nil {
		t.Fatalf("initial enrollment: %v", err)
	}
	if _, err := st.enrollHost(ctx, "other-replica", "0.1.0", "static-token", "static-token"); !errors.Is(err, ErrHostAgentConnected) {
		t.Fatalf("takeover of an online row: got %v, want ErrHostAgentConnected", err)
	}

	// Offline is the whole condition: the row must be re-enrollable once its agent is
	// recorded as gone, or a host that lost its node_secret could never come back.
	if err := st.markOffline(ctx, res.HostID); err != nil {
		t.Fatalf("mark offline: %v", err)
	}
	if _, err := st.enrollHost(ctx, "other-replica", "0.1.0", "static-token", "static-token"); err != nil {
		t.Fatalf("re-enrolling an offline row: %v", err)
	}
}

// A database outage during redemption is not "your token is bad". Reporting it as
// auth_failed sends the operator to rotate a credential that was fine.
func TestRedemptionOutageIsNotAnAuthFailure(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	outage := errors.New("server closed the connection unexpectedly")
	st := &agentStore{
		pool:             pool,
		isAgentConnected: func(string) bool { return false },
		redeemEnrollment: func(context.Context, hostenroll.DBTX, string, string) error { return outage },
	}

	_, err := st.enrollHost(ctx, "outage-host", "0.1.0", "some-token", "static-token")
	if errors.Is(err, ErrInvalidEnrollmentToken) {
		t.Fatalf("a redemption outage was reported as an auth failure: %v", err)
	}
	if !errors.Is(err, outage) {
		t.Fatalf("got %v, want an error wrapping %v", err, outage)
	}
}

// A store built without a redeemer (older wiring, or a test fixture) must refuse the
// unknown token rather than panic on the pre-auth path; the static token still enrolls.
func TestEnrollmentWithoutMintedTokenSupport(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	st := &agentStore{pool: pool}

	if _, err := st.enrollHost(ctx, "no-minting", "0.1.0", "whatever", "static-token"); !errors.Is(err, ErrInvalidEnrollmentToken) {
		t.Fatalf("minted token with no redeemer wired: got %v, want ErrInvalidEnrollmentToken", err)
	}
	if _, err := st.enrollHost(ctx, "no-minting", "0.1.0", "static-token", "static-token"); err != nil {
		t.Fatalf("static token with no redeemer wired: %v", err)
	}
}
