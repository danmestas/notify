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
