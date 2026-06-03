package auth_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kennguy3n/kapp-fab/internal/auth"
)

func newRotationSigner(t *testing.T) *auth.Signer {
	t.Helper()
	signer, err := auth.NewSigner(auth.SignerConfig{
		Algorithm:  auth.AlgHS256,
		HMACKey:    []byte("rotation-test-secret-key-at-least-32-bytes-long"),
		Issuer:     "kapp",
		Audience:   "kapp",
		AccessTTL:  15 * time.Minute,
		RefreshTTL: 24 * time.Hour,
		Leeway:     30 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	return signer
}

// seedSession creates a live session and mints the matching refresh
// token whose jti equals the session's refresh_jti, mirroring the
// Exchange path.
func seedSession(t *testing.T, signer *auth.Signer, store *memSessionStore) (session auth.Session, refreshToken string) {
	t.Helper()
	tenantID := uuid.New()
	userID := uuid.New()
	sess, err := store.Create(context.Background(), auth.Session{
		TenantID:   tenantID,
		UserID:     userID,
		RefreshJTI: uuid.NewString(),
		ExpiresAt:  time.Now().Add(24 * time.Hour),
	})
	if err != nil {
		t.Fatalf("seed session: %v", err)
	}
	refresh, err := signer.IssueRefresh(auth.Claims{
		UserID:    userID,
		TenantID:  tenantID,
		SessionID: sess.ID,
		JWTID:     sess.RefreshJTI,
	})
	if err != nil {
		t.Fatalf("issue refresh: %v", err)
	}
	return *sess, refresh
}

// TestRefresh_RotatesToken verifies the happy path: a refresh consumes
// the presented token and returns a DIFFERENT one, and the original is
// then dead (single-use).
func TestRefresh_RotatesToken(t *testing.T) {
	signer := newRotationSigner(t)
	store := &memSessionStore{sessions: map[uuid.UUID]*auth.Session{}}
	svc := auth.NewSSOService(nil, signer, store, nil, nil)
	_, refresh := seedSession(t, signer, store)

	res, err := svc.Refresh(context.Background(), refresh)
	if err != nil {
		t.Fatalf("first refresh failed: %v", err)
	}
	if res.RefreshToken == "" || res.RefreshToken == refresh {
		t.Fatalf("refresh did not rotate the token (got %q, original %q)", res.RefreshToken, refresh)
	}
	// The rotated token works for a subsequent refresh.
	if _, err := svc.Refresh(context.Background(), res.RefreshToken); err != nil {
		t.Fatalf("rotated token rejected on next refresh: %v", err)
	}
}

// TestRefresh_ReplayRevokesFamily is the core reuse-detection test: a
// SECOND refresh with the already-consumed token must (a) fail and (b)
// revoke the entire session family, so even the legitimately-rotated
// token stops working afterwards.
func TestRefresh_ReplayRevokesFamily(t *testing.T) {
	signer := newRotationSigner(t)
	store := &memSessionStore{sessions: map[uuid.UUID]*auth.Session{}}
	svc := auth.NewSSOService(nil, signer, store, nil, nil)
	sess, refresh := seedSession(t, signer, store)

	// Legitimate first use → rotates, returns a fresh token.
	res, err := svc.Refresh(context.Background(), refresh)
	if err != nil {
		t.Fatalf("first refresh failed: %v", err)
	}

	// Replay the original (now-consumed) token → reuse detected.
	if _, err := svc.Refresh(context.Background(), refresh); err == nil {
		t.Fatal("replay of consumed refresh token succeeded; want reuse error")
	} else if !errors.Is(err, auth.ErrRefreshReuse) {
		t.Fatalf("replay error = %v, want ErrRefreshReuse", err)
	}

	// Family revoked: the session is gone …
	if _, err := store.Get(context.Background(), sess.TenantID, sess.ID); !errors.Is(err, auth.ErrSessionNotFound) {
		t.Fatalf("session Get after reuse = %v, want ErrSessionNotFound (family revoked)", err)
	}
	// … and the legitimately-rotated token no longer works either.
	if _, err := svc.Refresh(context.Background(), res.RefreshToken); err == nil {
		t.Fatal("rotated token still valid after family revocation; want rejection")
	}
}
