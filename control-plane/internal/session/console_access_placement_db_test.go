package session

import (
	"context"
	"errors"
	"testing"
)

// TestConsoleAccessPlacementHold is amendment 18's placement-hold acceptance:
// no new session lands on a host while its console access is `applying`, or
// while a PATCH-triggered hold has not yet been settled by an agreeing
// report; a host with no console_capabilities row (no access reported at
// all) is unaffected, exactly as before this amendment.
func TestConsoleAccessPlacementHold(t *testing.T) {
	pool := testDB(t)
	s := seed(t, pool, 1)
	ctx := context.Background()
	store := NewStore(pool)
	p := launchParams(s)

	// Baseline: no console_capabilities row at all — placement is unaffected.
	first, err := store.ScheduleAndCreate(ctx, p)
	must(t, err)
	if first.HostID == nil || *first.HostID != s.hostID {
		t.Fatalf("baseline placement used %v, want %s", first.HostID, s.hostID)
	}
	_, err = store.Transition(ctx, first.ID, StateFailed, nil, nil)
	must(t, err)

	// `applying`: held regardless of the hold marker.
	_, err = pool.Exec(ctx, `
		INSERT INTO console_capabilities (host_id, capabilities, updated_at)
		VALUES ($1::uuid, $2::jsonb, now())
		ON CONFLICT (host_id) DO UPDATE SET capabilities = EXCLUDED.capabilities, updated_at = now()
	`, s.hostID, `{"connectors":[],"audio_sinks":[],"input_devices":[],"access":{"state":"applying","target":true,"request_id":"r1","reason":null,"started_at":null,"finished_at":null,"summary":"replacing"}}`)
	must(t, err)
	_, err = store.ScheduleAndCreate(ctx, p)
	if !errors.Is(err, ErrNoHostAvailable) {
		t.Fatalf("applying host was placeable: err=%v", err)
	}

	// Settled access (`on`), but the PATCH-pending hold marker is still set:
	// still held.
	_, err = pool.Exec(ctx, `
		UPDATE console_capabilities
		   SET capabilities = jsonb_set(jsonb_set($2::jsonb, '{_placement_hold_pending}', 'true', true), '{access,state}', '"on"', true)
		 WHERE host_id::text = $1
	`, s.hostID, `{"connectors":[],"audio_sinks":[],"input_devices":[],"access":{"state":"on","target":true,"request_id":"r1","reason":null,"started_at":null,"finished_at":null,"summary":"on"}}`)
	must(t, err)
	_, err = store.ScheduleAndCreate(ctx, p)
	if !errors.Is(err, ErrNoHostAvailable) {
		t.Fatalf("hold-pending host was placeable: err=%v", err)
	}

	// The hold clears (an agreeing report settled it): placeable again.
	_, err = pool.Exec(ctx, `
		UPDATE console_capabilities
		   SET capabilities = capabilities - '_placement_hold_pending'
		 WHERE host_id::text = $1
	`, s.hostID)
	must(t, err)
	second, err := store.ScheduleAndCreate(ctx, p)
	must(t, err)
	if second.HostID == nil || *second.HostID != s.hostID {
		t.Fatalf("settled placement used %v, want %s", second.HostID, s.hostID)
	}
}
