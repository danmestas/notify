package aibuilder

import "encoding/json"

// Tool names mirror the reference platform's AiWorkflowToolsEnum
// (packages/shared/src/types/ai.ts) so the design stays in parity with the
// upstream iterative builder.
const (
	toolReasoning            = "reasoning"
	toolSetWorkflowMetadata  = "setWorkflowMetadata"
	toolAddStep              = "addStep"
	toolAddStepInBetween     = "addStepInBetween"
	toolEditStepContent      = "editStepContent"
	toolUpdateStepConditions = "updateStepConditions"
	toolRemoveStep           = "removeStep"
	toolMoveStep             = "moveStep"
	toolUpdatePayloadSchema  = "updatePayloadSchema"
)

// buildTools assembles the 9 mutation tools the model may call. The addStep and
// addStepInBetween `type` enums are built dynamically from gatedTypes — the
// provider-gated set of available step types — so the model is structurally
// unable to add a step for an unconfigured channel (the reference platform's z.enum guard).
func buildTools(gatedTypes []string) []ToolDef {
	typeEnum := make([]any, len(gatedTypes))
	for i, t := range gatedTypes {
		typeEnum[i] = t
	}

	return []ToolDef{
		{
			Name:        toolReasoning,
			Description: "Record a short note about your plan or the next change before you make it. Does not modify the workflow.",
			InputSchema: schema(props{
				"label":   strProp("A 2-5 word summary of what you are about to do."),
				"thought": strProp("A fuller sentence explaining your reasoning."),
			}, nil),
		},
		{
			Name:        toolSetWorkflowMetadata,
			Description: "Set the workflow's name (and optional description/tags). The trigger identifier is slugified from the name on first set and is immutable thereafter.",
			InputSchema: schema(props{
				"name":        strProp("Human-readable workflow name, e.g. \"Welcome Email\"."),
				"description": strProp("Optional description of what the workflow does."),
				"tags":        arrProp("Optional list of tags.", strProp("")),
			}, []string{"name"}),
		},
		{
			Name:        toolAddStep,
			Description: "Append a new step of the given type to the end of the workflow. The step is seeded with default control values; use editStepContent to fill them in.",
			InputSchema: schema(props{
				"type": enumProp("The step type to add. Only the listed types are available in this environment.", typeEnum),
				"name": strProp("Optional human label for the step."),
			}, []string{"type"}),
		},
		{
			Name:        toolAddStepInBetween,
			Description: "Insert a new step immediately after the step with afterStepId.",
			InputSchema: schema(props{
				"type":        enumProp("The step type to add. Only the listed types are available in this environment.", typeEnum),
				"name":        strProp("Optional human label for the step."),
				"afterStepId": strProp("The stepId after which to insert the new step."),
			}, []string{"type", "afterStepId"}),
		},
		{
			Name:        toolEditStepContent,
			Description: "Replace a step's control values (the channel content / action configuration) with the given object. See the system prompt for each type's fields.",
			InputSchema: schema(props{
				"stepId":        strProp("The stepId to edit."),
				"controlValues": objProp("The full control-values object for this step type."),
			}, []string{"stepId", "controlValues"}),
		},
		{
			Name:        toolUpdateStepConditions,
			Description: "Set a step's filter conditions. filters is an array of condition groups (top-level groups are OR'd). Omit or pass an empty array to always run the step.",
			InputSchema: schema(props{
				"stepId":  strProp("The stepId whose conditions to set."),
				"filters": filtersProp(),
			}, []string{"stepId", "filters"}),
		},
		{
			Name:        toolRemoveStep,
			Description: "Remove the step with the given stepId from the workflow.",
			InputSchema: schema(props{
				"stepId": strProp("The stepId to remove."),
			}, []string{"stepId"}),
		},
		{
			Name:        toolMoveStep,
			Description: "Move the step with the given stepId to a new zero-based index. Out-of-range indices are clamped.",
			InputSchema: schema(props{
				"stepId":   strProp("The stepId to move."),
				"newIndex": intProp("The target zero-based position."),
			}, []string{"stepId", "newIndex"}),
		},
		{
			Name:        toolUpdatePayloadSchema,
			Description: "Set the workflow's trigger payload JSON schema (the shape of variables Handlebars templates and conditions can reference).",
			InputSchema: schema(props{
				"schema": objProp("A JSON Schema object describing the trigger payload."),
			}, []string{"schema"}),
		},
	}
}

// props is a JSON-schema properties map.
type props map[string]any

// schema builds a JSON Schema object blob from properties and required keys.
func schema(p props, required []string) json.RawMessage {
	obj := map[string]any{
		"type":       "object",
		"properties": map[string]any(p),
	}
	if len(required) > 0 {
		obj["required"] = required
	}
	raw, _ := json.Marshal(obj)
	return raw
}

func strProp(desc string) map[string]any {
	m := map[string]any{"type": "string"}
	if desc != "" {
		m["description"] = desc
	}
	return m
}

func intProp(desc string) map[string]any {
	return map[string]any{"type": "integer", "description": desc}
}

func objProp(desc string) map[string]any {
	return map[string]any{"type": "object", "description": desc}
}

func arrProp(desc string, items map[string]any) map[string]any {
	return map[string]any{"type": "array", "description": desc, "items": items}
}

func enumProp(desc string, values []any) map[string]any {
	return map[string]any{"type": "string", "description": desc, "enum": values}
}

// filtersProp is the JSON schema for the updateStepConditions filters argument:
// an array of recursive condition groups mirroring filter.Group / filter.Condition.
func filtersProp() map[string]any {
	condition := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"on":       map[string]any{"type": "string", "description": "Data source: \"payload\" or \"subscriber\"."},
			"field":    strProp("Dotted path within the source, e.g. \"order.total\"."),
			"operator": enumProp("Comparison operator.", operatorEnum()),
			"value":    map[string]any{"description": "The comparand (scalar, list, or [min,max] for BETWEEN)."},
			"negated":  map[string]any{"type": "boolean", "description": "Invert the condition result."},
		},
		"required": []string{"on", "field", "operator"},
	}
	group := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"combinator": enumProp("How to combine conditions within the group.", []any{"AND", "OR"}),
			"conditions": map[string]any{"type": "array", "items": condition},
		},
		"required": []string{"combinator", "conditions"},
	}
	return map[string]any{
		"type":        "array",
		"description": "Condition groups; top-level groups are OR'd together.",
		"items":       group,
	}
}

// operatorEnum is the 16-operator condition vocabulary as JSON enum values.
func operatorEnum() []any {
	out := make([]any, len(conditionOperators))
	for i, op := range conditionOperators {
		out[i] = op
	}
	return out
}

// conditionOperators is the full filter operator vocabulary (contracts §3 / the
// 16 the reference platform FieldOperatorEnum members). IS_NOT_DEFINED is expressed at runtime as
// IS_DEFINED with negated=true, but is offered to the model as a distinct token.
var conditionOperators = []string{
	"EQUAL", "NOT_EQUAL",
	"LARGER", "SMALLER", "LARGER_EQUAL", "SMALLER_EQUAL",
	"IN", "NOT_IN",
	"IS_DEFINED", "IS_NOT_DEFINED",
	"ALL_IN", "ANY_IN",
	"BETWEEN", "NOT_BETWEEN",
	"LIKE", "NOT_LIKE",
}
