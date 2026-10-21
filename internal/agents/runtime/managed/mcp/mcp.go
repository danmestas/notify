// Package mcp is the small slice of the Model Context Protocol a managed
// agent needs: list a server's tools and call them, over Streamable HTTP.
// Every request is a JSON-RPC 2.0 POST; a response comes back as a JSON body
// or as a server-sent event stream. Responses are read up to MaxResponseBytes,
// redirects are never followed and private addresses are refused, so a server
// can neither flood the turn nor point notify at its own network.
package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/danmestas/notify/internal/delivery/httpaction"
)

// ProtocolVersion is the MCP revision this client speaks.
const ProtocolVersion = "2025-06-18"

// MaxResponseBytes caps the bytes read from the wire for one response.
const MaxResponseBytes = 256 << 10

// Class names why a call failed, in terms a model can act on.
type Class string

// The failure classes.
const (
	Timeout     Class = "timeout"
	Unavailable Class = "unavailable"
	TooLarge    Class = "too_large"
	Rejected    Class = "rejected"
)

// Error is a classified failure.
type Error struct {
	Class Class
	Err   error
}

func (e *Error) Error() string {
	if e.Err == nil {
		return string(e.Class)
	}
	return string(e.Class) + ": " + e.Err.Error()
}

func (e *Error) Unwrap() error { return e.Err }

func fail(c Class, format string, args ...any) *Error {
	return &Error{Class: c, Err: fmt.Errorf(format, args...)}
}

// ClassOf classifies err: "" for nil, Unavailable when nothing more specific
// is known.
func ClassOf(err error) Class {
	if err == nil {
		return ""
	}
	var e *Error
	if errors.As(err, &e) {
		return e.Class
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return Timeout
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return Timeout
	}
	if strings.Contains(err.Error(), "ssrf guard:") {
		return Rejected
	}
	return Unavailable
}

// CheckURL refuses a server address notify must not call: anything but https,
// credentials in the URL, or a literal private, link-local or loopback
// address. allowLoopback admits loopback hosts, over http too, for
// development and tests.
func CheckURL(raw string, allowLoopback bool) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("mcp server url is not a URL: %w", err)
	}
	host := u.Hostname()
	if host == "" {
		return errors.New("mcp server url needs a host")
	}
	if u.User != nil {
		return errors.New("mcp server url must not carry credentials: use the auth header")
	}
	loopback := isLoopback(host)
	switch strings.ToLower(u.Scheme) {
	case "https":
	case "http":
		if !loopback || !allowLoopback {
			return errors.New("mcp server url must be https")
		}
	default:
		return errors.New("mcp server url must be https")
	}
	if loopback {
		if !allowLoopback {
			return errors.New("mcp server url is a loopback address")
		}
		return nil
	}
	if ip := net.ParseIP(host); ip != nil && httpaction.IsBlockedIP(ip) {
		return fmt.Errorf("mcp server url %s is in a private or reserved range", ip)
	}
	return nil
}

func isLoopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// NewHTTPClient returns the client MCP requests go through: the SSRF-guarded
// transport, no redirects, and every URL re-checked with CheckURL. With
// allowLoopback, loopback hosts bypass the guard.
func NewHTTPClient(allowLoopback bool) *http.Client {
	safe := httpaction.NewSSRFSafeClient()
	return &http.Client{
		Transport: &guard{safe: safe.Transport, plain: http.DefaultTransport, allowLoopback: allowLoopback},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
		Timeout: time.Minute, // backstop; callers bound each call with ctx
	}
}

type guard struct {
	safe, plain   http.RoundTripper
	allowLoopback bool
}

func (g *guard) RoundTrip(r *http.Request) (*http.Response, error) {
	if err := CheckURL(r.URL.String(), g.allowLoopback); err != nil {
		return nil, &Error{Class: Rejected, Err: err}
	}
	if g.allowLoopback && isLoopback(r.URL.Hostname()) {
		return g.plain.RoundTrip(r)
	}
	return g.safe.RoundTrip(r)
}

// Tool is one tool a server offers.
type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

// Result is a tool's answer: its structuredContent when it sent one,
// otherwise its text content joined by newlines.
type Result struct {
	Structured json.RawMessage
	Text       string
	IsError    bool
}

// Stats are the bytes one call moved on the wire.
type Stats struct {
	RequestBytes  int
	ResponseBytes int
}

// Client talks to one server for the length of one turn.
type Client struct {
	http    *http.Client
	url     string
	header  http.Header
	session string
	ready   bool
	nextID  int
}

// New returns a client for the server at url. header is sent with every
// request (the auth header and correlation headers).
func New(hc *http.Client, url string, header http.Header) *Client {
	return &Client{http: hc, url: url, header: header}
}

// Initialize opens the session: initialize, then notifications/initialized.
func (c *Client) Initialize(ctx context.Context) error {
	var res struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	params := map[string]any{
		"protocolVersion": ProtocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "notify", "version": "1"},
	}
	if _, err := c.request(ctx, "initialize", params, &res); err != nil {
		return err
	}
	c.ready = true
	_, err := c.notify(ctx, "notifications/initialized")
	return err
}

// maxToolPages bounds tools/list paging against a server whose cursor never
// ends.
const maxToolPages = 20

// ListTools lists every tool, following nextCursor.
func (c *Client) ListTools(ctx context.Context) ([]Tool, error) {
	var tools []Tool
	cursor := ""
	for page := 0; page < maxToolPages; page++ {
		params := map[string]any{}
		if cursor != "" {
			params["cursor"] = cursor
		}
		var res struct {
			Tools      []Tool `json:"tools"`
			NextCursor string `json:"nextCursor"`
		}
		if _, err := c.request(ctx, "tools/list", params, &res); err != nil {
			return nil, err
		}
		tools = append(tools, res.Tools...)
		if res.NextCursor == "" {
			return tools, nil
		}
		cursor = res.NextCursor
	}
	return tools, nil
}

// CallTool calls one tool. A tool's own failure is a Result with IsError; an
// error return is a failure to get an answer at all.
func (c *Client) CallTool(ctx context.Context, name string, args json.RawMessage) (Result, Stats, error) {
	if len(bytes.TrimSpace(args)) == 0 {
		args = json.RawMessage(`{}`)
	}
	var res struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		StructuredContent json.RawMessage `json:"structuredContent"`
		IsError           bool            `json:"isError"`
	}
	st, err := c.request(ctx, "tools/call", map[string]any{"name": name, "arguments": args}, &res)
	if err != nil {
		return Result{}, st, err
	}
	out := Result{IsError: res.IsError}
	if s := bytes.TrimSpace(res.StructuredContent); len(s) > 0 && string(s) != "null" {
		out.Structured = s
		return out, st, nil
	}
	var texts []string
	for _, p := range res.Content {
		if p.Type == "text" {
			texts = append(texts, p.Text)
		}
	}
	out.Text = strings.Join(texts, "\n")
	return out, st, nil
}

type rpcMessage struct {
	ID     json.RawMessage `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// request sends one JSON-RPC request and decodes its result into out.
func (c *Client) request(ctx context.Context, method string, params any, out any) (Stats, error) {
	c.nextID++
	id := strconv.Itoa(c.nextID)
	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": c.nextID, "method": method, "params": params})
	if err != nil {
		return Stats{}, fail(Rejected, "encode %s: %v", method, err)
	}
	resp, st, err := c.post(ctx, body)
	if err != nil {
		return st, err
	}
	defer func() { _ = resp.Body.Close() }()
	r := &capReader{r: resp.Body, max: MaxResponseBytes}
	if c.session == "" {
		c.session = resp.Header.Get("Mcp-Session-Id")
	}
	var msg *rpcMessage
	mt, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	switch mt {
	case "text/event-stream":
		msg, err = readEvents(r, id)
	case "application/json":
		msg, err = readJSON(r)
	default:
		err = fail(Unavailable, "%s: unexpected content type %q", method, mt)
	}
	st.ResponseBytes = int(r.n)
	if err != nil {
		return st, classify(err)
	}
	if msg.Error != nil {
		return st, fail(Rejected, "%s: %s (%d)", method, msg.Error.Message, msg.Error.Code)
	}
	if err := json.Unmarshal(msg.Result, out); err != nil {
		return st, fail(Unavailable, "%s: malformed result: %v", method, err)
	}
	return st, nil
}

// notify sends a JSON-RPC notification, which has no response body.
func (c *Client) notify(ctx context.Context, method string) (Stats, error) {
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": method})
	resp, st, err := c.post(ctx, body)
	if err != nil {
		return st, err
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, MaxResponseBytes))
	_ = resp.Body.Close()
	return st, nil
}

// post sends body and returns a 2xx response, or a classified error.
func (c *Client) post(ctx context.Context, body []byte) (*http.Response, Stats, error) {
	st := Stats{RequestBytes: len(body)}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(body))
	if err != nil {
		return nil, st, fail(Rejected, "build request: %v", err)
	}
	for k, vs := range c.header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if c.ready {
		req.Header.Set("Mcp-Protocol-Version", ProtocolVersion)
	}
	if c.session != "" {
		req.Header.Set("Mcp-Session-Id", c.session)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, st, classify(err)
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return resp, st, nil
	}
	_ = resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusRequestTimeout || resp.StatusCode == http.StatusGatewayTimeout:
		return nil, st, fail(Timeout, "server answered %d", resp.StatusCode)
	case resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests:
		return nil, st, fail(Unavailable, "server answered %d", resp.StatusCode)
	case resp.StatusCode >= 300 && resp.StatusCode < 400:
		return nil, st, fail(Rejected, "server redirected (%d); redirects are not followed", resp.StatusCode)
	default:
		return nil, st, fail(Rejected, "server answered %d", resp.StatusCode)
	}
}

func classify(err error) error {
	var e *Error
	if errors.As(err, &e) {
		return e
	}
	return &Error{Class: ClassOf(err), Err: err}
}

var errTooLarge = &Error{Class: TooLarge, Err: fmt.Errorf("response exceeds %d bytes", MaxResponseBytes)}

// capReader reads at most max bytes, then fails with errTooLarge. n counts
// what was read, never more than max+1.
type capReader struct {
	r   io.Reader
	n   int64
	max int64
}

func (c *capReader) Read(p []byte) (int, error) {
	if c.n > c.max {
		return 0, errTooLarge
	}
	if rest := c.max - c.n + 1; int64(len(p)) > rest {
		p = p[:rest]
	}
	k, err := c.r.Read(p)
	c.n += int64(k)
	if c.n > c.max {
		return k, errTooLarge
	}
	return k, err
}

func readJSON(r io.Reader) (*rpcMessage, error) {
	raw, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	var m rpcMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fail(Unavailable, "malformed response: %v", err)
	}
	return &m, nil
}

// readEvents reads a server-sent event stream until the response to id,
// skipping the server's own notifications and requests.
func readEvents(r io.Reader, id string) (*rpcMessage, error) {
	br := bufio.NewReader(r)
	var data strings.Builder
	dispatch := func() *rpcMessage {
		defer data.Reset()
		var m rpcMessage
		if data.Len() == 0 || json.Unmarshal([]byte(data.String()), &m) != nil {
			return nil
		}
		if string(bytes.TrimSpace(m.ID)) != id || (m.Result == nil && m.Error == nil) {
			return nil
		}
		return &m
	}
	for {
		line, err := br.ReadString('\n')
		if line != "" || err == nil {
			line = strings.TrimRight(line, "\r\n")
			switch {
			case line == "":
				if m := dispatch(); m != nil {
					return m, nil
				}
			case strings.HasPrefix(line, "data:"):
				data.WriteString(strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
				data.WriteByte('\n')
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				if m := dispatch(); m != nil {
					return m, nil
				}
				return nil, fail(Unavailable, "event stream ended without a response")
			}
			return nil, err
		}
	}
}
