package audit

import (
	"encoding/json"
	"reflect"
)

// Diff computes a shallow field-level diff between two JSON objects and
// returns a JSONB object of the form {"field": {"old": ..., "new": ...}}
// for every field whose value changed.
//
// Callers use Diff to populate audit_log.before/after in a minimal
// "changed-only" form. If either side is not a JSON object, Diff returns
// the full before/after as {"before": ..., "after": ...} so no information
// is lost.
func Diff(before, after json.RawMessage) json.RawMessage {
	var b, a map[string]any
	beforeOk := json.Unmarshal(before, &b) == nil
	afterOk := json.Unmarshal(after, &a) == nil
	if !beforeOk || !afterOk {
		return fallbackDiff(before, after)
	}
	diff := make(map[string]map[string]any)
	for k, bv := range b {
		av, present := a[k]
		if !present {
			diff[k] = map[string]any{"old": bv, "new": nil}
			continue
		}
		if !reflect.DeepEqual(bv, av) {
			diff[k] = map[string]any{"old": bv, "new": av}
		}
	}
	for k, av := range a {
		if _, present := b[k]; !present {
			diff[k] = map[string]any{"old": nil, "new": av}
		}
	}
	out, err := json.Marshal(diff)
	if err != nil {
		return fallbackDiff(before, after)
	}
	return out
}

func fallbackDiff(before, after json.RawMessage) json.RawMessage {
	raw, err := json.Marshal(map[string]json.RawMessage{
		"before": before,
		"after":  after,
	})
	if err != nil {
		return json.RawMessage("{}")
	}
	return raw
}
