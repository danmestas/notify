// Package steptype is the single source of truth for notify's workflow step
// types. Each step type (in_app, email, push, sms, chat, delay, digest) is a
// self-contained value implementing StepType; adding or extending a type means
// touching one file here instead of the ~8 correlated switch/list sites that
// previously fanned out across console, httpapi, authoring, and the editor
// template.
//
// The registry is intentionally PURE: it knows how to default, parse, flatten,
// validate, compile, and name the editor partial for a step type — all
// functions of the step's own configuration. Runtime delivery wiring (the
// engine task handlers built from store/connectivity/eventbus/preferences in
// cmd/notify) stays out of this package so the pure consumers (authoring,
// httpapi, console) are not forced to import the delivery stack.
package steptype

import (
	"encoding/json"
	"net/url"
	"strconv"
	"strings"

	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/engine"
)

// StepType is everything notify needs to know about one workflow step type.
// Implementations are immutable, dependency-free values.
type StepType interface {
	// Name is the wire/type identifier (e.g. "in_app", "delay").
	Name() string
	// DisplayName is the human-friendly label shown in the console UI (e.g.
	// "In-App", "HTTP Request").
	DisplayName() string
	// DefaultControlValues is the minimal valid controlValues blob seeded into
	// a freshly added step of this type.
	DefaultControlValues() json.RawMessage
	// ParseForm builds a step's controlValues JSON from the per-type editor
	// form fields.
	ParseForm(form url.Values) (json.RawMessage, error)
	// ControlMap flattens a step's controlValues into a string map for form
	// pre-fill (scalar values only; nested objects dropped).
	ControlMap(cv json.RawMessage) map[string]string
	// Compile turns a persisted step into its engine StepDef (without
	// DependsOn — positional chaining is the caller's concern).
	Compile(step domain.Step) (engine.StepDef, error)
	// Validate reports whether a controlValues blob is acceptable for this type.
	Validate(cv json.RawMessage) error
	// EditorPartial is the html/template partial name rendering this type's
	// editor fields.
	EditorPartial() string
}

// StepEntry pairs a type's wire name with its human label for use in UI lists
// such as the console's add-step menu.
type StepEntry struct {
	Name  string
	Label string
}

// Registry is an ordered, name-keyed set of step types. It is built once and
// treated as immutable; lookups are concurrency-safe (read-only).
type Registry struct {
	order  []string
	byName map[string]StepType
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{byName: make(map[string]StepType)}
}

// Register adds st to the registry, preserving registration order. It panics on
// a duplicate or empty name — registration happens at startup, so a collision
// is a programming error to surface loudly.
func (r *Registry) Register(st StepType) {
	name := st.Name()
	if name == "" {
		panic("steptype: Register with empty name")
	}
	if _, dup := r.byName[name]; dup {
		panic("steptype: duplicate registration for " + name)
	}
	r.byName[name] = st
	r.order = append(r.order, name)
}

// Get returns the step type registered under name, or nil when absent.
func (r *Registry) Get(name string) StepType { return r.byName[name] }

// Has reports whether name is a registered step type.
func (r *Registry) Has(name string) bool {
	_, ok := r.byName[name]
	return ok
}

// Names returns the registered type names in registration order. It is the ONE
// source of truth behind the console's "add step" set and the httpapi's
// allowed-types list (which previously drifted as two hand-maintained lists).
func (r *Registry) Names() []string {
	out := make([]string, len(r.order))
	copy(out, r.order)
	return out
}

// All returns the registered step types in registration order.
func (r *Registry) All() []StepType {
	out := make([]StepType, 0, len(r.order))
	for _, name := range r.order {
		out = append(out, r.byName[name])
	}
	return out
}

// Catalog returns a (Name, Label) entry for each registered type in
// registration order. Consumers such as the console's add-step menu range over
// this instead of maintaining a parallel hardcoded list.
func (r *Registry) Catalog() []StepEntry {
	out := make([]StepEntry, 0, len(r.order))
	for _, name := range r.order {
		st := r.byName[name]
		out = append(out, StepEntry{Name: name, Label: st.DisplayName()})
	}
	return out
}

// Default builds the canonical registry of notify's built-in step types. The
// registration order is the historical order of the workflowStepTypes /
// allowedStepTypes lists this registry replaces; Names() must keep matching it.
func Default() *Registry {
	r := NewRegistry()
	r.Register(inApp{})
	r.Register(email{})
	r.Register(push{})
	r.Register(chat{})
	r.Register(sms{})
	r.Register(delay{})
	r.Register(digest{})
	r.Register(throttle{})
	r.Register(httpRequest{})
	r.Register(stream{})
	r.Register(agentMessage{})
	return r
}

// flattenControlValues decodes a controlValues blob into a flat string map for
// form pre-fill. Non-string scalars are stringified; nested objects are
// dropped. Shared by every step type (the flattening is type-independent).
func flattenControlValues(raw json.RawMessage) map[string]string {
	out := map[string]string{}
	if len(raw) == 0 {
		return out
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return out
	}
	for k, v := range m {
		switch t := v.(type) {
		case string:
			out[k] = t
		case float64:
			out[k] = strconv.FormatFloat(t, 'f', -1, 64)
		case bool:
			out[k] = strconv.FormatBool(t)
		}
	}
	return out
}

// formField trims a single form value (mirrors the old controlValuesFromForm
// helper, which trimmed every per-type field).
func formField(form url.Values, name string) string {
	return strings.TrimSpace(form.Get(name))
}
