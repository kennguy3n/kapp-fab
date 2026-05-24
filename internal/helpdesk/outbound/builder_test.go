package outbound

import (
	"net/mail"
	"strings"
	"testing"
	"time"
)

func fixedClock(t time.Time) func() time.Time { return func() time.Time { return t } }

// TestBuild_PlainTextHappyPath pins the canonical reply shape: a
// plain-text body, threading headers populated, parseable by
// net/mail (which is the same parser real MTAs use as a sanity
// check before relaying).
func TestBuild_PlainTextHappyPath(t *testing.T) {
	out, err := Build(fixedClock(time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)), Message{
		From:       "support@acme.com",
		To:         []string{"customer@example.com"},
		Subject:    "Re: Login broken",
		InReplyTo:  "abcd1234@example.com",
		References: []string{"orig123@example.com", "abcd1234@example.com"},
		BodyText:   "Thanks for reaching out. We'll look into it shortly.",
		Headers: []Header{
			{Name: "X-Helpdesk-Ticket-ID", Value: "T-42"},
		},
	}, "support.acme.com")
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if out.MessageID == "" {
		t.Fatalf("expected generated MessageID")
	}
	if !strings.HasSuffix(out.MessageID, "@support.acme.com") {
		t.Errorf("expected MessageID host = configured domain, got %q", out.MessageID)
	}
	wire := string(out.Wire)
	mustContain(t, wire, "From: support@acme.com\r\n")
	mustContain(t, wire, "To: customer@example.com\r\n")
	mustContain(t, wire, "Subject: Re: Login broken\r\n")
	mustContain(t, wire, "In-Reply-To: <abcd1234@example.com>\r\n")
	mustContain(t, wire, "References: <orig123@example.com> <abcd1234@example.com>\r\n")
	mustContain(t, wire, "X-Helpdesk-Ticket-ID: T-42\r\n")
	mustContain(t, wire, "MIME-Version: 1.0\r\n")
	mustContain(t, wire, "Content-Type: text/plain; charset=\"utf-8\"\r\n")
	mustContain(t, wire, "Thanks for reaching out.")

	// Round-trip through net/mail parser to confirm the wire
	// bytes are well-formed.
	msg, err := mail.ReadMessage(strings.NewReader(wire))
	if err != nil {
		t.Fatalf("net/mail.ReadMessage: %v", err)
	}
	if got := msg.Header.Get("Subject"); got != "Re: Login broken" {
		t.Errorf("parsed Subject = %q", got)
	}
}

// TestBuild_MultipartAlternative pins the multipart/alternative
// envelope shape when HTMLBody is set. Text part first, HTML part
// last (modern clients render the last-acceptable part).
func TestBuild_MultipartAlternative(t *testing.T) {
	out, err := Build(fixedClock(time.Now()), Message{
		From:     "a@x.com",
		To:       []string{"b@y.com"},
		Subject:  "s",
		BodyText: "plain",
		HTMLBody: "<p>html</p>",
	}, "x.com")
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	wire := string(out.Wire)
	mustContain(t, wire, "Content-Type: multipart/alternative; boundary=\"kapp-helpdesk-")
	// Text part is BEFORE the HTML part in the boundary
	// sequence.
	textIdx := strings.Index(wire, "Content-Type: text/plain")
	htmlIdx := strings.Index(wire, "Content-Type: text/html")
	if textIdx < 0 || htmlIdx < 0 || textIdx > htmlIdx {
		t.Errorf("expected text/plain before text/html, got text=%d html=%d", textIdx, htmlIdx)
	}
	// Trailing close boundary present.
	if !strings.HasSuffix(strings.TrimRight(wire, "\r\n"), "--") {
		t.Errorf("expected wire to end with closing boundary, got tail %q", wire[len(wire)-32:])
	}
}

// TestBuild_ValidationErrors pins the input-validation matrix. Each
// case should return an error and zero MessageID.
func TestBuild_ValidationErrors(t *testing.T) {
	cases := []struct {
		name string
		m    Message
		dom  string
	}{
		{"empty_from", Message{To: []string{"b@x"}, Subject: "s", BodyText: "b"}, "d"},
		{"invalid_from", Message{From: "not an addr", To: []string{"b@x.com"}, Subject: "s", BodyText: "b"}, "d"},
		{"empty_to", Message{From: "a@x.com", Subject: "s", BodyText: "b"}, "d"},
		{"invalid_to", Message{From: "a@x.com", To: []string{"oops"}, Subject: "s", BodyText: "b"}, "d"},
		{"empty_subject", Message{From: "a@x.com", To: []string{"b@x.com"}, BodyText: "b"}, "d"},
		{"whitespace_subject", Message{From: "a@x.com", To: []string{"b@x.com"}, Subject: "   ", BodyText: "b"}, "d"},
		{"empty_body", Message{From: "a@x.com", To: []string{"b@x.com"}, Subject: "s"}, "d"},
		{"whitespace_body", Message{From: "a@x.com", To: []string{"b@x.com"}, Subject: "s", BodyText: "  "}, "d"},
		{"empty_domain", Message{From: "a@x.com", To: []string{"b@x.com"}, Subject: "s", BodyText: "b"}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := Build(fixedClock(time.Now()), tc.m, tc.dom)
			if err == nil {
				t.Fatalf("expected validation error")
			}
			if out.MessageID != "" || out.Wire != nil {
				t.Errorf("expected zero output on validation error, got msgID=%q wire-len=%d", out.MessageID, len(out.Wire))
			}
		})
	}
}

// TestBuild_GeneratedMessageIDUnique pins that repeated Build()
// calls with identical input produce DIFFERENT Message-IDs. The
// ID is random, not content-derived — repeated autoresponders to
// the same address get distinct IDs.
func TestBuild_GeneratedMessageIDUnique(t *testing.T) {
	m := Message{From: "a@x.com", To: []string{"b@x.com"}, Subject: "s", BodyText: "b"}
	a, err := Build(fixedClock(time.Now()), m, "d")
	if err != nil {
		t.Fatalf("Build a: %v", err)
	}
	b, err := Build(fixedClock(time.Now()), m, "d")
	if err != nil {
		t.Fatalf("Build b: %v", err)
	}
	if a.MessageID == b.MessageID {
		t.Errorf("expected distinct Message-IDs, got %q twice", a.MessageID)
	}
}

// TestBuild_ReferencesAngleBracketsStripped pins that the builder
// accepts references with OR without angle brackets and emits
// canonical form on the wire.
func TestBuild_ReferencesAngleBracketsStripped(t *testing.T) {
	out, err := Build(fixedClock(time.Now()), Message{
		From:       "a@x.com",
		To:         []string{"b@x.com"},
		Subject:    "s",
		BodyText:   "b",
		References: []string{"<a@host>", "b@host", "<c@host>"},
	}, "d")
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	mustContain(t, string(out.Wire), "References: <a@host> <b@host> <c@host>\r\n")
}

// TestBuild_InReplyToAngleBracketsStripped pins the same for
// In-Reply-To, which is single-valued.
func TestBuild_InReplyToAngleBracketsStripped(t *testing.T) {
	out, err := Build(fixedClock(time.Now()), Message{
		From:      "a@x.com",
		To:        []string{"b@x.com"},
		Subject:   "s",
		BodyText:  "b",
		InReplyTo: "<foo@bar>",
	}, "d")
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	mustContain(t, string(out.Wire), "In-Reply-To: <foo@bar>\r\n")
}

// TestBuild_BodyEOLNormalised pins the LF→CRLF normalisation in
// the body. Operators pasting from text editors get correct wire
// output regardless of input EOL style.
func TestBuild_BodyEOLNormalised(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"lf_only", "line1\nline2", "line1\r\nline2"},
		{"crlf_already", "line1\r\nline2", "line1\r\nline2"},
		{"mixed", "line1\r\nline2\nline3\r\n", "line1\r\nline2\r\nline3\r\n"},
		{"empty_lines", "a\n\nb", "a\r\n\r\nb"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := Build(fixedClock(time.Now()), Message{
				From:     "a@x.com",
				To:       []string{"b@x.com"},
				Subject:  "s",
				BodyText: tc.in,
			}, "d")
			if err != nil {
				t.Fatalf("Build: %v", err)
			}
			body := string(out.Wire)
			// Strip headers — the body starts after the
			// first \r\n\r\n.
			idx := strings.Index(body, "\r\n\r\n")
			if idx < 0 {
				t.Fatalf("no body separator")
			}
			gotBody := body[idx+4:]
			if !strings.Contains(gotBody, tc.want) {
				t.Errorf("expected body to contain %q, got %q", tc.want, gotBody)
			}
		})
	}
}

// TestBuild_CcRendered pins the Cc header (rare but supported).
func TestBuild_CcRendered(t *testing.T) {
	out, err := Build(fixedClock(time.Now()), Message{
		From:     "a@x.com",
		To:       []string{"b@x.com"},
		Cc:       []string{"c@x.com", "d@x.com"},
		Subject:  "s",
		BodyText: "b",
	}, "d")
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	mustContain(t, string(out.Wire), "Cc: c@x.com, d@x.com\r\n")
}

// TestBuild_NoInReplyToOrReferences pins that an outbound message
// with no threading headers (rare — first contact, or no parent
// message) is still well-formed; In-Reply-To / References are
// simply omitted.
func TestBuild_NoInReplyToOrReferences(t *testing.T) {
	out, err := Build(fixedClock(time.Now()), Message{
		From:     "a@x.com",
		To:       []string{"b@x.com"},
		Subject:  "First contact",
		BodyText: "Hello",
	}, "d")
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	wire := string(out.Wire)
	if strings.Contains(wire, "In-Reply-To:") {
		t.Errorf("expected no In-Reply-To when InReplyTo is empty")
	}
	if strings.Contains(wire, "References:") {
		t.Errorf("expected no References when slice is empty")
	}
}

// TestBuild_DateHeaderFormat pins RFC-1123Z format for the Date
// header. MTAs are pickier about this than the spec suggests.
func TestBuild_DateHeaderFormat(t *testing.T) {
	fixed := time.Date(2025, 6, 15, 14, 30, 45, 0, time.UTC)
	out, err := Build(fixedClock(fixed), Message{
		From: "a@x.com", To: []string{"b@x.com"}, Subject: "s", BodyText: "b",
	}, "d")
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	want := "Date: " + fixed.Format(time.RFC1123Z) + "\r\n"
	mustContain(t, string(out.Wire), want)
}

func mustContain(t *testing.T, haystack, needle string) {
	t.Helper()
	if !strings.Contains(haystack, needle) {
		t.Errorf("expected wire to contain %q\n--- got (first 600 chars) ---\n%s",
			needle, truncate(haystack, 600))
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
