package httpapi_test

import (
	"net/http"
	"testing"

	"github.com/danmestas/notify/internal/httpapi"
)

// TestContractHeader_OnEveryV1Response: every /v1 response carries the
// contract version, whatever group serves it and whether or not it succeeds,
// so a client can check the contract before reading the body. Routes outside
// /v1 do not carry it.
func TestContractHeader_OnEveryV1Response(t *testing.T) {
	h := newHTTPActionHarness(t)
	cases := []struct {
		name, path, auth string
		want             string
	}{
		{"protected route", "/v1/workflows", h.authHeader, httpapi.ContractVersion},
		{"no API key", "/v1/workflows", "", httpapi.ContractVersion},
		{"subscriber inbox", "/v1/messages?subscriberId=x", "", httpapi.ContractVersion},
		{"unknown v1 path", "/v1/no-such-route", h.authHeader, httpapi.ContractVersion},
		{"health probe", "/healthz", "", ""},
	}
	for _, c := range cases {
		req, err := http.NewRequest(http.MethodGet, h.srv.URL+c.path, nil)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if c.auth != "" {
			req.Header.Set("Authorization", c.auth)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		_ = resp.Body.Close()
		if got := resp.Header.Get(httpapi.ContractHeader); got != c.want {
			t.Errorf("%s (%d): %s = %q, want %q", c.name, resp.StatusCode, httpapi.ContractHeader, got, c.want)
		}
	}
}
