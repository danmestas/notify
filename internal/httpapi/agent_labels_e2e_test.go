package httpapi_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func agentIdentifiers(t *testing.T, body string) []string {
	t.Helper()
	var list []struct {
		Identifier string            `json:"identifier"`
		Labels     map[string]string `json:"labels"`
	}
	if err := json.Unmarshal([]byte(body), &list); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	out := []string{}
	for _, a := range list {
		if a.Labels == nil {
			t.Fatalf("agent %s has no labels object", a.Identifier)
		}
		out = append(out, a.Identifier)
	}
	return out
}

// TestAgentLabels_TagAndFilter: agents carry labels, the list filters on all
// requested labels, a patch replaces them, and malformed labels are refused.
func TestAgentLabels_TagAndFilter(t *testing.T) {
	srv, auth := startAgentServer(t)
	create := func(id, labels string) {
		t.Helper()
		body := `{"identifier":"` + id + `","name":"` + id + `","runtime":"mesh","meshSubject":"agents.` + id + `"` + labels + `}`
		if status, rb := doPost(t, srv.URL+"/v1/agents", auth, body); status != http.StatusCreated {
			t.Fatalf("create %s: %d %s", id, status, rb)
		}
	}
	create("owner-bot", `,"labels":{"owner":"dan","tier":"pro"}`)
	create("lou-bot", `,"labels":{"owner":"lou"}`)
	create("plain-bot", ``)

	list := func(q string) []string {
		t.Helper()
		status, body := doGet(t, srv.URL+"/v1/agents"+q, auth)
		if status != http.StatusOK {
			t.Fatalf("list %q: %d %s", q, status, body)
		}
		return agentIdentifiers(t, body)
	}
	if got := list(""); !equal(got, []string{"lou-bot", "owner-bot", "plain-bot"}) {
		t.Fatalf("all = %v", got)
	}
	if got := list("?label=owner:lou"); !equal(got, []string{"lou-bot"}) {
		t.Fatalf("owner:lou = %v", got)
	}
	if got := list("?label=owner:dan&label=tier:pro"); !equal(got, []string{"owner-bot"}) {
		t.Fatalf("owner:dan+tier:pro = %v", got)
	}
	if got := list("?label=owner:dan&label=tier:free"); len(got) != 0 {
		t.Fatalf("no match = %v", got)
	}

	// A patch replaces the labels; {} clears them.
	patch := func(id, body string) int {
		req, _ := http.NewRequest(http.MethodPatch, srv.URL+"/v1/agents/"+id, strings.NewReader(body))
		req.Header.Set("Authorization", auth)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		return resp.StatusCode
	}
	if s := patch("lou-bot", `{"labels":{"owner":"dan"}}`); s != http.StatusOK {
		t.Fatalf("patch: %d", s)
	}
	if got := list("?label=owner:dan"); !equal(got, []string{"lou-bot", "owner-bot"}) {
		t.Fatalf("after patch = %v", got)
	}
	if s := patch("lou-bot", `{"name":"Lou"}`); s != http.StatusOK || !equal(list("?label=owner:dan"), []string{"lou-bot", "owner-bot"}) {
		t.Fatalf("a patch without labels must keep them (status %d)", s)
	}
	if s := patch("lou-bot", `{"labels":{}}`); s != http.StatusOK || !equal(list("?label=owner:dan"), []string{"owner-bot"}) {
		t.Fatalf("clearing labels (status %d)", s)
	}

	for _, bad := range []string{`{"Owner":"x"}`, `{"a:b":"x"}`, `{"owner":"line\nbreak"}`} {
		if s := patch("plain-bot", `{"labels":`+bad+`}`); s != http.StatusUnprocessableEntity {
			t.Errorf("labels %s: status %d, want 422", bad, s)
		}
	}
	if status, _ := doGet(t, srv.URL+"/v1/agents?label=nocolon", auth); status != http.StatusBadRequest {
		t.Errorf("malformed filter: status %d, want 400", status)
	}
}
