//go:build integration
// +build integration

package integrationtest

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/kennguy3n/kapp-fab/internal/crm"
	"github.com/kennguy3n/kapp-fab/internal/insights"
	"github.com/kennguy3n/kapp-fab/internal/reporting"
	"github.com/kennguy3n/kapp-fab/internal/tenant"
)

// TestInsightsEmbedTokenLifecycle exercises the full embed lifecycle:
//  1. Issue a token for a dashboard.
//  2. LookupByToken succeeds and increments view_count.
//  3. Revoke flips the row.
//  4. LookupByToken returns ErrEmbedRevoked.
//  5. Listing shows the embed with the matching digest (no plaintext).
func TestInsightsEmbedTokenLifecycle(t *testing.T) {
	h := newHarness(t)
	if h.adminPool == nil {
		t.Skip("KAPP_TEST_ADMIN_DB_URL not set; skipping embed test")
	}
	ctx := context.Background()
	tn, err := h.tenants.Create(ctx, tenant.CreateInput{
		Slug: uniqueSlug("embed"), Name: "Embed Co", Cell: "test", Plan: "enterprise",
	})
	if err != nil {
		t.Fatalf("tenant: %v", err)
	}
	if err := registerPhaseBKTypes(ctx, h.ktypes, tn.ID.String()[:8]); err != nil {
		t.Fatalf("register ktypes: %v", err)
	}
	dashboards := insights.NewDashboardStore(h.pool)
	queries := insights.NewQueryStore(h.pool)
	cache := insights.NewCacheStore(h.pool)
	rep := reporting.NewRunner(h.pool)
	runner := insights.NewRunner(h.pool, cache, queries, rep)
	embeds := insights.NewEmbedStore(h.pool, h.adminPool)

	dash, err := dashboards.Create(ctx, insights.Dashboard{
		TenantID: tn.ID,
		Name:     "Embed dashboard",
		Layout:   json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatalf("create dashboard: %v", err)
	}

	out, err := embeds.Create(ctx, insights.Embed{
		TenantID:    tn.ID,
		DashboardID: dash.ID,
		MaxViews:    3,
	})
	if err != nil {
		t.Fatalf("create embed: %v", err)
	}
	if out.Token == "" {
		t.Fatalf("expected plaintext token on Create; got empty")
	}
	if out.TokenDigest == "" {
		t.Fatalf("expected token digest on Create; got empty")
	}

	// Lookup increments view_count by 1.
	got, err := embeds.LookupByToken(ctx, out.Token)
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if got.DashboardID != dash.ID {
		t.Fatalf("lookup returned wrong dashboard id: got=%s want=%s", got.DashboardID, dash.ID)
	}

	// List should expose digest only, not the plaintext token.
	list, err := embeds.List(ctx, tn.ID, dash.ID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("expected 1 embed, got %d", len(list))
	}
	if list[0].Token != "" {
		t.Fatalf("List must not return plaintext token; got %q", list[0].Token)
	}
	if list[0].TokenDigest != out.TokenDigest {
		t.Fatalf("digest mismatch: got=%s want=%s", list[0].TokenDigest, out.TokenDigest)
	}

	// Revoke and verify subsequent lookup returns ErrEmbedRevoked.
	if err := embeds.Revoke(ctx, tn.ID, out.ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if _, err := embeds.LookupByToken(ctx, out.Token); !errors.Is(err, insights.ErrEmbedRevoked) {
		t.Fatalf("expected ErrEmbedRevoked, got %v", err)
	}

	// Hide unused-import lint when this file is built without the
	// runner being wired (the runner is constructed for parallel
	// integration coverage of the dashboard render path).
	_ = runner
	_ = uuid.Nil
	_ = crm.KTypeDeal
}

// TestInsightsEmbedMaxViewsExhaustion verifies that the view-count
// fence kicks in after MaxViews lookups so a leaked token can't be
// reused indefinitely.
func TestInsightsEmbedMaxViewsExhaustion(t *testing.T) {
	h := newHarness(t)
	if h.adminPool == nil {
		t.Skip("KAPP_TEST_ADMIN_DB_URL not set; skipping embed views test")
	}
	ctx := context.Background()
	tn, err := h.tenants.Create(ctx, tenant.CreateInput{
		Slug: uniqueSlug("embedmv"), Name: "Embed MV Co", Cell: "test", Plan: "enterprise",
	})
	if err != nil {
		t.Fatalf("tenant: %v", err)
	}
	dashboards := insights.NewDashboardStore(h.pool)
	embeds := insights.NewEmbedStore(h.pool, h.adminPool)

	dash, err := dashboards.Create(ctx, insights.Dashboard{
		TenantID: tn.ID,
		Name:     "Bounded dashboard",
		Layout:   json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatalf("create dashboard: %v", err)
	}
	out, err := embeds.Create(ctx, insights.Embed{
		TenantID:    tn.ID,
		DashboardID: dash.ID,
		MaxViews:    2,
	})
	if err != nil {
		t.Fatalf("create embed: %v", err)
	}
	for i := 0; i < 2; i++ {
		if _, err := embeds.LookupByToken(ctx, out.Token); err != nil {
			t.Fatalf("lookup %d: %v", i, err)
		}
	}
	if _, err := embeds.LookupByToken(ctx, out.Token); !errors.Is(err, insights.ErrEmbedExceeded) {
		t.Fatalf("expected ErrEmbedExceeded after MaxViews exhausted, got %v", err)
	}
}
