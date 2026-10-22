package session

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// #784: a database at 0105 holding apps with default_encode_slots <= 0 comes
// through 0106 with those rows at 1, every other row untouched, and the column
// refuses 0 afterwards.
func TestMigration0106RepairsZeroSlotAppsAndForbidsThem(t *testing.T) {
	url := scratchDB(t)
	m := newMigrator(t, url)
	migrateTo(t, m, 105)
	pool, err := pgxpool.New(context.Background(), url)
	must(t, err)
	t.Cleanup(pool.Close)
	ctx := context.Background()

	app := func(name string, slots int) string {
		t.Helper()
		var id string
		must(t, pool.QueryRow(ctx, `INSERT INTO apps (name, default_encode_slots)
			VALUES ($1, $2) RETURNING id::text`, name, slots).Scan(&id))
		return id
	}
	zero, negative, two := app("zero", 0), app("negative", -3), app("two", 2)

	migrateTo(t, m, 106)

	slots := func(id string) int {
		t.Helper()
		var n int
		must(t, pool.QueryRow(ctx, `SELECT default_encode_slots FROM apps WHERE id = $1::uuid`, id).Scan(&n))
		return n
	}
	for id, want := range map[string]int{zero: 1, negative: 1, two: 2} {
		if got := slots(id); got != want {
			t.Errorf("app %s: default_encode_slots = %d, want %d", id, got, want)
		}
	}
	if err := exec(t, pool, `INSERT INTO apps (name, default_encode_slots) VALUES ('again', 0)`); err == nil {
		t.Error("an app with default_encode_slots = 0 was accepted after 0106")
	}
	if err := exec(t, pool, `UPDATE apps SET default_encode_slots = 0 WHERE id = $1::uuid`, two); err == nil {
		t.Error("default_encode_slots was updated to 0 after 0106")
	}

	// The down drops the CHECK and leaves the repaired values.
	migrateTo(t, m, 105)
	must(t, exec(t, pool, `UPDATE apps SET default_encode_slots = 0 WHERE id = $1::uuid`, two))
}
