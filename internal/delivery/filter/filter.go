// Package filter is a pure, dependency-free evaluator for step
// filters (StepFilter). It ports the behavior of the reference platform's conditions-filter
// usecase and processFilterEquality so that filter decisions stay in parity
// with the upstream engine, without any store, sidecar, or network access.
//
// A filter is a slice of Groups OR'd together (the reference platform filter() ORs the top-level
// filters array). Each Group combines its child Conditions and nested Groups
// with AND or OR. Each Condition is a single leaf test against a value resolved
// from a named data source by a dotted path.
package filter

import (
	"reflect"
	"strconv"
	"strings"
)

// Operator is a the reference platform FieldOperatorEnum value.
type Operator string

// The implemented operators (the reference platform FieldOperatorEnum, builder.ts).
//
// TODO(parity): ALL_IN, ANY_IN, BETWEEN, NOT_BETWEEN, LIKE, NOT_LIKE are part
// of FieldOperatorEnum but are not yet implemented. An unrecognized operator
// evaluates to false (condition does not pass) rather than erroring, matching
// the conservative "no match" behavior.
const (
	OpEqual        Operator = "EQUAL"
	OpNotEqual     Operator = "NOT_EQUAL"
	OpLarger       Operator = "LARGER"
	OpSmaller      Operator = "SMALLER"
	OpLargerEqual  Operator = "LARGER_EQUAL"
	OpSmallerEqual Operator = "SMALLER_EQUAL"
	OpIn           Operator = "IN"
	OpNotIn        Operator = "NOT_IN"
	OpIsDefined    Operator = "IS_DEFINED"
)

// Condition is a single leaf test. On names the data source ("payload",
// "subscriber", ...); Field is a dotted path within that source; Value is the
// comparand. Negated inverts the result (the reference platform isNegated).
type Condition struct {
	On       string
	Field    string
	Operator Operator
	Value    any
	Negated  bool
}

// Group is a recursive AND/OR tree. Combinator is "AND" or "OR" (case
// insensitive; anything other than "OR" is treated as AND). A group passes per
// its combinator over Conditions AND nested Groups.
type Group struct {
	Combinator string
	Conditions []Condition
	Groups     []Group
}

// EvalContext holds the named data sources. payload + subscriber are the
// must-haves; extras (webhook, step, ...) are optional named maps.
type EvalContext struct {
	Sources map[string]map[string]any
}

// Match is the top level: a slice of Groups OR'd together (the reference platform filter() ORs
// the top-level filters array). Empty => pass (send when no filters).
func Match(filters []Group, ctx EvalContext) (bool, error) {
	if len(filters) == 0 {
		return true, nil
	}
	for _, g := range filters {
		ok, err := Eval(g, ctx)
		if err != nil {
			return false, err
		}
		if ok {
			return true, nil
		}
	}
	return false, nil
}

// Eval reports whether ctx satisfies group. An empty group (no conditions, no
// groups) passes (vacuous truth — matches the reference platform: no filters => send).
func Eval(group Group, ctx EvalContext) (bool, error) {
	if len(group.Conditions) == 0 && len(group.Groups) == 0 {
		return true, nil
	}
	or := strings.EqualFold(group.Combinator, "OR")

	for _, c := range group.Conditions {
		ok := evalCondition(c, ctx)
		if or && ok {
			return true, nil
		}
		if !or && !ok {
			return false, nil
		}
	}
	for _, g := range group.Groups {
		ok, err := Eval(g, ctx)
		if err != nil {
			return false, err
		}
		if or && ok {
			return true, nil
		}
		if !or && !ok {
			return false, nil
		}
	}
	// AND with no failure => true; OR with no success => false.
	return !or, nil
}

func evalCondition(c Condition, ctx EvalContext) bool {
	res := matchOperator(c, ctx)
	if c.Negated {
		return !res
	}
	return res
}

func matchOperator(c Condition, ctx EvalContext) bool {
	field, defined := resolve(c.On, c.Field, ctx)

	switch c.Operator {
	case OpIsDefined:
		return defined
	case OpEqual:
		return defined && equals(field, c.Value)
	case OpNotEqual:
		// Mirror the reference platform: NOT_EQUAL is the logical negation of equality. A missing
		// field is not equal to a concrete comparand.
		return !equals(field, c.Value)
	case OpLarger:
		return numCmp(field, c.Value, func(a, b float64) bool { return a > b })
	case OpSmaller:
		return numCmp(field, c.Value, func(a, b float64) bool { return a < b })
	case OpLargerEqual:
		return numCmp(field, c.Value, func(a, b float64) bool { return a >= b })
	case OpSmallerEqual:
		return numCmp(field, c.Value, func(a, b float64) bool { return a <= b })
	case OpIn:
		return defined && contains(c.Value, field)
	case OpNotIn:
		return !contains(c.Value, field)
	default:
		// Unimplemented operator (ALL_IN/ANY_IN/BETWEEN/NOT_BETWEEN/LIKE/
		// NOT_LIKE) or unknown: conservatively no match. See package TODO.
		return false
	}
}

// equals matches the reference platform processFilterEquality EQUAL semantics: equal when the
// stringified forms match OR when both sides parse as numbers and are
// numerically equal.
func equals(a, b any) bool {
	if stringify(a) == stringify(b) {
		return true
	}
	fa, oka := toFloat(a)
	fb, okb := toFloat(b)
	return oka && okb && fa == fb
}

// numCmp parses both sides as float64 and applies cmp. A non-numeric side
// yields false (no match), never an error.
func numCmp(a, b any, cmp func(x, y float64) bool) bool {
	fa, oka := toFloat(a)
	fb, okb := toFloat(b)
	if !oka || !okb {
		return false
	}
	return cmp(fa, fb)
}

// contains tests membership of field in set. set may be a slice/array (each
// element compared by equals) or a comma-separated string (split on ",", each
// piece trimmed and compared by equals). Anything else: single-value compare.
func contains(set, field any) bool {
	rv := reflect.ValueOf(set)
	switch rv.Kind() {
	case reflect.Slice, reflect.Array:
		for i := 0; i < rv.Len(); i++ {
			if equals(rv.Index(i).Interface(), field) {
				return true
			}
		}
		return false
	}
	if s, ok := set.(string); ok {
		for _, part := range strings.Split(s, ",") {
			if equals(strings.TrimSpace(part), field) {
				return true
			}
		}
		return false
	}
	return equals(set, field)
}

// resolve walks the dotted path through the named source map and any nested
// map[string]any / structs. It returns the leaf value and whether it resolved
// to a present, non-nil value.
func resolve(on, field string, ctx EvalContext) (any, bool) {
	src, ok := ctx.Sources[on]
	if !ok {
		return nil, false
	}
	var cur any = src
	for _, seg := range strings.Split(field, ".") {
		next, ok := descend(cur, seg)
		if !ok {
			return nil, false
		}
		cur = next
	}
	if cur == nil {
		return nil, false
	}
	return cur, true
}

// descend returns the child value for key seg from cur, where cur is a
// map[string]any (by key) or a struct (by json tag, else field name).
func descend(cur any, seg string) (any, bool) {
	if m, ok := cur.(map[string]any); ok {
		v, ok := m[seg]
		return v, ok
	}
	rv := reflect.ValueOf(cur)
	for rv.Kind() == reflect.Pointer {
		if rv.IsNil() {
			return nil, false
		}
		rv = rv.Elem()
	}
	if rv.Kind() != reflect.Struct {
		return nil, false
	}
	rt := rv.Type()
	for i := 0; i < rt.NumField(); i++ {
		f := rt.Field(i)
		if !f.IsExported() {
			continue
		}
		name := f.Name
		if tag, ok := f.Tag.Lookup("json"); ok {
			if before, _, _ := strings.Cut(tag, ","); before != "" && before != "-" {
				name = before
			}
		}
		if name == seg {
			return rv.Field(i).Interface(), true
		}
	}
	return nil, false
}

// stringify renders v for stringified equality. Strings are verbatim; other
// scalars use a minimal numeric/bool formatting so that int 3 and string "3"
// agree.
func stringify(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case bool:
		return strconv.FormatBool(t)
	}
	if f, ok := toFloat(v); ok {
		return strconv.FormatFloat(f, 'g', -1, 64)
	}
	return ""
}

// toFloat coerces numeric kinds and numeric strings to float64.
func toFloat(v any) (float64, bool) {
	switch t := v.(type) {
	case nil:
		return 0, false
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(t), 64)
		return f, err == nil
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return float64(rv.Int()), true
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return float64(rv.Uint()), true
	case reflect.Float32, reflect.Float64:
		return rv.Float(), true
	}
	return 0, false
}
