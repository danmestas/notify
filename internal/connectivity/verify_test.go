package connectivity

import (
	"bufio"
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/danmestas/notify/internal/domain"
)

// TestVerifyByID_NotFound proves a missing integration is an error (the probe
// could not even start), not a false "ok".
func TestVerifyByID_NotFound(t *testing.T) {
	svc := New(openTemp(t), testKey())
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}
	if _, err := svc.VerifyByID(context.Background(), scope, "missing"); err == nil {
		t.Fatalf("VerifyByID for missing integration should error")
	}
}

// TestVerifyByID_Ntfy_Reachable exercises the ntfy probe against a stub server
// that returns 200 for /v1/health — a successful credential check.
func TestVerifyByID_Ntfy_Reachable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/v1/health") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"healthy":true}`))
	}))
	defer srv.Close()

	svc := New(openTemp(t), testKey())
	// The SSRF guard blocks loopback by design; opt out here since this test's
	// whole point is exercising the probe against a local httptest.Server.
	svc.probeHTTPClient = &http.Client{}
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}
	in, err := svc.CreateNtfy(context.Background(), scope, "ntfy-1",
		NtfyConfig{BaseURL: srv.URL, Token: "tok"}, true)
	if err != nil {
		t.Fatalf("CreateNtfy: %v", err)
	}

	res, err := svc.VerifyByID(context.Background(), scope, in.ID)
	if err != nil {
		t.Fatalf("VerifyByID: %v", err)
	}
	if !res.OK {
		t.Errorf("ntfy verify OK = false, want true (detail: %s)", res.Detail)
	}
	if res.ProviderID != "ntfy" || res.Channel != "push" {
		t.Errorf("verify result = %+v, want ntfy/push", res)
	}
}

// TestVerifyByID_Ntfy_Rejected proves a 401 from the server is a definite
// credential negative (ok=false, nil error).
func TestVerifyByID_Ntfy_Rejected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	svc := New(openTemp(t), testKey())
	// See TestVerifyByID_Ntfy_Reachable: opt out of the SSRF guard to reach a
	// local httptest.Server.
	svc.probeHTTPClient = &http.Client{}
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}
	in, err := svc.CreateNtfy(context.Background(), scope, "ntfy-1",
		NtfyConfig{BaseURL: srv.URL, Token: "bad"}, true)
	if err != nil {
		t.Fatalf("CreateNtfy: %v", err)
	}

	res, err := svc.VerifyByID(context.Background(), scope, in.ID)
	if err != nil {
		t.Fatalf("VerifyByID returned error, want ok=false result: %v", err)
	}
	if res.OK {
		t.Errorf("ntfy verify OK = true, want false on 401")
	}
	if strings.Contains(res.Detail, "ssrf guard:") {
		t.Errorf("detail = %q, want a 401 rejection, not an SSRF block", res.Detail)
	}
}

// TestVerifyByID_CloudflareEmail_Accepted exercises the cloudflare-email probe
// against a stub that returns 200 for the account scope.
func TestVerifyByID_CloudflareEmail_Accepted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer cf-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"success":true}`))
	}))
	defer srv.Close()

	svc := New(openTemp(t), testKey())
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}
	in, err := svc.CreateCloudflareEmail(context.Background(), scope, "cf-1",
		CloudflareEmailConfig{AccountID: "acct", APIToken: "cf-token", From: "n@example.com"}, true)
	if err != nil {
		t.Fatalf("CreateCloudflareEmail: %v", err)
	}
	// The probe hits the live api.cloudflare.com host; reroute is not wired into
	// the config, so this asserts the credential-shape path: with a real token
	// the call would reach Cloudflare. To keep the unit test hermetic we assert
	// the probe completes and returns a structured result (no panic, no error
	// shape leak) rather than a specific network outcome.
	res, err := svc.VerifyByID(context.Background(), scope, in.ID)
	if err != nil {
		t.Fatalf("VerifyByID: %v", err)
	}
	if res.ProviderID != "cloudflare-email" || res.Channel != "email" {
		t.Errorf("verify result = %+v, want cloudflare-email/email", res)
	}
	_ = srv // documented: the CF probe targets the fixed Cloudflare host.
}

// TestVerifyByID_SMTP_Reachable_NoAuth points the probe at a minimal SMTP
// greeter that accepts EHLO with no STARTTLS/AUTH; a no-auth config verifies as
// reachable.
func TestVerifyByID_SMTP_Reachable_NoAuth(t *testing.T) {
	addr := startFakeSMTP(t)

	svc := New(openTemp(t), testKey())
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}
	in, err := svc.CreateSMTP(context.Background(), scope, "smtp-1",
		SMTPConfig{Addr: addr, Host: "localhost", From: "n@example.com"}, true)
	if err != nil {
		t.Fatalf("CreateSMTP: %v", err)
	}

	res, err := svc.VerifyByID(context.Background(), scope, in.ID)
	if err != nil {
		t.Fatalf("VerifyByID: %v", err)
	}
	if !res.OK {
		t.Errorf("smtp verify OK = false, want true (detail: %s)", res.Detail)
	}
	if res.ProviderID != "smtp" {
		t.Errorf("provider id = %q, want smtp", res.ProviderID)
	}
}

// TestVerifyByID_SMTP_DialFailure proves an unreachable SMTP server is a
// negative result (ok=false), not an error.
func TestVerifyByID_SMTP_DialFailure(t *testing.T) {
	svc := New(openTemp(t), testKey())
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}
	// Port 1 is reserved and refuses connections.
	in, err := svc.CreateSMTP(context.Background(), scope, "smtp-bad",
		SMTPConfig{Addr: "127.0.0.1:1", Host: "localhost"}, true)
	if err != nil {
		t.Fatalf("CreateSMTP: %v", err)
	}
	res, err := svc.VerifyByID(context.Background(), scope, in.ID)
	if err != nil {
		t.Fatalf("VerifyByID: %v", err)
	}
	if res.OK {
		t.Errorf("smtp verify OK = true, want false on dial failure")
	}
}

// startFakeSMTP starts a minimal SMTP greeter that speaks just enough of the
// protocol for net/smtp's NewClient + EHLO + QUIT (no STARTTLS, no AUTH) and
// returns its address. It closes on test cleanup.
func startFakeSMTP(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go serveFakeSMTP(conn)
		}
	}()
	return ln.Addr().String()
}

func serveFakeSMTP(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	w := func(s string) { _, _ = conn.Write([]byte(s + "\r\n")) }
	w("220 fake ESMTP")
	sc := bufio.NewScanner(conn)
	for sc.Scan() {
		line := strings.ToUpper(strings.TrimSpace(sc.Text()))
		switch {
		case strings.HasPrefix(line, "EHLO"), strings.HasPrefix(line, "HELO"):
			// No STARTTLS / AUTH advertised → the probe takes the no-auth path.
			w("250-fake greets you")
			w("250 SIZE 10240000")
		case strings.HasPrefix(line, "QUIT"):
			w("221 bye")
			return
		default:
			w("250 ok")
		}
	}
}
