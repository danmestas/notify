package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestProbeBridgeAuth_StatusHandling pins the contract that keeps #325 from
// recurring: notify must refuse to boot against a bridge that will not hand it
// tasks, and must distinguish "wrong credentials" from "wrong URL entirely".
func TestProbeBridgeAuth_StatusHandling(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		token   string
		wantErr bool
		wantIn  []string
	}{
		{name: "400 is the expected pass", status: http.StatusBadRequest},
		{name: "200 also passes", status: http.StatusOK},
		{
			name: "401 is fatal and names the token", status: http.StatusUnauthorized, token: "tok",
			wantErr: true, wantIn: []string{"DAGNATS_BRIDGE_TOKEN", "401"},
		},
		{
			name: "401 with no token says so", status: http.StatusUnauthorized,
			wantErr: true, wantIn: []string{"no DAGNATS_BRIDGE_TOKEN set"},
		},
		{
			name: "403 is fatal", status: http.StatusForbidden, token: "tok",
			wantErr: true, wantIn: []string{"DAGNATS_BRIDGE_TOKEN"},
		},
		{
			name: "404 means not a bridge", status: http.StatusNotFound,
			wantErr: true, wantIn: []string{"does not look like a dagnats worker bridge", "bridge-url"},
		},
		{
			name: "405 means not a bridge", status: http.StatusMethodNotAllowed,
			wantErr: true, wantIn: []string{"does not look like a dagnats worker bridge"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
			}))
			defer srv.Close()

			err := probeBridgeAuthN(context.Background(), srv.Client(), srv.URL, tc.token, 2, time.Millisecond)
			if tc.wantErr && err == nil {
				t.Fatalf("probe passed on HTTP %d, want a fatal error", tc.status)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("probe failed on HTTP %d: %v", tc.status, err)
			}
			for _, want := range tc.wantIn {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not mention %q", err, want)
				}
			}
			if tc.token != "" && err != nil && strings.Contains(err.Error(), tc.token) {
				t.Errorf("probe leaked the bridge token into its error: %v", err)
			}
		})
	}
}

// TestProbeBridgeAuth_SendsBearer asserts the probe authenticates the same way
// the poll loop does -- a probe that skipped the header would pass on a bridge
// the engine cannot actually use.
func TestProbeBridgeAuth_SendsBearer(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer srv.Close()

	if err := probeBridgeAuthN(context.Background(), srv.Client(), srv.URL, "tok", 1, time.Millisecond); err != nil {
		t.Fatalf("probe: %v", err)
	}
	if got != "Bearer tok" {
		t.Errorf("Authorization = %q, want %q", got, "Bearer tok")
	}
}

// TestProbeBridgeAuth_RetriesServerError covers a bridge still finishing its
// own startup: 5xx is retried, and a bridge that comes good within the bound
// passes.
func TestProbeBridgeAuth_RetriesServerError(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) < 3 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer srv.Close()

	if err := probeBridgeAuthN(context.Background(), srv.Client(), srv.URL, "", 3, time.Millisecond); err != nil {
		t.Fatalf("probe did not recover from transient 5xx: %v", err)
	}
	if calls.Load() != 3 {
		t.Errorf("calls = %d, want 3", calls.Load())
	}
}

// TestProbeBridgeAuth_ExhaustsRetries asserts a permanently-5xx bridge is
// eventually fatal rather than booting past.
func TestProbeBridgeAuth_ExhaustsRetries(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	err := probeBridgeAuthN(context.Background(), srv.Client(), srv.URL, "", 2, time.Millisecond)
	if err == nil {
		t.Fatal("probe passed against a permanently failing bridge")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("error does not carry the status: %v", err)
	}
}

// TestProbeBridgeAuth_InvalidBodyIsTheProbe documents the probe's mechanism:
// the request must be one dagnats rejects at parse time, so it can never
// consume a real task.
func TestProbeBridgeAuth_InvalidBodyIsTheProbe(t *testing.T) {
	var body, path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		buf := make([]byte, 64)
		n, _ := r.Body.Read(buf)
		body = string(buf[:n])
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer srv.Close()

	if err := probeBridgeAuthN(context.Background(), srv.Client(), srv.URL, "", 1, time.Millisecond); err != nil {
		t.Fatalf("probe: %v", err)
	}
	if path != "/v1/tasks/poll" {
		t.Errorf("path = %q, want /v1/tasks/poll", path)
	}
	if !strings.Contains(body, `"task_types":[]`) {
		t.Errorf("body = %q, want an empty task_types that dagnats rejects before fetching work", body)
	}
}
