package delivery

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/danmestas/notify/internal/delivery/template"
	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/engine"
	"github.com/danmestas/notify/internal/runenv"
	"github.com/danmestas/notify/internal/store"
)

// rebuildEnvelope reconstructs the run envelope for a channel step from the
// store, keyed by the run id. It is the self-source fallback used when a step's
// TaskPayload.Input is empty — e.g. a channel step that DependsOn a delay/sleep
// step, whose nil output leaves the downstream step's input empty. The
// Input-present path never calls this.
func (h *Handler) rebuildEnvelope(ctx context.Context, t engine.TaskPayload) (runenv.InAppEnvelope, string, error) {
	return rebuildEnvelope(ctx, h.store, t)
}

// rebuildEnvelope is the shared self-source logic backing both Handler and
// EmailHandler (each holds a *store.Store). It resolves the notification by run
// id, loads the authored step config and subscriber, and rebuilds the envelope.
func rebuildEnvelope(ctx context.Context, s *store.Store, t engine.TaskPayload) (runenv.InAppEnvelope, string, error) {
	notif, err := s.Notifications().GetByRunID(ctx, t.RunID)
	if err != nil {
		return runenv.InAppEnvelope{}, "", fmt.Errorf("rebuild: notification by run %q: %w", t.RunID, err)
	}
	scope := domain.Scope{OrgID: notif.OrgID, EnvID: notif.EnvID}

	wf, err := s.Workflows().GetByID(ctx, scope, notif.WorkflowID)
	if err != nil {
		return runenv.InAppEnvelope{}, "", fmt.Errorf("rebuild: workflow %q: %w", notif.WorkflowID, err)
	}

	var steps []domain.Step
	if len(wf.Steps) > 0 {
		if err := json.Unmarshal(wf.Steps, &steps); err != nil {
			return runenv.InAppEnvelope{}, "", fmt.Errorf("rebuild: unmarshal steps: %w", err)
		}
	}
	var found *domain.Step
	for i := range steps {
		if steps[i].StepID == t.StepID {
			found = &steps[i]
			break
		}
	}
	if found == nil {
		return runenv.InAppEnvelope{}, "", fmt.Errorf("rebuild: step %q not found in workflow %q", t.StepID, notif.WorkflowID)
	}
	var controls struct {
		Subject  string `json:"subject"`
		Body     string `json:"body"`
		Throttle struct {
			Limit     int `json:"limit"`
			WindowSec int `json:"windowSec"`
		} `json:"throttle"`
		LayoutKey string `json:"layoutKey"`
		// Push extras (ntfy): forwarded verbatim into runenv.Step.
		Priority int      `json:"priority"`
		Tags     []string `json:"tags"`
		Markdown bool     `json:"markdown"`
	}
	if len(found.ControlValues) > 0 {
		if err := json.Unmarshal(found.ControlValues, &controls); err != nil {
			return runenv.InAppEnvelope{}, "", fmt.Errorf("rebuild: unmarshal controlValues: %w", err)
		}
	}

	sub, err := s.Subscribers().GetByExternalID(ctx, scope, notif.SubscriberID)
	if err != nil {
		return runenv.InAppEnvelope{}, "", fmt.Errorf("rebuild: subscriber %q: %w", notif.SubscriberID, err)
	}
	// Same handlebars namespace shape the trigger marshals into the envelope.
	subscriberJSON, err := json.Marshal(struct {
		SubscriberID string          `json:"subscriberId"`
		FirstName    string          `json:"firstName"`
		LastName     string          `json:"lastName"`
		Email        string          `json:"email"`
		Phone        string          `json:"phone"`
		Locale       string          `json:"locale"`
		Timezone     string          `json:"timezone"`
		Data         json.RawMessage `json:"data,omitempty"`
	}{
		SubscriberID: sub.SubscriberID,
		FirstName:    sub.FirstName,
		LastName:     sub.LastName,
		Email:        sub.Email,
		Phone:        sub.Phone,
		Locale:       sub.Locale,
		Timezone:     sub.Timezone,
		Data:         sub.Data,
	})
	if err != nil {
		return runenv.InAppEnvelope{}, "", fmt.Errorf("rebuild: marshal subscriber: %w", err)
	}

	return runenv.InAppEnvelope{
		Scope:          runenv.Scope{OrgID: notif.OrgID, EnvID: notif.EnvID},
		TransactionID:  notif.TransactionID,
		WorkflowID:     notif.WorkflowID,
		NotificationID: notif.ID,
		SubscriberID:   notif.SubscriberID,
		Subscriber:     subscriberJSON,
		Payload:        notif.Payload,
		Step: runenv.Step{
			StepID:            t.StepID,
			Channel:           found.Type,
			SubjectTemplate:   controls.Subject,
			BodyTemplate:      controls.Body,
			ThrottleLimit:     controls.Throttle.Limit,
			ThrottleWindowSec: controls.Throttle.WindowSec,
			LayoutKey:         controls.LayoutKey,
			Priority:          controls.Priority,
			Tags:              controls.Tags,
			Markdown:          controls.Markdown,
		},
	}, notif.DigestKey, nil
}

// storeTranslator builds the {{t}} translate func for a render. The locale is
// read from subMap["locale"] (default "en" when absent/empty); the lookup map
// is the store's translations for that locale within scope. A key with no
// translation resolves to itself, so an unseeded key renders verbatim.
func storeTranslator(ctx context.Context, s *store.Store, scope domain.Scope, subMap map[string]any) func(key string) string {
	locale, _ := subMap["locale"].(string)
	if locale == "" {
		locale = "en"
	}
	m, _ := s.Translations().Map(ctx, scope, locale)
	return func(key string) string {
		if v, ok := m[key]; ok {
			return v
		}
		return key
	}
}

// wrapLayout injects the rendered body as {{content}} into the named layout's
// content template and renders it, returning the wrapped body. When layoutKey is
// empty or the layout is not found, content is returned unchanged (fail-open) so
// a step without a configured layout renders exactly as before. The render
// context is a shallow copy of data with "content" set, so the caller's map is
// not mutated.
func wrapLayout(ctx context.Context, s *store.Store, scope domain.Scope, layoutKey, content string, data map[string]any, translate func(key string) string) string {
	if layoutKey == "" {
		return content
	}
	layout, err := s.Layouts().Get(ctx, scope, layoutKey)
	if err != nil {
		return content
	}
	wrapData := make(map[string]any, len(data)+1)
	for k, v := range data {
		wrapData[k] = v
	}
	wrapData["content"] = content
	return template.RenderI18n(layout.Content, wrapData, translate)
}
