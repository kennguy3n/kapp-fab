package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/kennguy3n/kapp-fab/internal/tenant"
)

// placementHandlers backs GET/PUT /api/v1/tenants/{id}/placement.
//
// Read is open to any authenticated tenant member: the policy
// describes data residency the tenant is paying for, so users on
// the free pooled plan can still see what's currently active for
// their data even though they can't customise it.
//
// Write is gated behind plan tier — only paid plans can mutate the
// policy; free plans always see the platform-derived default. The
// gate matches the wizard's seeding behaviour (free → pooled b2c
// contract on the fabric).
//
// When a fabric client is wired, putPolicy forwards the validated
// policy to the fabric console; the local row is updated only after
// the fabric accepts so the local copy never drifts ahead of what
// the fabric thinks is active.
type placementHandlers struct {
	tenants *tenant.PGStore
	fabric  placementFabricClient
}

// placementFabricClient is the slice of *tenant.ZKFabricClient the
// placement endpoints need. Defined as an interface here so tests
// can swap in a stub without spinning up the real console.
type placementFabricClient interface {
	SetPlacementPolicy(ctx context.Context, tenantID uuid.UUID, policy tenant.PlacementPolicy) error
}

func (h *placementHandlers) get(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid tenant id", http.StatusBadRequest)
		return
	}
	t, err := h.tenants.Get(r.Context(), id)
	if err != nil {
		if errors.Is(err, tenant.ErrNotFound) {
			http.Error(w, "tenant not found", http.StatusNotFound)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	policy, ok, err := h.tenants.GetPlacementPolicy(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if !ok {
		// Surface the derived default for free / unprovisioned tenants
		// so the editor has a starting point.
		policy = tenant.DerivePlacementPolicy(tenant.PlacementPolicyConfig{Plan: t.Plan})
		policy.Tenant = id.String()
	}
	writeJSON(w, http.StatusOK, policy)
}

func (h *placementHandlers) put(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid tenant id", http.StatusBadRequest)
		return
	}
	t, err := h.tenants.Get(r.Context(), id)
	if err != nil {
		if errors.Is(err, tenant.ErrNotFound) {
			http.Error(w, "tenant not found", http.StatusNotFound)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if t.Plan == tenant.PlanFree {
		http.Error(w, "placement policy customisation requires a paid plan", http.StatusForbidden)
		return
	}
	var policy tenant.PlacementPolicy
	if err := json.NewDecoder(r.Body).Decode(&policy); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	policy.Tenant = id.String()
	if t.ZKBucket != "" {
		policy.Bucket = t.ZKBucket
	}
	if err := validatePlacementPolicy(policy); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if h.fabric != nil {
		if err := h.fabric.SetPlacementPolicy(r.Context(), id, policy); err != nil {
			http.Error(w, fmt.Sprintf("forward placement to fabric: %v", err), http.StatusBadGateway)
			return
		}
	}
	if err := h.tenants.SetPlacementPolicy(r.Context(), id, policy); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, policy)
}

// validatePlacementPolicy mirrors the structural checks the fabric's
// `placement_policy.Policy.Validate` enforces server-side. We
// duplicate them here so misconfigured requests return 400 fast
// without round-tripping to the fabric.
func validatePlacementPolicy(p tenant.PlacementPolicy) error {
	switch p.Spec.Encryption.Mode {
	case tenant.EncryptionModeManaged,
		tenant.EncryptionModeClientSide,
		tenant.EncryptionModePublicDistribute:
	case "":
		return errors.New("placement: encryption.mode is required")
	default:
		return fmt.Errorf("placement: unknown encryption.mode %q", p.Spec.Encryption.Mode)
	}
	if len(p.Spec.Placement.Provider) == 0 {
		return errors.New("placement: placement.provider must list at least one provider")
	}
	for i, c := range p.Spec.Placement.Country {
		if len(c) != 2 {
			return fmt.Errorf("placement: placement.country[%d]=%q is not an ISO-3166 alpha-2 code", i, c)
		}
	}
	return nil
}
