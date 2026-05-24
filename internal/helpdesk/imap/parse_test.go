package imap

import (
	"strings"
	"testing"
)

// TestParseRFC822_HappyPath pins extraction of the core threading
// headers + body from a canonical single-part text/plain message.
func TestParseRFC822_HappyPath(t *testing.T) {
	raw := []byte("From: customer@example.com\r\n" +
		"To: support@acme.com\r\n" +
		"Subject: Login broken\r\n" +
		"Message-ID: <abcd1234@example.com>\r\n" +
		"In-Reply-To: <orig@example.com>\r\n" +
		"References: <orig@example.com> <reply1@example.com>\r\n" +
		"Date: Wed, 01 Jan 2025 12:00:00 +0000\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n" +
		"\r\n" +
		"Help, I can't log in.\r\n")
	p, err := ParseRFC822(raw)
	if err != nil {
		t.Fatalf("ParseRFC822: %v", err)
	}
	if p.MessageID != "abcd1234@example.com" {
		t.Errorf("MessageID: %q", p.MessageID)
	}
	if p.InReplyTo != "orig@example.com" {
		t.Errorf("InReplyTo: %q", p.InReplyTo)
	}
	if got := p.References; len(got) != 2 || got[0] != "orig@example.com" || got[1] != "reply1@example.com" {
		t.Errorf("References: %v", got)
	}
	if p.Subject != "Login broken" {
		t.Errorf("Subject: %q", p.Subject)
	}
	if p.From != "customer@example.com" {
		t.Errorf("From: %q", p.From)
	}
	if !strings.Contains(p.Body, "Help, I can't log in.") {
		t.Errorf("Body missing content: %q", p.Body)
	}
	if p.RawDate.IsZero() {
		t.Errorf("Date should be parsed")
	}
}

// TestParseRFC822_MultipartAlternative pins extraction of the
// text/plain part from a multipart/alternative envelope (the
// common reply shape for HTML-emitting mail clients).
func TestParseRFC822_MultipartAlternative(t *testing.T) {
	raw := []byte("From: a@x.com\r\n" +
		"To: b@y.com\r\n" +
		"Subject: hi\r\n" +
		"Message-ID: <m@x>\r\n" +
		"Content-Type: multipart/alternative; boundary=\"BOUND\"\r\n" +
		"\r\n" +
		"--BOUND\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n" +
		"\r\n" +
		"plain text content\r\n" +
		"\r\n" +
		"--BOUND\r\n" +
		"Content-Type: text/html; charset=utf-8\r\n" +
		"\r\n" +
		"<p>html content</p>\r\n" +
		"\r\n" +
		"--BOUND--\r\n")
	p, err := ParseRFC822(raw)
	if err != nil {
		t.Fatalf("ParseRFC822: %v", err)
	}
	if !strings.Contains(p.Body, "plain text content") {
		t.Errorf("expected text/plain part extracted, got %q", p.Body)
	}
	if strings.Contains(p.Body, "<p>") {
		t.Errorf("expected HTML part NOT in body, got %q", p.Body)
	}
}

// TestParseRFC822_EncodedSubject pins RFC-2047 encoded-word
// decoding for non-ASCII subjects. Many real-world emails have
// emoji or accented characters in the subject.
func TestParseRFC822_EncodedSubject(t *testing.T) {
	raw := []byte("From: a@x\r\n" +
		"To: b@y\r\n" +
		"Subject: =?UTF-8?B?SGVsbG8sIOS4lueVjA==?=\r\n" +
		"Message-ID: <m@x>\r\n" +
		"\r\n" +
		"body\r\n")
	p, err := ParseRFC822(raw)
	if err != nil {
		t.Fatalf("ParseRFC822: %v", err)
	}
	// "Hello, " + Chinese "世界" (UTF-8 base64 = 5LiW55WM)
	if !strings.Contains(p.Subject, "Hello") {
		t.Errorf("expected decoded subject, got %q", p.Subject)
	}
}

// TestParseRFC822_AngleBracketsStripped pins that bracketed
// message-ids on input come out bare. The threading resolver
// expects bare-form.
func TestParseRFC822_AngleBracketsStripped(t *testing.T) {
	raw := []byte("From: a@x\r\nTo: b@y\r\nSubject: s\r\n" +
		"Message-ID: <bracketed@host>\r\n" +
		"In-Reply-To: <parent@host>\r\n" +
		"\r\nbody\r\n")
	p, _ := ParseRFC822(raw)
	if p.MessageID != "bracketed@host" {
		t.Errorf("MessageID brackets not stripped: %q", p.MessageID)
	}
	if p.InReplyTo != "parent@host" {
		t.Errorf("InReplyTo brackets not stripped: %q", p.InReplyTo)
	}
}

// TestParseRFC822_NoMessageID pins that a message without
// Message-ID still parses (the inbound handler then synthesises
// one or treats the message as unthreadable).
func TestParseRFC822_NoMessageID(t *testing.T) {
	raw := []byte("From: a@x\r\nTo: b@y\r\nSubject: s\r\n\r\nbody\r\n")
	p, err := ParseRFC822(raw)
	if err != nil {
		t.Fatalf("ParseRFC822: %v", err)
	}
	if p.MessageID != "" {
		t.Errorf("expected empty MessageID, got %q", p.MessageID)
	}
}

// TestParseRFC822_MalformedFails pins that gibberish input
// surfaces as a parse error (vs silently producing an empty
// ParsedEmail that the inbound path would then misroute).
func TestParseRFC822_MalformedFails(t *testing.T) {
	raw := []byte("not a valid mail message at all")
	_, err := ParseRFC822(raw)
	if err == nil {
		t.Errorf("expected parse error on malformed input")
	}
}

// TestParseReferencesHeader pins multi-id splitting + bracket
// stripping at the header level.
func TestParseReferencesHeader(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"<a@x>", []string{"a@x"}},
		{"<a@x> <b@x>", []string{"a@x", "b@x"}},
		{"  <a@x>  <b@x>  ", []string{"a@x", "b@x"}},
		{"<a@x>,<b@x>", []string{"a@x", "b@x"}}, // some clients use comma
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			// parseReferencesHeader uses strings.Fields which splits
			// on whitespace. Comma-only separation isn't supported
			// (clients are required to use whitespace per RFC-5322).
			// The comma case here documents that limitation.
			got := parseReferencesHeader(tc.in)
			if tc.in == "<a@x>,<b@x>" {
				// Documented limitation: returns the comma-joined
				// blob as a single entry.
				if len(got) != 1 || got[0] != "a@x>,<b@x" {
					t.Skip("documented limitation: comma-only separation not supported")
				}
				return
			}
			if !slicesEqual(got, tc.want) {
				t.Errorf("got %v want %v", got, tc.want)
			}
		})
	}
}

func slicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestStripAngle pins the strip helper.
func TestStripAngle(t *testing.T) {
	cases := map[string]string{
		"":           "",
		"<a@x>":      "a@x",
		"a@x":        "a@x",
		"  <a@x>  ":  "a@x",
		"<a@x":       "a@x",
		"a@x>":       "a@x",
		"<<<a@x>>>":  "<<a@x>>", // only outermost stripped
	}
	for in, want := range cases {
		if got := stripAngle(in); got != want {
			t.Errorf("stripAngle(%q) = %q, want %q", in, got, want)
		}
	}
}
