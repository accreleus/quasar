package audit

import (
	"context"
	"encoding/json"
	"os"
	"strings"
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

// #1209: an oversized row used to be refused, and TryRecord then only LOGGED
// the refusal — the operation happened and nothing recorded it. The row is now
// written cut down, inside the same bound. The payload is NULs, which jsonb also
// refuses on its own (A3), so this pins both halves of the guarantee at once.
func TestWritesOversizedDetailsCutDown(t *testing.T) {
	s := testStore(t)
	// 5000 NULs: 30 000 encoded bytes, and every one of them unstoreable as-is.
	value := strings.Repeat("\x00", 5000)
	if err := s.Record(context.Background(), "", "x", "host", "h",
		map[string]any{"value": value, "count": 1}); err != nil {
		t.Fatalf("an oversized row must be written, not refused: %v", err)
	}

	var raw []byte
	var size int
	if err := s.pool.QueryRow(context.Background(),
		`SELECT details, octet_length(details::text) FROM admin_activity WHERE action = 'x'`).Scan(&raw, &size); err != nil {
		t.Fatalf("no audit row for an oversized payload: %v", err)
	}
	if size > maxDetailBytes {
		t.Fatalf("row is %d bytes, over the %d-byte CHECK", size, maxDetailBytes)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("details are not a JSON object: %v", err)
	}
	if got[detailsTruncatedKey] != true || got["value_truncated"] != true {
		t.Errorf("cut row is not flagged: %#v", got)
	}
	if got[detailsSanitizedKey] != true || got["value_sanitized"] != true {
		t.Errorf("row with NULs is not flagged sanitized: %#v", got)
	}
	stored, _ := got["value"].(string)
	if stored == "" {
		t.Fatal("the cut row lost its value entirely")
	}
	if strings.ContainsRune(stored, 0) {
		t.Error("the stored value still holds a NUL")
	}
	if !strings.ContainsRune(stored, replacementRune) {
		t.Error("the stored value does not show where the NULs were")
	}
	if got["count"] != float64(1) {
		t.Errorf("a short field was lost: %#v", got)
	}
}
