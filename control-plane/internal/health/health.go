package health

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

type response struct {
	Status string `json:"status"`
	DB     string `json:"db"`
}

// Handler returns an http.HandlerFunc for GET /health.
// It pings the database with a short timeout; a failed ping returns 503.
func Handler(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()

		dbStatus := "ok"
		httpStatus := http.StatusOK

		if err := pool.Ping(ctx); err != nil {
			dbStatus = "unavailable"
			httpStatus = http.StatusServiceUnavailable
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(httpStatus)
		_ = json.NewEncoder(w).Encode(response{
			Status: map[bool]string{true: "ok", false: "degraded"}[httpStatus == http.StatusOK],
			DB:     dbStatus,
		})
	}
}
