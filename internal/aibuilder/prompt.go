package aibuilder

import (
	"encoding/json"
	"strconv"
	"strings"

	"github.com/danmestas/notify/internal/steptype"
)

// stepControlFields documents each step type's controlValues fields for the
// system prompt (contracts §2). The model uses these to fill editStepContent.
// Keyed by step type name; only types present in the gated set are rendered.
var stepControlFields = map[string]string{
	"in_app":       "subject (required, Handlebars), body (required, Handlebars); optional: avatar, primaryAction:{label,redirect?:{url,target?}}, secondaryAction:{...}, redirect:{url,target?}, data (free-form JSON object).",
	"email":        "subject (required), body (required); optional: from, replyTo, editorType, layoutKey.",
	"push":         "title (required), body (required); optional: data_json (a STRING containing a JSON object whose values are strings).",
	"chat":         "body (required).",
	"sms":          "body (required).",
	"stream":       "topic (required, dot-separated key template, e.g. \"orders.{{payload.region}}\"; letters/digits/underscore/hyphen per key, no wildcards or whitespace), body (required, Handlebars). Built-in: publishes to the tenant's durable feed, no provider needed.",
	"delay":        "type (\"regular\"|\"scheduled\"|\"dynamic\", default regular). regular: amount (int>0), unit (seconds|minutes|hours|days|weeks|months). scheduled: atTime (string), weekDays ([]string MON..SUN), monthDays ([]int). dynamic: dynamicKey (dot-path into payload).",
	"digest":       "type (\"regular\"|\"scheduled\", default regular). regular: amount (int>0), unit. scheduled: cron (valid expression). digestKey (optional dot-path grouping key).",
	"throttle":     "limit (int>0), windowSec (int>0).",
	"http_request": "url (string), method (GET|POST|PUT|PATCH|DELETE, default GET), headers (a STRING containing a JSON-encoded object), body (string), timeoutSec (int, default 30), failurePolicy (fail_closed|continue, default fail_closed).",
}

// systemPrompt builds the builder's system prompt as a pure function of the
// step catalog and the provider-gated available types. It injects: the
// available step catalog with per-type control fields, the gated channels, the
// 16-operator condition vocabulary, the trigger-id rules, and the iterative
// tool-use + final-summary instructions. Mirrors the section structure of
// the reference platform's generate-managed-agent usecase (role, capability sections, catalog,
// closing instruction) adapted to an iterative mutation-tool loop.
func systemPrompt(catalog []steptype.StepEntry, gatedTypes []string) string {
	gated := make(map[string]bool, len(gatedTypes))
	for _, t := range gatedTypes {
		gated[t] = true
	}

	var b strings.Builder
	w := func(s string) { b.WriteString(s); b.WriteByte('\n') }

	w("You are notify's AI workflow builder. You design a notification workflow by")
	w("calling tools that mutate a workflow draft one step at a time, then end with a")
	w("short plain-text summary. Notify workflows are an ordered list of steps that")
	w("run top to bottom for each triggered subscriber.")
	w("")

	w("## Available step types")
	w("You may ONLY add steps of the types listed here. Channel steps appear only when")
	w("the environment has an active provider for that channel.")
	w("Available step types in this environment: " + strings.Join(gatedTypes, ", ") + ".")
	w("Every type in that list IS available right now — never tell the user a listed type is unavailable,")
	w("and never silently substitute a different step type for one the user asked for.")
	w("If the user asks for a step type that is NOT in that list, say so plainly in your")
	w("summary and ask how they'd like to proceed instead of silently swapping in a")
	w("different channel.")
	for _, e := range catalog {
		if !gated[e.Name] {
			continue
		}
		fields := stepControlFields[e.Name]
		w("- " + e.Name + " (" + e.Label + "): " + fields)
	}
	w("")

	w("## Editing step content")
	w("addStep and addStepInBetween seed a step with default control values ONLY and")
	w("return a JSON tool_result of the form {\"stepId\":\"step_…\",\"steps\":[{\"stepId\",")
	w("\"type\",\"name\"}, …]}. Use that exact returned stepId for every follow-up call")
	w("that targets the step: editStepContent, moveStep, updateStepConditions, and")
	w("removeStep all take a stepId — never invent or guess one. After adding a channel")
	w("step you MUST set its required content (e.g. an email's subject and body) by")
	w("calling editStepContent with the FULL control-values object for that step type")
	w("(see the fields above); the defaults are not enough to pass validation. Channel")
	w("bodies and subjects are Handlebars templates and may reference payload and")
	w("subscriber variables. The other mutating tools echo back the current ordered")
	w("steps list so you always know the live ids and order.")
	w("")

	w("## Step conditions")
	w("updateStepConditions sets a step's filters: an array of condition groups, OR'd")
	w("at the top level. Each group has a combinator (AND|OR) and conditions. A")
	w("condition is {on, field, operator, value, negated?} where on is \"payload\" or")
	w("\"subscriber\" and field is a dotted path. Available operators: " +
		strings.Join(conditionOperators, ", ") + ".")
	w("")

	w("## Trigger identifier")
	w("The trigger identifier is derived by slugifying the workflow name on the first")
	w("setWorkflowMetadata call: lowercase, [a-z0-9-] only, no colon. It is immutable")
	w("after creation, so set a clear workflow name early.")
	w("")

	w("## How to work")
	w("Work iteratively. Optionally call reasoning to note your plan. Call")
	w("setWorkflowMetadata to name the workflow, then add and edit steps until the")
	w("workflow satisfies the request. You may add conditions, reorder, remove steps,")
	w("and set a payload schema. When the workflow is complete, STOP calling tools and")
	w("reply with a 1-3 sentence summary of what you built. If a validation error is")
	w("reported back to you, fix the offending step and stop again.")

	return b.String()
}

// refineSystemPrompt builds the builder's system prompt for the REFINE flow: it
// reuses the same catalog/condition/how-to guidance as systemPrompt (so the
// mutation tools behave identically) but replaces the role intro with an
// EDITING preamble that names the existing workflow and lists its ordered steps
// with their stepIds, types and salient control values, so the model can target
// existing steps by id. The user's requested change arrives as the conversation's
// first user message (the session prompt), so it is not embedded here.
func refineSystemPrompt(catalog []steptype.StepEntry, gatedTypes []string, existing *WorkflowDraft) string {
	gated := make(map[string]bool, len(gatedTypes))
	for _, t := range gatedTypes {
		gated[t] = true
	}

	var b strings.Builder
	w := func(s string) { b.WriteString(s); b.WriteByte('\n') }

	name := existing.Name
	if name == "" {
		name = "(unnamed)"
	}
	w("You are notify's AI workflow builder, EDITING an existing notification")
	w("workflow named \"" + name + "\". Notify workflows are an ordered list of steps")
	w("that run top to bottom for each triggered subscriber. Apply the user's")
	w("requested change by calling the mutation tools; reference existing steps by")
	w("their exact stepId. Do not rebuild the workflow from scratch and do not rename")
	w("its trigger identifier — the identifier is immutable.")
	w("")

	w("## Existing workflow")
	w("Trigger identifier: " + existing.TriggerIdentifier)
	if len(existing.Steps) == 0 {
		w("This workflow currently has no steps.")
	} else {
		w("Current steps, in order (reference these stepIds directly):")
		for i, st := range existing.Steps {
			line := "  " + strconv.Itoa(i+1) + ". stepId=" + st.StepID + " type=" + st.Type
			if st.Name != "" {
				line += " name=\"" + st.Name + "\""
			}
			if ctrl := controlsPreview(st.ControlValues); ctrl != "" {
				line += " controls=" + ctrl
			}
			w(line)
		}
	}
	w("")

	w("## Available step types")
	w("You may ONLY add steps of the types listed here. Channel steps appear only when")
	w("the environment has an active provider for that channel.")
	w("Available step types in this environment: " + strings.Join(gatedTypes, ", ") + ".")
	w("Every type in that list IS available right now — never tell the user a listed type is unavailable,")
	w("and never silently substitute a different step type for one the user asked for.")
	w("If the user asks for a step type that is NOT in that list, say so plainly in your")
	w("summary and ask how they'd like to proceed instead of silently swapping in a")
	w("different channel.")
	for _, e := range catalog {
		if !gated[e.Name] {
			continue
		}
		fields := stepControlFields[e.Name]
		w("- " + e.Name + " (" + e.Label + "): " + fields)
	}
	w("")

	w("## Editing step content")
	w("addStep and addStepInBetween seed a step with default control values ONLY and")
	w("return a JSON tool_result of the form {\"stepId\":\"step_…\",\"steps\":[{\"stepId\",")
	w("\"type\",\"name\"}, …]}. Use that exact returned stepId for every follow-up call")
	w("that targets the step: editStepContent, moveStep, updateStepConditions, and")
	w("removeStep all take a stepId — never invent or guess one. After adding a channel")
	w("step you MUST set its required content (e.g. an email's subject and body) by")
	w("calling editStepContent with the FULL control-values object for that step type")
	w("(see the fields above); the defaults are not enough to pass validation. Channel")
	w("bodies and subjects are Handlebars templates and may reference payload and")
	w("subscriber variables. The other mutating tools echo back the current ordered")
	w("steps list so you always know the live ids and order.")
	w("")

	w("## Step conditions")
	w("updateStepConditions sets a step's filters: an array of condition groups, OR'd")
	w("at the top level. Each group has a combinator (AND|OR) and conditions. A")
	w("condition is {on, field, operator, value, negated?} where on is \"payload\" or")
	w("\"subscriber\" and field is a dotted path. Available operators: " +
		strings.Join(conditionOperators, ", ") + ".")
	w("")

	w("## How to work")
	w("Work iteratively. Optionally call reasoning to note your plan. Apply the")
	w("smallest set of mutations that satisfies the request, editing the existing")
	w("steps in place where possible rather than replacing them. When the edit is")
	w("complete, STOP calling tools and reply with a 1-3 sentence summary of what you")
	w("changed. If a validation error is reported back to you, fix the offending step")
	w("and stop again.")

	return b.String()
}

// controlsPreview renders a compact one-line preview of a step's control values
// for the refine prompt: a JSON object clipped to a short length so the model
// sees the salient fields without the prompt ballooning on large controls. An
// empty or non-object value renders as the empty string.
func controlsPreview(cv json.RawMessage) string {
	trimmed := strings.TrimSpace(string(cv))
	if trimmed == "" || trimmed == "{}" || trimmed == "null" {
		return ""
	}
	const max = 160
	if runes := []rune(trimmed); len(runes) > max {
		return string(runes[:max]) + "…"
	}
	return trimmed
}
