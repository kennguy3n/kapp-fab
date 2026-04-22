package platform

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// idempotencyCachedResponse is persisted in the idempotency_keys table so a
// replay returns the same status + body the original request produced.
type idempotencyCachedResponse struct {
	Status int             `json:"status"`
	Body   json.RawMessage `json:"body"`
}

// IdempotencyMiddleware implements ARCHITECTURE.md §8 rule 6: all mutating
// requests MUST carry an Idempotency-Key header. The middleware looks up the
// (tenant_id, key) pair in the idempotency_keys table:
//
//   - hit: reply with the cached status + body, short-circuiting the handler
//   - miss: let the handler run, buffer its response, and persist the pair
//
// The middleware only acts on mutating HTTP methods (POST/PATCH/PUT/DELETE).
// Safe methods are forwarded unchanged. Keys scope to the tenant on the
// request context, so two tenants can legitimately use the same client-chosen
// key.
func IdempotencyMiddleware(pool *pgxpool.Pool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !isMutation(r.Method) {
				next.ServeHTTP(w, r)
				return
			}
			key := r.Header.Get("Idempotency-Key")
			if key == "" {
				http.Error(w, "Idempotency-Key header required", http.StatusBadRequest)
				return
			}
			t := TenantFromContext(r.Context())
			if t == nil {
				http.Error(w, "tenant context missing", http.StatusInternalServerError)
				return
			}

			cached, err := loadIdempotent(r.Context(), pool, t.ID, key)
			if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				http.Error(w, "idempotency store unavailable", http.StatusInternalServerError)
				return
			}
			if cached != nil {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(cached.Status)
				_, _ = w.Write(cached.Body)
				return
			}

			rec := &responseRecorder{ResponseWriter: w, status: http.StatusOK, buf: new(bytes.Buffer)}
			next.ServeHTTP(rec, r)

			if rec.status < 400 {
				_ = saveIdempotent(r.Context(), pool, t.ID, key, rec.status, rec.buf.Bytes())
			}
		})
	}
}

func isMutation(method string) bool {
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	}
	return false
}

// responseRecorder captures the status code and body so we can persist them
// alongside the idempotency key. It still writes through to the underlying
// ResponseWriter so the client sees the response immediately.
type responseRecorder struct {
	http.ResponseWriter
	status int
	buf    *bytes.Buffer
	wrote  bool
}

func (r *responseRecorder) WriteHeader(code int) {
	if r.wrote {
		return
	}
	r.status = code
	r.wrote = true
	r.ResponseWriter.WriteHeader(code)
}

func (r *responseRecorder) Write(p []byte) (int, error) {
	if !r.wrote {
		r.wrote = true
	}
	r.buf.Write(p)
	return r.ResponseWriter.Write(p)
}

func loadIdempotent(
	ctx context.Context,
	pool *pgxpool.Pool,
	tenantID uuid.UUID,
	key string,
) (*idempotencyCachedResponse, error) {
	var cached idempotencyCachedResponse
	err := WithTenantTx(ctx, pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var status int
		var body json.RawMessage
		err := tx.QueryRow(ctx,
			`SELECT response_code, response_body FROM idempotency_keys
			 WHERE tenant_id = $1 AND key = $2`,
			tenantID, key,
		).Scan(&status, &body)
		if err != nil {
			return err
		}
		cached.Status = status
		cached.Body = body
		return nil
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return &cached, nil
}

func saveIdempotent(
	ctx context.Context,
	pool *pgxpool.Pool,
	tenantID uuid.UUID,
	key string,
	status int,
	body []byte,
) error {
	payload := json.RawMessage(body)
	if !json.Valid(payload) {
		payload = nil
	}
	return WithTenantTx(ctx, pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO idempotency_keys (tenant_id, key, response_code, response_body)
			 VALUES ($1, $2, $3, $4)
			 ON CONFLICT (tenant_id, key) DO NOTHING`,
			tenantID, key, status, payload,
		)
		return err
	})
}
