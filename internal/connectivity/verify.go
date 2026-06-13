package connectivity

// verify.go holds the per-provider credential probes used by VerifyByID
// (W4 ADR-013 credential-verify durable action). Each probe attempts a real
// authenticated handshake against the configured provider WITHOUT delivering a
// message to a subscriber, and reports (ok, detail). They never panic and never
// leak a secret into the detail string.

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/smtp"
	"net/url"
	"strings"
	"time"
)

// probeTimeout bounds every credential probe so a wedged provider cannot stall
// the engine worker indefinitely.
const probeTimeout = 10 * time.Second

// probeSMTP dials the configured SMTP server, runs STARTTLS when offered, and
// authenticates with PLAIN. It never sends MAIL/DATA, so no message is
// delivered. A successful AUTH proves the credentials; an auth rejection is a
// definite negative (ok=false, nil-shaped detail).
func probeSMTP(cfg SMTPConfig) (ok bool, detail string) {
	if strings.TrimSpace(cfg.Addr) == "" {
		return false, "smtp: no server address configured"
	}
	host := cfg.Host
	if host == "" {
		if h, _, err := net.SplitHostPort(cfg.Addr); err == nil {
			host = h
		}
	}

	conn, err := net.DialTimeout("tcp", cfg.Addr, probeTimeout)
	if err != nil {
		return false, fmt.Sprintf("smtp: dial %s failed: %v", cfg.Addr, err)
	}
	_ = conn.SetDeadline(time.Now().Add(probeTimeout))

	client, err := smtp.NewClient(conn, host)
	if err != nil {
		_ = conn.Close()
		return false, fmt.Sprintf("smtp: handshake with %s failed: %v", cfg.Addr, err)
	}
	defer func() { _ = client.Close() }()

	if ok, _ := client.Extension("STARTTLS"); ok {
		if err := client.StartTLS(&tls.Config{ServerName: host, MinVersion: tls.VersionTLS12}); err != nil {
			return false, fmt.Sprintf("smtp: STARTTLS with %s failed: %v", host, err)
		}
	}

	if cfg.Username != "" || cfg.Password != "" {
		auth := smtp.PlainAuth("", cfg.Username, cfg.Password, host)
		if err := client.Auth(auth); err != nil {
			return false, fmt.Sprintf("smtp: authentication rejected by %s: %v", host, err)
		}
		return true, fmt.Sprintf("smtp: authenticated with %s", host)
	}
	return true, fmt.Sprintf("smtp: connected to %s (no auth configured)", host)
}

// probeCloudflareEmail issues an authenticated GET against the account scope of
// the Cloudflare API. A 2xx proves the token and account id are valid; a 401/403
// is a definite credential negative; other statuses are reported verbatim.
func probeCloudflareEmail(ctx context.Context, cfg CloudflareEmailConfig) (ok bool, detail string) {
	if strings.TrimSpace(cfg.AccountID) == "" || strings.TrimSpace(cfg.APIToken) == "" {
		return false, "cloudflare-email: account id or api token not configured"
	}
	endpoint := "https://api.cloudflare.com/client/v4/accounts/" + url.PathEscape(cfg.AccountID)
	status, body, err := authedGET(ctx, endpoint, "Bearer "+cfg.APIToken)
	if err != nil {
		return false, fmt.Sprintf("cloudflare-email: probe %s failed: %v", endpoint, err)
	}
	switch {
	case status >= 200 && status < 300:
		return true, "cloudflare-email: token accepted for account"
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return false, fmt.Sprintf("cloudflare-email: credentials rejected (status %d)", status)
	default:
		return false, fmt.Sprintf("cloudflare-email: unexpected status %d: %s", status, body)
	}
}

// probeNtfy issues an authenticated GET against the ntfy server root. ntfy
// returns 200 for the docs/health root; a 401/403 indicates the token is
// invalid for an auth-protected server. A transport failure is reported as a
// negative with the cause.
func probeNtfy(ctx context.Context, cfg NtfyConfig) (ok bool, detail string) {
	base := strings.TrimSpace(cfg.BaseURL)
	if base == "" {
		return false, "ntfy: no server base URL configured"
	}
	endpoint := strings.TrimRight(base, "/") + "/v1/health"
	var auth string
	if cfg.Token != "" {
		auth = "Bearer " + cfg.Token
	}
	status, body, err := authedGET(ctx, endpoint, auth)
	if err != nil {
		return false, fmt.Sprintf("ntfy: probe %s failed: %v", endpoint, err)
	}
	switch {
	case status >= 200 && status < 300:
		return true, fmt.Sprintf("ntfy: server %s reachable", base)
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return false, fmt.Sprintf("ntfy: credentials rejected (status %d)", status)
	default:
		return false, fmt.Sprintf("ntfy: unexpected status %d: %s", status, body)
	}
}

// authedGET performs a bounded GET with an optional Authorization header and
// returns the status code and a short truncated body. The probe never follows
// into the response beyond 256 bytes.
func authedGET(ctx context.Context, endpoint, authHeader string) (status int, body string, err error) {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return 0, "", err
	}
	if authHeader != "" {
		req.Header.Set("Authorization", authHeader)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
	return resp.StatusCode, strings.TrimSpace(string(b)), nil
}
