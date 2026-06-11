package bankfeed

import (
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
)

// fakeEncryptor is a deterministic reversible "cipher" — it prefixes the
// tenant id so a round-trip also proves the credential is bound to the
// right tenant. It is NOT cryptographic; it only exercises seal/open.
type fakeEncryptor struct{ failEncrypt, failDecrypt bool }

func (f fakeEncryptor) EncryptString(tenantID uuid.UUID, plaintext string) (string, error) {
	if f.failEncrypt {
		return "", fmt.Errorf("encrypt boom")
	}
	return "enc:" + tenantID.String() + ":" + plaintext, nil
}

func (f fakeEncryptor) DecryptString(tenantID uuid.UUID, value string) (string, error) {
	if f.failDecrypt {
		return "", fmt.Errorf("decrypt boom")
	}
	want := "enc:" + tenantID.String() + ":"
	if len(value) < len(want) || value[:len(want)] != want {
		return "", fmt.Errorf("ciphertext not bound to tenant %s", tenantID)
	}
	return value[len(want):], nil
}

func TestSealOpenRoundTrip(t *testing.T) {
	s := NewConnectionStore(nil, fakeEncryptor{}, nil)
	tn := uuid.New()
	sealed, err := s.seal(tn, "access-token-xyz")
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	raw, ok := sealed.([]byte)
	if !ok {
		t.Fatalf("seal returned %T; want []byte", sealed)
	}
	// Ciphertext must not contain the plaintext token.
	if string(raw) == "access-token-xyz" {
		t.Fatal("seal stored plaintext")
	}
	got, err := s.open(tn, raw)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if got != "access-token-xyz" {
		t.Fatalf("round-trip = %q; want original", got)
	}
}

func TestSealEmptyIsNull(t *testing.T) {
	s := NewConnectionStore(nil, fakeEncryptor{}, nil)
	sealed, err := s.seal(uuid.New(), "")
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	if sealed != nil {
		t.Fatalf("empty plaintext should seal to nil (SQL NULL); got %v", sealed)
	}
}

func TestOpenEmptyIsEmptyString(t *testing.T) {
	s := NewConnectionStore(nil, fakeEncryptor{}, nil)
	got, err := s.open(uuid.New(), nil)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if got != "" {
		t.Fatalf("open(nil) = %q; want empty", got)
	}
}

func TestSealOpenNilEncryptorPassthrough(t *testing.T) {
	// In dev (enc == nil) credentials are stored verbatim.
	s := NewConnectionStore(nil, nil, nil)
	tn := uuid.New()
	sealed, err := s.seal(tn, "plain")
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	if string(sealed.([]byte)) != "plain" {
		t.Fatalf("nil-enc seal = %v; want verbatim", sealed)
	}
	got, _ := s.open(tn, []byte("plain"))
	if got != "plain" {
		t.Fatalf("nil-enc open = %q; want verbatim", got)
	}
}

func TestSealCrossTenantCannotDecrypt(t *testing.T) {
	s := NewConnectionStore(nil, fakeEncryptor{}, nil)
	tenantA := uuid.New()
	tenantB := uuid.New()
	sealed, _ := s.seal(tenantA, "secret")
	// Opening tenant A's ciphertext under tenant B must fail — proves the
	// credential is cryptographically bound to its tenant.
	if _, err := s.open(tenantB, sealed.([]byte)); err == nil {
		t.Fatal("cross-tenant decrypt should fail")
	}
}

func TestSealPropagatesEncryptError(t *testing.T) {
	s := NewConnectionStore(nil, fakeEncryptor{failEncrypt: true}, nil)
	if _, err := s.seal(uuid.New(), "x"); err == nil {
		t.Fatal("expected encrypt error to propagate")
	}
}

// stubRow feeds canned column values into scanConnection.
type stubRow struct{ vals []any }

func (r stubRow) Scan(dest ...any) error {
	for i := range dest {
		switch d := dest[i].(type) {
		case *uuid.UUID:
			*d = r.vals[i].(uuid.UUID)
		case *string:
			*d = r.vals[i].(string)
		case *[]byte:
			if r.vals[i] == nil {
				*d = nil
			} else {
				*d = r.vals[i].([]byte)
			}
		case **string:
			if r.vals[i] == nil {
				*d = nil
			} else {
				v := r.vals[i].(string)
				*d = &v
			}
		case **time.Time:
			if r.vals[i] == nil {
				*d = nil
			} else {
				v := r.vals[i].(time.Time)
				*d = &v
			}
		case *time.Time:
			*d = r.vals[i].(time.Time)
		default:
			return fmt.Errorf("stubRow: unhandled dest %T at %d", dest[i], i)
		}
	}
	return nil
}

func TestScanConnectionDecryptsAndMapsNullables(t *testing.T) {
	s := NewConnectionStore(nil, fakeEncryptor{}, nil)
	tn := uuid.New()
	id := uuid.New()
	acct := uuid.New()
	now := time.Now().UTC()
	accessEnc, _ := s.seal(tn, "access-tok")
	// columns: tenant_id, id, bank_account_id, provider, access_enc,
	// refresh_enc, cursor, external_id, status, last_sync_at, last_error,
	// created_at, updated_at
	row := stubRow{vals: []any{
		tn, id, acct, ProviderPlaid,
		accessEnc.([]byte), nil, "cur-1", "ext-9", StatusActive,
		nil, nil, now, now,
	}}
	conn, err := s.scanConnection(tn, row)
	if err != nil {
		t.Fatalf("scanConnection: %v", err)
	}
	if conn.AccessToken != "access-tok" {
		t.Errorf("access token = %q; want decrypted", conn.AccessToken)
	}
	if conn.RefreshToken != "" {
		t.Errorf("refresh token = %q; want empty (NULL column)", conn.RefreshToken)
	}
	if conn.Cursor != "cur-1" || conn.ExternalID != "ext-9" {
		t.Errorf("nullable strings mismapped: %+v", conn)
	}
	if conn.LastSyncAt != nil {
		t.Errorf("LastSyncAt = %v; want nil", conn.LastSyncAt)
	}
}
