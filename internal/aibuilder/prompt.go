package aibuilder

import (
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
	w("the environment has an active provider for that channel; never assume an")
	w("unavailable channel.")
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
