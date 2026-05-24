package imap

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

// fakeClient is a deterministic Client that records every call.
// Tests configure SelectResult / FetchAfter responses to drive
// the Poller through specific scenarios.
type fakeClient struct {
	mu sync.Mutex

	connectErr error
	loginErr   error
	selectRes  SelectResult
	selectErr  error
	fetchByCall [][]FetchedMessage
	fetchErr   error
	logoutErr  error

	connects, logins, selects, fetches, logouts int
}

func (f *fakeClient) Connect(_ context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.connects++
	return f.connectErr
}
func (f *fakeClient) Login(_ context.Context, _, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.logins++
	return f.loginErr
}
func (f *fakeClient) Select(_ context.Context, _ Folder) (SelectResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.selects++
	return f.selectRes, f.selectErr
}
func (f *fakeClient) FetchAfter(_ context.Context, _ uint32, _ int) ([]FetchedMessage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fetchErr != nil {
		return nil, f.fetchErr
	}
	if f.fetches >= len(f.fetchByCall) {
		f.fetches++
		return nil, nil
	}
	batch := f.fetchByCall[f.fetches]
	f.fetches++
	return batch, nil
}
func (f *fakeClient) Logout(_ context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.logouts++
	return f.logoutErr
}

// fakeUIDState is an in-memory UIDState for tests.
type fakeUIDState struct {
	mu sync.Mutex

	uidValidity uint32
	lastUID     uint32
	consecutive int
	lastErrMsg  string
	getErr      error
	setErr      error

	setCalls    int
	recordCalls int
	clearCalls  int
}

func (s *fakeUIDState) Get(_ context.Context, _, _ uuid.UUID) (uidValidity, lastUID uint32, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.uidValidity, s.lastUID, s.getErr
}
func (s *fakeUIDState) Set(_ context.Context, _, _ uuid.UUID, uv, lu uint32) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.setCalls++
	if s.setErr != nil {
		return s.setErr
	}
	s.uidValidity = uv
	s.lastUID = lu
	s.consecutive = 0
	s.lastErrMsg = ""
	return nil
}
func (s *fakeUIDState) RecordError(_ context.Context, _, _ uuid.UUID, m string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.recordCalls++
	s.consecutive++
	s.lastErrMsg = m
	return nil
}
func (s *fakeUIDState) ClearError(_ context.Context, _, _ uuid.UUID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.clearCalls++
	s.consecutive = 0
	s.lastErrMsg = ""
	return nil
}

// fakeProcessor records every Process call.
type fakeProcessor struct {
	mu     sync.Mutex
	calls  int
	emails []ParsedEmail
	err    error
}

func (p *fakeProcessor) Process(_ context.Context, _, _ uuid.UUID, _ []byte, parsed ParsedEmail) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	p.emails = append(p.emails, parsed)
	return p.err
}

func mkMessage(uid uint32, msgID string) FetchedMessage {
	body := fmt.Sprintf("From: c@x\r\nTo: s@y\r\nSubject: t-%d\r\nMessage-ID: <%s>\r\n\r\nbody-%d\r\n",
		uid, msgID, uid)
	return FetchedMessage{UID: uid, Body: []byte(body), SeenAt: time.Unix(int64(uid), 0)}
}

// TestNewPoller_ValidationErrors pins constructor validation.
func TestNewPoller_ValidationErrors(t *testing.T) {
	cfg := Config{TenantID: uuid.New(), MailboxID: uuid.New()}
	if _, err := NewPoller(cfg, nil, &fakeUIDState{}, &fakeProcessor{}, nil); err == nil {
		t.Errorf("expected error for nil client")
	}
	if _, err := NewPoller(cfg, &fakeClient{}, nil, &fakeProcessor{}, nil); err == nil {
		t.Errorf("expected error for nil state")
	}
	if _, err := NewPoller(cfg, &fakeClient{}, &fakeUIDState{}, nil, nil); err == nil {
		t.Errorf("expected error for nil processor")
	}
	if _, err := NewPoller(Config{MailboxID: uuid.New()}, &fakeClient{}, &fakeUIDState{}, &fakeProcessor{}, nil); err == nil {
		t.Errorf("expected error for nil tenant id")
	}
	if _, err := NewPoller(Config{TenantID: uuid.New()}, &fakeClient{}, &fakeUIDState{}, &fakeProcessor{}, nil); err == nil {
		t.Errorf("expected error for nil mailbox id")
	}
}

// TestPollOnce_HappyPath pins the canonical happy flow:
// connect \u2192 login \u2192 select \u2192 fetch \u2192 process \u2192 advance \u2192 logout.
func TestPollOnce_HappyPath(t *testing.T) {
	client := &fakeClient{
		selectRes: SelectResult{UIDValidity: 100, UIDNext: 10, Exists: 3},
		fetchByCall: [][]FetchedMessage{
			{mkMessage(1, "m1@x"), mkMessage(2, "m2@x")},
		},
	}
	state := &fakeUIDState{uidValidity: 100, lastUID: 0}
	processor := &fakeProcessor{}
	p, err := NewPoller(Config{
		TenantID: uuid.New(), MailboxID: uuid.New(),
		Folder: "INBOX", PollInterval: time.Second,
	}, client, state, processor, nil)
	if err != nil {
		t.Fatalf("NewPoller: %v", err)
	}
	n, err := p.PollOnce(context.Background())
	if err != nil {
		t.Fatalf("PollOnce: %v", err)
	}
	if n != 2 {
		t.Errorf("expected 2 processed, got %d", n)
	}
	if client.connects != 1 || client.logins != 1 || client.selects != 1 || client.logouts != 1 {
		t.Errorf("expected single connect/login/select/logout, got %+v", client)
	}
	if processor.calls != 2 {
		t.Errorf("expected 2 processor calls, got %d", processor.calls)
	}
	if state.lastUID != 2 {
		t.Errorf("expected lastUID advanced to 2, got %d", state.lastUID)
	}
}

// TestPollOnce_UIDValidityChangeResets pins the UIDVALIDITY
// reset path: a server-side validity change wipes the checkpoint
// and re-scans from 0.
func TestPollOnce_UIDValidityChangeResets(t *testing.T) {
	client := &fakeClient{
		selectRes: SelectResult{UIDValidity: 200, UIDNext: 100, Exists: 50},
		fetchByCall: [][]FetchedMessage{
			{mkMessage(1, "m@x")},
		},
	}
	// State has old validity (100) and high last_uid (99).
	state := &fakeUIDState{uidValidity: 100, lastUID: 99}
	p, _ := NewPoller(Config{
		TenantID: uuid.New(), MailboxID: uuid.New(),
	}, client, state, &fakeProcessor{}, nil)
	_, err := p.PollOnce(context.Background())
	if err != nil {
		t.Fatalf("PollOnce: %v", err)
	}
	// lastUID must have been reset (otherwise the test's UID=1
	// message wouldn't be fetched because 1 < 99).
	if state.uidValidity != 200 {
		t.Errorf("expected uidValidity updated to 200, got %d", state.uidValidity)
	}
	if state.lastUID != 1 {
		t.Errorf("expected lastUID advanced to 1 (from reset 0), got %d", state.lastUID)
	}
}

// TestPollOnce_ErrAuthReturnsSentinel pins that ErrAuth
// surfaces unwrapped so the Manager can fast-fail without
// retry-with-backoff.
func TestPollOnce_ErrAuthReturnsSentinel(t *testing.T) {
	client := &fakeClient{loginErr: ErrAuth}
	state := &fakeUIDState{}
	p, _ := NewPoller(Config{
		TenantID: uuid.New(), MailboxID: uuid.New(),
	}, client, state, &fakeProcessor{}, nil)
	_, err := p.PollOnce(context.Background())
	if !errors.Is(err, ErrAuth) {
		t.Errorf("expected ErrAuth, got %v", err)
	}
}

// TestPollOnce_ConnectErrorShortCircuits pins that a connect
// failure surfaces immediately without calling Login / Select.
func TestPollOnce_ConnectErrorShortCircuits(t *testing.T) {
	client := &fakeClient{connectErr: errors.New("dial failed")}
	p, _ := NewPoller(Config{
		TenantID: uuid.New(), MailboxID: uuid.New(),
	}, client, &fakeUIDState{}, &fakeProcessor{}, nil)
	_, err := p.PollOnce(context.Background())
	if err == nil {
		t.Fatalf("expected error")
	}
	if client.logins != 0 || client.selects != 0 {
		t.Errorf("expected no login/select after connect failure")
	}
}

// TestPollOnce_ParseFailureAdvancesPastBadMessage pins the
// "don't get stuck on a malformed message" contract: a parse
// error advances lastUID past the offending UID so the mailbox
// keeps draining.
func TestPollOnce_ParseFailureAdvancesPastBadMessage(t *testing.T) {
	bad := FetchedMessage{UID: 5, Body: []byte("garbage")}
	good := mkMessage(6, "good@x")
	client := &fakeClient{
		selectRes:   SelectResult{UIDValidity: 1},
		fetchByCall: [][]FetchedMessage{{bad, good}},
	}
	state := &fakeUIDState{uidValidity: 1}
	processor := &fakeProcessor{}
	p, _ := NewPoller(Config{
		TenantID: uuid.New(), MailboxID: uuid.New(),
	}, client, state, processor, nil)
	n, err := p.PollOnce(context.Background())
	if err != nil {
		t.Fatalf("PollOnce: %v", err)
	}
	if n != 1 {
		t.Errorf("expected 1 processed (the good one), got %d", n)
	}
	if state.lastUID != 6 {
		t.Errorf("expected lastUID advanced past the bad message to 6, got %d", state.lastUID)
	}
}

// TestPollOnce_ProcessorErrorAbortsBatch pins that a Processor
// failure stops the poll mid-batch (so the Manager retries with
// backoff). The state's lastUID is NOT advanced past the failing
// UID \u2014 the next poll re-fetches it.
func TestPollOnce_ProcessorErrorAbortsBatch(t *testing.T) {
	client := &fakeClient{
		selectRes:   SelectResult{UIDValidity: 1},
		fetchByCall: [][]FetchedMessage{{mkMessage(1, "m@x"), mkMessage(2, "n@x")}},
	}
	state := &fakeUIDState{uidValidity: 1}
	processor := &fakeProcessor{err: errors.New("db outage")}
	p, _ := NewPoller(Config{
		TenantID: uuid.New(), MailboxID: uuid.New(),
	}, client, state, processor, nil)
	_, err := p.PollOnce(context.Background())
	if err == nil {
		t.Fatalf("expected processor error")
	}
	// First message was processed (called) but failed. Second
	// message was never reached.
	if processor.calls != 1 {
		t.Errorf("expected 1 processor call before abort, got %d", processor.calls)
	}
	// state.Set was NOT called for the failing batch (lastUID
	// stays at 0).
	if state.setCalls != 0 {
		t.Errorf("expected zero Set calls on processor failure, got %d", state.setCalls)
	}
}

// TestPollOnce_MultiBatchDrains pins the loop-until-empty
// contract: a mailbox with more messages than FetchBatchSize is
// drained across multiple FetchAfter calls.
func TestPollOnce_MultiBatchDrains(t *testing.T) {
	batch1 := []FetchedMessage{mkMessage(1, "a@x"), mkMessage(2, "b@x")}
	batch2 := []FetchedMessage{mkMessage(3, "c@x")}
	client := &fakeClient{
		selectRes:   SelectResult{UIDValidity: 1},
		fetchByCall: [][]FetchedMessage{batch1, batch2, {}},
	}
	state := &fakeUIDState{uidValidity: 1}
	processor := &fakeProcessor{}
	p, _ := NewPoller(Config{
		TenantID: uuid.New(), MailboxID: uuid.New(),
		FetchBatchSize: 2,
	}, client, state, processor, nil)
	n, err := p.PollOnce(context.Background())
	if err != nil {
		t.Fatalf("PollOnce: %v", err)
	}
	if n != 3 {
		t.Errorf("expected 3 processed across 2 batches, got %d", n)
	}
	if state.lastUID != 3 {
		t.Errorf("expected lastUID = 3, got %d", state.lastUID)
	}
}

// TestRun_BackoffOnError pins exponential backoff: an error
// triggers RecordError + a sleep before the next attempt.
func TestRun_BackoffOnError(t *testing.T) {
	client := &fakeClient{
		connectErr: errors.New("dial failed"),
	}
	state := &fakeUIDState{}
	processor := &fakeProcessor{}
	p, _ := NewPoller(Config{
		TenantID: uuid.New(), MailboxID: uuid.New(),
		PollInterval: 10 * time.Millisecond,
		MaxBackoff:   100 * time.Millisecond,
	}, client, state, processor, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	_ = p.Run(ctx)
	// At least one error was recorded.
	if state.recordCalls < 1 {
		t.Errorf("expected at least 1 RecordError, got %d", state.recordCalls)
	}
}

// TestRun_ErrAuthExits pins that an ErrAuth causes Run to
// return immediately (no retry-with-backoff).
func TestRun_ErrAuthExits(t *testing.T) {
	client := &fakeClient{loginErr: ErrAuth}
	state := &fakeUIDState{}
	processor := &fakeProcessor{}
	p, _ := NewPoller(Config{
		TenantID: uuid.New(), MailboxID: uuid.New(),
		PollInterval: 10 * time.Millisecond,
	}, client, state, processor, nil)
	done := make(chan error, 1)
	go func() {
		done <- p.Run(context.Background())
	}()
	select {
	case err := <-done:
		if !errors.Is(err, ErrAuth) {
			t.Errorf("expected ErrAuth exit, got %v", err)
		}
	case <-time.After(200 * time.Millisecond):
		t.Errorf("Run should have exited on ErrAuth")
	}
}

// TestManager_StartAndStop pins the supervised lifecycle:
// Start spawns a goroutine, Stop cancels it.
func TestManager_StartAndStop(t *testing.T) {
	mailboxID := uuid.New()
	pollerRan := make(chan struct{}, 1)
	m := NewManager(func(cfg Config) (*Poller, error) {
		client := &fakeClient{
			connectErr: errors.New("infinite dial fail"),
		}
		return NewPoller(cfg, client, &fakeUIDState{}, &fakeProcessor{}, nil)
	}, nil)
	defer m.StopAll()

	go func() {
		_ = m.Start(context.Background(), Config{
			TenantID:     uuid.New(),
			MailboxID:    mailboxID,
			PollInterval: 5 * time.Millisecond,
			MaxBackoff:   10 * time.Millisecond,
		})
		pollerRan <- struct{}{}
	}()

	// Wait briefly for the Start to register.
	time.Sleep(20 * time.Millisecond)
	active := m.ActiveMailboxes()
	if len(active) != 1 || active[0] != mailboxID {
		t.Errorf("expected one active mailbox, got %v", active)
	}

	// Stop returns true the first time, false on repeat.
	if !m.Stop(mailboxID) {
		t.Errorf("expected Stop to return true")
	}
	// Give the goroutine a moment to clean up.
	time.Sleep(20 * time.Millisecond)
	if m.Stop(mailboxID) {
		t.Errorf("expected Stop to return false on repeat")
	}
}

// TestManager_DuplicateStartIsNoop pins idempotent Start: calling
// Start twice for the same mailbox does NOT spawn two goroutines.
func TestManager_DuplicateStartIsNoop(t *testing.T) {
	mailboxID := uuid.New()
	called := 0
	m := NewManager(func(cfg Config) (*Poller, error) {
		called++
		return NewPoller(cfg, &fakeClient{}, &fakeUIDState{}, &fakeProcessor{}, nil)
	}, nil)
	defer m.StopAll()
	cfg := Config{
		TenantID: uuid.New(), MailboxID: mailboxID,
		PollInterval: time.Hour, // effectively never fires
	}
	if err := m.Start(context.Background(), cfg); err != nil {
		t.Fatalf("Start 1: %v", err)
	}
	if err := m.Start(context.Background(), cfg); err != nil {
		t.Fatalf("Start 2: %v", err)
	}
	if called != 1 {
		t.Errorf("expected newPoller called once, got %d", called)
	}
}

// TestPollOnce_UIDValidityResetOnEmptyMailboxPersists pins the
// fix for the "UIDVALIDITY reset on empty mailbox spams the log
// forever" regression: when validity changes and no messages
// are present, the new validity MUST be persisted immediately
// so the next poll doesn't re-detect the mismatch.
func TestPollOnce_UIDValidityResetOnEmptyMailboxPersists(t *testing.T) {
	client := &fakeClient{
		selectRes: SelectResult{UIDValidity: 200, UIDNext: 1, Exists: 0},
		// Empty mailbox: FetchAfter returns no messages.
		fetchByCall: [][]FetchedMessage{},
	}
	state := &fakeUIDState{uidValidity: 100, lastUID: 99}
	processor := &fakeProcessor{}
	p, _ := NewPoller(Config{
		TenantID: uuid.New(), MailboxID: uuid.New(),
		PollInterval: time.Second,
	}, client, state, processor, nil)

	processed, err := p.PollOnce(context.Background())
	if err != nil {
		t.Fatalf("PollOnce: %v", err)
	}
	if processed != 0 {
		t.Errorf("expected 0 processed, got %d", processed)
	}
	// The new validity MUST be persisted even though the
	// mailbox was empty. Without the fix, state.uidValidity
	// stays at 100 and every subsequent poll re-warns.
	if state.uidValidity != 200 {
		t.Errorf("expected uidValidity persisted as 200, got %d", state.uidValidity)
	}
	if state.lastUID != 0 {
		t.Errorf("expected lastUID reset to 0, got %d", state.lastUID)
	}
	if state.setCalls < 1 {
		t.Errorf("expected at least 1 Set call (persist new validity), got %d", state.setCalls)
	}
}

// TestManager_RestartAfterStopPreservesNewGoroutine pins the
// fix for the goroutine-leak race in Manager.Start: when a
// Stop -> Start sequence runs on the same mailbox and the
// first goroutine hasn't yet finished its deferred cleanup,
// G1's defer-delete must NOT wipe G2's map entry.
//
// Repro: Start G1 with a poller that exits immediately after
// ctx is cancelled. Stop the mailbox (causing G1 to start
// shutting down). Immediately Start the same mailbox again
// (registering G2 before G1's defer fires). Wait for G1 to
// finish, then verify G2 is still tracked + StopAll terminates
// cleanly.
func TestManager_RestartAfterStopPreservesNewGoroutine(t *testing.T) {
	mailboxID := uuid.New()
	// Use a slow-Connect poller so we can sequence Stop +
	// Start before the goroutine reaches Run's main loop.
	makePoller := func(cfg Config) (*Poller, error) {
		client := &fakeClient{
			// Block in Connect until ctx is cancelled, then
			// return ctx.Err(). This guarantees PollOnce
			// exits as soon as Stop fires.
			connectErr: errors.New("test: connect blocked"),
		}
		return NewPoller(cfg, client, &fakeUIDState{}, &fakeProcessor{}, nil)
	}
	m := NewManager(makePoller, nil)

	cfg := Config{
		TenantID: uuid.New(), MailboxID: mailboxID,
		PollInterval: 5 * time.Millisecond,
		MaxBackoff:   10 * time.Millisecond,
	}
	if err := m.Start(context.Background(), cfg); err != nil {
		t.Fatalf("Start G1: %v", err)
	}
	// Let G1 enter Run + start backing off.
	time.Sleep(20 * time.Millisecond)
	if !m.Stop(mailboxID) {
		t.Fatalf("Stop G1 returned false")
	}
	// IMMEDIATELY restart -- G1's defer-cleanup is racing
	// against this Start. Without the gen-token fix, G1's
	// defer would wipe G2's entry once G1's Run returns.
	if err := m.Start(context.Background(), cfg); err != nil {
		t.Fatalf("Start G2: %v", err)
	}
	// Give both goroutines time to settle: G1 to finish its
	// defer, G2 to register + enter Run.
	time.Sleep(50 * time.Millisecond)

	active := m.ActiveMailboxes()
	if len(active) != 1 || active[0] != mailboxID {
		t.Fatalf("expected exactly G2 active for %s, got %v", mailboxID, active)
	}

	// StopAll MUST terminate cleanly. With the bug, G2's
	// entry would be missing (deleted by G1's stale defer),
	// so the cancel call would never reach G2 -- but G2's
	// wg.Add(1) is still pending, so wg.Wait() would hang.
	// We gate on a deadline to catch the hang as a test
	// failure rather than a timeout.
	done := make(chan struct{})
	go func() {
		m.StopAll()
		close(done)
	}()
	select {
	case <-done:
		// success
	case <-time.After(2 * time.Second):
		t.Fatalf("StopAll hung -- goroutine leak indicates the Manager race regressed")
	}
}

// TestManager_StopAllWaitsForExit pins clean shutdown: StopAll
// returns AFTER every Poller goroutine has finished.
func TestManager_StopAllWaitsForExit(t *testing.T) {
	m := NewManager(func(cfg Config) (*Poller, error) {
		client := &fakeClient{connectErr: errors.New("never connects")}
		return NewPoller(cfg, client, &fakeUIDState{}, &fakeProcessor{}, nil)
	}, nil)
	for i := 0; i < 3; i++ {
		_ = m.Start(context.Background(), Config{
			TenantID: uuid.New(), MailboxID: uuid.New(),
			PollInterval: 5 * time.Millisecond,
			MaxBackoff:   10 * time.Millisecond,
		})
	}
	time.Sleep(20 * time.Millisecond)
	if len(m.ActiveMailboxes()) != 3 {
		t.Errorf("expected 3 active, got %d", len(m.ActiveMailboxes()))
	}
	m.StopAll()
	if len(m.ActiveMailboxes()) != 0 {
		t.Errorf("expected 0 active after StopAll, got %d", len(m.ActiveMailboxes()))
	}
}

// TestManager_StartAfterStopAllReturnsErrStopped pins the
// round-4 hardening: once StopAll() has been called, every
// subsequent Start() refuses with ErrManagerStopped instead of
// spawning a goroutine that StopAll can no longer track. Without
// this guard, a late-arriving convergence tick could race the
// shutdown and leave a wg.Add(1) outstanding after StopAll's
// wg.Wait() has already returned (or worse, block StopAll from
// ever returning if Start interleaved between the entries reset
// and wg.Wait).
func TestManager_StartAfterStopAllReturnsErrStopped(t *testing.T) {
	m := NewManager(func(cfg Config) (*Poller, error) {
		client := &fakeClient{connectErr: errors.New("never connects")}
		return NewPoller(cfg, client, &fakeUIDState{}, &fakeProcessor{}, nil)
	}, nil)
	// Start one + Stop everything.
	if err := m.Start(context.Background(), Config{
		TenantID: uuid.New(), MailboxID: uuid.New(),
		PollInterval: 5 * time.Millisecond,
		MaxBackoff:   10 * time.Millisecond,
	}); err != nil {
		t.Fatalf("initial start: %v", err)
	}
	m.StopAll()
	// Post-shutdown Start should refuse.
	err := m.Start(context.Background(), Config{
		TenantID: uuid.New(), MailboxID: uuid.New(),
		PollInterval: 5 * time.Millisecond,
	})
	if !errors.Is(err, ErrManagerStopped) {
		t.Fatalf("expected ErrManagerStopped after StopAll, got %v", err)
	}
	// And it must not have populated the entries map.
	if active := m.ActiveMailboxes(); len(active) != 0 {
		t.Fatalf("expected no active mailboxes after refused start, got %d", len(active))
	}
}

// TestManager_IsActiveReflectsRunningSet pins the supervisor's
// "skip Start if already running" optimisation: IsActive must
// return true between Start and Stop for a mailbox, false
// before Start, and false after Stop.
func TestManager_IsActiveReflectsRunningSet(t *testing.T) {
	mailboxID := uuid.New()
	m := NewManager(func(cfg Config) (*Poller, error) {
		return NewPoller(cfg, &fakeClient{}, &fakeUIDState{}, &fakeProcessor{}, nil)
	}, nil)
	if m.IsActive(mailboxID) {
		t.Fatalf("expected IsActive=false before Start")
	}
	if err := m.Start(context.Background(), Config{
		TenantID: uuid.New(), MailboxID: mailboxID,
		PollInterval: time.Hour, // never fires
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if !m.IsActive(mailboxID) {
		t.Fatalf("expected IsActive=true after Start")
	}
	if !m.Stop(mailboxID) {
		t.Fatalf("Stop returned false for active mailbox")
	}
	// Stop signals the goroutine; the deferred entry-delete races
	// the goroutine's exit. Poll a few ticks before declaring failure
	// so the deferred cleanup has a chance to run on slow CI.
	deadline := time.Now().Add(500 * time.Millisecond)
	for m.IsActive(mailboxID) && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if m.IsActive(mailboxID) {
		t.Fatalf("expected IsActive=false after Stop within deadline")
	}
	m.StopAll()
}

// TestPollOnce_ProcessorErrorPersistsPartialProgress pins the
// round-4 fix: if processor.Process fails on UID N+1 after UID N
// succeeded, the checkpoint is persisted at UID N before the
// error returns so the next poll doesn't re-fetch + re-process
// UID N. The Processor's Message-ID dedup catches duplicates if
// the next poll re-fetches UID N+1, so this strictly reduces
// duplicate-processing load without changing correctness.
func TestPollOnce_ProcessorErrorPersistsPartialProgress(t *testing.T) {
	client := &fakeClient{
		selectRes:   SelectResult{UIDValidity: 1},
		fetchByCall: [][]FetchedMessage{{mkMessage(10, "a@x"), mkMessage(11, "b@x")}},
	}
	state := &fakeUIDState{uidValidity: 1}
	// Processor succeeds on first call (UID 10), fails on
	// second (UID 11).
	processor := &fakeProcessorSeq{errAt: 2}
	p, _ := NewPoller(Config{
		TenantID: uuid.New(), MailboxID: uuid.New(),
	}, client, state, processor, nil)
	_, err := p.PollOnce(context.Background())
	if err == nil {
		t.Fatalf("expected processor error on UID 11")
	}
	// state.Set must have been called once on the error path
	// (NOT zero like the all-fail batch case) with lastUID=10.
	if state.setCalls != 1 {
		t.Errorf("expected 1 Set on partial-progress error, got %d", state.setCalls)
	}
	if state.lastUID != 10 {
		t.Errorf("expected lastUID=10 persisted, got %d", state.lastUID)
	}
}

// fakeProcessorSeq returns nil on the first errAt-1 calls then
// an error on call errAt. Used to exercise the
// "succeed-then-fail" partial-batch path.
type fakeProcessorSeq struct {
	calls int
	errAt int
}

func (f *fakeProcessorSeq) Process(_ context.Context, _, _ uuid.UUID, _ []byte, _ ParsedEmail) error {
	f.calls++
	if f.calls == f.errAt {
		return errors.New("db outage")
	}
	return nil
}
