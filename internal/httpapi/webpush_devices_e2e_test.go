package httpapi_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/danmestas/notify/internal/audience"
	"github.com/danmestas/notify/internal/connectivity"
	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/httpapi"
	"github.com/danmestas/notify/internal/identity"
	"github.com/danmestas/notify/internal/inboxapi"
	"github.com/danmestas/notify/internal/store"
	"github.com/danmestas/notify/internal/trigger"
)

// recordingPublisher captures the events published to the delivery stream.
type recordingPublisher struct {
	mu     sync.Mutex
	events []map[string]any
}

func (p *recordingPublisher) PublishDelivery(_ string, data []byte) error {
	var ev map[string]any
	if err := json.Unmarshal(data, &ev); err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.events = append(p.events, ev)
	return nil
}

func (p *recordingPublisher) all() []map[string]any {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]map[string]any(nil), p.events...)
}

func startWebPushDevicesServer(t *testing.T) (*httptest.Server, string, domain.Scope, *identity.Service, *recordingPublisher) {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "notify.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	pub := &recordingPublisher{}
	masterKey := bytes.Repeat([]byte{0x2a}, 32)
	id := identity.New(st, masterKey)
	aud := audience.New(st).WithEventPublisher(pub)
	conn := connectivity.New(st, masterKey, connectivity.WithPublicBaseURL("https://notify.example.com"))
	key, scope, err := id.Bootstrap(ctx, "Acme", "dev")
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	srv := httptest.NewServer(httpapi.New(httpapi.Deps{Identity: id, Audience: aud, Trigger: trigger.New(aud, st, nil), Inbox: inboxapi.New(st), Connectivity: conn}))
	t.Cleanup(srv.Close)
	return srv, "ApiKey " + key, scope, id, pub
}

func hashOf(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

type deviceDTO struct {
	ID             string `json:"id"`
	EndpointHash   string `json:"endpointHash"`
	CreatedAt      string `json:"createdAt"`
	LastSuccessAt  string `json:"lastSuccessAt"`
	LastErrorClass string `json:"lastErrorClass"`
}

func listDevices(t *testing.T, base, sub, auth string) []deviceDTO {
	t.Helper()
	status, body := doGet(t, base+"/v1/subscribers/"+sub+"/credentials/webpush", auth)
	if status != http.StatusOK {
		t.Fatalf("GET devices: status=%d body=%s", status, body)
	}
	if strings.Contains(body, "https://") || strings.Contains(body, "p256dh") {
		t.Fatalf("device list leaked the endpoint or keys: %s", body)
	}
	var resp struct {
		Data []deviceDTO `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return resp.Data
}

func registerDevice(t *testing.T, base, sub, auth, endpoint string) {
	t.Helper()
	body := `{"providerId":"webpush","credentials":{"endpoint":"` + endpoint + `","keys":{"p256dh":"p","auth":"a"}}}`
	if status, b := doPut(t, base+"/v1/subscribers/"+sub+"/credentials", auth, body); status != http.StatusNoContent {
		t.Fatalf("PUT device: status=%d body=%s", status, b)
	}
}

// TestWebPushDevices_ListAndDeleteByID is #172: a client lists a subscriber's
// devices as endpoint hashes, deletes one by id, and the webhook event fires
// once the row is gone.
func TestWebPushDevices_ListAndDeleteByID(t *testing.T) {
	srv, auth, scope, _, pub := startWebPushDevicesServer(t)
	const epA, epB = "https://fcm.googleapis.com/fcm/send/dev-a", "https://web.push.apple.com/dev-b"
	registerDevice(t, srv.URL, "u1", auth, epA)
	registerDevice(t, srv.URL, "u1", auth, epB)

	devs := listDevices(t, srv.URL, "u1", auth)
	if len(devs) != 2 {
		t.Fatalf("devices = %+v, want 2", devs)
	}
	if devs[0].EndpointHash != hashOf(epA) || devs[1].EndpointHash != hashOf(epB) {
		t.Fatalf("hashes = %q, %q", devs[0].EndpointHash, devs[1].EndpointHash)
	}
	if devs[0].ID == "" || devs[0].CreatedAt == "" || devs[0].LastSuccessAt != "" || devs[0].LastErrorClass != "" {
		t.Fatalf("device fields = %+v", devs[0])
	}

	status, body := doDelete(t, srv.URL+"/v1/subscribers/u1/credentials/webpush/"+devs[0].ID, auth)
	if status != http.StatusNoContent {
		t.Fatalf("DELETE by id: status=%d body=%s", status, body)
	}
	if left := listDevices(t, srv.URL, "u1", auth); len(left) != 1 || left[0].ID != devs[1].ID {
		t.Fatalf("after delete = %+v", left)
	}
	evs := pub.all()
	if len(evs) != 1 {
		t.Fatalf("events = %v, want 1", evs)
	}
	want := map[string]any{
		"orgId": scope.OrgID, "envId": scope.EnvID, "type": "webpush.subscription_removed",
		"subscriberId": "u1", "deviceId": devs[0].ID, "endpointHash": hashOf(epA), "reason": "deleted",
	}
	for k, v := range want {
		if evs[0][k] != v {
			t.Errorf("event[%q] = %v, want %v", k, evs[0][k], v)
		}
	}

	// A second delete is a no-op: 404, and no second event.
	if status, _ := doDelete(t, srv.URL+"/v1/subscribers/u1/credentials/webpush/"+devs[0].ID, auth); status != http.StatusNotFound {
		t.Fatalf("repeat DELETE status = %d, want 404", status)
	}
	if n := len(pub.all()); n != 1 {
		t.Fatalf("events after no-op delete = %d, want 1", n)
	}
}

// TestWebPushDevices_DeleteByHashAndEndpoint covers the other two ways to name
// a device: its endpoint hash, and the existing ?endpoint= route, which now
// also fires the event.
func TestWebPushDevices_DeleteByHashAndEndpoint(t *testing.T) {
	srv, auth, _, _, pub := startWebPushDevicesServer(t)
	const epA, epB = "https://fcm.googleapis.com/fcm/send/dev-a", "https://web.push.apple.com/dev-b"
	if status, b := doPost(t, srv.URL+"/v1/subscribers", auth, `{"subscriberId":"u2"}`); status >= 300 {
		t.Fatalf("create subscriber: status=%d body=%s", status, b)
	}
	registerDevice(t, srv.URL, "u2", auth, epA)
	registerDevice(t, srv.URL, "u2", auth, epB)

	if status, b := doDelete(t, srv.URL+"/v1/subscribers/u2/credentials/webpush/"+hashOf(epA), auth); status != http.StatusNoContent {
		t.Fatalf("DELETE by hash: status=%d body=%s", status, b)
	}
	if status, b := doDelete(t, srv.URL+"/v1/subscribers/u2/credentials/webpush?endpoint="+url.QueryEscape(epB), auth); status != http.StatusNoContent {
		t.Fatalf("DELETE by endpoint: status=%d body=%s", status, b)
	}
	evs := pub.all()
	if len(evs) != 2 || evs[0]["endpointHash"] != hashOf(epA) || evs[1]["endpointHash"] != hashOf(epB) || evs[1]["reason"] != "deleted" {
		t.Fatalf("events = %v", evs)
	}
	if got := listDevices(t, srv.URL, "u2", auth); len(got) != 0 {
		t.Fatalf("devices after deleting both = %+v", got)
	}
}

// TestWebPushDevices_ScopeAndUnknownSubscriber: another environment's key sees
// 404 for list and delete, as does a subscriber that does not exist.
func TestWebPushDevices_ScopeAndUnknownSubscriber(t *testing.T) {
	srv, authA, _, id, pub := startWebPushDevicesServer(t)
	keyB, _, err := id.Bootstrap(context.Background(), "Other", "dev")
	if err != nil {
		t.Fatalf("Bootstrap B: %v", err)
	}
	authB := "ApiKey " + keyB
	registerDevice(t, srv.URL, "u3", authA, "https://fcm.googleapis.com/fcm/send/dev-a")
	devs := listDevices(t, srv.URL, "u3", authA)

	if status, _ := doGet(t, srv.URL+"/v1/subscribers/u3/credentials/webpush", authB); status != http.StatusNotFound {
		t.Fatalf("cross-scope GET status = %d, want 404", status)
	}
	if status, _ := doDelete(t, srv.URL+"/v1/subscribers/u3/credentials/webpush/"+devs[0].ID, authB); status != http.StatusNotFound {
		t.Fatalf("cross-scope DELETE status = %d, want 404", status)
	}
	if status, _ := doGet(t, srv.URL+"/v1/subscribers/nobody/credentials/webpush", authA); status != http.StatusNotFound {
		t.Fatalf("unknown subscriber GET status = %d, want 404", status)
	}
	if len(listDevices(t, srv.URL, "u3", authA)) != 1 || len(pub.all()) != 0 {
		t.Fatalf("cross-scope delete must not remove the device or fire an event")
	}
}

// TestWebPushDevices_KnownSubscriberWithNoDevices is an empty list, not 404.
func TestWebPushDevices_KnownSubscriberWithNoDevices(t *testing.T) {
	srv, auth, _, _, _ := startWebPushDevicesServer(t)
	status, body := doPost(t, srv.URL+"/v1/subscribers", auth, `{"subscriberId":"known","email":"k@example.com"}`)
	if status >= 300 {
		t.Fatalf("create subscriber: status=%d body=%s", status, body)
	}
	status, body = doGet(t, srv.URL+"/v1/subscribers/known/credentials/webpush", auth)
	if status != http.StatusOK || strings.TrimSpace(body) != `{"data":[]}` {
		t.Fatalf("status=%d body=%s, want 200 {\"data\":[]}", status, body)
	}
}
