package eventbus

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/danmestas/notify/internal/timestamp"
	"github.com/nats-io/nats.go/jetstream"
)

// AnalyticsEventVersion is the schema version carried in AnalyticsEvent.V.
const AnalyticsEventVersion = 1

// AnalyticsEvent is the metadata-only record of one delivery event, published
// to NOTIFY_ANALYTICS for the analytics lake. It is an explicit allow-list:
// ids, enums and timestamps only. It must never carry the subject, content,
// error text, skip reason, or any address or phone number, because the stream
// is drained into an unencrypted lake. Adding a field means adding it to the
// allow-list in TestAnalyticsEvent_FieldAllowList on purpose.
//
// SubscriberID is an opaque id the tenant chose, not an address.
type AnalyticsEvent struct {
	V              int    `json:"v"`
	MessageID      string `json:"messageId"`
	OrgID          string `json:"orgId"`
	EnvID          string `json:"envId"`
	WorkflowID     string `json:"workflowId"`
	NotificationID string `json:"notificationId"`
	RunID          string `json:"runId"`
	StepID         string `json:"stepId"`
	TransactionID  string `json:"transactionId"`
	IntegrationID  string `json:"integrationId"`
	SubscriberID   string `json:"subscriberId"`
	Channel        string `json:"channel"`
	ProviderID     string `json:"providerId"`
	Status         string `json:"status"`
	ErrorClass     string `json:"errorClass"`
	CreatedAt      string `json:"createdAt"`
	At             string `json:"at"`
}

// analyticsSource decodes only the allow-listed keys of a domain.Message
// delivery payload (which has no json tags, so keys are the Go field names).
// The decoder drops everything else in the payload.
type analyticsSource struct {
	ID             string
	OrgID          string
	WorkflowID     string
	NotificationID string
	RunID          string
	StepID         string
	TransactionID  string
	IntegrationID  string
	SubscriberID   string
	Channel        string
	ProviderID     string
	Status         string
	ErrorClass     string
	CreatedAt      string
}

// buildAnalyticsEvent maps a delivery payload to its slim event. ok is false
// for a payload that is not a message delivery event: the emailloop reply
// event (it has no message id, and carries reply text and a sender address) and
// anything unparseable are skipped, never partially mapped.
func buildAnalyticsEvent(environmentID string, data []byte, now time.Time) (AnalyticsEvent, bool) {
	var s analyticsSource
	if json.Unmarshal(data, &s) != nil || s.ID == "" {
		return AnalyticsEvent{}, false
	}
	return AnalyticsEvent{
		V: AnalyticsEventVersion, MessageID: s.ID, OrgID: s.OrgID, EnvID: environmentID,
		WorkflowID: s.WorkflowID, NotificationID: s.NotificationID, RunID: s.RunID,
		StepID: s.StepID, TransactionID: s.TransactionID, IntegrationID: s.IntegrationID,
		SubscriberID: s.SubscriberID, Channel: s.Channel, ProviderID: s.ProviderID,
		Status: s.Status, ErrorClass: s.ErrorClass, CreatedAt: s.CreatedAt,
		At: timestamp.Format(now),
	}, true
}

// analyticsSubjectFor is notify.analytics.<environmentID>.
func analyticsSubjectFor(environmentID string) string {
	return "notify.analytics." + environmentID
}

// publishAnalytics is the best-effort second publish of PublishDelivery. It
// reuses the delivery event's Nats-Msg-Id so the lake's event id equals the
// analytics row key, and so a duplicate delivery publish dedups here too. A
// failure is logged and swallowed: analytics must not break delivery.
func (b *Bus) publishAnalytics(ctx context.Context, environmentID string, data []byte, msgID string) {
	ev, ok := buildAnalyticsEvent(environmentID, data, time.Now())
	if !ok {
		return
	}
	body, err := json.Marshal(ev)
	if err == nil {
		_, err = b.js.Publish(ctx, analyticsSubjectFor(environmentID), body, jetstream.WithMsgID(msgID))
	}
	if err != nil {
		slog.Default().Warn("eventbus: analytics publish failed",
			"env", environmentID, "msgId", msgID, "err", err)
	}
}
