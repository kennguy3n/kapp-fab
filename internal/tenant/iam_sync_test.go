package tenant

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
)

// fakeProvisioner is a minimal IAMProvisioner whose ProvisionUser/Tenant
// return preset ids so the orchestrator's persistence-race handling can
// be exercised without a live iam-core.
type fakeProvisioner struct {
	tenantID string
	clientID string
	userID   string
}

func (f *fakeProvisioner) ProvisionTenant(_ context.Context, _ uuid.UUID, _, _ string) (string, error) {
	return f.tenantID, nil
}

func (f *fakeProvisioner) ProvisionWebApplication(_ context.Context, _, _ string) (string, error) {
	return f.clientID, nil
}

func (f *fakeProvisioner) ProvisionUser(_ context.Context, _ string, _ uuid.UUID, _, _ string) (string, error) {
	return f.userID, nil
}

// fakeUserStore models the users table for the race: SetIAMUserID
// reports the column as already mapped (the winner of a concurrent
// run), and GetUser returns that winner's mapping.
type fakeUserStore struct {
	existing  *User
	setErr    error
	getCalled bool
}

func (s *fakeUserStore) GetUser(_ context.Context, _ uuid.UUID) (*User, error) {
	s.getCalled = true
	return s.existing, nil
}

func (s *fakeUserStore) SetIAMUserID(_ context.Context, _ uuid.UUID, _ string) error {
	return s.setErr
}

// TestSyncUser_RaceReReadsWinnerMapping proves SyncUser mirrors
// EnsureTenant: when SetIAMUserID loses the persist race
// (ErrIAMUserAlreadyMapped) it re-reads the row and returns the
// winner's iam_user_id instead of bubbling the error up and aborting
// onboarding.
func TestSyncUser_RaceReReadsWinnerMapping(t *testing.T) {
	userID := uuid.New()
	winner := &User{ID: userID, IAMUserID: "iam-winner-123"}
	users := &fakeUserStore{existing: winner, setErr: ErrIAMUserAlreadyMapped}

	sync, err := NewIAMSync(&fakeProvisioner{userID: "iam-loser-456"}, &fakeTenantStore{}, users, nil)
	if err != nil {
		t.Fatalf("NewIAMSync: %v", err)
	}

	got, err := sync.SyncUser(context.Background(), "iam-tenant", userID, "u@example.com", "U")
	if err != nil {
		t.Fatalf("SyncUser should swallow the race, got error: %v", err)
	}
	if got != "iam-winner-123" {
		t.Fatalf("SyncUser returned %q, want the winner's mapping %q", got, "iam-winner-123")
	}
	if !users.getCalled {
		t.Fatal("SyncUser did not re-read the existing mapping after the race")
	}
}

// TestSyncUser_HappyPathReturnsFreshMapping confirms the non-race path
// returns the freshly provisioned id and never re-reads.
func TestSyncUser_HappyPathReturnsFreshMapping(t *testing.T) {
	userID := uuid.New()
	users := &fakeUserStore{setErr: nil}

	sync, err := NewIAMSync(&fakeProvisioner{userID: "iam-fresh-789"}, &fakeTenantStore{}, users, nil)
	if err != nil {
		t.Fatalf("NewIAMSync: %v", err)
	}

	got, err := sync.SyncUser(context.Background(), "iam-tenant", userID, "u@example.com", "U")
	if err != nil {
		t.Fatalf("SyncUser: %v", err)
	}
	if got != "iam-fresh-789" {
		t.Fatalf("SyncUser returned %q, want %q", got, "iam-fresh-789")
	}
	if users.getCalled {
		t.Fatal("SyncUser re-read the mapping on the happy path; it should not")
	}
}

// TestSyncUser_NonRaceErrorPropagates confirms a genuine persist error
// (not the race sentinel) still aborts.
func TestSyncUser_NonRaceErrorPropagates(t *testing.T) {
	userID := uuid.New()
	boom := errors.New("db down")
	users := &fakeUserStore{setErr: boom}

	sync, err := NewIAMSync(&fakeProvisioner{userID: "iam-x"}, &fakeTenantStore{}, users, nil)
	if err != nil {
		t.Fatalf("NewIAMSync: %v", err)
	}

	if _, err := sync.SyncUser(context.Background(), "iam-tenant", userID, "u@example.com", "U"); !errors.Is(err, boom) {
		t.Fatalf("SyncUser should propagate non-race errors, got: %v", err)
	}
	if users.getCalled {
		t.Fatal("SyncUser re-read the mapping on a non-race error; it should not")
	}
}

// fakeTenantStore is an unused-by-these-tests iamTenantStore stub so
// NewIAMSync's required-arg checks pass.
type fakeTenantStore struct{}

func (fakeTenantStore) Get(_ context.Context, _ uuid.UUID) (*Tenant, error) { return &Tenant{}, nil }
func (fakeTenantStore) SetIAMTenantID(_ context.Context, _ uuid.UUID, _ string) error {
	return nil
}
