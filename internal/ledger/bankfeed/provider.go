// Package bankfeed provides the multi-provider bank-feed abstraction
// that brings KApp's reconciliation surface to Xero parity: live feeds
// (Plaid, GoCardless Open Banking) alongside the existing CSV upload,
// behind one Provider interface. The package owns provider connections
// (persisted with field-encrypted credentials), tenant-configurable
// auto-categorization rules, and the hourly sync handler; the smart
// matching engine itself lives in the parent internal/ledger package
// (matcher.go) so it can reuse the journal-entry primitives without an
// import cycle.
//
// Security posture: provider credentials are encrypted at rest with the
// per-tenant key derived from KAPP_MASTER_KEY (internal/tenant/
// encryption.go) and are never logged. Every mutation emits an audit
// entry via audit.PGLogger. All new tables are RLS-scoped per tenant
// (migration 000082).
package bankfeed

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

// Provider names. Stored verbatim in bank_feed_connections.provider and
// used as the registry key, so they are part of the persisted contract —
// do not rename without a migration.
const (
	ProviderPlaid      = "plaid"
	ProviderGoCardless = "gocardless"
	ProviderCSV        = "csv"
)

// RawTransaction is a provider-neutral statement line. Providers map
// their native payload onto this shape; the sync handler then upserts
// each one as a finance.bank_transaction KRecord + typed row. Amount
// follows the ledger convention: positive = money into the account
// (credit/deposit), negative = money out (debit/payment).
type RawTransaction struct {
	// ExternalID is the provider's stable, idempotent id for the line
	// (Plaid transaction_id, GoCardless transactionId). It is the
	// dedup key on re-sync; an empty value means the provider does not
	// supply one and the sync handler must fall back to a content hash.
	ExternalID  string
	ValueDate   time.Time
	Description string
	Amount      decimal.Decimal
	Currency    string
	// Counterparty is the merchant / payer name when the provider
	// exposes it separately from the free-text description. Empty when
	// unavailable; rule evaluation falls back to Description.
	Counterparty string
	// Pending marks a not-yet-settled authorization. The sync handler
	// skips pending lines so a later settled version (with a final
	// amount) is the one that lands in the ledger.
	Pending bool
}

// Connection is one persisted (bank_account, provider) link. The token
// fields hold DECRYPTED credentials in memory only — the store encrypts
// them on write and decrypts on read. Callers must never log AccessToken
// or RefreshToken.
type Connection struct {
	ID            uuid.UUID
	TenantID      uuid.UUID
	BankAccountID uuid.UUID
	Provider      string
	AccessToken   string
	RefreshToken  string
	// Cursor is the provider's opaque incremental-sync marker. Persisted
	// in clear (not a secret) and advanced after each successful fetch.
	Cursor string
	// ExternalID is the provider's account/item handle, used to reuse a
	// connection row on re-link.
	ExternalID string
	Status     string
	LastSyncAt *time.Time
	LastError  string
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// Connection statuses, mirroring the bank_feed_connections CHECK.
const (
	StatusActive  = "active"
	StatusExpired = "expired"
	StatusRevoked = "revoked"
)

// Provider is the multi-provider abstraction. Implementations are
// stateless and safe for concurrent use: all per-connection state lives
// in the *Connection they are handed. InitiateConnect / CompleteConnect
// implement the provider's link handshake; FetchTransactions pulls the
// incremental delta since `since` (or since the connection cursor, which
// the provider prefers when set) and returns the advanced cursor.
type Provider interface {
	// Name returns the stable provider key (one of the Provider*
	// constants).
	Name() string

	// InitiateConnect begins the link flow and returns a URL or token
	// the frontend hands to the provider's widget (Plaid Link token,
	// GoCardless authorisation URL). redirectURI is where the provider
	// sends the user back after consent.
	InitiateConnect(ctx context.Context, tenantID, bankAccountID uuid.UUID, redirectURI string) (string, error)

	// CompleteConnect exchanges the provider's post-consent code (Plaid
	// public_token, GoCardless requisition reference) for durable
	// credentials and returns an unsaved Connection. The caller persists
	// it via the Store, which encrypts the tokens. TenantID and
	// BankAccountID on the returned Connection are populated by the
	// caller from request context, not the provider.
	CompleteConnect(ctx context.Context, tenantID uuid.UUID, code string) (*Connection, error)

	// FetchTransactions returns settled transactions and the advanced
	// cursor. Implementations prefer conn.Cursor when set and use `since`
	// only on the first sync (empty cursor). The returned cursor must be
	// persisted by the caller so the next sync is incremental.
	FetchTransactions(ctx context.Context, conn *Connection, since time.Time) ([]RawTransaction, string, error)

	// Disconnect revokes the link at the provider. Best-effort: a
	// provider-side failure is surfaced so the caller can still mark the
	// local connection revoked (a stale provider grant is harmless once
	// we stop syncing it).
	Disconnect(ctx context.Context, conn *Connection) error
}

// FetchDelta is the full incremental result of one provider sync walk:
// new lines plus, for feeds that express post-hoc edits, the lines whose
// content changed (Modified) and the external ids the provider retracted
// (Removed). Cursor is the advanced incremental marker the caller
// persists. Added carries RawTransactions; Modified carries the revised
// values for already-synced lines (matched by ExternalID); Removed lists
// the ExternalIDs to void.
type FetchDelta struct {
	Added    []RawTransaction
	Modified []RawTransaction
	Removed  []string
	Cursor   string
}

// ChangeFetcher is the optional capability a Provider implements when its
// incremental feed delivers post-hoc mutations beyond plain additions.
// Plaid satisfies it (the modified/removed arrays of /transactions/sync);
// GoCardless and CSV do not (their feeds are append-only from our view),
// so the sync handler falls back to FetchTransactions for them. Keeping
// it optional avoids widening the core Provider contract for providers
// that have nothing to mutate.
//
// FetchChanges must return the same Added/Cursor that FetchTransactions
// would (so a ChangeFetcher's FetchTransactions can delegate to it),
// plus the Modified/Removed deltas the caller applies after ingest.
type ChangeFetcher interface {
	FetchChanges(ctx context.Context, conn *Connection, since time.Time) (FetchDelta, error)
}

// Sentinel errors shared across providers and the HTTP/agent layers.
var (
	// ErrProviderNotConfigured is returned when a provider is selected
	// but its credentials are not present in config. In production this
	// is fail-closed at boot (see internal/platform/config.go); at
	// request time it maps to a 503-class response.
	ErrProviderNotConfigured = errors.New("bankfeed: provider not configured")
	// ErrUnsupported marks an operation a provider cannot perform (e.g.
	// InitiateConnect on the CSV provider, which has no live link).
	ErrUnsupported = errors.New("bankfeed: operation not supported by provider")
	// ErrUnknownProvider is returned by the Registry when no provider is
	// registered under the requested name.
	ErrUnknownProvider = errors.New("bankfeed: unknown provider")
)

// Registry resolves a provider name to its implementation. It is built
// once at startup from config and is read-only thereafter, so lookups
// need no locking.
type Registry struct {
	providers map[string]Provider
}

// NewRegistry builds a registry from the supplied providers. Nil entries
// are skipped so a caller can pass an optionally-nil Plaid/GoCardless
// provider (unconfigured) without a panic; the CSV provider is always
// available.
func NewRegistry(providers ...Provider) *Registry {
	m := make(map[string]Provider, len(providers))
	for _, p := range providers {
		if p == nil {
			continue
		}
		m[p.Name()] = p
	}
	return &Registry{providers: m}
}

// Get returns the provider registered under name, or ErrUnknownProvider.
func (r *Registry) Get(name string) (Provider, error) {
	if r == nil {
		return nil, ErrUnknownProvider
	}
	p, ok := r.providers[name]
	if !ok {
		return nil, ErrUnknownProvider
	}
	return p, nil
}

// Names returns the registered provider names (unordered). Used by the
// frontend bootstrap to advertise which connect buttons to show.
func (r *Registry) Names() []string {
	if r == nil {
		return nil
	}
	out := make([]string, 0, len(r.providers))
	for n := range r.providers {
		out = append(out, n)
	}
	return out
}
