package console

import (
	"encoding/json"

	"github.com/danmestas/notify/internal/aibuilder"
	"github.com/danmestas/notify/internal/domain"
)

// draftFromWorkflow converts a persisted workflow into a WorkflowDraft the
// AgentLoop can seed a refine run from: the name + trigger identifier carry over,
// and the JSON steps column is decoded into the draft's []domain.Step. The
// trigger identifier is immutable, so seeding it lets setWorkflowMetadata (which
// only writes an empty identifier) leave it untouched. The builder view
// (GET /console/workflows/ai?slug=) seeds a refine session's canvas + draft from
// this.
func draftFromWorkflow(wf domain.Workflow) (aibuilder.WorkflowDraft, error) {
	draft := aibuilder.WorkflowDraft{
		Name:              wf.Name,
		TriggerIdentifier: wf.TriggerIdentifier,
		PayloadSchema:     wf.PayloadSchema,
	}
	if len(wf.Steps) > 0 {
		if err := json.Unmarshal(wf.Steps, &draft.Steps); err != nil {
			return aibuilder.WorkflowDraft{}, err
		}
	}
	return draft, nil
}
