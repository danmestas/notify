package httpaction_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/danmestas/notify/internal/authoring"
	"github.com/danmestas/notify/internal/delivery/httpaction"
	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/engine"
	"github.com/danmestas/notify/internal/secretbox"
	"github.com/danmestas/notify/internal/store"
)

// TestHTTPAction_HeaderSecret_EncryptedAtRest_PlaintextOnWire is the end-to-end
// proof of #264: an http_request step authored with a secret header is stored
// as ciphertext (never plaintext in the DB), yet the outbound request carries
// the real header value.
//
// It goes through the real authoring path (seal on write) and the real handler
// path (open at run time), not a hand-seeded row, so it exercises the whole
// chokepoint rather than either half in isolation.
func TestHTTPAction_HeaderSecret_EncryptedAtRest_PlaintextOnWire(t *testing.T) {
	const secretToken = "Bearer sk-live-e2e-secret"

	// Echo server records the Authorization header it actually received.
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	ctx := context.Background()
	s, err := store.Open(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	box, err := secretbox.New(bytes.Repeat([]byte{0x2a}, 32))
	if err != nil {
		t.Fatalf("secretbox.New: %v", err)
	}

	// Author the workflow THROUGH authoring, so headers are sealed on write.
	au := authoring.New(s, nil, authoring.WithSecretBox(box))
	scope := domain.Scope{OrgID: "org_1", EnvID: "env_1"}
	headers, _ := json.Marshal(map[string]string{"Authorization": secretToken})

	wf, err := au.Create(ctx, scope, authoring.WorkflowInput{
		Name:              "Call",
		TriggerIdentifier: "call",
		Steps: []domain.Step{{
			StepID: "h1", Type: "http_request",
			ControlValues: mustJSON(map[string]any{
				"url": srv.URL, "method": "POST",
				"headers": string(headers), "body": "",
				"timeoutSec": 5, "failurePolicy": "fail_closed",
			}),
		}},
	})
	if err != nil {
		t.Fatalf("authoring.Create: %v", err)
	}

	// The stored row must NOT contain the plaintext secret.
	stored, err := s.Workflows().GetByID(ctx, scope, wf.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if strings.Contains(string(stored.Steps), secretToken) {
		t.Fatalf("stored workflow steps contain the plaintext secret:\n%s", stored.Steps)
	}
	if strings.Contains(string(stored.Steps), "sk-live-e2e-secret") {
		t.Fatalf("stored workflow steps leak the token body:\n%s", stored.Steps)
	}

	// Seed a run so the handler can resolve the step.
	if _, err := s.Subscribers().Upsert(ctx, scope, domain.Subscriber{SubscriberID: "user_1", FirstName: "Ada"}); err != nil {
		t.Fatalf("Subscribers.Upsert: %v", err)
	}
	if _, err := s.Notifications().Create(ctx, scope, domain.Notification{
		ID: "ntf_1", WorkflowID: wf.ID, SubscriberID: "user_1", RunID: "r1",
		Payload: json.RawMessage(`{}`),
	}); err != nil {
		t.Fatalf("Notifications.Create: %v", err)
	}

	// Run through the handler WITH the same box: it must decrypt and send the
	// real header. A bare client bypasses the SSRF guard so loopback is allowed.
	h := httpaction.NewWithClient(s, &http.Client{}, httpaction.WithSecretBox(box))
	if _, err := h.Handle(ctx, engine.TaskPayload{RunID: "r1", StepID: "h1"}); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	if gotAuth != secretToken {
		t.Fatalf("outbound Authorization header = %q, want the decrypted secret %q", gotAuth, secretToken)
	}
}

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}
