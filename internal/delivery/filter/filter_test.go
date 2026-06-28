package filter

import "testing"

func ctx() EvalContext {
	return EvalContext{Sources: map[string]map[string]any{
		"payload": {
			"itemCount": 5,
			"name":      "widget",
			"nested":    map[string]any{"score": "3"},
			"empty":     nil,
		},
		"subscriber": {
			"firstName": "Ada",
		},
	}}
}

func leaf(c Condition) Group { return Group{Combinator: "AND", Conditions: []Condition{c}} }

func TestConditions(t *testing.T) {
	tests := []struct {
		name string
		cond Condition
		want bool
	}{
		{"equal string match", Condition{On: "payload", Field: "name", Operator: OpEqual, Value: "widget"}, true},
		{"equal string mismatch", Condition{On: "payload", Field: "name", Operator: OpEqual, Value: "gadget"}, false},
		{"equal numeric 5==5", Condition{On: "payload", Field: "itemCount", Operator: OpEqual, Value: 5}, true},
		{"equal cross-type 3==\"3\"", Condition{On: "payload", Field: "nested.score", Operator: OpEqual, Value: 3}, true},
		{"not_equal true", Condition{On: "payload", Field: "name", Operator: OpNotEqual, Value: "gadget"}, true},
		{"not_equal false", Condition{On: "payload", Field: "name", Operator: OpNotEqual, Value: "widget"}, false},
		{"larger 5>3", Condition{On: "payload", Field: "itemCount", Operator: OpLarger, Value: 3}, true},
		{"larger non-numeric field", Condition{On: "payload", Field: "name", Operator: OpLarger, Value: 3}, false},
		{"smaller false 5<3", Condition{On: "payload", Field: "itemCount", Operator: OpSmaller, Value: 3}, false},
		{"larger_equal boundary 5>=5", Condition{On: "payload", Field: "itemCount", Operator: OpLargerEqual, Value: 5}, true},
		{"smaller_equal boundary 5<=5", Condition{On: "payload", Field: "itemCount", Operator: OpSmallerEqual, Value: 5}, true},
		{"smaller_equal over", Condition{On: "payload", Field: "itemCount", Operator: OpSmallerEqual, Value: 4}, false},
		{"in slice", Condition{On: "payload", Field: "name", Operator: OpIn, Value: []any{"widget", "gadget"}}, true},
		{"in comma-string", Condition{On: "payload", Field: "name", Operator: OpIn, Value: "gadget,widget"}, true},
		{"in miss", Condition{On: "payload", Field: "name", Operator: OpIn, Value: []any{"a", "b"}}, false},
		{"not_in true", Condition{On: "payload", Field: "name", Operator: OpNotIn, Value: []any{"a", "b"}}, true},
		{"is_defined present", Condition{On: "payload", Field: "name", Operator: OpIsDefined}, true},
		{"is_defined missing path", Condition{On: "payload", Field: "absent", Operator: OpIsDefined}, false},
		{"is_defined nil value", Condition{On: "payload", Field: "empty", Operator: OpIsDefined}, false},
		{"negated inverts", Condition{On: "payload", Field: "name", Operator: OpEqual, Value: "widget", Negated: true}, false},
		{"negated is_defined missing", Condition{On: "payload", Field: "absent", Operator: OpIsDefined, Negated: true}, true},
		{"subscriber source", Condition{On: "subscriber", Field: "firstName", Operator: OpEqual, Value: "Ada"}, true},
		{"unknown source", Condition{On: "ghost", Field: "x", Operator: OpIsDefined}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Eval(leaf(tt.cond), ctx())
			if err != nil {
				t.Fatalf("Eval error: %v", err)
			}
			if got != tt.want {
				t.Errorf("got %v want %v", got, tt.want)
			}
		})
	}
}

// newOpsCtx is a fixture for the ALL_IN/ANY_IN/BETWEEN/LIKE/NOT_LIKE operators.
// tags/roles are array fields; price/qty are numeric; note is a string.
func newOpsCtx() EvalContext {
	return EvalContext{Sources: map[string]map[string]any{
		"payload": {
			"tags":  []any{"a", "b", "c"},
			"roles": []any{"admin"},
			"price": 7,
			"qty":   5,
			"note":  "Hello World",
			"name":  "widget",
		},
	}}
}

// TestNewOperators covers the parity operators ported from the reference platform's json-logic
// query-parser (libs/application-generic/src/services/query-parser/
// query-parser.service.ts). Each operator has true + false cases plus a
// type-edge case proving non-matching shapes evaluate to false (no panic).
func TestNewOperators(t *testing.T) {
	tests := []struct {
		name string
		cond Condition
		want bool
	}{
		// LIKE: case-sensitive substring contains (query-parser.service.ts:200-203
		// 'contains' => input.includes(value)). Field + value must be strings.
		{"like substring match", Condition{On: "payload", Field: "note", Operator: OpLike, Value: "lo Wo"}, true},
		{"like substring miss", Condition{On: "payload", Field: "note", Operator: OpLike, Value: "xyz"}, false},
		{"like case-sensitive miss", Condition{On: "payload", Field: "note", Operator: OpLike, Value: "hello"}, false},
		{"like non-string field", Condition{On: "payload", Field: "price", Operator: OpLike, Value: "7"}, false},
		{"like non-string value", Condition{On: "payload", Field: "note", Operator: OpLike, Value: 7}, false},

		// NOT_LIKE: negation of contains (query-parser.service.ts:205-208
		// 'doesNotContain' => !input.includes(value)). Non-string => false.
		{"not_like true", Condition{On: "payload", Field: "note", Operator: OpNotLike, Value: "xyz"}, true},
		{"not_like false when contains", Condition{On: "payload", Field: "note", Operator: OpNotLike, Value: "Hello"}, false},
		{"not_like non-string field", Condition{On: "payload", Field: "price", Operator: OpNotLike, Value: "7"}, false},

		// BETWEEN: inclusive numeric range (query-parser.service.ts:241-249
		// 'between' => field >= min && field <= max). Value is [min,max] numbers.
		{"between inside", Condition{On: "payload", Field: "price", Operator: OpBetween, Value: []any{5, 10}}, true},
		{"between lower boundary inclusive", Condition{On: "payload", Field: "qty", Operator: OpBetween, Value: []any{5, 10}}, true},
		{"between upper boundary inclusive", Condition{On: "payload", Field: "price", Operator: OpBetween, Value: []any{1, 7}}, true},
		{"between outside", Condition{On: "payload", Field: "price", Operator: OpBetween, Value: []any{8, 10}}, false},
		{"between non-numeric field", Condition{On: "payload", Field: "note", Operator: OpBetween, Value: []any{1, 10}}, false},
		{"between value not 2-element", Condition{On: "payload", Field: "price", Operator: OpBetween, Value: []any{5}}, false},
		{"between value non-array", Condition{On: "payload", Field: "price", Operator: OpBetween, Value: "5,10"}, false},

		// NOT_BETWEEN: outside the range (query-parser.service.ts:251-259
		// 'notBetween' => field < min || field > max). Non-numeric => false.
		{"not_between outside true", Condition{On: "payload", Field: "price", Operator: OpNotBetween, Value: []any{8, 10}}, true},
		{"not_between inside false", Condition{On: "payload", Field: "price", Operator: OpNotBetween, Value: []any{5, 10}}, false},
		{"not_between boundary false", Condition{On: "payload", Field: "qty", Operator: OpNotBetween, Value: []any{5, 10}}, false},
		{"not_between non-numeric field", Condition{On: "payload", Field: "note", Operator: OpNotBetween, Value: []any{1, 10}}, false},

		// ANY_IN: array overlap (query-parser.service.ts:220-224 'containsAny' =>
		// field.some(item => value.includes(item))). Field must be an array.
		{"any_in overlap", Condition{On: "payload", Field: "tags", Operator: OpAnyIn, Value: []any{"c", "z"}}, true},
		{"any_in no overlap", Condition{On: "payload", Field: "tags", Operator: OpAnyIn, Value: []any{"x", "y"}}, false},
		{"any_in empty value-list", Condition{On: "payload", Field: "tags", Operator: OpAnyIn, Value: []any{}}, false},
		{"any_in non-array field", Condition{On: "payload", Field: "name", Operator: OpAnyIn, Value: []any{"widget"}}, false},

		// ALL_IN: every element of the field array is in the value-list. (the reference platform has
		// no runtime ALL_IN; semantics per task spec — documented in filter.go.)
		{"all_in subset", Condition{On: "payload", Field: "roles", Operator: OpAllIn, Value: []any{"admin", "user"}}, true},
		{"all_in full match", Condition{On: "payload", Field: "tags", Operator: OpAllIn, Value: []any{"a", "b", "c", "d"}}, true},
		{"all_in not subset", Condition{On: "payload", Field: "tags", Operator: OpAllIn, Value: []any{"a", "b"}}, false},
		{"all_in non-array field", Condition{On: "payload", Field: "name", Operator: OpAllIn, Value: []any{"widget"}}, false},
		{"all_in empty value-list non-empty field", Condition{On: "payload", Field: "tags", Operator: OpAllIn, Value: []any{}}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Eval(leaf(tt.cond), newOpsCtx())
			if err != nil {
				t.Fatalf("Eval error: %v", err)
			}
			if got != tt.want {
				t.Errorf("got %v want %v", got, tt.want)
			}
		})
	}
}

func TestGroups(t *testing.T) {
	pass := Condition{On: "payload", Field: "name", Operator: OpEqual, Value: "widget"}
	fail := Condition{On: "payload", Field: "name", Operator: OpEqual, Value: "nope"}

	tests := []struct {
		name  string
		group Group
		want  bool
	}{
		{"empty group vacuous", Group{Combinator: "AND"}, true},
		{"and all pass", Group{Combinator: "AND", Conditions: []Condition{pass, pass}}, true},
		{"and one fails", Group{Combinator: "AND", Conditions: []Condition{pass, fail}}, false},
		{"or one passes", Group{Combinator: "OR", Conditions: []Condition{fail, pass}}, true},
		{"or all fail", Group{Combinator: "OR", Conditions: []Condition{fail, fail}}, false},
		{"nested and-of-or pass", Group{Combinator: "AND", Conditions: []Condition{pass}, Groups: []Group{
			{Combinator: "OR", Conditions: []Condition{fail, pass}},
		}}, true},
		{"nested and-of-or fail", Group{Combinator: "AND", Conditions: []Condition{pass}, Groups: []Group{
			{Combinator: "OR", Conditions: []Condition{fail, fail}},
		}}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Eval(tt.group, ctx())
			if err != nil {
				t.Fatalf("Eval error: %v", err)
			}
			if got != tt.want {
				t.Errorf("got %v want %v", got, tt.want)
			}
		})
	}
}

func TestMatch(t *testing.T) {
	pass := Group{Combinator: "AND", Conditions: []Condition{{On: "payload", Field: "name", Operator: OpEqual, Value: "widget"}}}
	fail := Group{Combinator: "AND", Conditions: []Condition{{On: "payload", Field: "name", Operator: OpEqual, Value: "nope"}}}

	if got, _ := Match(nil, ctx()); !got {
		t.Errorf("empty filters should pass (send)")
	}
	if got, _ := Match([]Group{fail, pass}, ctx()); !got {
		t.Errorf("top-level OR: one group passes => true")
	}
	if got, _ := Match([]Group{fail, fail}, ctx()); got {
		t.Errorf("top-level OR: all fail => false")
	}
}
