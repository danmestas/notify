package domain

import "encoding/json"

// Scope is the tenancy key threaded through every repository call.
type Scope struct {
	OrgID string
	EnvID string
}

type Organization struct {
	ID        string `db:"id"`
	Name      string `db:"name"`
	CreatedAt string `db:"created_at"`
}

type Environment struct {
	ID         string `db:"id"`
	OrgID      string `db:"org_id"`
	Name       string `db:"name"`
	Identifier string `db:"identifier"`
	Type       string `db:"type"`
	BridgeURL  string `db:"bridge_url"`
	ParentID   string `db:"parent_id"`
}

type WebhookEndpoint struct {
	ID     string `db:"id"`
	OrgID  string `db:"org_id"`
	EnvID  string `db:"environment_id"`
	URL    string `db:"url"`
	Active bool   `db:"active"`
}

type APIKey struct {
	Hash   string `db:"hash"`
	EnvID  string `db:"environment_id"`
	OrgID  string `db:"org_id"`
	EncKey []byte `db:"enc_key"`
	UserID string `db:"user_id"`
}

type Subscriber struct {
	ID           string          `db:"id"`
	OrgID        string          `db:"org_id"`
	EnvID        string          `db:"environment_id"`
	SubscriberID string          `db:"subscriber_id"`
	Email        string          `db:"email"`
	Phone        string          `db:"phone"`
	FirstName    string          `db:"first_name"`
	LastName     string          `db:"last_name"`
	Locale       string          `db:"locale"`
	Timezone     string          `db:"timezone"`
	Data         json.RawMessage `db:"data"`
	Deleted      bool            `db:"deleted"`
}

// SubscriberChannel is a per-subscriber channel address for one provider
// (push topic, chat webhook). Credentials is a JSON document, e.g.
// {"topic":"agent-alerts"} for ntfy; server-side secrets live encrypted in
// Integration.EncCredentials, never here.
type SubscriberChannel struct {
	ID           string          `db:"id"`
	OrgID        string          `db:"org_id"`
	EnvID        string          `db:"environment_id"`
	SubscriberID string          `db:"subscriber_id"`
	ProviderID   string          `db:"provider_id"`
	Type         string          `db:"type"` // channel: "push" | "chat"
	Credentials  json.RawMessage `db:"credentials"`
}

// WebPushSubscription is one browser PushSubscription registered for a
// subscriber: endpoint + the p256dh/auth keys needed to encrypt payloads for
// it. Unlike SubscriberChannel (one row per provider), a subscriber can hold
// many of these — one per device/browser tab — so it lives in its own table.
type WebPushSubscription struct {
	ID           string `db:"id"`
	OrgID        string `db:"org_id"`
	EnvID        string `db:"environment_id"`
	SubscriberID string `db:"subscriber_id"`
	Endpoint     string `db:"endpoint"`
	P256dh       string `db:"p256dh"`
	Auth         string `db:"auth"`
	CreatedAt    string `db:"created_at"`
	LastUsedAt   string `db:"last_used_at"`
}

type Integration struct {
	ID             string          `db:"id"`
	OrgID          string          `db:"org_id"`
	EnvID          string          `db:"environment_id"`
	ProviderID     string          `db:"provider_id"`
	Channel        string          `db:"channel"`
	EncCredentials []byte          `db:"enc_credentials"`
	Configurations json.RawMessage `db:"configurations"`
	Active         bool            `db:"active"`
	Primary        bool            `db:"primary_provider"`
	Priority       int             `db:"priority"`
	Identifier     string          `db:"identifier"`
}

// StreamSource is an inbound webhook-push source: an external producer
// authorized (by a per-source secret) to POST event batches into one
// environment's durable feed, scoped to a single topic prefix.
type StreamSource struct {
	ID              string `db:"id"` // ULID
	OrgID           string `db:"org_id"`
	EnvID           string `db:"environment_id"`
	Name            string `db:"name"`               // human label
	Topic           string `db:"topic"`              // home topic; ValidateFeedTopic-checked; prefix bound for ingest
	SecretHash      string `db:"secret_hash"`        // hex SHA-256 of the raw secret (lookup key, unique)
	EncSecret       []byte `db:"enc_secret"`         // AES-256-GCM of the raw secret (reveal/rotate)
	RateLimitPerMin int    `db:"rate_limit_per_min"` // events/min; ThrottleAllow limit
	MaxEventBytes   int    `db:"max_event_bytes"`    // per-event data cap
	MaxBatchEvents  int    `db:"max_batch_events"`   // events per request cap
	ByteBudget      int64  `db:"byte_budget"`        // bytes per budget window (see S3)
	Active          bool   `db:"active"`
	LastEventAt     string `db:"last_event_at"` // RFC3339, best-effort
	LastError       string `db:"last_error"`    // last rejected-ingest reason, best-effort
	CreatedAt       string `db:"created_at"`
}

type Workflow struct {
	ID                string          `db:"id"`
	OrgID             string          `db:"org_id"`
	EnvID             string          `db:"environment_id"`
	Name              string          `db:"name"`
	TriggerIdentifier string          `db:"trigger_identifier"`
	Active            bool            `db:"active"`
	Origin            string          `db:"origin"`
	Steps             json.RawMessage `db:"steps"`
	PayloadSchema     json.RawMessage `db:"payload_schema"`
	// CompiledDAGVer is the id of the workflow's latest workflow_versions row
	// (see store.WorkflowVersionRepo). It was originally a scaffolded marker
	// (0001_init.sql) never set to a non-empty value; store.WorkflowVersionRepo.Create
	// retired that dead state by repointing it at the latest published
	// version instead of leaving a second, independent version marker.
	CompiledDAGVer string `db:"compiled_dag_version"`
	// Critical marks a no-opt-out workflow (e.g. password-reset): subscriber
	// channel/workflow preferences cannot suppress delivery. Enforced at
	// delivery time by preferences.Service.ChannelEnabled, not here.
	Critical bool `db:"critical"`
}

// Step is one element of Workflow.Steps (JSON, not a table column).
type Step struct {
	StepID        string          `json:"stepId"`
	Type          string          `json:"type"`
	Name          string          `json:"name,omitempty"`
	ControlValues json.RawMessage `json:"controlValues"`
	Filters       json.RawMessage `json:"filters,omitempty"`
}

type Notification struct {
	ID            string          `db:"id"`
	OrgID         string          `db:"org_id"`
	EnvID         string          `db:"environment_id"`
	WorkflowID    string          `db:"workflow_id"`
	SubscriberID  string          `db:"subscriber_id"`
	TransactionID string          `db:"transaction_id"`
	RunID         string          `db:"run_id"`
	Payload       json.RawMessage `db:"payload"`
	CreatedAt     string          `db:"created_at"`
	DigestKey     string          `db:"digest_key"`
}

type Message struct {
	ID         string `db:"id"`
	OrgID      string `db:"org_id"`
	EnvID      string `db:"environment_id"`
	WorkflowID string `db:"workflow_id"`
	// WorkflowIdentifier is the trigger identifier of the workflow named by
	// WorkflowID (e.g. "quarry-mention"), resolved at read time by a
	// scope-safe join against the workflows table. Empty when the workflow
	// row has since been deleted, or for rows written before this field
	// existed to no workflow at all — never an error. Not persisted on the
	// messages table itself; Create leaves it unset.
	WorkflowIdentifier string `db:"-"`
	NotificationID     string `db:"notification_id"`
	RunID              string `db:"run_id"`
	StepID             string `db:"step_id"`
	SubscriberID       string `db:"subscriber_id"`
	Channel            string `db:"channel"`
	ProviderID         string `db:"provider_id"`
	// IntegrationID is the ID of the integration row that dispatched this
	// message (populated from Wave 4 / migration 0017 onward). Empty for
	// rows written before the migration; those are excluded from
	// integrationId-filtered queries.
	IntegrationID string `db:"integration_id"`
	Content       string `db:"content"`
	Subject       string `db:"subject"`
	Status        string `db:"status"`
	Seen          bool   `db:"seen"`
	Read          bool   `db:"read"`
	Archived      bool   `db:"archived"`
	TransactionID string `db:"transaction_id"`
	ErrorText     string `db:"error_text"`
	// SkipReason names the gate that skipped this message when Status is
	// "skipped". Empty for every other status. Kept separate from ErrorText
	// because a skip is not an error -- a honoured subscriber preference is the
	// system working correctly, and filing it under error_text would read as a
	// fault to anyone inspecting the row.
	SkipReason string `db:"skip_reason"`
	CreatedAt  string `db:"created_at"`
	// Extras is the in_app rich-field snapshot (avatar, primaryAction,
	// secondaryAction, redirect, data) captured at send time, mirroring the reference
	// inbox notification shape. Non-in_app messages and rows written before
	// migration 0022 carry "{}". The column is NOT NULL; messageRepo.Create
	// coerces a nil value to "{}" so the constraint-less callers stay nil-safe.
	Extras json.RawMessage `db:"extras" json:"extras"`
}

type Preference struct {
	ID           string          `db:"id"`
	OrgID        string          `db:"org_id"`
	EnvID        string          `db:"environment_id"`
	Type         string          `db:"type"` // "subscriberGlobal" | "workflow"
	SubscriberID string          `db:"subscriber_id"`
	WorkflowID   string          `db:"workflow_id"` // empty for subscriberGlobal
	Channels     json.RawMessage `db:"channels"`    // JSON object {"in_app":true,"email":false,...}
}

// DigestWindow is a digest cadence: "every Amount Unit". It is the shape a
// subscriber may override per workflow (preference type "workflowDigest") and
// the shape a digest step authors in its controlValues, so resolution compares
// like with like.
type DigestWindow struct {
	Amount int64  `json:"amount"`
	Unit   string `json:"unit"`
}

// IsZero reports whether no window is set. A zero Amount is the only marker:
// an empty Unit is a valid "seconds" default in steptype.UnitDuration.
func (w DigestWindow) IsZero() bool { return w.Amount <= 0 }

type AnalyticsDelivery struct {
	ID            string `db:"id"`
	OrgID         string `db:"org_id"`
	EnvID         string `db:"environment_id"`
	Channel       string `db:"channel"`
	Status        string `db:"status"`
	SubscriberID  string `db:"subscriber_id"`
	WorkflowID    string `db:"workflow_id"`
	TransactionID string `db:"transaction_id"`
	// RunID is the workflow run (engine run id) this delivery belongs to —
	// shared by every channel a single trigger fans out to. Backs the
	// Workflow Runs Trend chart's COUNT(DISTINCT run_id) query so it counts
	// runs, not per-channel delivery rows (M2, GAP-AF-06 follow-up).
	RunID     string `db:"run_id"`
	CreatedAt string `db:"created_at"`
}

// DeliveryDayCount is one row of the delivery time-series: the per-day count of
// deliveries for a (channel, status) pair within a scope.
type DeliveryDayCount struct {
	Day     string `json:"day"`
	Channel string `json:"channel"`
	Status  string `json:"status"`
	Count   int    `json:"count"`
}

// RunDayCount is one row of the workflow-runs time-series: the per-day count
// of DISTINCT workflow runs within a scope, optionally filtered to one
// workflow. Backs the analytics Workflow Runs Trend chart.
type RunDayCount struct {
	Day   string `json:"day"`
	Count int    `json:"count"`
}

type Topic struct {
	ID    string `db:"id"`
	OrgID string `db:"org_id"`
	EnvID string `db:"environment_id"`
	Key   string `db:"key"`
	Name  string `db:"name"`
}

type Translation struct {
	ID     string `db:"id"`
	OrgID  string `db:"org_id"`
	EnvID  string `db:"environment_id"`
	Locale string `db:"locale"`
	Key    string `db:"key"`
	Value  string `db:"value"`
}

type Layout struct {
	ID        string `db:"id"`
	OrgID     string `db:"org_id"`
	EnvID     string `db:"environment_id"`
	Key       string `db:"key"`
	Name      string `db:"name"`
	Content   string `db:"content"`
	IsDefault bool   `db:"is_default"`
}

type Context struct {
	ID         string          `db:"id"`
	OrgID      string          `db:"org_id"`
	EnvID      string          `db:"environment_id"`
	Type       string          `db:"type"`
	ExternalID string          `db:"external_id"`
	Data       json.RawMessage `db:"data"`
	CreatedAt  string          `db:"created_at"`
	UpdatedAt  string          `db:"updated_at"`
}

// EnvVar is a per-scope key/value environment variable. Values are stored
// AES-GCM-encrypted in env_var_values and never held in this struct.
type EnvVar struct {
	ID        string `db:"id"`
	OrgID     string `db:"org_id"`
	EnvID     string `db:"environment_id"`
	Key       string `db:"key"`
	IsSecret  bool   `db:"is_secret"`
	CreatedAt string `db:"created_at"`
	UpdatedAt string `db:"updated_at"`
}

// InboundDomain is a per-scope domain authorized for inbound email reception.
type InboundDomain struct {
	ID                 string `db:"id"`
	OrgID              string `db:"org_id"`
	EnvID              string `db:"environment_id"`
	Name               string `db:"name"`
	Status             string `db:"status"` // "pending" | "verified" | "failed"
	MXConfigured       bool   `db:"mx_configured"`
	ExpectedMXHost     string `db:"expected_mx_host"`
	ExpectedMXPriority int    `db:"expected_mx_priority"`
	CreatedAt          string `db:"created_at"`
	UpdatedAt          string `db:"updated_at"`
}
