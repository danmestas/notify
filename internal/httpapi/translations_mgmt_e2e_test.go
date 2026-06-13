package httpapi_test

import (
	"encoding/json"
	"net/http"
	"testing"

	_ "modernc.org/sqlite"
)

// TestTranslationsMgmt_ListEmpty: GET /v1/translations with no data → 200 with
// an empty locales array.
func TestTranslationsMgmt_ListEmpty(t *testing.T) {
	srv, authHeader, _, _, _ := startConsoleServer(t)

	status, body := doGet(t, srv.URL+"/v1/translations", authHeader)
	if status != http.StatusOK {
		t.Fatalf("GET list = %d body=%s, want 200", status, body)
	}
	var resp struct {
		Locales []struct {
			Locale   string `json:"locale"`
			KeyCount int    `json:"keyCount"`
		} `json:"locales"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("decode list: %v (body=%s)", err, body)
	}
	if len(resp.Locales) != 0 {
		t.Errorf("empty list = %+v, want []", resp.Locales)
	}
}

// TestTranslationsMgmt_CreateAndGet: POST creates a key, GET /{locale} returns
// the full map, and the list reflects the key count.
func TestTranslationsMgmt_CreateAndGet(t *testing.T) {
	srv, authHeader, _, _, _ := startConsoleServer(t)

	if status, body := doPost(t, srv.URL+"/v1/translations", authHeader,
		`{"locale":"en","key":"greeting","value":"Hello"}`); status != http.StatusCreated {
		t.Fatalf("POST = %d body=%s, want 201", status, body)
	}
	if status, body := doPost(t, srv.URL+"/v1/translations", authHeader,
		`{"locale":"en","key":"bye","value":"Bye"}`); status != http.StatusCreated {
		t.Fatalf("POST second = %d body=%s, want 201", status, body)
	}

	status, body := doGet(t, srv.URL+"/v1/translations/en", authHeader)
	if status != http.StatusOK {
		t.Fatalf("GET en = %d body=%s, want 200", status, body)
	}
	var m map[string]string
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		t.Fatalf("decode map: %v (body=%s)", err, body)
	}
	if m["greeting"] != "Hello" || m["bye"] != "Bye" {
		t.Errorf("map = %v, want greeting=Hello bye=Bye", m)
	}

	status, body = doGet(t, srv.URL+"/v1/translations", authHeader)
	if status != http.StatusOK {
		t.Fatalf("list = %d body=%s", status, body)
	}
	var resp struct {
		Locales []struct {
			Locale   string `json:"locale"`
			KeyCount int    `json:"keyCount"`
		} `json:"locales"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(resp.Locales) != 1 || resp.Locales[0].Locale != "en" || resp.Locales[0].KeyCount != 2 {
		t.Errorf("list = %+v, want [en/2]", resp.Locales)
	}

	// Unknown locale → 404.
	if status, _ := doGet(t, srv.URL+"/v1/translations/zz", authHeader); status != http.StatusNotFound {
		t.Errorf("GET unknown locale = %d, want 404", status)
	}
}

// TestTranslationsMgmt_BulkReplace: PUT /{locale} replaces the entire map.
func TestTranslationsMgmt_BulkReplace(t *testing.T) {
	srv, authHeader, _, _, _ := startConsoleServer(t)

	if status, body := doPost(t, srv.URL+"/v1/translations", authHeader,
		`{"locale":"en","key":"old","value":"A"}`); status != http.StatusCreated {
		t.Fatalf("POST = %d body=%s", status, body)
	}

	status, body := doPut(t, srv.URL+"/v1/translations/en", authHeader,
		`{"new1":"X","new2":"Y"}`)
	if status != http.StatusOK {
		t.Fatalf("PUT bulk = %d body=%s, want 200", status, body)
	}
	var resp struct {
		Locale   string `json:"locale"`
		KeyCount int    `json:"keyCount"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("decode put: %v (body=%s)", err, body)
	}
	if resp.Locale != "en" || resp.KeyCount != 2 {
		t.Errorf("put resp = %+v, want en/2", resp)
	}

	status, body = doGet(t, srv.URL+"/v1/translations/en", authHeader)
	if status != http.StatusOK {
		t.Fatalf("GET after bulk = %d body=%s", status, body)
	}
	var m map[string]string
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		t.Fatalf("decode map: %v", err)
	}
	if len(m) != 2 || m["new1"] != "X" || m["new2"] != "Y" {
		t.Errorf("map after bulk = %v, want only new1/new2", m)
	}
	if _, ok := m["old"]; ok {
		t.Errorf("bulk replace left stale key 'old': %v", m)
	}
}

// TestTranslationsMgmt_DeleteKey: DELETE returns 204; GET shows the key gone;
// a second DELETE returns 404.
func TestTranslationsMgmt_DeleteKey(t *testing.T) {
	srv, authHeader, _, _, _ := startConsoleServer(t)

	if status, body := doPost(t, srv.URL+"/v1/translations", authHeader,
		`{"locale":"en","key":"greeting","value":"Hello"}`); status != http.StatusCreated {
		t.Fatalf("POST = %d body=%s", status, body)
	}

	if status, _ := doDelete(t, srv.URL+"/v1/translations/en/greeting", authHeader); status != http.StatusNoContent {
		t.Fatalf("DELETE = %d, want 204", status)
	}

	// Locale now empty → GET 404.
	if status, _ := doGet(t, srv.URL+"/v1/translations/en", authHeader); status != http.StatusNotFound {
		t.Errorf("GET after delete = %d, want 404", status)
	}

	// Second DELETE → 404.
	if status, _ := doDelete(t, srv.URL+"/v1/translations/en/greeting", authHeader); status != http.StatusNotFound {
		t.Errorf("second DELETE = %d, want 404", status)
	}
}
