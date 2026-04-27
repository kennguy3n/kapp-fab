package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/kennguy3n/kapp-fab/internal/crm"
	"github.com/kennguy3n/kapp-fab/internal/finance"
	"github.com/kennguy3n/kapp-fab/internal/helpdesk"
	"github.com/kennguy3n/kapp-fab/internal/insights"
	"github.com/kennguy3n/kapp-fab/internal/inventory"
	"github.com/kennguy3n/kapp-fab/internal/ktype"
	"github.com/kennguy3n/kapp-fab/internal/ledger"
	"github.com/kennguy3n/kapp-fab/internal/lms"
	"github.com/kennguy3n/kapp-fab/internal/record"
	"github.com/kennguy3n/kapp-fab/internal/workflow"
)

// CommandRequest is the Phase A+B slash-command payload. KChat POSTs this
// envelope to /kchat/commands when a user invokes `/kapp ...`.
type CommandRequest struct {
	TenantID uuid.UUID `json:"tenant_id"`
	UserID   uuid.UUID `json:"user_id"`
	Command  string    `json:"command"`
	Args     []string  `json:"args"`
	// ThreadID is the KChat thread the command was issued in. The
	// /ticket-from-thread command stores it on the resulting
	// helpdesk.ticket so status changes can post back to the
	// same thread (services/worker/notifications.go).
	ThreadID string `json:"thread_id,omitempty"`
	// ThreadSubject is the (optional) parent message preview KChat
	// renders above a thread. /ticket-from-thread uses it as the
	// ticket's subject when the slash invocation has no args.
	ThreadSubject string `json:"thread_subject,omitempty"`
	// ThreadBody is the concatenated text of the thread the user
	// turned into a ticket. /ticket-from-thread copies it into the
	// ticket description so the agent has context without
	// re-opening the chat.
	ThreadBody string `json:"thread_body,omitempty"`
}

// CommandResponse is what KChat will render inline in the chat thread.
type CommandResponse struct {
	Text  string `json:"text"`
	Card  *Card  `json:"card,omitempty"`
	Error string `json:"error,omitempty"`
}

// CommandDispatcher routes slash commands to concrete handlers. Phase B
// extends the Phase A surface with record-creating and workflow-driving
// commands; the dispatcher is the single funnel through which every
// KChat user action reaches the platform services.
type CommandDispatcher struct {
	registry           *ktype.PGRegistry
	records            *record.PGStore
	workflow           *workflow.Engine
	approvals          *workflow.Engine
	ledger             *ledger.PGStore
	poster             *ledger.InvoicePoster
	inventory          *inventory.PGStore
	lmsIssuer          *lms.CertificateIssuer
	cards              *CardRenderer
	formsBase          string
	insightsQueries    *insights.QueryStore
	insightsDashboards *insights.DashboardStore
	insightsRunner     *insights.Runner
	// dashboardBase is the URL prefix the dashboard digest card links
	// to (e.g. https://app.example.com). Empty disables the deep link.
	dashboardBase string
}

// Dispatch runs the command and returns a response. Unknown commands
// are a user-facing condition — the response is still 200 so KChat can
// render the error inline, consistent with Slack/Teams conventions.
func (d *CommandDispatcher) Dispatch(ctx context.Context, req CommandRequest) (CommandResponse, error) {
	cmd := strings.ToLower(strings.TrimPrefix(req.Command, "/"))
	switch cmd {
	case "list-ktypes", "ktypes":
		return d.listKTypes(ctx)
	case "lead":
		return d.createRecord(ctx, req, crm.KTypeLead, leadFromArgs(req.Args, req.UserID))
	case "contact":
		return d.createRecord(ctx, req, crm.KTypeContact, contactFromArgs(req.Args, req.UserID))
	case "deal":
		data, err := dealFromArgs(req.Args, req.UserID)
		if err != nil {
			return CommandResponse{Text: fmt.Sprintf("/deal: %v", err)}, nil
		}
		return d.createRecord(ctx, req, crm.KTypeDeal, data)
	case "task":
		data, err := taskFromArgs(req.Args, req.UserID)
		if err != nil {
			return CommandResponse{Text: fmt.Sprintf("/task: %v", err)}, nil
		}
		return d.createRecord(ctx, req, crm.KTypeTask, data)
	case "approve":
		return d.decideApproval(ctx, req)
	case "invoice":
		data, err := invoiceFromArgs(req.Args, req.UserID)
		if err != nil {
			return CommandResponse{Text: fmt.Sprintf("/invoice: %v", err)}, nil
		}
		return d.createRecord(ctx, req, finance.KTypeARInvoice, data)
	case "bill":
		data, err := billFromArgs(req.Args, req.UserID)
		if err != nil {
			return CommandResponse{Text: fmt.Sprintf("/bill: %v", err)}, nil
		}
		return d.createRecord(ctx, req, finance.KTypeAPBill, data)
	case "customer":
		data, err := customerFromArgs(req.Args, req.UserID)
		if err != nil {
			return CommandResponse{Text: fmt.Sprintf("/customer: %v", err)}, nil
		}
		return d.createRecord(ctx, req, crm.KTypeCustomer, data)
	case "supplier":
		data, err := supplierFromArgs(req.Args, req.UserID)
		if err != nil {
			return CommandResponse{Text: fmt.Sprintf("/supplier: %v", err)}, nil
		}
		return d.createRecord(ctx, req, crm.KTypeSupplier, data)
	case "payment":
		data, err := paymentFromArgs(req.Args, req.UserID)
		if err != nil {
			return CommandResponse{Text: fmt.Sprintf("/payment: %v", err)}, nil
		}
		return d.createRecord(ctx, req, finance.KTypePayment, data)
	case "post-invoice":
		return d.postInvoice(ctx, req)
	case "post-bill":
		return d.postBill(ctx, req)
	case "stock":
		return d.stockLevels(ctx, req)
	case "reverse-stock-move":
		return d.reverseStockMove(ctx, req)
	case "batch":
		return d.assignBatch(ctx, req)
	case "certificate":
		return d.issueCertificate(ctx, req)
	case "learn":
		return d.learnCourses(ctx, req)
	case "form":
		return d.formLink(req)
	case "ticket":
		data, err := ticketFromArgs(req.Args, req.UserID)
		if err != nil {
			return CommandResponse{Text: fmt.Sprintf("/ticket: %v", err)}, nil
		}
		return d.createRecord(ctx, req, helpdesk.KTypeTicket, data)
	case "ticket-from-thread":
		data, err := ticketFromThread(req)
		if err != nil {
			return CommandResponse{Text: fmt.Sprintf("/ticket-from-thread: %v", err)}, nil
		}
		return d.createRecord(ctx, req, helpdesk.KTypeTicket, data)
	case "recurring-invoice":
		data, err := recurringInvoiceFromArgs(req.Args)
		if err != nil {
			return CommandResponse{Text: fmt.Sprintf("/recurring-invoice: %v", err)}, nil
		}
		return d.createRecord(ctx, req, finance.KTypeRecurringInvoice, data)
	case "insight":
		return d.runInsight(ctx, req)
	case "dashboard-digest":
		return d.dashboardDigest(ctx, req)
	case "help":
		return CommandResponse{
			Text: "Commands: /list-ktypes, /lead, /contact, /deal, /task, /customer, /supplier, /invoice, /bill, /payment, /post-invoice, /post-bill, /stock, /reverse-stock-move, /learn, /certificate, /approve, /ticket, /ticket-from-thread, /recurring-invoice, /form, /insight, /dashboard-digest, /help",
		}, nil
	default:
		return CommandResponse{
			Text: fmt.Sprintf("Unknown command: %s. Try /help.", req.Command),
		}, nil
	}
}

func (d *CommandDispatcher) listKTypes(ctx context.Context) (CommandResponse, error) {
	kts, err := d.registry.List(ctx)
	if err != nil {
		return CommandResponse{}, err
	}
	if len(kts) == 0 {
		return CommandResponse{Text: "No KTypes registered yet."}, nil
	}
	names := make([]string, 0, len(kts))
	for _, kt := range kts {
		names = append(names, fmt.Sprintf("%s@v%d", kt.Name, kt.Version))
	}
	return CommandResponse{Text: "Registered KTypes: " + strings.Join(names, ", ")}, nil
}

// createRecord is the common path for /lead, /contact, /deal, /task. It
// validates tenant + user context, creates the record, and renders the
// KType's card as the response. Record creation triggers the normal
// event + audit pipeline through record.PGStore.Create; no extra work
// is required here.
func (d *CommandDispatcher) createRecord(
	ctx context.Context,
	req CommandRequest,
	ktypeName string,
	data map[string]any,
) (CommandResponse, error) {
	if req.TenantID == uuid.Nil {
		return CommandResponse{Text: "tenant_id required"}, nil
	}
	if req.UserID == uuid.Nil {
		return CommandResponse{Text: "user_id required"}, nil
	}
	if d.records == nil {
		return CommandResponse{Text: "record store not configured"}, nil
	}
	dataJSON, err := json.Marshal(data)
	if err != nil {
		return CommandResponse{}, fmt.Errorf("marshal data: %w", err)
	}
	kt, err := d.registry.Get(ctx, ktypeName, 0)
	if err != nil {
		return CommandResponse{Text: fmt.Sprintf("unknown ktype %s — has your tenant been set up?", ktypeName)}, nil
	}
	created, err := d.records.Create(ctx, record.KRecord{
		TenantID:     req.TenantID,
		KType:        ktypeName,
		KTypeVersion: kt.Version,
		Data:         dataJSON,
		CreatedBy:    req.UserID,
	})
	if err != nil {
		var verrs ktype.ValidationErrors
		if errors.As(err, &verrs) {
			return CommandResponse{Text: fmt.Sprintf("%s validation failed: %v", ktypeName, verrs)}, nil
		}
		return CommandResponse{}, fmt.Errorf("create %s: %w", ktypeName, err)
	}
	var cardData map[string]any
	if err := json.Unmarshal(created.Data, &cardData); err != nil {
		return CommandResponse{Text: fmt.Sprintf("%s created: %s", ktypeName, created.ID)}, nil
	}
	card, err := d.cards.RenderCard(ctx, ktypeName, cardData)
	if err != nil {
		return CommandResponse{Text: fmt.Sprintf("%s created: %s", ktypeName, created.ID)}, nil
	}
	return CommandResponse{
		Text: fmt.Sprintf("Created %s %s", ktypeName, created.ID),
		Card: &card,
	}, nil
}

// decideApproval implements `/approve <id> [approve|reject]`. A missing
// decision defaults to approve to match the common case (an approver
// typing the command is almost always saying yes).
func (d *CommandDispatcher) decideApproval(ctx context.Context, req CommandRequest) (CommandResponse, error) {
	if d.approvals == nil {
		return CommandResponse{Text: "approvals engine not configured"}, nil
	}
	if len(req.Args) < 1 {
		return CommandResponse{Text: "Usage: /approve <approval_id> [approve|reject]"}, nil
	}
	approvalID, err := uuid.Parse(req.Args[0])
	if err != nil {
		return CommandResponse{Text: "invalid approval id"}, nil
	}
	decision := workflow.DecisionApprove
	if len(req.Args) >= 2 {
		switch strings.ToLower(req.Args[1]) {
		case "approve", "yes", "ok":
			decision = workflow.DecisionApprove
		case "reject", "no", "deny":
			decision = workflow.DecisionReject
		default:
			return CommandResponse{Text: "decision must be approve or reject"}, nil
		}
	}
	approval, err := d.approvals.Decide(ctx, req.TenantID, approvalID, decision, req.UserID)
	if err != nil {
		return CommandResponse{Text: fmt.Sprintf("/approve failed: %v", err)}, nil
	}
	return CommandResponse{
		Text: fmt.Sprintf("Approval %s recorded: %s (state=%s)",
			approval.ID, decision, approval.State),
	}, nil
}

// postInvoice implements `/post-invoice <invoice_id>`. The invoice must
// already exist as a draft (or pending_approval) finance.ar_invoice
// KRecord with the account codes populated — the poster will reject
// otherwise, surfacing the problem in chat rather than silently
// erroring.
func (d *CommandDispatcher) postInvoice(ctx context.Context, req CommandRequest) (CommandResponse, error) {
	if d.poster == nil {
		return CommandResponse{Text: "ledger not configured"}, nil
	}
	if req.TenantID == uuid.Nil || req.UserID == uuid.Nil {
		return CommandResponse{Text: "tenant_id and user_id required"}, nil
	}
	if len(req.Args) < 1 {
		return CommandResponse{Text: "Usage: /post-invoice <invoice_id>"}, nil
	}
	invoiceID, err := uuid.Parse(req.Args[0])
	if err != nil {
		return CommandResponse{Text: "invalid invoice id"}, nil
	}
	entry, err := d.poster.PostSalesInvoice(ctx, req.TenantID, invoiceID, req.UserID)
	if err != nil {
		return CommandResponse{Text: fmt.Sprintf("/post-invoice failed: %v", err)}, nil
	}
	return CommandResponse{
		Text: fmt.Sprintf("Posted invoice %s → journal entry %s", invoiceID, entry.ID),
	}, nil
}

// postBill mirrors postInvoice for finance.ap_bill.
func (d *CommandDispatcher) postBill(ctx context.Context, req CommandRequest) (CommandResponse, error) {
	if d.poster == nil {
		return CommandResponse{Text: "ledger not configured"}, nil
	}
	if req.TenantID == uuid.Nil || req.UserID == uuid.Nil {
		return CommandResponse{Text: "tenant_id and user_id required"}, nil
	}
	if len(req.Args) < 1 {
		return CommandResponse{Text: "Usage: /post-bill <bill_id>"}, nil
	}
	billID, err := uuid.Parse(req.Args[0])
	if err != nil {
		return CommandResponse{Text: "invalid bill id"}, nil
	}
	entry, err := d.poster.PostPurchaseBill(ctx, req.TenantID, billID, req.UserID)
	if err != nil {
		return CommandResponse{Text: fmt.Sprintf("/post-bill failed: %v", err)}, nil
	}
	return CommandResponse{
		Text: fmt.Sprintf("Posted bill %s → journal entry %s", billID, entry.ID),
	}, nil
}

// stockLevels implements `/stock [sku]`. Without arguments it returns a
// summary of every item's current stock; with a single SKU it returns
// the per-warehouse breakdown for that item. Quantities are fetched
// from the stock_levels view which is RLS-scoped to the caller's
// tenant.
func (d *CommandDispatcher) stockLevels(ctx context.Context, req CommandRequest) (CommandResponse, error) {
	if d.inventory == nil {
		return CommandResponse{Text: "inventory not configured"}, nil
	}
	if req.TenantID == uuid.Nil {
		return CommandResponse{Text: "tenant_id required"}, nil
	}
	var itemFilter *uuid.UUID
	var sku string
	if len(req.Args) >= 1 && req.Args[0] != "" {
		sku = req.Args[0]
		it, err := d.inventory.GetItemBySKU(ctx, req.TenantID, sku)
		if err != nil {
			if errors.Is(err, inventory.ErrItemNotFound) {
				return CommandResponse{Text: fmt.Sprintf("/stock: no item with sku %q", sku)}, nil
			}
			return CommandResponse{}, err
		}
		id := it.ID
		itemFilter = &id
	}
	levels, err := d.inventory.ListStockLevels(ctx, req.TenantID, itemFilter)
	if err != nil {
		return CommandResponse{}, err
	}
	if len(levels) == 0 {
		if sku != "" {
			return CommandResponse{Text: fmt.Sprintf("/stock: %s — no moves recorded", sku)}, nil
		}
		return CommandResponse{Text: "/stock: no stock recorded for this tenant"}, nil
	}
	lines := make([]string, 0, len(levels))
	for _, l := range levels {
		lines = append(lines, fmt.Sprintf("%s @ %s: %s", l.ItemID, l.WarehouseID, l.Qty.String()))
	}
	title := "Stock levels"
	if sku != "" {
		title = fmt.Sprintf("Stock for %s", sku)
	}
	return CommandResponse{
		Text: title + "\n" + strings.Join(lines, "\n"),
	}, nil
}

// reverseStockMove implements `/reverse-stock-move <move_id> [memo]`.
// Posts a contra-entry that cancels the named inventory move.
// Reverses are confirmation-required actions when invoked by the
// agent tool; KChat slash commands are user-initiated so the
// equivalent confirmation is the explicit /reverse-stock-move
// invocation. Errors from the store are surfaced inline so the
// operator can retry with the right id / role.
func (d *CommandDispatcher) reverseStockMove(ctx context.Context, req CommandRequest) (CommandResponse, error) {
	if d.inventory == nil {
		return CommandResponse{Text: "inventory not configured"}, nil
	}
	if req.TenantID == uuid.Nil {
		return CommandResponse{Text: "tenant_id required"}, nil
	}
	if len(req.Args) < 1 {
		return CommandResponse{Text: "/reverse-stock-move <move_id> [memo]"}, nil
	}
	moveID, err := strconv.ParseInt(req.Args[0], 10, 64)
	if err != nil || moveID <= 0 {
		return CommandResponse{Text: fmt.Sprintf("/reverse-stock-move: invalid move_id %q", req.Args[0])}, nil
	}
	memo := ""
	if len(req.Args) > 1 {
		memo = strings.Join(req.Args[1:], " ")
	}
	move, err := d.inventory.ReverseMove(ctx, req.TenantID, moveID, req.UserID, memo)
	if err != nil {
		switch {
		case errors.Is(err, inventory.ErrMoveNotFound):
			return CommandResponse{Text: fmt.Sprintf("/reverse-stock-move: move %d not found", moveID)}, nil
		case errors.Is(err, inventory.ErrAlreadyReversed):
			return CommandResponse{Text: fmt.Sprintf("/reverse-stock-move: move %d already reversed", moveID)}, nil
		case errors.Is(err, inventory.ErrCannotReverseContra):
			return CommandResponse{Text: fmt.Sprintf("/reverse-stock-move: %d is itself a contra-entry — reverse the original instead", moveID)}, nil
		}
		return CommandResponse{}, err
	}
	return CommandResponse{
		Text: fmt.Sprintf("Reversed stock move %d → contra-entry %d (qty=%s)", moveID, move.ID, move.Qty.String()),
	}, nil
}

// parseSlashDate parses a YYYY-MM-DD date supplied to a slash command
// (used by /batch and other commands that accept date-only inputs).
func parseSlashDate(s string) (time.Time, error) {
	return time.Parse("2006-01-02", s)
}

// assignBatch implements `/batch <sku> <batch_no> [expires_at]`.
// Creates an inventory_batches row tied to the supplied item. Returns
// the new batch id so a follow-up /move command can reference it via
// the agent tool. Re-issuing the same batch_no surfaces the unique
// violation as a friendly inline error.
func (d *CommandDispatcher) assignBatch(ctx context.Context, req CommandRequest) (CommandResponse, error) {
	if d.inventory == nil {
		return CommandResponse{Text: "inventory not configured"}, nil
	}
	if req.TenantID == uuid.Nil {
		return CommandResponse{Text: "tenant_id required"}, nil
	}
	if len(req.Args) < 2 {
		return CommandResponse{Text: "/batch <sku> <batch_no> [expires_at YYYY-MM-DD]"}, nil
	}
	sku := req.Args[0]
	batchNo := req.Args[1]
	it, err := d.inventory.GetItemBySKU(ctx, req.TenantID, sku)
	if err != nil {
		if errors.Is(err, inventory.ErrItemNotFound) {
			return CommandResponse{Text: fmt.Sprintf("/batch: no item with sku %q", sku)}, nil
		}
		return CommandResponse{}, err
	}
	b := inventory.Batch{
		TenantID:  req.TenantID,
		ItemID:    it.ID,
		BatchNo:   batchNo,
		CreatedBy: req.UserID,
	}
	if len(req.Args) >= 3 {
		ts, perr := parseSlashDate(req.Args[2])
		if perr != nil {
			return CommandResponse{Text: fmt.Sprintf("/batch: invalid expires_at %q (want YYYY-MM-DD)", req.Args[2])}, nil
		}
		b.ExpiresAt = &ts
	}
	out, err := d.inventory.CreateBatch(ctx, b)
	if err != nil {
		if errors.Is(err, inventory.ErrItemNotFound) {
			return CommandResponse{Text: fmt.Sprintf("/batch: no item with sku %q", sku)}, nil
		}
		if errors.Is(err, inventory.ErrDuplicateBatch) {
			return CommandResponse{Text: fmt.Sprintf("/batch: batch %q already exists for %s", batchNo, sku)}, nil
		}
		return CommandResponse{}, err
	}
	return CommandResponse{
		Text: fmt.Sprintf("Batch %s created for %s (batch id %s)", out.BatchNo, sku, out.ID),
	}, nil
}

// issueCertificate implements `/certificate <enrollment_id>`. Issues
// (or re-fetches the existing) lms.certificate KRecord for a
// completed enrollment.
func (d *CommandDispatcher) issueCertificate(ctx context.Context, req CommandRequest) (CommandResponse, error) {
	if d.lmsIssuer == nil {
		return CommandResponse{Text: "lms certificate issuer not configured"}, nil
	}
	if req.TenantID == uuid.Nil || req.UserID == uuid.Nil {
		return CommandResponse{Text: "tenant_id and user_id required"}, nil
	}
	if len(req.Args) < 1 {
		return CommandResponse{Text: "/certificate <enrollment_id>"}, nil
	}
	enrollmentID, err := uuid.Parse(req.Args[0])
	if err != nil {
		return CommandResponse{Text: fmt.Sprintf("/certificate: invalid enrollment_id %q", req.Args[0])}, nil
	}
	cert, err := d.lmsIssuer.IssueCertificate(ctx, req.TenantID, enrollmentID, req.UserID, lms.CertificateOptions{})
	if err != nil && !errors.Is(err, lms.ErrCertificateAlreadyIssued) {
		switch {
		case errors.Is(err, lms.ErrEnrollmentNotFound):
			return CommandResponse{Text: fmt.Sprintf("/certificate: enrollment %s not found", enrollmentID)}, nil
		case errors.Is(err, lms.ErrEnrollmentNotComplete):
			return CommandResponse{Text: fmt.Sprintf("/certificate: enrollment %s is not yet completed", enrollmentID)}, nil
		}
		return CommandResponse{}, err
	}
	// IssueCertificate can return (nil, ErrCertificateAlreadyIssued)
	// when the 23505 race loses and findExisting can't locate the
	// sibling row — surface a human-readable message rather than
	// dereferencing a nil *KRecord.
	if cert == nil {
		return CommandResponse{Text: fmt.Sprintf("/certificate: enrollment %s already has a certificate (lookup of existing row failed)", enrollmentID)}, nil
	}
	prefix := "Issued"
	if errors.Is(err, lms.ErrCertificateAlreadyIssued) {
		prefix = "Already issued"
	}
	return CommandResponse{
		Text: fmt.Sprintf("%s certificate %s for enrollment %s", prefix, cert.ID, enrollmentID),
	}, nil
}

// learnCourses implements `/learn [keyword]`. Without arguments it
// returns the first page of published courses; with a keyword it
// filters by substring in the course title. The data-layer query
// stays cheap by relying on the generic KRecord list endpoint since
// courses are just KRecords of ktype=lms.course.
func (d *CommandDispatcher) learnCourses(ctx context.Context, req CommandRequest) (CommandResponse, error) {
	if d.records == nil {
		return CommandResponse{Text: "lms not configured"}, nil
	}
	if req.TenantID == uuid.Nil {
		return CommandResponse{Text: "tenant_id required"}, nil
	}
	filter := record.ListFilter{KType: "lms.course", Limit: 10}
	recs, err := d.records.List(ctx, req.TenantID, filter)
	if err != nil {
		return CommandResponse{}, err
	}
	keyword := ""
	if len(req.Args) >= 1 {
		keyword = strings.ToLower(req.Args[0])
	}
	lines := make([]string, 0, len(recs))
	for _, r := range recs {
		var body struct {
			Title  string `json:"title"`
			Status string `json:"status"`
		}
		_ = json.Unmarshal(r.Data, &body)
		if keyword != "" && !strings.Contains(strings.ToLower(body.Title), keyword) {
			continue
		}
		lines = append(lines, fmt.Sprintf("%s — %s (%s)", r.ID, body.Title, body.Status))
	}
	if len(lines) == 0 {
		return CommandResponse{Text: "/learn: no matching courses"}, nil
	}
	return CommandResponse{Text: "Courses\n" + strings.Join(lines, "\n")}, nil
}

// formLink returns a deep link the user can share to collect records via
// the public form KApp.
func (d *CommandDispatcher) formLink(req CommandRequest) (CommandResponse, error) {
	if len(req.Args) < 1 {
		return CommandResponse{Text: "Usage: /form <ktype>"}, nil
	}
	ktypeName := req.Args[0]
	base := strings.TrimRight(d.formsBase, "/")
	if base == "" {
		base = "/forms"
	}
	return CommandResponse{
		Text: fmt.Sprintf("Share this link to collect %s: %s/new/%s?tenant=%s",
			ktypeName, base, ktypeName, req.TenantID),
	}, nil
}

// ---- argument parsers ----------------------------------------------------

// Argument parsing for Phase B slash commands is deliberately simple:
// the first token becomes the primary field (name/title/subject) and
// trailing tokens with known prefixes are pulled out. Richer parsing
// lives in composer actions where the full message body is available.

func leadFromArgs(args []string, owner uuid.UUID) map[string]any {
	name := strings.Join(args, " ")
	return map[string]any{
		"name":   name,
		"status": "new",
		"owner":  owner.String(),
	}
}

func contactFromArgs(args []string, owner uuid.UUID) map[string]any {
	name := strings.Join(args, " ")
	return map[string]any{
		"name":  name,
		"owner": owner.String(),
	}
}

// dealFromArgs expects `[name...] [amount]` — the trailing numeric token
// is parsed as amount if present.
func dealFromArgs(args []string, owner uuid.UUID) (map[string]any, error) {
	if len(args) == 0 {
		return nil, errors.New("usage: /deal <name> [amount]")
	}
	name := args
	amount := 0.0
	if last := args[len(args)-1]; last != "" {
		if v, err := strconv.ParseFloat(last, 64); err == nil {
			amount = v
			name = args[:len(args)-1]
		}
	}
	if len(name) == 0 {
		return nil, errors.New("deal name required")
	}
	data := map[string]any{
		"name":     strings.Join(name, " "),
		"stage":    "qualification",
		"currency": "USD",
		"owner":    owner.String(),
	}
	if amount > 0 {
		data["amount"] = amount
	}
	return data, nil
}

// taskFromArgs expects `[title...] @[assignee_id]`. KChat resolves
// @mentions to UUIDs before dispatch, so the token arrives as
// `@<uuid>`. Falls back to self-assignment.
func taskFromArgs(args []string, requester uuid.UUID) (map[string]any, error) {
	if len(args) == 0 {
		return nil, errors.New("usage: /task <title> [@assignee]")
	}
	titleParts := args
	assignee := requester
	if last := args[len(args)-1]; strings.HasPrefix(last, "@") {
		if id, err := uuid.Parse(strings.TrimPrefix(last, "@")); err == nil {
			assignee = id
			titleParts = args[:len(args)-1]
		}
	}
	title := strings.Join(titleParts, " ")
	if title == "" {
		return nil, errors.New("task title required")
	}
	return map[string]any{
		"title":    title,
		"status":   "open",
		"assignee": assignee.String(),
	}, nil
}

// invoiceFromArgs expects `<customer_id> <total> [currency] [invoice_number]`.
// The resulting record is a draft finance.ar_invoice — account codes
// aren't required until the invoice is posted, so this keeps the
// quick-create path to two mandatory arguments.
func invoiceFromArgs(args []string, owner uuid.UUID) (map[string]any, error) {
	if len(args) < 2 {
		return nil, errors.New("usage: /invoice <customer_id> <total> [currency] [number]")
	}
	customer, err := uuid.Parse(args[0])
	if err != nil {
		return nil, fmt.Errorf("invalid customer id: %w", err)
	}
	total, err := strconv.ParseFloat(args[1], 64)
	if err != nil {
		return nil, fmt.Errorf("invalid total: %w", err)
	}
	currency := "USD"
	if len(args) >= 3 && args[2] != "" {
		currency = strings.ToUpper(args[2])
	}
	data := map[string]any{
		"customer_id": customer.String(),
		"subtotal":    total,
		"total":       total,
		"currency":    currency,
		"status":      "draft",
		"owner":       owner.String(),
	}
	if len(args) >= 4 && args[3] != "" {
		data["invoice_number"] = args[3]
	}
	return data, nil
}

// customerFromArgs expects `<name...> [currency] [credit_limit]`. Trailing
// tokens are opportunistically parsed: a 3-letter upper-case string is
// treated as ISO-4217 currency; a numeric token becomes credit_limit.
func customerFromArgs(args []string, owner uuid.UUID) (map[string]any, error) {
	if len(args) == 0 {
		return nil, errors.New("usage: /customer <name> [currency] [credit_limit]")
	}
	nameParts := args
	currency := ""
	creditLimit := 0.0
	for len(nameParts) > 0 {
		last := nameParts[len(nameParts)-1]
		if v, err := strconv.ParseFloat(last, 64); err == nil && creditLimit == 0 {
			creditLimit = v
			nameParts = nameParts[:len(nameParts)-1]
			continue
		}
		if len(last) == 3 && strings.ToUpper(last) == last && currency == "" {
			currency = last
			nameParts = nameParts[:len(nameParts)-1]
			continue
		}
		break
	}
	if len(nameParts) == 0 {
		return nil, errors.New("customer name required")
	}
	if currency == "" {
		currency = "USD"
	}
	data := map[string]any{
		"name":            strings.Join(nameParts, " "),
		"currency":        currency,
		"status":          "active",
		"ar_aging_bucket": "current",
		"owner":           owner.String(),
	}
	if creditLimit > 0 {
		data["credit_limit"] = creditLimit
	}
	return data, nil
}

// supplierFromArgs mirrors customerFromArgs without credit_limit.
func supplierFromArgs(args []string, owner uuid.UUID) (map[string]any, error) {
	if len(args) == 0 {
		return nil, errors.New("usage: /supplier <name> [currency]")
	}
	nameParts := args
	currency := ""
	if last := args[len(args)-1]; len(last) == 3 && strings.ToUpper(last) == last {
		currency = last
		nameParts = args[:len(args)-1]
	}
	if len(nameParts) == 0 {
		return nil, errors.New("supplier name required")
	}
	if currency == "" {
		currency = "USD"
	}
	return map[string]any{
		"name":            strings.Join(nameParts, " "),
		"currency":        currency,
		"status":          "active",
		"ap_aging_bucket": "current",
		"owner":           owner.String(),
	}, nil
}

// paymentFromArgs parses /payment <receive|pay> <party_id> <amount> [currency] [reference].
// Optional allocation flags (invoice=id:amount) are accepted for
// multi-invoice settlement; the slash command records the draft and
// leaves posting to an explicit /post-payment follow-up.
func paymentFromArgs(args []string, owner uuid.UUID) (map[string]any, error) {
	if len(args) < 3 {
		return nil, errors.New("usage: /payment <receive|pay> <party_id> <amount> [currency] [reference]")
	}
	paymentType := strings.ToLower(args[0])
	if paymentType != "receive" && paymentType != "pay" {
		return nil, errors.New("payment_type must be 'receive' or 'pay'")
	}
	partyID := args[1]
	amount, err := strconv.ParseFloat(args[2], 64)
	if err != nil || amount <= 0 {
		return nil, fmt.Errorf("invalid amount: %s", args[2])
	}
	currency := "USD"
	reference := ""
	for _, a := range args[3:] {
		if len(a) == 3 && strings.ToUpper(a) == a {
			currency = a
			continue
		}
		reference = a
	}
	partyType := "customer"
	if paymentType == "pay" {
		partyType = "supplier"
	}
	return map[string]any{
		"payment_type": paymentType,
		"party_type":   partyType,
		"party_id":     partyID,
		"amount":       amount,
		"currency":     currency,
		"reference":    reference,
		"status":       "draft",
		"owner":        owner.String(),
	}, nil
}

// billFromArgs mirrors invoiceFromArgs for finance.ap_bill.
func billFromArgs(args []string, owner uuid.UUID) (map[string]any, error) {
	if len(args) < 2 {
		return nil, errors.New("usage: /bill <supplier_id> <total> [currency] [number]")
	}
	supplier, err := uuid.Parse(args[0])
	if err != nil {
		return nil, fmt.Errorf("invalid supplier id: %w", err)
	}
	total, err := strconv.ParseFloat(args[1], 64)
	if err != nil {
		return nil, fmt.Errorf("invalid total: %w", err)
	}
	currency := "USD"
	if len(args) >= 3 && args[2] != "" {
		currency = strings.ToUpper(args[2])
	}
	data := map[string]any{
		"supplier_id": supplier.String(),
		"subtotal":    total,
		"total":       total,
		"currency":    currency,
		"status":      "draft",
		"owner":       owner.String(),
	}
	if len(args) >= 4 && args[3] != "" {
		data["bill_number"] = args[3]
	}
	return data, nil
}

// ticketFromArgs parses `/ticket <subject...> [priority=high] [channel=chat]
// [customer=<uuid>]`. The subject is the concatenation of every token
// that is not a key=value pair so operators can just type what they
// mean. Priority defaults to `medium`, channel defaults to `chat` so
// the ticket slots straight into the SLA policy grid.
func ticketFromArgs(args []string, owner uuid.UUID) (map[string]any, error) {
	if len(args) == 0 {
		return nil, errors.New("usage: /ticket <subject...> [priority=low|medium|high|urgent] [channel=chat|email|portal|phone] [customer=<uuid>]")
	}
	data := map[string]any{
		"status":   "open",
		"priority": "medium",
		"channel":  "chat",
		"owner":    owner.String(),
	}
	subject := make([]string, 0, len(args))
	for _, tok := range args {
		if idx := strings.Index(tok, "="); idx > 0 {
			key := strings.ToLower(tok[:idx])
			val := tok[idx+1:]
			switch key {
			case "priority":
				data["priority"] = strings.ToLower(val)
				continue
			case "channel":
				data["channel"] = strings.ToLower(val)
				continue
			case "customer", "customer_id":
				if _, err := uuid.Parse(val); err != nil {
					return nil, fmt.Errorf("invalid customer id: %w", err)
				}
				data["customer_id"] = val
				continue
			case "assigned":
				if _, err := uuid.Parse(val); err != nil {
					return nil, fmt.Errorf("invalid assignee id: %w", err)
				}
				data["assigned_to"] = val
				continue
			}
		}
		subject = append(subject, tok)
	}
	if len(subject) == 0 {
		return nil, errors.New("ticket subject required")
	}
	data["subject"] = strings.Join(subject, " ")
	return data, nil
}

// ticketFromThread builds a helpdesk.ticket record from the KChat
// thread context the bridge attached to the slash invocation. The
// resulting ticket carries `thread_id` so the worker's notification
// router can post status updates back to the same thread.
//
// Subject defaults to ThreadSubject; the slash command's args (if
// any) override it so an agent can pin a clearer title at submit
// time. Description is the thread body verbatim.
func ticketFromThread(req CommandRequest) (map[string]any, error) {
	if req.ThreadID == "" {
		return nil, errors.New("thread_id missing — invoke /ticket-from-thread inside a thread")
	}
	subject := strings.TrimSpace(strings.Join(req.Args, " "))
	if subject == "" {
		subject = strings.TrimSpace(req.ThreadSubject)
	}
	if subject == "" {
		return nil, errors.New("ticket subject required (pass as args or set thread_subject)")
	}
	data := map[string]any{
		"subject":     subject,
		"description": req.ThreadBody,
		"status":      "open",
		"priority":    "medium",
		"channel":     "chat",
		"owner":       req.UserID.String(),
		"thread_id":   req.ThreadID,
	}
	return data, nil
}

// recurringInvoiceFromArgs parses
//
//	/recurring-invoice <name> <template_invoice_id> <frequency> <start_date> [end=YYYY-MM-DD] [auto_post=true]
//
// `name` is a single token (use underscores or hyphens for spaces);
// frequency is one of daily|weekly|monthly|quarterly|yearly. The
// resulting record drives the recurring engine — start_date doubles
// as next_generation_date so the very next sweeper tick fires the
// first run.
func recurringInvoiceFromArgs(args []string) (map[string]any, error) {
	if len(args) < 4 {
		return nil, errors.New("usage: /recurring-invoice <name> <template_invoice_id> <frequency> <start_date> [end=YYYY-MM-DD] [auto_post=true]")
	}
	name := args[0]
	template, err := uuid.Parse(args[1])
	if err != nil {
		return nil, fmt.Errorf("invalid template_invoice_id: %w", err)
	}
	freq := strings.ToLower(args[2])
	switch freq {
	case finance.FrequencyDaily, finance.FrequencyWeekly, finance.FrequencyMonthly,
		finance.FrequencyQuarterly, finance.FrequencyYearly:
	default:
		return nil, fmt.Errorf("invalid frequency %q", freq)
	}
	start := args[3]
	data := map[string]any{
		"name":                 name,
		"template_invoice_id":  template.String(),
		"frequency":            freq,
		"start_date":           start,
		"next_generation_date": start,
		"auto_post":            false,
		"status":               finance.RecurringStatusActive,
	}
	for _, tok := range args[4:] {
		idx := strings.Index(tok, "=")
		if idx < 1 {
			continue
		}
		key := strings.ToLower(tok[:idx])
		val := tok[idx+1:]
		switch key {
		case "end", "end_date":
			data["end_date"] = val
		case "auto_post":
			data["auto_post"] = strings.EqualFold(val, "true")
		}
	}
	return data, nil
}

// ---------- Phase L Insights ----------

// runInsight runs a saved insights query by name and returns the
// result as an inline card. Usage: `/insight <query-name>`. Spaces in
// the query name are joined back from the args slice. Falls back to a
// helpful error response when the dispatcher was constructed without
// the insights wiring (e.g. older deployment), instead of panicking.
func (d *CommandDispatcher) runInsight(ctx context.Context, req CommandRequest) (CommandResponse, error) {
	if d.insightsQueries == nil || d.insightsRunner == nil {
		return CommandResponse{Text: "/insight: insights surface not configured for this deployment"}, nil
	}
	if req.TenantID == uuid.Nil {
		return CommandResponse{Text: "/insight: tenant context required"}, nil
	}
	if len(req.Args) == 0 {
		return CommandResponse{Text: "/insight: usage: /insight <query name>"}, nil
	}
	name := strings.TrimSpace(strings.Join(req.Args, " "))
	queries, err := d.insightsQueries.List(ctx, req.TenantID)
	if err != nil {
		return CommandResponse{}, err
	}
	var match *insights.Query
	for i := range queries {
		if strings.EqualFold(queries[i].Name, name) {
			match = &queries[i]
			break
		}
	}
	if match == nil {
		return CommandResponse{Text: fmt.Sprintf("/insight: no saved query named %q", name)}, nil
	}
	out, err := d.insightsRunner.RunSaved(ctx, req.TenantID, match.ID, nil, false)
	if err != nil {
		return CommandResponse{Text: fmt.Sprintf("/insight: %v", err)}, nil
	}
	card := renderInsightCard(match, out)
	var rowCount int
	if out != nil && out.Result != nil {
		rowCount = len(out.Result.Rows)
	}
	return CommandResponse{
		Text: fmt.Sprintf("Insights — %s (%d rows)", match.Name, rowCount),
		Card: &card,
	}, nil
}

// dashboardDigest renders a multi-section card that summarises every
// widget on a dashboard by name. Each section gets one CardField whose
// value is a short text summary of the widget's latest result.
// Usage: `/dashboard-digest <dashboard-name>`.
func (d *CommandDispatcher) dashboardDigest(ctx context.Context, req CommandRequest) (CommandResponse, error) {
	if d.insightsDashboards == nil || d.insightsRunner == nil {
		return CommandResponse{Text: "/dashboard-digest: insights surface not configured for this deployment"}, nil
	}
	if req.TenantID == uuid.Nil {
		return CommandResponse{Text: "/dashboard-digest: tenant context required"}, nil
	}
	if len(req.Args) == 0 {
		return CommandResponse{Text: "/dashboard-digest: usage: /dashboard-digest <dashboard name>"}, nil
	}
	name := strings.TrimSpace(strings.Join(req.Args, " "))
	dashboards, err := d.insightsDashboards.List(ctx, req.TenantID)
	if err != nil {
		return CommandResponse{}, err
	}
	var match *insights.Dashboard
	for i := range dashboards {
		if strings.EqualFold(dashboards[i].Name, name) {
			match = &dashboards[i]
			break
		}
	}
	if match == nil {
		return CommandResponse{Text: fmt.Sprintf("/dashboard-digest: no dashboard named %q", name)}, nil
	}
	widgets, err := d.insightsDashboards.ListWidgets(ctx, req.TenantID, match.ID)
	if err != nil {
		return CommandResponse{}, err
	}

	card := Card{
		Title:    fmt.Sprintf("Dashboard digest — %s", match.Name),
		Subtitle: fmt.Sprintf("%d widget(s)", len(widgets)),
	}
	if d.dashboardBase != "" {
		card.Actions = append(card.Actions, CardLink{
			Label: "Open dashboard",
			URL:   fmt.Sprintf("%s/insights/dashboards#%s", strings.TrimRight(d.dashboardBase, "/"), match.ID),
		})
	}
	for _, w := range widgets {
		label := w.VizType
		if w.QueryID == nil {
			card.Fields = append(card.Fields, CardKV{
				Label: label,
				Value: "(no saved query bound)",
			})
			continue
		}
		out, err := d.insightsRunner.RunSaved(ctx, req.TenantID, *w.QueryID, nil, false)
		if err != nil || out == nil || out.Result == nil {
			card.Fields = append(card.Fields, CardKV{
				Label: label,
				Value: "(unable to run widget)",
			})
			continue
		}
		card.Fields = append(card.Fields, CardKV{
			Label: label,
			Value: shortInsightSummary(out),
		})
	}
	return CommandResponse{
		Text: fmt.Sprintf("Dashboard digest — %s", match.Name),
		Card: &card,
	}, nil
}

// renderInsightCard turns a single query run into a Card. Tables get
// a column-list body with the first ~5 rows; aggregations / number
// cards collapse to a key=value field list. Stays well under typical
// chat-card size limits.
func renderInsightCard(q *insights.Query, out *insights.RunResult) Card {
	card := Card{
		Title:    fmt.Sprintf("Insights — %s", q.Name),
		Subtitle: q.Definition.Source,
	}
	if out == nil || out.Result == nil {
		card.Body = "(no result)"
		return card
	}
	rows := out.Result.Rows
	cols := out.Result.Columns
	if len(rows) == 0 {
		card.Body = "0 rows"
		return card
	}
	if len(rows) == 1 {
		first := rows[0]
		for _, c := range cols {
			card.Fields = append(card.Fields, CardKV{
				Label: c,
				Value: fmt.Sprintf("%v", first[c]),
			})
		}
		return card
	}
	limit := len(rows)
	if limit > 5 {
		limit = 5
	}
	var lines []string
	lines = append(lines, strings.Join(cols, " | "))
	for i := 0; i < limit; i++ {
		row := rows[i]
		vals := make([]string, 0, len(cols))
		for _, c := range cols {
			vals = append(vals, fmt.Sprintf("%v", row[c]))
		}
		lines = append(lines, strings.Join(vals, " | "))
	}
	if len(rows) > limit {
		lines = append(lines, fmt.Sprintf("…and %d more rows", len(rows)-limit))
	}
	card.Body = strings.Join(lines, "\n")
	return card
}

func shortInsightSummary(out *insights.RunResult) string {
	if out == nil || out.Result == nil {
		return "(no result)"
	}
	rows := out.Result.Rows
	if len(rows) == 0 {
		return "0 rows"
	}
	if len(rows) == 1 {
		first := rows[0]
		pairs := make([]string, 0, len(first))
		for _, c := range out.Result.Columns {
			pairs = append(pairs, fmt.Sprintf("%s=%v", c, first[c]))
		}
		return strings.Join(pairs, ", ")
	}
	return fmt.Sprintf("%d rows", len(rows))
}
