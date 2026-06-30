package aibuilder

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/danmestas/notify/internal/authoring"
	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/steptype"
)

// DefaultMaxSteps caps how many steps a single draft may hold. It bounds a
// runaway model that keeps appending steps and keeps drafts within a size the
// canvas and downstream compile path comfortably handle.
const DefaultMaxSteps = 30

// EventKind classifies a builder Event so a downstream consumer (B3's SSE
// bridge) can route it to the right UI region.
type EventKind string

const (
	// KindReasoning is the model narrating its plan (free text or a reasoning
	// tool call). No draft change.
	KindReasoning EventKind = "reasoning"
	// KindTool announces a mutation tool call about to be reflected on the canvas.
	KindTool EventKind = "tool"
	// KindCanvas carries a fresh draft snapshot after a mutation.
	KindCanvas EventKind = "canvas"
	// KindDone marks a validated, complete draft with the model's final summary.
	KindDone EventKind = "done"
	// KindError marks an unrecoverable failure (validation still failing after
	// the self-correct round, or a transport error).
	KindError EventKind = "error"
)

// Event is one observation emitted during a builder run. Draft is a snapshot of
// the draft at emit time (safe to read after the run continues mutating).
type Event struct {
	Kind     EventKind
	Text     string
	ToolName string
	Draft    *WorkflowDraft
	Err      error
}

// Builder drives the iterative tool-use loop against a Client. It is safe to
// reuse across runs; each Run mutates the caller-owned draft in place.
type Builder struct {
	client    Client
	reg       *steptype.Registry
	ints      IntegrationLister
	model     string
	maxRounds int
	maxSteps  int
}

// Option configures a Builder.
type Option func(*Builder)

// WithModel overrides the default Claude model.
func WithModel(model string) Option {
	return func(b *Builder) {
		if model != "" {
			b.model = model
		}
	}
}

// WithMaxRounds caps the number of model turns before the loop gives up. The
// self-correct round counts toward this budget.
func WithMaxRounds(n int) Option {
	return func(b *Builder) {
		if n > 0 {
			b.maxRounds = n
		}
	}
}

// WithMaxSteps caps how many steps a single draft may hold. Add-step tool calls
// past the cap are rejected with an error tool result (fed back to the model),
// not a hard failure. Non-positive values are ignored.
func WithMaxSteps(n int) Option {
	return func(b *Builder) {
		if n > 0 {
			b.maxSteps = n
		}
	}
}

// WithRegistry overrides the step-type registry (defaults to steptype.Default()).
func WithRegistry(reg *steptype.Registry) Option {
	return func(b *Builder) {
		if reg != nil {
			b.reg = reg
		}
	}
}

// NewBuilder constructs a Builder over a Client and an integration lister (used
// for provider gating).
func NewBuilder(client Client, ints IntegrationLister, opts ...Option) *Builder {
	b := &Builder{
		client:    client,
		reg:       steptype.Default(),
		ints:      ints,
		model:     DefaultModel,
		maxRounds: 24,
		maxSteps:  DefaultMaxSteps,
	}
	for _, opt := range opts {
		opt(b)
	}
	return b
}

// Run drives the model to build out draft in response to userPrompt. It gates
// the available step types by the scope's active integrations, calls the model
// with the mutation tools (tool_choice auto), applies each tool call to draft,
// and emits an Event per reasoning / tool / canvas step. When the model stops
// calling tools, the draft is validated by compiling it through the pure
// authoring path; a compile failure is fed back for exactly one self-correct
// round before a terminal done/error Event. emit must not be nil.
func (b *Builder) Run(ctx context.Context, scope domain.Scope, draft *WorkflowDraft, userPrompt string, emit func(Event)) error {
	integrations, err := b.ints.List(ctx, scope)
	if err != nil {
		return fmt.Errorf("aibuilder: list integrations: %w", err)
	}
	gatedTypes := AvailableStepTypes(b.reg, integrations)
	gatedSet := make(map[string]bool, len(gatedTypes))
	for _, t := range gatedTypes {
		gatedSet[t] = true
	}

	sys := systemPrompt(b.reg.Catalog(), gatedTypes)
	tools := buildTools(gatedTypes)

	conversation := []Message{{Role: RoleUser, Text: userPrompt}}
	selfCorrected := false

	for round := 0; round < b.maxRounds; round++ {
		resp, err := b.client.Message(ctx, MessageRequest{
			Model:      b.model,
			System:     sys,
			Messages:   conversation,
			Tools:      tools,
			ToolChoice: "auto",
			MaxTokens:  DefaultMaxTokens,
		})
		if err != nil {
			emit(Event{Kind: KindError, Err: err})
			return fmt.Errorf("aibuilder: model call: %w", err)
		}

		// Record the assistant turn verbatim so the next request carries the
		// model's own tool_use blocks (the API requires tool_result to follow
		// the matching tool_use).
		conversation = append(conversation, Message{
			Role:      RoleAssistant,
			Text:      resp.Text,
			ToolCalls: resp.ToolCalls,
		})

		if resp.Text != "" {
			emit(Event{Kind: KindReasoning, Text: resp.Text, Draft: draft.clone()})
		}

		// No tool calls => the model is done. Validate and finalize.
		if len(resp.ToolCalls) == 0 {
			vErr := b.validate(scope, draft)
			if vErr == nil {
				emit(Event{Kind: KindDone, Text: resp.Text, Draft: draft.clone()})
				return nil
			}
			if selfCorrected {
				emit(Event{Kind: KindError, Err: vErr, Draft: draft.clone()})
				return vErr
			}
			selfCorrected = true
			conversation = append(conversation, Message{
				Role: RoleUser,
				Text: "The workflow failed validation: " + vErr.Error() +
					". Fix the offending step, then stop.",
			})
			continue
		}

		// Apply each tool call, emitting events and collecting results to feed
		// back to the model.
		var results []ToolResult
		for _, tc := range resp.ToolCalls {
			ev, applyErr := b.applyOne(draft, gatedSet, tc)
			if applyErr != nil {
				results = append(results, ToolResult{
					ToolCallID: tc.ID,
					Content:    applyErr.Error(),
					IsError:    true,
				})
				continue
			}
			switch ev.Kind {
			case KindReasoning:
				ev.Draft = draft.clone()
				emit(ev)
			case KindCanvas:
				emit(Event{Kind: KindTool, ToolName: tc.Name, Draft: draft.clone()})
				ev.Draft = draft.clone()
				emit(ev)
			}
			results = append(results, ToolResult{ToolCallID: tc.ID, Content: "ok"})
		}
		conversation = append(conversation, Message{Role: RoleUser, ToolResults: results})
	}

	err = fmt.Errorf("aibuilder: exceeded max rounds (%d) without completing", b.maxRounds)
	emit(Event{Kind: KindError, Err: err, Draft: draft.clone()})
	return err
}

// applyOne enforces provider gating for add-step tools (the runtime mirror of
// the tool-schema enum) and then applies the tool call to the draft.
func (b *Builder) applyOne(draft *WorkflowDraft, gatedSet map[string]bool, tc ToolCall) (Event, error) {
	if tc.Name == toolAddStep || tc.Name == toolAddStepInBetween {
		if len(draft.Steps) >= b.maxSteps {
			return Event{}, fmt.Errorf("%s: step limit reached (%d); remove a step before adding another", tc.Name, b.maxSteps)
		}
		var a struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(tc.Input, &a); err != nil {
			return Event{}, fmt.Errorf("%s: %w", tc.Name, err)
		}
		if !gatedSet[a.Type] {
			return Event{}, fmt.Errorf("%s: step type %q is not available in this environment", tc.Name, a.Type)
		}
	}
	return applyToolCall(draft, b.reg, newStepID, tc.Name, tc.Input)
}

// validate compiles the draft through the pure authoring path. It builds no
// store rows and never persists (that is B2's confirm step); a compile failure
// is wrapped as *authoring.ErrCompileFailure so callers and the self-correct
// round see the same shape the HTTP layer maps to 422.
func (b *Builder) validate(scope domain.Scope, draft *WorkflowDraft) error {
	// Shape checks the compile path does not cover: an unnamed, untriggered, or
	// stepless draft compiles to a degenerate workflow rather than an error.
	// Wrapping them as ErrCompileFailure keeps the self-correct round and the
	// HTTP layer (422) seeing the one validation-failure shape.
	if draft.Name == "" {
		return &authoring.ErrCompileFailure{Cause: errors.New("workflow name is required (call setWorkflowMetadata)")}
	}
	if draft.TriggerIdentifier == "" {
		return &authoring.ErrCompileFailure{Cause: errors.New("workflow trigger identifier is required")}
	}
	if len(draft.Steps) == 0 {
		return &authoring.ErrCompileFailure{Cause: errors.New("workflow must have at least one step")}
	}

	stepsJSON, err := json.Marshal(draft.Steps)
	if err != nil {
		return fmt.Errorf("aibuilder: marshal steps: %w", err)
	}
	wf := domain.Workflow{
		EnvID:             scope.EnvID,
		Name:              draft.Name,
		TriggerIdentifier: draft.TriggerIdentifier,
		Steps:             stepsJSON,
		PayloadSchema:     draft.PayloadSchema,
	}
	if _, err := authoring.Compile(wf); err != nil {
		return &authoring.ErrCompileFailure{Cause: err}
	}
	return nil
}
