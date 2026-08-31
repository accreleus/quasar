package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestWriteClientTooOldBody pins the 426 body shape shared by the login gate
// (P9-08) and the bearer gate (#380). control-api.md promises the two are
// byte-identical, so both go through this one writer.
func TestWriteClientTooOldBody(t *testing.T) {
	t.Run("with latest advisory", func(t *testing.T) {
		h := &Handler{minClientVersion: "1.0.0", latestClientVersion: "1.3.0"}
		rr := httptest.NewRecorder()
		h.writeClientTooOld(rr)

		if rr.Code != http.StatusUpgradeRequired {
			t.Fatalf("status = %d, want 426", rr.Code)
		}
		var body map[string]any
		if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		errObj, _ := body["error"].(map[string]any)
		if errObj["code"] != "client_too_old" {
			t.Errorf("error.code = %v, want client_too_old", errObj["code"])
		}
		if body["min_client_version"] != "1.0.0" {
			t.Errorf("min_client_version = %v, want 1.0.0", body["min_client_version"])
		}
		if body["latest_client_version"] != "1.3.0" {
			t.Errorf("latest_client_version = %v, want 1.3.0", body["latest_client_version"])
		}
	})

	t.Run("latest omitted when unconfigured", func(t *testing.T) {
		h := &Handler{minClientVersion: "1.0.0"}
		rr := httptest.NewRecorder()
		h.writeClientTooOld(rr)

		var body map[string]any
		if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if _, ok := body["latest_client_version"]; ok {
			t.Error("latest_client_version present with no advisory configured")
		}
	})
}

// These are pure middleware unit tests (no database): RequireAdmin reads the
// user RequireAuth injected into the request context and gates on role. They
// run even without TEST_DATABASE_URL.

func TestRequireAdminAllowsAdmin(t *testing.T) {
	h := &Handler{}
	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/apps", nil)
	req = req.WithContext(context.WithValue(req.Context(), userCtxKey, User{ID: "u1", Role: RoleAdmin}))
	rr := httptest.NewRecorder()

	h.RequireAdmin(next).ServeHTTP(rr, req)

	if rr.Code != http.StatusOK || !called {
		t.Fatalf("admin token: want 200 with next called, got %d (called=%v)", rr.Code, called)
	}
}

func TestRequireAdminDeniesNonAdmin(t *testing.T) {
	h := &Handler{}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next must not be reached for a non-admin token")
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/apps", nil)
	req = req.WithContext(context.WithValue(req.Context(), userCtxKey, User{ID: "u1", Role: RoleUser}))
	rr := httptest.NewRecorder()

	h.RequireAdmin(next).ServeHTTP(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("non-admin token: want 403, got %d", rr.Code)
	}
}

func TestRequireAdminWithoutUserIs401(t *testing.T) {
	h := &Handler{}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next must not be reached without an authenticated user")
	})

	// No user in context (RequireAuth not run / failed): defensive 401.
	req := httptest.NewRequest(http.MethodPost, "/v1/apps", nil)
	rr := httptest.NewRecorder()

	h.RequireAdmin(next).ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("missing user: want 401, got %d", rr.Code)
	}
}
