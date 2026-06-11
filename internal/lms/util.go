package lms

import "encoding/json"

// mustJSON marshals v for use as an audit before/after snapshot. Audit
// payloads are best-effort context, never control flow, so a marshal
// failure (which should be impossible for the plain structs we pass)
// degrades to nil rather than failing the surrounding mutation.
func mustJSON(v any) []byte {
	if v == nil {
		return nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return b
}
