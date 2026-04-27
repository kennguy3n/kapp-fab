package agents

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/kennguy3n/kapp-fab/internal/insights"
	"github.com/kennguy3n/kapp-fab/internal/reporting"
)

// RegisterInsightsTools wires the Phase L insights agent tools onto an
// executor. Generation tools are gated behind RequiresConfirmation()
// so the assistant always shows the user the JSON it intends to save
// before mutating the tenant. Read-only tools (explain_result,
// post_dashboard_digest) are also gated to keep the audit trail
// uniform — the dry-run branch returns the preview without running
// the underlying SQL.
func RegisterInsightsTools(
	x *Executor,
	queries *insights.QueryStore,
	dashboards *insights.DashboardStore,
	runner *insights.Runner,
) {
	x.Register(&generateInsightsQueryTool{
		executor: x, queries: queries,
	})
	x.Register(&explainInsightsResultTool{
		executor: x, queries: queries, runner: runner,
	})
	x.Register(&postDashboardDigestTool{
		executor: x, dashboards: dashboards, runner: runner,
	})
}

// ----- insights.generate_query -----

type generateInsightsQueryInput struct {
	Prompt          string                  `json:"prompt"`
	Source          string                  `json:"source,omitempty"`
	Definition      *insights.QueryDefinition `json:"definition,omitempty"`
	Name            string                  `json:"name,omitempty"`
	Description     string                  `json:"description,omitempty"`
	CacheTTLSeconds *int                    `json:"cache_ttl_seconds,omitempty"`
}

type generateInsightsQueryTool struct {
	executor *Executor
	queries  *insights.QueryStore
}

func (t *generateInsightsQueryTool) Name() string {
	return "insights.generate_query"
}

// RequiresConfirmation is true because the commit-mode path persists a
// new saved query that any subsequent dashboard widget can replay —
// the assistant must show the QueryDefinition JSON to a human first.
func (t *generateInsightsQueryTool) RequiresConfirmation() bool { return true }

func (t *generateInsightsQueryTool) Invoke(ctx context.Context, inv Invocation) (*Result, error) {
	var in generateInsightsQueryInput
	if err := decodeInputs(inv, &in); err != nil {
		return nil, err
	}
	if strings.TrimSpace(in.Prompt) == "" && in.Definition == nil {
		return nil, errors.New("insights.generate_query: prompt or definition required")
	}
	def := buildQueryDefinitionFromPrompt(in)
	if err := def.Validate(); err != nil {
		return nil, fmt.Errorf("insights.generate_query: %w", err)
	}

	name := in.Name
	if name == "" {
		name = deriveQueryName(in.Prompt)
	}

	previewQuery := insights.Query{
		TenantID:        inv.TenantID,
		Name:            name,
		Description:     in.Description,
		Definition:      def,
		CacheTTLSeconds: in.CacheTTLSeconds,
	}
	previewBytes, _ := json.Marshal(previewQuery)

	if inv.Mode == ModeDryRun {
		return &Result{
			Tool:    t.Name(),
			Mode:    inv.Mode,
			Summary: fmt.Sprintf("Would create insights query %q over %s", name, def.Source),
			Preview: previewBytes,
		}, nil
	}

	saved, err := t.queries.Create(ctx, insights.Query{
		TenantID:        inv.TenantID,
		Name:            name,
		Description:     in.Description,
		Definition:      def,
		CacheTTLSeconds: in.CacheTTLSeconds,
		CreatedBy:       &inv.ActorID,
	})
	if err != nil {
		return nil, err
	}
	savedBytes, _ := json.Marshal(saved)
	return &Result{
		Tool:    t.Name(),
		Mode:    inv.Mode,
		Summary: fmt.Sprintf("Created insights query %s (%s)", saved.ID, saved.Name),
		Preview: savedBytes,
		Extra:   map[string]any{"query_id": saved.ID.String()},
	}, nil
}

// buildQueryDefinitionFromPrompt synthesises a sane starting definition
// from natural-language hints. The intent here is *not* to be a
// fully-fledged NL→SQL pipeline (the LLM caller is expected to fill
// in the structured `definition` field directly when it has more
// confidence). Instead we keep the heuristic narrow — pick a source,
// emit a "count" aggregation, and let the human confirm via dry_run.
func buildQueryDefinitionFromPrompt(in generateInsightsQueryInput) insights.QueryDefinition {
	if in.Definition != nil {
		return *in.Definition
	}
	source := in.Source
	if source == "" {
		source = guessSourceFromPrompt(in.Prompt)
	}
	def := insights.QueryDefinition{
		Definition: reporting.Definition{
			Source: source,
			Aggregations: []reporting.Aggregation{{
				Op:    reporting.AggCount,
				Alias: "count",
			}},
			Limit: 1000,
		},
	}
	return def
}

func guessSourceFromPrompt(prompt string) string {
	p := strings.ToLower(prompt)
	switch {
	case strings.Contains(p, "deal") || strings.Contains(p, "pipeline"):
		return "ktype:crm.deal"
	case strings.Contains(p, "lead"):
		return "ktype:crm.lead"
	case strings.Contains(p, "ticket") || strings.Contains(p, "helpdesk"):
		return "ktype:helpdesk.ticket"
	case strings.Contains(p, "invoice"):
		return "ktype:finance.ar_invoice"
	case strings.Contains(p, "bill"):
		return "ktype:finance.ap_bill"
	case strings.Contains(p, "journal") || strings.Contains(p, "ledger"):
		return "journal_entries"
	case strings.Contains(p, "stock") || strings.Contains(p, "inventory"):
		return "inventory_moves"
	default:
		return "ktype:crm.deal"
	}
}

func deriveQueryName(prompt string) string {
	p := strings.TrimSpace(prompt)
	if p == "" {
		return "Generated insights query"
	}
	if len(p) > 60 {
		p = p[:60] + "…"
	}
	return p
}

// ----- insights.explain_result -----

type explainInsightsResultInput struct {
	QueryID      uuid.UUID      `json:"query_id"`
	FilterParams map[string]any `json:"filter_params,omitempty"`
}

type explainInsightsResultTool struct {
	executor *Executor
	queries  *insights.QueryStore
	runner   *insights.Runner
}

func (t *explainInsightsResultTool) Name() string {
	return "insights.explain_result"
}

// RequiresConfirmation = true because the explanation embeds row
// values from the underlying SQL — surfacing those values into the
// LLM context window is itself worth a human ack on first use.
func (t *explainInsightsResultTool) RequiresConfirmation() bool { return true }

func (t *explainInsightsResultTool) Invoke(ctx context.Context, inv Invocation) (*Result, error) {
	var in explainInsightsResultInput
	if err := decodeInputs(inv, &in); err != nil {
		return nil, err
	}
	if in.QueryID == uuid.Nil {
		return nil, errors.New("insights.explain_result: query_id required")
	}
	q, err := t.queries.Get(ctx, inv.TenantID, in.QueryID)
	if err != nil {
		return nil, err
	}
	if inv.Mode == ModeDryRun {
		preview := map[string]any{
			"query_id":     q.ID,
			"query_name":   q.Name,
			"would_run":    true,
			"filter_count": len(in.FilterParams),
		}
		previewBytes, _ := json.Marshal(preview)
		return &Result{
			Tool:    t.Name(),
			Mode:    inv.Mode,
			Summary: fmt.Sprintf("Would run insights query %q and summarise results", q.Name),
			Preview: previewBytes,
		}, nil
	}

	out, err := t.runner.RunSaved(ctx, inv.TenantID, q.ID, in.FilterParams, false)
	if err != nil {
		return nil, err
	}
	summary := summariseRunResult(q, out)
	var rowCount int
	var cacheHit bool
	var queryHash string
	if out != nil {
		cacheHit = out.CacheHit
		queryHash = out.QueryHash
		if out.Result != nil {
			rowCount = len(out.Result.Rows)
		}
	}
	extraBytes, _ := json.Marshal(map[string]any{
		"row_count":  rowCount,
		"cache_hit":  cacheHit,
		"query_id":   q.ID,
		"query_hash": queryHash,
	})
	return &Result{
		Tool:    t.Name(),
		Mode:    inv.Mode,
		Summary: summary,
		Preview: extraBytes,
	}, nil
}

func summariseRunResult(q *insights.Query, out *insights.RunResult) string {
	if out == nil || out.Result == nil {
		return fmt.Sprintf("Query %q produced no result", q.Name)
	}
	rows := out.Result.Rows
	if len(rows) == 0 {
		return fmt.Sprintf("Query %q returned 0 rows over %s", q.Name, q.Definition.Source)
	}
	first := rows[0]
	pairs := make([]string, 0, len(first))
	for _, col := range out.Result.Columns {
		pairs = append(pairs, fmt.Sprintf("%s=%v", col, first[col]))
	}
	return fmt.Sprintf(
		"Query %q returned %d rows over %s. First row: %s",
		q.Name, len(rows), q.Definition.Source, strings.Join(pairs, ", "),
	)
}

// ----- insights.post_dashboard_digest -----

type postDashboardDigestInput struct {
	DashboardID uuid.UUID `json:"dashboard_id"`
	Channel     string    `json:"channel,omitempty"`
}

type postDashboardDigestTool struct {
	executor   *Executor
	dashboards *insights.DashboardStore
	runner     *insights.Runner
}

func (t *postDashboardDigestTool) Name() string {
	return "insights.post_dashboard_digest"
}

func (t *postDashboardDigestTool) RequiresConfirmation() bool { return true }

func (t *postDashboardDigestTool) Invoke(ctx context.Context, inv Invocation) (*Result, error) {
	var in postDashboardDigestInput
	if err := decodeInputs(inv, &in); err != nil {
		return nil, err
	}
	if in.DashboardID == uuid.Nil {
		return nil, errors.New("insights.post_dashboard_digest: dashboard_id required")
	}
	d, err := t.dashboards.Get(ctx, inv.TenantID, in.DashboardID)
	if err != nil {
		return nil, err
	}
	widgets, err := t.dashboards.ListWidgets(ctx, inv.TenantID, in.DashboardID)
	if err != nil {
		return nil, err
	}
	d.Widgets = widgets

	// Dry-run path skips the per-widget RunSaved fan-out: the preview
	// only needs the dashboard / widget metadata plus a placeholder so
	// the human can see the shape of the digest before paying SQL for
	// every widget. Commit mode below runs the queries.
	if inv.Mode == ModeDryRun {
		dryRunSections := make([]map[string]any, 0, len(widgets))
		for _, w := range widgets {
			dryRunSections = append(dryRunSections, map[string]any{
				"widget_id": w.ID,
				"viz_type":  w.VizType,
				"text":      "(preview — query not executed in dry-run)",
			})
		}
		preview := map[string]any{
			"dashboard_id":   d.ID,
			"dashboard_name": d.Name,
			"channel":        in.Channel,
			"sections":       dryRunSections,
		}
		previewBytes, _ := json.Marshal(preview)
		return &Result{
			Tool: t.Name(),
			Mode: inv.Mode,
			Summary: fmt.Sprintf(
				"Would post Dashboard %q digest with %d widget sections",
				d.Name, len(widgets),
			),
			Preview: previewBytes,
		}, nil
	}

	sections := make([]map[string]any, 0, len(widgets))
	for _, w := range widgets {
		section := map[string]any{
			"widget_id": w.ID,
			"viz_type":  w.VizType,
		}
		if w.QueryID == nil {
			section["text"] = "(no saved query bound)"
			sections = append(sections, section)
			continue
		}
		out, err := t.runner.RunSaved(ctx, inv.TenantID, *w.QueryID, nil, false)
		if err != nil || out == nil || out.Result == nil {
			section["text"] = "(unable to run widget query)"
			sections = append(sections, section)
			continue
		}
		section["row_count"] = len(out.Result.Rows)
		section["cache_hit"] = out.CacheHit
		section["text"] = digestWidgetText(out)
		sections = append(sections, section)
	}

	preview := map[string]any{
		"dashboard_id":   d.ID,
		"dashboard_name": d.Name,
		"channel":        in.Channel,
		"sections":       sections,
	}
	previewBytes, _ := json.Marshal(preview)

	summary := fmt.Sprintf(
		"Dashboard %q digest with %d widget sections",
		d.Name, len(sections),
	)
	// Commit-mode delivery is handled by the kchat-bridge process: the
	// caller (workflow / scheduler / interactive agent) takes the
	// preview payload and forwards it as a card. Returning the same
	// payload keeps the surface uniform.
	return &Result{
		Tool:    t.Name(),
		Mode:    inv.Mode,
		Summary: "Prepared " + summary,
		Preview: previewBytes,
		Extra: map[string]any{
			"dashboard_id": d.ID.String(),
		},
	}, nil
}

func digestWidgetText(out *insights.RunResult) string {
	if out == nil || out.Result == nil {
		return "no result"
	}
	rows := out.Result.Rows
	if len(rows) == 0 {
		return "0 rows"
	}
	if len(rows) == 1 {
		first := rows[0]
		pairs := make([]string, 0, len(first))
		for _, col := range out.Result.Columns {
			pairs = append(pairs, fmt.Sprintf("%s=%v", col, first[col]))
		}
		return strings.Join(pairs, ", ")
	}
	return fmt.Sprintf("%d rows", len(rows))
}
