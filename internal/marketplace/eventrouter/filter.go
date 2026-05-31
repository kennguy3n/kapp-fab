package eventrouter

import (
	"encoding/json"
	"strconv"
	"strings"
)

// filterMatches reports whether the event payload satisfies the
// subscription's filter spec.
//
// Spec §7 (docs/EXTENSION_SPEC.md): "The filter block is an
// equality match on the event payload — anything more expressive
// belongs in the extension's own handler." We honour that
// narrowly: every key in the filter must appear in the payload
// (at any depth, traversed via dot-separated key paths) and the
// stringified value at that path must equal the filter's value.
//
// Empty / nil filters match every payload (the default case for
// subscriptions that want every event of a given type).
//
// The "stringified value at that path" coercion is intentional:
// JSON event payloads carry untyped numbers / booleans / strings
// and the filter's YAML source is always strings (the
// `map[string]string` shape on manifest.WebhookRef.Filter). We
// compare by string-equivalence so a publisher can write
// `filter: { status: posted }` and match a payload that has
// `{"status": "posted"}` regardless of whether the payload's
// `status` was a string or got coerced through a JSON Number
// somewhere upstream. Numbers are stringified via
// strconv.FormatFloat / FormatInt rules so `42` matches `42`
// (not `42.0`).
//
// Dot-traversal: a filter key like `record.status` walks the
// payload's `record` sub-object and reads `status` from it. A
// missing intermediate object means the filter doesn't match
// (we do NOT treat missing-as-empty-string — that would let a
// filter accidentally match every payload that omits the field).
func filterMatches(filter map[string]string, payload []byte) (bool, error) {
	if len(filter) == 0 {
		return true, nil
	}
	if len(payload) == 0 {
		// A non-empty filter cannot match an absent payload.
		return false, nil
	}
	var decoded map[string]any
	if err := json.Unmarshal(payload, &decoded); err != nil {
		return false, err
	}
	for key, want := range filter {
		got, ok := walkPath(decoded, key)
		if !ok {
			return false, nil
		}
		if stringifyValue(got) != want {
			return false, nil
		}
	}
	return true, nil
}

// walkPath descends a dotted-key path through a decoded JSON
// object. Returns the leaf value and true on hit; nil and false
// on any traversal miss (missing key, intermediate non-object).
func walkPath(root map[string]any, path string) (any, bool) {
	segments := strings.Split(path, ".")
	cur := any(root)
	for _, seg := range segments {
		obj, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		nxt, ok := obj[seg]
		if !ok {
			return nil, false
		}
		cur = nxt
	}
	return cur, true
}

// stringifyValue converts a JSON-decoded value to its canonical
// string form for comparison against filter values. encoding/json
// decodes untyped JSON numbers as float64; we strip trailing zeros
// on integers so `42` and `42.0` both compare as "42".
func stringifyValue(v any) string {
	switch tv := v.(type) {
	case string:
		return tv
	case bool:
		return strconv.FormatBool(tv)
	case float64:
		// Integers come through encoding/json as float64;
		// preserve "42" not "42.000000" or "42.0".
		if tv == float64(int64(tv)) {
			return strconv.FormatInt(int64(tv), 10)
		}
		return strconv.FormatFloat(tv, 'f', -1, 64)
	case nil:
		return ""
	default:
		// Fallback: re-encode to JSON. Covers arrays /
		// nested objects. Tests should avoid filtering on
		// these — semantics for equality on a re-encoded
		// nested object are field-ordering-sensitive — but
		// the encoder gives a deterministic output for
		// simple cases.
		buf, _ := json.Marshal(tv)
		return string(buf)
	}
}
