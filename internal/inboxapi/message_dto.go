package inboxapi

import (
	"encoding/json"

	"github.com/danmestas/notify/internal/domain"
)

// MessageDTO is the wire shape of one message: GET /v1/messages (every
// branch) and GET /v1/notifications/{id}/messages. domain.Message carries no
// JSON tags, so serialising it directly leaked Go field names; this keeps the
// wire camelCase like every other API response and leaves out the scope ids
// (orgId, envId).
type MessageDTO struct {
	ID                 string          `json:"id"`
	NotificationID     string          `json:"notificationId"`
	TransactionID      string          `json:"transactionId"`
	WorkflowID         string          `json:"workflowId"`
	WorkflowIdentifier string          `json:"workflowIdentifier"`
	RunID              string          `json:"runId"`
	StepID             string          `json:"stepId"`
	SubscriberID       string          `json:"subscriberId"`
	Channel            string          `json:"channel"`
	ProviderID         string          `json:"providerId"`
	ProviderMessageID  string          `json:"providerMessageId"`
	IntegrationID      string          `json:"integrationId"`
	Status             string          `json:"status"`
	SkipReason         string          `json:"skipReason"`
	ErrorClass         string          `json:"errorClass"`
	ErrorText          string          `json:"errorText"`
	Subject            string          `json:"subject"`
	Content            string          `json:"content"`
	Seen               bool            `json:"seen"`
	Read               bool            `json:"read"`
	Archived           bool            `json:"archived"`
	CreatedAt          string          `json:"createdAt"`
	Extras             json.RawMessage `json:"extras"`
}

// ToMessageDTO converts one stored message to its wire shape.
func ToMessageDTO(m domain.Message) MessageDTO {
	extras := m.Extras
	if len(extras) == 0 {
		extras = json.RawMessage("{}")
	}
	return MessageDTO{
		ID:                 m.ID,
		NotificationID:     m.NotificationID,
		TransactionID:      m.TransactionID,
		WorkflowID:         m.WorkflowID,
		WorkflowIdentifier: m.WorkflowIdentifier,
		RunID:              m.RunID,
		StepID:             m.StepID,
		SubscriberID:       m.SubscriberID,
		Channel:            m.Channel,
		ProviderID:         m.ProviderID,
		ProviderMessageID:  m.ProviderMessageID,
		IntegrationID:      m.IntegrationID,
		Status:             m.Status,
		SkipReason:         m.SkipReason,
		ErrorClass:         m.ErrorClass,
		ErrorText:          m.ErrorText,
		Subject:            m.Subject,
		Content:            m.Content,
		Seen:               m.Seen,
		Read:               m.Read,
		Archived:           m.Archived,
		CreatedAt:          m.CreatedAt,
		Extras:             extras,
	}
}

// ToMessageDTOs converts a slice; the result is never nil so it encodes as [].
func ToMessageDTOs(ms []domain.Message) []MessageDTO {
	out := make([]MessageDTO, 0, len(ms))
	for _, m := range ms {
		out = append(out, ToMessageDTO(m))
	}
	return out
}
