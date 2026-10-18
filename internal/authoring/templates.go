package authoring

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/danmestas/notify/internal/domain"
)

// Template is a workflow notify ships ready to use. Applying it creates the
// workflow in the caller's environment under the template's trigger
// identifier.
type Template struct {
	Name        string
	Description string
	Workflow    EnsureInput
}

// ErrUnknownTemplate is returned by ApplyTemplate for a name no template has.
var ErrUnknownTemplate = errors.New("authoring: unknown workflow template")

// Templates lists the built-in workflow templates.
func Templates() []Template {
	return []Template{inboundMailTemplate()}
}

// inboundMailTemplate answers each inbound email with a draft from the agent
// the event names. A mailbox with no agent sends an empty agent and is
// skipped. The payload is a mail client's inbound-mail event (mailbox,
// messageId, rfcMessageId, references, subject, from, and text, the body
// shared only when the mailbox has an agent).
func inboundMailTemplate() Template {
	return Template{
		Name:        "inbound-mail",
		Description: "Answer each inbound email with a draft from the agent the mailbox chose, for the owner to review and send.",
		Workflow: EnsureInput{
			Name:              "Inbound mail drafts",
			TriggerIdentifier: "inbound-mail",
			Steps: []domain.Step{{
				StepID: "draft",
				Type:   "agent_message",
				ControlValues: json.RawMessage(`{` +
					`"agent":"{{payload.agent}}",` +
					`"text":"New email from {{payload.from}}\nSubject: {{payload.subject}}\n\n{{payload.text}}",` +
					`"conversation":"{{payload.mailbox}}:{{payload.messageId}}",` +
					`"failurePolicy":"fail_closed",` +
					`"reviewDraft":true}`),
			}},
		},
	}
}

// ApplyTemplate creates the named template's workflow in scope and publishes
// it. A workflow already under that trigger identifier is left exactly as it
// is, edited or not, and returned with created false: applying a template
// never overwrites someone's workflow.
func (s *Service) ApplyTemplate(ctx context.Context, scope domain.Scope, name string) (domain.Workflow, bool, error) {
	var tmpl *Template
	for _, t := range Templates() {
		if t.Name == name {
			tmpl = &t
			break
		}
	}
	if tmpl == nil {
		return domain.Workflow{}, false, ErrUnknownTemplate
	}
	wf, outcome, err := s.Ensure(ctx, scope, tmpl.Workflow.TriggerIdentifier, tmpl.Workflow, OnConflictKeep)
	var conflict *ErrDefinitionConflict
	switch {
	case errors.As(err, &conflict):
		return conflict.Existing, false, nil
	case err != nil:
		return domain.Workflow{}, false, err
	case outcome != EnsureCreated:
		return wf, false, nil
	}
	if err := s.Publish(ctx, scope, wf.TriggerIdentifier); err != nil {
		return domain.Workflow{}, false, err
	}
	return wf, true, nil
}
