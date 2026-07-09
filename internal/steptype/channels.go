package steptype

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"unicode"

	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/engine"
)

// channelStepDef builds the engine StepDef shared by every channel type: a
// normal "send.<type>" task. DependsOn is left for the caller (positional
// chaining is workflow-level, not type-level).
func channelStepDef(step domain.Step) engine.StepDef {
	return engine.StepDef{ID: step.StepID, Task: "send." + step.Type, Type: "normal"}
}

// emptyControlValues is the default controlValues for a channel step.
func emptyControlValues() json.RawMessage { return json.RawMessage(`{}`) }

// marshalControlValues marshals a per-type form field map into controlValues
// JSON, preserving the historical error text.
func marshalControlValues(m map[string]any) (json.RawMessage, error) {
	b, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("marshal control values: %w", err)
	}
	return b, nil
}

// inApp is the in-app channel step type. Its editor authors a subject, a
// Handlebars body, and rich fields (avatar, primary/secondary CTA,
// top-level redirect, free-form data). ParseForm and ControlMap live in
// in_app_rich.go.
type inApp struct{}

func (inApp) Name() string                                     { return "in_app" }
func (inApp) DisplayName() string                              { return "In-App" }
func (inApp) DefaultControlValues() json.RawMessage            { return emptyControlValues() }
func (inApp) Compile(step domain.Step) (engine.StepDef, error) { return channelStepDef(step), nil }
func (inApp) Validate(json.RawMessage) error                   { return nil }
func (inApp) EditorPartial() string                            { return "wf-step-fields-in_app" }

// email is the email channel step type (subject + body).
type email struct{}

func (email) Name() string                                     { return "email" }
func (email) DisplayName() string                              { return "Email" }
func (email) DefaultControlValues() json.RawMessage            { return emptyControlValues() }
func (email) ControlMap(cv json.RawMessage) map[string]string  { return flattenControlValues(cv) }
func (email) Compile(step domain.Step) (engine.StepDef, error) { return channelStepDef(step), nil }
func (email) Validate(json.RawMessage) error                   { return nil }
func (email) EditorPartial() string                            { return "wf-step-fields-email" }

func (email) ParseForm(form url.Values) (json.RawMessage, error) {
	m := map[string]any{
		"subject": formField(form, "subject"),
		"body":    formField(form, "body"),
	}
	// Optional email overrides: omit keys whose trimmed value is empty so the
	// JSON stays compact and does not corrupt controlValues on new/unset steps.
	if v := formField(form, "from"); v != "" {
		m["from"] = v
	}
	if v := formField(form, "reply_to"); v != "" {
		m["replyTo"] = v
	}
	if v := formField(form, "editor_type"); v != "" {
		m["editorType"] = v
	}
	if v := formField(form, "layout_key"); v != "" {
		m["layoutKey"] = v
	}
	return marshalControlValues(m)
}

// push is the push channel step type (title + body).
type push struct{}

func (push) Name() string                                     { return "push" }
func (push) DisplayName() string                              { return "Push" }
func (push) DefaultControlValues() json.RawMessage            { return emptyControlValues() }
func (push) ControlMap(cv json.RawMessage) map[string]string  { return flattenControlValues(cv) }
func (push) Compile(step domain.Step) (engine.StepDef, error) { return channelStepDef(step), nil }
func (push) Validate(cv json.RawMessage) error {
	if len(cv) == 0 {
		return nil
	}
	var m struct {
		DataJSON string `json:"data_json"`
	}
	if err := json.Unmarshal(cv, &m); err != nil {
		return err
	}
	return validateDataJSON(m.DataJSON)
}
func (push) EditorPartial() string { return "wf-step-fields-push" }

func (push) ParseForm(form url.Values) (json.RawMessage, error) {
	m := map[string]any{
		"title": formField(form, "title"),
		"body":  formField(form, "body"),
	}
	if dataJSON := formField(form, "data_json"); dataJSON != "" {
		if err := validateDataJSON(dataJSON); err != nil {
			return nil, err
		}
		m["data_json"] = dataJSON
	}
	return marshalControlValues(m)
}

// validateDataJSON checks that s is a JSON object whose every value is a JSON
// string literal. It rejects null, numbers, booleans, arrays, and nested
// objects. An empty s is accepted (means data_json is absent).
func validateDataJSON(s string) error {
	if s == "" {
		return nil
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(s), &raw); err != nil {
		return errors.New("data must be a JSON object with string values only")
	}
	for _, v := range raw {
		// A JSON string literal always starts with '"'; anything else (null,
		// number, bool, array, object) is rejected.
		if len(v) == 0 || v[0] != '"' {
			return errors.New("data must be a JSON object with string values only")
		}
	}
	return nil
}

// sms is the SMS channel step type (body only).
type sms struct{}

func (sms) Name() string                                     { return "sms" }
func (sms) DisplayName() string                              { return "SMS" }
func (sms) DefaultControlValues() json.RawMessage            { return emptyControlValues() }
func (sms) ControlMap(cv json.RawMessage) map[string]string  { return flattenControlValues(cv) }
func (sms) Compile(step domain.Step) (engine.StepDef, error) { return channelStepDef(step), nil }
func (sms) Validate(json.RawMessage) error                   { return nil }
func (sms) EditorPartial() string                            { return "wf-step-fields-sms" }

func (sms) ParseForm(form url.Values) (json.RawMessage, error) {
	return marshalControlValues(map[string]any{"body": formField(form, "body")})
}

// chat is the chat channel step type (body only).
type chat struct{}

func (chat) Name() string                                     { return "chat" }
func (chat) DisplayName() string                              { return "Chat" }
func (chat) DefaultControlValues() json.RawMessage            { return emptyControlValues() }
func (chat) ControlMap(cv json.RawMessage) map[string]string  { return flattenControlValues(cv) }
func (chat) Compile(step domain.Step) (engine.StepDef, error) { return channelStepDef(step), nil }
func (chat) Validate(json.RawMessage) error                   { return nil }
func (chat) EditorPartial() string                            { return "wf-step-fields-chat" }

func (chat) ParseForm(form url.Values) (json.RawMessage, error) {
	return marshalControlValues(map[string]any{"body": formField(form, "body")})
}

// stream is the stream channel step type — a 10th, built-in (provider-less,
// like in_app) type that publishes a rendered event to a tenant's durable
// feed topic (topic + body). It compiles to the "send.stream" task.
type stream struct{}

func (stream) Name() string                                     { return "stream" }
func (stream) DisplayName() string                              { return "Stream" }
func (stream) DefaultControlValues() json.RawMessage            { return emptyControlValues() }
func (stream) ControlMap(cv json.RawMessage) map[string]string  { return flattenControlValues(cv) }
func (stream) Compile(step domain.Step) (engine.StepDef, error) { return channelStepDef(step), nil }
func (stream) EditorPartial() string                            { return "wf-step-fields-stream" }

func (stream) ParseForm(form url.Values) (json.RawMessage, error) {
	return marshalControlValues(map[string]any{
		"topic": formField(form, "topic"),
		"body":  formField(form, "body"),
	})
}

// streamTopicForbidden reports whether r may never appear in an authored
// stream topic. This is a cheap, dependency-free authoring-time guard —
// steptype must not import eventbus (package doc, keeps the registry
// dependency-light). The authoritative gate is the RENDERED topic checked by
// eventbus.ValidateFeedTopic at send time (Slice A4); this only rejects
// characters that could never be legal even after template rendering.
func streamTopicForbidden(r rune) bool {
	return r == '*' || r == '>' || r == '$' || unicode.IsSpace(r)
}

// Validate rejects a missing topic and any authored topic containing a
// character that can never be legal in a NATS-derived subject token (wildcard
// markers, whitespace, or '$'), even inside a Handlebars template span — the
// rendered value is re-validated at send time.
func (stream) Validate(cv json.RawMessage) error {
	var m struct {
		Topic string `json:"topic"`
	}
	if len(cv) > 0 {
		if err := json.Unmarshal(cv, &m); err != nil {
			return err
		}
	}
	if m.Topic == "" {
		return errors.New("stream: topic is required")
	}
	if strings.IndexFunc(m.Topic, streamTopicForbidden) >= 0 {
		return errors.New("stream: topic contains an invalid character")
	}
	return nil
}
