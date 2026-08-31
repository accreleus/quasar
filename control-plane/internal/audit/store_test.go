package audit

import (
	"context"
	"os"
	"testing"

	"github.com/accreleus/quasar/control-plane/internal/migrate"
	"github.com/accreleus/quasar/control-plane/migrations"
	"github.com/jackc/pgx/v5/pgxpool"
)

func testStore(t *testing.T) *Store {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	if err := migrate.Run(migrations.FS, url); err != nil {
		t.Fatal(err)
	}
	pool, err := pgxpool.New(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(context.Background(), "DELETE FROM admin_activity"); err != nil {
		t.Fatal(err)
	}
	return NewStore(pool)
}

func TestRecordAndPaginate(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	actor := "11111111-1111-4111-8111-111111111111"
	if err := s.Record(ctx, actor, "host.restart", "host", "h1", map[string]any{"confirmed": true}); err != nil {
		t.Fatal(err)
	}
	if err := s.Record(ctx, actor, "host.settings.update", "host", "h1", map[string]any{"keys": []string{"encoder"}}); err != nil {
		t.Fatal(err)
	}
	items, next, err := s.List(ctx, 0, 1, ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || next == nil || items[0].Action != "host.settings.update" {
		t.Fatalf("unexpected first page: %#v next=%v", items, next)
	}
	older, _, err := s.List(ctx, *next, 10, ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(older) != 1 || older[0].Action != "host.restart" {
		t.Fatalf("unexpected older page: %#v", older)
	}
}

func TestRejectsOversizedDetails(t *testing.T) {
	s := testStore(t)
	if err := s.Record(context.Background(), "", "x", "host", "h", map[string]any{"value": string(make([]byte, 5000))}); err == nil {
		t.Fatal("expected oversized details rejection")
	}
}
