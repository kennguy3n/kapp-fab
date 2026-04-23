// Cost centres are a GL dimension, not a posting target. A journal
// line may optionally carry a cost_center code; reports (trial balance,
// income statement) then filter on it to produce per-cost-centre P&L.
// This is the Kapp analogue of ERPNext's Accounting Dimensions — a
// lightweight per-line tag rather than a second chart of accounts.
package ledger

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/kennguy3n/kapp-fab/internal/dbutil"
	"github.com/kennguy3n/kapp-fab/internal/ktype"
)

const KTypeCostCenter = "finance.cost_center"

var costCenterSchema = []byte(`{
  "name": "finance.cost_center",
  "version": 1,
  "fields": [
    {"name": "code", "type": "string", "required": true, "max_length": 32},
    {"name": "name", "type": "string", "required": true, "max_length": 200},
    {"name": "parent_code", "type": "string", "max_length": 32},
    {"name": "active", "type": "boolean", "default": true}
  ],
  "views": {
    "list": {"columns": ["code", "name", "parent_code", "active"]},
    "form": {"sections": [{"title": "Cost Center", "fields": ["code", "name", "parent_code", "active"]}]}
  },
  "cards": {"summary": "{{code}} — {{name}}"},
  "permissions": {"read": ["tenant.member"], "write": ["finance.admin", "tenant.admin"]}
}`)

// CostCenter is the typed row persisted in `cost_centers`.
type CostCenter struct {
	TenantID   uuid.UUID `json:"tenant_id"`
	Code       string    `json:"code"`
	Name       string    `json:"name"`
	ParentCode string    `json:"parent_code,omitempty"`
	Active     bool      `json:"active"`
}

// CostCenterKType returns the metadata-driven KType mirror of the typed
// cost_centers table. Kept separate from finance.All() so callers can
// ship the CC surface independently of the core finance catalog.
func CostCenterKType() ktype.KType {
	return ktype.KType{Name: KTypeCostCenter, Version: 1, Schema: costCenterSchema}
}

// UpsertCostCenter inserts or updates a row in `cost_centers`.
func (s *PGStore) UpsertCostCenter(ctx context.Context, cc CostCenter) (*CostCenter, error) {
	if cc.TenantID == uuid.Nil {
		return nil, errors.New("ledger: tenant id required")
	}
	if cc.Code == "" || cc.Name == "" {
		return nil, errors.New("ledger: cost_center code + name required")
	}
	out := cc
	err := dbutil.WithTenantTx(ctx, s.pool, cc.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO cost_centers (tenant_id, code, name, parent_code, active)
			 VALUES ($1, $2, $3, $4, $5)
			 ON CONFLICT (tenant_id, code) DO UPDATE SET
			     name = EXCLUDED.name,
			     parent_code = EXCLUDED.parent_code,
			     active = EXCLUDED.active`,
			cc.TenantID, cc.Code, cc.Name, nullIfEmpty(cc.ParentCode), cc.Active,
		)
		if err != nil {
			return fmt.Errorf("ledger: upsert cost_center: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}
