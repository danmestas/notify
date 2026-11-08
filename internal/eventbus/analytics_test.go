package eventbus

import (
	"context"
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/danmestas/notify/internal/timestamp"
	"github.com/nats-io/nats.go/jetstream"
)

// analyticsAllowList is every key the stored event may carry. A new field on
// AnalyticsEvent fails the test until it is added here on purpose, after
// checking it holds no message content or personal data.
var analyticsAllowList = []string{
	"at", "channel", "createdAt", "envId", "errorClass", "integrationId",
	"messageId", "notificationId", "orgId", "providerId", "runId", "status",
	"stepId", "subscriberId", "transactionId", "v", "workflowId",
}

func TestAnalyticsEvent_FieldAllowList(t *testing.T) {
	var got []string
	typ := reflect.TypeOf(AnalyticsEvent{})
	for i := 0; i < typ.NumField(); i++ {
		name, _, _ := strings.Cut(typ.Field(i).Tag.Get("json"), ",")
		if name == "" || name == "-" {
			t.Fatalf("field %s has no json name", typ.Field(i).Name)
		}
		got = append(got, name)
	}
	sort.Strings(got)
	if !reflect.DeepEqual(got, analyticsAllowList) {
		t.Fatalf("AnalyticsEvent json fields = %v, want exactly %v", got, analyticsAllowList)
	}
}

const analyticsPayload = `{"ID":"m1","OrgID":"o1","EnvID":"env1","WorkflowID":"w1",` +
	`"NotificationID":"n1","RunID":"r1","StepID":"s1","SubscriberID":"sub1",` +
	`"Channel":"email","ProviderID":"smtp","IntegrationID":"i1","TransactionID":"t1",` +
	`"Status":"error","ErrorClass":"auth","CreatedAt":"2026-01-02T03:04:05Z",` +
	`"Subject":"SECRET-SUBJECT-xyzzy","Content":"SECRET-BODY-xyzzy",` +
	`"ErrorText":"smtp to dan@example.com failed","SkipReason":"pref off"}`

func mustStream(t *testing.T, natsURL, name string) jetstream.Stream {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	st, err := inspectJS(t, natsURL).Stream(ctx, name)
	if err != nil {
		t.Fatalf("Stream %s: %v", name, err)
	}
	return st
}

func connectBus(t *testing.T, natsURL string) (*Bus, context.Context) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)
	bus, err := Connect(ctx, natsURL)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close() })
	return bus, ctx
}

func TestPublishDelivery_PublishesMetadataOnlyAnalyticsEvent(t *testing.T) {
	natsURL := startSidecar(t)
	bus, ctx := connectBus(t, natsURL)

	for i := 0; i < 2; i++ { // the duplicate must dedup
		if err := bus.PublishDelivery("env1", []byte(analyticsPayload)); err != nil {
			t.Fatalf("PublishDelivery #%d: %v", i, err)
		}
	}
	st := mustStream(t, natsURL, analyticsStream)
	info, err := st.Info(ctx)
	if err != nil {
		t.Fatalf("Info: %v", err)
	}
	if info.State.Msgs != 1 {
		t.Fatalf("NOTIFY_ANALYTICS holds %d msgs, want 1 (duplicate dropped)", info.State.Msgs)
	}
	raw, err := st.GetMsg(ctx, info.State.FirstSeq)
	if err != nil {
		t.Fatalf("GetMsg: %v", err)
	}
	if raw.Subject != "notify.analytics.env1" {
		t.Fatalf("subject = %q", raw.Subject)
	}
	if got := raw.Header.Get(jetstream.MsgIDHeader); got != "m1:error" {
		t.Fatalf("Nats-Msg-Id = %q, want m1:error", got)
	}
	for _, leak := range []string{"xyzzy", "example.com", "pref off", "SECRET"} {
		if strings.Contains(string(raw.Data), leak) {
			t.Fatalf("stored analytics event leaks %q: %s", leak, raw.Data)
		}
	}
	var generic map[string]any
	if err := json.Unmarshal(raw.Data, &generic); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if generic["v"] != float64(1) {
		t.Fatalf("v = %v", generic["v"])
	}
	delete(generic, "v")
	m := map[string]string{}
	for k, v := range generic {
		s, ok := v.(string)
		if !ok {
			t.Fatalf("field %s = %v, want string", k, v)
		}
		m[k] = s
	}
	want := map[string]string{
		"messageId": "m1", "orgId": "o1", "envId": "env1", "workflowId": "w1",
		"notificationId": "n1", "runId": "r1", "stepId": "s1", "transactionId": "t1",
		"integrationId": "i1", "subscriberId": "sub1", "channel": "email",
		"providerId": "smtp", "status": "error", "errorClass": "auth",
		"createdAt": "2026-01-02T03:04:05Z",
	}
	at := m["at"]
	delete(m, "at")
	if !reflect.DeepEqual(m, want) {
		t.Fatalf("event = %v, want %v", m, want)
	}
	if _, err := timestamp.Parse(at); err != nil {
		t.Fatalf("at = %q: %v", at, err)
	}
}

func TestPublishDelivery_ReplyEventGetsNoAnalyticsEvent(t *testing.T) {
	natsURL := startSidecar(t)
	bus, ctx := connectBus(t, natsURL)
	reply := `{"OrgID":"o1","EnvID":"env1","type":"reply","text":"hi","from":"a@b.c"}`
	if err := bus.PublishDelivery("env1", []byte(reply)); err != nil {
		t.Fatalf("PublishDelivery: %v", err)
	}
	info, err := mustStream(t, natsURL, analyticsStream).Info(ctx)
	if err != nil {
		t.Fatalf("Info: %v", err)
	}
	if info.State.Msgs != 0 {
		t.Fatalf("reply event produced %d analytics msgs, want 0", info.State.Msgs)
	}
}

// A missing analytics stream must not fail the delivery publish.
func TestPublishDelivery_AnalyticsFailureDoesNotFailDelivery(t *testing.T) {
	natsURL := startSidecar(t)
	bus, ctx := connectBus(t, natsURL)
	if err := inspectJS(t, natsURL).DeleteStream(ctx, analyticsStream); err != nil {
		t.Fatalf("DeleteStream: %v", err)
	}
	if err := bus.PublishDelivery("env1", []byte(analyticsPayload)); err != nil {
		t.Fatalf("PublishDelivery must succeed without the analytics stream: %v", err)
	}
	info, err := mustStream(t, natsURL, deliveryStream).Info(ctx)
	if err != nil || info.State.Msgs != 1 {
		t.Fatalf("delivery stream msgs = %v, err %v; want 1", info, err)
	}
}

func TestStreams_AnalyticsSizedWithNoMaxAge(t *testing.T) {
	for _, s := range Streams() {
		if s.Name != analyticsStream {
			continue
		}
		if s.MaxAge != 0 {
			t.Fatalf("MaxAge = %v, want none", s.MaxAge)
		}
		if s.MaxBytesFraction <= 0 || s.MaxBytesFloor < 16<<20 {
			t.Fatalf("sizing = %v / %d", s.MaxBytesFraction, s.MaxBytesFloor)
		}
		return
	}
	t.Fatal("no analytics stream spec")
}
