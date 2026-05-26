package main

// Phase N9d — KChat slash command for cycle counts.
//
// Sub-commands mirror the HTTP surface but skip seeding (the UI
// is the right place to seed) and fold list + show together:
//
//   /cycle-count list [status]
//   /cycle-count show <code-or-id>
//   /cycle-count post <code-or-id>

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/kennguy3n/kapp-fab/internal/inventory"
)

// cycleCountCommand routes the /cycle-count slash command.
func (d *CommandDispatcher) cycleCountCommand(ctx context.Context, req CommandRequest) (CommandResponse, error) {
	if d.cycleCounts == nil {
		return CommandResponse{Text: "/cycle-count: cycle-count store not configured"}, nil
	}
	if req.TenantID == uuid.Nil {
		return CommandResponse{Text: "/cycle-count: tenant context required"}, nil
	}
	sub := ""
	rest := ""
	if len(req.Args) > 0 {
		sub = strings.ToLower(req.Args[0])
	}
	if len(req.Args) > 1 {
		rest = strings.TrimSpace(strings.Join(req.Args[1:], " "))
	}
	switch sub {
	case "", "list":
		return d.cycleCountList(ctx, req.TenantID, rest)
	case "show":
		return d.cycleCountShow(ctx, req.TenantID, rest)
	case "post":
		return d.cycleCountPost(ctx, req.TenantID, req.UserID, rest)
	default:
		return CommandResponse{
			Text: "/cycle-count: unknown sub-command. Try /cycle-count list|show|post.",
		}, nil
	}
}

func (d *CommandDispatcher) cycleCountList(ctx context.Context, tenantID uuid.UUID, status string) (CommandResponse, error) {
	filter := inventory.CycleCountFilter{Status: strings.ToLower(strings.TrimSpace(status))}
	sessions, err := d.cycleCounts.ListSessions(ctx, tenantID, filter)
	if err != nil {
		return CommandResponse{Text: fmt.Sprintf("/cycle-count list: %v", err)}, nil
	}
	if len(sessions) == 0 {
		return CommandResponse{Text: "No cycle-count sessions found."}, nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Cycle-count sessions (%d):\n", len(sessions))
	for i := range sessions {
		s := &sessions[i]
		fmt.Fprintf(&b, "• %s — %s — %s\n", s.Code, s.Status, s.ID)
	}
	return CommandResponse{Text: b.String()}, nil
}

func (d *CommandDispatcher) cycleCountShow(ctx context.Context, tenantID uuid.UUID, ref string) (CommandResponse, error) {
	if ref == "" {
		return CommandResponse{Text: "/cycle-count show: id or code required"}, nil
	}
	session, err := d.resolveCycleCountSession(ctx, tenantID, ref)
	if err != nil {
		return CommandResponse{Text: fmt.Sprintf("/cycle-count show: %v", err)}, nil
	}
	lines, err := d.cycleCounts.ListLines(ctx, tenantID, session.ID)
	if err != nil {
		return CommandResponse{Text: fmt.Sprintf("/cycle-count show: %v", err)}, nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Cycle-count session %s\n", session.Code)
	fmt.Fprintf(&b, "Status: %s\n", session.Status)
	fmt.Fprintf(&b, "Warehouse: %s\n", session.WarehouseID)
	fmt.Fprintf(&b, "Lines: %d\n", len(lines))
	for i := range lines {
		ln := &lines[i]
		fmt.Fprintf(&b, "  • item %s — expected %s — counted %s — variance %s\n",
			ln.ItemID, ln.ExpectedQty.String(), ln.CountedQty.String(), ln.Variance.String())
	}
	return CommandResponse{Text: b.String()}, nil
}

func (d *CommandDispatcher) cycleCountPost(ctx context.Context, tenantID, actor uuid.UUID, ref string) (CommandResponse, error) {
	if ref == "" {
		return CommandResponse{Text: "/cycle-count post: id or code required"}, nil
	}
	session, err := d.resolveCycleCountSession(ctx, tenantID, ref)
	if err != nil {
		return CommandResponse{Text: fmt.Sprintf("/cycle-count post: %v", err)}, nil
	}
	out, err := d.cycleCounts.PostSession(ctx, tenantID, session.ID, actor)
	if err != nil {
		return CommandResponse{Text: fmt.Sprintf("/cycle-count post: %v", err)}, nil
	}
	return CommandResponse{Text: fmt.Sprintf("Posted cycle-count session %s (status=%s).", out.Code, out.Status)}, nil
}

// resolveCycleCountSession accepts either a UUID or a code. A UUID
// parse-success short-circuits the lookup; otherwise we walk the
// session list looking for a code match (case-insensitive).
func (d *CommandDispatcher) resolveCycleCountSession(ctx context.Context, tenantID uuid.UUID, ref string) (*inventory.CycleCountSession, error) {
	if id, err := uuid.Parse(ref); err == nil {
		return d.cycleCounts.GetSession(ctx, tenantID, id)
	}
	sessions, err := d.cycleCounts.ListSessions(ctx, tenantID, inventory.CycleCountFilter{Limit: 200})
	if err != nil {
		return nil, err
	}
	for i := range sessions {
		if strings.EqualFold(sessions[i].Code, ref) {
			return &sessions[i], nil
		}
	}
	return nil, errors.New("session not found")
}
