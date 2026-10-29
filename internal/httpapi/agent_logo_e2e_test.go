package httpapi_test

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

func logoOf(t *testing.T, body string) string {
	t.Helper()
	var a struct {
		Logo *string `json:"logo"`
	}
	if err := json.Unmarshal([]byte(body), &a); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	if a.Logo == nil {
		t.Fatalf("agent has no logo field: %s", body)
	}
	return *a.Logo
}

// TestAgentLogo_SetPatchClearAndList: a logo is set on create, replaced and
// cleared by PATCH, absent PATCH keeps it, and every read returns it.
func TestAgentLogo_SetPatchClearAndList(t *testing.T) {
	srv, auth := startAgentServer(t)
	logo := "data:image/png;base64," + base64.StdEncoding.EncodeToString([]byte("\x89PNG\r\n\x1a\nabc"))
	other := "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString([]byte{0xff, 0xd8, 0xff, 0xe0})

	status, body := doPost(t, srv.URL+"/v1/agents", auth,
		`{"identifier":"logo-bot","name":"L","runtime":"mesh","meshSubject":"agents.l","logo":"`+logo+`"}`)
	if status != http.StatusCreated || logoOf(t, body) != logo {
		t.Fatalf("create: %d %s", status, body)
	}
	if status, body = doPost(t, srv.URL+"/v1/agents", auth, `{"identifier":"plain","name":"P","runtime":"mesh","meshSubject":"agents.p"}`); status != http.StatusCreated || logoOf(t, body) != "" {
		t.Fatalf("create without logo: %d %s", status, body)
	}

	patch := func(body string) (int, string) {
		t.Helper()
		req, _ := http.NewRequest(http.MethodPatch, srv.URL+"/v1/agents/logo-bot", strings.NewReader(body))
		req.Header.Set("Authorization", auth)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	if s, b := patch(`{"name":"Renamed"}`); s != http.StatusOK || logoOf(t, b) != logo {
		t.Fatalf("patch without logo must keep it: %d %s", s, b)
	}
	if s, b := patch(`{"logo":"` + other + `"}`); s != http.StatusOK || logoOf(t, b) != other {
		t.Fatalf("patch logo: %d %s", s, b)
	}
	for _, bad := range []string{"data:image/svg+xml;base64,PHN2Zy8+", "http://x/y.png", "data:image/png;base64,@@"} {
		if s, _ := patch(`{"logo":"` + bad + `"}`); s != http.StatusUnprocessableEntity {
			t.Errorf("logo %q: status %d, want 422", bad, s)
		}
	}
	if s, b := patch(`{"logo":""}`); s != http.StatusOK || logoOf(t, b) != "" {
		t.Fatalf("clearing logo: %d %s", s, b)
	}
	if s, b := patch(`{"logo":"` + logo + `"}`); s != http.StatusOK || logoOf(t, b) != logo {
		t.Fatalf("re-set logo: %d %s", s, b)
	}

	status, body = doGet(t, srv.URL+"/v1/agents", auth)
	var list []struct {
		Identifier string `json:"identifier"`
		Logo       string `json:"logo"`
	}
	if err := json.Unmarshal([]byte(body), &list); err != nil || status != http.StatusOK {
		t.Fatalf("list: %d %s %v", status, body, err)
	}
	got := map[string]string{}
	for _, a := range list {
		got[a.Identifier] = a.Logo
	}
	if got["logo-bot"] != logo || got["plain"] != "" {
		t.Fatalf("list logos = %v", got)
	}
	if status, body = doGet(t, srv.URL+"/v1/agents/logo-bot", auth); status != http.StatusOK || logoOf(t, body) != logo {
		t.Fatalf("get: %d %s", status, body)
	}

	if s, _ := doPost(t, srv.URL+"/v1/agents", auth,
		`{"identifier":"bad","name":"B","runtime":"mesh","meshSubject":"agents.b","logo":"data:image/svg+xml;base64,PHN2Zy8+"}`); s != http.StatusUnprocessableEntity {
		t.Errorf("create with svg logo: status %d, want 422", s)
	}
}
