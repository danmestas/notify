package webpush

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	vapidpush "github.com/SherClockHolmes/webpush-go"

	"github.com/danmestas/notify/internal/providers"
)

// captured is one request observed by the stub push server.
type captured struct {
	method string
	header http.Header
	body   []byte
}

// stubServer returns an httptest server answering status and a pointer to the
// last captured request.
func stubServer(t *testing.T, status int) (*httptest.Server, *captured) {
	t.Helper()
	got := &captured{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got.method = r.Method
		got.header = r.Header.Clone()
		got.body = b
		w.WriteHeader(status)
	}))
	t.Cleanup(srv.Close)
	return srv, got
}

// testSubscription returns a Subscription whose keys are valid base64url P-256
// public-key / 16-byte-auth-secret shapes so webpush-go's ECDH step succeeds;
// the endpoint points at the stub server.
func testSubscription(t *testing.T, endpoint string) Subscription {
	t.Helper()
	// A valid uncompressed P-256 point: reuse VAPID key generation, which
	// returns exactly that shape for its public key.
	_, pub, err := vapidpush.GenerateVAPIDKeys()
	if err != nil {
		t.Fatalf("GenerateVAPIDKeys: %v", err)
	}
	authSecret := make([]byte, 16)
	if _, err := rand.Read(authSecret); err != nil {
		t.Fatalf("rand.Read: %v", err)
	}
	return Subscription{
		Endpoint: endpoint,
		P256dh:   pub,
		Auth:     base64.RawURLEncoding.EncodeToString(authSecret),
	}
}

func vapidKeys(t *testing.T) (priv, pub string) {
	t.Helper()
	priv, pub, err := vapidpush.GenerateVAPIDKeys()
	if err != nil {
		t.Fatalf("GenerateVAPIDKeys: %v", err)
	}
	return priv, pub
}

// TestSend_Wire pins the encrypted request shape: Authorization carries a
// vapid t=...,k=... credential, Content-Encoding is aes128gcm, and TTL is
// present as a header.
func TestSend_Wire(t *testing.T) {
	srv, got := stubServer(t, http.StatusCreated)
	priv, pub := vapidKeys(t)
	c := &Client{PublicKey: pub, PrivateKey: priv, Subject: testSubject, HTTPClient: srv.Client()}

	_, err := c.SendToSubscription(context.Background(), testSubscription(t, srv.URL), providers.PushMessage{
		Title: "Build failed",
		Body:  "notify CI is red",
	})
	if err != nil {
		t.Fatalf("SendToSubscription: %v", err)
	}

	if got.method != http.MethodPost {
		t.Errorf("method = %q, want POST", got.method)
	}
	if enc := got.header.Get("Content-Encoding"); enc != "aes128gcm" {
		t.Errorf("Content-Encoding = %q, want aes128gcm", enc)
	}
	if ttl := got.header.Get("TTL"); ttl != "86400" {
		t.Errorf("TTL = %q, want 86400 (default 24h)", ttl)
	}
	auth := got.header.Get("Authorization")
	if !strings.HasPrefix(auth, "vapid ") {
		t.Errorf("Authorization = %q, want vapid ... prefix", auth)
	}
	if len(got.body) == 0 {
		t.Error("request body is empty, want the encrypted payload")
	}
}

// TestSend_CustomTTL proves msg.Data["ttl"] (seconds) overrides the 24h
// default.
func TestSend_CustomTTL(t *testing.T) {
	srv, got := stubServer(t, http.StatusCreated)
	priv, pub := vapidKeys(t)
	c := &Client{PublicKey: pub, PrivateKey: priv, Subject: testSubject, HTTPClient: srv.Client()}

	_, err := c.SendToSubscription(context.Background(), testSubscription(t, srv.URL), providers.PushMessage{
		Title: "t", Body: "b",
		Data: map[string]string{"ttl": "3600"},
	})
	if err != nil {
		t.Fatalf("SendToSubscription: %v", err)
	}
	if ttl := got.header.Get("TTL"); ttl != "3600" {
		t.Errorf("TTL = %q, want 3600", ttl)
	}
}

// TestSend_2xxIsSuccess proves any 2xx status is treated as delivered.
func TestSend_2xxIsSuccess(t *testing.T) {
	srv, _ := stubServer(t, http.StatusOK)
	priv, pub := vapidKeys(t)
	c := &Client{PublicKey: pub, PrivateKey: priv, Subject: testSubject, HTTPClient: srv.Client()}

	if _, err := c.SendToSubscription(context.Background(), testSubscription(t, srv.URL), providers.PushMessage{Title: "t", Body: "b"}); err != nil {
		t.Fatalf("SendToSubscription: %v", err)
	}
}

// TestSend_404And410AreSubscriptionGone proves both codes the push services
// use to report a dead subscription classify as ErrSubscriptionGone.
func TestSend_404And410AreSubscriptionGone(t *testing.T) {
	for _, status := range []int{http.StatusNotFound, http.StatusGone} {
		srv, _ := stubServer(t, status)
		priv, pub := vapidKeys(t)
		c := &Client{PublicKey: pub, PrivateKey: priv, Subject: testSubject, HTTPClient: srv.Client()}

		_, err := c.SendToSubscription(context.Background(), testSubscription(t, srv.URL), providers.PushMessage{Title: "t", Body: "b"})
		if err != ErrSubscriptionGone {
			t.Errorf("status %d: err = %v, want ErrSubscriptionGone", status, err)
		}
	}
}

// TestSend_5xxIsTransient proves a 5xx response is retry-safe.
func TestSend_5xxIsTransient(t *testing.T) {
	srv, _ := stubServer(t, http.StatusServiceUnavailable)
	priv, pub := vapidKeys(t)
	c := &Client{PublicKey: pub, PrivateKey: priv, Subject: testSubject, HTTPClient: srv.Client()}

	_, err := c.SendToSubscription(context.Background(), testSubscription(t, srv.URL), providers.PushMessage{Title: "t", Body: "b"})
	if err == nil {
		t.Fatal("expected an error for 503")
	}
	if !providers.IsTransient(err) {
		t.Errorf("err = %v, want a TransientError", err)
	}
}

// TestSend_4xxOtherIsPermanent proves a non-404/410/5xx failure (e.g. 400 bad
// request) is a plain permanent error, not transient and not
// ErrSubscriptionGone.
func TestSend_4xxOtherIsPermanent(t *testing.T) {
	srv, _ := stubServer(t, http.StatusBadRequest)
	priv, pub := vapidKeys(t)
	c := &Client{PublicKey: pub, PrivateKey: priv, Subject: testSubject, HTTPClient: srv.Client()}

	_, err := c.SendToSubscription(context.Background(), testSubscription(t, srv.URL), providers.PushMessage{Title: "t", Body: "b"})
	if err == nil {
		t.Fatal("expected an error for 400")
	}
	if providers.IsTransient(err) {
		t.Error("400 must not be classified transient")
	}
	if err == ErrSubscriptionGone {
		t.Error("400 must not be classified ErrSubscriptionGone")
	}
}

// TestSend_ErrorNeverLeaksPrivateKey proves the returned error text never
// contains the VAPID private key or the subscription's auth secret, across
// every classified failure path.
func TestSend_ErrorNeverLeaksPrivateKey(t *testing.T) {
	priv, pub := vapidKeys(t)
	for _, status := range []int{http.StatusBadRequest, http.StatusServiceUnavailable, http.StatusGone} {
		srv, _ := stubServer(t, status)
		c := &Client{PublicKey: pub, PrivateKey: priv, Subject: testSubject, HTTPClient: srv.Client()}
		sub := testSubscription(t, srv.URL)

		_, err := c.SendToSubscription(context.Background(), sub, providers.PushMessage{Title: "t", Body: "b"})
		if err == nil {
			continue
		}
		msg := err.Error()
		if strings.Contains(msg, priv) {
			t.Errorf("status %d: error text leaks the VAPID private key: %q", status, msg)
		}
		if strings.Contains(msg, sub.Auth) {
			t.Errorf("status %d: error text leaks the subscription auth secret: %q", status, msg)
		}
	}
}

// TestSend_UnreachableEndpointIsTransient proves a transport-level failure
// (connection refused) is retry-safe, mirroring the ntfy client's convention.
func TestSend_UnreachableEndpointIsTransient(t *testing.T) {
	priv, pub := vapidKeys(t)
	c := &Client{PublicKey: pub, PrivateKey: priv, Subject: testSubject}

	_, err := c.SendToSubscription(context.Background(), testSubscription(t, "http://127.0.0.1:1"), providers.PushMessage{Title: "t", Body: "b"})
	if err == nil {
		t.Fatal("expected an error for an unreachable endpoint")
	}
	if !providers.IsTransient(err) {
		t.Errorf("err = %v, want a TransientError", err)
	}
}

// TestClient_SatisfiesPushProvider proves *Client implements
// providers.PushProvider for compile-time compatibility with
// connectivity.PushProviderFromIntegration's switch, even though real sends
// always go through SendToSubscription.
func TestClient_SatisfiesPushProvider(t *testing.T) {
	var _ providers.PushProvider = (*Client)(nil)
}

// testSubject is a VAPID contact a push service would accept.
const testSubject = "mailto:ops@example.com"

// TestSend_SubjectIsSignedAndRequired: the configured subject is what the
// VAPID JWT carries (Apple rejects a localhost one, #358), and a client with
// no subject refuses to send rather than inventing one.
func TestSend_SubjectIsSignedAndRequired(t *testing.T) {
	priv, pub := vapidKeys(t)
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	c := &Client{PublicKey: pub, PrivateKey: priv, Subject: "https://notify.example.com", HTTPClient: srv.Client()}
	if _, err := c.SendToSubscription(context.Background(), testSubscription(t, srv.URL), providers.PushMessage{Title: "t"}); err != nil {
		t.Fatalf("SendToSubscription: %v", err)
	}
	if got := jwtSub(t, auth); got != "https://notify.example.com" {
		t.Fatalf("JWT sub = %q, want the configured subject", got)
	}

	bare := &Client{PublicKey: pub, PrivateKey: priv, HTTPClient: srv.Client()}
	if _, err := bare.SendToSubscription(context.Background(), testSubscription(t, srv.URL), providers.PushMessage{Title: "t"}); err == nil {
		t.Fatal("a client with no subject sent anyway")
	}
}

// jwtSub extracts the "sub" claim from a "vapid t=<jwt>, k=<key>" header.
func jwtSub(t *testing.T, auth string) string {
	t.Helper()
	i := strings.Index(auth, "t=")
	if i < 0 {
		t.Fatalf("no VAPID token in Authorization %q", auth)
	}
	tok := auth[i+2:]
	if j := strings.IndexByte(tok, ','); j >= 0 {
		tok = tok[:j]
	}
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		t.Fatalf("malformed JWT %q", tok)
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("decode JWT claims: %v", err)
	}
	var claims struct {
		Sub string `json:"sub"`
	}
	if err := json.Unmarshal(raw, &claims); err != nil {
		t.Fatalf("unmarshal JWT claims: %v", err)
	}
	return claims.Sub
}
