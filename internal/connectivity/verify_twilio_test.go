package connectivity

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/providers"
)

// toServer sends every request to srv, keeping its path, so a probe aimed at
// the fixed Twilio host lands on a stub.
type toServer struct{ srv *httptest.Server }

func (t toServer) RoundTrip(r *http.Request) (*http.Response, error) {
	u, _ := url.Parse(t.srv.URL)
	r = r.Clone(r.Context())
	r.URL.Scheme, r.URL.Host, r.Host = u.Scheme, u.Host, u.Host
	return http.DefaultTransport.RoundTrip(r)
}

type seenRequest struct {
	method, host, path, user, pass string
}

func twilioProbe(t *testing.T, status int) (VerifyResult, []seenRequest) {
	t.Helper()
	var (
		mu   sync.Mutex
		seen []seenRequest
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, _ := r.BasicAuth()
		mu.Lock()
		seen = append(seen, seenRequest{r.Method, r.Host, r.URL.Path, user, pass})
		mu.Unlock()
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"sid":"AC123","status":"active"}`))
	}))
	defer srv.Close()

	svc := New(openTemp(t), testKey())
	svc.probeHTTPClient = &http.Client{Transport: toServer{srv}}
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}
	in, err := svc.CreateTwilio(context.Background(), scope, "twilio-1",
		TwilioConfig{AccountSID: "AC123", AuthToken: "secret-token", From: "+15550001111"}, true)
	if err != nil {
		t.Fatalf("CreateTwilio: %v", err)
	}
	res, err := svc.VerifyByID(context.Background(), scope, in.ID)
	if err != nil {
		t.Fatalf("VerifyByID: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	return res, append([]seenRequest(nil), seen...)
}

// TestVerifyByID_Twilio_Accepted: the probe is one authenticated GET of the
// account resource. It reads; it never posts a message.
func TestVerifyByID_Twilio_Accepted(t *testing.T) {
	res, seen := twilioProbe(t, http.StatusOK)
	if !res.OK || res.Unsupported {
		t.Fatalf("result = %+v, want ok", res)
	}
	if res.ProviderID != "twilio" || res.Channel != "sms" {
		t.Errorf("result = %+v, want twilio/sms", res)
	}
	want := seenRequest{http.MethodGet, "", "/2010-04-01/Accounts/AC123.json", "AC123", "secret-token"}
	if len(seen) != 1 {
		t.Fatalf("requests = %+v, want exactly one", seen)
	}
	got := seen[0]
	got.host = ""
	if got != want {
		t.Errorf("request = %+v, want %+v", got, want)
	}
	if strings.Contains(res.Detail, "secret-token") {
		t.Errorf("detail leaks the token: %q", res.Detail)
	}
}

// TestVerifyByID_Twilio_Rejected: 401 and 403 are a rejected credential, which
// delivery health reads as an auth failure.
func TestVerifyByID_Twilio_Rejected(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		res, _ := twilioProbe(t, status)
		if res.OK || res.Unsupported {
			t.Fatalf("status %d: result = %+v, want a rejection", status, res)
		}
		if c := providers.ClassifyText(res.Detail); c != providers.ClassAuth {
			t.Errorf("status %d: detail %q classifies as %q, want auth", status, res.Detail, c)
		}
	}
}

// TestVerifyByID_Twilio_OtherStatusIsNotARejection: a 5xx says nothing about
// the credential, so it must not classify as auth.
func TestVerifyByID_Twilio_OtherStatusIsNotARejection(t *testing.T) {
	res, _ := twilioProbe(t, http.StatusServiceUnavailable)
	if res.OK {
		t.Fatalf("result = %+v, want not ok", res)
	}
	if c := providers.ClassifyText(res.Detail); c == providers.ClassAuth {
		t.Errorf("detail %q classifies as auth on a 503", res.Detail)
	}
}
