package wtendpoint

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// DB is the slice of pgxpool.Pool this package uses.
type DB interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// ErrHostNotFound is returned for an id that names no host.
var ErrHostNotFound = errors.New("host not found")

// Load reads one host's reported endpoint and override. A malformed stored column reads as
// not reported, never an error.
func Load(ctx context.Context, db DB, hostID string) (Host, error) {
	var (
		online                              bool
		transports, addresses, certificates []byte
		port                                *int
		publicURL                           *string
		publicCert                          string
	)
	err := db.QueryRow(ctx, `
		SELECT (status = 'online' AND agent_disconnected_at IS NULL),
		       transports, wt_port, wt_addresses, wt_certificates, wt_public_url, wt_public_certificate
		FROM hosts WHERE id = $1::uuid
	`, hostID).Scan(&online, &transports, &port, &addresses, &certificates, &publicURL, &publicCert)
	if errors.Is(err, pgx.ErrNoRows) {
		return Host{}, ErrHostNotFound
	}
	if err != nil {
		return Host{}, fmt.Errorf("load host webtransport: %w", err)
	}
	h := Host{Online: online}
	if len(transports) > 0 {
		r := &Reported{Addresses: []string{}, Certificates: []Certificate{}}
		if json.Unmarshal(transports, &r.Transports) == nil {
			if port != nil {
				r.Port = *port
			}
			_ = json.Unmarshal(addresses, &r.Addresses)
			_ = json.Unmarshal(certificates, &r.Certificates)
			h.Reported = r
		}
	}
	if publicURL != nil {
		h.Override = &Override{PublicURL: *publicURL, Certificate: publicCert}
	}
	return h, nil
}

// Record stores the agent's capacity report wholesale: a report without WebTransport clears the
// columns (agent-api.md capacity.transports). r nil means none reported.
func Record(ctx context.Context, db DB, hostID string, r *Reported) error {
	var transports, addresses, certificates any
	var port any
	if r != nil {
		transports, _ = json.Marshal(r.Transports)
		if r.Serves() {
			port = r.Port
			addresses, _ = json.Marshal(nonNil(r.Addresses))
			certificates, _ = json.Marshal(nonNilCerts(r.Certificates))
		}
	}
	// A nil any binds as SQL NULL; []byte values bind as JSON text via the jsonb cast.
	if _, err := db.Exec(ctx, `
		UPDATE hosts SET transports = $2::jsonb, wt_port = $3, wt_addresses = $4::jsonb, wt_certificates = $5::jsonb
		WHERE id = $1::uuid
	`, hostID, asText(transports), port, asText(addresses), asText(certificates)); err != nil {
		return fmt.Errorf("record host webtransport: %w", err)
	}
	return nil
}

// SetOverride writes (or with a nil url clears) the admin's public-URL override.
func SetOverride(ctx context.Context, db DB, hostID string, publicURL *string, certificate string) error {
	tag, err := db.Exec(ctx, `
		UPDATE hosts SET wt_public_url = $2, wt_public_certificate = $3 WHERE id = $1::uuid
	`, hostID, publicURL, certificate)
	if err != nil {
		return fmt.Errorf("set host webtransport override: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrHostNotFound
	}
	return nil
}

func asText(v any) any {
	if b, ok := v.([]byte); ok {
		return string(b)
	}
	return nil
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func nonNilCerts(c []Certificate) []Certificate {
	if c == nil {
		return []Certificate{}
	}
	return c
}
