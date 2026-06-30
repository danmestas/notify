// Package runenv defines the run-input envelope passed from the trigger
// (as the StartRun input) to a channel send handler (which reads it from the
// dagnats TaskPayload.Input of the root step). It is the wire contract between
// internal/trigger (producer) and internal/delivery (consumer); both marshal
// the same shape, so it lives here to avoid duplication or drift.
package runenv

import "encoding/json"

// Scope carries tenancy through the envelope (the worker has no ambient Scope).
type Scope struct {
	OrgID string `json:"orgId"`
	EnvID string `json:"envId"`
}

// InAppRedirect is the navigation target of an in-app CTA or of the message
// itself (the reference platform inAppOutputSchema redirect: {url, target?}). Target is an
// optional anchor target (_self/_blank/_parent/_top); url is required when the
// redirect is present at all.
type InAppRedirect struct {
	URL    string `json:"url"`
	Target string `json:"target,omitempty"`
}

// InAppAction is an in-app CTA button (the reference platform primaryAction / secondaryAction:
// {label, redirect?}). Redirect is omitted when the action has no navigation.
type InAppAction struct {
	Label    string         `json:"label"`
	Redirect *InAppRedirect `json:"redirect,omitempty"`
}

// StepControls is the decoded shape of domain.Step.ControlValues. It is the
// single exported definition shared by both internal/trigger (ingest path) and
// internal/delivery/rebuild (self-source path), so the compiler enforces sync
// between the two paths instead of relying on developer discipline.
type StepControls struct {
	Subject  string `json:"subject"`
	Body     string `json:"body"`
	Throttle struct {
		Limit     int `json:"limit"`
		WindowSec int `json:"windowSec"`
	} `json:"throttle"`
	LayoutKey string   `json:"layoutKey"`
	Priority  int      `json:"priority"`
	Tags      []string `json:"tags"`
	Markdown  bool     `json:"markdown"`
	// Email-specific per-step overrides. Empty string means "use integration default".
	From       string `json:"from,omitempty"`
	ReplyTo    string `json:"replyTo,omitempty"`
	EditorType string `json:"editorType,omitempty"`
	// DataJSON is the raw JSON string stored under "data_json" in controlValues
	// (written by push.ParseForm). Unmarshaled to map[string]string at
	// envelope-build time and forwarded as runenv.Step.Data.
	DataJSON string `json:"data_json,omitempty"`
	// In-app rich fields (the reference platform inAppOutputSchema parity). Authored in the in_app
	// editor; snapshotted onto the persisted message's extras blob at send time.
	// Absent on every other channel.
	Avatar          string          `json:"avatar,omitempty"`
	PrimaryAction   *InAppAction    `json:"primaryAction,omitempty"`
	SecondaryAction *InAppAction    `json:"secondaryAction,omitempty"`
	Redirect        *InAppRedirect  `json:"redirect,omitempty"`
	Data            json.RawMessage `json:"data,omitempty"`
}

// Step is the per-step authored config the handler needs at runtime. Templates
// travel here because dagnats StepDef config does NOT reach the worker \u2014 only
// TaskPayload.Input does (verified against dag.ResolveInput).
type Step struct {
	StepID            string `json:"stepId"`
	Channel           string `json:"channel"`
	ProviderID        string `json:"providerId,omitempty"`
	SubjectTemplate   string `json:"subjectTemplate,omitempty"`
	BodyTemplate      string `json:"bodyTemplate"`
	ThrottleLimit     int    `json:"throttleLimit,omitempty"`
	ThrottleWindowSec int    `json:"throttleWindowSec,omitempty"`
	LayoutKey         string `json:"layoutKey,omitempty"`
	// Push extras (ntfy): authored alongside subject/body in controlValues.
	Priority int      `json:"priority,omitempty"` // 1 (min) .. 5 (max); 0 = provider default
	Tags     []string `json:"tags,omitempty"`
	Markdown bool     `json:"markdown,omitempty"`
	// Data is the optional key-value map forwarded as native data to FCM and
	// Expo (deep-links, badges). Absent/nil sends no data payload.
	Data map[string]string `json:"data,omitempty"`
	// Email-only per-step overrides. Empty means "use integration-level default".
	// Non-email handlers must ignore these fields.
	From       string `json:"from,omitempty"`
	ReplyTo    string `json:"replyTo,omitempty"`
	EditorType string `json:"editorType,omitempty"` // "html" \u2192 HTML body; "" or "block" \u2192 Text body
	// In-app rich fields carried to the in_app send handler, which snapshots them
	// onto the persisted message's extras blob (the reference platform field names). Non-in_app
	// handlers ignore them. InAppData is named distinctly from the push Data map
	// above to avoid the field collision while keeping a free-form JSON object.
	Avatar          string          `json:"avatar,omitempty"`
	PrimaryAction   *InAppAction    `json:"primaryAction,omitempty"`
	SecondaryAction *InAppAction    `json:"secondaryAction,omitempty"`
	Redirect        *InAppRedirect  `json:"redirect,omitempty"`
	InAppData       json.RawMessage `json:"inAppData,omitempty"`
	// Filters is the step's per-step condition (a JSON array of filter.Group,
	// copied verbatim from domain.Step.Filters). Empty/absent => no condition =>
	// the step always runs. When present, the send handler evaluates it against
	// the {subscriber, payload} context and SKIPS the send when it is false.
	Filters json.RawMessage `json:"filters,omitempty"`
}

// InAppEnvelope is the StartRun input for an in_app workflow run. Subscriber and
// Payload are raw JSON objects so the handler can unmarshal them into
// map[string]any for {{path}} template resolution.
type InAppEnvelope struct {
	Scope          Scope           `json:"scope"`
	TransactionID  string          `json:"transactionId"`
	WorkflowID     string          `json:"workflowId"`
	NotificationID string          `json:"notificationId"`
	SubscriberID   string          `json:"subscriberId"`
	Subscriber     json.RawMessage `json:"subscriber"`
	Payload        json.RawMessage `json:"payload"`
	Step           Step            `json:"step"`
}
