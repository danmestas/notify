package console

import (
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"strconv"
	"strings"

	"github.com/aymerick/raymond"

	"github.com/danmestas/notify/internal/delivery/filter"
)

// workflow_step_conditions.go renders the step editor's CONDITIONS row
// authoring UI and the live Handlebars step preview (W4-W3):
//
//   - GET  /console/api/fragments/step-preview            live Handlebars preview
//
// The CONDITIONS fieldset is a fixed pool of stepConditionRows rows,
// server-rendered and revealed client-side; conditionsFromForm/
// conditionsFromFilters convert between the posted form rows and the
// filter.Group wire shape (workflow_builder.go's step-save/step-editor
// handlers call into this file for that conversion).
// stepConditionRow is a single CONDITIONS authoring row, flattened from a
// filter.Condition for form pre-fill. Value is rendered verbatim into a text
// input; On/Operator seed their respective uiSelect value signals.
type stepConditionRow struct {
	On       string
	Field    string
	Operator string
	Value    string
}

// stepConditionRows is the size of the fixed pool of condition rows the editor
// renders server-side and the iteration cap conditionsFromForm reads up to. The
// editor shows the first $condRowCount of them; "Add condition" grows the
// visible count up to this pool size. No nested groups.
const stepConditionRows = 10

// condRowSignals are the four per-row condition signals the editor binds:
// condOn{i} / condField{i} / condOperator{i} / condValue{i}. The remove
// handler's positional shift moves all four up a slot so the visible rows stay
// a dense 1..count block.
var condRowSignals = [...]string{"condOn", "condField", "condOperator", "condValue"}

// condRemoveExpr builds the Datastar data-on:click expression for row k's
// remove (×) button. A naive "blank row k + decrement count" leaves a visible
// hole when k is a MIDDLE row: the count only hides the trailing slot, so the
// blanked middle row stays on screen and the wrong row collapses. Instead this
// does a POSITIONAL SHIFT — every later filled row's four signals move up one
// slot (row j gets row j+1's values), the trailing visible slot is cleared, and
// the count is decremented. The result is always a dense, hole-free 1..count
// block so the decrement correctly hides the now-empty trailing slot.
//
// The pool size is static, so the full shift chain is generated at render time;
// each step is guarded by $condRowCount so only the in-range rows shift:
//   - $condRowCount > j+1  → copy row j+1 into row j
//   - $condRowCount === j+1 → row j is the last visible row being vacated, blank it
//
// The j===k, count===k+1 branch covers removing the last row (blank in place).
func condRemoveExpr(k int) template.JS {
	var b strings.Builder
	for j := k; j < stepConditionRows-1; j++ {
		// Copy row j+1 → row j when a later row exists.
		copies := make([]string, 0, len(condRowSignals))
		blanks := make([]string, 0, len(condRowSignals))
		for _, sig := range condRowSignals {
			cur := fmt.Sprintf("$%s%d", sig, j)
			next := fmt.Sprintf("$%s%d", sig, j+1)
			copies = append(copies, cur+" = "+next)
			blanks = append(blanks, cur+" = ''")
		}
		// Guarded ternary: shift-up when more rows follow, else blank when this
		// is the last visible row, else no-op.
		fmt.Fprintf(
			&b,
			"$condRowCount > %d ? (%s) : ($condRowCount === %d ? (%s) : null); ",
			j+1, strings.Join(copies, ", "),
			j+1, strings.Join(blanks, ", "),
		)
	}
	// Decrement the visible-row count (floor at 1).
	b.WriteString("$condRowCount = ($condRowCount > 1 ? $condRowCount - 1 : 1)")
	return template.JS(b.String())
}

// stepPreviewView is what the step-preview fragment binds (W4-W3).
type stepPreviewView struct {
	// Output is the rendered Handlebars output on success.
	Output string
	// Error is a template compile/render error shown inline; never causes a 500.
	Error string
	// IsError is true when Error is set, so the template can choose the error style.
	IsError bool
}

// conditionsFromFilters decodes step.Filters ([]filter.Group) back into the
// editor's combinator + per-row form model. Only the first Group is surfaced
// (the UI authors a single AND/OR group; no nested groups). The returned rows
// are padded to stepConditionRows so the editor renders a fixed pool of rows
// (the trailing blanks are hidden client-side until "Add condition" reveals
// them). visible is the count of saved conditions, clamped to at least 1, and
// seeds the $condRowCount signal. Combinator defaults to "AND" when absent.
func conditionsFromFilters(raw json.RawMessage) (combinator string, rows []stepConditionRow, visible int) {
	combinator = "AND"
	rows = make([]stepConditionRow, 0, stepConditionRows)
	if len(raw) > 0 {
		var groups []filter.Group
		if err := json.Unmarshal(raw, &groups); err == nil && len(groups) > 0 {
			g := groups[0]
			if g.Combinator != "" {
				combinator = strings.ToUpper(g.Combinator)
			}
			for _, c := range g.Conditions {
				rows = append(rows, stepConditionRow{
					On:       c.On,
					Field:    c.Field,
					Operator: string(c.Operator),
					Value:    conditionValueString(c.Value),
				})
			}
		}
	}
	visible = len(rows)
	if visible < 1 {
		visible = 1
	}
	if visible > stepConditionRows {
		visible = stepConditionRows
	}
	for len(rows) < stepConditionRows {
		rows = append(rows, stepConditionRow{})
	}
	return combinator, rows, visible
}

// listValueOperators are the operators whose form Value is a single text field
// holding a comma-separated list (IN/NOT_IN/ALL_IN/ANY_IN) or a "min,max" pair
// (BETWEEN/NOT_BETWEEN). conditionsFromForm parses them into a []any; the
// parse-back renders the []any as comma-joined text. Single-value operators
// (LIKE/NOT_LIKE, EQUAL, ...) keep a plain string Value.
var listValueOperators = map[filter.Operator]bool{
	filter.OpIn:         true,
	filter.OpNotIn:      true,
	filter.OpAllIn:      true,
	filter.OpAnyIn:      true,
	filter.OpBetween:    true,
	filter.OpNotBetween: true,
}

// conditionValueForOperator shapes the single Value text field into the
// evaluator's expected wire shape for the given operator. For the list/range
// operators it splits on "," into a []any (each piece trimmed); BETWEEN/
// NOT_BETWEEN coerce each bound to a number when possible so the wire value is
// a numeric [min,max] (matching the reference platform's between [min,max] number array), while
// IN-family keep string elements. All other operators keep the raw string.
func conditionValueForOperator(op filter.Operator, raw string) any {
	if !listValueOperators[op] {
		return raw
	}
	parts := strings.Split(raw, ",")
	out := make([]any, 0, len(parts))
	numeric := op == filter.OpBetween || op == filter.OpNotBetween
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if numeric {
			if f, err := strconv.ParseFloat(p, 64); err == nil {
				out = append(out, f)
				continue
			}
		}
		out = append(out, p)
	}
	return out
}

// conditionValueString renders a decoded condition Value back to the text the
// operator typed. Scalars round-trip; a []any (the IN-family list and the
// BETWEEN range) renders as comma-joined text so the single Value input
// repopulates. Other non-scalars come back empty.
func conditionValueString(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(t)
	case []any:
		parts := make([]string, len(t))
		for i, e := range t {
			parts[i] = conditionValueString(e)
		}
		return strings.Join(parts, ",")
	}
	return ""
}

// wireCondition / wireGroup mirror filter.Condition / filter.Group with the
// shared-contract json tags. The vendored filter types carry no struct tags
// (they rely on Go's case-insensitive unmarshal at eval time), so marshaling
// them directly would emit Go field-name keys ("On", "Combinator", ...) rather
// than the contract's lowercase keys ("on", "combinator", ...). We build
// filter.* values for type-safety, then convert to these tagged mirrors for the
// on-the-wire bytes the evaluator (and any other consumer) reads back.
type wireCondition struct {
	On       string          `json:"on"`
	Field    string          `json:"field"`
	Operator filter.Operator `json:"operator"`
	Value    any             `json:"value"`
	Negated  bool            `json:"negated"`
}

type wireGroup struct {
	Combinator string          `json:"combinator"`
	Conditions []wireCondition `json:"conditions"`
	Groups     []wireGroup     `json:"groups,omitempty"`
}

func toWireGroup(g filter.Group) wireGroup {
	wg := wireGroup{Combinator: g.Combinator}
	for _, c := range g.Conditions {
		wg.Conditions = append(wg.Conditions, wireCondition{
			On:       c.On,
			Field:    c.Field,
			Operator: c.Operator,
			Value:    c.Value,
			Negated:  c.Negated,
		})
	}
	for _, child := range g.Groups {
		wg.Groups = append(wg.Groups, toWireGroup(child))
	}
	return wg
}

// conditionsFromForm parses the posted CONDITIONS rows into a single
// filter.Group wrapped in a JSON array of Group, marshaled with the shared
// contract's lowercase keys. Rows with an empty Field are skipped. When no
// usable row exists it returns (nil, nil) so the step's Filters stay absent and
// the step is not gated.
func conditionsFromForm(r *http.Request) (json.RawMessage, error) {
	field := func(name string) string { return strings.TrimSpace(r.FormValue(name)) }

	combinator := strings.ToUpper(field("cond_combinator"))
	if combinator != "OR" {
		combinator = "AND"
	}

	var conditions []filter.Condition
	for i := 0; i < stepConditionRows; i++ {
		suffix := strconv.Itoa(i)
		path := field("cond_field_" + suffix)
		if path == "" {
			continue
		}
		on := field("cond_on_" + suffix)
		if on == "" {
			on = "payload"
		}
		op := filter.Operator(field("cond_operator_" + suffix))
		conditions = append(conditions, filter.Condition{
			On:       on,
			Field:    path,
			Operator: op,
			Value:    conditionValueForOperator(op, field("cond_value_"+suffix)),
		})
	}
	if len(conditions) == 0 {
		return nil, nil
	}
	groups := []wireGroup{toWireGroup(filter.Group{Combinator: combinator, Conditions: conditions})}
	b, err := json.Marshal(groups)
	if err != nil {
		return nil, fmt.Errorf("marshal conditions: %w", err)
	}
	return b, nil
}

// ─── W4-W3: Handlebars live-preview pane ─────────────────────────────────────

// serveFragmentStepPreview handles
// GET /console/api/fragments/step-preview.
//
// Query parameters (sent as Datastar signals via @get):
//
//	tmpl    — the Handlebars template string to render
//	payload — a JSON object whose keys become the {{payload.*}} namespace
//
// The endpoint is read-only (GET, no CSRF required, no state mutation).
// Template errors are returned as inline HTML — never a 500.
// Scope safety: raymond evaluates a logicless Handlebars template; there
// is no file I/O, no network access, and no helper registration — the
// sandbox is raymond's own strict evaluation model.
func (c *Console) serveFragmentStepPreview(w http.ResponseWriter, r *http.Request) {
	if w == nil {
		panic("serveFragmentStepPreview: w is nil")
	}
	if r == nil {
		panic("serveFragmentStepPreview: r is nil")
	}

	// Read signals (Datastar @get sends signals as a URL-encoded JSON query
	// param). Fall back to raw query params so plain HTTP GET also works
	// (browser navigation / tests).
	var tmplStr, payloadStr string
	env, ok := readSignalsBestEffort(r)
	if ok {
		tmplStr = env.WfPreviewTmpl
		payloadStr = env.WfPreviewPayload
	}
	// URL query params override / supplement signal values so a plain
	// GET ?tmpl=…&payload=… (curl, tests) also works.
	if q := r.URL.Query().Get("tmpl"); q != "" {
		tmplStr = q
	}
	if q := r.URL.Query().Get("payload"); q != "" {
		payloadStr = q
	}

	view := renderStepPreview(tmplStr, payloadStr)

	html, err := renderFragment(c.ts.base, "step-preview", view)
	if err != nil {
		c.deps.Logger.Error("console: render step-preview fragment", "err", err)
		http.Error(w, "render failed", http.StatusInternalServerError)
		return
	}
	c.patchHTML(w, r, html)
}

// renderStepPreview renders tmplStr against a data map built from
// payloadStr (a JSON object). It is extracted from the handler so tests
// can call it without HTTP plumbing.
//
// The data namespace is byte-for-byte the send-time context built in
// internal/delivery/delivery.go (and email.go / push.go):
//
//	data := map[string]any{"subscriber": subMap, "payload": payloadMap}
//
// so a body that renders {{payload.name}} in the preview renders the same
// at send time. A passthrough {{t}} helper is registered to mirror
// template.RenderI18n — an unseeded translate key renders verbatim, so a
// template that uses {{t "key"}} previews without a spurious error.
// There is no subscriber at design time, so {{subscriber.*}} resolves to
// "" (the same fail-soft behaviour the engine uses for an absent path).
//
// Template errors (parse or execute) and bad payload JSON surface as
// stepPreviewView.Error rather than panicking or returning a 500. This
// uses raymond directly (the same library internal/delivery/template
// wraps) so we can distinguish a compile error from a successful render
// — the package-level Render helper deliberately swallows errors into the
// literal source, which a preview pane must not do.
func renderStepPreview(tmplStr, payloadStr string) stepPreviewView {
	// Parse the template first so we can report compile errors distinctly
	// from execute errors. raymond.Parse returns an error on malformed
	// Handlebars (unterminated {{ etc.).
	tpl, err := raymond.Parse(tmplStr)
	if err != nil {
		return stepPreviewView{IsError: true, Error: "Template error: " + err.Error()}
	}
	// Mirror template.RenderI18n: an unseeded key renders verbatim.
	tpl.RegisterHelper("t", func(key string) string { return key })

	// Build the data context. payload is a JSON object; if it is empty or
	// not valid JSON we report the JSON error so the operator can fix it.
	payloadData := make(map[string]any)
	if payloadStr != "" {
		if jsonErr := json.Unmarshal([]byte(payloadStr), &payloadData); jsonErr != nil {
			return stepPreviewView{IsError: true, Error: "Payload JSON error: " + jsonErr.Error()}
		}
	}

	data := map[string]any{
		"payload":    payloadData,
		"subscriber": map[string]any{},
	}

	out, err := tpl.Exec(data)
	if err != nil {
		return stepPreviewView{IsError: true, Error: "Render error: " + err.Error()}
	}
	return stepPreviewView{Output: out}
}
