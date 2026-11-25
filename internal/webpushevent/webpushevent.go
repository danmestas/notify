// Package webpushevent builds the webpush.subscription_removed event: the
// signal a tenant's outbound webhooks get when a Web Push device is deleted,
// by notify after the push service reported it gone or by the tenant's own
// DELETE route (#172).
package webpushevent

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	"github.com/danmestas/notify/internal/domain"
)

// Type is the event's `type`.
const Type = "webpush.subscription_removed"

// Reasons a device was removed.
const (
	// ReasonGone: the push service answered 404/410 and notify deleted the row.
	ReasonGone = "gone"
	// ReasonRetired: the push service refused the device too many times in a
	// row and notify deleted the row.
	ReasonRetired = "retired"
	// ReasonDeleted: the tenant called a DELETE route.
	ReasonDeleted = "deleted"
)

// Publisher hands an event to the delivery stream the outbound webhooks
// consume. eventbus.Bus satisfies it.
type Publisher interface {
	PublishDelivery(environmentID string, data []byte) error
}

// Event is the webhook body. OrgID and EnvID route it, like every event on
// the delivery stream.
type Event struct {
	OrgID        string `json:"orgId"`
	EnvID        string `json:"envId"`
	Type         string `json:"type"`
	SubscriberID string `json:"subscriberId"`
	DeviceID     string `json:"deviceId"`
	EndpointHash string `json:"endpointHash"`
	Reason       string `json:"reason"`
}

// EndpointHash is the lowercase hex SHA-256 of the endpoint exactly as
// registered. It identifies a device without echoing the capability URL.
func EndpointHash(endpoint string) string {
	sum := sha256.Sum256([]byte(endpoint))
	return hex.EncodeToString(sum[:])
}

// PublishRemoved tells the tenant's webhooks that sub was deleted. Best
// effort: a nil publisher or a publish error never fails the removal.
func PublishRemoved(pub Publisher, sub domain.WebPushSubscription, reason string) {
	if pub == nil {
		return
	}
	data, err := json.Marshal(Event{
		OrgID: sub.OrgID, EnvID: sub.EnvID, Type: Type,
		SubscriberID: sub.SubscriberID, DeviceID: sub.ID,
		EndpointHash: EndpointHash(sub.Endpoint), Reason: reason,
	})
	if err != nil {
		return
	}
	_ = pub.PublishDelivery(sub.EnvID, data)
}
