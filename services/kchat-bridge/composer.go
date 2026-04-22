package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/kennguy3n/kapp-fab/internal/crm"
	"github.com/kennguy3n/kapp-fab/internal/ktype"
	"github.com/kennguy3n/kapp-fab/internal/record"
)

// Composer handles "turn this chat message into a record" actions. KChat
// surfaces these in the message context menu (Task / Deal / Activity).
// On first click the browser POSTs the message + target KType; the
// bridge pre-fills fields and returns an editable card. The user's
// confirmation arrives as a second POST with `confirm=true` and the
// edited data, at which point the record is created.
//
// Phase B keeps the extraction heuristics intentionally simple: first
// line → title, remainder → description. Smarter extraction (entity
// recognition, @mention resolution, amount parsing) can be layered in
// via the agent-tools service without changing this surface.
type Composer struct {
	registry *ktype.PGRegistry
	records  *record.PGStore
	cards    *CardRenderer
}

// ComposerRequest is the POST body for /kchat/composer/actions.
type ComposerRequest struct {
	TenantID uuid.UUID `json:"tenant_id"`
	UserID   uuid.UUID `json:"user_id"`
	// Action is one of "task", "deal", "activity".
	Action string `json:"action"`
	// Message is the original chat message body the user selected.
	Message string `json:"message"`
	// Confirm signals that Data carries the user-edited payload and the
	// bridge should persist it. Absent → return a prefilled preview.
	Confirm bool `json:"confirm,omitempty"`
	// Data is the edited payload sent with confirm=true.
	Data map[string]any `json:"data,omitempty"`
}

// ComposerResponse returns the preview card on the first POST and the
// created record on the confirmation POST.
type ComposerResponse struct {
	Preview *Card          `json:"preview,omitempty"`
	Record  *record.KRecord `json:"record,omitempty"`
	Error   string         `json:"error,omitempty"`
}

// Handle routes a composer request. Two-phase: preview → confirm.
func (c *Composer) Handle(ctx context.Context, req ComposerRequest) (ComposerResponse, error) {
	if req.TenantID == uuid.Nil || req.UserID == uuid.Nil {
		return ComposerResponse{Error: "tenant_id and user_id required"}, nil
	}
	action := strings.ToLower(req.Action)
	ktypeName, ok := composerKType(action)
	if !ok {
		return ComposerResponse{Error: fmt.Sprintf("unknown composer action %q", action)}, nil
	}

	// Confirm path — persist the edited data as a KRecord.
	if req.Confirm {
		return c.persist(ctx, req, ktypeName)
	}

	// Preview path — derive defaults and render a card.
	data := extractFields(action, req.Message, req.UserID)
	dataJSON, _ := json.Marshal(data)
	preview := Card{
		Title:    fmt.Sprintf("Create %s", ktypeName),
		Subtitle: "Review and confirm to create the record",
		Body:     string(dataJSON),
	}
	// If we have a registered KType with a card template, use it for a
	// richer preview. Unregistered KType → fall back to the JSON blob
	// so the client still has a visual.
	if c.cards != nil {
		if rendered, err := c.cards.RenderCard(ctx, ktypeName, data); err == nil {
			preview.Body = rendered.Body
			preview.Fields = rendered.Fields
		}
	}
	return ComposerResponse{Preview: &preview}, nil
}

func (c *Composer) persist(ctx context.Context, req ComposerRequest, ktypeName string) (ComposerResponse, error) {
	if c.records == nil {
		return ComposerResponse{Error: "record store not configured"}, nil
	}
	if req.Data == nil {
		return ComposerResponse{Error: "data required on confirm"}, nil
	}
	kt, err := c.registry.Get(ctx, ktypeName, 0)
	if err != nil {
		return ComposerResponse{Error: fmt.Sprintf("unknown ktype %s", ktypeName)}, nil
	}
	dataJSON, err := json.Marshal(req.Data)
	if err != nil {
		return ComposerResponse{}, fmt.Errorf("marshal composer data: %w", err)
	}
	created, err := c.records.Create(ctx, record.KRecord{
		TenantID:     req.TenantID,
		KType:        ktypeName,
		KTypeVersion: kt.Version,
		Data:         dataJSON,
		CreatedBy:    req.UserID,
	})
	if err != nil {
		var verrs ktype.ValidationErrors
		if errors.As(err, &verrs) {
			return ComposerResponse{Error: fmt.Sprintf("validation: %v", verrs)}, nil
		}
		return ComposerResponse{}, fmt.Errorf("composer create %s: %w", ktypeName, err)
	}
	return ComposerResponse{Record: created}, nil
}

// composerKType maps the UI action label to the target KType.
func composerKType(action string) (string, bool) {
	switch action {
	case "task":
		return crm.KTypeTask, true
	case "deal":
		return crm.KTypeDeal, true
	case "activity":
		return crm.KTypeActivity, true
	}
	return "", false
}

// extractFields splits a message body into KType-appropriate fields
// using trivial heuristics:
//   - Task:     title = first line, description = remainder, assignee = author
//   - Deal:     name = first line, notes = remainder, stage = qualification
//   - Activity: subject = first line, type = note
func extractFields(action, message string, author uuid.UUID) map[string]any {
	lines := strings.SplitN(strings.TrimSpace(message), "\n", 2)
	head := ""
	body := ""
	if len(lines) > 0 {
		head = strings.TrimSpace(lines[0])
	}
	if len(lines) > 1 {
		body = strings.TrimSpace(lines[1])
	}
	switch action {
	case "task":
		return map[string]any{
			"title":       head,
			"status":      "open",
			"assignee":    author.String(),
			"description": body,
		}
	case "deal":
		return map[string]any{
			"name":     head,
			"stage":    "qualification",
			"currency": "USD",
			"owner":    author.String(),
			"notes":    body,
		}
	case "activity":
		return map[string]any{
			"type":    "note",
			"subject": head,
		}
	}
	return map[string]any{}
}

// HandleHTTP adapts Composer.Handle to an HTTP handler. Kept inline
// here so main.go can wire the route with a one-liner without needing a
// separate request wrapper.
func (c *Composer) HandleHTTP(w http.ResponseWriter, r *http.Request) {
	var req ComposerRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	resp, err := c.Handle(r.Context(), req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}
