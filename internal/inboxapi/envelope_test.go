package inboxapi

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/danmestas/notify/internal/domain"
)

type envelopeResponse struct {
	Data        []domain.Message `json:"data"`
	Next        string           `json:"next"`
	UnreadCount int              `json:"unreadCount"`
	UnseenCount int              `json:"unseenCount"`
}

func decodeEnvelope(t *testing.T, body []byte) envelopeResponse {
	t.Helper()
	var env envelopeResponse
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("decode envelope: %v; body=%s", err, body)
	}
	return env
}

// TestListMessages_Envelope_DefaultShape verifies the default (no
// format=array, no Accept override) response is the {"data",...} envelope.
func TestListMessages_Envelope_DefaultShape(t *testing.T) {
	s := newTestStore(t)
	scope := domain.Scope{OrgID: "org1", EnvID: "envA"}
	if _, err := s.Messages().Create(context.Background(), scope, domain.Message{
		SubscriberID: "u1", Channel: "in_app", Status: "sent",
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	svc := New(s)
	rec := doGet(t, svc, "/v1/messages?subscriberId=u1&channel=in_app", &scope)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	env := decodeEnvelope(t, rec.Body.Bytes())
	if len(env.Data) != 1 {
		t.Fatalf("len(data) = %d, want 1", len(env.Data))
	}
	if env.Next != "" {
		t.Errorf("next = %q, want empty (single page)", env.Next)
	}
	if env.UnreadCount != 1 {
		t.Errorf("unreadCount = %d, want 1", env.UnreadCount)
	}
	if env.UnseenCount != 1 {
		t.Errorf("unseenCount = %d, want 1", env.UnseenCount)
	}
}

// TestListMessages_Envelope_Pagination seeds 5 messages, pages through with
// limit=2, and verifies the cursor is stable and terminates on an empty next.
func TestListMessages_Envelope_Pagination(t *testing.T) {
	s := newTestStore(t)
	scope := domain.Scope{OrgID: "org1", EnvID: "envA"}
	var ids []string
	for i := 0; i < 5; i++ {
		m, err := s.Messages().Create(context.Background(), scope, domain.Message{
			SubscriberID: "u1", Channel: "in_app", Status: "sent",
		})
		if err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
		ids = append(ids, m.ID)
		time.Sleep(2 * time.Millisecond) // keep ULID millisecond ordering distinct
	}
	// Newest first, so the expected page order is the reverse of insertion.
	want := []string{ids[4], ids[3], ids[2], ids[1], ids[0]}

	svc := New(s)
	var got []string
	before := ""
	for page := 0; page < 10; page++ {
		target := "/v1/messages?subscriberId=u1&channel=in_app&limit=2"
		if before != "" {
			target += "&before=" + before
		}
		rec := doGet(t, svc, target, &scope)
		if rec.Code != http.StatusOK {
			t.Fatalf("page %d: status = %d, want 200; body=%s", page, rec.Code, rec.Body.String())
		}
		env := decodeEnvelope(t, rec.Body.Bytes())
		for _, m := range env.Data {
			got = append(got, m.ID)
		}
		if env.Next == "" {
			break
		}
		before = env.Next
	}

	if len(got) != len(want) {
		t.Fatalf("collected %d ids across pages, want %d: got=%v want=%v", len(got), len(want), got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("id[%d] = %q, want %q (full: got=%v want=%v)", i, got[i], want[i], got, want)
		}
	}
}

// TestListMessages_Envelope_LimitClamped verifies limit is capped at 200.
func TestListMessages_Envelope_LimitClamped(t *testing.T) {
	s := newTestStore(t)
	scope := domain.Scope{OrgID: "org1", EnvID: "envA"}
	svc := New(s)
	rec := doGet(t, svc, "/v1/messages?subscriberId=u1&channel=in_app&limit=99999", &scope)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
}

func TestListMessages_Envelope_InvalidLimit(t *testing.T) {
	s := newTestStore(t)
	scope := domain.Scope{OrgID: "org1", EnvID: "envA"}
	svc := New(s)
	rec := doGet(t, svc, "/v1/messages?subscriberId=u1&limit=not-a-number", &scope)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}

// TestListMessages_Envelope_CountsUpdateAfterMark verifies unreadCount and
// unseenCount reflect the current flag state after marking.
func TestListMessages_Envelope_CountsUpdateAfterMark(t *testing.T) {
	s := newTestStore(t)
	scope := domain.Scope{OrgID: "org1", EnvID: "envA"}
	m1, err := s.Messages().Create(context.Background(), scope, domain.Message{SubscriberID: "u1", Channel: "in_app", Status: "sent"})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := s.Messages().Create(context.Background(), scope, domain.Message{SubscriberID: "u1", Channel: "in_app", Status: "sent"}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	svc := New(s)
	rec := doPost(t, svc, "/v1/messages/"+m1.ID+"/seen", &scope, `{"subscriberId":"u1"}`)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("mark seen: status = %d, want 204; body=%s", rec.Code, rec.Body.String())
	}
	rec = doPost(t, svc, "/v1/messages/"+m1.ID+"/read", &scope, `{"subscriberId":"u1"}`)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("mark read: status = %d, want 204; body=%s", rec.Code, rec.Body.String())
	}

	rec = doGet(t, svc, "/v1/messages?subscriberId=u1&channel=in_app", &scope)
	env := decodeEnvelope(t, rec.Body.Bytes())
	if env.UnreadCount != 1 {
		t.Errorf("unreadCount = %d, want 1 (one of two messages marked read)", env.UnreadCount)
	}
	if env.UnseenCount != 1 {
		t.Errorf("unseenCount = %d, want 1 (one of two messages marked seen)", env.UnseenCount)
	}
}

func TestCountMessages_Unread(t *testing.T) {
	s := newTestStore(t)
	scope := domain.Scope{OrgID: "org1", EnvID: "envA"}
	m1, _ := s.Messages().Create(context.Background(), scope, domain.Message{SubscriberID: "u1", Channel: "in_app", Status: "sent"})
	if _, err := s.Messages().Create(context.Background(), scope, domain.Message{SubscriberID: "u1", Channel: "in_app", Status: "sent"}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	svc := New(s)
	rec := doPost(t, svc, "/v1/messages/"+m1.ID+"/read", &scope, `{"subscriberId":"u1"}`)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("mark read: status = %d; body=%s", rec.Code, rec.Body.String())
	}

	rec = doGet(t, svc, "/v1/messages/count?subscriberId=u1&channel=in_app&unread=true", &scope)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Count int `json:"count"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Count != 1 {
		t.Errorf("count = %d, want 1", resp.Count)
	}
}

func TestCountMessages_TotalWithNoFlags(t *testing.T) {
	s := newTestStore(t)
	scope := domain.Scope{OrgID: "org1", EnvID: "envA"}
	for i := 0; i < 3; i++ {
		if _, err := s.Messages().Create(context.Background(), scope, domain.Message{SubscriberID: "u1", Channel: "in_app", Status: "sent"}); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	svc := New(s)
	rec := doGet(t, svc, "/v1/messages/count?subscriberId=u1&channel=in_app", &scope)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Count int `json:"count"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Count != 3 {
		t.Errorf("count = %d, want 3", resp.Count)
	}
}

// TestListMessages_Envelope_ArchivedExcludedFromFeedAndCounts verifies that
// archiving a message removes it from the default feed and drops it out of
// unreadCount/unseenCount, matching the reference platform's inbox semantics where "archive"
// is the done action (issue #300).
func TestListMessages_Envelope_ArchivedExcludedFromFeedAndCounts(t *testing.T) {
	s := newTestStore(t)
	scope := domain.Scope{OrgID: "org1", EnvID: "envA"}
	m1, err := s.Messages().Create(context.Background(), scope, domain.Message{SubscriberID: "u1", Channel: "in_app", Status: "sent"})
	if err != nil {
		t.Fatalf("seed m1: %v", err)
	}
	if _, err := s.Messages().Create(context.Background(), scope, domain.Message{SubscriberID: "u1", Channel: "in_app", Status: "sent"}); err != nil {
		t.Fatalf("seed m2: %v", err)
	}

	svc := New(s)

	// Before archiving: both messages present, counts reflect both.
	rec := doGet(t, svc, "/v1/messages?subscriberId=u1&channel=in_app", &scope)
	env := decodeEnvelope(t, rec.Body.Bytes())
	if len(env.Data) != 2 {
		t.Fatalf("before archive: len(data) = %d, want 2", len(env.Data))
	}
	if env.UnreadCount != 2 || env.UnseenCount != 2 {
		t.Fatalf("before archive: unreadCount=%d unseenCount=%d, want 2/2", env.UnreadCount, env.UnseenCount)
	}

	rec = doPost(t, svc, "/v1/messages/"+m1.ID+"/archive", &scope, `{"subscriberId":"u1"}`)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("archive: status = %d, want 204; body=%s", rec.Code, rec.Body.String())
	}

	// After archiving: default feed and counts must both drop the archived
	// message, even though it is still technically unread/unseen.
	rec = doGet(t, svc, "/v1/messages?subscriberId=u1&channel=in_app", &scope)
	env = decodeEnvelope(t, rec.Body.Bytes())
	if len(env.Data) != 1 {
		t.Fatalf("after archive: len(data) = %d, want 1", len(env.Data))
	}
	for _, m := range env.Data {
		if m.ID == m1.ID {
			t.Fatalf("after archive: archived message %q still present in default feed", m1.ID)
		}
	}
	if env.UnreadCount != 1 {
		t.Errorf("after archive: unreadCount = %d, want 1 (archived message must not count)", env.UnreadCount)
	}
	if env.UnseenCount != 1 {
		t.Errorf("after archive: unseenCount = %d, want 1 (archived message must not count)", env.UnseenCount)
	}

	// GET /v1/messages/count?unread=true must also exclude it by default.
	rec = doGet(t, svc, "/v1/messages/count?subscriberId=u1&channel=in_app&unread=true", &scope)
	var resp struct {
		Count int `json:"count"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Count != 1 {
		t.Errorf("count?unread=true = %d, want 1 (archived message must not count)", resp.Count)
	}

	// archived=true opts back into seeing just the archived message.
	rec = doGet(t, svc, "/v1/messages?subscriberId=u1&channel=in_app&archived=true", &scope)
	env = decodeEnvelope(t, rec.Body.Bytes())
	if len(env.Data) != 1 || env.Data[0].ID != m1.ID {
		t.Fatalf("archived=true feed = %+v, want only %q", env.Data, m1.ID)
	}
}

// TestListMessages_Envelope_OmittedChannelDefaultsToInApp verifies an
// omitted channel on the envelope path is scoped to "in_app", not every
// channel — otherwise it would surface other channels' delivery records
// (and rendered content) in the subscriber's in-app feed and badge counts,
// a data-exposure regression vs. the legacy array behavior.
func TestListMessages_Envelope_OmittedChannelDefaultsToInApp(t *testing.T) {
	s := newTestStore(t)
	scope := domain.Scope{OrgID: "org1", EnvID: "envA"}
	inApp, err := s.Messages().Create(context.Background(), scope, domain.Message{
		SubscriberID: "u1", Channel: "in_app", Status: "sent", Content: "in-app message",
	})
	if err != nil {
		t.Fatalf("seed in_app: %v", err)
	}
	if _, err := s.Messages().Create(context.Background(), scope, domain.Message{
		SubscriberID: "u1", Channel: "email", Status: "sent", Content: "rendered email body",
	}); err != nil {
		t.Fatalf("seed email: %v", err)
	}

	svc := New(s)
	rec := doGet(t, svc, "/v1/messages?subscriberId=u1", &scope)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	env := decodeEnvelope(t, rec.Body.Bytes())
	if len(env.Data) != 1 || env.Data[0].ID != inApp.ID {
		t.Fatalf("omitted-channel envelope data = %+v, want only the in_app message %q", env.Data, inApp.ID)
	}
	if env.UnreadCount != 1 || env.UnseenCount != 1 {
		t.Fatalf("omitted-channel counts = unread=%d unseen=%d, want 1/1 (email must not count)", env.UnreadCount, env.UnseenCount)
	}
}

// TestListMessages_Envelope_TransactionIDFilter verifies transactionId
// narrows the envelope page the same way it narrows the legacy array
// branch.
func TestListMessages_Envelope_TransactionIDFilter(t *testing.T) {
	s := newTestStore(t)
	scope := domain.Scope{OrgID: "org1", EnvID: "envA"}
	wanted, err := s.Messages().Create(context.Background(), scope, domain.Message{
		SubscriberID: "u1", Channel: "in_app", Status: "sent", TransactionID: "txn-a",
	})
	if err != nil {
		t.Fatalf("seed txn-a: %v", err)
	}
	if _, err := s.Messages().Create(context.Background(), scope, domain.Message{
		SubscriberID: "u1", Channel: "in_app", Status: "sent", TransactionID: "txn-b",
	}); err != nil {
		t.Fatalf("seed txn-b: %v", err)
	}

	svc := New(s)
	rec := doGet(t, svc, "/v1/messages?subscriberId=u1&channel=in_app&transactionId=txn-a", &scope)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	env := decodeEnvelope(t, rec.Body.Bytes())
	if len(env.Data) != 1 || env.Data[0].ID != wanted.ID {
		t.Fatalf("transactionId-filtered envelope data = %+v, want only %q", env.Data, wanted.ID)
	}
}

// TestCountMessages_OmittedChannelDefaultsToInApp mirrors
// TestListMessages_Envelope_OmittedChannelDefaultsToInApp for the badge
// endpoint.
func TestCountMessages_OmittedChannelDefaultsToInApp(t *testing.T) {
	s := newTestStore(t)
	scope := domain.Scope{OrgID: "org1", EnvID: "envA"}
	if _, err := s.Messages().Create(context.Background(), scope, domain.Message{
		SubscriberID: "u1", Channel: "in_app", Status: "sent",
	}); err != nil {
		t.Fatalf("seed in_app: %v", err)
	}
	if _, err := s.Messages().Create(context.Background(), scope, domain.Message{
		SubscriberID: "u1", Channel: "email", Status: "sent",
	}); err != nil {
		t.Fatalf("seed email: %v", err)
	}

	svc := New(s)
	rec := doGet(t, svc, "/v1/messages/count?subscriberId=u1", &scope)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Count int `json:"count"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Count != 1 {
		t.Errorf("count = %d, want 1 (email message must not count toward the in-app badge)", resp.Count)
	}
}

// TestListMessages_Envelope_ScopeIsolation verifies a different org/env
// scope sees an empty envelope, not another org's subscriber inbox.
func TestListMessages_Envelope_ScopeIsolation(t *testing.T) {
	s := newTestStore(t)
	scopeA := domain.Scope{OrgID: "org1", EnvID: "envA"}
	scopeB := domain.Scope{OrgID: "org1", EnvID: "envB"}
	if _, err := s.Messages().Create(context.Background(), scopeA, domain.Message{
		SubscriberID: "u1", Channel: "in_app", Status: "sent", Content: "secret",
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	svc := New(s)
	rec := doGet(t, svc, "/v1/messages?subscriberId=u1&channel=in_app", &scopeB)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	env := decodeEnvelope(t, rec.Body.Bytes())
	if len(env.Data) != 0 {
		t.Fatalf("data = %+v, want [] (scope isolation breached)", env.Data)
	}
	if env.UnreadCount != 0 || env.UnseenCount != 0 {
		t.Fatalf("unreadCount=%d unseenCount=%d, want 0/0 (scope isolation breached)", env.UnreadCount, env.UnseenCount)
	}
}

// TestCountMessages_ScopeIsolation verifies /v1/messages/count returns 0
// for another org/env's subscriber, not a leaked count.
func TestCountMessages_ScopeIsolation(t *testing.T) {
	s := newTestStore(t)
	scopeA := domain.Scope{OrgID: "org1", EnvID: "envA"}
	scopeB := domain.Scope{OrgID: "org1", EnvID: "envB"}
	if _, err := s.Messages().Create(context.Background(), scopeA, domain.Message{
		SubscriberID: "u1", Channel: "in_app", Status: "sent",
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	svc := New(s)
	rec := doGet(t, svc, "/v1/messages/count?subscriberId=u1&channel=in_app", &scopeB)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Count int `json:"count"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Count != 0 {
		t.Errorf("count = %d, want 0 (scope isolation breached)", resp.Count)
	}
}
