// Package bluesky implements providers.ChatProvider against the AT Protocol
// (https://atproto.com), the network Bluesky runs on. Send authenticates
// against a Personal Data Server (PDS) with com.atproto.server.createSession
// and then POSTs a skeet via com.atproto.repo.createRecord. A minted session
// (accessJwt/refreshJwt) is cached on the Client and reused across sends;
// when the cached accessJwt has expired the client refreshes it via
// com.atproto.server.refreshSession and retries once, falling back to a
// full re-login when the refresh itself fails.
package bluesky

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/danmestas/notify/internal/providers"
)

var _ providers.ChatProvider = (*Client)(nil)

// defaultPDSURL is used when PDSURL is unset, matching the entry point most
// Bluesky accounts are hosted on.
const defaultPDSURL = "https://bsky.social"

// maxGraphemes is the AT Protocol post text limit. notify truncates to this
// (with a trailing ellipsis) rather than letting the PDS reject an oversized
// post outright.
const maxGraphemes = 300

// Client posts to one Bluesky account via its PDS. PDSURL defaults to
// https://bsky.social when empty. Identifier is the account's handle or DID;
// AppPassword is an app-specific password (Settings → Privacy and Security →
// App Passwords), never the account's own login password. A nil HTTPClient
// gets a 10s-timeout default per Send.
//
// msg.Address is unused: Bluesky posts are public broadcasts to the
// account's own feed, not addressed to an individual recipient the way a
// Telegram chat_id or Discord webhook override is.
type Client struct {
	PDSURL      string
	Identifier  string
	AppPassword string
	HTTPClient  *http.Client

	mu         sync.Mutex
	accessJwt  string
	refreshJwt string
	did        string
}

// session response from createSession / refreshSession.
type sessionResp struct {
	AccessJwt  string `json:"accessJwt"`
	RefreshJwt string `json:"refreshJwt"`
	DID        string `json:"did"`
}

// createRecordResp is the response from com.atproto.repo.createRecord.
type createRecordResp struct {
	URI string `json:"uri"`
}

// Send authenticates (minting or reusing a cached session) and posts
// msg.Content as an app.bsky.feed.post record. Text longer than 300
// graphemes is truncated with a trailing ellipsis. The provider message id
// is the created record's at:// uri.
func (c *Client) Send(ctx context.Context, msg providers.ChatMessage) (providers.Result, error) {
	if c.Identifier == "" {
		return providers.Result{}, fmt.Errorf("bluesky: empty identifier")
	}
	if c.AppPassword == "" {
		return providers.Result{}, fmt.Errorf("bluesky: empty app password")
	}

	hc := c.httpClient()
	pds := c.pdsURL()
	text := truncateGraphemes(msg.Content, maxGraphemes)

	if !c.hasSession() {
		if err := c.login(ctx, hc, pds); err != nil {
			return providers.Result{}, err
		}
	}

	uri, status, err := c.createPost(ctx, hc, pds, text)
	if err == nil {
		return providers.Result{ProviderMessageID: uri}, nil
	}
	if status != http.StatusUnauthorized {
		return providers.Result{}, err
	}

	// accessJwt expired: refresh, falling back to a full re-login when the
	// refresh itself is rejected.
	if rerr := c.refresh(ctx, hc, pds); rerr != nil {
		if lerr := c.login(ctx, hc, pds); lerr != nil {
			return providers.Result{}, lerr
		}
	}

	uri, _, err = c.createPost(ctx, hc, pds, text)
	if err != nil {
		return providers.Result{}, err
	}
	return providers.Result{ProviderMessageID: uri}, nil
}

func (c *Client) httpClient() *http.Client {
	if c.HTTPClient != nil {
		return c.HTTPClient
	}
	return &http.Client{Timeout: 10 * time.Second}
}

// pdsURL returns c.PDSURL trimmed of any trailing slash, defaulting to
// https://bsky.social when unset.
func (c *Client) pdsURL() string {
	if c.PDSURL == "" {
		return defaultPDSURL
	}
	return strings.TrimRight(c.PDSURL, "/")
}

func (c *Client) hasSession() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.accessJwt != ""
}

// login calls com.atproto.server.createSession with Identifier/AppPassword
// and caches the returned accessJwt/refreshJwt/did.
func (c *Client) login(ctx context.Context, hc *http.Client, pds string) error {
	reqBody, err := json.Marshal(map[string]string{
		"identifier": c.Identifier,
		"password":   c.AppPassword,
	})
	if err != nil {
		return fmt.Errorf("bluesky: build createSession request: %w", err)
	}

	sess, _, err := c.doSessionRequest(ctx, hc, pds+"/xrpc/com.atproto.server.createSession", reqBody, "")
	if err != nil {
		return c.redact(fmt.Errorf("bluesky: create session: %w", err))
	}

	c.mu.Lock()
	c.accessJwt = sess.AccessJwt
	c.refreshJwt = sess.RefreshJwt
	c.did = sess.DID
	c.mu.Unlock()
	return nil
}

// refresh calls com.atproto.server.refreshSession with the cached
// refreshJwt and caches the returned accessJwt/refreshJwt.
func (c *Client) refresh(ctx context.Context, hc *http.Client, pds string) error {
	c.mu.Lock()
	refreshJwt := c.refreshJwt
	c.mu.Unlock()
	if refreshJwt == "" {
		return errors.New("bluesky: no refresh token cached")
	}

	sess, _, err := c.doSessionRequest(ctx, hc, pds+"/xrpc/com.atproto.server.refreshSession", nil, refreshJwt)
	if err != nil {
		return c.redact(fmt.Errorf("bluesky: refresh session: %w", err))
	}

	c.mu.Lock()
	c.accessJwt = sess.AccessJwt
	c.refreshJwt = sess.RefreshJwt
	c.mu.Unlock()
	return nil
}

// doSessionRequest POSTs to endpoint (createSession or refreshSession) and
// decodes a sessionResp on 2xx. bearer, when non-empty, is sent as the
// Authorization header (refreshSession authenticates with the refresh JWT
// instead of a body).
func (c *Client) doSessionRequest(ctx context.Context, hc *http.Client, endpoint string, body []byte, bearer string) (sessionResp, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return sessionResp{}, 0, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}

	resp, err := hc.Do(req)
	if err != nil {
		return sessionResp{}, 0, providers.NewTransientError(fmt.Errorf("do request: %w", err))
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))

	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		var sess sessionResp
		if err := json.Unmarshal(respBody, &sess); err != nil {
			return sessionResp{}, resp.StatusCode, fmt.Errorf("decode response: %w", err)
		}
		return sess, resp.StatusCode, nil
	case resp.StatusCode >= 500:
		return sessionResp{}, resp.StatusCode, providers.NewTransientError(
			fmt.Errorf("status %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody))),
		)
	default:
		return sessionResp{}, resp.StatusCode, fmt.Errorf("status %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
}

// createPost POSTs com.atproto.repo.createRecord with the current cached
// accessJwt and returns the created record's at:// uri, the HTTP status
// code (so Send can detect a 401 and retry after refresh), and any error.
func (c *Client) createPost(ctx context.Context, hc *http.Client, pds, text string) (string, int, error) {
	c.mu.Lock()
	accessJwt := c.accessJwt
	did := c.did
	c.mu.Unlock()

	reqBody, err := json.Marshal(map[string]any{
		"repo":       did,
		"collection": "app.bsky.feed.post",
		"record": map[string]any{
			"$type":     "app.bsky.feed.post",
			"text":      text,
			"createdAt": time.Now().UTC().Format(time.RFC3339),
		},
	})
	if err != nil {
		return "", 0, fmt.Errorf("bluesky: build createRecord request: %w", err)
	}

	endpoint := pds + "/xrpc/com.atproto.repo.createRecord"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(reqBody))
	if err != nil {
		return "", 0, fmt.Errorf("bluesky: build createRecord request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+accessJwt)

	resp, err := hc.Do(req)
	if err != nil {
		return "", 0, c.redact(providers.NewTransientError(fmt.Errorf("bluesky: create record: %w", err)))
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))

	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		var rec createRecordResp
		_ = json.Unmarshal(respBody, &rec)
		return rec.URI, resp.StatusCode, nil
	case resp.StatusCode >= 500:
		return "", resp.StatusCode, c.redact(providers.NewTransientError(
			fmt.Errorf("bluesky: create record: status %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody))),
		))
	default:
		// Includes 401 (expired accessJwt), which Send retries after a
		// refresh/re-login; any other 4xx is permanent.
		return "", resp.StatusCode, c.redact(
			fmt.Errorf("bluesky: create record: status %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody))),
		)
	}
}

// redact strips AppPassword and any cached JWTs out of err's message.
// Mirrors telegram.redactToken (#102): secrets must never appear in an
// error string, since Message.ErrorText persists it and logging echoes it
// verbatim.
func (c *Client) redact(err error) error {
	if err == nil {
		return err
	}
	c.mu.Lock()
	secrets := []string{c.AppPassword, c.accessJwt, c.refreshJwt}
	c.mu.Unlock()

	msg := err.Error()
	clean := msg
	for _, s := range secrets {
		if s == "" {
			continue
		}
		clean = strings.ReplaceAll(clean, s, "<redacted>")
	}
	if clean == msg {
		return err
	}
	if providers.IsTransient(err) {
		return providers.NewTransientError(errors.New(clean))
	}
	return errors.New(clean)
}

// truncateGraphemes truncates s to at most max runes, replacing the final
// rune with an ellipsis when truncation occurs so the total length is still
// max. This is a rune-count approximation of grapheme counting (sufficient
// for AT Protocol's practical limit); it does not join combining marks or
// surrogate-pair-style multi-rune clusters into a single unit.
func truncateGraphemes(s string, max int) string {
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max-1]) + "…"
}
