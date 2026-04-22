package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/kennguy3n/kapp-fab/internal/ktype"
)

// Card is the payload we return to KChat for inline rendering. The real
// KChat card schema is richer; this is the minimum viable shape for Phase A.
type Card struct {
	Title    string     `json:"title"`
	Subtitle string     `json:"subtitle,omitempty"`
	Body     string     `json:"body,omitempty"`
	Fields   []CardKV   `json:"fields,omitempty"`
	Actions  []CardLink `json:"actions,omitempty"`
}

// CardKV is a labeled value displayed on a card.
type CardKV struct {
	Label string `json:"label"`
	Value string `json:"value"`
}

// CardLink is an action button on a card.
type CardLink struct {
	Label string `json:"label"`
	URL   string `json:"url"`
}

// CardRenderer renders a KType instance into a Card using the KType's
// cards.message template. The template is a Go text/template string stored in
// the KType schema; for Phase A we support simple field substitution.
type CardRenderer struct {
	registry *ktype.PGRegistry
}

// RenderCard resolves the KType and renders data into a Card.
func (c *CardRenderer) RenderCard(ctx context.Context, name string, data map[string]any) (Card, error) {
	kt, err := c.registry.Get(ctx, name, 0)
	if err != nil {
		return Card{}, err
	}
	var schema struct {
		Cards struct {
			// Summary is the flat-string form used by the Phase B CRM
			// KType schemas (e.g. "{{name}} ({{source}}) — {{status}}").
			// When present and Message.Title is empty, it is substituted
			// into the card title so the renderer handles both the
			// structured and the shorthand card contracts.
			Summary string `json:"summary"`
			Message struct {
				Title    string   `json:"title"`
				Subtitle string   `json:"subtitle"`
				Body     string   `json:"body"`
				Fields   []string `json:"fields"`
			} `json:"message"`
		} `json:"cards"`
	}
	_ = json.Unmarshal(kt.Schema, &schema)

	titleTpl := schema.Cards.Message.Title
	if titleTpl == "" {
		titleTpl = schema.Cards.Summary
	}
	card := Card{
		Title:    substitute(titleTpl, data, kt.Name),
		Subtitle: substitute(schema.Cards.Message.Subtitle, data, ""),
		Body:     substitute(schema.Cards.Message.Body, data, ""),
	}
	for _, field := range schema.Cards.Message.Fields {
		value := fmt.Sprintf("%v", data[field])
		card.Fields = append(card.Fields, CardKV{Label: field, Value: value})
	}
	return card, nil
}

// substitute replaces placeholders in the template with values from data.
// Supports the documented `{{ field }}` / `{{field}}` Mustache-style form
// (ARCHITECTURE.md §6) and the shorthand `{field}` form. If the template is
// empty, the fallback is returned.
func substitute(tpl string, data map[string]any, fallback string) string {
	if tpl == "" {
		return fallback
	}
	out := tpl
	for k, v := range data {
		value := fmt.Sprintf("%v", v)
		out = strings.ReplaceAll(out, "{{ "+k+" }}", value)
		out = strings.ReplaceAll(out, "{{"+k+"}}", value)
		out = strings.ReplaceAll(out, "{"+k+"}", value)
	}
	return out
}
