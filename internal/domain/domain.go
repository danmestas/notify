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
	CompiledDAGVer    string          `db:"compiled_dag_version"`
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
	ID             string `db:"id"`
	OrgID          string `db:"org_id"`
	EnvID          string `db:"environment_id"`
	WorkflowID     string `db:"workflow_id"`
	NotificationID string `db:"notification_id"`
	RunID          string `db:"run_id"`
	StepID         string `db:"step_id"`
	SubscriberID   string `db:"subscriber_id"`
	Channel        string `db:"channel"`
	ProviderID     string `db:"provider_id"`
	Content        string `db:"content"`
	Subject        string `db:"subject"`
	Status         string `db:"status"`
	Seen           bool   `db:"seen"`
	Read           bool   `db:"read"`
	Archived       bool   `db:"archived"`
	TransactionID  string `db:"transaction_id"`
	ErrorText      string `db:"error_text"`
	CreatedAt      string `db:"created_at"`
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

type AnalyticsDelivery struct {
	ID            string `db:"id"`
	OrgID         string `db:"org_id"`
	EnvID         string `db:"environment_id"`
	Channel       string `db:"channel"`
	Status        string `db:"status"`
	SubscriberID  string `db:"subscriber_id"`
	WorkflowID    string `db:"workflow_id"`
	TransactionID string `db:"transaction_id"`
	CreatedAt     string `db:"created_at"`
}

// DeliveryDayCount is one row of the delivery time-series: the per-day count of
// deliveries for a (channel, status) pair within a scope.
type DeliveryDayCount struct {
	Day     string `json:"day"`
	Channel string `json:"channel"`
	Status  string `json:"status"`
	Count   int    `json:"count"`
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
	ID      string `db:"id"`
	OrgID   string `db:"org_id"`
	EnvID   string `db:"environment_id"`
	Key     string `db:"key"`
	Name    string `db:"name"`
	Content string `db:"content"`
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
