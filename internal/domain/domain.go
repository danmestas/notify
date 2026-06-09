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
