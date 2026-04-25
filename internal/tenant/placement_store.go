package tenant

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// SetPlacementPolicy persists the active placement policy on the
// tenants row. Called by the wizard after it provisions the tenant on
// the fabric console (`PUT /api/tenants/{id}/placement`) so the local
// row always agrees with what the fabric thinks the policy is.
//
// Returns ErrNotFound when the tenant id does not exist.
func (s *PGStore) SetPlacementPolicy(ctx context.Context, id uuid.UUID, policy PlacementPolicy) error {
	if id == uuid.Nil {
		return errors.New("tenant: placement policy: tenant id required")
	}
	body, err := json.Marshal(policy)
	if err != nil {
		return fmt.Errorf("tenant: marshal placement policy: %w", err)
	}
	tag, err := s.pool.Exec(ctx,
		`UPDATE tenants
		    SET placement_policy = $1::jsonb,
		        updated_at       = now()
		  WHERE id = $2`,
		body, id,
	)
	if err != nil {
		return fmt.Errorf("tenant: set placement policy: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// GetPlacementPolicy reads the active placement policy off the tenants
// row. Returns (zero, false, nil) for tenants that have not had a
// policy set yet (`placement_policy IS NULL`) so the caller can fall
// back to a derived default.
func (s *PGStore) GetPlacementPolicy(ctx context.Context, id uuid.UUID) (PlacementPolicy, bool, error) {
	var raw []byte
	err := s.pool.QueryRow(ctx,
		`SELECT placement_policy FROM tenants WHERE id = $1`, id,
	).Scan(&raw)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return PlacementPolicy{}, false, ErrNotFound
		}
		return PlacementPolicy{}, false, fmt.Errorf("tenant: get placement policy: %w", err)
	}
	if len(raw) == 0 {
		return PlacementPolicy{}, false, nil
	}
	var policy PlacementPolicy
	if err := json.Unmarshal(raw, &policy); err != nil {
		return PlacementPolicy{}, false, fmt.Errorf("tenant: decode placement policy: %w", err)
	}
	return policy, true, nil
}
