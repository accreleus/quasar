package settings

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/accreleus/quasar/control-plane/internal/migrate"
	"github.com/accreleus/quasar/control-plane/migrations"
)

// testDB returns a migrated pool against TEST_DATABASE_URL, cleaned for a fresh slate.
// Skipped when the env var is unset so `go test` stays green without a database.
func testDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dbURL := os.Getenv("TEST_DATABASE_URL")
	if dbURL == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping DB integration test")
	}
	if err := migrate.Run(migrations.FS, dbURL); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if _, err := pool.Exec(ctx, `TRUNCATE instance_settings, users CASCADE`); err != nil {
		pool.Close()
		t.Fatalf("truncate: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func TestSeedDefaultsClosed(t *testing.T) {
	pool := testDB(t)
	s := NewStore(pool)
	ctx := context.Background()

	if err := s.Seed(ctx, ""); err != nil {
		t.Fatalf("seed: %v", err)
	}
	mode, err := s.RegistrationMode(ctx)
	if err != nil {
		t.Fatalf("mode: %v", err)
	}
	if mode != RegistrationClosed {
		t.Fatalf("default mode: got %q want closed", mode)
	}
}

func TestSeedTakesEnvThenIdempotent(t *testing.T) {
	pool := testDB(t)
	s := NewStore(pool)
	ctx := context.Background()

	if err := s.Seed(ctx, RegistrationOpen); err != nil {
		t.Fatalf("seed open: %v", err)
	}
	// A second seed with a different value is a no-op: the persisted row wins, so an
	// admin's runtime choice is never clobbered on a later boot.
	if err := s.Seed(ctx, RegistrationInviteOnly); err != nil {
		t.Fatalf("reseed: %v", err)
	}
	mode, _ := s.RegistrationMode(ctx)
	if mode != RegistrationOpen {
		t.Fatalf("idempotent seed: got %q want open (first seed wins)", mode)
	}
}

func TestSeedRejectsBadMode(t *testing.T) {
	pool := testDB(t)
	s := NewStore(pool)
	if err := s.Seed(context.Background(), "wide-open"); err == nil {
		t.Fatal("expected error seeding an invalid mode")
	}
}

func TestUpdateRegistrationMode(t *testing.T) {
	pool := testDB(t)
	s := NewStore(pool)
	ctx := context.Background()
	if err := s.Seed(ctx, RegistrationClosed); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Need a real admin id for updated_by (FK). Insert one directly.
	var adminID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO users (email, username, password_hash, role)
		VALUES ('a@x.io','admin','x','admin') RETURNING id::text`).Scan(&adminID); err != nil {
		t.Fatalf("seed admin: %v", err)
	}

	st, err := s.UpdateRegistrationMode(ctx, RegistrationInviteOnly, adminID)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if st.RegistrationMode != RegistrationInviteOnly {
		t.Fatalf("update mode: got %q want invite_only", st.RegistrationMode)
	}
	if st.UpdatedBy == nil || *st.UpdatedBy != adminID {
		t.Fatalf("updated_by not stamped: %v", st.UpdatedBy)
	}
	mode, _ := s.RegistrationMode(ctx)
	if mode != RegistrationInviteOnly {
		t.Fatalf("persisted mode: got %q want invite_only", mode)
	}
}

func TestSeedDefaultsStorageAuto(t *testing.T) {
	pool := testDB(t)
	s := NewStore(pool)
	ctx := context.Background()
	if err := s.Seed(ctx, ""); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// storage_provider has a column DEFAULT 'auto' (migration 0030) — the seed only
	// carries registration_mode, so a fresh row still reads 'auto'.
	prov, err := s.StorageProvider(ctx)
	if err != nil {
		t.Fatalf("storage provider: %v", err)
	}
	if prov != StorageAuto {
		t.Fatalf("default storage_provider: got %q want auto", prov)
	}
}

func TestStorageProviderDefaultsAutoWhenUnseeded(t *testing.T) {
	pool := testDB(t)
	s := NewStore(pool)
	// No Seed: a missing singleton must read as auto (the safe managed-home default).
	prov, err := s.StorageProvider(context.Background())
	if err != nil {
		t.Fatalf("storage provider: %v", err)
	}
	if prov != StorageAuto {
		t.Fatalf("unseeded storage_provider: got %q want auto", prov)
	}
}

func TestUpdateStorageProvider(t *testing.T) {
	pool := testDB(t)
	s := NewStore(pool)
	ctx := context.Background()
	if err := s.Seed(ctx, RegistrationClosed); err != nil {
		t.Fatalf("seed: %v", err)
	}
	var adminID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO users (email, username, password_hash, role)
		VALUES ('s@x.io','sadmin','x','admin') RETURNING id::text`).Scan(&adminID); err != nil {
		t.Fatalf("seed admin: %v", err)
	}

	st, err := s.UpdateStorageProvider(ctx, StorageLocal, adminID)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if st.StorageProvider != StorageLocal {
		t.Fatalf("update provider: got %q want local", st.StorageProvider)
	}
	// registration_mode must be untouched by a storage-only update.
	if st.RegistrationMode != RegistrationClosed {
		t.Fatalf("registration_mode clobbered: got %q want closed", st.RegistrationMode)
	}
	if st.UpdatedBy == nil || *st.UpdatedBy != adminID {
		t.Fatalf("updated_by not stamped: %v", st.UpdatedBy)
	}
	prov, _ := s.StorageProvider(ctx)
	if prov != StorageLocal {
		t.Fatalf("persisted provider: got %q want local", prov)
	}
}

func TestValidStorageProvider(t *testing.T) {
	for _, ok := range []string{StorageAuto, StorageLocal} {
		if !ValidStorageProvider(ok) {
			t.Errorf("ValidStorageProvider(%q) = false, want true", ok)
		}
	}
	// "volume" is REJECTED (#473 hard removal), not merely another invalid
	// value — IsRemovedVolumeProvider is what lets the PATCH handler tell an
	// admin why, instead of the generic "must be auto or local".
	for _, bad := range []string{"", "nfs", "Local", "cloud", "volume"} {
		if ValidStorageProvider(bad) {
			t.Errorf("ValidStorageProvider(%q) = true, want false", bad)
		}
	}
	if !IsRemovedVolumeProvider("volume") {
		t.Error(`IsRemovedVolumeProvider("volume") = false, want true`)
	}
	for _, notVolume := range []string{"", "nfs", "local", "auto"} {
		if IsRemovedVolumeProvider(notVolume) {
			t.Errorf("IsRemovedVolumeProvider(%q) = true, want false", notVolume)
		}
	}
}

// TestValidLibraryDiscoveryIntervalMinutes — the pure bounds check the PATCH
// handler and the instance_settings CHECK constraint (migration 0047) both
// implement; this pins the Go side's boundary.
func TestValidLibraryDiscoveryIntervalMinutes(t *testing.T) {
	for _, ok := range []int{15, 360, 10080} {
		if !ValidLibraryDiscoveryIntervalMinutes(ok) {
			t.Errorf("ValidLibraryDiscoveryIntervalMinutes(%d) = false, want true", ok)
		}
	}
	for _, bad := range []int{14, 10081, 0, -1} {
		if ValidLibraryDiscoveryIntervalMinutes(bad) {
			t.Errorf("ValidLibraryDiscoveryIntervalMinutes(%d) = true, want false", bad)
		}
	}
}

// TestLibraryDiscoveryIntervalAndAppDetailsDefaultsWhenUnseeded — the two
// admin-libraries amendment fields must read their column defaults (360
// minutes, appdetails off) even with no seeded row, the same fail-closed
// posture library_discovery_enabled already has.
func TestLibraryDiscoveryIntervalAndAppDetailsDefaultsWhenUnseeded(t *testing.T) {
	pool := testDB(t)
	s := NewStore(pool)
	ctx := context.Background()

	minutes, err := s.LibraryDiscoveryIntervalMinutes(ctx)
	if err != nil {
		t.Fatalf("interval: %v", err)
	}
	if minutes != 360 {
		t.Fatalf("unseeded interval: got %d want 360", minutes)
	}
	enabled, err := s.LibraryDiscoveryAppDetailsEnabled(ctx)
	if err != nil {
		t.Fatalf("appdetails: %v", err)
	}
	if enabled {
		t.Fatalf("unseeded appdetails: got true want false")
	}
}

// TestUpdateLibraryDiscoveryIntervalMinutes — the store-level write + read
// back, and that it does not disturb the master switch or storage provider.
func TestUpdateLibraryDiscoveryIntervalMinutes(t *testing.T) {
	pool := testDB(t)
	s := NewStore(pool)
	ctx := context.Background()
	if err := s.Seed(ctx, RegistrationClosed); err != nil {
		t.Fatalf("seed: %v", err)
	}
	var adminID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO users (email, username, password_hash, role)
		VALUES ('li@x.io','liadmin','x','admin') RETURNING id::text`).Scan(&adminID); err != nil {
		t.Fatalf("seed admin: %v", err)
	}

	st, err := s.UpdateLibraryDiscoveryIntervalMinutes(ctx, 120, adminID)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if st.LibraryDiscoveryIntervalMinutes != 120 {
		t.Fatalf("update interval: got %d want 120", st.LibraryDiscoveryIntervalMinutes)
	}
	if st.RegistrationMode != RegistrationClosed {
		t.Fatalf("registration_mode clobbered: got %q want closed", st.RegistrationMode)
	}
	minutes, _ := s.LibraryDiscoveryIntervalMinutes(ctx)
	if minutes != 120 {
		t.Fatalf("persisted interval: got %d want 120", minutes)
	}
}

// TestUpdateLibraryDiscoveryAppDetailsEnabled — same for the boolean field.
func TestUpdateLibraryDiscoveryAppDetailsEnabled(t *testing.T) {
	pool := testDB(t)
	s := NewStore(pool)
	ctx := context.Background()
	if err := s.Seed(ctx, RegistrationClosed); err != nil {
		t.Fatalf("seed: %v", err)
	}
	var adminID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO users (email, username, password_hash, role)
		VALUES ('ad@x.io','adadmin','x','admin') RETURNING id::text`).Scan(&adminID); err != nil {
		t.Fatalf("seed admin: %v", err)
	}

	st, err := s.UpdateLibraryDiscoveryAppDetailsEnabled(ctx, true, adminID)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if !st.LibraryDiscoveryAppDetailsEnabled {
		t.Fatalf("update appdetails: got false want true")
	}
	enabled, _ := s.LibraryDiscoveryAppDetailsEnabled(ctx)
	if !enabled {
		t.Fatalf("persisted appdetails: got false want true")
	}
}

func TestRegistrationModeDefaultsClosedWhenUnseeded(t *testing.T) {
	pool := testDB(t)
	s := NewStore(pool)
	// No Seed: a missing singleton must read as closed, never fail open.
	mode, err := s.RegistrationMode(context.Background())
	if err != nil {
		t.Fatalf("mode: %v", err)
	}
	if mode != RegistrationClosed {
		t.Fatalf("unseeded mode: got %q want closed", mode)
	}
}

// TestMicCaptureEnabledDefaultsFalseWhenUnseeded — the microphone-capture
// amendment's instance gate (spec §3.5) must read false with no seeded row,
// the same fail-closed posture LibraryDiscoveryEnabled already has: a
// capability the operator never explicitly turned on never fails open.
func TestMicCaptureEnabledDefaultsFalseWhenUnseeded(t *testing.T) {
	pool := testDB(t)
	s := NewStore(pool)
	enabled, err := s.MicCaptureEnabled(context.Background())
	if err != nil {
		t.Fatalf("mic_capture_enabled: %v", err)
	}
	if enabled {
		t.Fatalf("unseeded mic_capture_enabled: got true want false")
	}
}

// TestUpdateMicCaptureEnabled — the store-level write + read back, and that
// it does not disturb any neighbouring field.
func TestUpdateMicCaptureEnabled(t *testing.T) {
	pool := testDB(t)
	s := NewStore(pool)
	ctx := context.Background()
	if err := s.Seed(ctx, RegistrationClosed); err != nil {
		t.Fatalf("seed: %v", err)
	}
	var adminID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO users (email, username, password_hash, role)
		VALUES ('mic-ad@x.io','micadmin','x','admin') RETURNING id::text`).Scan(&adminID); err != nil {
		t.Fatalf("seed admin: %v", err)
	}

	st, err := s.UpdateMicCaptureEnabled(ctx, true, adminID)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if !st.MicCaptureEnabled {
		t.Fatalf("update mic_capture_enabled: got false want true")
	}
	if st.RegistrationMode != RegistrationClosed {
		t.Fatalf("an unrelated field changed: registration_mode = %q, want %q", st.RegistrationMode, RegistrationClosed)
	}
	enabled, _ := s.MicCaptureEnabled(ctx)
	if !enabled {
		t.Fatalf("persisted mic_capture_enabled: got false want true")
	}
}

// --- image_update_policy (image-management P3, migration 0054's column) -------

// TestImageUpdatePolicyDefaultsNotify — the DDL default is `notify`, NOT
// `manual`: never silently auto-update an instance nobody configured, but do
// surface the badge. An unseeded singleton must read the same thing a seeded
// one does.
func TestImageUpdatePolicyDefaultsNotify(t *testing.T) {
	pool := testDB(t)
	s := NewStore(pool)
	ctx := context.Background()

	st, err := s.Get(ctx)
	if err != nil {
		t.Fatalf("get unseeded: %v", err)
	}
	if st.ImageUpdatePolicy != ImagePolicyNotify {
		t.Fatalf("unseeded image_update_policy: got %q want notify", st.ImageUpdatePolicy)
	}
	if err := s.Seed(ctx, RegistrationClosed); err != nil {
		t.Fatalf("seed: %v", err)
	}
	st, err = s.Get(ctx)
	if err != nil {
		t.Fatalf("get seeded: %v", err)
	}
	if st.ImageUpdatePolicy != ImagePolicyNotify {
		t.Fatalf("seeded image_update_policy: got %q want notify", st.ImageUpdatePolicy)
	}
}

// TestUpdateImageUpdatePolicy — the write path the admin UI's policy selector
// uses, and the neighbour-field-untouched property every settings update owes.
func TestUpdateImageUpdatePolicy(t *testing.T) {
	pool := testDB(t)
	s := NewStore(pool)
	ctx := context.Background()
	if err := s.Seed(ctx, RegistrationClosed); err != nil {
		t.Fatalf("seed: %v", err)
	}
	var adminID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO users (email, username, password_hash, role)
		VALUES ('p@x.io','padmin','x','admin') RETURNING id::text`).Scan(&adminID); err != nil {
		t.Fatalf("seed admin: %v", err)
	}

	st, err := s.UpdateImageUpdatePolicy(ctx, ImagePolicyAuto, adminID)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if st.ImageUpdatePolicy != ImagePolicyAuto {
		t.Fatalf("update policy: got %q want auto", st.ImageUpdatePolicy)
	}
	if st.RegistrationMode != RegistrationClosed {
		t.Fatalf("registration_mode clobbered: got %q want closed", st.RegistrationMode)
	}
	if st.UpdatedBy == nil || *st.UpdatedBy != adminID {
		t.Fatalf("updated_by not stamped: %v", st.UpdatedBy)
	}
	got, err := s.Get(ctx)
	if err != nil {
		t.Fatalf("re-read: %v", err)
	}
	if got.ImageUpdatePolicy != ImagePolicyAuto {
		t.Fatalf("persisted policy: got %q want auto", got.ImageUpdatePolicy)
	}
}

// TestValidImageUpdatePolicy — the enum the PATCH handler rejects on, mirroring
// the instance_settings CHECK.
func TestValidImageUpdatePolicy(t *testing.T) {
	for _, ok := range []string{ImagePolicyManual, ImagePolicyNotify, ImagePolicyAuto} {
		if !ValidImageUpdatePolicy(ok) {
			t.Errorf("ValidImageUpdatePolicy(%q) = false, want true", ok)
		}
	}
	for _, bad := range []string{"", "Auto", "automatic", "off"} {
		if ValidImageUpdatePolicy(bad) {
			t.Errorf("ValidImageUpdatePolicy(%q) = true, want false", bad)
		}
	}
}
