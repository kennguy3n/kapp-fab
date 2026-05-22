package main

import (
	"errors"
	"log"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/kennguy3n/kapp-fab/internal/authz"
	"github.com/kennguy3n/kapp-fab/internal/ledger"
	"github.com/kennguy3n/kapp-fab/internal/notifications"
	"github.com/kennguy3n/kapp-fab/internal/platform"
	"github.com/kennguy3n/kapp-fab/internal/sales"
	"github.com/kennguy3n/kapp-fab/internal/tenant"
)

// registerRoutes is the single seam between dependency wiring and
// HTTP surface. main.go's run() builds an apiDeps and then hands it
// here; every chi.Mux configuration the binary serves lives in this
// function or its sub-mounts. Keeping it as one function preserves
// the original main.go's middleware/route order — chi's route
// matcher is order-sensitive (static segments preferred over
// {params}, longer prefixes preferred over shorter), so splitting
// across multiple chi.Mux instances would risk subtle precedence
// regressions.
//
// A few stores are constructed inline (notifications.NewStore,
// the per-portal mailer, the consolidation/tier-upgrade handlers).
// They each depend on an enclosing-block condition (e.g. only when
// `d.iah != nil` and `d.adminPool != nil`) so hoisting them into
// apiDeps would either duplicate the nil-checks or instantiate
// stores that the binary never serves. Leaving them inline keeps
// the conditional shape close to the routes that use them.
func registerRoutes(d *apiDeps) chi.Router {
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(middleware.Recoverer)

	r.Get("/healthz", healthHandler(d.pool))
	r.Get("/api/v1/", rootHandler)

	// Phase H auth routes. SSO and refresh are unauthenticated (they
	// bootstrap the auth context); the rest of the surface will be
	// migrated onto the Bearer-token middleware over subsequent PRs
	// while the X-Tenant-ID header keeps working for local dev.
	r.Route("/api/v1/auth", func(r chi.Router) {
		r.Post("/sso", d.authh.sso)
		r.Post("/refresh", d.authh.refresh)
	})

	// Phase F event stream. SSE tail of the tenant's outbox so the web
	// UI can react to state changes without polling. Defined at the root
	// router so it does NOT inherit the 30s request timeout applied below
	// — chi's middleware.Timeout wraps the ResponseWriter and cancels the
	// context after the deadline, which would break any long-lived
	// stream. Idempotency/rate-limit are also skipped because SSE is a
	// GET and a spammed subscription is bounded by connection count.
	r.Route("/api/v1/events", func(r chi.Router) {
		d.tenantChain(r)
		r.Use(d.apiCallMW)
		r.Get("/stream", d.eh.stream)
	})

	// All non-streaming routes run under a 30s request deadline so a
	// slow handler can't hold a connection open indefinitely. The SSE
	// stream is deliberately registered above this group.
	r.Group(func(r chi.Router) {
		r.Use(middleware.Timeout(30 * time.Second))

		// Helpdesk customer portal. Auth endpoints are unauthenticated
		// — they run the magic-link flow themselves. Ticket endpoints
		// require a portal-scoped JWT issued by /auth/verify. No
		// X-Tenant-ID header is expected on portal routes; the tenant
		// is taken from the JWT (for data routes) or the request body
		// (for auth endpoints) so external customers never have to
		// know their tenant's internal UUID.
		//
		// Registered inside the 30s timeout group so a slow or
		// malicious portal client can't hold a goroutine + DB conn
		// open indefinitely. Portal handlers are regular request /
		// response, no streaming, so the deadline is safe.
		if d.authh.signer != nil {
			// Real SMTP delivery for magic links. When SMTPHost is
			// empty the failingPortalMailer is wired so every
			// /portal/auth/request errors out — production mis-
			// configurations must surface visibly rather than fall
			// back to log.Printf'ing tokens to stdout, which is
			// where they previously ended up via the old stdout
			// stub.
			var pmailer portalMailer
			if d.cfg.SMTPHost != "" {
				smtpAdapter := notifications.NewSMTPAdapter(notifications.SMTPConfig{
					Host:     d.cfg.SMTPHost,
					Port:     d.cfg.SMTPPort,
					User:     d.cfg.SMTPUser,
					Password: d.cfg.SMTPPassword,
					From:     d.cfg.SMTPFrom,
				})
				pmailer = portalSMTPMailer{sender: smtpAdapter}
			} else {
				pmailer = failingPortalMailer{
					err: errors.New("portal: SMTP not configured (set SMTP_HOST); cannot send magic links"),
				}
				log.Printf("api: WARN portal magic-link mailer disabled (SMTP_HOST empty); /portal/auth/request will return 503 until SMTP is configured")
			}
			porh := &portalHandlers{
				tenants:  d.tenantSvc,
				portal:   d.portalStore,
				signer:   d.authh.signer,
				records:  d.recordStore,
				mailer:   pmailer,
				features: d.featureStore,
			}
			r.Route("/api/v1/portal", func(r chi.Router) {
				r.Route("/auth", func(r chi.Router) {
					// /auth/* gate inline inside the handlers — they need
					// the tenant lookup first and can't share the
					// claims-based middleware below.
					r.Post("/request", porh.requestMagicLink)
					r.Post("/verify", porh.verifyMagicLink)
				})
				r.Route("/tickets", func(r chi.Router) {
					r.Use(portalAuthMiddleware(d.authh.signer))
					// FeaturePortal gate sits after auth so the tenant
					// is taken from the JWT claims — standard
					// DynamicFeatureMiddleware cannot be used here
					// because the portal skips TenantMiddleware.
					r.Use(portalFeatureMiddleware(d.featureStore))
					// Bridge the portal claims into the platform tenant
					// + user context slots so the standard rate-limit /
					// api-call / quota / idempotency middleware below
					// runs unchanged. Without this the portal surface
					// would have no rate limiting and a stolen portal
					// JWT could create unbounded ticket replies.
					r.Use(portalTenantContextMiddleware(d.tenantSvc))
					r.Use(d.apiCallMW)
					r.Use(platform.IdempotencyMiddleware(d.pool))
					r.Use(d.rateLimitMW)
					r.Use(platform.QuotaMiddleware(d.quotaEnforcer))
					r.Get("/", porh.listTickets)
					r.Post("/", porh.createTicket)
					r.Get("/{id}", porh.getTicket)
					r.Post("/{id}/reply", porh.replyTicket)
				})
			})
		}

		// Control-plane tenant lifecycle routes. The /me sub-tree
		// is user-facing (any authenticated tenant member can read
		// its own features / usage / plan) so it runs under
		// tenantChain — auth.Middleware derives the tenant from the
		// JWT claim, not the X-Tenant-ID request header that
		// platform.TenantMiddleware honored before Phase 1.
		//
		// Why this matters: changePlan reads the tenant from URL
		// params (which changePlanMe populates from header-derived
		// ctx) with no user-identity check of its own. Before the
		// switch to tenantChain, sending
		//   POST /api/v1/tenants/me/plan
		//   X-Tenant-ID: <victim-uuid>
		// from an unauthenticated client downgraded the victim
		// tenant's plan. tenantChain closes that gap.
		//
		// tenantChain also mounts auth.RequireActiveHomeTenant so a
		// platform admin admitted via the recovery bypass cannot
		// ALSO mutate tenant-scoped data on the inactive tenant
		// via /me. Admin recovery proceeds through adminChain (the
		// {id} sub-tree below), which intentionally omits the
		// guard.
		//
		// Everything else under /api/v1/tenants mutates or reads
		// across tenants and is gated behind adminChain (JWT +
		// IsPlatformAdmin). /me is registered first so chi's
		// static-segment preference matches it before the {id}
		// routes underneath.
		r.Route("/api/v1/tenants", func(r chi.Router) {
			r.Group(func(r chi.Router) {
				d.tenantChain(r)
				r.Route("/me", func(r chi.Router) {
					r.Get("/features", d.feath.listMe)
					r.Get("/usage", d.meth.usageMe)
					r.Get("/usage/history", d.meth.usageHistory)
					r.Post("/plan", d.meth.changePlanMe)
				})
			})

			r.Group(func(r chi.Router) {
				d.adminChain(r)
				r.Get("/", d.th.list)
				r.Post("/", d.th.create)
				r.Get("/{id}", d.th.get)
				r.Post("/{id}/suspend", d.th.suspend)
				r.Post("/{id}/activate", d.th.activate)
				r.Post("/{id}/archive", d.th.archive)
				r.Delete("/{id}", d.th.delete)
				r.Post("/{id}/setup", d.th.setup)
				r.Get("/{id}/features", d.feath.list)
				r.Put("/{id}/features", d.feath.update)
				r.Get("/{id}/placement", d.plch.get)
				r.Put("/{id}/placement", d.plch.put)
				if d.reth != nil {
					r.Get("/{id}/retention", d.reth.list)
					r.Put("/{id}/retention", d.reth.put)
				}
				r.Get("/{id}/usage", d.meth.usage)
				r.Get("/{id}/usage/history", d.meth.usageHistory)
				r.Post("/{id}/plan", d.meth.changePlan)
			})
		})

		// Plan definitions are shared metadata (not tenant-scoped) so
		// they live at /api/v1/plans alongside /api/v1/ktypes.
		r.Route("/api/v1/plans", func(r chi.Router) {
			r.Get("/", d.meth.listPlans)
		})

		// Phase J/K — runtime isolation audit. Returns the JSON
		// report from platform.IsolationAuditor.Run. Admin-only
		// in spirit; the route group is intentionally not wrapped
		// in TenantMiddleware because the audit must run with the
		// admin GUC. Operators authenticate via the same JWT
		// envelope as other admin surfaces.
		if d.iah != nil {
			r.Route("/api/v1/admin", func(r chi.Router) {
				d.adminChain(r)
				r.Get("/isolation-audit", d.iah.get)
				// Phase G — tier upgrade endpoint. Replaces the
				// scripts/upgrade_tier.d.sh shell script with an
				// admin-only API call. Requires d.adminPool because
				// CREATE SCHEMA + cross-schema INSERT must run
				// outside any tenant-scoped RLS context.
				if d.adminPool != nil {
					tih := &tierUpgradeHandlers{
						tenants:   d.th,
						adminPool: d.adminPool,
						auditor:   d.auditor,
					}
					r.Post("/tenants/{id}/upgrade-tier", tih.upgrade)

					// Phase M Task 7 — admin-only multi-tenant
					// consolidation. The store reads each member
					// tenant's trial balance via the admin d.pool
					// (BYPASSRLS) so a single run can span tenants.
					// Reuses d.apiExchangeRates so the consolidation
					// rate translations converge on the same in-process
					// rate store that the ledger and the /currencies
					// browser endpoints already share — a separate store
					// would not be incorrect (both wrap the same pool)
					// but it would split any future in-memory caching
					// or rotation logic into two parallel copies.
					consStore := ledger.NewConsolidationStore(d.adminPool, d.ledgerStore, d.apiExchangeRates)
					ch := &consolidationHandlers{store: consStore}
					r.Post("/consolidation/groups", ch.createGroup)
					r.Post("/consolidation/groups/{id}/run", ch.run)
				}
			})
		}

		// Phase RBAC — role and permission management. Tenant-scoped
		// and gated behind authz.Middleware so only an actor with
		// `tenant.admin` (or wildcard) can mutate the role graph.
		// Mutations invalidate the authz cache for the affected
		// tenant so the next request sees the new grants.
		r.Route("/api/v1/roles", func(r chi.Router) {
			d.tenantChain(r)
			r.Use(d.apiCallMW)
			r.Use(authz.Middleware(d.authzEval, "tenant.admin", ""))
			r.Use(platform.IdempotencyMiddleware(d.pool))
			r.Use(d.rateLimitMW)
			r.Use(platform.QuotaMiddleware(d.quotaEnforcer))
			r.Get("/", d.roleh.listRoles)
			r.Post("/", d.roleh.createRole)
			r.Put("/{name}", d.roleh.updateRole)
			r.Delete("/{name}", d.roleh.deleteRole)
			r.Get("/{name}/permissions", d.roleh.listPermissions)
			r.Post("/{name}/permissions", d.roleh.grantPermission)
			r.Delete("/{name}/permissions/{id}", d.roleh.revokePermission)
		})
		r.Route("/api/v1/users", func(r chi.Router) {
			d.tenantChain(r)
			r.Use(d.apiCallMW)
			r.Use(authz.Middleware(d.authzEval, "tenant.admin", ""))
			r.Use(d.rateLimitMW)
			r.Get("/{id}/roles", d.roleh.listUserRoles)
			r.Post("/{id}/roles", d.roleh.assignUserRole)
			r.Delete("/{id}/roles/{role}", d.roleh.removeUserRole)
		})

		// KType registry routes (shared metadata, not tenant-scoped).
		// GET is public so the web UI can render the schema list and
		// per-KType detail without a JWT. POST mutates the install-
		// wide schema registry and is gated behind the platform
		// admin chain — a misbehaving tenant should not be able to
		// register or replace a KType that every other tenant sees.
		r.Route("/api/v1/ktypes", func(r chi.Router) {
			r.Get("/", d.kh.list)
			r.Get("/{name}", d.kh.get)
			r.Group(func(r chi.Router) {
				d.adminChain(r)
				r.Post("/", d.kh.register)
			})
		})

		// Webhook management + delivery-log surface. Gated behind
		// the per-tenant `webhook` feature flag (derived from the
		// path via DynamicFeatureMiddleware). CRUD runs under the
		// same middleware stack as other mutation routes so the
		// tenant cannot bypass idempotency / rate-limit / quota.
		r.Route("/api/v1/webhooks", func(r chi.Router) {
			d.tenantChain(r)
			r.Use(d.apiCallMW)
			r.Use(d.featureMW)
			r.Use(platform.IdempotencyMiddleware(d.pool))
			r.Use(d.rateLimitMW)
			r.Use(platform.QuotaMiddleware(d.quotaEnforcer))
			r.Get("/", d.whh.list)
			r.Post("/", d.whh.create)
			r.Get("/{id}", d.whh.get)
			r.Put("/{id}", d.whh.update)
			r.Delete("/{id}", d.whh.delete)
			r.Get("/{id}/deliveries", d.whh.deliveries)
		})

		// Full-text search across the krecords table. Reads are
		// tenant-scoped (RLS on krecords already covers it) so the
		// group only needs tenant + api-call middleware; idempotency
		// and quota are skipped because GET /search is a pure read.
		r.Route("/api/v1/search", func(r chi.Router) {
			d.tenantChain(r)
			r.Use(d.apiCallMW)
			r.Use(d.rateLimitMW)
			r.Get("/", d.sh.search)
		})

		// KRecord CRUD routes. These require tenant context, rate limiting,
		// quota enforcement, and idempotency keys on mutations.
		r.Route("/api/v1/records", func(r chi.Router) {
			d.tenantChain(r)
			r.Use(d.apiCallMW)
			r.Use(d.featureMW)
			r.Use(d.authzMethodGate("krecord.read", "krecord.write", ""))
			// Idempotency runs before rate-limit/quota so a replay of a
			// previously-successful mutation returns the cached response even
			// when the tenant has since hit its rate-limit or quota ceiling —
			// the replay is not a new unit of work (ARCHITECTURE.md §8 rule 6).
			r.Use(platform.IdempotencyMiddleware(d.pool))
			r.Use(d.rateLimitMW)
			r.Use(platform.QuotaMiddleware(d.quotaEnforcer))
			r.Post("/{ktype}", d.rh.create)
			r.Get("/{ktype}", d.rh.list)
			// Bulk actions endpoint — multi-id status_change, delete,
			// or CSV export in one transaction. Matches the pattern
			// frappe/frappe uses on its List View: the UI collects
			// selected rows and dispatches to a single backend entry
			// point rather than looping over per-row endpoints.
			r.Post("/{ktype}/bulk", d.rh.bulk)
			r.Get("/{ktype}/{id}", d.rh.get)
			r.Patch("/{ktype}/{id}", d.rh.update)
			r.Delete("/{ktype}/{id}", d.rh.delete)
			// Print surface — HTML preview + PDF download per
			// record. Sits under /records so the tenant +
			// rate-limit middleware is inherited, but the
			// FeaturePrint flag is enforced explicitly here:
			// DynamicFeatureMiddleware keys on the URL domain
			// segment ("records") which has no per-feature
			// mapping, so the print routes would otherwise be
			// silently un-gated even when the tenant's plan has
			// FeaturePrint=false.
			r.Group(func(pr chi.Router) {
				pr.Use(platform.FeatureMiddleware(d.featureStore, tenant.FeaturePrint))
				pr.Get("/{ktype}/{id}/pdf", d.ph.pdf)
				pr.Get("/{ktype}/{id}/html", d.ph.html)
			})
			// Workflow action endpoint (ARCHITECTURE.md §10). Runs under the
			// same tenant + idempotency + rate-limit + quota stack as record
			// CRUD so a spammed transition can't starve other tenants.
			r.Post("/{ktype}/{id}/actions/{action}", d.wh.action)
			// Workflow-run read endpoint. The list/kanban UI hydrates the
			// RightPane from this so it can show the authoritative state
			// the engine holds rather than inferring it from the record
			// data field (ARCHITECTURE.md §7).
			r.Get("/{ktype}/{id}/workflow-run", d.wh.getRunByRecord)
		})

		// Agent tool invocation surface. ARCHITECTURE.md §10-§11 requires
		// every mutation to be tenant-scoped and attributable, so mutating
		// calls run under the same middleware stack as record CRUD. The
		// read-only list endpoint lives in the same route group for
		// discoverability even though it does not need idempotency.
		r.Route("/api/v1/agents", func(r chi.Router) {
			d.tenantChain(r)
			r.Use(d.apiCallMW)
			r.Use(d.featureMW)
			r.Use(d.authzGate("tenant.member", ""))
			r.Use(platform.IdempotencyMiddleware(d.pool))
			r.Use(d.rateLimitMW)
			r.Use(platform.QuotaMiddleware(d.quotaEnforcer))
			r.Get("/tools", d.ah.list)
			r.Post("/tools/{name}", d.ah.invoke)
		})

		// Approvals surface. GET endpoints are safe to replay under
		// IdempotencyMiddleware (the middleware short-circuits non-mutating
		// methods) and the mutations (POST /, POST /{id}/decide) need the
		// same tenant + idempotency + rate-limit + quota stack as record
		// CRUD so a spammed approve / reject can't starve other tenants.
		r.Route("/api/v1/approvals", func(r chi.Router) {
			d.tenantChain(r)
			r.Use(d.apiCallMW)
			r.Use(d.featureMW)
			r.Use(platform.IdempotencyMiddleware(d.pool))
			r.Use(d.rateLimitMW)
			r.Use(platform.QuotaMiddleware(d.quotaEnforcer))
			r.Get("/", d.aph.list)
			r.Post("/", d.aph.create)
			r.Get("/{id}", d.aph.get)
			r.Post("/{id}/decide", d.aph.decide)
		})

		// Audit log read surface. Queries the audit_log table under tenant
		// context via dbutil.WithTenantTx so RLS is enforced. Admin-only in
		// production; auth enforcement lands with the broader auth layer
		// in Phase C.
		r.Route("/api/v1/audit", func(r chi.Router) {
			d.tenantChain(r)
			r.Use(d.apiCallMW)
			r.Use(d.featureMW)
			r.Use(d.authzGate("tenant.admin", ""))
			r.Get("/", d.auh.list)
			r.Get("/verify", d.auh.verify)
		})

		// Finance surface (Phase C). Chart of accounts, journal entries,
		// invoice/bill posting, period lockout, and reports. Mutations
		// need the full tenant + idempotency + rate-limit + quota stack
		// because a spammed post can't be allowed to starve other tenants
		// or double-post an invoice under replay.
		r.Route("/api/v1/finance", func(r chi.Router) {
			d.tenantChain(r)
			r.Use(d.apiCallMW)
			r.Use(d.featureMW)
			r.Use(d.authzMethodGate("finance.read", "finance.admin", ""))
			r.Use(platform.IdempotencyMiddleware(d.pool))
			r.Use(d.rateLimitMW)
			r.Use(platform.QuotaMiddleware(d.quotaEnforcer))
			r.Post("/accounts", d.finh.createAccount)
			r.Get("/accounts", d.finh.listAccounts)
			r.Get("/accounts/{code}", d.finh.getAccount)
			r.Post("/journal-entries", d.finh.postJournalEntry)
			r.Get("/journal-entries", d.finh.listJournalEntries)
			r.Get("/journal-entries/{id}", d.finh.getJournalEntry)
			r.Post("/invoices/{id}/post", d.finh.postInvoice)
			r.Post("/bills/{id}/post", d.finh.postBill)
			r.Post("/credit-notes/{id}/post", d.finh.postCreditNote)
			r.Post("/debit-notes/{id}/post", d.finh.postDebitNote)
			r.Post("/payments/{id}/post", d.finh.postPayment)
			r.Post("/tax-codes", d.finh.upsertTaxCode)
			r.Get("/tax-codes", d.finh.listTaxCodes)
			r.Get("/tax-codes/{code}", d.finh.getTaxCode)
			r.Post("/periods/lock", d.finh.lockPeriod)
			r.Get("/reports/trial-balance", d.finh.trialBalance)
			r.Get("/reports/ar-aging", d.finh.arAging)
			r.Get("/reports/ap-aging", d.finh.apAging)
			r.Get("/reports/income-statement", d.finh.incomeStatement)
			// Phase I — exchange rate CRUD + ad-hoc convert + unrealized
			// gain/loss calculator. Lookups do not mutate so they skip
			// the idempotency key requirement enforced by the middleware.
			r.Post("/exchange-rates", d.curh.upsertRate)
			r.Get("/exchange-rates", d.curh.listRates)
			r.Get("/exchange-rates/convert", d.curh.convert)
			r.Post("/exchange-rates/unrealized", d.curh.unrealizedGL)
		})

		// Phase M Task 6 — POS finalize. Reuses InvoicePoster +
		// PaymentPoster for the underlying double-entry; this
		// route just handles the orchestration. Gated on FeaturePOS
		// via the dynamic feature middleware (path → "pos").
		posh := &posHandlers{poster: sales.NewPOSPoster(d.recordStore, d.invoicePoster, d.paymentPoster)}
		r.Route("/api/v1/pos", func(r chi.Router) {
			d.tenantChain(r)
			r.Use(d.apiCallMW)
			r.Use(d.featureMW)
			r.Use(platform.IdempotencyMiddleware(d.pool))
			r.Use(d.rateLimitMW)
			r.Use(platform.QuotaMiddleware(d.quotaEnforcer))
			r.Post("/invoices/{id}/finalize", posh.finalize)
		})

		// Phase J payroll surface — generate draft payslips for a
		// pay_run and post the approved batch as a single journal
		// entry. The pay_run / payslip KRecords themselves ride the
		// generic CRUD at /api/v1/records/hr.pay_run and hr.payslip.
		r.Route("/api/v1/hr", func(r chi.Router) {
			d.tenantChain(r)
			r.Use(d.apiCallMW)
			r.Use(d.featureMW)
			r.Use(d.authzGate("hr.admin", ""))
			r.Use(platform.IdempotencyMiddleware(d.pool))
			r.Use(d.rateLimitMW)
			r.Use(platform.QuotaMiddleware(d.quotaEnforcer))
			r.Post("/pay-runs/{id}/generate", d.hrh.generatePayslips)
			r.Post("/pay-runs/{id}/post", d.hrh.postPayRun)
			r.Get("/pay-runs/{id}/payslips", d.hrh.listPayRunPayslips)
		})

		// Phase I helpdesk surface. Tickets themselves ride the generic
		// KRecord CRUD at /api/v1/records/helpdesk.ticket; these routes
		// back the SLA policy list/upsert the UI needs when authoring
		// policies and the per-ticket SLA log the right pane renders.
		r.Route("/api/v1/helpdesk", func(r chi.Router) {
			d.tenantChain(r)
			r.Use(d.apiCallMW)
			r.Use(d.featureMW)
			r.Use(d.authzMethodGate("helpdesk.read", "helpdesk.admin", ""))
			r.Use(platform.IdempotencyMiddleware(d.pool))
			r.Use(d.rateLimitMW)
			r.Use(platform.QuotaMiddleware(d.quotaEnforcer))
			r.Post("/sla-policies", d.hdh.upsertPolicy)
			r.Get("/sla-policies", d.hdh.listPolicies)
			r.Get("/sla-policies/resolve", d.hdh.resolvePolicy)
			r.Get("/tickets/{id}/sla-log", d.hdh.ticketLog)
		})

		// Inbound email → ticket. Sits OUTSIDE the JWT-tenant
		// middleware because the relay does not carry session
		// credentials; instead we authenticate by static shared
		// secret and resolve the tenant from the recipient host.
		//
		// Rate-limit MUST be IP-keyed here, not tenant-keyed: the
		// route runs before the handler resolves which tenant the
		// recipient belongs to, so the tenant-scoped d.rateLimitMW
		// would call TenantFromContext → nil → 500 on every
		// request. d.publicInboundIPLimit is the right shape — it
		// keeps a misconfigured relay or a forged-sender flood
		// from saturating the inbound pipeline without depending
		// on tenant context.
		if d.inboundHandler != nil {
			r.Route("/api/v1/helpdesk/inbound-email", func(r chi.Router) {
				r.Use(d.publicInboundIPLimit)
				r.Post("/", d.inboundHandler.post)
			})
		}

		// Phase I reports surface. Saved report CRUD + ad-hoc execution
		// under the same tenant/idempotency/rate-limit/quota stack so
		// spammed runs cannot starve other tenants.
		r.Route("/api/v1/reports", func(r chi.Router) {
			d.tenantChain(r)
			r.Use(d.apiCallMW)
			r.Use(d.featureMW)
			r.Use(d.authzGate("reports.read", ""))
			r.Use(platform.IdempotencyMiddleware(d.pool))
			r.Use(d.rateLimitMW)
			r.Use(platform.QuotaMiddleware(d.quotaEnforcer))
			r.Get("/", d.reph.list)
			r.Post("/", d.reph.create)
			r.Post("/run", d.reph.runAdhoc)
			r.Get("/{id}", d.reph.get)
			r.Put("/{id}", d.reph.update)
			r.Delete("/{id}", d.reph.delete)
			r.Get("/{id}/run", d.reph.runSaved)
			r.Patch("/{id}/share", d.reph.share)
		})

		// Phase K — data export queue. Submission enqueues; the
		// worker (services/worker/export_worker.go) drains it and
		// streams payload via /download.
		r.Route("/api/v1/exports", func(r chi.Router) {
			d.tenantChain(r)
			r.Use(d.apiCallMW)
			r.Use(d.featureMW)
			r.Use(platform.IdempotencyMiddleware(d.pool))
			r.Use(d.rateLimitMW)
			r.Use(platform.QuotaMiddleware(d.quotaEnforcer))
			r.Get("/", d.exph.list)
			r.Post("/", d.exph.create)
			r.Get("/{id}", d.exph.get)
			r.Get("/{id}/download", d.exph.download)
		})

		// Phase K — report schedules. CRUD only; the worker owns
		// dispatch via reporting.ActionTypeReportSchedule.
		r.Route("/api/v1/report-schedules", func(r chi.Router) {
			d.tenantChain(r)
			r.Use(d.apiCallMW)
			r.Use(d.featureMW)
			r.Use(platform.IdempotencyMiddleware(d.pool))
			r.Use(d.rateLimitMW)
			r.Use(platform.QuotaMiddleware(d.quotaEnforcer))
			r.Get("/", d.repsh.list)
			r.Post("/", d.repsh.create)
			r.Get("/{id}", d.repsh.get)
			r.Put("/{id}", d.repsh.update)
			r.Delete("/{id}", d.repsh.delete)
		})

		// Phase L Insights. CRUD for saved queries + dashboards,
		// cache-aware query execution under per-tenant
		// statement_timeout, dashboard widget upsert/delete, and
		// role/user share grants. Gated on the `insights`
		// feature flag via DynamicFeatureMiddleware so a free /
		// starter plan can't reach the surface even with a
		// stolen tenant header.
		r.Route("/api/v1/insights", func(r chi.Router) {
			d.tenantChain(r)
			r.Use(d.apiCallMW)
			r.Use(d.featureMW)
			r.Use(d.authzGate("insights.read", ""))
			r.Use(platform.IdempotencyMiddleware(d.pool))
			r.Use(d.rateLimitMW)
			r.Use(platform.QuotaMiddleware(d.quotaEnforcer))

			r.Route("/queries", func(r chi.Router) {
				r.Get("/", d.insh.listQueries)
				r.Post("/", d.insh.createQuery)
				r.Get("/{id}", d.insh.getQuery)
				r.Put("/{id}", d.insh.updateQuery)
				r.Delete("/{id}", d.insh.deleteQuery)
				r.Post("/{id}/run", d.insh.runQuery)
				// Raw-SQL editor mode (Phase M). Gated by an
				// additional `insights_sql_editor` feature flag
				// on top of the parent `insights` gate so a non-
				// enterprise plan with a stolen tenant header
				// can't reach the surface even with `insights`
				// turned on.
				r.Group(func(r chi.Router) {
					r.Use(platform.FeatureMiddleware(d.featureStore, tenant.FeatureInsightsSQLEditor))
					r.Post("/{id}/run-sql", d.insh.runRawSQL)
				})
				r.Post("/{id}/share", d.insh.shareQuery)
				r.Get("/{id}/shares", d.insh.listQueryShares)
				r.Delete("/{id}/shares/{shareID}", d.insh.deleteQueryShare)
			})
			r.Route("/dashboards", func(r chi.Router) {
				r.Get("/", d.insh.listDashboards)
				r.Post("/", d.insh.createDashboard)
				r.Get("/{id}", d.insh.getDashboard)
				r.Put("/{id}", d.insh.updateDashboard)
				r.Delete("/{id}", d.insh.deleteDashboard)
				r.Post("/{id}/share", d.insh.shareDashboard)
				r.Get("/{id}/shares", d.insh.listDashboardShares)
				r.Delete("/{id}/shares/{shareID}", d.insh.deleteDashboardShare)
				r.Post("/{id}/widgets", d.insh.upsertWidget)
				r.Delete("/{id}/widgets/{widgetID}", d.insh.deleteWidget)
				// Embed-token CRUD on a per-dashboard collection.
				// Auth-gated; the public unauth lookup lives at
				// /api/v1/insights/embed/{token} (mounted below).
				r.Get("/{id}/embeds", d.insembh.list)
				r.Post("/{id}/embeds", d.insembh.create)
				r.Post("/{id}/embeds/{embed_id}/revoke", d.insembh.revoke)
			})
			// External data sources (Phase L deferred). Connection
			// strings are encrypted at rest; the test endpoint
			// pings the remote with SELECT 1 so the UI can
			// distinguish a typo from a credential failure.
			r.Route("/data-sources", func(r chi.Router) {
				r.Get("/", d.insdsh.list)
				r.Post("/", d.insdsh.create)
				r.Put("/{id}", d.insdsh.update)
				r.Delete("/{id}", d.insdsh.delete)
				r.Post("/{id}/test", d.insdsh.test)
			})
		})

		// Public unauth dashboard embed endpoint. Mounted outside
		// the auth chain so anonymous viewers can fetch a
		// pre-rendered dashboard via a long-lived bearer token.
		//
		// Rate-limit MUST be IP-keyed here, not tenant-keyed: the
		// route runs before any tenant context is on the request
		// (the owning tenant is resolved from the embed token
		// inside insembh.public, not from a header or claim).
		// Mounting the tenant-scoped d.rateLimitMW would call
		// TenantFromContext → nil → 500 on every request — exactly
		// the bug the bot caught.
		//
		// The handler itself bills the owning tenant's quota
		// bucket once it resolves the token (see
		// insights_embed_handlers.go), so a viral embed cannot
		// starve other tenants from the IP-tier control alone.
		r.Route("/api/v1/insights/embed", func(r chi.Router) {
			r.Use(d.publicEmbedIPLimit)
			r.Get("/{token}", d.insembh.public)
		})

		// Phase I KPI dashboard aggregation. Reads only, so no idempotency
		// needed — quota + rate-limit keep it in bounds.
		r.Route("/api/v1/dashboard", func(r chi.Router) {
			d.tenantChain(r)
			r.Use(d.apiCallMW)
			r.Use(d.featureMW)
			r.Use(d.rateLimitMW)
			r.Use(platform.QuotaMiddleware(d.quotaEnforcer))
			r.Get("/summary", d.dashh.summary)
		})

		// Inventory surface (Phase D). Item + warehouse masters, the
		// append-only stock-move ledger, the stock_levels view, and the
		// valuation report. Mutations run under the same tenant +
		// idempotency + rate-limit + quota stack as finance because a
		// spammed move post can't starve other tenants or double-post a
		// source-record move under replay (the partial unique index on
		// inventory_moves handles that at the DB layer).
		r.Route("/api/v1/inventory", func(r chi.Router) {
			d.tenantChain(r)
			r.Use(d.apiCallMW)
			r.Use(d.featureMW)
			r.Use(d.authzMethodGate("inventory.read", "inventory.admin", ""))
			r.Use(platform.IdempotencyMiddleware(d.pool))
			r.Use(d.rateLimitMW)
			r.Use(platform.QuotaMiddleware(d.quotaEnforcer))
			r.Post("/items", d.invh.upsertItem)
			r.Get("/items", d.invh.listItems)
			r.Get("/items/{id}", d.invh.getItem)
			r.Post("/warehouses", d.invh.upsertWarehouse)
			r.Get("/warehouses", d.invh.listWarehouses)
			r.Post("/moves", d.invh.recordMove)
			r.Get("/moves", d.invh.listMoves)
			r.Post("/moves/{id}/reverse", d.invh.reverseMove)
			r.Post("/transfers", d.invh.recordTransfer)
			r.Get("/stock-levels", d.invh.listStockLevels)
			r.Get("/stock-levels/{id}", d.invh.stockLevelsByItem)
			r.Post("/batches", d.invh.createBatch)
			r.Get("/items/{id}/batches", d.invh.listBatchesByItem)
			r.Get("/reports/valuation", d.invh.valuation)
		})

		// Forms KApp. Creation and tenant-scoped lookups go through the
		// tenant middleware; public read + submit explicitly do NOT so
		// anonymous submissions work. The public submit route mounts
		// an IP-keyed token bucket and a honeypot check so the lack
		// of tenant context does not translate to "wide open".
		r.Route("/api/v1/forms", func(r chi.Router) {
			r.Group(func(r chi.Router) {
				d.tenantChain(r)
				r.Use(d.apiCallMW)
				r.Use(d.featureMW)
				r.Post("/", d.fh.create)
			})
			r.Get("/{id}", d.fh.public)
			// Public submit endpoint. There is no JWT or tenant
			// header to key on, so abuse is bounded with two
			// independent controls: an IP-keyed token bucket (10
			// requests/minute, shared across replicas via Redis
			// when available) and a honeypot check inside the
			// handler. Together they cut the most common drive-by
			// abuse vectors without breaking the public-form UX.
			r.Group(func(r chi.Router) {
				r.Use(d.publicFormIPLimit)
				r.Post("/{id}/submit", d.fh.submit)
			})
		})

		// Phase F file attachments. Uploads run under the full tenant +
		// idempotency + rate-limit + quota stack so a spammed upload cannot
		// starve other tenants; the object store dedups by SHA-256 so
		// rehosting the same source attachment across tenants costs one
		// physical blob.
		r.Route("/api/v1/files", func(r chi.Router) {
			d.tenantChain(r)
			r.Use(d.apiCallMW)
			r.Use(d.featureMW)
			r.Use(platform.IdempotencyMiddleware(d.pool))
			r.Use(d.rateLimitMW)
			r.Use(platform.QuotaMiddleware(d.quotaEnforcer))
			r.Post("/", d.fileh.upload)
			r.Get("/{id}", d.fileh.get)
			r.Get("/{id}/content", d.fileh.download)
		})

		// Phase F Base KApp — ad-hoc tables per tenant. Same middleware
		// stack as records: a tenant can't starve another via spammed
		// row inserts, and RLS stops cross-tenant row reads even if a
		// URL is forged.
		r.Route("/api/v1/base", func(r chi.Router) {
			d.tenantChain(r)
			r.Use(d.apiCallMW)
			r.Use(d.featureMW)
			r.Use(platform.IdempotencyMiddleware(d.pool))
			r.Use(d.rateLimitMW)
			r.Use(platform.QuotaMiddleware(d.quotaEnforcer))
			r.Get("/tables", d.bh.listTables)
			r.Post("/tables", d.bh.createTable)
			r.Get("/tables/{id}", d.bh.getTable)
			r.Patch("/tables/{id}", d.bh.updateTable)
			r.Get("/tables/{id}/rows", d.bh.listRows)
			r.Post("/tables/{id}/rows", d.bh.createRow)
			r.Patch("/tables/{id}/rows/{rowID}", d.bh.updateRow)
			r.Delete("/tables/{id}/rows/{rowID}", d.bh.deleteRow)
		})

		// Phase F Docs KApp — artifact documents with append-only version
		// history. SaveVersion and Restore each write a new history row
		// under tenant context; the immutable history table has no UPDATE
		// or DELETE policy so an audit replay always reproduces the edit
		// timeline.
		r.Route("/api/v1/docs", func(r chi.Router) {
			d.tenantChain(r)
			r.Use(d.apiCallMW)
			r.Use(d.featureMW)
			r.Use(platform.IdempotencyMiddleware(d.pool))
			r.Use(d.rateLimitMW)
			r.Use(platform.QuotaMiddleware(d.quotaEnforcer))
			r.Get("/", d.dh.list)
			r.Post("/", d.dh.create)
			r.Get("/{id}", d.dh.get)
			r.Post("/{id}/versions", d.dh.saveVersion)
			r.Get("/{id}/versions", d.dh.versions)
			r.Post("/{id}/restore", d.dh.restore)
		})

		// Phase H notifications inbox — durable in-app bell/inbox surface
		// backed by the notifications table. External transports (KChat,
		// webhook, email) are served by the worker; this endpoint backs
		// the web inbox regardless of transport success.
		notifStore := notifications.NewStore(d.pool)
		nh := newNotificationsHandlers(notifStore)
		r.Route("/api/v1/notifications", func(r chi.Router) {
			d.tenantChain(r)
			r.Use(d.apiCallMW)
			r.Use(d.featureMW)
			r.Use(d.rateLimitMW)
			r.Get("/", nh.list)
			r.Post("/{id}/read", nh.markRead)
			r.Post("/read-all", nh.markAllRead)
		})

		// Phase G saved views — per-user, per-KType filter/sort/column
		// layouts the RecordListPage persists so operators resume their
		// curated worklist across sessions. Mutations run under the same
		// idempotency + rate-limit + quota stack as record CRUD so a
		// spammed save cannot starve other tenants. RLS on saved_views
		// enforces tenant isolation; owner-only rules live in the store.
		r.Route("/api/v1/views", func(r chi.Router) {
			d.tenantChain(r)
			r.Use(d.apiCallMW)
			r.Use(d.featureMW)
			r.Use(platform.IdempotencyMiddleware(d.pool))
			r.Use(d.rateLimitMW)
			r.Use(platform.QuotaMiddleware(d.quotaEnforcer))
			r.Get("/", d.vh.list)
			r.Post("/", d.vh.create)
			r.Get("/{id}", d.vh.get)
			r.Patch("/{id}", d.vh.update)
			r.Delete("/{id}", d.vh.delete)
		})

		// OpenAPI machine-readable schema served for API consumers.
		r.Get("/api/v1/openapi.json", d.oh.serve)
	}) // end timeout-guarded group

	return r
}
