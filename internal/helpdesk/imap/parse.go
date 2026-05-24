package imap

import (
	"io"
	"mime"
	"net/mail"
	"strings"
)

// stripAngle removes the surrounding angle brackets from an
// RFC-822 message-id-like header. Returns "" for an empty input.
func stripAngle(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "<")
	s = strings.TrimSuffix(s, ">")
	return strings.TrimSpace(s)
}

// parseReferencesHeader splits the space-separated References
// header value into a slice of bare message-ids.
func parseReferencesHeader(v string) []string {
	v = strings.TrimSpace(v)
	if v == "" {
		return nil
	}
	fields := strings.Fields(v)
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		id := stripAngle(f)
		if id != "" {
			out = append(out, id)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// decodeHeader applies RFC-2047 word decoding to a header value
// (e.g. "=?UTF-8?B?...?=" \u2192 the decoded UTF-8 string). Falls
// back to the raw value on decode error so a malformed input
// doesn't drop the whole field.
func decodeHeader(v string) string {
	if v == "" {
		return ""
	}
	dec := &mime.WordDecoder{}
	out, err := dec.DecodeHeader(v)
	if err != nil {
		return v
	}
	return out
}

// readAll reads from r until EOF.
func readAll(r io.Reader) ([]byte, error) {
	var buf strings.Builder
	tmp := make([]byte, 4096)
	for {
		n, err := r.Read(tmp)
		if n > 0 {
			buf.Write(tmp[:n])
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
	}
	return []byte(buf.String()), nil
}

// boundaryFromContentType extracts the boundary= parameter from a
// Content-Type header. Returns "" if the parameter is absent.
func boundaryFromContentType(v string) string {
	_, params, err := mime.ParseMediaType(v)
	if err != nil {
		return ""
	}
	return params["boundary"]
}

// splitOnBoundary splits a multipart body on the boundary marker.
// The leading "--" prefix is the marker for each new part; the
// final "--" suffix on a marker closes the multipart envelope.
// Returns the parts between markers, with leading/trailing
// whitespace stripped per RFC-2046.
func splitOnBoundary(body, boundary string) []string {
	marker := "--" + boundary
	terminator := marker + "--"
	chunks := strings.Split(body, marker)
	parts := make([]string, 0, len(chunks))
	for i, c := range chunks {
		// First chunk is the preamble (before the first
		// marker) \u2014 discard per RFC-2046 \u00a75.1.1.
		if i == 0 {
			continue
		}
		// Closing marker chunk \u2014 anything after the
		// terminator is the epilogue, also discarded.
		if strings.HasPrefix(c, "--") {
			break
		}
		c = strings.TrimLeft(c, "\r\n")
		c = strings.TrimRight(c, "\r\n")
		parts = append(parts, c)
	}
	// Edge case: the body could be a multipart-of-one where
	// the terminator is missing. The loop above handles it.
	_ = terminator
	return parts
}

// splitHeaderBody parses one multipart part into a lowercase-keyed
// header map + the body string. Returns empty map + the input as
// body if the part has no \r\n\r\n separator (rare; some MTAs
// emit malformed multiparts).
func splitHeaderBody(part string) (headers map[string]string, body string) {
	headers = make(map[string]string)
	idx := strings.Index(part, "\r\n\r\n")
	if idx < 0 {
		// Try LF-only as a fallback for misencoded servers.
		idx = strings.Index(part, "\n\n")
		if idx < 0 {
			return headers, part
		}
		body = part[idx+2:]
		readHeaders(part[:idx], headers)
		return headers, body
	}
	body = part[idx+4:]
	readHeaders(part[:idx], headers)
	return headers, body
}

// readHeaders walks the header block of a multipart part and
// populates the map. Continuation folding (lines starting with
// whitespace) is appended to the previous header.
func readHeaders(block string, out map[string]string) {
	lines := strings.Split(block, "\n")
	var lastKey string
	for _, line := range lines {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			continue
		}
		if (line[0] == ' ' || line[0] == '\t') && lastKey != "" {
			out[lastKey] += " " + strings.TrimSpace(line)
			continue
		}
		colon := strings.IndexByte(line, ':')
		if colon < 0 {
			continue
		}
		key := strings.ToLower(strings.TrimSpace(line[:colon]))
		val := strings.TrimSpace(line[colon+1:])
		out[key] = val
		lastKey = key
	}
}

// Verify mail.Header is referenced for godoc cross-links in the
// surrounding file even though parse.go uses it indirectly.
var _ = mail.Header(nil)
