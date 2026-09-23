package session

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestMigration0091BackfillsOnlyCanonicalApps(t *testing.T) {
	url := scratchDB(t)
	m := newMigrator(t, url)
	migrateTo(t, m, 90)
	pool, err := pgxpool.New(context.Background(), url)
	must(t, err)
	t.Cleanup(pool.Close)
	ctx := context.Background()
	var parent, child string
	must(t, pool.QueryRow(ctx, `INSERT INTO apps(name) VALUES ('existing parent') RETURNING id::text`).Scan(&parent))
	must(t, pool.QueryRow(ctx, `INSERT INTO apps(name,parent_app_id,external_source,external_id)
		VALUES ('existing tile',$1::uuid,'steam','42') RETURNING id::text`, parent).Scan(&child))
	migrateTo(t, m, 91)
	var mode, revision string
	must(t, pool.QueryRow(ctx, `SELECT mode,revision::text FROM app_placement WHERE app_id=$1::uuid`, parent).Scan(&mode, &revision))
	if mode != "all_eligible" || revision != "0" {
		t.Fatalf("canonical backfill = (%s,%s), want all_eligible revision 0", mode, revision)
	}
	var count int
	must(t, pool.QueryRow(ctx, `SELECT count(*) FROM app_placement WHERE app_id=$1::uuid`, child).Scan(&count))
	if count != 0 {
		t.Fatalf("derived tile received %d independent placement rows", count)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO app_placement(app_id,mode) VALUES ($1::uuid,'fixed')`, child); err == nil {
		t.Fatal("derived tile accepted an independent placement row")
	}
	var next string
	must(t, pool.QueryRow(ctx, `INSERT INTO apps(name) VALUES ('future parent') RETURNING id::text`).Scan(&next))
	must(t, pool.QueryRow(ctx, `SELECT mode FROM app_placement WHERE app_id=$1::uuid`, next).Scan(&mode))
	if mode != "all_eligible" {
		t.Fatalf("future parent placement = %q", mode)
	}
}
