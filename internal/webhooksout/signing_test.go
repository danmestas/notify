package webhooksout

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/secretbox"
)

func testBox(t *testing.T) *secretbox.Box {
	t.Helper()
	key := bytes.Repeat([]byte{0x5a}, 32)
	box, err := secretbox.New(key)
	if err != nil {
		t.Fatalf("secretbox.New: %v", err)
	}
	return box
}

// verify recomputes the consumer's scheme the way a receiver would:
// sha256=hex(HMAC-SHA256(secret, timestamp + "." + body)).
func verify(secret, ts string, body []byte, sig string) bool {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts + "."))
	mac.Write(body)
	return hmac.Equal([]byte("sha256="+hex.EncodeToString(mac.Sum(nil))), []byte(sig))
}

// signedReceiver is a fake endpoint that records the headers and body of each
// POST.
type signedReceiver struct {
	srv  *httptest.Server
	hits chan receivedPost
}

type receivedPost struct {
	sig, ts string
	body    []byte
}

func newSignedReceiver(t *testing.T) *signedReceiver {
	t.Helper()
	r := &signedReceiver{hits: make(chan receivedPost, 8)}
	r.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		r.hits <- receivedPost{sig: req.Header.Get(signatureHeader), ts: req.Header.Get(timestampHeader), body: body}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(r.srv.Close)
	return r
}

func (r *signedReceiver) next(t *testing.T) receivedPost {
	t.Helper()
	select {
	case p := <-r.hits:
		return p
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for POST")
		return receivedPost{}
	}
}

// TestManagement_CreateReturnsSecretOnceAndStoresItSealed: the create result
// carries the plaintext; the row carries only a sealed value that is not it.
func TestManagement_CreateReturnsSecretOnceAndStoresItSealed(t *testing.T) {
	st := newStore(t)
	box := testBox(t)
	ctx := context.Background()
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}

	created, err := NewManagement(st, box).Create(ctx, scope, "https://example.org/hook", true)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if len(created.Secret) != 64 {
		t.Fatalf("secret = %q, want 32 random bytes as hex", created.Secret)
	}
	row, err := st.WebhookEndpoints().GetByID(ctx, scope, created.Endpoint.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if row.Secret == "" || row.Secret == created.Secret || !secretbox.IsSealed(row.Secret) {
		t.Fatalf("stored secret = %q; want sealed and not the plaintext", row.Secret)
	}
	if opened, _ := box.Open(row.Secret); opened != created.Secret {
		t.Fatalf("sealed value does not open to the returned secret")
	}
}

// TestManagement_RotateSecret: a rotation mints a different secret and the
// row is updated; an unknown id is not found; with no box it is unavailable.
func TestManagement_RotateSecret(t *testing.T) {
	st := newStore(t)
	box := testBox(t)
	ctx := context.Background()
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}
	mgmt := NewManagement(st, box)

	created, _ := mgmt.Create(ctx, scope, "https://example.org/hook", true)
	rotated, err := mgmt.RotateSecret(ctx, scope, created.Endpoint.ID)
	if err != nil {
		t.Fatalf("RotateSecret: %v", err)
	}
	if rotated.Secret == created.Secret || len(rotated.Secret) != 64 {
		t.Fatalf("rotated secret = %q, want a fresh one", rotated.Secret)
	}
	row, _ := st.WebhookEndpoints().GetByID(ctx, scope, created.Endpoint.ID)
	if opened, _ := box.Open(row.Secret); opened != rotated.Secret {
		t.Fatal("row does not carry the rotated secret")
	}
	if _, err := mgmt.RotateSecret(ctx, scope, "nope"); err == nil {
		t.Fatal("rotating an unknown endpoint succeeded")
	}
	if _, err := NewManagement(st, nil).RotateSecret(ctx, scope, created.Endpoint.ID); err != ErrSigningUnavailable {
		t.Fatalf("rotate with no box = %v, want ErrSigningUnavailable", err)
	}
}

// TestConsumer_SignsWithEndpointSecret is the contract with the receiver:
// both headers present, and the signature verifies over timestamp + "." + body
// with the secret Create returned.
func TestConsumer_SignsWithEndpointSecret(t *testing.T) {
	bus := newBus(t)
	st := newStore(t)
	box := testBox(t)
	ctx := context.Background()
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}
	rcv := newSignedReceiver(t)

	created, err := NewManagement(st, box).Create(ctx, scope, rcv.srv.URL, true)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	consumer := NewWithClient(bus, st, &http.Client{Timeout: 5 * time.Second}).WithSecrets(box)
	stop, err := consumer.Start(ctx)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = stop() }()

	body := mustJSON(t, domain.Message{OrgID: "o1", EnvID: "e1", Channel: "email", Status: "sent"})
	if err := bus.PublishDelivery(scope.EnvID, body); err != nil {
		t.Fatalf("PublishDelivery: %v", err)
	}

	got := rcv.next(t)
	if got.ts == "" || got.sig == "" {
		t.Fatalf("headers missing: ts=%q sig=%q", got.ts, got.sig)
	}
	if !strings.HasPrefix(got.sig, "sha256=") {
		t.Errorf("signature = %q, want sha256= prefix", got.sig)
	}
	if !verify(created.Secret, got.ts, got.body, got.sig) {
		t.Fatalf("signature does not verify over ts.body with the created secret")
	}
	if verify(created.Secret, got.ts, []byte(`{"tampered":true}`), got.sig) {
		t.Fatal("signature verified over a different body")
	}
	if n, _ := strconv.ParseInt(got.ts, 10, 64); time.Since(time.Unix(n, 0)) > time.Minute {
		t.Errorf("timestamp %s is not recent", got.ts)
	}
}

// TestConsumer_UnsignedEndpointStaysUnsigned: an endpoint that predates
// signing (no secret) keeps receiving events, with neither header.
func TestConsumer_UnsignedEndpointStaysUnsigned(t *testing.T) {
	bus := newBus(t)
	st := newStore(t)
	ctx := context.Background()
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}
	rcv := newSignedReceiver(t)

	if _, err := st.WebhookEndpoints().Create(ctx, scope, domain.WebhookEndpoint{URL: rcv.srv.URL, Active: true}); err != nil {
		t.Fatalf("create endpoint: %v", err)
	}
	consumer := NewWithClient(bus, st, &http.Client{Timeout: 5 * time.Second}).WithSecrets(testBox(t))
	stop, err := consumer.Start(ctx)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = stop() }()

	_ = bus.PublishDelivery(scope.EnvID, mustJSON(t, domain.Message{OrgID: "o1", EnvID: "e1", Status: "sent"}))
	got := rcv.next(t)
	if got.sig != "" || got.ts != "" {
		t.Fatalf("unsigned endpoint got headers: sig=%q ts=%q", got.sig, got.ts)
	}
}

// TestConsumer_UnopenableSecretIsSkippedNotSentUnsigned: a sealed secret this
// process cannot open (wrong master key) must not degrade to an unsigned
// send, which any verifying receiver would reject anyway; it is skipped and
// logged.
func TestConsumer_UnopenableSecretIsSkippedNotSentUnsigned(t *testing.T) {
	bus := newBus(t)
	st := newStore(t)
	ctx := context.Background()
	scope := domain.Scope{OrgID: "o1", EnvID: "e1"}
	rcv := newSignedReceiver(t)

	// Sealed under one key, consumed under another.
	if _, err := NewManagement(st, testBox(t)).Create(ctx, scope, rcv.srv.URL, true); err != nil {
		t.Fatalf("Create: %v", err)
	}
	other, _ := secretbox.New(bytes.Repeat([]byte{0x11}, 32))
	consumer := NewWithClient(bus, st, &http.Client{Timeout: 5 * time.Second}).WithSecrets(other)
	stop, err := consumer.Start(ctx)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = stop() }()

	_ = bus.PublishDelivery(scope.EnvID, mustJSON(t, domain.Message{OrgID: "o1", EnvID: "e1", Status: "sent"}))
	select {
	case got := <-rcv.hits:
		t.Fatalf("an event was delivered despite an unopenable secret (sig=%q)", got.sig)
	case <-time.After(1500 * time.Millisecond):
	}
}
