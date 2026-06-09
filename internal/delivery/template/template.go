// Package template renders an interpolation-only subset of Handlebars.
//
// It substitutes every {{ dotted.path }} token with the value resolved from
// data. No helpers, no #if/#each — interpolation only.
package template

import (
	"fmt"
	"reflect"
	"strings"
)

// Render replaces every {{ dotted.path }} token in tmpl with the value
// resolved from data. Non-token text is copied verbatim, and an unterminated
// "{{" is copied verbatim. Any miss or non-leaf resolution yields "".
func Render(tmpl string, data map[string]any) string {
	var b strings.Builder
	for i := 0; i < len(tmpl); {
		open := strings.Index(tmpl[i:], "{{")
		if open < 0 {
			b.WriteString(tmpl[i:])
			break
		}
		open += i
		b.WriteString(tmpl[i:open])
		end := strings.Index(tmpl[open+2:], "}}")
		if end < 0 {
			// Unterminated token: copy verbatim.
			b.WriteString(tmpl[open:])
			break
		}
		path := strings.TrimSpace(tmpl[open+2 : open+2+end])
		b.WriteString(resolve(path, data))
		i = open + 2 + end + 2
	}
	return b.String()
}

// resolve walks the dotted path through maps and structs, formatting the leaf
// as a string. Any miss or non-leaf yields "".
func resolve(path string, data map[string]any) string {
	var cur any = data
	for _, seg := range strings.Split(path, ".") {
		next, ok := descend(cur, seg)
		if !ok {
			return ""
		}
		cur = next
	}
	return leaf(cur)
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

// leaf formats a resolved value as a string. Strings are verbatim; numbers and
// bools use fmt %v. Maps, structs, slices and nil are non-leaf -> "".
func leaf(v any) string {
	if v == nil {
		return ""
	}
	switch reflect.ValueOf(v).Kind() {
	case reflect.Map, reflect.Struct, reflect.Slice, reflect.Array, reflect.Pointer:
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprintf("%v", v)
}
