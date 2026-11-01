package steptype

import (
	"encoding/json"
	"fmt"
	"net/url"

	"github.com/danmestas/dagnats/dag"

	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/engine"
	"github.com/danmestas/notify/internal/names"
)

// agentMessage is the agent_message ACTION step type: it posts the rendered
// text into a conversation of a registered agent and starts the agent's turn,
// in process, so a triggered event can wake an agent (danmestas/notify#4).
// The handler reads agent/text/conversation/failurePolicy from the step's
// controlValues at run time, as http_request does.
type agentMessage struct{}

func (agentMessage) Name() string        { return "agent_message" }
func (agentMessage) DisplayName() string { return "Agent Message" }

func (agentMessage) DefaultControlValues() json.RawMessage {
	return json.RawMessage(`{"agent":"","text":"","conversation":"","failurePolicy":"fail_closed","reviewDraft":false}`)
}

func (agentMessage) ControlMap(cv json.RawMessage) map[string]string {
	return flattenControlValues(cv)
}

func (agentMessage) EditorPartial() string { return "wf-step-fields-agent_message" }

func (agentMessage) ParseForm(form url.Values) (json.RawMessage, error) {
	failurePolicy := formField(form, "failurePolicy")
	if failurePolicy == "" {
		failurePolicy = "fail_closed"
	}
	return marshalControlValues(map[string]any{
		"agent":         formField(form, "agent"),
		"text":          formField(form, "text"),
		"conversation":  formField(form, "conversation"),
		"failurePolicy": failurePolicy,
		"reviewDraft":   formField(form, "reviewDraft") == "on" || formField(form, "reviewDraft") == "true",
	})
}

func (agentMessage) Validate(cv json.RawMessage) error {
	_, err := AgentMessageConfig(cv)
	return err
}

func (agentMessage) Compile(step domain.Step) (engine.StepDef, error) {
	if _, err := AgentMessageConfig(step.ControlValues); err != nil {
		return engine.StepDef{}, err
	}
	return engine.StepDef{ID: step.StepID, Task: names.AgentMessage, Type: dag.StepTypeNormal}, nil
}

func (agentMessage) TaskTypes() []string { return []string{names.AgentMessage} }

// AgentMessageCfg is an agent_message step's control values. Agent, Text and
// Conversation are templates rendered against the run at send time, so an
// event can name its own agent ({{payload.agent}}).
type AgentMessageCfg struct {
	Agent         string `json:"agent"`
	Text          string `json:"text"`
	Conversation  string `json:"conversation"`
	FailurePolicy string `json:"failurePolicy"`
	// ReviewDraft makes the agent's reply a draft for the mailbox owner to
	// review and send, instead of a plain reply. The trigger payload must be
	// an inbound-mail event (mailbox, messageId, and the threading fields).
	ReviewDraft bool `json:"reviewDraft"`
}

// AgentMessageConfig parses and validates an agent_message step's control
// values. agent and text may be empty while a step is being authored; the
// handler fails the step if they still are when it runs. failurePolicy
// defaults to fail_closed.
func AgentMessageConfig(cv json.RawMessage) (AgentMessageCfg, error) {
	var cfg AgentMessageCfg
	if len(cv) > 0 {
		if err := json.Unmarshal(cv, &cfg); err != nil {
			return AgentMessageCfg{}, fmt.Errorf("agent_message: invalid control values: %w", err)
		}
	}
	if cfg.FailurePolicy == "" {
		cfg.FailurePolicy = "fail_closed"
	}
	switch cfg.FailurePolicy {
	case "fail_closed", "continue":
	default:
		return AgentMessageCfg{}, fmt.Errorf("agent_message: failurePolicy %q is not one of fail_closed/continue", cfg.FailurePolicy)
	}
	return cfg, nil
}
