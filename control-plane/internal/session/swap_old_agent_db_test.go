package session

// swap_old_agent_db_test.go — #1429: a swap into a managed-home app has no
// owner for its home between its guard and its commit on an agent without
// terminal_home_cleanup_v1, so a launch of that app (or another session's swap
// into it) admitted in the window passes the single-writer guard and mounts the
// home twice. Such a swap is refused, as the swapper already refuses a
// managed-home swap whose agent connection is gone. Semantics: control-api.md
// §Sessions, POST /v1/sessions/{id}/swap.

import (
	"context"
	"errors"
	"testing"
	"time"
)

// seedSwapTarget is a managed-home app the fixture's session can swap into: a
// plain desktop, or the Steam parent (the Steam family, holding the parent's
// two slots — so the caller reserves them on the session's own app).
func seedSwapTarget(t *testing.T, f *warmFixture, name string) (appID, claimApp string) {
	t.Helper()
	if name == "Steam" {
		return f.steam, f.steam
	}
	id := seedDesktop(t, f, false)
	return id, id
}

// startedSwapSource launches the session's starting app, reserving the slots
// the swap target needs: the Steam parent holds two.
func startedSwapSource(t *testing.T, f *warmFixture, name string) (sess, from string) {
	t.Helper()
	from = seedDesktop(t, f, false)
	if name == "Steam" {
		must(t, exec(t, f.pool, `UPDATE apps SET default_encode_slots=2 WHERE id=$1::uuid`, from))
	}
	return f.startedLaunch(t, from), from
}

// #1429: on an agent without terminal home cleanup a swap into a managed-home
// app is refused before any guard, so nothing is held and the session is left
// untouched — for a desktop and for the Steam family alike.
func TestSwapIntoAManagedHomeOnAnAgentWithoutCleanupIsRefused(t *testing.T) {
	for _, name := range []string{"desktop", "Steam"} {
		t.Run(name, func(t *testing.T) {
			f := newWarmFixture(t, 8)
			sess, from := startedSwapSource(t, f, name)
			target, claimApp := seedSwapTarget(t, f, name)
			f.disp.setIncapable(true)
			ctx := context.Background()

			if _, err := f.coord.Swap(ctx, sess, target); !errors.Is(err, ErrSessionNotSwappable) {
				t.Fatalf("swap into %s on a cleanup-less agent = %v, want session_not_swappable", name, err)
			}
			if h, tok := f.claimHolder(t, claimApp); h != "" || tok != "" {
				t.Errorf("target claim holder=%q token=%q after the refusal, want none", h, tok)
			}
			if h, _ := f.claimHolder(t, f.steam); h != "" {
				t.Errorf("Steam hold = %q after the refusal, want none", h)
			}
			f.disp.mu.Lock()
			dispatched := len(f.disp.swaps)
			f.disp.mu.Unlock()
			if dispatched != 0 {
				t.Errorf("%d session_swap_app dispatched after the refusal, want none", dispatched)
			}
			got, err := f.store.Get(ctx, sess)
			must(t, err)
			if got.AppID != from || got.State != StateRunning || deref(got.StateDetail) == swapDetailInProgress {
				t.Errorf("session after the refusal = app %s state %s detail %q, want %s still running and not swapping",
					got.AppID, got.State, deref(got.StateDetail), from)
			}
		})
	}
}

// #1429: an agent that reports terminal home cleanup takes the target's hold
// and performs the swap exactly as before.
func TestSwapIntoAManagedHomeOnACapableAgentProceeds(t *testing.T) {
	for _, name := range []string{"desktop", "Steam"} {
		t.Run(name, func(t *testing.T) {
			f := newWarmFixture(t, 8)
			sess, _ := startedSwapSource(t, f, name)
			target, claimApp := seedSwapTarget(t, f, name)
			ctx := context.Background()

			if _, err := f.coord.Swap(ctx, sess, target); err != nil {
				t.Fatalf("swap into %s on a capable agent: %v", name, err)
			}
			f.disp.waitFor(t, "swap")
			if h, tok := f.claimHolder(t, claimApp); h != sess || tok == "" {
				t.Errorf("target claim holder=%q token=%q after the guard, want the session %s under a token", h, tok, sess)
			}
			if got := readRow(t, f.pool, sess); got.Detail != swapDetailInProgress {
				t.Errorf("session detail = %q after a capable swap, want %q", got.Detail, swapDetailInProgress)
			}
			f.commitSwap(t, sess)
			got, err := f.store.Get(ctx, sess)
			must(t, err)
			if got.AppID != target {
				t.Errorf("app_id = %s after the commit, want the target %s", got.AppID, target)
			}
		})
	}
}

// #1429 (review of #1425): an agent that loses terminal home cleanup between
// the swap's guard and its handoff cannot take the target's hold, so the swap
// is refused and the swapper's unstarted-swap release leaves nothing held.
func TestSwapDowngradedBeforeHandoffIsRefusedAndLeavesNothingHeld(t *testing.T) {
	f := newWarmFixture(t, 8)
	from := seedDesktop(t, f, false)
	sess := f.startedLaunch(t, from)
	target := seedDesktop(t, f, false)
	ctx := context.Background()
	f.disp.downgradeOnSwap()

	if _, err := f.coord.Swap(ctx, sess, target); err != nil {
		t.Fatalf("swap guarded on a capable agent: %v", err)
	}
	// The refusal runs on the swap's own goroutine: wait for its detail.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if readRow(t, f.pool, sess).Detail == swapDetailRejected {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got := readRow(t, f.pool, sess); got.Detail != swapDetailRejected {
		t.Fatalf("session detail after the downgrade = %q, want %q", got.Detail, swapDetailRejected)
	}
	if h, tok := f.claimHolder(t, target); h != "" || tok != "" {
		t.Errorf("target claim holder=%q token=%q after the refusal, want none", h, tok)
	}
	s, err := f.store.Get(ctx, sess)
	must(t, err)
	if s.AppID != from || s.State != StateRunning {
		t.Errorf("session after the refusal = app %s state %s, want %s still running", s.AppID, s.State, from)
	}
}
