package httpapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"sort"
	"testing"

	"github.com/danmestas/notify/internal/domain"
)

// TestGetMessages_NoIntegrationId_ReturnsEmptyFeed verifies that GET /v1/messages
// without integrationId falls through to the in-app subscriber feed, returning
// the paginated envelope (empty data for a fresh tenant) rather than 400.
func TestGetMessages_NoIntegrationId_ReturnsEmptyFeed(t *testing.T) {
	srv, authHeader, _, _, _ := startConsoleServer(t)

	status, body := doGet(t, srv.URL+"/v1/messages", authHeader)
	if status != http.StatusOK {
		t.Fatalf("GET /v1/messages without integrationId: status=%d body=%s", status, body)
	}
	var env struct {
		Data        []any `json:"data"`
		UnreadCount int   `json:"unreadCount"`
		UnseenCount int   `json:"unseenCount"`
	}
	if err := json.Unmarshal([]byte(body), &env); err != nil {
		t.Fatalf("decode response: %v (body=%s)", err, body)
	}
	if len(env.Data) != 0 {
		t.Errorf("expected empty in-app feed, got %d items", len(env.Data))
	}
}

// TestGetMessages_LegacyArrayFormat verifies the ?format=array escape hatch
// still returns the bare array shape for callers depending on it.
func TestGetMessages_LegacyArrayFormat(t *testing.T) {
	srv, authHeader, _, _, _ := startConsoleServer(t)

	status, body := doGet(t, srv.URL+"/v1/messages?format=array", authHeader)
	if status != http.StatusOK {
		t.Fatalf("GET /v1/messages?format=array: status=%d body=%s", status, body)
	}
	var list []any
	if err := json.Unmarshal([]byte(body), &list); err != nil {
		t.Fatalf("decode response as bare array: %v (body=%s)", err, body)
	}
	if len(list) != 0 {
		t.Errorf("expected empty legacy array, got %d items", len(list))
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
	if list[0]["integrationId"] != "integ-target" {
		t.Errorf("integrationId = %v, want integ-target", list[0]["integrationId"])
	}
	if list[0]["status"] != "sent" {
		t.Errorf("status = %v, want sent", list[0]["status"])
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
	if list[0]["subscriberId"] != "sub1" {
		t.Errorf("subscriberId = %v, want sub1", list[0]["subscriberId"])
	}
}

// wantMessageKeys is the exact key set of a message on the wire (#157): the
// camelCase DTO, with no Go field names and no scope ids.
var wantMessageKeys = []string{
	"archived", "channel", "content", "createdAt", "errorClass", "errorText",
	"extras", "id", "integrationId", "notificationId", "providerId",
	"providerMessageId", "read", "runId", "seen", "skipReason", "status",
	"stepId", "subject", "subscriberId", "transactionId", "workflowId",
	"workflowIdentifier",
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// TestGetNotificationMessages_CamelCaseShape pins the exact JSON keys, and
// that a skipped message carries skipReason and a failed one errorClass and
// errorText; GET /v1/messages emits the same shape.
func TestGetNotificationMessages_CamelCaseShape(t *testing.T) {
	ctx := context.Background()
	srv, authHeader, scope, _, st := startConsoleServer(t)

	for _, m := range []domain.Message{
		{
			NotificationID: "ntf-9", TransactionID: "tx-9", WorkflowID: "wf1", RunID: "r1", StepID: "s1",
			SubscriberID: "sub1", Channel: "sms", ProviderID: "twilio", IntegrationID: "integ-1",
			Status: "skipped", SkipReason: "no_verified_phone",
		},
		{
			NotificationID: "ntf-9", TransactionID: "tx-9", WorkflowID: "wf1", RunID: "r1", StepID: "s2",
			SubscriberID: "sub1", Channel: "email", ProviderID: "smtp", IntegrationID: "integ-2",
			Status: "error", ErrorClass: "auth", ErrorText: "smtp: 535 bad credentials",
		},
	} {
		if _, err := st.Messages().Create(ctx, scope, m); err != nil {
			t.Fatalf("seed message: %v", err)
		}
	}

	status, body := doGet(t, srv.URL+"/v1/notifications/ntf-9/messages", authHeader)
	if status != http.StatusOK {
		t.Fatalf("status=%d body=%s", status, body)
	}
	var list []map[string]any
	if err := json.Unmarshal([]byte(body), &list); err != nil {
		t.Fatalf("decode: %v (body=%s)", err, body)
	}
	if len(list) != 2 {
		t.Fatalf("want 2 messages, got %d: %s", len(list), body)
	}
	byStatus := map[string]map[string]any{}
	for _, m := range list {
		if got := sortedKeys(m); !reflect.DeepEqual(got, wantMessageKeys) {
			t.Errorf("keys = %v, want %v", got, wantMessageKeys)
		}
		byStatus[m["status"].(string)] = m
	}
	if got := byStatus["skipped"]["skipReason"]; got != "no_verified_phone" {
		t.Errorf("skipReason = %v", got)
	}
	if got := byStatus["error"]["errorClass"]; got != "auth" {
		t.Errorf("errorClass = %v", got)
	}
	if got := byStatus["error"]["errorText"]; got != "smtp: 535 bad credentials" {
		t.Errorf("errorText = %v", got)
	}
	if byStatus["error"]["notificationId"] != "ntf-9" || byStatus["error"]["transactionId"] != "tx-9" {
		t.Errorf("ids not populated: %v", byStatus["error"])
	}

	// The messages list emits the same shape.
	status, body = doGet(t, srv.URL+"/v1/messages?format=array&subscriberId=sub1&channel=sms", authHeader)
	if status != http.StatusOK {
		t.Fatalf("list status=%d body=%s", status, body)
	}
	var feed []map[string]any
	if err := json.Unmarshal([]byte(body), &feed); err != nil || len(feed) != 1 {
		t.Fatalf("list decode: %v len=%d body=%s", err, len(feed), body)
	}
	if got := sortedKeys(feed[0]); !reflect.DeepEqual(got, wantMessageKeys) {
		t.Errorf("list keys = %v, want %v", got, wantMessageKeys)
	}
}
