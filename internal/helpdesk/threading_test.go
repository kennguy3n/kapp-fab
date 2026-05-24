package helpdesk

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

// fakeMessageLookuper is an in-memory MessageLookuper for unit tests
// against the resolver's candidate-walk + freshness logic. Keyed on
// the angle-bracket-stripped MessageID so the resolver's
// normalisation behaviour is exercised end-to-end.
type fakeMessageLookuper struct {
	byID    map[string]*Message
	lookups []string // observed lookup order, oldest first
}

func newFakeMessageLookuper() *fakeMessageLookuper {
	return &fakeMessageLookuper{byID: map[string]*Message{}}
}

func (f *fakeMessageLookuper) Lookup(_ context.Context, tenantID uuid.UUID, messageID string) (*Message, error) {
	f.lookups = append(f.lookups, messageID)
	m, ok := f.byID[messageID]
	if !ok {
		return nil, ErrMessageNotFound
	}
	if m.TenantID != tenantID {
		return nil, ErrMessageNotFound
	}
	out := *m
	return &out, nil
}

func (f *fakeMessageLookuper) put(m Message) {
	f.byID[m.MessageID] = &m
}

// TestNormalizeMessageID pins the angle-bracket-stripping contract.
// The Message-ID lookup table is keyed on the bare form, so any
// caller passing the RFC-822 angle-bracketed form must be normalised
// to the same key. Failure to do so silently misses every parent
// lookup — caught here rather than at the DB.
func TestNormalizeMessageID(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"bare", "abc@host", "abc@host"},
		{"angle_bracketed", "<abc@host>", "abc@host"},
		{"angle_bracketed_with_spaces", "  <abc@host>  ", "abc@host"},
		{"only_opening_bracket", "<abc@host", "abc@host"},
		{"only_closing_bracket", "abc@host>", "abc@host"},
		{"empty", "", ""},
		{"angle_bracketed_empty", "<>", ""},
		{"whitespace_only", "   ", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := normalizeMessageID(tc.in)
			if got != tc.want {
				t.Fatalf("normalizeMessageID(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestThreadingResolver_InReplyToHit covers the canonical happy
// path: an inbound email with In-Reply-To matching a known message
// threads onto the existing ticket. Lookback default applies — the
// parent's ReceivedAt is "now-ish" so the freshness gate accepts it.
func TestThreadingResolver_InReplyToHit(t *testing.T) {
	tenant := uuid.New()
	ticket := uuid.New()
	fake := newFakeMessageLookuper()
	fake.put(Message{
		TenantID:   tenant,
		MessageID:  "parent@host",
		TicketID:   ticket,
		Direction:  DirectionInbound,
		ReceivedAt: time.Now().UTC().Add(-1 * time.Hour),
	})
	r := NewThreadingResolver(fake, 0)
	got, err := r.Resolve(context.Background(), tenant, InboundEmail{
		InReplyTo: "<parent@host>",
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got != ticket {
		t.Fatalf("expected ticket %s, got %s", ticket, got)
	}
}

// TestThreadingResolver_ReferencesWalkOrder pins the
// "newest-to-oldest" References walk. The References header carries
// the chain oldest-first per RFC-5322; the resolver must walk it in
// reverse so the immediate parent is probed before deeper ancestors.
// This matters when the chain has missing intermediate rows: A and
// C exist but B doesn't — the resolver should prefer C (closer) over
// A (root).
func TestThreadingResolver_ReferencesWalkOrder(t *testing.T) {
	tenant := uuid.New()
	rootTicket := uuid.New()
	leafTicket := uuid.New()
	fake := newFakeMessageLookuper()
	// Chain: A (root) → B (missing) → C (leaf parent). The
	// inbound email's References is [A, B, C] (oldest first).
	// Resolver should pick C, NOT A.
	fake.put(Message{
		TenantID:   tenant,
		MessageID:  "a@host",
		TicketID:   rootTicket,
		Direction:  DirectionInbound,
		ReceivedAt: time.Now().UTC().Add(-12 * time.Hour),
	})
	fake.put(Message{
		TenantID:   tenant,
		MessageID:  "c@host",
		TicketID:   leafTicket,
		Direction:  DirectionInbound,
		ReceivedAt: time.Now().UTC().Add(-1 * time.Hour),
	})
	r := NewThreadingResolver(fake, 0)
	got, err := r.Resolve(context.Background(), tenant, InboundEmail{
		References: []string{"<a@host>", "<b@host>", "<c@host>"},
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got != leafTicket {
		t.Fatalf("expected leaf ticket %s (newest match), got %s", leafTicket, got)
	}
	// Verify probe order — In-Reply-To missing means References
	// only, walked newest-to-oldest: C, B (miss), A skipped
	// because C already hit.
	if len(fake.lookups) < 1 || fake.lookups[0] != "c@host" {
		t.Fatalf("expected first probe to be c@host (newest), got %v", fake.lookups)
	}
}

// TestThreadingResolver_InReplyToBeatsReferences pins the dispatch
// order: In-Reply-To is the most specific signal and must win over
// References when both are present and both have hits. This guards
// against a stale References chain (auto-forwarder appended an
// unrelated thread) hijacking the reply.
func TestThreadingResolver_InReplyToBeatsReferences(t *testing.T) {
	tenant := uuid.New()
	inReplyTicket := uuid.New()
	refsTicket := uuid.New()
	fake := newFakeMessageLookuper()
	fake.put(Message{
		TenantID:   tenant,
		MessageID:  "direct-parent@host",
		TicketID:   inReplyTicket,
		ReceivedAt: time.Now().UTC().Add(-1 * time.Hour),
	})
	fake.put(Message{
		TenantID:   tenant,
		MessageID:  "refs-ancestor@host",
		TicketID:   refsTicket,
		ReceivedAt: time.Now().UTC().Add(-1 * time.Hour),
	})
	r := NewThreadingResolver(fake, 0)
	got, err := r.Resolve(context.Background(), tenant, InboundEmail{
		InReplyTo:  "<direct-parent@host>",
		References: []string{"<refs-ancestor@host>"},
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got != inReplyTicket {
		t.Fatalf("expected In-Reply-To ticket %s to win, got %s", inReplyTicket, got)
	}
	if fake.lookups[0] != "direct-parent@host" {
		t.Fatalf("expected first probe to be In-Reply-To, got %v", fake.lookups)
	}
}

// TestThreadingResolver_FreshnessGate pins the lookback enforcement.
// A parent older than the cutoff is rejected even when the
// MessageID matches — guards against Message-ID-replay hijacking a
// long-closed ticket.
func TestThreadingResolver_FreshnessGate(t *testing.T) {
	tenant := uuid.New()
	staleTicket := uuid.New()
	fake := newFakeMessageLookuper()
	fake.put(Message{
		TenantID:   tenant,
		MessageID:  "stale@host",
		TicketID:   staleTicket,
		ReceivedAt: time.Now().UTC().Add(-90 * 24 * time.Hour), // 90 days old
	})
	r := NewThreadingResolver(fake, 30*24*time.Hour) // 30-day window
	got, err := r.Resolve(context.Background(), tenant, InboundEmail{
		InReplyTo: "<stale@host>",
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got != uuid.Nil {
		t.Fatalf("expected freshness gate to reject stale parent, got ticket %s", got)
	}
}

// TestThreadingResolver_FreshnessGate_NegativeLookbackDisables pins
// that a negative lookback is the explicit "freshness gate off"
// signal — useful for tests against time-skewed parents and for
// operators who want infinite-lookback threading (acknowledging the
// replay risk).
func TestThreadingResolver_FreshnessGate_NegativeLookbackDisables(t *testing.T) {
	tenant := uuid.New()
	staleTicket := uuid.New()
	fake := newFakeMessageLookuper()
	fake.put(Message{
		TenantID:   tenant,
		MessageID:  "ancient@host",
		TicketID:   staleTicket,
		ReceivedAt: time.Now().UTC().Add(-365 * 24 * time.Hour), // a year old
	})
	r := NewThreadingResolver(fake, -1)
	got, err := r.Resolve(context.Background(), tenant, InboundEmail{
		InReplyTo: "<ancient@host>",
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got != staleTicket {
		t.Fatalf("expected negative lookback to disable freshness gate, got %s", got)
	}
}

// TestThreadingResolver_NoHeaders covers the first-message-in-a-
// thread case: no In-Reply-To, no References — resolver returns
// uuid.Nil so the handler opens a new ticket.
func TestThreadingResolver_NoHeaders(t *testing.T) {
	tenant := uuid.New()
	fake := newFakeMessageLookuper()
	r := NewThreadingResolver(fake, 0)
	got, err := r.Resolve(context.Background(), tenant, InboundEmail{})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got != uuid.Nil {
		t.Fatalf("expected uuid.Nil for missing headers, got %s", got)
	}
	if len(fake.lookups) != 0 {
		t.Fatalf("expected zero lookups for missing headers, got %v", fake.lookups)
	}
}

// TestThreadingResolver_AllCandidatesMiss covers the "unknown
// parents" case: headers present but no row in the store. Resolver
// returns uuid.Nil so the handler opens a new ticket — this is the
// dominant case for ANY first-message-with-a-Reply-To from a
// migrating-platform customer whose old thread predates email_messages.
func TestThreadingResolver_AllCandidatesMiss(t *testing.T) {
	tenant := uuid.New()
	fake := newFakeMessageLookuper()
	r := NewThreadingResolver(fake, 0)
	got, err := r.Resolve(context.Background(), tenant, InboundEmail{
		InReplyTo:  "<unknown@host>",
		References: []string{"<also-unknown@host>"},
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got != uuid.Nil {
		t.Fatalf("expected uuid.Nil for all-miss, got %s", got)
	}
	if len(fake.lookups) != 2 {
		t.Fatalf("expected 2 lookup probes (in-reply-to + 1 reference), got %d (%v)", len(fake.lookups), fake.lookups)
	}
}

// TestThreadingResolver_CrossTenantIsolation pins that a Message-ID
// that exists in tenant A does NOT match for tenant B even when the
// MessageID collides. The fake mirrors the DB's RLS-scoped lookup;
// a real implementation that forgot to scope by tenant would let an
// attacker open a ticket on tenant A by guessing a known A-side
// Message-ID from a publicly-visible source. This test guards the
// contract.
func TestThreadingResolver_CrossTenantIsolation(t *testing.T) {
	tenantA := uuid.New()
	tenantB := uuid.New()
	ticketA := uuid.New()
	fake := newFakeMessageLookuper()
	fake.put(Message{
		TenantID:   tenantA,
		MessageID:  "shared@host",
		TicketID:   ticketA,
		ReceivedAt: time.Now().UTC().Add(-1 * time.Hour),
	})
	r := NewThreadingResolver(fake, 0)
	got, err := r.Resolve(context.Background(), tenantB, InboundEmail{
		InReplyTo: "<shared@host>",
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got != uuid.Nil {
		t.Fatalf("expected cross-tenant isolation to return nil, got ticket %s", got)
	}
}

// TestThreadingResolver_PropagatesLookupError pins that transient
// errors (e.g. DB connection drop) propagate to the caller so the
// HTTP layer translates them to 5xx and the relay retries. The
// resolver only swallows ErrMessageNotFound.
func TestThreadingResolver_PropagatesLookupError(t *testing.T) {
	tenant := uuid.New()
	want := errors.New("simulated DB error")
	fake := &errorLookuper{err: want}
	r := NewThreadingResolver(fake, 0)
	_, err := r.Resolve(context.Background(), tenant, InboundEmail{
		InReplyTo: "<x@host>",
	})
	if !errors.Is(err, want) {
		t.Fatalf("expected propagation of underlying error, got %v", err)
	}
}

type errorLookuper struct {
	err error
}

func (e *errorLookuper) Lookup(_ context.Context, _ uuid.UUID, _ string) (*Message, error) {
	return nil, e.err
}

// TestThreadingResolver_TenantNilRejected pins the defence-in-depth
// check that catches programmer error (passing uuid.Nil tenant)
// before any lookup happens. Without this, a bug upstream that
// failed to resolve the tenant id would result in a tenant-scoped
// query running with no tenant context — which under RLS would
// return nothing, but the bug would be silent.
func TestThreadingResolver_TenantNilRejected(t *testing.T) {
	fake := newFakeMessageLookuper()
	r := NewThreadingResolver(fake, 0)
	_, err := r.Resolve(context.Background(), uuid.Nil, InboundEmail{
		InReplyTo: "<x@host>",
	})
	if err == nil {
		t.Fatalf("expected uuid.Nil tenant to be rejected")
	}
}

// TestNewThreadingResolver_NilStorePanics pins the constructor
// guard that catches the test-author mistake of passing nil and the
// production wiring mistake of forgetting to construct the store.
// Caught at boot, not at first request.
func TestNewThreadingResolver_NilStorePanics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatalf("expected panic on nil store")
		}
	}()
	_ = NewThreadingResolver(nil, 0)
}

// TestNewThreadingResolver_ZeroLookbackUsesDefault pins the
// "zero means default" sentinel. An operator who explicitly wants
// infinite lookback should pass a negative value (see the negative-
// lookback test); a zero argument shouldn't silently disable the
// gate.
func TestNewThreadingResolver_ZeroLookbackUsesDefault(t *testing.T) {
	fake := newFakeMessageLookuper()
	r := NewThreadingResolver(fake, 0)
	if r.lookback != DefaultThreadingLookback {
		t.Fatalf("expected zero lookback to expand to default %s, got %s", DefaultThreadingLookback, r.lookback)
	}
}
