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

// The implemented operators (the reference platform FieldOperatorEnum, packages/shared/src/types/
// builder.ts). All 16 enum members are now handled; an unrecognized operator
// still evaluates to false (condition does not pass) rather than erroring,
// matching the conservative "no match" behavior.
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
	OpAllIn        Operator = "ALL_IN"
	OpAnyIn        Operator = "ANY_IN"
	OpBetween      Operator = "BETWEEN"
	OpNotBetween   Operator = "NOT_BETWEEN"
	OpLike         Operator = "LIKE"
	OpNotLike      Operator = "NOT_LIKE"
)

// Condition is a single leaf test. On names the data source ("payload",
// "subscriber", ...); Field is a dotted path within that source; Value is the
// comparand. Negated inverts the result (the reference platform isNegated).
type Condition struct {
	On       string   `json:"on"`
	Field    string   `json:"field"`
	Operator Operator `json:"operator"`
	Value    any      `json:"value"`
	Negated  bool     `json:"negated"`
}

// Group is a recursive AND/OR tree. Combinator is "AND" or "OR" (case
// insensitive; anything other than "OR" is treated as AND). A group passes per
// its combinator over Conditions AND nested Groups.
type Group struct {
	Combinator string      `json:"combinator"`
	Conditions []Condition `json:"conditions"`
	Groups     []Group     `json:"groups,omitempty"`
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
	case OpLike:
		// the reference platform 'contains' (query-parser.service.ts:200-203): case-SENSITIVE
		// substring test, field.includes(value). Both sides must be strings;
		// a non-string field or value is no match.
		return likeContains(field, c.Value)
	case OpNotLike:
		// the reference platform 'doesNotContain' (query-parser.service.ts:205-208): the negation
		// of LIKE, but still string-only. A non-string field/value is no match
		// (NOT the negation of a no-op), mirroring createStringOperator returning
		// false on invalid input before the predicate runs.
		s, sok := field.(string)
		v, vok := c.Value.(string)
		return sok && vok && !strings.Contains(s, v)
	case OpBetween:
		// the reference platform 'between' (query-parser.service.ts:241-249): inclusive numeric
		// range, field >= min && field <= max. Value must be a 2-element list of
		// numbers; a non-numeric field or malformed bounds is no match.
		return between(field, c.Value, func(x, lo, hi float64) bool { return x >= lo && x <= hi })
	case OpNotBetween:
		// the reference platform 'notBetween' (query-parser.service.ts:251-259): field < min ||
		// field > max. Same shape requirements as BETWEEN; invalid shape => false
		// (NOT the negation), mirroring validateRangeInput rejecting first.
		return between(field, c.Value, func(x, lo, hi float64) bool { return x < lo || x > hi })
	case OpAnyIn:
		// the reference platform 'containsAny' (query-parser.service.ts:220-224): array overlap,
		// field.some(item => value.includes(item)). Both field and value must be
		// arrays; an empty value-list or a non-array field is no match.
		return anyIn(field, c.Value)
	case OpAllIn:
		// ALL_IN: every element of the field array is present in the value-list.
		// the reference platform's runtime has no ALL_IN/containsAll operator (it is in
		// FieldOperatorEnum but never evaluated — processFilterEquality and the
		// json-logic query-parser both omit it), so this mirrors the task spec's
		// "all of field in value-list" direction. Field must be an array; an
		// empty field array vacuously passes, an empty value-list against a
		// non-empty field fails.
		return allIn(field, c.Value)
	default:
		// Unknown operator: conservatively no match.
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

// likeContains implements the reference platform's 'contains' string operator: a case-sensitive
// substring test where both the field and the comparand must be strings. Any
// non-string operand yields false (no match), never a panic.
func likeContains(field, value any) bool {
	s, sok := field.(string)
	v, vok := value.(string)
	return sok && vok && strings.Contains(s, v)
}

// between coerces field to a number and value to a 2-element numeric [min,max]
// list, then applies cmp(field, min, max). Mirrors the reference platform validateRangeInput:
// value must be a slice/array of exactly two number-coercible elements and the
// field must be numeric; otherwise false. The number coercion accepts numeric
// strings (via toFloat) so the console's []any{"1","10"} string shape works too.
func between(field, value any, cmp func(x, lo, hi float64) bool) bool {
	x, ok := toFloat(field)
	if !ok {
		return false
	}
	bounds, ok := toAnySlice(value)
	if !ok || len(bounds) != 2 {
		return false
	}
	lo, okLo := toFloat(bounds[0])
	hi, okHi := toFloat(bounds[1])
	if !okLo || !okHi {
		return false
	}
	return cmp(x, lo, hi)
}

// anyIn implements the reference platform 'containsAny': field is an array and at least one of its
// elements is present in the value-list. Both operands must be arrays; a
// non-array field or empty value-list yields false.
func anyIn(field, value any) bool {
	items, ok := toAnySlice(field)
	if !ok {
		return false
	}
	set, ok := toAnySlice(value)
	if !ok {
		return false
	}
	for _, item := range items {
		for _, s := range set {
			if equals(item, s) {
				return true
			}
		}
	}
	return false
}

// allIn implements "every element of the field array is in the value-list".
// Field must be an array; an empty field array passes vacuously, an empty
// value-list against a non-empty field fails. (the reference platform has no runtime ALL_IN; see
// the matchOperator comment.)
func allIn(field, value any) bool {
	items, ok := toAnySlice(field)
	if !ok {
		return false
	}
	set, ok := toAnySlice(value)
	if !ok {
		return false
	}
	for _, item := range items {
		found := false
		for _, s := range set {
			if equals(item, s) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// toAnySlice returns the elements of v as a []any when v is a slice or array.
// Anything else (including strings) is not treated as a list here — these
// operators require a genuine array operand, matching the reference platform's Array.isArray gate.
func toAnySlice(v any) ([]any, bool) {
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Slice, reflect.Array:
		out := make([]any, rv.Len())
		for i := 0; i < rv.Len(); i++ {
			out[i] = rv.Index(i).Interface()
		}
		return out, true
	}
	return nil, false
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
