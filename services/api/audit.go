package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kennguy3n/kapp-fab/internal/audit"
	"github.com/kennguy3n/kapp-fab/internal/dbutil"
	"github.com/kennguy3n/kapp-fab/internal/platform"
)

// auditHandlers exposes a read-only slice of the audit_log table. The
// audit log is append-only (ARCHITECTURE.md §9) so there is no write
// surface here — entries are produced as a side effect of mutations via
// audit.LogTx inside tenant-scoped transactions.
type auditHandlers struct {
	pool *pgxpool.Pool
}

// verify walks the tenant's hash chain in (id) order and reports the
// first break — either a row whose recomputed hash does not match the
// stored row_hash, or a row whose prev_hash does not match the prior
// row's row_hash. Returns {ok: true} when the entire chain validates.
// Pre-chain rows (NULL row_hash, pre-dating the migration) are
// skipped so the verifier can be rolled out without a backfill.
func (h *auditHandlers) verify(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	res, err := verifyAuditChain(r.Context(), h.pool, t.ID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

type auditVerifyResult struct {
	OK       bool   `json:"ok"`
	Checked  int    `json:"checked"`
	BreakAt  *int64 `json:"break_at,omitempty"`
	Reason   string `json:"reason,omitempty"`
}

func verifyAuditChain(ctx context.Context, pool *pgxpool.Pool, tenantID uuid.UUID) (auditVerifyResult, error) {
	var res auditVerifyResult
	err := dbutil.WithTenantTx(ctx, pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx,
			`SELECT id, tenant_id, target_id, action, before, after, context,
			        created_at, prev_hash, row_hash
			 FROM audit_log
			 WHERE tenant_id = $1
			 ORDER BY id ASC`,
			tenantID,
		)
		if err != nil {
			return fmt.Errorf("audit verify: query: %w", err)
		}
		defer rows.Close()
		var lastHash []byte
		for rows.Next() {
			var (
				id        int64
				tID       uuid.UUID
				targetID  *uuid.UUID
				action    string
				before    []byte
				after     []byte
				contextB  []byte
				createdAt time.Time
				prevHash  []byte
				rowHash   []byte
			)
			if err := rows.Scan(&id, &tID, &targetID, &action, &before, &after, &contextB,
				&createdAt, &prevHash, &rowHash); err != nil {
				return fmt.Errorf("audit verify: scan: %w", err)
			}
			res.Checked++
			// Skip rows that pre-date the chain migration.
			if rowHash == nil {
				continue
			}
			if !bytes.Equal(prevHash, lastHash) {
				res.BreakAt = &id
				res.Reason = "prev_hash mismatch"
				return nil
			}
			entry := audit.Entry{
				TenantID: tID,
				Action:   action,
				TargetID: targetID,
			}
			want := audit.ComputeRowHash(prevHash, entry, before, after, contextB, createdAt)
			if !bytes.Equal(want, rowHash) {
				res.BreakAt = &id
				res.Reason = "row_hash mismatch"
				return nil
			}
			lastHash = rowHash
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("audit verify: iterate: %w", err)
		}
		res.OK = true
		return nil
	})
	return res, err
}

// list runs under tenant context so RLS filters rows by tenant even
// though audit_log partitions on tenant_id. Pagination is offset-based
// because tenants' audit volumes are bounded by recent activity; if
// this endpoint ever needs to page deep we'll switch to keyset.
func (h *auditHandlers) list(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}

	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	if offset < 0 {
		offset = 0
	}

	filter := auditFilter{
		TargetKType: r.URL.Query().Get("target_ktype"),
		Limit:       limit,
		Offset:      offset,
	}
	if raw := r.URL.Query().Get("target_id"); raw != "" {
		id, err := uuid.Parse(raw)
		if err != nil {
			http.Error(w, "invalid target_id", http.StatusBadRequest)
			return
		}
		filter.TargetID = &id
	}

	entries, err := listAuditEntries(r.Context(), h.pool, t.ID, filter)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if entries == nil {
		entries = []audit.Entry{}
	}
	writeJSON(w, http.StatusOK, entries)
}

type auditFilter struct {
	TargetKType string
	TargetID    *uuid.UUID
	Limit       int
	Offset      int
}

// listAuditEntries runs the audit read query inside a tenant-scoped
// transaction so `SET LOCAL app.tenant_id` is active when RLS policies
// evaluate.
func listAuditEntries(
	ctx context.Context,
	pool *pgxpool.Pool,
	tenantID uuid.UUID,
	filter auditFilter,
) ([]audit.Entry, error) {
	var entries []audit.Entry
	err := dbutil.WithTenantTx(ctx, pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		// Build the WHERE clause dynamically; keep the ordering stable
		// on (created_at, id) so offset pagination is deterministic.
		args := []any{tenantID}
		where := "tenant_id = $1"
		if filter.TargetKType != "" {
			args = append(args, filter.TargetKType)
			where += fmt.Sprintf(" AND target_ktype = $%d", len(args))
		}
		if filter.TargetID != nil {
			args = append(args, *filter.TargetID)
			where += fmt.Sprintf(" AND target_id = $%d", len(args))
		}
		args = append(args, filter.Limit, filter.Offset)
		q := fmt.Sprintf(
			`SELECT id, tenant_id, actor_id, actor_kind, action,
			        target_ktype, target_id, before, after, context, created_at
			 FROM audit_log
			 WHERE %s
			 ORDER BY created_at DESC, id DESC
			 LIMIT $%d OFFSET $%d`,
			where, len(args)-1, len(args),
		)
		rows, err := tx.Query(ctx, q, args...)
		if err != nil {
			return fmt.Errorf("audit: list: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var (
				e           audit.Entry
				actorKind   string
				targetKType *string
				before      []byte
				after       []byte
				contextCol  []byte
			)
			if err := rows.Scan(
				&e.ID, &e.TenantID, &e.ActorID, &actorKind, &e.Action,
				&targetKType, &e.TargetID, &before, &after, &contextCol, &e.CreatedAt,
			); err != nil {
				return fmt.Errorf("audit: scan: %w", err)
			}
			e.ActorKind = audit.ActorKind(actorKind)
			if targetKType != nil {
				e.TargetKType = *targetKType
			}
			if len(before) > 0 {
				e.Before = json.RawMessage(before)
			}
			if len(after) > 0 {
				e.After = json.RawMessage(after)
			}
			if len(contextCol) > 0 {
				e.Context = json.RawMessage(contextCol)
			}
			entries = append(entries, e)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return entries, nil
}
