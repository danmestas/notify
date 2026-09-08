package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/danmestas/notify/internal/audience"
	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/httpapi"
	"github.com/danmestas/notify/internal/identity"
	"github.com/danmestas/notify/internal/inboxapi"
	"github.com/danmestas/notify/internal/store"
	"github.com/danmestas/notify/internal/trigger"
)

// sessionFixture is a running API with two subscribers, each holding one
// in_app message, plus an API key and a session token minted for "alice".
type sessionFixture struct {
	srv       *httptest.Server
	apiKey    string
	aliceTok  string
	scope     domain.Scope
	aliceMsg  string
	bobMsgID  string
	storeInst *store.Store
}

func newSessionFixture(t *testing.T) sessionFixture {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "notify.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	masterKey := bytes.Repeat([]byte{0x2a}, 32)
	id := identity.New(st, masterKey)
	aud := audience.New(st)
	srv := httptest.NewServer(httpapi.New(httpapi.Deps{
		Identity: id, Audience: aud, Trigger: trigger.New(aud, st, nil),
		Inbox: inboxapi.New(st), Store: st,
	}))
	t.Cleanup(srv.Close)

	key, scope, err := id.Bootstrap(ctx, "Acme", "dev")
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	for _, sub := range []string{"alice", "bob"} {
		if _, err := st.Subscribers().Upsert(ctx, scope, domain.Subscriber{SubscriberID: sub}); err != nil {
			t.Fatalf("upsert %s: %v", sub, err)
		}
	}
	aliceMsg := seedMessage(t, st, scope, "alice", "for alice")
	bobMsg := seedMessage(t, st, scope, "bob", "for bob")

	tok, _ := id.MintSubscriberSession(scope, "alice")
	return sessionFixture{
		srv: srv, apiKey: "ApiKey " + key, aliceTok: "Bearer " + tok,
		scope: scope, aliceMsg: aliceMsg, bobMsgID: bobMsg, storeInst: st,
	}
}

func seedMessage(t *testing.T, st *store.Store, scope domain.Scope, subscriberID, content string) string {
	t.Helper()
	m, err := st.Messages().Create(context.Background(), scope, domain.Message{
		SubscriberID: subscriberID,
		Channel:      "in_app",
		Content:      content,
		Status:       "sent",
	})
	if err != nil {
		t.Fatalf("create message for %s: %v", subscriberID, err)
	}
	return m.ID
}

func req(t *testing.T, method, url, auth, body string) (int, string) {
	t.Helper()
	var r *http.Request
	var err error
	if body == "" {
		r, err = http.NewRequest(method, url, nil)
	} else {
		r, err = http.NewRequest(method, url, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
	}
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if auth != "" {
		r.Header.Set("Authorization", auth)
	}
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	buf := new(bytes.Buffer)
	_, _ = buf.ReadFrom(resp.Body)
	return resp.StatusCode, buf.String()
}

// TestSubscriberSession_CannotWidenItsOwnScope is the central guarantee of
// #332: the credential decides whose inbox is read, so a session token that
// names a different subscriber still gets its own feed. Comparing the supplied
// id instead of overriding it would make this a 403 at best and a leak at
// worst.
func TestSubscriberSession_CannotWidenItsOwnScope(t *testing.T) {
	f := newSessionFixture(t)

	status, body := req(t, http.MethodGet, f.srv.URL+"/v1/messages?subscriberId=bob", f.aliceTok, "")
	if status != 200 {
		t.Fatalf("status = %d, body = %s", status, body)
	}
	if strings.Contains(body, "for bob") {
		t.Fatalf("alice's session read bob's inbox by naming him: %s", body)
	}
	if !strings.Contains(body, "for alice") {
		t.Fatalf("alice's session did not return her own feed: %s", body)
	}
}

// TestSubscriberSession_RejectedOnNonInboxRoutes is the structural half: the
// token is only a credential where SubscriberMiddleware is mounted, so every
// other route answers 401 rather than relying on each handler to refuse it.
func TestSubscriberSession_RejectedOnNonInboxRoutes(t *testing.T) {
	f := newSessionFixture(t)

	routes := []struct{ method, path, body string }{
		{http.MethodPost, "/v1/events/trigger", `{"name":"x","to":{"subscriberId":"bob"}}`},
		{http.MethodGet, "/v1/subscribers", ""},
		{http.MethodGet, "/v1/subscribers/bob", ""},
		{http.MethodPost, "/v1/subscribers", `{"subscriberId":"mallory"}`},
		{http.MethodGet, "/v1/topics", ""},
	}
	for _, rt := range routes {
		t.Run(rt.method+" "+rt.path, func(t *testing.T) {
			status, body := req(t, rt.method, f.srv.URL+rt.path, f.aliceTok, rt.body)
			if status != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401; a subscriber session must not authenticate %s. body=%s",
					status, rt.path, body)
			}
		})
	}
}

// TestSubscriberSession_CannotListAcrossSubscribers: the integrationId branch
// of GET /v1/messages returns rows for every subscriber, so it is refused to a
// session rather than quietly narrowed.
func TestSubscriberSession_CannotListAcrossSubscribers(t *testing.T) {
	f := newSessionFixture(t)

	status, body := req(t, http.MethodGet, f.srv.URL+"/v1/messages?integrationId=int_1", f.aliceTok, "")
	if status != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 for a cross-subscriber query. body=%s", status, body)
	}
	// The same query with the tenant-wide key is unchanged.
	if status, _ := req(t, http.MethodGet, f.srv.URL+"/v1/messages?integrationId=int_1", f.apiKey, ""); status != 200 {
		t.Fatalf("api-key integrationId query = %d, want 200 (behaviour must be unchanged)", status)
	}
}

// TestSubscriberSession_CannotMarkAnotherSubscribersMessage: marking reuses
// the store's existing owner check, which answers 404 rather than 403 so the
// response cannot be used to probe for someone else's message ids.
func TestSubscriberSession_CannotMarkAnotherSubscribersMessage(t *testing.T) {
	f := newSessionFixture(t)

	status, _ := req(t, http.MethodPost,
		f.srv.URL+"/v1/messages/"+f.bobMsgID+"/read", f.aliceTok, `{"subscriberId":"bob"}`)
	if status != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: alice marked bob's message read", status)
	}
	// Her own message still marks fine.
	if status, body := req(t, http.MethodPost,
		f.srv.URL+"/v1/messages/"+f.aliceMsg+"/read", f.aliceTok, `{"subscriberId":"alice"}`); status != 204 {
		t.Fatalf("marking own message = %d, want 204. body=%s", status, body)
	}
}

// TestApiKey_InboxBehaviourUnchanged: the backend credential still addresses
// any subscriber, so moving the inbox behind a second middleware changed
// nothing for existing callers.
func TestApiKey_InboxBehaviourUnchanged(t *testing.T) {
	f := newSessionFixture(t)

	status, body := req(t, http.MethodGet, f.srv.URL+"/v1/messages?subscriberId=bob", f.apiKey, "")
	if status != 200 {
		t.Fatalf("status = %d, body = %s", status, body)
	}
	if !strings.Contains(body, "for bob") {
		t.Fatalf("api key could not read bob's feed: %s", body)
	}
}

// TestMintSession_RequiresApiKeyAndRealSubscriber: minting is a backend act,
// so a session token cannot mint another; and an unknown subscriber fails at
// mint rather than handing back a token for a permanently empty inbox.
func TestMintSession_RequiresApiKeyAndRealSubscriber(t *testing.T) {
	f := newSessionFixture(t)

	if status, _ := req(t, http.MethodPost, f.srv.URL+"/v1/subscribers/bob/session", f.aliceTok, ""); status != http.StatusUnauthorized {
		t.Errorf("minting with a session token = %d, want 401", status)
	}
	if status, _ := req(t, http.MethodPost, f.srv.URL+"/v1/subscribers/nobody/session", f.apiKey, ""); status != http.StatusNotFound {
		t.Errorf("minting for an unknown subscriber = %d, want 404", status)
	}

	status, body := req(t, http.MethodPost, f.srv.URL+"/v1/subscribers/alice/session", f.apiKey, "")
	if status != 200 {
		t.Fatalf("mint = %d, body = %s", status, body)
	}
	var out struct {
		Token     string `json:"token"`
		ExpiresAt string `json:"expiresAt"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("decode mint response: %v", err)
	}
	if out.Token == "" || out.ExpiresAt == "" {
		t.Fatalf("mint response missing fields: %s", body)
	}
	// The minted token works on the inbox.
	if status, _ := req(t, http.MethodGet, f.srv.URL+"/v1/messages", "Bearer "+out.Token, ""); status != 200 {
		t.Errorf("minted token rejected by the inbox: %d", status)
	}
}
