package notifications

import (
	"encoding/json"
	"fmt"
	"strings"
)

// EvaluateConditions returns true when the event payload satisfies
// the supplied JSONB-expression filter. The filter language is a
// small AND-of-equality / IN / prefix dialect deliberately limited
// so a tenant cannot DoS the worker by submitting arbitrary
// expression trees.
//
// Syntax — the stored JSONB is one of:
//
//   - {} or null — "no condition; always match" (default).
//   - {"key.path": "value"} — matches when the dotted JSON path
//     resolves to exactly that scalar.
//   - {"key.path": {"$in": ["a", "b"]}} — matches when the path
//     resolves to any of the listed values.
//   - {"key.path": {"$prefix": "abc"}} — string prefix match.
//   - {"key.path": {"$exists": true}} — path-present match (any
//     value, including null).
//
// Multiple keys are AND-combined: every condition must hold.
//
// Evaluation is fail-closed: any malformed filter or path-resolution
// error returns false so a corrupted condition silently drops
// delivery rather than leaking events to a webhook the operator
// thought was filtered out.
func EvaluateConditions(rawConditions, payload json.RawMessage) bool {
	if len(rawConditions) == 0 {
		return true
	}
	trimmed := strings.TrimSpace(string(rawConditions))
	if trimmed == "" || trimmed == "null" || trimmed == "{}" {
		return true
	}
	var conds map[string]any
	if err := json.Unmarshal(rawConditions, &conds); err != nil {
		return false
	}
	if len(conds) == 0 {
		return true
	}
	var doc any
	if len(payload) == 0 {
		doc = map[string]any{}
	} else if err := json.Unmarshal(payload, &doc); err != nil {
		return false
	}
	for path, want := range conds {
		got, ok := resolvePath(doc, path)
		if !matchCondition(got, ok, want) {
			return false
		}
	}
	return true
}

// resolvePath walks doc by dotted key and returns the resolved
// value plus a present flag. Numeric path segments index into JSON
// arrays so callers can target e.g. "items.0.sku".
func resolvePath(doc any, path string) (any, bool) {
	if path == "" {
		return doc, true
	}
	cur := doc
	for _, seg := range strings.Split(path, ".") {
		switch v := cur.(type) {
		case map[string]any:
			next, ok := v[seg]
			if !ok {
				return nil, false
			}
			cur = next
		case []any:
			idx := -1
			fmt.Sscanf(seg, "%d", &idx)
			if idx < 0 || idx >= len(v) {
				return nil, false
			}
			cur = v[idx]
		default:
			return nil, false
		}
	}
	return cur, true
}

// matchCondition compares a resolved value against the condition
// operand. operand may be a scalar (equality match) or a map with
// one of the supported operators.
func matchCondition(got any, present bool, operand any) bool {
	switch op := operand.(type) {
	case map[string]any:
		if v, ok := op["$exists"]; ok {
			want, _ := v.(bool)
			if want != present {
				return false
			}
		}
		if v, ok := op["$in"]; ok {
			arr, _ := v.([]any)
			matched := false
			for _, candidate := range arr {
				if scalarEqual(got, candidate) {
					matched = true
					break
				}
			}
			if !matched {
				return false
			}
		}
		if v, ok := op["$prefix"]; ok {
			prefix, _ := v.(string)
			s, isStr := got.(string)
			if !isStr || !strings.HasPrefix(s, prefix) {
				return false
			}
		}
		if v, ok := op["$eq"]; ok {
			if !scalarEqual(got, v) {
				return false
			}
		}
		return true
	default:
		// Bare operand — equality match.
		if !present {
			return false
		}
		return scalarEqual(got, op)
	}
}

// scalarEqual normalises numeric types before comparing: JSON
// numbers always decode to float64, so a cond authored as `1` and
// a payload value of `1.0` compare equal.
func scalarEqual(a, b any) bool {
	if a == nil || b == nil {
		return a == b
	}
	if af, aok := toFloat(a); aok {
		if bf, bok := toFloat(b); bok {
			return af == bf
		}
		return false
	}
	return a == b
}

func toFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		if err != nil {
			return 0, false
		}
		return f, true
	}
	return 0, false
}
