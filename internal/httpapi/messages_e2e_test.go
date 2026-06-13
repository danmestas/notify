package httpapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/danmestas/notify/internal/domain"
)

// TestGetMessages_NoIntegrationId_ReturnsEmptyFeed verifies that GET /v1/messages
// without integrationId falls through to the in-app subscriber feed (returning
// an empty array for a fresh tenant rather than 400).
func TestGetMessages_NoIntegrationId_ReturnsEmptyFeed(t *testing.T) {
	srv, authHeader, _, _, _ := startConsoleServer(t)

	status, body := doGet(t, srv.URL+"/v1/messages", authHeader)
	if status != http.StatusOK {
		t.Fatalf("GET /v1/messages without integrationId: status=%d body=%s", status, body)
	}
	var list []any
	if err := json.Unmarshal([]byte(body), &list); err != nil {
		t.Fatalf("decode response: %v (body=%s)", err, body)
	}
	if len(list) != 0 {
		t.Errorf("expected empty in-app feed, got %d items", len(list))
	}
}

// TestGetMessages_AuthGuard verifies that both new message routes require auth.
func TestGetMessages_AuthGuard(t *testing.T) {
	srv, _, _, _, _ := startConsoleServer(t)

	routes := []string{
		"/v1/messages?integrationId=integ-123",
		"/v1/notifications/some-id/messages",
	}
	for _, path := range routes {
		status, _ := doGet(t, srv.URL+path, "")
		if status != http.StatusUnauthorized {
			t.Errorf("GET %s without auth: status=%d, want 401", path, status)
		}
	}
}

// TestGetMessages_EmptyForUnknownIntegration returns an empty array
// (not 404) for an integrationId with no messages.
func TestGetMessages_EmptyForUnknownIntegration(t *testing.T) {
	srv, authHeader, _, _, _ := startConsoleServer(t)

	status, body := doGet(t, srv.URL+"/v1/messages?integrationId=no-such-integ", authHeader)
	if status != http.StatusOK {
		t.Fatalf("GET /v1/messages?integrationId=...: status=%d body=%s", status, body)
	}
	var list []any
	if err := json.Unmarshal([]byte(body), &list); err != nil {
		t.Fatalf("decode response: %v (body=%s)", err, body)
	}
	if len(list) != 0 {
		t.Errorf("expected empty list, got %d items", len(list))
	}
}

// TestGetMessages_ByIntegrationId seeds a message with a known
// integration_id and verifies it is returned by the route.
func TestGetMessages_ByIntegrationId(t *testing.T) {
	ctx := context.Background()
	srv, authHeader, scope, _, st := startConsoleServer(t)

	// Seed a message with a specific integration_id.
	_, err := st.Messages().Create(ctx, scope, domain.Message{
		WorkflowID:     "wf1",
		NotificationID: "ntf1",
		RunID:          "run1",
		StepID:         "step1",
		SubscriberID:   "sub1",
		Channel:        "email",
		ProviderID:     "smtp",
		IntegrationID:  "integ-target",
		Status:         "sent",
	})
	if err != nil {
		t.Fatalf("seed message: %v", err)
	}
	// Also seed a message with a different integration_id (must not appear).
	_, err = st.Messages().Create(ctx, scope, domain.Message{
		WorkflowID:     "wf1",
		NotificationID: "ntf1",
		RunID:          "run2",
		StepID:         "step1",
		SubscriberID:   "sub1",
		Channel:        "email",
		ProviderID:     "smtp",
		IntegrationID:  "integ-other",
		Status:         "sent",
	})
	if err != nil {
		t.Fatalf("seed second message: %v", err)
	}

	status, body := doGet(t, srv.URL+"/v1/messages?integrationId=integ-target", authHeader)
	if status != http.StatusOK {
		t.Fatalf("GET /v1/messages?integrationId=integ-target: status=%d body=%s", status, body)
	}
	var list []map[string]any
	if err := json.Unmarshal([]byte(body), &list); err != nil {
		t.Fatalf("decode response: %v (body=%s)", err, body)
	}
	if len(list) != 1 {
		t.Fatalf("expected 1 message, got %d: %s", len(list), body)
	}
	if list[0]["IntegrationID"] != "integ-target" {
		t.Errorf("IntegrationID = %v, want integ-target", list[0]["IntegrationID"])
	}
	if list[0]["Status"] != "sent" {
		t.Errorf("Status = %v, want sent", list[0]["Status"])
	}
}

// TestGetNotificationMessages_Empty returns an empty array for a
// notification with no messages.
func TestGetNotificationMessages_Empty(t *testing.T) {
	srv, authHeader, _, _, _ := startConsoleServer(t)

	status, body := doGet(t, srv.URL+"/v1/notifications/no-such-ntf/messages", authHeader)
	if status != http.StatusOK {
		t.Fatalf("GET /v1/notifications/.../messages: status=%d body=%s", status, body)
	}
	var list []any
	if err := json.Unmarshal([]byte(body), &list); err != nil {
		t.Fatalf("decode response: %v (body=%s)", err, body)
	}
	if len(list) != 0 {
		t.Errorf("expected empty list, got %d items", len(list))
	}
}

// TestGetNotificationMessages_ReturnsScopedMessages seeds two messages for
// the same notification_id (one in-scope, one out-of-scope) and verifies
// only the in-scope message is returned.
func TestGetNotificationMessages_ReturnsScopedMessages(t *testing.T) {
	ctx := context.Background()
	srv, authHeader, scope, _, st := startConsoleServer(t)

	// In-scope message for ntf-1.
	_, err := st.Messages().Create(ctx, scope, domain.Message{
		WorkflowID:     "wf1",
		NotificationID: "ntf-1",
		RunID:          "runA",
		StepID:         "step1",
		SubscriberID:   "sub1",
		Channel:        "email",
		ProviderID:     "smtp",
		Status:         "sent",
	})
	if err != nil {
		t.Fatalf("seed in-scope message: %v", err)
	}

	// Out-of-scope message for ntf-1 (different org/env).
	otherScope := domain.Scope{OrgID: "other-org", EnvID: "other-env"}
	_, err = st.Messages().Create(ctx, otherScope, domain.Message{
		WorkflowID:     "wf1",
		NotificationID: "ntf-1",
		RunID:          "runB",
		StepID:         "step1",
		SubscriberID:   "sub2",
		Channel:        "email",
		ProviderID:     "smtp",
		Status:         "sent",
	})
	if err != nil {
		t.Fatalf("seed out-of-scope message: %v", err)
	}

	status, body := doGet(t, srv.URL+"/v1/notifications/ntf-1/messages", authHeader)
	if status != http.StatusOK {
		t.Fatalf("GET /v1/notifications/ntf-1/messages: status=%d body=%s", status, body)
	}
	var list []map[string]any
	if err := json.Unmarshal([]byte(body), &list); err != nil {
		t.Fatalf("decode response: %v (body=%s)", err, body)
	}
	if len(list) != 1 {
		t.Fatalf("expected 1 in-scope message, got %d: %s", len(list), body)
	}
	if list[0]["SubscriberID"] != "sub1" {
		t.Errorf("SubscriberID = %v, want sub1", list[0]["SubscriberID"])
	}
}
