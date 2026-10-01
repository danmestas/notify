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
// itself (redirect: {url, target?}). Target is an
// optional anchor target (_self/_blank/_parent/_top); url is required when the
// redirect is present at all.
type InAppRedirect struct {
	URL    string `json:"url"`
	Target string `json:"target,omitempty"`
}

// InAppAction is an in-app CTA button (primaryAction / secondaryAction:
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
	Subject string `json:"subject"`
	// Title is the legacy spelling of Subject on push steps: the console
	// editor stored the push title under "title" while every reader looked at
	// "subject", so authored titles never reached the device (#334). It is
	// read only as a fallback by SubjectTemplate, never written, and exists so
	// steps authored before the fix keep rendering without being re-saved.
	Title string `json:"title"`
	Body  string `json:"body"`
	// Topic is the stream channel's routing key: a dot-key template rendered
	// against {subscriber,payload}. Absent on every other channel.
	Topic    string `json:"topic,omitempty"`
	Throttle struct {
		Limit     int `json:"limit"`
		WindowSec int `json:"windowSec"`
	} `json:"throttle"`
	LayoutKey string   `json:"layoutKey"`
	Priority  int      `json:"priority"`
	Tags      []string `json:"tags"`
	Markdown  bool     `json:"markdown"`
	// Integration is the optional identifier of a specific integration to send
	// through, overriding the channel's primary integration. Empty means "use
	// primary" (existing Resolve* behavior, unchanged). Send steps only
	// (email/sms/push/chat); absent on every other step type.
	Integration string `json:"integration,omitempty"`
	// Email-specific per-step overrides. Empty string means "use integration default".
	From       string `json:"from,omitempty"`
	ReplyTo    string `json:"replyTo,omitempty"`
	EditorType string `json:"editorType,omitempty"`
	// Reply opts an email step into reply-to-notification (#345): the send
	// carries a tokenized reply mailbox and a reply is handed back to the
	// environment's webhook endpoints. Default off, so existing workflows are
	// byte-identical.
	Reply bool `json:"reply,omitempty"`
	// HTMLBody is an email step's optional HTML body (#369). When set, Body
	// is the plain-text part and the message goes out as
	// multipart/alternative with both. Absent keeps today's single-part send.
	HTMLBody string `json:"htmlBody,omitempty"`
	// DataJSON is the raw JSON string stored under "data_json" in controlValues
	// (written by push.ParseForm). Unmarshaled to map[string]string at
	// envelope-build time and forwarded as runenv.Step.Data.
	DataJSON string `json:"data_json,omitempty"`
	// In-app rich fields . Authored in the in_app
	// editor; snapshotted onto the persisted message's extras blob at send time.
	// Absent on every other channel.
	Avatar          string          `json:"avatar,omitempty"`
	PrimaryAction   *InAppAction    `json:"primaryAction,omitempty"`
	SecondaryAction *InAppAction    `json:"secondaryAction,omitempty"`
	Redirect        *InAppRedirect  `json:"redirect,omitempty"`
	Data            json.RawMessage `json:"data,omitempty"`
}

// SubjectTemplate is the authored subject line for the step: the value that
// becomes runenv.Step.SubjectTemplate. Prefer it over reading Subject
// directly, so the legacy push "title" spelling is honoured in exactly one
// place rather than at each mapping site.
func (c StepControls) SubjectTemplate() string {
	if c.Subject != "" {
		return c.Subject
	}
	return c.Title
}

// Step is the per-step authored config the handler needs at runtime. Templates
// travel here because dagnats StepDef config does NOT reach the worker \u2014 only
// TaskPayload.Input does (verified against dag.ResolveInput).
type Step struct {
	StepID          string `json:"stepId"`
	Channel         string `json:"channel"`
	ProviderID      string `json:"providerId,omitempty"`
	SubjectTemplate string `json:"subjectTemplate,omitempty"`
	BodyTemplate    string `json:"bodyTemplate"`
	// Topic is the stream channel's routing key: a dot-key template rendered
	// against {subscriber,payload}. Absent on every other channel.
	Topic             string `json:"topic,omitempty"`
	ThrottleLimit     int    `json:"throttleLimit,omitempty"`
	ThrottleWindowSec int    `json:"throttleWindowSec,omitempty"`
	LayoutKey         string `json:"layoutKey,omitempty"`
	// Push extras (ntfy): authored alongside subject/body in controlValues.
	Priority int      `json:"priority,omitempty"` // 1 (min) .. 5 (max); 0 = provider default
	Tags     []string `json:"tags,omitempty"`
	Markdown bool     `json:"markdown,omitempty"`
	// Integration is the optional per-step integration identifier (control key
	// "integration"). Empty means "use the channel's primary integration" —
	// byte-identical to pre-#314 behavior. Send handlers resolve it through
	// connectivity.Service.ResolveIntegration instead of Resolve<Channel> when
	// set.
	Integration string `json:"integration,omitempty"`
	// Data is the optional key-value map forwarded as native data to FCM and
	// Expo (deep-links, badges). Absent/nil sends no data payload.
	Data map[string]string `json:"data,omitempty"`
	// Email-only per-step overrides. Empty means "use integration-level default".
	// Non-email handlers must ignore these fields.
	From       string `json:"from,omitempty"`
	ReplyTo    string `json:"replyTo,omitempty"`
	EditorType string `json:"editorType,omitempty"` // "html" \u2192 HTML body; "" or "block" \u2192 Text body
	// ReplyEnabled mirrors StepControls.Reply: the send handler arms a reply
	// thread and overrides ReplyTo with the tokenized mailbox when set.
	ReplyEnabled bool `json:"replyEnabled,omitempty"`
	// HTMLBodyTemplate mirrors StepControls.HTMLBody: an email step's HTML
	// part, sent alongside BodyTemplate as its text part (#369).
	HTMLBodyTemplate string `json:"htmlBodyTemplate,omitempty"`
	// In-app rich fields carried to the in_app send handler, which snapshots them
	// onto the persisted message's extras blob. Non-in_app
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
	Scope         Scope  `json:"scope"`
	TransactionID string `json:"transactionId"`
	WorkflowID    string `json:"workflowId"`
	// WorkflowCritical mirrors the triggering domain.Workflow.Critical flag,
	// populated once at trigger time (and on self-source rebuild) from the
	// loaded workflow row. It is the sole channel by which delivery handlers
	// learn a workflow is no-opt-out; they forward it verbatim to
	// preferences.Service.ChannelEnabled rather than re-deriving it.
	WorkflowCritical bool `json:"workflowCritical,omitempty"`
	// WorkflowIdentifier is the trigger identifier (the name a caller triggers
	// by), carried so a reply event can say which workflow the recipient
	// answered without a lookup. WorkflowID above is the internal row id.
	WorkflowIdentifier string          `json:"workflowIdentifier,omitempty"`
	NotificationID     string          `json:"notificationId"`
	SubscriberID       string          `json:"subscriberId"`
	Subscriber         json.RawMessage `json:"subscriber"`
	Payload            json.RawMessage `json:"payload"`
	Step               Step            `json:"step"`
	// IdempotencyKey is the dagnats StartRun admission value: authoring.Compile
	// points every compiled DAG's IdempotencyKey dot-path at this field. It
	// combines TransactionID with SubscriberID (trigger.go composes it) rather
	// than reusing TransactionID alone: a multicast trigger (to.topicKey) mints
	// ONE TransactionID shared by every fan-out recipient, so keying only on
	// TransactionID would collide every recipient after the first onto the same
	// dagnats run. Keying on (TransactionID, SubscriberID) still dedupes a
	// genuine client retry of the identical (transaction, recipient) pair,
	// without merging distinct recipients of the same multicast.
	IdempotencyKey string `json:"idempotencyKey"`
	// DigestDeadline is the instant a regular digest window closes for THIS
	// subscriber, RFC3339. A regular digest step compiles to an engine sleep
	// whose config is {"until_input_path":"digestDeadline"}, so dagnats reads
	// this field out of the run input at dispatch and sleeps until it.
	//
	// The window therefore lives in the per-subscriber run input rather than in
	// the per-workflow DAG, which is what lets one workflow serve subscribers on
	// different cadences (#336). trigger.go fills it with
	// now + (subscriber override, else the step's authored window).
	DigestDeadline string `json:"digestDeadline,omitempty"`
}
