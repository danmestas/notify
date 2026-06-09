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
