package connectivity

// verify.go holds the per-provider credential probes used by VerifyByID
// (W4 ADR-013 credential-verify durable action). Each probe attempts a real
// authenticated handshake against the configured provider WITHOUT delivering a
// message to a subscriber, and reports (ok, detail). They never panic and never
// leak a secret into the detail string.

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/smtp"
	"net/url"
	"os/exec"
	"strings"
	"time"

	"github.com/danmestas/notify/internal/delivery/httpaction"
)

// probeTimeout bounds every credential probe so a wedged provider cannot stall
// the engine worker indefinitely.
const probeTimeout = 10 * time.Second

// probeClient returns the HTTP client used by every credential probe that
// makes an outbound HTTP call (ntfy's BaseURL is tenant-supplied; the
// cloudflare-email probe target is fixed but goes through the same helper for
// a single code path). Production always gets the SSRF-safe client, whose
// Timeout mirrors probeTimeout so a probe that never completes a TCP
// handshake still bounds the same way it did with http.DefaultClient; the
// per-call context.WithTimeout in authedGET is belt-and-suspenders on top of
// that. See Service.probeHTTPClient for the test-only override.
func (svc *Service) probeClient() *http.Client {
	if svc.probeHTTPClient != nil {
		return svc.probeHTTPClient
	}
	return httpaction.NewSSRFSafeClientWithTimeout(probeTimeout)
}

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
func probeCloudflareEmail(ctx context.Context, client *http.Client, cfg CloudflareEmailConfig) (ok bool, detail string) {
	if strings.TrimSpace(cfg.AccountID) == "" || strings.TrimSpace(cfg.APIToken) == "" {
		return false, "cloudflare-email: account id or api token not configured"
	}
	endpoint := "https://api.cloudflare.com/client/v4/accounts/" + url.PathEscape(cfg.AccountID)
	status, body, err := authedGET(ctx, client, endpoint, "Bearer "+cfg.APIToken)
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
func probeNtfy(ctx context.Context, client *http.Client, cfg NtfyConfig) (ok bool, detail string) {
	base := strings.TrimSpace(cfg.BaseURL)
	if base == "" {
		return false, "ntfy: no server base URL configured"
	}
	endpoint := strings.TrimRight(base, "/") + "/v1/health"
	var auth string
	if cfg.Token != "" {
		auth = "Bearer " + cfg.Token
	}
	status, body, err := authedGET(ctx, client, endpoint, auth)
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

// probeMastodon issues an authenticated GET against the Mastodon
// verify_credentials endpoint. A 2xx proves the access token is valid; a
// 401/403 is a definite credential negative.
func probeMastodon(ctx context.Context, client *http.Client, cfg MastodonConfig) (ok bool, detail string) {
	base := strings.TrimSpace(cfg.BaseURL)
	if base == "" || strings.TrimSpace(cfg.AccessToken) == "" {
		return false, "mastodon: base url or access token not configured"
	}
	endpoint := strings.TrimRight(base, "/") + "/api/v1/accounts/verify_credentials"
	status, body, err := authedGET(ctx, client, endpoint, "Bearer "+cfg.AccessToken)
	if err != nil {
		return false, fmt.Sprintf("mastodon: probe %s failed: %v", endpoint, err)
	}
	switch {
	case status >= 200 && status < 300:
		return true, "mastodon: access token accepted"
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return false, fmt.Sprintf("mastodon: credentials rejected (status %d)", status)
	default:
		return false, fmt.Sprintf("mastodon: unexpected status %d: %s", status, body)
	}
}

// probeBluesky attempts com.atproto.server.createSession against the
// configured PDS. A successful session mint proves the identifier/app
// password pair; a rejection is a definite credential negative. No message is
// posted and the minted session is discarded after the probe.
func probeBluesky(ctx context.Context, client *http.Client, cfg BlueskyConfig) (ok bool, detail string) {
	identifier := strings.TrimSpace(cfg.Identifier)
	if identifier == "" || strings.TrimSpace(cfg.AppPassword) == "" {
		return false, "bluesky: identifier or app password not configured"
	}
	pds := strings.TrimSpace(cfg.PDSURL)
	if pds == "" {
		pds = "https://bsky.social"
	}
	endpoint := strings.TrimRight(pds, "/") + "/xrpc/com.atproto.server.createSession"

	reqBody, err := json.Marshal(map[string]string{
		"identifier": identifier,
		"password":   cfg.AppPassword,
	})
	if err != nil {
		return false, fmt.Sprintf("bluesky: build createSession request: %v", err)
	}

	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(reqBody))
	if err != nil {
		return false, fmt.Sprintf("bluesky: build createSession request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return false, fmt.Sprintf("bluesky: probe %s failed: %v", endpoint, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 256))

	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return true, "bluesky: session created"
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return false, fmt.Sprintf("bluesky: credentials rejected (status %d)", resp.StatusCode)
	default:
		return false, fmt.Sprintf("bluesky: unexpected status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
}

// probeWebPush validates that the org's auto-minted VAPID keypair (see
// EnsureWebPushKeys) is well-formed: both keys base64url-decode, both are
// valid P-256 points/scalars, and the private key actually derives the
// stored public key. Unlike every other probe this makes no network call —
// there is no third-party API to hit; a browser PushSubscription endpoint is
// the closest thing to a "server" and per-device, not per-org, so there is
// nothing meaningful to dial here.
func probeWebPush(cfg WebPushConfig) (ok bool, detail string) {
	if strings.TrimSpace(cfg.PublicKey) == "" || strings.TrimSpace(cfg.PrivateKey) == "" {
		return false, "webpush: vapid keys are missing"
	}
	pubRaw, err := base64.RawURLEncoding.DecodeString(cfg.PublicKey)
	if err != nil {
		return false, fmt.Sprintf("webpush: public key is not valid base64url: %v", err)
	}
	privRaw, err := base64.RawURLEncoding.DecodeString(cfg.PrivateKey)
	if err != nil {
		return false, fmt.Sprintf("webpush: private key is not valid base64url: %v", err)
	}

	curve := ecdh.P256()
	pub, err := curve.NewPublicKey(pubRaw)
	if err != nil {
		return false, fmt.Sprintf("webpush: public key is not a valid P-256 point: %v", err)
	}
	priv, err := curve.NewPrivateKey(privRaw)
	if err != nil {
		return false, fmt.Sprintf("webpush: private key is not a valid P-256 scalar: %v", err)
	}
	if !bytes.Equal(priv.PublicKey().Bytes(), pub.Bytes()) {
		return false, "webpush: private key does not match public key"
	}
	return true, "webpush: vapid keypair is valid"
}

// authedGET performs a bounded GET with an optional Authorization header and
// returns the status code and a short truncated body. The probe never follows
// into the response beyond 256 bytes.
func authedGET(ctx context.Context, client *http.Client, endpoint, authHeader string) (status int, body string, err error) {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return 0, "", err
	}
	if authHeader != "" {
		req.Header.Set("Authorization", authHeader)
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
	return resp.StatusCode, strings.TrimSpace(string(b)), nil
}

// probeSendmail verifies the pipe command can be found and is executable. It
// deliberately does not run it: invoking an MTA with no message is not a
// meaningful check, and some MTAs would wait on stdin.
func probeSendmail(argv []string) (ok bool, detail string) {
	if len(argv) == 0 || strings.TrimSpace(argv[0]) == "" {
		return false, "sendmail: no command configured"
	}
	path, err := exec.LookPath(argv[0])
	if err != nil {
		return false, fmt.Sprintf("sendmail: %s not found or not executable: %v", argv[0], err)
	}
	return true, fmt.Sprintf("sendmail: %s found (%s)", path, strings.Join(argv, " "))
}
