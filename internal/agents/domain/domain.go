// Package domain holds the agent-communication core types: the Agent platform
// object, its channel links, the durable conversation transcript, and the
// Runtime port a turn executes through. It imports nothing internal so every
// agents/* sibling (store, api, runtime, ingress) can depend on it freely.
package domain

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// Allowed values of Agent.Runtime. The runtime selects how a turn is executed:
// mesh publishes an A2A envelope over NATS, bridge POSTs to an HTTP endpoint,
// managed compiles onto the engine's agent_loop.
const (
	RuntimeMesh    = "mesh"
	RuntimeBridge  = "bridge"
	RuntimeManaged = "managed"
)

// ValidRuntime reports whether r is one of the allowed runtime values.
func ValidRuntime(r string) bool {
	switch r {
	case RuntimeMesh, RuntimeBridge, RuntimeManaged:
		return true
	}
	return false
}

// SupportedRuntimes lists the runtimes that actually have a registered executor
// and can therefore execute turns without dead-lettering. It is the single
// source of truth the console renders its runtime <option> list from and guards
// create/save against. Keep it in lockstep with the WithRuntime(...) calls in
// cmd/notify/main.go: a runtime constant exists here (RuntimeBridge,
// RuntimeManaged) before its executor does, so offering it in the UI would
// silently dead-letter every turn (see issue #103). Add an entry here only when
// the matching WithRuntime registration lands.
var SupportedRuntimes = []string{RuntimeMesh, RuntimeManaged}

// IsSupportedRuntime reports whether r has a registered executor (i.e. appears
// in SupportedRuntimes). Unlike ValidRuntime, it rejects runtimes that are
// defined but not yet implemented.
func IsSupportedRuntime(r string) bool {
	for _, s := range SupportedRuntimes {
		if r == s {
			return true
		}
	}
	return false
}

// identifierPattern is the URL-safe slug allowlist for Agent.Identifier:
// lowercase letters, digits, hyphens, and underscores, starting with a
// letter or digit. An identifier is used verbatim as a console URL path
// segment (view/edit/delete/save/activate routes); an identifier containing
// '/' (path separator) or other reserved/URL-unsafe characters produces an
// agent every console route 404s on, permanently orphaning it (same class as
// H2, see authoring.triggerIdentifierPattern).
var identifierPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

// ValidIdentifier reports whether id is a URL-safe agent identifier.
// Checked by both the console and API create handlers before any store
// write.
func ValidIdentifier(id string) bool {
	return identifierPattern.MatchString(id)
}

// MaxLogoBytes is the largest decoded agent logo.
const MaxLogoBytes = 96 * 1024

// logoTypes maps each allowed logo media type to the magic bytes its image
// data must start with. SVG is deliberately absent: it can carry script.
var logoTypes = map[string][]string{
	"image/png":  {"\x89PNG\r\n\x1a\n"},
	"image/jpeg": {"\xff\xd8\xff"},
	"image/webp": {"RIFF"},
}

// ValidateLogo checks an agent logo: empty (no logo) or a
// data:image/{png,jpeg,webp};base64,... URI whose decoded bytes are at most
// MaxLogoBytes and start like the declared image type.
func ValidateLogo(logo string) error {
	if logo == "" {
		return nil
	}
	rest, ok := strings.CutPrefix(logo, "data:")
	if !ok {
		return errors.New("logo must be a data: URI")
	}
	mediaType, payload, ok := strings.Cut(rest, ";base64,")
	if !ok {
		return errors.New("logo must be a base64 data: URI")
	}
	magics, ok := logoTypes[mediaType]
	if !ok {
		return errors.New("logo must be image/png, image/jpeg or image/webp")
	}
	if base64.StdEncoding.DecodedLen(len(payload)) > MaxLogoBytes+2 {
		return fmt.Errorf("logo is larger than %d bytes", MaxLogoBytes)
	}
	raw, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		return errors.New("logo is not valid base64")
	}
	if len(raw) == 0 {
		return errors.New("logo has no image data")
	}
	if len(raw) > MaxLogoBytes {
		return fmt.Errorf("logo is larger than %d bytes", MaxLogoBytes)
	}
	for _, m := range magics {
		if strings.HasPrefix(string(raw), m) {
			return nil
		}
	}
	return fmt.Errorf("logo data is not a %s image", mediaType)
}

// Agent is a conversational endpoint registered with the platform, uniquely
// identified by (environment_id, identifier). Exactly one of MeshSubject,
// BridgeURL, or Managed is meaningful, selected by Runtime.
type Agent struct {
	ID             string          `db:"id"`
	OrgID          string          `db:"org_id"`
	EnvID          string          `db:"environment_id"`
	Identifier     string          `db:"identifier"`
	Name           string          `db:"name"`
	Description    string          `db:"description"`
	Runtime        string          `db:"runtime"` // mesh|bridge|managed
	Active         bool            `db:"active"`
	MeshSubject    string          `db:"mesh_subject"` // runtime=mesh: prompt subject
	BridgeURL      string          `db:"bridge_url"`   // runtime=bridge
	Managed        json.RawMessage `db:"managed"`      // runtime=managed: {integration_id, system_prompt, tools[], mcp_servers[], skills[]}
	WelcomeMessage string          `db:"welcome_message"`
	// Logo is a data: URI of a png, jpeg or webp image (see ValidateLogo);
	// empty means none.
	Logo string `db:"logo"`
	// Labels are free-form key/value tags, e.g. {"owner":"lou"}, a client uses
	// to scope which agents a user may pick. notify gives none a meaning.
	Labels    map[string]string `db:"labels"`
	CreatedAt string            `db:"created_at"`
	UpdatedAt string            `db:"updated_at"`
}

// AgentLink connects an agent to a channel integration, keyed by
// (agent_id, integration_id) within a scope.
type AgentLink struct {
	ID            string          `db:"id"`
	OrgID         string          `db:"org_id"`
	EnvID         string          `db:"environment_id"`
	AgentID       string          `db:"agent_id"`
	IntegrationID string          `db:"integration_id"`
	ConnectedAt   string          `db:"connected_at"`
	Config        json.RawMessage `db:"config"`
}

// Conversation is one thread between an agent and a platform-side party,
// keyed by (agent_id, integration_id, platform_thread_id).
type Conversation struct {
	ID               string `db:"id"`
	OrgID            string `db:"org_id"`
	EnvID            string `db:"environment_id"`
	AgentID          string `db:"agent_id"`
	IntegrationID    string `db:"integration_id"`
	PlatformThreadID string `db:"platform_thread_id"` // slack thread_ts | tg chat id | inbox conv id
	SubscriberID     string `db:"subscriber_id"`      // the human, when resolvable
	Status           string `db:"status"`
	CreatedAt        string `db:"created_at"`
	LastActivityAt   string `db:"last_activity_at"`
}

// Activity is one append-only entry in a conversation's turn log. Content is
// the JSON body {parts:[{text}], action?, attachments?}; RunID joins an
// agent-role activity back to its dagnats turn run.
type Activity struct {
	ID             string          `db:"id"`
	OrgID          string          `db:"org_id"`
	EnvID          string          `db:"environment_id"`
	ConversationID string          `db:"conversation_id"`
	Role           string          `db:"role"` // user|agent|system|action
	Content        json.RawMessage `db:"content"`
	RunID          string          `db:"run_id"` // dagnats turn run, when role=agent
	CreatedAt      string          `db:"created_at"`
	// ClientMessageID is the caller's idempotency key for a composed user
	// message, unique within the conversation. Empty for every other row.
	ClientMessageID string `db:"client_message_id" json:"-"`
}

// Part is one content fragment of a reply: text now, attachments later.
type Part struct {
	Text string `json:"text"`
}

// Draft is an agent's reply to an email, held for the mailbox owner to review
// and send. notify never sends it. To, Subject, InReplyTo and References are
// advisory: the mailbox owner recomputes recipients and threading itself.
type Draft struct {
	Seq             int64
	ID              string
	OrgID           string
	EnvID           string
	SubscriberID    string
	Mailbox         string
	AgentIdentifier string
	AgentName       string
	ConversationID  string // the conversation's platform thread handle
	ActivityID      string // the agent activity the draft came from
	SourceMessageID string // the mailbox's own id for the message answered
	InReplyTo       string
	References      []string
	Subject         string
	To              []string
	Text            string
	CreatedAt       string
}

// SuggestedAction is an optional button a capable channel renders alongside a
// reply; clicking it feeds Value back as the next user message.
type SuggestedAction struct {
	Label string `json:"label"`
	Value string `json:"value"`
}

// Turn is one inbound message plus the bounded context a runtime needs to
// answer it. History carries the last N activities oldest-first, so runtimes
// stay stateless per turn and need no session affinity with the platform.
type Turn struct {
	Agent        Agent
	Conversation Conversation
	History      []Activity // last N=20, oldest first
	Message      Activity   // the inbound user activity
}

// Reply is a runtime's answer to a Turn.
type Reply struct {
	Parts   []Part
	Actions []SuggestedAction
}

// Runtime executes one conversation turn. Implementations (mesh, bridge,
// managed) hide their entire wire protocol behind Dispatch; nothing above the
// runtime packages sees NATS subjects, HTTP signatures, or model calls.
type Runtime interface {
	Dispatch(ctx context.Context, t Turn) (Reply, error)
}

// PermanentError marks a Dispatch failure that retrying cannot fix: a
// disallowed subject, a protocol rejection, an explicit task failure. The
// turn worker treats unmarked Dispatch errors as transient (engine retries)
// and permanent ones as non-retriable (straight to the DLQ).
type PermanentError struct{ Err error }

func (e *PermanentError) Error() string { return e.Err.Error() }
func (e *PermanentError) Unwrap() error { return e.Err }

// NewPermanentError wraps err as a permanent Dispatch failure.
func NewPermanentError(err error) error { return &PermanentError{Err: err} }

// IsPermanent reports whether err is (or wraps) a PermanentError.
func IsPermanent(err error) bool {
	var pe *PermanentError
	return errors.As(err, &pe)
}
