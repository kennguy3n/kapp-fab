package imap

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"
)

// UIDState is the per-(tenant, mailbox) checkpoint persistence
// shape. Implementations back this with helpdesk_imap_state in
// Postgres; tests use the in-memory fake below.
type UIDState interface {
	// Get returns the stored uid_validity + last_uid. A
	// not-found returns (0, 0, nil) \u2014 the Poller treats that
	// as "first poll".
	Get(ctx context.Context, tenantID, mailboxID uuid.UUID) (uidValidity uint32, lastUID uint32, err error)

	// Set persists the new (uidValidity, lastUID). The
	// implementation MUST upsert; the Poller calls Set on
	// every successful fetch batch.
	Set(ctx context.Context, tenantID, mailboxID uuid.UUID, uidValidity, lastUID uint32) error

	// RecordError increments consecutive_errors and stores
	// the last error string. The Manager uses
	// consecutive_errors for backoff + alerting.
	RecordError(ctx context.Context, tenantID, mailboxID uuid.UUID, message string) error

	// ClearError resets consecutive_errors to 0 on a
	// successful poll.
	ClearError(ctx context.Context, tenantID, mailboxID uuid.UUID) error
}

// Processor is the inbound-email handoff. The Poller calls
// Process for every parsed message; the implementation is the
// helpdesk handler's ProcessThreaded path (which owns threading +
// the InboundEmailHandler.Process fallback). Decoupling via this
// interface keeps the IMAP package independent of the helpdesk
// package's import graph.
type Processor interface {
	// Process consumes one parsed inbound email. Errors are
	// treated as transient by the Poller (the message stays
	// un-acked; the next poll cycle re-processes); the
	// Message-ID dedup at the email_messages PRIMARY KEY
	// catches duplicates so re-delivery is safe.
	Process(ctx context.Context, tenantID uuid.UUID, mailboxID uuid.UUID, raw []byte, parsed ParsedEmail) error
}

// Config is the per-mailbox poller configuration. The Manager
// instantiates one Poller per Config.
type Config struct {
	TenantID  uuid.UUID
	MailboxID uuid.UUID
	Folder    Folder
	Username  string
	Password  string

	// PollInterval is the cadence between successful polls.
	// On error, the Poller applies exponential backoff
	// capped at MaxBackoff.
	PollInterval time.Duration

	// MaxBackoff caps the exponential backoff on consecutive
	// errors. The first error sleeps PollInterval; each
	// subsequent error doubles up to this cap.
	MaxBackoff time.Duration

	// FetchBatchSize caps a single FetchAfter call. 100 is
	// a sane default \u2014 enough to drain a quiet mailbox in
	// one shot, small enough that a big backlog comes in
	// chunks (so a single fetch failure doesn't waste a
	// 10k-message batch).
	FetchBatchSize int
}

// Withdefaults applies sensible defaults to zero-valued fields.
func (c *Config) withDefaults() {
	if c.PollInterval <= 0 {
		c.PollInterval = 60 * time.Second
	}
	if c.MaxBackoff <= 0 {
		c.MaxBackoff = 15 * time.Minute
	}
	if c.FetchBatchSize <= 0 {
		c.FetchBatchSize = 100
	}
}

// Poller is the per-mailbox event loop. Run() blocks until ctx is
// cancelled, polling cyclically. Errors are recorded to UIDState +
// logged; the loop continues with backoff so a transient outage
// doesn't take down the mailbox permanently.
type Poller struct {
	cfg       Config
	client    Client
	state     UIDState
	processor Processor
	logger    *slog.Logger

	// now is the clock used for backoff calculation. Tests
	// inject a deterministic clock.
	now func() time.Time
}

// NewPoller wires a Poller. Logger defaults to slog.Default() if
// nil. now defaults to time.Now if nil.
func NewPoller(cfg Config, client Client, state UIDState, processor Processor, logger *slog.Logger) (*Poller, error) {
	if client == nil {
		return nil, errors.New("imap: client required")
	}
	if state == nil {
		return nil, errors.New("imap: state required")
	}
	if processor == nil {
		return nil, errors.New("imap: processor required")
	}
	if cfg.TenantID == uuid.Nil {
		return nil, errors.New("imap: tenant id required")
	}
	if cfg.MailboxID == uuid.Nil {
		return nil, errors.New("imap: mailbox id required")
	}
	if cfg.Folder == "" {
		cfg.Folder = "INBOX"
	}
	cfg.withDefaults()
	if logger == nil {
		logger = slog.Default()
	}
	return &Poller{
		cfg: cfg, client: client, state: state, processor: processor,
		logger: logger, now: time.Now,
	}, nil
}

// SetClock injects a deterministic clock for tests. Production
// callers leave the constructor default.
func (p *Poller) SetClock(now func() time.Time) {
	if now != nil {
		p.now = now
	}
}

// PollOnce runs a single poll cycle: connect, login, select,
// fetch-and-process, logout. Returns the number of messages
// processed. Exposed (vs. only Run) so tests + the operator
// dashboard's "poll now" button can exercise it without spinning
// up the cyclic loop.
func (p *Poller) PollOnce(ctx context.Context) (int, error) {
	if err := p.client.Connect(ctx); err != nil {
		return 0, fmt.Errorf("imap: connect: %w", err)
	}
	defer func() {
		_ = p.client.Logout(ctx)
	}()
	if err := p.client.Login(ctx, p.cfg.Username, p.cfg.Password); err != nil {
		// Wrap auth errors so the Manager can fast-fail on them.
		if errors.Is(err, ErrAuth) {
			return 0, err
		}
		return 0, fmt.Errorf("imap: login: %w", err)
	}
	sel, err := p.client.Select(ctx, p.cfg.Folder)
	if err != nil {
		return 0, fmt.Errorf("imap: select %q: %w", p.cfg.Folder, err)
	}

	uidValid, lastUID, err := p.state.Get(ctx, p.cfg.TenantID, p.cfg.MailboxID)
	if err != nil {
		return 0, fmt.Errorf("imap: state get: %w", err)
	}
	// UIDVALIDITY mismatch \u2192 reset checkpoint, full re-scan.
	if uidValid != sel.UIDValidity {
		p.logger.Warn("imap: uidvalidity changed; resetting checkpoint",
			slog.String("tenant_id", p.cfg.TenantID.String()),
			slog.String("mailbox_id", p.cfg.MailboxID.String()),
			slog.Any("old_validity", uidValid),
			slog.Any("new_validity", sel.UIDValidity))
		lastUID = 0
	}

	processed := 0
	for {
		batch, err := p.client.FetchAfter(ctx, lastUID, p.cfg.FetchBatchSize)
		if err != nil {
			return processed, fmt.Errorf("imap: fetch after %d: %w", lastUID, err)
		}
		if len(batch) == 0 {
			break
		}
		for _, msg := range batch {
			parsed, perr := ParseRFC822(msg.Body)
			if perr != nil {
				p.logger.Warn("imap: parse failed; skipping message",
					slog.Uint64("uid", uint64(msg.UID)),
					slog.Any("error", perr))
				// Advance checkpoint past this message
				// anyway \u2014 a malformed message that
				// repeatedly fails parse would otherwise
				// block the whole mailbox.
				if msg.UID > lastUID {
					lastUID = msg.UID
				}
				continue
			}
			if err := p.processor.Process(ctx, p.cfg.TenantID, p.cfg.MailboxID, msg.Body, parsed); err != nil {
				return processed, fmt.Errorf("imap: process uid=%d: %w", msg.UID, err)
			}
			if msg.UID > lastUID {
				lastUID = msg.UID
			}
			processed++
		}
		if err := p.state.Set(ctx, p.cfg.TenantID, p.cfg.MailboxID, sel.UIDValidity, lastUID); err != nil {
			return processed, fmt.Errorf("imap: state set: %w", err)
		}
		// If the batch was smaller than the requested size,
		// we've drained the mailbox.
		if len(batch) < p.cfg.FetchBatchSize {
			break
		}
	}
	return processed, nil
}

// Run starts the cyclic poll loop and blocks until ctx is
// cancelled. Each iteration calls PollOnce; on success, sleep
// PollInterval; on error, sleep with exponential backoff up to
// MaxBackoff. UIDState.RecordError + ClearError track the run of
// consecutive failures for dashboard surfacing.
func (p *Poller) Run(ctx context.Context) error {
	backoff := p.cfg.PollInterval
	for {
		processed, err := p.PollOnce(ctx)
		if err != nil {
			// Auth errors are permanent \u2014 surface
			// immediately + bail out so the Manager
			// alerts the operator.
			if errors.Is(err, ErrAuth) {
				_ = p.state.RecordError(ctx, p.cfg.TenantID, p.cfg.MailboxID, err.Error())
				return err
			}
			p.logger.Warn("imap: poll cycle failed",
				slog.String("tenant_id", p.cfg.TenantID.String()),
				slog.String("mailbox_id", p.cfg.MailboxID.String()),
				slog.Any("error", err),
				slog.Duration("next_in", backoff))
			_ = p.state.RecordError(ctx, p.cfg.TenantID, p.cfg.MailboxID, err.Error())
			if !sleep(ctx, backoff) {
				return ctx.Err()
			}
			// Exponential backoff with cap.
			backoff *= 2
			if backoff > p.cfg.MaxBackoff {
				backoff = p.cfg.MaxBackoff
			}
			continue
		}
		_ = p.state.ClearError(ctx, p.cfg.TenantID, p.cfg.MailboxID)
		if processed > 0 {
			p.logger.Info("imap: poll cycle drained",
				slog.String("tenant_id", p.cfg.TenantID.String()),
				slog.String("mailbox_id", p.cfg.MailboxID.String()),
				slog.Int("processed", processed))
		}
		backoff = p.cfg.PollInterval
		if !sleep(ctx, backoff) {
			return ctx.Err()
		}
	}
}

// sleep blocks for d or until ctx is cancelled. Returns true if
// the full duration elapsed, false on ctx cancellation.
func sleep(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// Manager supervises one Poller per (tenant, mailbox). It owns
// the goroutine lifecycle: Start launches a Poller; Stop cancels
// it; ListActive returns the current set of running pollers for
// the operator dashboard.
type Manager struct {
	mu       sync.Mutex
	cancels  map[uuid.UUID]context.CancelFunc
	wg       sync.WaitGroup
	newPoller func(Config) (*Poller, error)
	logger   *slog.Logger
}

// NewManager wires a Manager. newPoller is the factory: in
// production this captures the shared Client / UIDState /
// Processor and produces a Poller per Config; in tests, it can
// return a fake.
func NewManager(newPoller func(Config) (*Poller, error), logger *slog.Logger) *Manager {
	if logger == nil {
		logger = slog.Default()
	}
	return &Manager{
		cancels:   make(map[uuid.UUID]context.CancelFunc),
		newPoller: newPoller,
		logger:    logger,
	}
}

// Start launches a Poller for cfg.MailboxID. If a Poller is
// already running for that mailbox, Start is a no-op + returns
// nil.
func (m *Manager) Start(parent context.Context, cfg Config) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.cancels[cfg.MailboxID]; ok {
		return nil
	}
	p, err := m.newPoller(cfg)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(parent)
	m.cancels[cfg.MailboxID] = cancel
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		defer func() {
			m.mu.Lock()
			delete(m.cancels, cfg.MailboxID)
			m.mu.Unlock()
		}()
		if err := p.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			m.logger.Error("imap: poller exited with error",
				slog.String("tenant_id", cfg.TenantID.String()),
				slog.String("mailbox_id", cfg.MailboxID.String()),
				slog.Any("error", err))
		}
	}()
	return nil
}

// Stop cancels the Poller for mailboxID. Returns false if no
// Poller was running for that mailbox.
func (m *Manager) Stop(mailboxID uuid.UUID) bool {
	m.mu.Lock()
	cancel, ok := m.cancels[mailboxID]
	if !ok {
		m.mu.Unlock()
		return false
	}
	delete(m.cancels, mailboxID)
	m.mu.Unlock()
	cancel()
	return true
}

// StopAll cancels every running Poller and waits for them all to
// exit. Used at worker shutdown.
func (m *Manager) StopAll() {
	m.mu.Lock()
	for _, c := range m.cancels {
		c()
	}
	m.cancels = make(map[uuid.UUID]context.CancelFunc)
	m.mu.Unlock()
	m.wg.Wait()
}

// ActiveMailboxes returns the set of mailbox-IDs currently being
// polled. Used by the operator dashboard.
func (m *Manager) ActiveMailboxes() []uuid.UUID {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]uuid.UUID, 0, len(m.cancels))
	for id := range m.cancels {
		out = append(out, id)
	}
	return out
}
