package console

import (
	"html/template"
	"strings"
)

// uiSelectThreshold is the option count above which the searchable filter
// input is rendered. At or below it the popover is a plain option list (a
// search box over six-or-fewer rows is noise).
const uiSelectThreshold = 6

// uiSelectRow is one rendered row in a ui-select popover. Value/Label are the
// raw data; the Expr fields are pre-built Datastar expressions returned as
// template.JS so html/template inserts them verbatim into the data-* attribute
// JS context (it would otherwise rewrite a bare "${{.Signal}}" into "$\"...\"",
// corrupting the expression — see the package note on Datastar escaping).
type uiSelectRow struct {
	Value            string
	Label            string
	SelectedExpr     template.JS // truthy when this row is the selected value (data-show)
	SelectedARIAExpr template.JS // "true"/"false" string for data-attr:aria-selected
	ClickExpr        template.JS // sets the value, closes the popover, runs OnChange
	FilterExpr       template.JS // data-show filter match (empty when not searchable)
	KeydownExpr      template.JS // option keyboard nav: Enter/Space select, arrows move
}

// uiSelectOption is the caller-facing (value,label) pair packed by
// uiSelectOptions before newUISelect expands it into rendered rows.
type uiSelectOption struct {
	Value string
	Label string
}

// uiSelect is the input bundle for the "ui-select" partial — a Basecoat-style
// searchable combobox that replaces native <select> across the console. It is
// constructed by the uiSelect template helper (see staticFuncMap) so callers
// pass named values without a struct literal in the template.
//
// Datastar wiring: Signal is the existing value signal the caller already
// binds (e.g. "activityRange"); the component reads and writes it directly so
// every consumer's @get/reload keeps firing unchanged. OpenSignal is a
// per-instance open/closed flag derived from ID so two selects on one page do
// not share popover state. FilterSignal is the per-instance search text.
//
// Every Datastar expression the template drops into a data-* attribute is a
// template.JS field built here, never a "{{.Signal}}"-style interpolation in
// the HTML: html/template's JS-context escaper rewrites bare interpolations
// inside data-on/data-attr attributes and would corrupt the expression.
type uiSelect struct {
	ID           string
	Name         string // form field name; empty => no hidden input rendered
	Signal       string // Datastar signal holding the selected value
	OpenSignal   string // per-instance popover open flag
	FilterSignal string // per-instance search filter text
	Placeholder  string
	Required     bool
	Searchable   bool

	// ListboxLabel is the accessible name announced for the popover
	// listbox (a screen reader otherwise reads an unnamed listbox). It
	// mirrors the field's purpose — the placeholder text, which describes
	// what the select chooses (e.g. "All statuses", "Pick a layout").
	ListboxLabel string

	// TriggerKeydownExpr opens the popover from the keyboard: ArrowDown,
	// Enter, or Space on the closed trigger opens it (in addition to the
	// existing Escape-closes wiring carried by EscapeExpr).
	TriggerKeydownExpr template.JS

	// DeclareValue is true for form selects (Name set): the component owns the
	// value signal and declares it in its own data-signals seeded to the
	// initial value. Filter selects leave it false because an ancestor
	// data-signals/data-store already declares (and seeds) the signal — a
	// re-declaration here would clobber that seeded value.
	DeclareValue bool

	Rows []uiSelectRow

	// Pre-built expressions for the wrapper/trigger/popover/search.
	SignalsAttr     template.JS // data-signals value declaring open + filter
	ExpandedExpr    template.JS // data-attr:aria-expanded value
	ToggleExpr      template.JS // trigger data-on:click
	EscapeExpr      template.JS // trigger data-on:keydown__window (Escape closes)
	ShowExpr        template.JS // popover data-show
	OutsideExpr     template.JS // popover data-on:click__outside
	HiddenValueExpr template.JS // hidden input data-attr:value
	SelectedLabel   template.JS // trigger data-text (selected label or placeholder)
	EmptyExpr       template.JS // no-results data-show (searchable only)
}

// newUISelect packs the ui-select inputs, deriving the per-instance signal
// names, the searchable flag (auto-on above uiSelectThreshold options), and
// every Datastar expression the partial renders.
//
// Signature is positional to stay callable from html/template:
//
//	{{ template "ui-select" (uiSelect "id" "name" "signal" $opts "placeholder" "onchange-expr" true) }}
//
// where $opts is a []uiSelectOption (build it with uiSelectOptions).
func newUISelect(
	id, name, signal string,
	options []uiSelectOption,
	placeholder, onChange string,
	required bool,
) uiSelect {
	return newUISelectForm(id, name, signal, options, placeholder, onChange, required, "", false)
}

// newUISelectForm is the full constructor. initialValue seeds the value signal
// when declareValue is true (form selects own their signal); filter selects
// pass declareValue=false because an ancestor data-signals already seeds the
// value signal.
func newUISelectForm(
	id, name, signal string,
	options []uiSelectOption,
	placeholder, onChange string,
	required bool,
	initialValue string,
	declareValue bool,
) uiSelect {
	safe := uiSelectSignalKey(id)
	openSig := "uiSelOpen_" + safe
	filterSig := "uiSelFilter_" + safe
	searchable := len(options) > uiSelectThreshold

	s := uiSelect{
		ID:           id,
		Name:         name,
		Signal:       signal,
		OpenSignal:   openSig,
		FilterSignal: filterSig,
		Placeholder:  placeholder,
		Required:     required,
		Searchable:   searchable,
		DeclareValue: declareValue,
		ListboxLabel: placeholder,
	}

	signalsDecl := "{" + openSig + ": false, " + filterSig + ": ''"
	if declareValue {
		signalsDecl += ", " + signal + ": " + jsStringLiteral(initialValue)
	}
	signalsDecl += "}"
	s.SignalsAttr = template.JS(signalsDecl)
	s.ExpandedExpr = template.JS("$" + openSig + " ? 'true' : 'false'")
	s.ToggleExpr = template.JS("$" + openSig + " = !$" + openSig)
	s.EscapeExpr = template.JS("evt.key === 'Escape' && ($" + openSig + " = false)")
	// Trigger keyboard support: ArrowDown / Enter / Space open the popover
	// and move focus to the first visible option; Escape (handled in the
	// same handler) closes it. preventDefault stops Space/ArrowDown from
	// scrolling the page when the trigger has focus.
	// The first-visible-option search uses a JS .style.display property
	// check (robust to "none" written with or without a space) rather than
	// the fragile CSS attribute-substring selector [style*="display: none"].
	s.TriggerKeydownExpr = template.JS(
		"if (evt.key === 'Escape') { $" + openSig + " = false }" +
			" else if (evt.key === 'ArrowDown' || evt.key === 'Enter' || evt.key === ' ') {" +
			" evt.preventDefault(); $" + openSig + " = true;" +
			" const opts = Array.from(evt.target.closest('.ui-select')" +
			".querySelectorAll('.ui-select-option'));" +
			" const o = opts.find(el => el.style.display !== 'none');" +
			" if (o) requestAnimationFrame(() => o.focus()) }",
	)
	s.ShowExpr = template.JS("$" + openSig)
	s.OutsideExpr = template.JS("$" + openSig + " = false")
	s.HiddenValueExpr = template.JS("$" + signal)

	for _, opt := range options {
		val := jsStringLiteral(opt.Value)
		click := "$" + signal + " = " + val +
			"; $" + openSig + " = false" +
			"; $" + filterSig + " = ''"
		if onChange != "" {
			click += "; " + onChange
		}
		// Option keyboard nav (roving focus): Enter/Space select (same as
		// click); ArrowDown/ArrowUp move focus to the next/previous VISIBLE
		// option so filtered-out rows are skipped; Escape closes and returns
		// focus to the trigger.
		keydown := "if (evt.key === 'Enter' || evt.key === ' ') {" +
			" evt.preventDefault(); " + click + " }" +
			" else if (evt.key === 'ArrowDown' || evt.key === 'ArrowUp') {" +
			" evt.preventDefault();" +
			" const dir = evt.key === 'ArrowDown' ? 'nextElementSibling' : 'previousElementSibling';" +
			" let n = evt.target[dir];" +
			" while (n && (!n.classList.contains('ui-select-option') ||" +
			" n.style.display === 'none')) { n = n[dir] }" +
			" if (n) n.focus() }" +
			" else if (evt.key === 'Escape') {" +
			" $" + openSig + " = false;" +
			" const t = evt.target.closest('.ui-select').querySelector('.ui-select-trigger');" +
			" if (t) t.focus() }"
		row := uiSelectRow{
			Value:            opt.Value,
			Label:            opt.Label,
			SelectedExpr:     template.JS("$" + signal + " === " + val),
			SelectedARIAExpr: template.JS("($" + signal + " === " + val + ") ? 'true' : 'false'"),
			ClickExpr:        template.JS(click),
			KeydownExpr:      template.JS(keydown),
		}
		if searchable {
			row.FilterExpr = template.JS(
				"$" + filterSig + " === '' || " +
					jsStringLiteral(opt.Label) + ".toLowerCase().includes($" +
					filterSig + ".toLowerCase())",
			)
		}
		s.Rows = append(s.Rows, row)
	}

	s.SelectedLabel = template.JS(selectedLabelExpr(signal, placeholder, options))
	if searchable {
		s.EmptyExpr = template.JS(emptyResultsExpr(filterSig, options))
	}
	return s
}

// selectedLabelExpr builds the nested-ternary Datastar expression that resolves
// to the selected option's label, falling back to the placeholder. Small option
// counts keep the chain short.
func selectedLabelExpr(signal, placeholder string, options []uiSelectOption) string {
	expr := jsStringLiteral(placeholder)
	for i := len(options) - 1; i >= 0; i-- {
		opt := options[i]
		if opt.Value == "" {
			// The empty-value row is the placeholder; the base case covers it.
			continue
		}
		expr = "$" + signal + " === " + jsStringLiteral(opt.Value) +
			" ? " + jsStringLiteral(opt.Label) + " : (" + expr + ")"
	}
	return expr
}

// emptyResultsExpr builds the Datastar expression that is truthy when the
// search filter is non-empty and matches no option label — gating the
// "No results" row.
func emptyResultsExpr(filterSig string, options []uiSelectOption) string {
	var labels strings.Builder
	labels.WriteByte('[')
	for i, opt := range options {
		if i > 0 {
			labels.WriteByte(',')
		}
		labels.WriteString(jsStringLiteral(opt.Label))
	}
	labels.WriteByte(']')
	return "$" + filterSig + " !== '' && " + labels.String() +
		".filter(l => l.toLowerCase().includes($" + filterSig +
		".toLowerCase())).length === 0"
}

// uiSelectOptions packs alternating value/label string pairs into the option
// slice newUISelect expands. Mirrors the dict/list helpers: it keeps the
// template free of struct literals.
//
//	{{ template "ui-select" (uiSelect "id" "" "sig" (uiSelectOptions "" "All" "sent" "Sent") "Pick" "" false) }}
func uiSelectOptions(pairs ...string) []uiSelectOption {
	out := make([]uiSelectOption, 0, len(pairs)/2)
	for i := 0; i+1 < len(pairs); i += 2 {
		out = append(out, uiSelectOption{Value: pairs[i], Label: pairs[i+1]})
	}
	return out
}

// uiSelectSignalKey reduces an element id to a safe Datastar signal-name
// suffix: signal keys are JS identifiers, so any non-alphanumeric becomes an
// underscore. Two ids that differ only in separators could collide, but ids
// are author-controlled and already unique per page.
func uiSelectSignalKey(id string) string {
	var b strings.Builder
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

// jsStringLiteral renders s as a double-quoted JS string literal for use inside
// a Datastar expression. It escapes only the characters that would break the
// JS literal itself; the value is carried as template.JS, so html/template
// applies attribute-level quote escaping (the browser decodes it back).
func jsStringLiteral(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}
