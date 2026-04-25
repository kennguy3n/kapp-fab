package main

import (
	"bytes"
	"context"
	"encoding/csv"
	"fmt"
	"html"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/kennguy3n/kapp-fab/internal/notifications"
	"github.com/kennguy3n/kapp-fab/internal/reporting"
	"github.com/kennguy3n/kapp-fab/internal/scheduler"
)

// reportPDFConverter is the minimal contract the report scheduler
// needs to render the HTML rows into a PDF for email delivery.
// Wired to the same internal/print HTMLToPDF used by the per-record
// print pipeline so a single binary covers both flows; tests can
// supply an in-memory passthrough.
type reportPDFConverter interface {
	Convert(ctx context.Context, html []byte) ([]byte, error)
}

// reportEmailSender is the SMTP slice the scheduler depends on.
// Implemented in production by *notifications.SMTPAdapter; nil-safe
// callers must check ErrSMTPDisabled.
type reportEmailSender interface {
	Send(ctx context.Context, to []string, subject, body string) error
}

// reportEmailAttachmentSender is an optional capability surfaced by
// SMTP adapters that can carry attachments. If the wired sender
// implements it the scheduler attaches the CSV/PDF; otherwise it
// inlines a download summary in the body and logs the omission.
type reportEmailAttachmentSender interface {
	SendWithAttachment(ctx context.Context, to []string, subject, body, filename, mime string, content []byte) error
}

// ReportScheduleHandler iterates the report_schedules table per
// tenant tick, runs every due schedule's saved report, renders the
// result as CSV or PDF, and emails the recipients. Mirrors the
// recurring-invoice handler shape so the scheduler dispatch path
// stays uniform.
type ReportScheduleHandler struct {
	scheduleStore *reporting.ScheduleStore
	reportStore   *reporting.Store
	runner        *reporting.Runner
	pdfConverter  reportPDFConverter
	email         reportEmailSender
	now           func() time.Time
}

// NewReportScheduleHandler wires the handler from the cooperating
// stores. The pdfConverter is optional — when nil, PDF schedules
// are downgraded to inline-HTML in the email body so a tenant
// without wkhtmltopdf still gets the report (just without the
// nicer rendering).
func NewReportScheduleHandler(
	scheduleStore *reporting.ScheduleStore,
	reportStore *reporting.Store,
	runner *reporting.Runner,
	pdfConverter reportPDFConverter,
	email reportEmailSender,
) *ReportScheduleHandler {
	return &ReportScheduleHandler{
		scheduleStore: scheduleStore,
		reportStore:   reportStore,
		runner:        runner,
		pdfConverter:  pdfConverter,
		email:         email,
		now:           func() time.Time { return time.Now().UTC() },
	}
}

// Handle is the scheduler.ActionHandler entry point.
func (h *ReportScheduleHandler) Handle(ctx context.Context, tenantID uuid.UUID, _ scheduler.ScheduledAction) error {
	now := h.now()
	due, err := h.scheduleStore.ListDue(ctx, tenantID, now)
	if err != nil {
		return fmt.Errorf("report_scheduler: list due: %w", err)
	}
	for _, sched := range due {
		if err := h.runOne(ctx, tenantID, sched, now); err != nil {
			// Per-row error: persist the failure and move on so a
			// single broken schedule doesn't starve the rest.
			_ = h.scheduleStore.MarkRun(ctx, tenantID, sched.ID, now, "error", err.Error())
			continue
		}
	}
	return nil
}

func (h *ReportScheduleHandler) runOne(ctx context.Context, tenantID uuid.UUID, sched reporting.ReportSchedule, now time.Time) error {
	report, err := h.reportStore.Get(ctx, tenantID, sched.ReportID)
	if err != nil {
		return fmt.Errorf("load report: %w", err)
	}
	result, err := h.runner.Run(ctx, tenantID, report.Definition)
	if err != nil {
		return fmt.Errorf("run report: %w", err)
	}

	subject := fmt.Sprintf("[Kapp] %s — %s", report.Name, now.Format("2006-01-02"))
	body := fmt.Sprintf(
		"Scheduled report %q for %s.\n\nRows: %d.\n\nSee the attached %s for the full result.",
		sched.Name, now.Format(time.RFC3339), len(result.Rows), strings.ToUpper(sched.Format),
	)

	switch sched.Format {
	case reporting.ReportScheduleFormatCSV:
		csvBytes, err := renderReportCSV(result)
		if err != nil {
			return fmt.Errorf("render csv: %w", err)
		}
		if err := h.deliver(ctx, sched, subject, body, "report.csv", "text/csv", csvBytes); err != nil {
			return fmt.Errorf("deliver csv: %w", err)
		}
	case reporting.ReportScheduleFormatPDF:
		html := renderReportHTML(report.Name, result)
		var attachment []byte
		mime := "text/html"
		filename := "report.html"
		if h.pdfConverter != nil {
			pdf, err := h.pdfConverter.Convert(ctx, html)
			if err != nil {
				// Fall back to HTML rather than fail outright; the
				// recipient still receives a usable artifact.
				attachment = html
			} else {
				attachment = pdf
				mime = "application/pdf"
				filename = "report.pdf"
			}
		} else {
			attachment = html
		}
		if err := h.deliver(ctx, sched, subject, body, filename, mime, attachment); err != nil {
			return fmt.Errorf("deliver pdf: %w", err)
		}
	default:
		return fmt.Errorf("unknown format %q", sched.Format)
	}

	return h.scheduleStore.MarkRun(ctx, tenantID, sched.ID, now, "success", "")
}

// deliver sends the rendered artifact to every recipient. If the
// SMTP adapter supports attachments we use the typed call;
// otherwise we fall back to an inline body (a soft no-op for the
// local-dev "SMTP disabled" path).
func (h *ReportScheduleHandler) deliver(ctx context.Context, sched reporting.ReportSchedule, subject, body, filename, mime string, content []byte) error {
	if attachable, ok := h.email.(reportEmailAttachmentSender); ok {
		return attachable.SendWithAttachment(ctx, sched.Recipients, subject, body, filename, mime, content)
	}
	if h.email == nil {
		return notifications.ErrSMTPDisabled
	}
	// No attachment support: inline a marker so the recipient knows
	// the artifact was generated even though the worker can't push
	// the bytes today.
	inlineBody := body + "\n\n[attachment skipped: SMTP adapter does not support attachments]"
	return h.email.Send(ctx, sched.Recipients, subject, inlineBody)
}

// renderReportCSV serialises a Result.Rows table to CSV using the
// declared column order.
func renderReportCSV(result *reporting.Result) ([]byte, error) {
	var buf bytes.Buffer
	w := csv.NewWriter(&buf)
	if err := w.Write(result.Columns); err != nil {
		return nil, err
	}
	for _, row := range result.Rows {
		rec := make([]string, 0, len(result.Columns))
		for _, col := range result.Columns {
			rec = append(rec, fmt.Sprint(row[col]))
		}
		if err := w.Write(rec); err != nil {
			return nil, err
		}
	}
	w.Flush()
	if err := w.Error(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// renderReportHTML produces a minimal HTML representation of the
// result so the PDF converter has something to render. Kept tiny on
// purpose — the per-record print path owns rich Print Format
// templates; scheduled reports get a pragmatic table.
func renderReportHTML(reportName string, result *reporting.Result) []byte {
	var b bytes.Buffer
	// Every interpolated string is escaped: report names come from
	// saved_reports.name and cell values come from krecord data —
	// both are tenant-controlled and could contain arbitrary HTML.
	escapedName := html.EscapeString(reportName)
	fmt.Fprintf(&b, "<!doctype html><html><head><meta charset=\"utf-8\"><title>%s</title>", escapedName)
	b.WriteString("<style>body{font-family:sans-serif;margin:24px}table{border-collapse:collapse;width:100%}th,td{border:1px solid #ccc;padding:6px;text-align:left}th{background:#f0f0f0}</style></head><body>")
	fmt.Fprintf(&b, "<h1>%s</h1><table><thead><tr>", escapedName)
	for _, c := range result.Columns {
		fmt.Fprintf(&b, "<th>%s</th>", html.EscapeString(c))
	}
	b.WriteString("</tr></thead><tbody>")
	for _, row := range result.Rows {
		b.WriteString("<tr>")
		for _, c := range result.Columns {
			fmt.Fprintf(&b, "<td>%s</td>", html.EscapeString(fmt.Sprintf("%v", row[c])))
		}
		b.WriteString("</tr>")
	}
	b.WriteString("</tbody></table></body></html>")
	return b.Bytes()
}
