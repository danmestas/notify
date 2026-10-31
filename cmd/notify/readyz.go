package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"time"
)

// readyzTimeout bounds the DB ping behind every /readyz request.
const readyzTimeout = 3 * time.Second

// readyzResponse is the GET /readyz JSON body. Dagnats reports the engine's
// NATS connection to dagnats, over which notify claims every task.
type readyzResponse struct {
	Status  string `json:"status"`
	Dagnats bool   `json:"dagnats"`
	DB      bool   `json:"db"`
}

// readyzHandler reports 200 once engineReady passes and writer answers a ping;
// until both pass it reports 503. Both checks run live on every request (not
// cached at boot), so the endpoint reflects the current state rather than a
// stale boot-time snapshot.
func readyzHandler(engineReady func() error, writer *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), readyzTimeout)
		defer cancel()

		dagnatsOK := engineReady() == nil
		dbOK := writer.PingContext(ctx) == nil
		resp := readyzResponse{Status: "not ready", Dagnats: dagnatsOK, DB: dbOK}

		w.Header().Set("Content-Type", "application/json")
		if dagnatsOK && dbOK {
			resp.Status = "ok"
			w.WriteHeader(http.StatusOK)
		} else {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
		_ = json.NewEncoder(w).Encode(resp)
	}
}
