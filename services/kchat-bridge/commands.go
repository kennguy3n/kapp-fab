package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/kennguy3n/kapp-fab/internal/crm"
	"github.com/kennguy3n/kapp-fab/internal/finance"
	"github.com/kennguy3n/kapp-fab/internal/helpdesk"
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
	registry  *ktype.PGRegistry
	records   *record.PGStore
	workflow  *workflow.Engine
	approvals *workflow.Engine
	ledger    *ledger.PGStore
	poster    *ledger.InvoicePoster
	inventory *inventory.PGStore
	lmsIssuer *lms.CertificateIssuer
	cards     *CardRenderer
	formsBase string
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
	case "help":
		return CommandResponse{
			Text: "Commands: /list-ktypes, /lead, /contact, /deal, /task, /customer, /supplier, /invoice, /bill, /payment, /post-invoice, /post-bill, /stock, /reverse-stock-move, /learn, /certificate, /approve, /ticket, /ticket-from-thread, /recurring-invoice, /form, /help",
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

// issueCertificate implements `/certificate <enrollment_id>`. Issues
// (or re-fetches the existing) lms.certificate KRecord for a
// completed enrollment.
func (d *CommandDispatcher) issueCertificate(ctx context.Context, req CommandRequest) (CommandResponse, error) {
	if d.lmsIssuer == nil {
		return CommandResponse{Text: "lms certificate issuer not configured"}, nil
	}
	if req.TenantID == uuid.Nil {
		return CommandResponse{Text: "tenant_id required"}, nil
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
