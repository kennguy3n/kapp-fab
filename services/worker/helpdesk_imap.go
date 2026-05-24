// Surface G (PR-7) — IMAP supervisor wiring for the worker. The
// worker is the only service that owns IMAP polling: the API
// continues to serve the existing /helpdesk/inbound webhook (Phase
// 4 ingress) for relay-pushed mail, and the worker handles the pull
// side (one Poller goroutine per enabled mailbox row, supervised by
// the Manager).
//
// Wiring layout:
//
//   - helpdeskIMAPProcessor implements imap.Processor. It is the
//     bridge between the IMAP package (which speaks ParsedEmail)
//     and the helpdesk package (which speaks InboundEmail). One
//     instance is built per worker boot; every Poller goroutine
//     shares it.
//   - helpdeskIMAPSupervisor owns the lifecycle of every Poller. On
//     a ticker it calls mailboxes.Store.ListAllEnabled, asks the
//     Manager to Start each row's Poller (idempotent), and Stops
//     pollers whose mailbox row was disabled or deleted. The
//     Supervisor is leader-gated (only the elected leader runs it)
//     so two hot-standby workers do not double-poll the same
//     mailbox and trip Gmail's "Too many simultaneous connections"
//     cap.
//   - the passwordResolver bridge maps the row's imap_password_ref
//     scheme to a plaintext password at Start time. Production
//     wiring is secrets.RefResolver + secrets.PasswordCache: the
//     resolver dispatches env: / file:// / vault:// / aws:// /
//     gcp:// refs to the corresponding per-scheme handler, and the
//     cache amortises remote-backend reads across the converge
//     loop's 60-second cadence (5-minute TTL per mailbox UUID).
//     The legacy staticPasswordResolver (env:NAME only) is kept
//     for tests that don't wire the secrets package.
//
// The IMAP wire-protocol Client implementation is deliberately NOT
// included here — go-imap/v1 sits behind the imap.Client interface
// in a follow-up "imap client adapter" commit. Until that adapter
// lands, the worker boots with the Supervisor disabled (logged
// INFO) so a misconfigured production deployment does not crash on
// the missing client factory; the entire helpdesk-IMAP path stays
// dark until both surfaces are in.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kennguy3n/kapp-fab/internal/helpdesk"
	"github.com/kennguy3n/kapp-fab/internal/helpdesk/imap"
	"github.com/kennguy3n/kapp-fab/internal/helpdesk/mailboxes"
	"github.com/kennguy3n/kapp-fab/internal/platform"
	"github.com/kennguy3n/kapp-fab/internal/record"
	"github.com/kennguy3n/kapp-fab/internal/secrets"
)

// imapClientFactory builds a fresh imap.Client per (host, port,
// useTLS) tuple. Each Poller owns one Client for its entire
// lifetime; the Manager invokes the factory once per Start. The
// factory is injected from outside so this file does not
// transitively import go-imap/v1 — a deliberate decoupling so the
// helpdesk-IMAP wire-up can compile + test in CI without the
// go-imap dependency until the adapter PR lands.
type imapClientFactory func(host string, port int, useTLS bool, logger *slog.Logger) imap.Client

// helpdeskIMAPProcessor adapts the Poller's Processor interface to
// the helpdesk.InboundEmailHandler.ProcessThreaded entry point. The
// Poller speaks ParsedEmail; the handler speaks InboundEmail. This
// adapter is the seam that lets the two packages stay decoupled (no
// import cycle) while sharing the same threading + dedup +
// ticket-creation logic.
type helpdeskIMAPProcessor struct {
	handler      *helpdesk.InboundEmailHandler
	logger       *slog.Logger
	recipientFor func(tenantID, mailboxID uuid.UUID) string
}

// Process translates one Poller-emitted ParsedEmail into an
// InboundEmail and hands it to ProcessThreaded. All errors propagate
// verbatim so the Poller's retry/backoff machinery treats them as
// transient; the message stays un-acked + email_messages PK catches
// duplicates if processing eventually succeeds after a re-fetch.
func (p *helpdeskIMAPProcessor) Process(ctx context.Context, tenantID, mailboxID uuid.UUID, raw []byte, parsed imap.ParsedEmail) error {
	var to string
	if p.recipientFor != nil {
		to = p.recipientFor(tenantID, mailboxID)
	}
	if strings.TrimSpace(to) == "" {
		// No recipient lookup wired for this mailbox. Fall
		// back to the parsed To: header — the resolver
		// dispatches on host anyway, which is the part the
		// mailbox name carries. Logged at DEBUG so the
		// operator can see the fall-through in development.
		to = parsed.To
		p.logger.Debug("helpdesk: imap processor — no recipient mapping; using parsed To",
			slog.String("tenant_id", tenantID.String()),
			slog.String("mailbox_id", mailboxID.String()),
			slog.String("parsed_to", parsed.To))
	}
	receivedAt := parsed.RawDate
	if receivedAt.IsZero() {
		receivedAt = time.Now().UTC()
	}
	email := helpdesk.InboundEmail{
		MessageID:  parsed.MessageID,
		To:         to,
		From:       parsed.From,
		Subject:    parsed.Subject,
		BodyText:   parsed.Body,
		InReplyTo:  parsed.InReplyTo,
		References: parsed.References,
		ReceivedAt: receivedAt,
		Source:     "imap",
	}
	if _, err := p.handler.ProcessThreaded(ctx, email); err != nil {
		return fmt.Errorf("imap: handler process: %w", err)
	}
	p.logger.Debug("imap: processed message",
		slog.String("tenant_id", tenantID.String()),
		slog.String("mailbox_id", mailboxID.String()),
		slog.String("message_id", parsed.MessageID),
		slog.Int("raw_bytes", len(raw)),
	)
	return nil
}

// recipientTable maps (tenantID, mailboxID) → recipient address used
// by the helpdesk handler's TenantResolver. The Supervisor populates
// this from the mailbox row's Name on every converge; the processor
// adapter looks it up at Process time. The mailbox Name is the full
// support address (e.g. "support@acme.kapp.io") — see
// mailboxes.Validate.
type recipientTable struct {
	mu sync.RWMutex
	m  map[uuid.UUID]map[uuid.UUID]string
}

func newRecipientTable() *recipientTable {
	return &recipientTable{
		m: make(map[uuid.UUID]map[uuid.UUID]string),
	}
}

func (t *recipientTable) set(tenantID, mailboxID uuid.UUID, recipient string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, ok := t.m[tenantID]; !ok {
		t.m[tenantID] = make(map[uuid.UUID]string)
	}
	t.m[tenantID][mailboxID] = recipient
}

func (t *recipientTable) get(tenantID, mailboxID uuid.UUID) string {
	t.mu.RLock()
	defer t.mu.RUnlock()
	if m, ok := t.m[tenantID]; ok {
		return m[mailboxID]
	}
	return ""
}

func (t *recipientTable) deleteMailbox(mailboxID uuid.UUID) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, m := range t.m {
		delete(m, mailboxID)
	}
}

// clientRegistry holds per-mailbox pre-attached imap.Client
// handles. The Supervisor's converge loop stashes the freshly-built
// client here before calling Manager.Start; the Manager's
// newPoller hook reads + removes it. Protected by a mutex because
// converge() (run on a ticker) and newPoller (called from inside
// Manager.Start while holding m.mu) can race when a tick fires
// while a Poller is being constructed for the previous tick's row.
type clientRegistry struct {
	mu sync.Mutex
	m  map[uuid.UUID]imap.Client
}

func newClientRegistry() *clientRegistry {
	return &clientRegistry{m: make(map[uuid.UUID]imap.Client)}
}

func (r *clientRegistry) put(mailboxID uuid.UUID, client imap.Client) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.m[mailboxID] = client
}

// take consumes the client for mailboxID (removes it from the map).
// Returns nil + false if no client was registered.
func (r *clientRegistry) take(mailboxID uuid.UUID) (imap.Client, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	c, ok := r.m[mailboxID]
	if !ok {
		return nil, false
	}
	delete(r.m, mailboxID)
	return c, true
}

// helpdeskIMAPSupervisor owns the lifecycle of every per-mailbox
// Poller goroutine across the entire worker. It periodically polls
// mailboxes.Store.ListAllEnabled and converges the Manager's set of
// running pollers to match the live enabled set — adding new
// mailboxes, stopping disabled / deleted ones. Runs ONLY on the
// elected leader.
type helpdeskIMAPSupervisor struct {
	store         mailboxes.Store
	manager       *imap.Manager
	clientFactory imapClientFactory
	clients       *clientRegistry
	passwords     passwordResolver
	recipients    *recipientTable
	convergeEvery time.Duration
	logger        *slog.Logger
}

// Run blocks until ctx is cancelled. It calls converge() once
// immediately on entry so the worker has live pollers within
// milliseconds of leader election, then on every tick until
// shutdown. On cancellation it calls Manager.StopAll, which waits
// for every Poller goroutine to unwind before returning so the
// worker process exits with no dangling IMAP connections.
func (s *helpdeskIMAPSupervisor) Run(ctx context.Context) error {
	s.logger.Info("helpdesk: imap supervisor starting",
		slog.Duration("converge_every", s.convergeEvery))
	if err := s.converge(ctx); err != nil {
		s.logger.Warn("helpdesk: imap supervisor initial converge failed",
			slog.String("err", err.Error()))
	}
	tick := time.NewTicker(s.convergeEvery)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			s.logger.Info("helpdesk: imap supervisor draining")
			s.manager.StopAll()
			s.logger.Info("helpdesk: imap supervisor drained")
			return nil
		case <-tick.C:
			if err := s.converge(ctx); err != nil {
				s.logger.Warn("helpdesk: imap supervisor converge failed",
					slog.String("err", err.Error()))
			}
		}
	}
}

// converge enumerates every enabled mailbox, builds an imap.Config
// for each, attaches the client, and asks the Manager to Start it.
// Already-running mailboxes are no-ops (Manager.Start short-circuits
// on a duplicate key). Disabled or deleted mailboxes are stopped by
// diffing the manager's active set against the live enabled set.
// Idempotent — safe to call on every tick.
func (s *helpdeskIMAPSupervisor) converge(ctx context.Context) error {
	rows, err := s.store.ListAllEnabled(ctx)
	if err != nil {
		return fmt.Errorf("list enabled mailboxes: %w", err)
	}
	want := make(map[uuid.UUID]bool, len(rows))
	for i := range rows {
		row := &rows[i]
		want[row.MailboxID] = true
		s.recipients.set(row.TenantID, row.MailboxID, row.Name)
		// Skip Start if already running — avoids the work of
		// resolving the password + building a client every
		// tick. The Manager would no-op the Start anyway but
		// the password resolve can be expensive (vault round
		// trip in the future), and a freshly-built client
		// would leak.
		if s.manager.IsActive(row.MailboxID) {
			continue
		}
		password, perr := s.passwords.Resolve(ctx, row.MailboxID.String(), row.IMAPPasswordRef)
		if perr != nil {
			s.logger.Warn("helpdesk: resolve mailbox password failed; skipping",
				slog.String("tenant_id", row.TenantID.String()),
				slog.String("mailbox_id", row.MailboxID.String()),
				slog.String("password_ref", row.IMAPPasswordRef),
				slog.String("err", perr.Error()))
			continue
		}
		client := s.clientFactory(row.IMAPHost, row.IMAPPort, row.IMAPUseTLS, s.logger)
		s.clients.put(row.MailboxID, client)
		cfg := imap.Config{
			TenantID:       row.TenantID,
			MailboxID:      row.MailboxID,
			Folder:         imap.Folder(row.Folder),
			Username:       row.IMAPUsername,
			Password:       string(password),
			PollInterval:   row.PollInterval(),
			MaxBackoff:     row.MaxBackoff(),
			FetchBatchSize: row.FetchBatchSizeOrDefault(),
		}
		if err := s.manager.Start(ctx, cfg); err != nil {
			// Clean up the stashed client — newPoller never
			// consumed it because Start short-circuited. The
			// Client may hold an open TCP / TLS connection
			// (the goimap adapter dials in Connect, which the
			// Poller would have called on its first Run tick);
			// Close() drops the socket without attempting a
			// LOGOUT handshake that the half-open session
			// can't answer cleanly.
			if stashed, ok := s.clients.take(row.MailboxID); ok {
				if cerr := stashed.Close(); cerr != nil {
					s.logger.Warn("helpdesk: close orphan imap client",
						slog.String("mailbox_id", row.MailboxID.String()),
						slog.String("err", cerr.Error()))
				}
			}
			if errors.Is(err, imap.ErrManagerStopped) {
				// Worker is shutting down; abort the
				// converge — Run will exit on the next
				// ctx.Done.
				return nil
			}
			s.logger.Warn("helpdesk: manager start failed",
				slog.String("tenant_id", row.TenantID.String()),
				slog.String("mailbox_id", row.MailboxID.String()),
				slog.String("err", err.Error()))
		}
	}
	// Stop any mailbox that's running but no longer enabled.
	for _, active := range s.manager.ActiveMailboxes() {
		if !want[active] {
			s.manager.Stop(active)
			s.recipients.deleteMailbox(active)
			// Drop the cached password so a re-enable
			// against a different ref does not pick up
			// stale credentials; also flushes the bytes
			// from process memory rather than holding
			// them for the full TTL.
			s.passwords.InvalidateScope(active.String())
		}
	}
	return nil
}

// passwordResolver bridges a (mailbox-scope, ref) tuple to a
// resolved plaintext password. The supervisor calls Resolve on
// every converge tick for every mailbox; the resolver's cache
// absorbs repeat hits within the TTL so an IMAP fleet of N
// mailboxes does not produce 60*N Vault / AWS SM reads per
// hour. InvalidateScope is called when a mailbox row is
// disabled / deleted so stale credentials do not linger in
// memory longer than necessary.
//
// The interface is narrow so unit tests for the supervisor can
// inject a fake without dragging in the secrets package's
// per-backend setup.
type passwordResolver interface {
	Resolve(ctx context.Context, scope string, ref string) ([]byte, error)
	InvalidateScope(scope string)
}

// newPasswordResolver returns the production wiring: a
// secrets.RefResolver dispatching env / file / vault / aws /
// gcp by scheme prefix, wrapped in a secrets.PasswordCache
// keyed on the mailbox UUID with the supplied TTL. ttl <= 0
// disables the cache (useful for tests that need every Resolve
// to fire the backend).
func newPasswordResolver(opts secrets.RefResolverOptions, ttl time.Duration) passwordResolver {
	return secrets.NewPasswordCache(secrets.NewRefResolver(opts), ttl)
}

// newWorkerPasswordResolver builds the production passwordResolver
// from the worker's loaded platform.Config. Each remote backend
// (Vault / AWS Secrets Manager / GCP Secret Manager) is wired in
// only when its config is populated:
//
//   - Vault when KAPP_SECRETS_VAULT_ADDR + KAPP_SECRETS_VAULT_TOKEN
//     are both set;
//   - AWS when KAPP_SECRETS_AWS_REGION is set;
//   - GCP when KAPP_SECRETS_GCP_PROJECT_ID is set.
//
// Unconfigured backends return ErrProviderNotConfigured at Resolve
// time for refs pointing at them. env: and file:// refs work
// unconditionally — they're stateless and never need a Provider.
//
// Errors during backend construction (e.g. an AWS region was set
// but the SDK rejected the config) are logged at WARN and the
// backend is left unwired. The worker boots either way; refs
// pointing at the unconfigured backend will fail loudly at the
// converge tick that needs them, which is the right surface for
// an operator misconfiguration.
//
// ttl is the per-mailbox cache TTL; the worker passes 5 minutes
// in the production wiring (matching the docstring on the
// passwordResolver interface).
func newWorkerPasswordResolver(ctx context.Context, cfg *platform.Config, ttl time.Duration, logger *slog.Logger) passwordResolver {
	opts := secrets.RefResolverOptions{}
	if cfg.SecretsVaultAddr != "" && cfg.SecretsVaultToken != "" {
		vault, err := secrets.NewVaultProvider(secrets.VaultProviderConfig{
			Addr:      cfg.SecretsVaultAddr,
			Token:     cfg.SecretsVaultToken,
			MountPath: cfg.SecretsVaultMountPath,
			SecretKey: cfg.SecretsVaultSecretKey,
		})
		if err != nil {
			logger.Warn("helpdesk: vault provider unavailable; vault:// refs will fail",
				slog.String("err", err.Error()))
		} else {
			opts.Vault = vault
		}
	}
	if cfg.SecretsAWSRegion != "" {
		aws, err := secrets.NewAWSProvider(ctx, secrets.AWSProviderConfig{
			Region:   cfg.SecretsAWSRegion,
			Prefix:   cfg.SecretsAWSPrefix,
			Endpoint: cfg.SecretsAWSEndpoint,
		})
		if err != nil {
			logger.Warn("helpdesk: aws provider unavailable; aws:// refs will fail",
				slog.String("err", err.Error()))
		} else {
			opts.AWS = aws
		}
	}
	if cfg.SecretsGCPProjectID != "" {
		gcp, err := secrets.NewGCPProvider(ctx, secrets.GCPProviderConfig{
			ProjectID: cfg.SecretsGCPProjectID,
			Prefix:    cfg.SecretsGCPPrefix,
			Version:   cfg.SecretsGCPVersion,
		})
		if err != nil {
			logger.Warn("helpdesk: gcp provider unavailable; gcp:// refs will fail",
				slog.String("err", err.Error()))
		} else {
			opts.GCP = gcp
		}
	}
	return newPasswordResolver(opts, ttl)
}

// staticPasswordResolver is the legacy env-only resolver. It
// stays in place for the disabled-supervisor smoke tests where
// the worker boot does not construct the secrets package's
// per-backend wiring; the converge path never touches it when
// the supervisor is wired with a real RefResolver.
type staticPasswordResolver struct{}

var _ passwordResolver = (*staticPasswordResolver)(nil)

// Resolve handles env:NAME refs only; all other schemes return
// an error so the operator sees a clear failure at converge
// time. The full RefResolver is the production path.
func (staticPasswordResolver) Resolve(_ context.Context, _, ref string) ([]byte, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return nil, errors.New("password ref is empty")
	}
	if !strings.HasPrefix(ref, "env:") {
		return nil, fmt.Errorf("static resolver only supports env: refs; got scheme %q", schemeOf(ref))
	}
	key := strings.TrimPrefix(ref, "env:")
	val := os.Getenv(key)
	if val == "" {
		return nil, fmt.Errorf("env var %q is empty or unset", key)
	}
	return []byte(val), nil
}

// InvalidateScope is a no-op on the static resolver — nothing
// to drop.
func (staticPasswordResolver) InvalidateScope(_ string) {}

// schemeOf extracts the scheme prefix for diagnostic logging.
// Conservative: returns the substring up to (but not including) the
// first ":" or "/" so the error message is short + safe.
func schemeOf(ref string) string {
	for i, r := range ref {
		if r == ':' || r == '/' {
			return ref[:i]
		}
	}
	return ref
}

// helpdeskIMAPState bundles the supervisor + the processor adapter
// + the helpdesk inbound handler so leaderState carries one field
// instead of three. Built once at boot via newHelpdeskIMAPState;
// nil when adminPool is unavailable (the supervisor needs it for
// ListAllEnabled) or when no client factory is wired.
type helpdeskIMAPState struct {
	supervisor *helpdeskIMAPSupervisor
}

// newHelpdeskIMAPState wires the full helpdesk-IMAP stack against
// the shared worker dependencies. Returns nil when:
//
//   - adminPool is nil (Supervisor's ListAllEnabled needs it to
//     scan across tenants via the admin-bypass RLS policy).
//   - clientFactory is nil (no IMAP wire-protocol adapter wired —
//     the supervisor would have no way to actually connect).
//
// nil is the correct shape for both cases: leadWorker skips the
// supervisor.Run goroutine, the rest of the worker continues
// normally, and the helpdesk-IMAP path stays inert. The webhook
// inbound path (api/helpdesk_inbound_handlers.go) is unaffected.
func newHelpdeskIMAPState(
	pool, adminPool *pgxpool.Pool,
	recordStore *record.PGStore,
	helpdeskStore *helpdesk.Store,
	clientFactory imapClientFactory,
	passwords passwordResolver,
	logger *slog.Logger,
) *helpdeskIMAPState {
	if adminPool == nil {
		logger.Info("helpdesk: imap supervisor disabled — admin pool not configured")
		return nil
	}
	if clientFactory == nil {
		logger.Info("helpdesk: imap supervisor disabled — no client factory wired (go-imap adapter PR pending)")
		return nil
	}
	if passwords == nil {
		logger.Info("helpdesk: imap supervisor disabled — no password resolver wired")
		return nil
	}
	resolver := helpdesk.NewPGTenantResolver(adminPool)
	messages := helpdesk.NewMessageStore(pool)
	// 30-day threading lookback matches the API path's default
	// (services/api/deps_build.go's helpdesk wiring).
	threading := helpdesk.NewThreadingResolver(messages, 30*24*time.Hour)
	handler := helpdesk.NewInboundEmailHandler(resolver, recordStore, helpdeskStore, workerSystemActor).
		WithThreading(messages, threading)

	store := mailboxes.NewPGStore(pool, adminPool)
	stateStore := imap.NewPGUIDState(pool)
	clients := newClientRegistry()
	recipients := newRecipientTable()
	processor := &helpdeskIMAPProcessor{
		handler:      handler,
		logger:       logger,
		recipientFor: recipients.get,
	}
	manager := imap.NewManager(func(cfg imap.Config) (*imap.Poller, error) {
		client, ok := clients.take(cfg.MailboxID)
		if !ok {
			return nil, fmt.Errorf("no imap client attached for mailbox %s (converge ordering bug)", cfg.MailboxID)
		}
		return imap.NewPoller(cfg, client, stateStore, processor, logger)
	}, logger)
	supervisor := &helpdeskIMAPSupervisor{
		store:           store,
		manager:         manager,
		clientFactory:   clientFactory,
		clients:         clients,
		passwords:     passwords,
		recipients:      recipients,
		convergeEvery:   60 * time.Second,
		logger:          logger,
	}
	return &helpdeskIMAPState{supervisor: supervisor}
}
