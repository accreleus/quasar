package agentws

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/accreleus/quasar/control-plane/internal/hostenroll"
)

// seedAdmin returns a user id to own minted tokens (created_by is NOT NULL).
func seedAdmin(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	var id string
	err := pool.QueryRow(context.Background(), `
		INSERT INTO users (email, username, password_hash, role)
		VALUES ('enroll-admin@test', 'enroll-admin', 'x', 'admin')
		ON CONFLICT (email) DO UPDATE SET username = EXCLUDED.username
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
	// guard must not break.
	connected = false
	if _, err := st.enrollHost(ctx, "contested", "0.1.0", "static-token", "static-token"); err != nil {
		t.Fatalf("re-enrolling a disconnected host: %v", err)
	}
}
