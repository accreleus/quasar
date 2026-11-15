package session

// Token kinds are separate credentials (schema.md session_tokens kind): the signaling redeem
// path refuses a transport token and leaves its row alone, and the transport mint never yields
// something /v1/signal accepts. The agent-side half (a signaling token is refused in a
// WebTransport hello) rests on the dispatch tests in webtransport_launch_db_test.go.

import (
	"context"
	"errors"
	"testing"
)

func TestSignalingRedeemRefusesATransportToken(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	s := seed(t, pool, 4)
	coord := newTestCoordinator(t, store, newFakeDispatcher(true), testLogger())
	ctx := context.Background()

	res, err := coord.Launch(ctx, s.userID, s.appID, StreamOverride{})
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	transport, err := store.MintTransportToken(ctx, res.Session.ID)
	if err != nil {
		t.Fatalf("mint transport token: %v", err)
	}

	if _, err := store.ConsumeSignalingToken(ctx, transport.Plaintext); !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("a transport token at the signaling redeem: err = %v, want ErrTokenInvalid", err)
	}
	var consumed bool
	must(t, pool.QueryRow(ctx, `SELECT consumed_at IS NOT NULL FROM session_tokens WHERE token_hash = $1`,
		transport.Hash).Scan(&consumed))
	if consumed {
		t.Error("the refused attempt stamped the transport token consumed")
	}

	// The signaling token minted at launch is still good, once.
	if _, err := store.ConsumeSignalingToken(ctx, res.SignalingToken); err != nil {
		t.Fatalf("the launch's signaling token after the transport attempt: %v", err)
	}
	if _, err := store.ConsumeSignalingToken(ctx, res.SignalingToken); !errors.Is(err, ErrTokenInvalid) {
		t.Errorf("second redeem of a signaling token: err = %v, want ErrTokenInvalid", err)
	}
}

func TestMintedTokensCarryTheirKind(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	s := seed(t, pool, 4)
	coord := newTestCoordinator(t, store, newFakeDispatcher(true), testLogger())
	ctx := context.Background()

	res, err := coord.Launch(ctx, s.userID, s.appID, StreamOverride{})
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	sig, err := store.MintSignalingToken(ctx, res.Session.ID)
	if err != nil {
		t.Fatalf("mint signaling: %v", err)
	}
	tr, err := store.MintTransportToken(ctx, res.Session.ID)
	if err != nil {
		t.Fatalf("mint transport: %v", err)
	}
	for hash, want := range map[string]string{
		hashToken(res.SignalingToken): "signaling", sig.Hash: "signaling", tr.Hash: "transport",
	} {
		var kind string
		must(t, pool.QueryRow(ctx, `SELECT kind FROM session_tokens WHERE token_hash = $1`, hash).Scan(&kind))
		if kind != want {
			t.Errorf("token %.8s kind = %q, want %q", hash, kind, want)
		}
	}

	// A transport token is not a signaling token even when it is the only one the caller holds.
	if _, err := store.ConsumeSignalingToken(ctx, tr.Plaintext); !errors.Is(err, ErrTokenInvalid) {
		t.Errorf("transport token at signaling: err = %v", err)
	}
}

func TestMintTransportTokenRefusesATerminalSession(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	s := seed(t, pool, 4)
	coord := newTestCoordinator(t, store, newFakeDispatcher(true), testLogger())
	ctx := context.Background()

	res, err := coord.Launch(ctx, s.userID, s.appID, StreamOverride{})
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	must(t, exec(t, pool, `UPDATE sessions SET state = 'stopped' WHERE id = $1::uuid`, res.Session.ID))
	if _, err := store.MintTransportToken(ctx, res.Session.ID); !errors.Is(err, ErrSessionTerminal) {
		t.Errorf("mint on a stopped session: err = %v, want ErrSessionTerminal", err)
	}
}
