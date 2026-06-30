package inboxapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/danmestas/notify/internal/domain"
)

// TestListMessages_ReturnsExtras proves the inbox API surfaces the in_app rich
// fields snapshot stored on the message Extras blob (avatar/primaryAction/data).
func TestListMessages_ReturnsExtras(t *testing.T) {
	s := newTestStore(t)
	scope := domain.Scope{OrgID: "org1", EnvID: "envA"}

	extras := json.RawMessage(`{"avatar":"https://x/a.png","primaryAction":{"label":"Open","redirect":{"url":"/foo"}},"data":{"orderId":"99"}}`)
	if _, err := s.Messages().Create(context.Background(), scope, domain.Message{
		SubscriberID: "u1",
		Channel:      "in_app",
		Content:      "hi",
		Subject:      "Greetings",
		Status:       "sent",
		Extras:       extras,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	svc := New(s)
	rec := doGet(t, svc, "/v1/messages?subscriberId=u1&channel=in_app", &scope)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	// Raw body must carry the nested extras object.
	if !strings.Contains(rec.Body.String(), `"primaryAction"`) {
		t.Errorf("response missing extras.primaryAction; body=%s", rec.Body.String())
	}

	var got []struct {
		Extras struct {
			Avatar        string `json:"avatar"`
			PrimaryAction *struct {
				Label string `json:"label"`
			} `json:"primaryAction"`
			Data struct {
				OrderID string `json:"orderId"`
			} `json:"data"`
		} `json:"extras"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v; body=%s", err, rec.Body.String())
	}
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1", len(got))
	}
	if got[0].Extras.PrimaryAction == nil || got[0].Extras.PrimaryAction.Label != "Open" {
		t.Errorf("extras.primaryAction.label not returned: %+v", got[0].Extras.PrimaryAction)
	}
	if got[0].Extras.Avatar != "https://x/a.png" {
		t.Errorf("extras.avatar = %q", got[0].Extras.Avatar)
	}
	if got[0].Extras.Data.OrderID != "99" {
		t.Errorf("extras.data.orderId = %q, want 99", got[0].Extras.Data.OrderID)
	}
}

// TestListMessages_LegacyEmptyExtras proves a message with no rich fields
// marshals extras as {} (not null) so inbox clients get a stable shape.
func TestListMessages_LegacyEmptyExtras(t *testing.T) {
	s := newTestStore(t)
	scope := domain.Scope{OrgID: "org1", EnvID: "envA"}
	if _, err := s.Messages().Create(context.Background(), scope, domain.Message{
		SubscriberID: "u1", Channel: "in_app", Status: "sent",
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	svc := New(s)
	rec := doGet(t, svc, "/v1/messages?subscriberId=u1&channel=in_app", &scope)
	if !strings.Contains(rec.Body.String(), `"extras":{}`) {
		t.Errorf("legacy message extras not {} ; body=%s", rec.Body.String())
	}
}
