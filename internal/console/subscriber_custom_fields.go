package console

// subscriber_custom_fields.go implements the subscriber profile's
// custom-field editor: a fixed pool of key/value rows, server-rendered and
// revealed client-side, shared by the create form (subscribers.go) and the
// detail page's edit form (subscriber_detail.go).

import (
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

// ─── custom fields (L10) ─────────────────────────────────────────────────────
//
// Subscribers already carry a free-form data map (domain.Subscriber.Data, a
// json.RawMessage "data" column threaded through audience.SubscriberInput and,
// at trigger time, into the {{subscriber.data.*}} handlebars namespace — see
// wireSubscriber in internal/trigger/trigger.go). This section adds a
// repeatable key-value editor for it on the console create/edit forms,
// mirroring the workflow step editor's condition-row pattern: a fixed pool of
// rows rendered server-side, revealed via a $cfRowCount signal, with
// "remove" doing a positional shift (see condRemoveExpr in workflows.go for
// the full rationale) rather than leaving a hole in the middle of the list.

// subscriberCustomFieldRowPool is the default pool of key/value rows the
// custom-fields editor renders server-side for a subscriber with no (or few)
// existing custom fields (mirrors stepConditionRows in workflows.go).
// "Add field" reveals rows up to this count.
//
// It is NOT a hard cap: audience.MaxSubscriberCustomFields is the real
// server-side cap, enforced in the service layer so both the console and the
// /v1 API are covered. A subscriber that already carries more fields than
// this pool (legacy data predating the cap, or written directly to the
// store) gets a wider pool — see subscriberCustomFieldsEditor — so the
// editor always renders every existing field and Save never silently
// truncates the ones past row 10 (this was a data-loss bug: the old fixed
// 10-row pool both rendered only the first 10 AND was the parse loop's hard
// bound, so re-saving such a subscriber unchanged deleted the rest).
const subscriberCustomFieldRowPool = 10

// subscriberCustomFieldRow is one row in the custom-fields editor or the
// read-only detail-page list.
type subscriberCustomFieldRow struct {
	Key   string
	Value string
}

// subscriberCustomFieldsEditorView is what the "subscriber-custom-fields"
// partial binds: a fixed-size Rows slice (padded to subscriberCustomFieldRowPool)
// plus Visible, the initial $cfRowCount signal value.
type subscriberCustomFieldsEditorView struct {
	Rows    []subscriberCustomFieldRow
	Visible int
}

// subscriberCustomFieldRowSignals are the two per-row signals the editor
// binds: cfKey{i} / cfValue{i}. Mirrors condRowSignals in workflows.go — same
// positional-shift remove technique, two signals per row instead of four.
var subscriberCustomFieldRowSignals = [...]string{"cfKey", "cfValue"}

// subscriberCustomFieldRemoveExpr builds the Datastar data-on:click expression
// for row k's remove (×) button, given the actual pool size (total) rendered
// for this subscriber — which may exceed subscriberCustomFieldRowPool for a
// legacy over-cap subscriber, see subscriberCustomFieldsEditor. See
// condRemoveExpr in workflows.go for the full rationale (a naive "blank +
// decrement" leaves a hole when k is a middle row); this applies the
// identical positional-shift technique to the custom-fields editor's
// two-signal (key, value) rows.
func subscriberCustomFieldRemoveExpr(k, total int) template.JS {
	var b strings.Builder
	for j := k; j < total-1; j++ {
		copies := make([]string, 0, len(subscriberCustomFieldRowSignals))
		blanks := make([]string, 0, len(subscriberCustomFieldRowSignals))
		for _, sig := range subscriberCustomFieldRowSignals {
			cur := fmt.Sprintf("$%s%d", sig, j)
			next := fmt.Sprintf("$%s%d", sig, j+1)
			copies = append(copies, cur+" = "+next)
			blanks = append(blanks, cur+" = ''")
		}
		fmt.Fprintf(
			&b,
			"$cfRowCount > %d ? (%s) : ($cfRowCount === %d ? (%s) : null); ",
			j+1, strings.Join(copies, ", "),
			j+1, strings.Join(blanks, ", "),
		)
	}
	b.WriteString("$cfRowCount = ($cfRowCount > 1 ? $cfRowCount - 1 : 1)")
	return template.JS(b.String())
}

// decodeSubscriberCustomFields unmarshals a subscriber's stored custom-data
// JSON object into a sorted-by-key slice. Malformed or absent data (nil, "",
// non-object JSON) degrades to an empty slice rather than erroring — a
// hand-edited or /v1-set Data value that isn't a flat string map just doesn't
// render in the console editor/list, it is never dropped from storage by this
// (read-only) helper.
func decodeSubscriberCustomFields(data json.RawMessage) []subscriberCustomFieldRow {
	fields := map[string]string{}
	if len(data) > 0 {
		_ = json.Unmarshal(data, &fields)
	}
	keys := make([]string, 0, len(fields))
	for k := range fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	rows := make([]subscriberCustomFieldRow, 0, len(keys))
	for _, k := range keys {
		rows = append(rows, subscriberCustomFieldRow{Key: k, Value: fields[k]})
	}
	return rows
}

// subscriberCustomFieldsEditor builds the editor view from a subscriber's
// stored Data: existing fields fill the first rows (sorted by key for stable
// rendering), padded with blank rows up to the pool size. The pool is
// subscriberCustomFieldRowPool, EXPANDED to fit len(existing) when a
// subscriber already carries more fields than that (legacy data predating
// audience.MaxSubscriberCustomFields, or a direct store write) — this is the
// W1 fix: truncating here is what caused Save to silently delete every field
// past row 10, since the old fixed pool was also subscriberCustomFieldsFromForm's
// parse bound. Visible is the number of existing fields, clamped to at least
// 1 so the editor always shows a row — the create form's fresh empty row, or
// the first row on a subscriber with no custom data yet.
func subscriberCustomFieldsEditor(data json.RawMessage) subscriberCustomFieldsEditorView {
	existing := decodeSubscriberCustomFields(data)
	poolSize := subscriberCustomFieldRowPool
	if len(existing) > poolSize {
		poolSize = len(existing)
	}
	rows := make([]subscriberCustomFieldRow, 0, poolSize)
	rows = append(rows, existing...)
	visible := len(existing)
	if visible < 1 {
		visible = 1
	}
	for len(rows) < poolSize {
		rows = append(rows, subscriberCustomFieldRow{})
	}
	return subscriberCustomFieldsEditorView{Rows: rows, Visible: visible}
}

// subscriberCustomFieldMaxRowPool bounds the cf_row_pool value a form POST
// may claim (see subscriberCustomFieldsFromForm): far above any realistic
// legacy-data pool size, but finite, so a forged cf_row_pool can't force an
// unbounded parse loop.
const subscriberCustomFieldMaxRowPool = 1000

// subscriberCustomFieldsFromForm parses the posted custom_key_N/custom_value_N
// rows into a JSON object suitable for SubscriberInput.Data. The row count to
// parse comes from the posted cf_row_pool hidden field (written by the
// "subscriber-custom-fields" template to the actual pool size it rendered —
// see subscriberCustomFieldsEditor), clamped to
// [subscriberCustomFieldRowPool, subscriberCustomFieldMaxRowPool] and
// defaulting to subscriberCustomFieldRowPool when absent or invalid. This is
// what lets a legacy subscriber with more than subscriberCustomFieldRowPool
// fields round-trip through Save without the parse loop truncating rows past
// 10 (see subscriberCustomFieldsEditor's doc for the full W1 rationale). A
// row with both key and value blank is an unused slot and is skipped — this
// is how the client-side "remove row" control works (it blanks the row's
// signals before submit). A row with a blank key but a non-blank value is
// rejected: the operator started a field but never named it. Keys and values
// are bounded by maxSubscriberFieldLen, the same guard already applied to
// the other free-text profile fields (audience.Service enforces this too,
// and additionally the audience.MaxSubscriberCustomFields field-count cap).
// Duplicate keys: the last row wins. The caller must call r.ParseForm() first.
//
// Unlike the other profile fields (which the store's merge-upsert preserves
// when posted blank), this always returns a well-formed JSON object, never
// nil: an editor submitted with zero populated rows returns "{}", which
// explicitly CLEARS any previously stored custom data on update — see the
// COALESCE/NULLIF merge in subscriberRepo.Upsert (store/subscriber_repo.go):
// a non-empty "{}" always replaces the stored data, only a true zero-byte
// value would have preserved the old row. The caller decides whether to
// apply that clear (see handleSubscriberUpdate, which only calls this when
// the request actually posted row 0 — i.e. the real edit form was
// submitted).
func subscriberCustomFieldsFromForm(r *http.Request) (json.RawMessage, string) {
	rowPool := subscriberCustomFieldRowPool
	if v := strings.TrimSpace(r.FormValue("cf_row_pool")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > rowPool {
			if n > subscriberCustomFieldMaxRowPool {
				n = subscriberCustomFieldMaxRowPool
			}
			rowPool = n
		}
	}
	fields := make(map[string]string)
	for i := 0; i < rowPool; i++ {
		suffix := strconv.Itoa(i)
		key := strings.TrimSpace(r.FormValue("custom_key_" + suffix))
		val := r.FormValue("custom_value_" + suffix)
		if key == "" && val == "" {
			continue
		}
		if key == "" {
			return nil, "Custom field key is required"
		}
		if len(key) > maxSubscriberFieldLen {
			return nil, "Custom field key is too long (max " + strconv.Itoa(maxSubscriberFieldLen) + " characters)"
		}
		if len(val) > maxSubscriberFieldLen {
			return nil, "Custom field value is too long (max " + strconv.Itoa(maxSubscriberFieldLen) + " characters)"
		}
		fields[key] = val // last-wins on duplicate keys
	}
	b, err := json.Marshal(fields)
	if err != nil {
		return nil, "Invalid custom field data"
	}
	return b, ""
}
