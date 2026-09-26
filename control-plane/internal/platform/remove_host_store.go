package platform

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// RemovableHost reads what the host-removal checks need; ErrHostNotFound when
// there is no such host.
func (s *Store) RemovableHost(ctx context.Context, hostID string) (RemovableHost, error) {
	h := RemovableHost{ID: hostID}
	err := s.pool.QueryRow(ctx, `
		SELECT node_name, install_mode, updater_present FROM hosts WHERE id = $1::uuid
	`, hostID).Scan(&h.NodeName, &h.InstallMode, &h.UpdaterPresent)
	if errors.Is(err, pgx.ErrNoRows) {
		return RemovableHost{}, ErrHostNotFound
	}
	if err != nil {
		return RemovableHost{}, fmt.Errorf("read host: %w", err)
	}
	return h, nil
}
