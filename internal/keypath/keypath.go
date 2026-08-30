// Package keypath resolves a dot-path against event JSON so a step can
// partition a shared window by payload content. digest (digestKey) and
// throttle (throttleKey) both use it, which is why it lives here rather than
// in either caller.
package keypath

import (
	"encoding/json"
	"strconv"
	"strings"
)

// Resolve walks a dot-path (e.g. "payload.orderId" or "subscriber.data.plan")
// against the event payload or subscriber JSON and returns the scalar value as
// a string. The first segment selects the root ("payload" or "subscriber"); a
// missing intermediate or leaf, a non-object node, an unknown root, or a
// single-segment path all resolve to "" — which callers treat as "no grouping
// value" and fall back to their base key.
func Resolve(path string, payload, subscriber json.RawMessage) string {
	if path == "" {
		return ""
	}
	parts := strings.Split(path, ".")
	if len(parts) < 2 {
		return ""
	}
	var root json.RawMessage
	switch parts[0] {
	case "payload":
		root = payload
	case "subscriber":
		root = subscriber
	default:
		return ""
	}
	var node map[string]any
	if len(root) == 0 || json.Unmarshal(root, &node) != nil {
		return ""
	}
	var cur any = node
	for _, p := range parts[1:] {
		m, ok := cur.(map[string]any)
		if !ok {
			return ""
		}
		cur, ok = m[p]
		if !ok {
			return ""
		}
	}
	switch v := cur.(type) {
	case string:
		return v
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(v)
	default:
		return ""
	}
}
