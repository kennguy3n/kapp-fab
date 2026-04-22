package main

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/kennguy3n/kapp-fab/internal/ktype"
)

// CommandRequest is the Phase A slash-command payload. KChat will POST this
// envelope to /kchat/commands when a user invokes `/kapp ...`.
type CommandRequest struct {
	TenantID uuid.UUID `json:"tenant_id"`
	UserID   uuid.UUID `json:"user_id"`
	Command  string    `json:"command"`
	Args     []string  `json:"args"`
}

// CommandResponse is what KChat will render inline in the chat thread.
type CommandResponse struct {
	Text  string `json:"text"`
	Card  *Card  `json:"card,omitempty"`
	Error string `json:"error,omitempty"`
}

// CommandDispatcher routes slash commands to concrete handlers. For Phase A
// we only handle /list-ktypes which enumerates the registered KType names.
type CommandDispatcher struct {
	registry *ktype.PGRegistry
}

// Dispatch runs the command and returns a response suitable for the caller
// to send straight back to KChat.
func (d *CommandDispatcher) Dispatch(ctx context.Context, req CommandRequest) (CommandResponse, error) {
	switch strings.ToLower(req.Command) {
	case "list-ktypes", "ktypes":
		return d.listKTypes(ctx)
	case "help":
		return CommandResponse{
			Text: "Available commands: /list-ktypes, /help",
		}, nil
	default:
		return CommandResponse{
			Text: fmt.Sprintf("Unknown command: %s", req.Command),
		}, errors.New("kchat: unknown command")
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
