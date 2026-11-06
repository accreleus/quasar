package session

// migration_0111_db_test.go — WebTransport, transport #2 (amendment 54). Up is additive and reads
// as WebRTC everywhere; down refuses while a live session uses WebTransport. Runs in a scratch
// database: it migrates down.

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestMigration0111UpAndDown(t *testing.T) {
	url := scratchDB(t)
	m := newMigrator(t, url)
	migrateTo(t, m, 110)

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("connect scratch: %v", err)
	}
	defer pool.Close()

	var userID, appID, hostID string
	must(t, pool.QueryRow(ctx, `INSERT INTO users (email, username, password_hash) VALUES ('m@t.local','m','x') RETURNING id::text`).Scan(&userID))
	must(t, pool.QueryRow(ctx, `INSERT INTO apps (name) VALUES ('m') RETURNING id::text`).Scan(&appID))
	must(t, pool.QueryRow(ctx, `INSERT INTO hosts (node_name, status) VALUES ('m','online') RETURNING id::text`).Scan(&hostID))
	sessionID := insertSessionRow(t, pool, userID, appID, &hostID, "running")
	must(t, exec(t, pool, `INSERT INTO session_tokens (session_id, token_hash, expires_at) VALUES ($1::uuid, 'h', now() + interval '1 minute')`, sessionID))

	migrateTo(t, m, 111)

	// Every existing row reads as WebRTC; nothing was backfilled with a value.
	var profile, transport, certMode, kind string
	var reason, fallback, publicURL, hostTransports *string
	must(t, pool.QueryRow(ctx, `SELECT transport FROM launch_profiles LIMIT 1`).Scan(&profile))
	must(t, pool.QueryRow(ctx, `SELECT transport, transport_reason, transport_fallback_reason FROM sessions WHERE id = $1::uuid`, sessionID).
		Scan(&transport, &reason, &fallback))
	must(t, pool.QueryRow(ctx, `SELECT wt_public_certificate, wt_public_url, transports::text FROM hosts WHERE id = $1::uuid`, hostID).
		Scan(&certMode, &publicURL, &hostTransports))
	must(t, pool.QueryRow(ctx, `SELECT kind FROM session_tokens WHERE token_hash = 'h'`).Scan(&kind))
	if profile != "webrtc" || transport != "webrtc" || reason != nil || fallback != nil ||
		certMode != "public" || publicURL != nil || hostTransports != nil || kind != "signaling" {
		t.Errorf("defaults: profile=%q session=%q reason=%v fallback=%v cert=%q url=%v transports=%v kind=%q",
			profile, transport, reason, fallback, certMode, publicURL, hostTransports, kind)
	}

	// The CHECKs.
	for name, sql := range map[string]string{
		"launch_profiles.transport":   `UPDATE launch_profiles SET transport = 'quic'`,
		"sessions.transport":          `UPDATE sessions SET transport = 'webtransport_opt_in'`,
		"hosts.wt_port high":          `UPDATE hosts SET wt_port = 65536`,
		"hosts.wt_port zero":          `UPDATE hosts SET wt_port = 0`,
		"hosts.wt_public_certificate": `UPDATE hosts SET wt_public_certificate = 'self'`,
		"session_tokens.kind":         `UPDATE session_tokens SET kind = 'refresh'`,
	} {
		if exec(t, pool, sql) == nil {
			t.Errorf("%s accepted a value outside its CHECK", name)
		}
	}
	must(t, exec(t, pool, `UPDATE launch_profiles SET transport = 'webtransport_opt_in'`))
	must(t, exec(t, pool, `UPDATE hosts SET wt_port = 65535`))

	// Down is free while no session uses WebTransport.
	must(t, exec(t, pool, `UPDATE launch_profiles SET transport = 'webrtc'`))
	must(t, exec(t, pool, `UPDATE sessions SET transport = 'webtransport'`))
	err = m.Migrate(110)
	if err == nil || !strings.Contains(err.Error(), "a WebTransport session is still live") {
		t.Fatalf("down with a live WebTransport session: err = %v, want the refusal", err)
	}
	_, _ = m.Close()
	m = newMigrator(t, url)
	if err := m.Force(111); err != nil {
		t.Fatalf("force: %v", err)
	}

	// An ended WebTransport session does not hold the down.
	must(t, exec(t, pool, `UPDATE sessions SET state = 'stopped' WHERE id = $1::uuid`, sessionID))
	migrateTo(t, m, 110)
	var cols int
	must(t, pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.columns
		WHERE (table_name = 'sessions' AND column_name IN ('transport','transport_reason','transport_fallback_reason'))
		   OR (table_name = 'launch_profiles' AND column_name = 'transport')
		   OR (table_name = 'session_tokens' AND column_name = 'kind')
		   OR (table_name = 'hosts' AND column_name IN ('transports','wt_port','wt_addresses','wt_certificates','wt_public_url','wt_public_certificate'))`).
		Scan(&cols))
	if cols != 0 {
		t.Errorf("%d migration 0111 columns survived the down", cols)
	}
	migrateTo(t, m, 111)
}
