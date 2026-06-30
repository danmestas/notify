package aibuilder

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/danmestas/notify/internal/delivery/filter"
	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/steptype"
)

// WorkflowDraft is the mutable in-memory workflow the model edits via tool
// calls. It maps 1:1 onto authoring.WorkflowInput plus a payload schema; the
// builder validates it by compiling a domain.Workflow built from these fields.
type WorkflowDraft struct {
	Name              string          `json:"name"`
	TriggerIdentifier string          `json:"triggerIdentifier"`
	Steps             []domain.Step   `json:"steps"`
	PayloadSchema     json.RawMessage `json:"payloadSchema,omitempty"`
}

// clone returns a deep-enough copy for an Event snapshot: the Steps slice is
// copied so a later in-place element replacement (editStepContent) does not
// mutate a previously emitted snapshot.
func (d *WorkflowDraft) clone() *WorkflowDraft {
	cp := &WorkflowDraft{
		Name:              d.Name,
		TriggerIdentifier: d.TriggerIdentifier,
		PayloadSchema:     d.PayloadSchema,
	}
	if d.Steps != nil {
		cp.Steps = make([]domain.Step, len(d.Steps))
		copy(cp.Steps, d.Steps)
	}
	return cp
}

// idGen produces a fresh, unique StepID. Tests inject a deterministic counter;
// production uses newStepID.
type idGen func() string

// newStepID returns a random, collision-resistant step id. Step ids are opaque
// (they key DependsOn chains and message rows), so any unique token works.
func newStepID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return "step_" + hex.EncodeToString(b[:])
}

// slugify lowercases s and collapses every run of non-[a-z0-9] characters to a
// single hyphen, trimming leading/trailing hyphens. It never emits a colon, so
// the result is safe as the NATS KV key suffix authoring requires
// ("Welcome Email!" -> "welcome-email").
func slugify(s string) string {
	var b strings.Builder
	lastHyphen := false
	for _, r := range strings.ToLower(s) {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
			lastHyphen = false
		default:
			if !lastHyphen && b.Len() > 0 {
				b.WriteByte('-')
				lastHyphen = true
			}
		}
	}
	return strings.Trim(b.String(), "-")
}

// applyToolCall mutates draft per one tool call and returns the Event to emit
// plus the tool_result content the caller feeds back to the model. It is pure
// over (draft, registry, id generator): no I/O, no model access. An unknown step
// id, an unregistered step type, or malformed args returns an error the caller
// feeds back to the model. reg gates addStep's registration check; channel
// availability gating is enforced one layer up in the loop.
//
// The result content is what lets a real model make progress: addStep /
// addStepInBetween return the newly minted stepId (which the model could not
// otherwise know) plus the current ordered step list, so the very next turn can
// target it with editStepContent / moveStep / updateStepConditions / removeStep.
// The other mutating tools echo the ordered step list; the non-step tools return
// a bare "ok".
func applyToolCall(draft *WorkflowDraft, reg *steptype.Registry, nextID idGen, toolName string, args json.RawMessage) (Event, string, error) {
	switch toolName {
	case toolReasoning:
		var a struct {
			Label   string `json:"label"`
			Thought string `json:"thought"`
		}
		_ = json.Unmarshal(args, &a)
		text := a.Thought
		if text == "" {
			text = a.Label
		}
		return Event{Kind: KindReasoning, ToolName: toolName, Text: text}, "ok", nil

	case toolSetWorkflowMetadata:
		var a struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(args, &a); err != nil {
			return Event{}, "", fmt.Errorf("setWorkflowMetadata: %w", err)
		}
		if a.Name == "" {
			return Event{}, "", fmt.Errorf("setWorkflowMetadata: name is required")
		}
		draft.Name = a.Name
		if draft.TriggerIdentifier == "" {
			draft.TriggerIdentifier = slugify(a.Name)
		}
		return Event{Kind: KindCanvas, ToolName: toolName}, "ok", nil

	case toolAddStep:
		var a struct {
			Type string `json:"type"`
			Name string `json:"name"`
		}
		if err := json.Unmarshal(args, &a); err != nil {
			return Event{}, "", fmt.Errorf("addStep: %w", err)
		}
		st, err := newStep(reg, nextID, a.Type, a.Name)
		if err != nil {
			return Event{}, "", err
		}
		draft.Steps = append(draft.Steps, st)
		return Event{Kind: KindCanvas, ToolName: toolName}, addStepResult(st.StepID, draft.Steps), nil

	case toolAddStepInBetween:
		var a struct {
			Type        string `json:"type"`
			Name        string `json:"name"`
			AfterStepID string `json:"afterStepId"`
		}
		if err := json.Unmarshal(args, &a); err != nil {
			return Event{}, "", fmt.Errorf("addStepInBetween: %w", err)
		}
		idx, ok := indexOf(draft.Steps, a.AfterStepID)
		if !ok {
			return Event{}, "", fmt.Errorf("addStepInBetween: unknown afterStepId %q", a.AfterStepID)
		}
		st, err := newStep(reg, nextID, a.Type, a.Name)
		if err != nil {
			return Event{}, "", err
		}
		draft.Steps = insertAt(draft.Steps, idx+1, st)
		return Event{Kind: KindCanvas, ToolName: toolName}, addStepResult(st.StepID, draft.Steps), nil

	case toolEditStepContent:
		var a struct {
			StepID        string          `json:"stepId"`
			ControlValues json.RawMessage `json:"controlValues"`
		}
		if err := json.Unmarshal(args, &a); err != nil {
			return Event{}, "", fmt.Errorf("editStepContent: %w", err)
		}
		if !isJSONObject(a.ControlValues) {
			return Event{}, "", fmt.Errorf("editStepContent: controlValues must be a JSON object")
		}
		idx, ok := indexOf(draft.Steps, a.StepID)
		if !ok {
			return Event{}, "", fmt.Errorf("editStepContent: unknown stepId %q", a.StepID)
		}
		draft.Steps[idx].ControlValues = a.ControlValues
		return Event{Kind: KindCanvas, ToolName: toolName}, stepsResult(draft.Steps), nil

	case toolUpdateStepConditions:
		var a struct {
			StepID  string         `json:"stepId"`
			Filters []filter.Group `json:"filters"`
		}
		if err := json.Unmarshal(args, &a); err != nil {
			return Event{}, "", fmt.Errorf("updateStepConditions: %w", err)
		}
		idx, ok := indexOf(draft.Steps, a.StepID)
		if !ok {
			return Event{}, "", fmt.Errorf("updateStepConditions: unknown stepId %q", a.StepID)
		}
		raw, err := json.Marshal(a.Filters)
		if err != nil {
			return Event{}, "", fmt.Errorf("updateStepConditions: %w", err)
		}
		draft.Steps[idx].Filters = raw
		return Event{Kind: KindCanvas, ToolName: toolName}, stepsResult(draft.Steps), nil

	case toolRemoveStep:
		var a struct {
			StepID string `json:"stepId"`
		}
		if err := json.Unmarshal(args, &a); err != nil {
			return Event{}, "", fmt.Errorf("removeStep: %w", err)
		}
		idx, ok := indexOf(draft.Steps, a.StepID)
		if !ok {
			return Event{}, "", fmt.Errorf("removeStep: unknown stepId %q", a.StepID)
		}
		draft.Steps = append(draft.Steps[:idx], draft.Steps[idx+1:]...)
		return Event{Kind: KindCanvas, ToolName: toolName}, stepsResult(draft.Steps), nil

	case toolMoveStep:
		var a struct {
			StepID   string `json:"stepId"`
			NewIndex int    `json:"newIndex"`
		}
		if err := json.Unmarshal(args, &a); err != nil {
			return Event{}, "", fmt.Errorf("moveStep: %w", err)
		}
		idx, ok := indexOf(draft.Steps, a.StepID)
		if !ok {
			return Event{}, "", fmt.Errorf("moveStep: unknown stepId %q", a.StepID)
		}
		st := draft.Steps[idx]
		rest := append(draft.Steps[:idx:idx], draft.Steps[idx+1:]...)
		dest := clamp(a.NewIndex, 0, len(rest))
		draft.Steps = insertAt(rest, dest, st)
		return Event{Kind: KindCanvas, ToolName: toolName}, stepsResult(draft.Steps), nil

	case toolUpdatePayloadSchema:
		var a struct {
			Schema json.RawMessage `json:"schema"`
		}
		if err := json.Unmarshal(args, &a); err != nil {
			return Event{}, "", fmt.Errorf("updatePayloadSchema: %w", err)
		}
		draft.PayloadSchema = a.Schema
		return Event{Kind: KindCanvas, ToolName: toolName}, "ok", nil

	default:
		return Event{}, "", fmt.Errorf("unknown tool %q", toolName)
	}
}

// stepRef is the model-facing summary of one draft step: the opaque id plus
// enough context (type, name) to identify it. It is the unit the mutating-tool
// results echo so the model can target any step in a follow-up call.
type stepRef struct {
	StepID string `json:"stepId"`
	Type   string `json:"type"`
	Name   string `json:"name"`
}

// stepsResult renders the current ordered step list as the JSON tool_result
// content a mutating tool feeds back to the model.
func stepsResult(steps []domain.Step) string {
	refs := make([]stepRef, len(steps))
	for i, s := range steps {
		refs[i] = stepRef{StepID: s.StepID, Type: s.Type, Name: s.Name}
	}
	out, err := json.Marshal(struct {
		Steps []stepRef `json:"steps"`
	}{Steps: refs})
	if err != nil {
		return "ok"
	}
	return string(out)
}

// addStepResult renders the tool_result for addStep / addStepInBetween: the
// newly created stepId (which the model has no other way to learn) plus the
// current ordered step list.
func addStepResult(stepID string, steps []domain.Step) string {
	refs := make([]stepRef, len(steps))
	for i, s := range steps {
		refs[i] = stepRef{StepID: s.StepID, Type: s.Type, Name: s.Name}
	}
	out, err := json.Marshal(struct {
		StepID string    `json:"stepId"`
		Steps  []stepRef `json:"steps"`
	}{StepID: stepID, Steps: refs})
	if err != nil {
		return "ok"
	}
	return string(out)
}

// newStep builds a fresh step of typ seeded with its registry default control
// values. An unregistered type is rejected (the registry is the source of truth
// for which types exist).
func newStep(reg *steptype.Registry, nextID idGen, typ, name string) (domain.Step, error) {
	st := reg.Get(typ)
	if st == nil {
		return domain.Step{}, fmt.Errorf("addStep: unknown step type %q", typ)
	}
	return domain.Step{
		StepID:        nextID(),
		Type:          typ,
		Name:          name,
		ControlValues: st.DefaultControlValues(),
	}, nil
}

// isJSONObject reports whether raw is a JSON object ({...}) rather than a scalar
// or array. raw has already been validated as JSON by the enclosing unmarshal,
// so inspecting the first non-whitespace byte is sufficient. An empty/absent
// controlValues is not an object and is rejected.
func isJSONObject(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) > 0 && trimmed[0] == '{'
}

// indexOf returns the position of the step with the given id.
func indexOf(steps []domain.Step, stepID string) (int, bool) {
	for i, s := range steps {
		if s.StepID == stepID {
			return i, true
		}
	}
	return 0, false
}

// insertAt returns steps with st inserted at index i (i is assumed clamped to
// [0, len(steps)]).
func insertAt(steps []domain.Step, i int, st domain.Step) []domain.Step {
	steps = append(steps, domain.Step{})
	copy(steps[i+1:], steps[i:])
	steps[i] = st
	return steps
}

// clamp bounds v to [lo, hi].
func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
