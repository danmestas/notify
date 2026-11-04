package httpapi_test

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

func historyOf(t *testing.T, body string) string {
	t.Helper()
	var a struct {
		History string `json:"history"`
	}
	if err := json.Unmarshal([]byte(body), &a); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	return a.History
}

// TestAgentHistory_DefaultSetPatchAndReject: history defaults to inline, is
// set on create, changed by PATCH (an absent field keeps it), and anything
// but inline or none is 422.
func TestAgentHistory_DefaultSetPatchAndReject(t *testing.T) {
	srv, auth := startAgentServer(t)

	status, body := doPost(t, srv.URL+"/v1/agents", auth,
		`{"identifier":"plain","name":"P","runtime":"mesh","meshSubject":"agents.p"}`)
	if status != http.StatusCreated || historyOf(t, body) != "inline" {
		t.Fatalf("default create: %d %s", status, body)
	}
	status, body = doPost(t, srv.URL+"/v1/agents", auth,
		`{"identifier":"stateful","name":"S","runtime":"mesh","meshSubject":"agents.s","history":"none"}`)
	if status != http.StatusCreated || historyOf(t, body) != "none" {
		t.Fatalf("create with history none: %d %s", status, body)
	}
	if s, _ := doPost(t, srv.URL+"/v1/agents", auth,
		`{"identifier":"bad","name":"B","runtime":"mesh","meshSubject":"agents.b","history":"all"}`); s != http.StatusUnprocessableEntity {
		t.Errorf("create with bad history = %d, want 422", s)
	}

	patch := func(body string) (int, string) {
		t.Helper()
		req, _ := http.NewRequest(http.MethodPatch, srv.URL+"/v1/agents/stateful", strings.NewReader(body))
		req.Header.Set("Authorization", auth)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	if s, b := patch(`{"name":"Renamed"}`); s != http.StatusOK || historyOf(t, b) != "none" {
		t.Fatalf("patch without history must keep it: %d %s", s, b)
	}
	if s, b := patch(`{"history":"inline"}`); s != http.StatusOK || historyOf(t, b) != "inline" {
		t.Fatalf("patch history: %d %s", s, b)
	}
	if s, _ := patch(`{"history":"all"}`); s != http.StatusUnprocessableEntity {
		t.Errorf("patch bad history = %d, want 422", s)
	}
}
