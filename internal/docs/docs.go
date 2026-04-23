// Package docs implements the Phase F "Docs KApp" — artifact documents
// with append-only version history. Each save writes a new row into
// docs_document_versions and updates docs_documents.current_version;
// nothing is ever UPDATE'd or DELETE'd from the history table, so the
// audit trail is immutable. A restore operation rewrites the live
// document from a historical version and writes a new history row
// pointing at the restored version, preserving the full timeline.
package docs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kennguy3n/kapp-fab/internal/dbutil"
)

// ErrNotFound is returned when a document or version does not exist.
var ErrNotFound = errors.New("docs: not found")

// Document is the live, current-version view of a Docs artifact.
type Document struct {
	ID             uuid.UUID       `json:"id"`
	TenantID       uuid.UUID       `json:"tenant_id"`
	Title          string          `json:"title"`
	DocType        string          `json:"doc_type"`
	Content        json.RawMessage `json:"content"`
	CurrentVersion int             `json:"current_version"`
	CreatedBy      uuid.UUID       `json:"created_by"`
	CreatedAt      time.Time       `json:"created_at"`
	UpdatedBy      *uuid.UUID      `json:"updated_by,omitempty"`
	UpdatedAt      time.Time       `json:"updated_at"`
}

// Version is an immutable history entry. RestoredFrom is non-nil when
// this row was written as a result of a /restore call — it points to
// the version whose content was copied over. Diff carries a caller-
// supplied JSON description of the change (typically a field-level
// patch set from the editor); the kernel treats it as opaque.
type Version struct {
	DocumentID    uuid.UUID       `json:"document_id"`
	TenantID      uuid.UUID       `json:"tenant_id"`
	Version       int             `json:"version"`
	Title         string          `json:"title"`
	Content       json.RawMessage `json:"content"`
	Diff          json.RawMessage `json:"diff,omitempty"`
	ChangeSummary string          `json:"change_summary,omitempty"`
	RestoredFrom  *int            `json:"restored_from,omitempty"`
	CreatedBy     uuid.UUID       `json:"created_by"`
	CreatedAt     time.Time       `json:"created_at"`
}

// Store persists Docs documents and their immutable history.
type Store struct {
	pool *pgxpool.Pool
}

// NewStore wires a Store over the shared pool.
func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// Create inserts a new document at version 1 and writes the initial
// version row in the same transaction so the history starts populated.
func (s *Store) Create(ctx context.Context, d Document) (*Document, error) {
	if d.TenantID == uuid.Nil || d.CreatedBy == uuid.Nil {
		return nil, errors.New("docs: tenant and creator required")
	}
	if d.Title == "" {
		return nil, errors.New("docs: title required")
	}
	if d.ID == uuid.Nil {
		d.ID = uuid.New()
	}
	if d.DocType == "" {
		d.DocType = "note"
	}
	if len(d.Content) == 0 {
		d.Content = json.RawMessage("{}")
	}
	d.CurrentVersion = 1

	err := dbutil.WithTenantTx(ctx, s.pool, d.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx,
			`INSERT INTO docs_documents
			     (id, tenant_id, title, doc_type, content, current_version,
			      created_by, created_at, updated_by, updated_at)
			 VALUES ($1,$2,$3,$4,$5,$6,$7, now(),$7, now())
			 RETURNING created_at, updated_at`,
			d.ID, d.TenantID, d.Title, d.DocType, []byte(d.Content),
			d.CurrentVersion, d.CreatedBy,
		).Scan(&d.CreatedAt, &d.UpdatedAt); err != nil {
			return fmt.Errorf("docs: insert: %w", err)
		}
		_, err := tx.Exec(ctx,
			`INSERT INTO docs_document_versions
			     (tenant_id, document_id, version, title, content,
			      diff, change_summary, restored_from, created_by, created_at)
			 VALUES ($1,$2,$3,$4,$5,'{}'::jsonb,$6,NULL,$7, now())`,
			d.TenantID, d.ID, d.CurrentVersion, d.Title, []byte(d.Content),
			"initial version", d.CreatedBy,
		)
		if err != nil {
			return fmt.Errorf("docs: insert version: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &d, nil
}

// SaveVersion writes a new history entry and updates the live document.
// Content fully replaces the previous content; diff and changeSummary
// are opaque caller-supplied metadata stored alongside.
func (s *Store) SaveVersion(
	ctx context.Context,
	tenantID, documentID, actor uuid.UUID,
	title string,
	content, diff json.RawMessage,
	changeSummary string,
) (*Document, error) {
	if tenantID == uuid.Nil || documentID == uuid.Nil || actor == uuid.Nil {
		return nil, errors.New("docs: tenant, document and actor required")
	}
	if len(content) == 0 {
		content = json.RawMessage("{}")
	}
	if len(diff) == 0 {
		diff = json.RawMessage("{}")
	}

	var out Document
	err := dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var current int
		var liveTitle string
		if err := tx.QueryRow(ctx,
			`SELECT current_version, title
			   FROM docs_documents
			  WHERE tenant_id = $1 AND id = $2
			  FOR UPDATE`,
			tenantID, documentID,
		).Scan(&current, &liveTitle); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return fmt.Errorf("docs: lock doc: %w", err)
		}
		newVersion := current + 1
		effectiveTitle := title
		if effectiveTitle == "" {
			effectiveTitle = liveTitle
		}

		if _, err := tx.Exec(ctx,
			`INSERT INTO docs_document_versions
			     (tenant_id, document_id, version, title, content,
			      diff, change_summary, restored_from, created_by, created_at)
			 VALUES ($1,$2,$3,$4,$5,$6,$7,NULL,$8, now())`,
			tenantID, documentID, newVersion, effectiveTitle,
			[]byte(content), []byte(diff), nullIfEmpty(changeSummary), actor,
		); err != nil {
			return fmt.Errorf("docs: insert version: %w", err)
		}

		if err := tx.QueryRow(ctx,
			`UPDATE docs_documents
			    SET title           = $3,
			        content         = $4,
			        current_version = $5,
			        updated_by      = $6,
			        updated_at      = now()
			  WHERE tenant_id = $1 AND id = $2
			  RETURNING id, tenant_id, title, doc_type, content, current_version,
			            created_by, created_at, updated_by, updated_at`,
				tenantID, documentID, effectiveTitle, []byte(content), newVersion, actor,
		).Scan(&out.ID, &out.TenantID, &out.Title, &out.DocType, &out.Content,
				&out.CurrentVersion, &out.CreatedBy, &out.CreatedAt,
				&out.UpdatedBy, &out.UpdatedAt); err != nil {
			return fmt.Errorf("docs: update doc: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// Get returns the live document.
func (s *Store) Get(ctx context.Context, tenantID, id uuid.UUID) (*Document, error) {
	var d Document
	err := dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT id, tenant_id, title, doc_type, content, current_version,
			        created_by, created_at, updated_by, updated_at
			   FROM docs_documents
			  WHERE tenant_id = $1 AND id = $2`,
			tenantID, id,
		).Scan(&d.ID, &d.TenantID, &d.Title, &d.DocType, &d.Content,
			&d.CurrentVersion, &d.CreatedBy, &d.CreatedAt,
			&d.UpdatedBy, &d.UpdatedAt)
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &d, nil
}

// List returns every document visible to the tenant.
func (s *Store) List(ctx context.Context, tenantID uuid.UUID) ([]Document, error) {
	var out []Document
	err := dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx,
			`SELECT id, tenant_id, title, doc_type, content, current_version,
			        created_by, created_at, updated_by, updated_at
			   FROM docs_documents
			  WHERE tenant_id = $1
			  ORDER BY updated_at DESC`,
			tenantID,
		)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var d Document
			if err := rows.Scan(&d.ID, &d.TenantID, &d.Title, &d.DocType,
				&d.Content, &d.CurrentVersion, &d.CreatedBy, &d.CreatedAt,
				&d.UpdatedBy, &d.UpdatedAt); err != nil {
				return err
			}
			out = append(out, d)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// Versions returns the full history for a document, newest first.
func (s *Store) Versions(ctx context.Context, tenantID, documentID uuid.UUID) ([]Version, error) {
	var out []Version
	err := dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx,
			`SELECT document_id, tenant_id, version, title, content,
			        diff, change_summary, restored_from, created_by, created_at
			   FROM docs_document_versions
			  WHERE tenant_id = $1 AND document_id = $2
			  ORDER BY version DESC`,
			tenantID, documentID,
		)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var v Version
			var summary *string
			var content, diff []byte
			if err := rows.Scan(&v.DocumentID, &v.TenantID, &v.Version, &v.Title,
				&content, &diff, &summary, &v.RestoredFrom, &v.CreatedBy, &v.CreatedAt); err != nil {
				return err
			}
			if len(content) > 0 {
				v.Content = json.RawMessage(content)
			}
			if len(diff) > 0 {
				v.Diff = json.RawMessage(diff)
			}
			if summary != nil {
				v.ChangeSummary = *summary
			}
			out = append(out, v)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// Restore copies a historical version's content onto the live document
// and writes a new history row flagged with restored_from so the audit
// trail shows the roll-back clearly.
func (s *Store) Restore(
	ctx context.Context,
	tenantID, documentID, actor uuid.UUID,
	targetVersion int,
) (*Document, error) {
	if tenantID == uuid.Nil || documentID == uuid.Nil || actor == uuid.Nil {
		return nil, errors.New("docs: tenant, document and actor required")
	}
	var out Document
	err := dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var current int
		if err := tx.QueryRow(ctx,
			`SELECT current_version
			   FROM docs_documents
			  WHERE tenant_id = $1 AND id = $2
			  FOR UPDATE`,
			tenantID, documentID,
		).Scan(&current); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		var srcTitle string
		var srcContent []byte
		if err := tx.QueryRow(ctx,
			`SELECT title, content
			   FROM docs_document_versions
			  WHERE tenant_id = $1 AND document_id = $2 AND version = $3`,
			tenantID, documentID, targetVersion,
		).Scan(&srcTitle, &srcContent); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}

		newVersion := current + 1
		if _, err := tx.Exec(ctx,
			`INSERT INTO docs_document_versions
			     (tenant_id, document_id, version, title, content,
			      diff, change_summary, restored_from, created_by, created_at)
			 VALUES ($1,$2,$3,$4,$5,'{}'::jsonb,$6,$7,$8, now())`,
			tenantID, documentID, newVersion, srcTitle, srcContent,
			fmt.Sprintf("restored from v%d", targetVersion), targetVersion, actor,
		); err != nil {
			return fmt.Errorf("docs: insert restore version: %w", err)
		}

		if err := tx.QueryRow(ctx,
			`UPDATE docs_documents
			    SET title           = $3,
			        content         = $4,
			        current_version = $5,
			        updated_by      = $6,
			        updated_at      = now()
			  WHERE tenant_id = $1 AND id = $2
			  RETURNING id, tenant_id, title, doc_type, content, current_version,
			            created_by, created_at, updated_by, updated_at`,
				tenantID, documentID, srcTitle, srcContent, newVersion, actor,
		).Scan(&out.ID, &out.TenantID, &out.Title, &out.DocType, &out.Content,
				&out.CurrentVersion, &out.CreatedBy, &out.CreatedAt,
				&out.UpdatedBy, &out.UpdatedAt); err != nil {
			return fmt.Errorf("docs: restore doc: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
