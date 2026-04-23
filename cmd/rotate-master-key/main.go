// Command rotate-master-key re-encrypts every `krecords.data` JSONB
// field under the current KAPP_MASTER_KEY by decrypting with the
// retiring KAPP_MASTER_KEY_PREV and re-encrypting with the current
// key. It walks the table in tenant-scoped batches so long-running
// rotations do not monopolize a single transaction.
//
// Operational flow:
//
//  1. Set the next master key as KAPP_MASTER_KEY and demote the
//     current key to KAPP_MASTER_KEY_PREV. All services must be
//     restarted so the KeyManager picks up both values and the
//     dual-key DecryptString path is active.
//  2. Run this tool (scripts/rotate_master_key.sh wraps it with
//     sensible defaults). It iterates every tenant, decrypts each
//     encrypted string field under the old key, and re-encrypts
//     under the new key.
//  3. After completion, unset KAPP_MASTER_KEY_PREV and restart.
//
// The rotation is idempotent: fields already under the current key
// decrypt via the primary path and are written back unchanged.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kennguy3n/kapp-fab/internal/dbutil"
	"github.com/kennguy3n/kapp-fab/internal/platform"
	"github.com/kennguy3n/kapp-fab/internal/tenant"
)

func main() {
	if err := run(); err != nil {
		log.Fatalf("rotate-master-key: %v", err)
	}
}

func run() error {
	batchSize := flag.Int("batch", 200, "records per tenant batch")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, err := platform.LoadConfig()
	if err != nil {
		return err
	}
	pool, err := platform.NewPool(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()

	masterKey, err := tenant.LoadMasterKey()
	if err != nil {
		return fmt.Errorf("load current master key: %w", err)
	}
	prevKey, err := tenant.LoadPrevMasterKey()
	if err != nil {
		return fmt.Errorf("load previous master key: %w", err)
	}
	if prevKey == nil {
		return fmt.Errorf("KAPP_MASTER_KEY_PREV is required; nothing to rotate from")
	}
	km, err := tenant.NewKeyManagerWithPrev(masterKey, prevKey, time.Hour)
	if err != nil {
		return err
	}

	tenants, err := listTenantIDs(ctx, pool)
	if err != nil {
		return err
	}
	log.Printf("rotate: %d tenants queued", len(tenants))
	for _, tID := range tenants {
		if err := rotateTenant(ctx, pool, km, tID, *batchSize); err != nil {
			return fmt.Errorf("tenant %s: %w", tID, err)
		}
	}
	log.Printf("rotate: done")
	return nil
}

func listTenantIDs(ctx context.Context, pool *pgxpool.Pool) ([]uuid.UUID, error) {
	rows, err := pool.Query(ctx, `SELECT id FROM tenants ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// rotateTenant iterates a tenant's krecords in id-order, paging by
// batchSize. For each row we walk the JSONB data field, re-encrypt any
// string that carries the ciphertext prefix, and UPDATE the row when
// anything changed. All writes happen inside a tenant-scoped
// transaction so RLS + SET LOCAL app.tenant_id are enforced.
func rotateTenant(ctx context.Context, pool *pgxpool.Pool, km *tenant.KeyManager, tID uuid.UUID, batchSize int) error {
	var rotated int
	lastID := uuid.Nil
	for {
		done, err := rotateBatch(ctx, pool, km, tID, lastID, batchSize, &rotated, &lastID)
		if err != nil {
			return err
		}
		if done {
			break
		}
	}
	log.Printf("rotate: tenant=%s rotated=%d records", tID, rotated)
	return nil
}

func rotateBatch(
	ctx context.Context,
	p *pgxpool.Pool,
	km *tenant.KeyManager,
	tID uuid.UUID,
	lastID uuid.UUID,
	batchSize int,
	rotated *int,
	lastOut *uuid.UUID,
) (bool, error) {
	done := true
	err := dbutil.WithTenantTx(ctx, p, tID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx,
			`SELECT id, data FROM krecords
			 WHERE tenant_id = $1 AND id > $2
			 ORDER BY id ASC
			 LIMIT $3`,
			tID, lastID, batchSize,
		)
		if err != nil {
			return err
		}
		type pending struct {
			id   uuid.UUID
			data json.RawMessage
		}
		var updates []pending
		for rows.Next() {
			var id uuid.UUID
			var raw []byte
			if err := rows.Scan(&id, &raw); err != nil {
				rows.Close()
				return err
			}
			updates = append(updates, pending{id: id, data: raw})
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		if len(updates) == 0 {
			return nil
		}
		done = false
		for _, u := range updates {
			newData, changed, err := rewriteEncrypted(km, tID, u.data)
			if err != nil {
				return fmt.Errorf("record %s: %w", u.id, err)
			}
			*lastOut = u.id
			if !changed {
				continue
			}
			if _, err := tx.Exec(ctx,
				`UPDATE krecords SET data = $1 WHERE tenant_id = $2 AND id = $3`,
				newData, tID, u.id,
			); err != nil {
				return fmt.Errorf("update record %s: %w", u.id, err)
			}
			*rotated++
		}
		return nil
	})
	return done, err
}

// rewriteEncrypted walks the record's JSON tree and re-encrypts any
// string that carries the ciphertext prefix. Returns (new bytes, true)
// when any field changed, (original, false) otherwise.
func rewriteEncrypted(km *tenant.KeyManager, tID uuid.UUID, raw json.RawMessage) (json.RawMessage, bool, error) {
	if len(raw) == 0 {
		return raw, false, nil
	}
	var root any
	if err := json.Unmarshal(raw, &root); err != nil {
		return raw, false, err
	}
	changed := false
	newRoot, err := walk(km, tID, root, &changed)
	if err != nil {
		return nil, false, err
	}
	if !changed {
		return raw, false, nil
	}
	out, err := json.Marshal(newRoot)
	if err != nil {
		return nil, false, err
	}
	return out, true, nil
}

func walk(km *tenant.KeyManager, tID uuid.UUID, v any, changed *bool) (any, error) {
	switch t := v.(type) {
	case map[string]any:
		for k, val := range t {
			nv, err := walk(km, tID, val, changed)
			if err != nil {
				return nil, err
			}
			t[k] = nv
		}
		return t, nil
	case []any:
		for i, val := range t {
			nv, err := walk(km, tID, val, changed)
			if err != nil {
				return nil, err
			}
			t[i] = nv
		}
		return t, nil
	case string:
		if !tenant.IsEncrypted(t) {
			return t, nil
		}
		rotated, err := km.ReencryptString(tID, t)
		if err != nil {
			return nil, err
		}
		if rotated != t {
			*changed = true
		}
		return rotated, nil
	default:
		return v, nil
	}
}
