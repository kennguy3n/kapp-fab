// Package spam implements a header-only spam scorer for inbound
// helpdesk email. The scorer is deliberately scoped to "what the
// upstream MTA already told us about authentication" + a small set
// of in-body heuristics; it does NOT perform external DNS lookups
// (SPF / DKIM / DMARC are read off the Authentication-Results header
// the relay attaches) so the helpdesk surface stays self-contained
// and a misconfigured DNS resolver can't open a tarpit on the
// inbound-email hot path.
//
// Reference: RFC 8601 (Authentication-Results header), SpamAssassin's
// header-based rule set, frappe/helpdesk's `is_spam` flag. The scorer
// produces an integer "spam score" + a structured slice of reasons
// the caller persists on the ticket and renders in the agent UI's
// triage view.
//
// Calibration: thresholds were picked to match SpamAssassin's
// default (5.0 → soft block, 7.0 → hard block), rounded to integers
// so the score can be stored in INTEGER columns without precision
// loss. Operators with stricter or looser tolerance can tune
// `Threshold` via `KAPP_HELPDESK_SPAM_THRESHOLD`.
package spam

import (
	"net/mail"
	"regexp"
	"strings"
)

// Result is the per-email spam decision. Score is the integer total;
// Reasons is the ordered list of rules that fired (with their
// individual point contributions) so the agent UI can render
// "why was this flagged".
type Result struct {
	// Score is the total points across all matched rules. Higher
	// = more spammy. The caller compares Score against its
	// Threshold to decide whether to open the ticket in "junk"
	// status vs "open".
	Score int

	// Reasons is the audit trail. Each entry pairs the rule
	// name with the points it contributed so the agent UI can
	// render a breakdown like "spam (8 points: SPF fail +5,
	// DMARC fail +5, no body content +3 minus +5 = ...)".
	Reasons []Reason

	// Decision is the bucket the caller surfaces in the UI.
	// "open" → score below threshold, treat as a normal
	// ticket. "junk" → score at or above threshold, still
	// open a ticket (so the agent can review false-positives)
	// but with the junk bucket so it doesn't pollute the
	// active queue.
	Decision string
}

// Reason is one rule's contribution to the total. Negative Points
// values represent positive signals (Reply-To matches From, etc.)
// that reduce the score below the dummy baseline.
type Reason struct {
	Rule   string `json:"rule"`
	Points int    `json:"points"`
	Detail string `json:"detail,omitempty"`
}

// Bucket constants. These match the helpdesk.ticket schema's status
// enum so the caller can map Decision → status field directly.
const (
	BucketOpen = "open"
	BucketJunk = "junk"
)

// DefaultThreshold is the integer score at or above which a message
// is marked junk. Matches SpamAssassin's default of 5.0 rounded up
// so a borderline message (e.g. SPF pass + Reply-To mismatch only)
// stays "open" — agents can still triage it, but a clearly
// spam-shaped message (multiple auth failures, suspicious subject
// patterns) is bucketed away.
const DefaultThreshold = 7

// Scorer applies the rule set to inbound headers + body. Stateless
// once constructed; callers can share a single Scorer across
// goroutines.
type Scorer struct {
	threshold int

	// whitelist is the set of From: domains that are always
	// scored 0. Populated from per-tenant config so operators
	// can rescue legitimate senders that keep tripping the
	// rules. Lower-cased on insert.
	whitelist map[string]struct{}
}

// Option tunes the scorer at construction time. Optional;
// `NewScorer(nil)` gives the platform defaults.
type Option func(*Scorer)

// WithThreshold overrides DefaultThreshold. Zero or negative values
// fall back to DefaultThreshold rather than disabling the bucket
// (a "always open, never junk" scorer is achievable by setting an
// arbitrarily high integer — making zero the disable sentinel would
// be a footgun).
func WithThreshold(t int) Option {
	return func(s *Scorer) {
		if t > 0 {
			s.threshold = t
		}
	}
}

// WithWhitelist registers a set of always-allowed sender domains
// (lower-cased and trimmed at registration). A message whose From:
// host matches a whitelist entry returns a Score of 0 regardless of
// other rule matches. Use for transactional partners (billing
// providers, SSO emails, CI notifications) that legitimately fire
// patterns the generic rules flag.
func WithWhitelist(domains []string) Option {
	return func(s *Scorer) {
		for _, d := range domains {
			d = strings.ToLower(strings.TrimSpace(d))
			if d != "" {
				s.whitelist[d] = struct{}{}
			}
		}
	}
}

// NewScorer wires a Scorer. Pass nil opts for the defaults.
func NewScorer(opts ...Option) *Scorer {
	s := &Scorer{
		threshold: DefaultThreshold,
		whitelist: map[string]struct{}{},
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// Headers is the slice of (key, value) pairs the scorer reads.
// Modeled as a slice (not a map) because RFC-5322 allows duplicate
// header names — and Authentication-Results in particular is often
// repeated by chained MTAs, each adding their own line. The scorer
// reads all of them.
type Headers []Header

// Header is one (Name, Value) pair. Name is canonicalised by the
// caller (Title-Case via net/textproto's CanonicalMIMEHeaderKey).
type Header struct {
	Name  string
	Value string
}

// Get returns the first value for a header name (case-insensitive).
// Empty when absent.
func (h Headers) Get(name string) string {
	for _, p := range h {
		if strings.EqualFold(p.Name, name) {
			return p.Value
		}
	}
	return ""
}

// All returns every value for a header name (case-insensitive).
// Used for Authentication-Results which is often repeated.
func (h Headers) All(name string) []string {
	out := []string{}
	for _, p := range h {
		if strings.EqualFold(p.Name, name) {
			out = append(out, p.Value)
		}
	}
	return out
}

// Email captures the fields the scorer reads. The caller assembles
// it from the upstream relay's parsed MIME or the IMAP poller's
// fetch result.
type Email struct {
	From     string
	To       string
	Subject  string
	BodyText string
	Headers  Headers
	HasAttachments bool
}

// suspiciousSubjectPattern matches common spam-shaped subjects. The
// pattern is intentionally minimal — Bayesian rules belong in a
// dedicated subsystem, NOT in the synchronous inbound-email path.
// The regex is compiled at init so per-call scoring is allocation-
// free in the regex path. Case-insensitive.
var suspiciousSubjectPattern = regexp.MustCompile(`(?i)\b(free|winner|congratulations|click here|verify your account|prince|inheritance|lottery|miracle|viagra|cialis)\b`)

// Score applies the rule set and returns the result. The scorer is
// stateless; concurrent calls are safe.
//
// Rule set (additive integer points):
//
//   - Authentication-Results SPF fail: +5
//   - Authentication-Results SPF softfail / temperror / permerror: +2
//   - Authentication-Results SPF none: +1 (sender's SPF record is missing
//     — softer signal than an explicit fail)
//   - Authentication-Results DKIM fail: +5
//   - Authentication-Results DKIM none: +1
//   - Authentication-Results DMARC fail: +5
//   - Authentication-Results DMARC none: +1
//   - Reply-To host != From host: +3 (spammers use this to redirect
//     replies to a throwaway domain)
//   - Subject matches suspicious-pattern regex: +2
//   - BodyText < 5 chars: +3 (legit support emails always carry a
//     description)
//   - Attachment-only with empty body: +1
//
// Whitelist short-circuit: if the From host is in the whitelist,
// the Result's Score is 0 and Reasons is empty.
func (s *Scorer) Score(email Email) Result {
	if s.fromIsWhitelisted(email.From) {
		return Result{
			Score:    0,
			Reasons:  nil,
			Decision: BucketOpen,
		}
	}

	out := Result{Reasons: []Reason{}}
	auths := email.Headers.All("Authentication-Results")
	out.scoreAuth(auths, "spf")
	out.scoreAuth(auths, "dkim")
	out.scoreAuth(auths, "dmarc")
	out.scoreReplyTo(email.Headers.Get("Reply-To"), email.From)
	out.scoreSubject(email.Subject)
	out.scoreBody(email.BodyText, email.HasAttachments)

	if out.Score >= s.threshold {
		out.Decision = BucketJunk
	} else {
		out.Decision = BucketOpen
	}
	return out
}

// fromIsWhitelisted returns true when the host portion of the
// From: address is registered in the whitelist. Parse errors fall
// through to "not whitelisted" — a malformed sender address
// should be scored, not waved through.
func (s *Scorer) fromIsWhitelisted(from string) bool {
	if len(s.whitelist) == 0 {
		return false
	}
	host := senderHost(from)
	if host == "" {
		return false
	}
	_, ok := s.whitelist[host]
	return ok
}

// scoreAuth reads the Authentication-Results header (RFC 8601) for
// one mechanism (spf / dkim / dmarc) and adds points based on the
// result. Multiple Authentication-Results headers (chained MTAs)
// are walked in turn — the WORST result wins so a downstream
// "pass" can't paper over an upstream "fail".
func (r *Result) scoreAuth(authResults []string, mechanism string) {
	worst := authPass // start at the most permissive
	for _, line := range authResults {
		got := extractAuthResult(line, mechanism)
		if rankAuth(got) > rankAuth(worst) {
			worst = got
		}
	}
	switch worst {
	case authFail:
		r.add(Reason{Rule: mechanism + "_fail", Points: 5, Detail: mechanism + "=fail"})
	case authSoftfail, authTemperror, authPermerror:
		if mechanism == "spf" { // softfail is SPF-specific terminology
			r.add(Reason{Rule: "spf_softfail", Points: 2, Detail: "spf=" + worst})
		} else {
			r.add(Reason{Rule: mechanism + "_temperror", Points: 2, Detail: mechanism + "=" + worst})
		}
	case authNone:
		r.add(Reason{Rule: mechanism + "_none", Points: 1, Detail: mechanism + "=none"})
	}
}

// Auth-results vocabulary per RFC 8601.
const (
	authPass      = "pass"
	authFail      = "fail"
	authSoftfail  = "softfail"
	authNone      = "none"
	authNeutral   = "neutral"
	authTemperror = "temperror"
	authPermerror = "permerror"
)

// rankAuth orders auth results by severity so the "worst across
// chained MTAs" reducer above stays correct. Higher rank = more
// suspicious.
func rankAuth(r string) int {
	switch r {
	case authPass:
		return 0
	case authNeutral, "":
		return 1
	case authNone:
		return 2
	case authSoftfail, authTemperror, authPermerror:
		return 3
	case authFail:
		return 4
	}
	return 1
}

// extractAuthResult parses an Authentication-Results header line
// (RFC 8601 §2.2). The header is structured but the parser is
// permissive — many MTAs emit slight syntactic variations. We
// match `<mechanism>=<result>` case-insensitively and stop at the
// first match per call. Returns empty string when the mechanism
// isn't mentioned on this line.
//
// Example input lines:
//
//	mx.acme.com; spf=pass smtp.mailfrom=...; dkim=pass header.d=acme.com; dmarc=pass action=none
//	authresult.example.com; spf=softfail; dkim=none; dmarc=none
func extractAuthResult(headerValue, mechanism string) string {
	lower := strings.ToLower(headerValue)
	target := strings.ToLower(mechanism) + "="
	idx := strings.Index(lower, target)
	if idx == -1 {
		return ""
	}
	rest := headerValue[idx+len(target):]
	// Walk to first non-alpha — the result token is alpha-only.
	end := 0
	for end < len(rest) {
		c := rest[end]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') {
			end++
			continue
		}
		break
	}
	return strings.ToLower(rest[:end])
}

// scoreReplyTo flags the canonical spam pattern of a Reply-To
// pointing at a different domain than the From. Legitimate uses
// (mailing-list managers, transactional bridges) are uncommon for
// inbound helpdesk traffic; the +3 contribution is below the
// junk threshold on its own.
func (r *Result) scoreReplyTo(replyTo, from string) {
	if replyTo == "" {
		return
	}
	rt := senderHost(replyTo)
	fr := senderHost(from)
	if rt == "" || fr == "" {
		return
	}
	if rt != fr {
		r.add(Reason{
			Rule:   "reply_to_mismatch",
			Points: 3,
			Detail: "reply-to=" + rt + " from=" + fr,
		})
	}
}

// scoreSubject runs the suspicious-subject regex. The regex is
// deliberately narrow — broader patterns (single-word triggers,
// emoji-only subjects) generate too many false positives on
// legitimate support traffic.
func (r *Result) scoreSubject(subject string) {
	if subject == "" {
		return
	}
	if suspiciousSubjectPattern.MatchString(subject) {
		r.add(Reason{
			Rule:   "suspicious_subject",
			Points: 2,
			Detail: "subject matched suspicious-pattern",
		})
	}
}

// scoreBody flags structurally-thin messages. A legit support
// ticket always carries enough body for the agent to triage the
// issue; a 1-character body is almost always a misfire (autoreply
// loops, click-tracking pixels).
func (r *Result) scoreBody(body string, hasAttachments bool) {
	trimmed := strings.TrimSpace(body)
	switch {
	case len(trimmed) < 5 && !hasAttachments:
		r.add(Reason{
			Rule:   "thin_body",
			Points: 3,
			Detail: "body shorter than 5 chars and no attachments",
		})
	case trimmed == "" && hasAttachments:
		r.add(Reason{
			Rule:   "attachment_only",
			Points: 1,
			Detail: "no body, attachments present",
		})
	}
}

// add appends a Reason and accumulates Points. Mutating helper for
// the rule-evaluation methods.
func (r *Result) add(reason Reason) {
	r.Reasons = append(r.Reasons, reason)
	r.Score += reason.Points
}

// senderHost extracts the host (lower-cased) from an RFC-5322
// address. Returns empty string on parse failure — callers treat
// that as "no host known" and skip host-based rules. The
// `net/mail` parser handles both the bare-address form
// (alice@host) and the angle-bracketed form (Alice
// <alice@host>).
func senderHost(addr string) string {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return ""
	}
	parsed, err := mail.ParseAddress(addr)
	if err != nil {
		// Permissive fallback: take the substring after the
		// last @ on the original input. Some legitimate
		// senders use display names that net/mail rejects
		// (raw UTF-8 without RFC-2047 encoding); we'd
		// rather still score the host than skip the rule.
		at := strings.LastIndex(addr, "@")
		if at == -1 || at == len(addr)-1 {
			return ""
		}
		return strings.ToLower(strings.TrimSpace(addr[at+1:]))
	}
	at := strings.LastIndex(parsed.Address, "@")
	if at == -1 {
		return ""
	}
	return strings.ToLower(parsed.Address[at+1:])
}
