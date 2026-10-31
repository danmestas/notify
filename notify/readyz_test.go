package notify

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/danmestas/notify/internal/dagtest"
	"github.com/danmestas/notify/internal/engine"
)

// TestReadyzHandler pins /readyz's observable states (notify hardening #6):
// the engine's dagnats connection up and the writer DB up -> 200 "ok"; the
// engine's connection closed -> 503; the writer DB closed -> 503. Every check
// runs live per request against real stand-ins (an in-process dagnats, a real
// sqlite *sql.DB), not mocks.
func TestReadyzHandler(t *testing.T) {
	natsURL := dagtest.StartSidecar(t)
	connect := func(t *testing.T) *engine.Engine {
		t.Helper()
		e, err := engine.New(natsURL)
		if err != nil {
			t.Fatalf("engine.New: %v", err)
		}
		return e
	}

	t.Run("dagnats ok and db ok: 200", func(t *testing.T) {
		e := connect(t)
		defer func() { _ = e.Stop() }()
		db := openWorkingDB(t)
		defer func() { _ = db.Close() }()

		rec, resp := doReadyz(t, e, db)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
		}
		if resp.Status != "ok" || !resp.Dagnats || !resp.DB {
			t.Errorf("resp = %+v, want status=ok dagnats=true db=true", resp)
		}
		if _, ok := resp.raw["bridge"]; ok {
			t.Errorf("body still reports a bridge field: %s", rec.Body.String())
		}
	})

	t.Run("dagnats connection closed: 503", func(t *testing.T) {
		e := connect(t)
		_ = e.Stop()
		db := openWorkingDB(t)
		defer func() { _ = db.Close() }()

		rec, resp := doReadyz(t, e, db)
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, want 503 (body=%s)", rec.Code, rec.Body.String())
		}
		if resp.Dagnats {
			t.Errorf("resp.Dagnats = true, want false (connection is closed)")
		}
	})

	t.Run("db closed: 503", func(t *testing.T) {
		e := connect(t)
		defer func() { _ = e.Stop() }()
		db := openWorkingDB(t)
		_ = db.Close() // closed before the request

		rec, resp := doReadyz(t, e, db)
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, want 503 (body=%s)", rec.Code, rec.Body.String())
		}
		if resp.DB {
			t.Errorf("resp.DB = true, want false (db is closed)")
		}
	})
}

type decodedReadyz struct {
	readyzResponse
	raw map[string]json.RawMessage
}

func doReadyz(t *testing.T, e *engine.Engine, db *sql.DB) (*httptest.ResponseRecorder, decodedReadyz) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rec := httptest.NewRecorder()
	readyzHandler(e.Ready, db)(rec, req)
	var out decodedReadyz
	if err := json.Unmarshal(rec.Body.Bytes(), &out.readyzResponse); err != nil {
		t.Fatalf("decode body %s: %v", rec.Body.String(), err)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out.raw); err != nil {
		t.Fatalf("decode body %s: %v", rec.Body.String(), err)
	}
	return rec, out
}

func openWorkingDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.PingContext(context.Background()); err != nil {
		t.Fatalf("ping sqlite: %v", err)
	}
	return db
}
