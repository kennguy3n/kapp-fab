package runtime

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// fixedClock returns a clock that always reports the supplied time.
// Used in tests to make signatures deterministic.
func fixedClock(t time.Time) func() time.Time {
	return func() time.Time { return t }
}

// validLifecycleDispatch returns a minimally-valid LifecycleDispatch
// for tests. Callers override the Phase / Body / WebhookBase as
// needed.
func validLifecycleDispatch(t *testing.T) *LifecycleDispatch {
	t.Helper()
	secret, err := GenerateSigningSecret()
	if err != nil {
		t.Fatalf("generate secret: %v", err)
	}
	return &LifecycleDispatch{
		TenantID:           uuid.New(),
		InstallationID:     uuid.New(),
		ExtensionID:        uuid.New(),
		ExtensionVersionID: uuid.New(),
		Phase:              PhasePreInstall,
		WebhookBase:        "https://publisher.example.test",
		SigningSecret:      secret,
		Body:               []byte(`{"hello":"world"}`),
	}
}

func TestNoopHooks_AlwaysSucceeds(t *testing.T) {
	hooks := NoopHooks()
	res, err := hooks.Dispatch(context.Background(), validLifecycleDispatch(t))
	if err != nil {
		t.Fatalf("noop dispatch: %v", err)
	}
	if res.Status != 200 || res.Aborted {
		t.Fatalf("noop result = %+v, want status=200 aborted=false", res)
	}
}

func TestNoopHooks_RejectsBadDispatch(t *testing.T) {
	hooks := NoopHooks()
	_, err := hooks.Dispatch(context.Background(), &LifecycleDispatch{})
	if err == nil {
		t.Fatal("noop should reject empty dispatch")
	}
}

func TestTransportHooks_PreInstall_2xx_NotAborted(t *testing.T) {
	tr := &InMemoryTransport{Handler: StaticResponseHandler(200, []byte(`{"ok":true}`))}
	hooks := NewTransportHooks(tr, nil, fixedClock(time.Unix(1700000000, 0).UTC()))

	in := validLifecycleDispatch(t)
	in.Phase = PhasePreInstall

	res, err := hooks.Dispatch(context.Background(), in)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if res.Aborted {
		t.Fatalf("2xx pre_install should not abort: %+v", res)
	}
	if res.Status != 200 || res.Attempt != 1 {
		t.Fatalf("res = %+v want status=200 attempt=1", res)
	}
	if got := tr.Len(); got != 1 {
		t.Fatalf("audit len = %d, want 1", got)
	}
	entry := tr.At(0)
	if !strings.HasSuffix(entry.Target, "/lifecycle/pre_install") {
		t.Fatalf("target = %q, want suffix /lifecycle/pre_install", entry.Target)
	}
	if entry.Headers[SignatureHeaderName] == "" {
		t.Fatalf("audit lacks signature header")
	}
}

func TestTransportHooks_PreInstall_404_NotAborted(t *testing.T) {
	tr := &InMemoryTransport{Handler: StaticResponseHandler(404, nil)}
	hooks := NewTransportHooks(tr, nil, fixedClock(time.Unix(1700000000, 0).UTC()))

	in := validLifecycleDispatch(t)
	in.Phase = PhasePreInstall

	res, err := hooks.Dispatch(context.Background(), in)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if res.Aborted {
		t.Fatalf("404 should NOT abort (extension didn't implement hook): %+v", res)
	}
	if res.Status != 404 {
		t.Fatalf("status = %d, want 404", res.Status)
	}
	if got := tr.Len(); got != 1 {
		t.Fatalf("expected single attempt, got %d", got)
	}
}

func TestTransportHooks_PreInstall_4xxOther_Aborts(t *testing.T) {
	tr := &InMemoryTransport{Handler: StaticResponseHandler(403, []byte(`{"error":"denied"}`))}
	hooks := NewTransportHooks(tr, nil, fixedClock(time.Unix(1700000000, 0).UTC()))

	in := validLifecycleDispatch(t)
	in.Phase = PhasePreInstall

	res, err := hooks.Dispatch(context.Background(), in)
	if !errors.Is(err, ErrPreInstallRejected) {
		t.Fatalf("err = %v, want ErrPreInstallRejected", err)
	}
	if !res.Aborted {
		t.Fatalf("403 pre_install should abort: %+v", res)
	}
	if res.Status != 403 {
		t.Fatalf("status = %d, want 403", res.Status)
	}
	// 4xx is terminal — single attempt, no retries.
	if got := tr.Len(); got != 1 {
		t.Fatalf("4xx should not retry; got %d attempts", got)
	}
}

func TestTransportHooks_PreInstall_5xxRetried_Aborts(t *testing.T) {
	// Three 502s in a row exhaust the retry budget (3 attempts).
	responses := []*DispatchResponse{
		{Status: 502, Body: nil, Header: map[string]string{}},
		{Status: 502, Body: nil, Header: map[string]string{}},
		{Status: 502, Body: nil, Header: map[string]string{}},
	}
	tr := &InMemoryTransport{Handler: SequenceHandler(responses, nil)}
	// Use a zero-Now so backoff stays cheap in test wall-clock.
	hooks := NewTransportHooks(tr, nil, fixedClock(time.Unix(1700000000, 0).UTC()))

	in := validLifecycleDispatch(t)
	in.Phase = PhasePreInstall

	start := time.Now()
	res, err := hooks.Dispatch(context.Background(), in)
	dur := time.Since(start)
	if !errors.Is(err, ErrPreInstallRejected) {
		t.Fatalf("err = %v, want ErrPreInstallRejected", err)
	}
	if !res.Aborted || res.Attempt != 3 {
		t.Fatalf("res = %+v, want aborted=true attempt=3", res)
	}
	// Two backoffs: 1s + 2s = 3s lower bound.
	if dur < 2900*time.Millisecond {
		t.Fatalf("dispatch returned in %s, expected at least 1+2 = 3s of backoff", dur)
	}
	if got := tr.Len(); got != 3 {
		t.Fatalf("audit len = %d, want 3 (all retries)", got)
	}
}

func TestTransportHooks_PreInstall_5xxThen2xx_Succeeds(t *testing.T) {
	responses := []*DispatchResponse{
		{Status: 502, Header: map[string]string{}},
		{Status: 200, Header: map[string]string{}},
	}
	tr := &InMemoryTransport{Handler: SequenceHandler(responses, nil)}
	hooks := NewTransportHooks(tr, nil, fixedClock(time.Unix(1700000000, 0).UTC()))

	in := validLifecycleDispatch(t)

	res, err := hooks.Dispatch(context.Background(), in)
	if err != nil {
		t.Fatalf("expected nil err, got %v", err)
	}
	if res.Aborted || res.Status != 200 || res.Attempt != 2 {
		t.Fatalf("res = %+v, want aborted=false status=200 attempt=2", res)
	}
	if got := tr.Len(); got != 2 {
		t.Fatalf("audit len = %d, want 2", got)
	}
}

func TestTransportHooks_PostInstall_5xxExhausted_NotAborted(t *testing.T) {
	// Post-phase failures are best-effort. The hook should NOT
	// return an error and should NOT set Aborted, but the result
	// should reflect the final status.
	responses := []*DispatchResponse{
		{Status: 503, Header: map[string]string{}},
		{Status: 503, Header: map[string]string{}},
		{Status: 503, Header: map[string]string{}},
	}
	tr := &InMemoryTransport{Handler: SequenceHandler(responses, nil)}
	hooks := NewTransportHooks(tr, nil, fixedClock(time.Unix(1700000000, 0).UTC()))

	in := validLifecycleDispatch(t)
	in.Phase = PhasePostInstall

	res, err := hooks.Dispatch(context.Background(), in)
	if err != nil {
		t.Fatalf("post-phase 5xx should not error: %v", err)
	}
	if res.Aborted {
		t.Fatalf("post-phase should never abort: %+v", res)
	}
	if res.Status != 503 || res.Attempt != 3 {
		t.Fatalf("res = %+v, want status=503 attempt=3", res)
	}
}

func TestTransportHooks_PreInstall_TransportError_Aborts(t *testing.T) {
	tr := &InMemoryTransport{Handler: func(ctx context.Context, _ string, _ []byte, _ map[string]string) (*DispatchResponse, error) {
		return nil, errors.New("simulated dns failure")
	}}
	hooks := NewTransportHooks(tr, nil, fixedClock(time.Unix(1700000000, 0).UTC()))

	in := validLifecycleDispatch(t)
	in.Phase = PhasePreInstall

	res, err := hooks.Dispatch(context.Background(), in)
	if !errors.Is(err, ErrPreInstallRejected) {
		t.Fatalf("err = %v, want ErrPreInstallRejected", err)
	}
	if !res.Aborted {
		t.Fatalf("transport-error pre_install should abort: %+v", res)
	}
	if !strings.Contains(res.AbortReason, "dns failure") {
		t.Fatalf("AbortReason = %q, want substring 'dns failure'", res.AbortReason)
	}
}

func TestLifecycleDispatch_Validate(t *testing.T) {
	cases := []struct {
		name string
		in   *LifecycleDispatch
		ok   bool
	}{
		{"nil", nil, false},
		{"empty", &LifecycleDispatch{}, false},
		{"no_phase", &LifecycleDispatch{TenantID: uuid.New(), InstallationID: uuid.New(), WebhookBase: "https://x.example"}, false},
		{"http_base", &LifecycleDispatch{TenantID: uuid.New(), InstallationID: uuid.New(), Phase: PhasePreInstall, WebhookBase: "http://x.example"}, false},
		{"happy", &LifecycleDispatch{TenantID: uuid.New(), InstallationID: uuid.New(), Phase: PhasePreInstall, WebhookBase: "https://x.example"}, true},
		// pre_install is allowed to carry uuid.Nil InstallationID
		// because the engine runs pre_install BEFORE the install
		// row is created — there is no id yet to send.
		{"pre_install_nil_install", &LifecycleDispatch{TenantID: uuid.New(), InstallationID: uuid.Nil, Phase: PhasePreInstall, WebhookBase: "https://x.example"}, true},
		// post_install / pre_uninstall / post_uninstall MUST carry
		// a real InstallationID — the row exists by then.
		{"post_install_nil_install", &LifecycleDispatch{TenantID: uuid.New(), InstallationID: uuid.Nil, Phase: PhasePostInstall, WebhookBase: "https://x.example"}, false},
		{"pre_uninstall_nil_install", &LifecycleDispatch{TenantID: uuid.New(), InstallationID: uuid.Nil, Phase: PhasePreUninstall, WebhookBase: "https://x.example"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.in.Validate()
			if c.ok && err != nil {
				t.Fatalf("expected ok, got %v", err)
			}
			if !c.ok && err == nil {
				t.Fatalf("expected err, got nil")
			}
		})
	}
}

// TestTransportHooks_PreInstall_408Retried_ThenSucceeds locks in
// Devin Review ANALYSIS_0002 round-2 on PR #127: 408 must be
// retryable in the lifecycle hooks dispatcher just like it is in
// the agent-tool Dispatcher (dispatcher.go:203). Before the fix a
// 408 from a slow-to-start extension webhook would have aborted
// pre_install on attempt 1 even though attempt 2 would have
// succeeded.
func TestTransportHooks_PreInstall_408Retried_ThenSucceeds(t *testing.T) {
	responses := []*DispatchResponse{
		{Status: 408, Header: map[string]string{}},
		{Status: 200, Header: map[string]string{}},
	}
	tr := &InMemoryTransport{Handler: SequenceHandler(responses, nil)}
	hooks := NewTransportHooks(tr, nil, fixedClock(time.Unix(1700000000, 0).UTC()))

	in := validLifecycleDispatch(t)
	in.Phase = PhasePreInstall

	res, err := hooks.Dispatch(context.Background(), in)
	if err != nil {
		t.Fatalf("expected nil err after 408->200, got %v", err)
	}
	if res.Aborted {
		t.Fatalf("408->200 pre_install must not abort: %+v", res)
	}
	if res.Status != 200 || res.Attempt != 2 {
		t.Fatalf("res = %+v, want status=200 attempt=2", res)
	}
	if got := tr.Len(); got != 2 {
		t.Fatalf("audit len = %d, want 2 attempts", got)
	}
}

// TestTransportHooks_PreInstall_408Exhausted_AbortsWithHttpReason
// covers the exhaust path for 408: after all retries are
// consumed the AbortReason must reflect the http status, not a
// stale transport-error string. Cross-checks the lastErr-clear
// fix in the same commit.
func TestTransportHooks_PreInstall_408Exhausted_AbortsWithHttpReason(t *testing.T) {
	responses := []*DispatchResponse{
		{Status: 408, Header: map[string]string{}},
		{Status: 408, Header: map[string]string{}},
		{Status: 408, Header: map[string]string{}},
	}
	tr := &InMemoryTransport{Handler: SequenceHandler(responses, nil)}
	hooks := NewTransportHooks(tr, nil, fixedClock(time.Unix(1700000000, 0).UTC()))

	in := validLifecycleDispatch(t)
	in.Phase = PhasePreInstall

	res, err := hooks.Dispatch(context.Background(), in)
	if !errors.Is(err, ErrPreInstallRejected) {
		t.Fatalf("err = %v, want ErrPreInstallRejected", err)
	}
	if !res.Aborted || res.Status != 408 || res.Attempt != 3 {
		t.Fatalf("res = %+v, want aborted=true status=408 attempt=3", res)
	}
	wantReason := "http 408 after 3 attempts"
	if res.AbortReason != wantReason {
		t.Fatalf("AbortReason = %q, want %q", res.AbortReason, wantReason)
	}
	if res.Err != nil {
		t.Fatalf("result.Err = %v, want nil (final attempt got an http round-trip)", res.Err)
	}
}

// TestTransportHooks_PreInstall_TransportErrThen5xxExhausted_AbortReasonReflectsHttp
// locks in Devin Review ANALYSIS_0001 round-2 on PR #127: when
// attempt 1 fails at the transport layer and a subsequent
// attempt completes an HTTP round-trip (even an unsuccessful
// one), the AbortReason / result.Err must reflect the http
// outcome from the final attempt — not the stale transport
// error from attempt 1. The lastErr/result.Err reset after a
// successful Send() guarantees this.
func TestTransportHooks_PreInstall_TransportErrThen5xxExhausted_AbortReasonReflectsHttp(t *testing.T) {
	responses := []*DispatchResponse{
		nil,
		{Status: 502, Header: map[string]string{}},
		{Status: 502, Header: map[string]string{}},
	}
	errs := []error{
		errors.New("simulated dns failure"),
		nil,
		nil,
	}
	tr := &InMemoryTransport{Handler: SequenceHandler(responses, errs)}
	hooks := NewTransportHooks(tr, nil, fixedClock(time.Unix(1700000000, 0).UTC()))

	in := validLifecycleDispatch(t)
	in.Phase = PhasePreInstall

	res, err := hooks.Dispatch(context.Background(), in)
	if !errors.Is(err, ErrPreInstallRejected) {
		t.Fatalf("err = %v, want ErrPreInstallRejected", err)
	}
	if !res.Aborted || res.Status != 502 || res.Attempt != 3 {
		t.Fatalf("res = %+v, want aborted=true status=502 attempt=3", res)
	}
	wantReason := "http 502 after 3 attempts"
	if res.AbortReason != wantReason {
		t.Fatalf("AbortReason = %q, want %q (stale transport err leaked from attempt 1)", res.AbortReason, wantReason)
	}
	if res.Err != nil {
		t.Fatalf("result.Err = %v, want nil (final attempt got an http round-trip; stale transport err leaked)", res.Err)
	}
}

// TestTransportHooks_PreInstall_3xx_TerminalNotRetried locks in
// Devin Review round-3 on PR #127: the hooks classifier MUST treat
// 3xx responses as terminal (mirrors the agent-tool Dispatcher
// catch-all at dispatcher.go:209-210). Previously, a 3xx response
// (e.g. the extension's webhook server replying 301/302 — the
// HTTPTransport refuses to follow redirects via CheckRedirect=
// http.ErrUseLastResponse so the raw status bubbles up) fell
// through every if-block and silently retried until MaxAttempts,
// reporting "transport: <stale>" as the AbortReason. The
// unconditional break in the catch-all guarantees a single attempt
// + correct "http 302 after 1 attempts" AbortReason.
func TestTransportHooks_PreInstall_3xx_TerminalNotRetried(t *testing.T) {
	for _, status := range []int{301, 302, 307, 308, 399} {
		t.Run("status_"+fmt.Sprint(status), func(t *testing.T) {
			tr := &InMemoryTransport{Handler: StaticResponseHandler(status, nil)}
			hooks := NewTransportHooks(tr, nil, fixedClock(time.Unix(1700000000, 0).UTC()))

			in := validLifecycleDispatch(t)
			in.Phase = PhasePreInstall
			// Lifecycle retry policy is hard-coded at
			// lifecycleRetry = MaxAttempts=3, so a buggy fall-
			// through would visibly retry up to 3 times; the
			// fix guarantees attempt=1 and audit-len=1.

			res, err := hooks.Dispatch(context.Background(), in)
			if !errors.Is(err, ErrPreInstallRejected) {
				t.Fatalf("err = %v, want ErrPreInstallRejected", err)
			}
			if !res.Aborted || res.Status != status || res.Attempt != 1 {
				t.Fatalf("res = %+v, want aborted=true status=%d attempt=1", res, status)
			}
			wantReason := fmt.Sprintf("http %d after 1 attempts", status)
			if res.AbortReason != wantReason {
				t.Fatalf("AbortReason = %q, want %q", res.AbortReason, wantReason)
			}
			if res.Err != nil {
				t.Fatalf("result.Err = %v, want nil (final attempt got an http round-trip)", res.Err)
			}
			if got := tr.Len(); got != 1 {
				t.Fatalf("audit len = %d, want 1 (3xx must NOT be retried)", got)
			}
		})
	}
}

// TestTransportHooks_PostInstall_3xx_BestEffortNotAborted asserts
// the symmetric post-phase behaviour: 3xx (or any other non-2xx)
// on a post-phase hook is logged but NOT an abort, since post-
// install / post-uninstall are best-effort. The catch-all break
// still applies, so the audit log records a single attempt.
func TestTransportHooks_PostInstall_3xx_BestEffortNotAborted(t *testing.T) {
	tr := &InMemoryTransport{Handler: StaticResponseHandler(302, nil)}
	hooks := NewTransportHooks(tr, nil, fixedClock(time.Unix(1700000000, 0).UTC()))

	in := validLifecycleDispatch(t)
	in.Phase = PhasePostInstall

	res, err := hooks.Dispatch(context.Background(), in)
	if err != nil {
		t.Fatalf("post-phase 3xx should NOT error: %v", err)
	}
	if res.Aborted {
		t.Fatalf("post-phase 3xx should NOT abort: %+v", res)
	}
	if res.Status != 302 || res.Attempt != 1 {
		t.Fatalf("res = %+v, want status=302 attempt=1", res)
	}
	if got := tr.Len(); got != 1 {
		t.Fatalf("audit len = %d, want 1 (3xx must NOT be retried even on post-phase)", got)
	}
}

// TestTransportHooks_LatencyMS_LocallyMeasured asserts that
// LifecycleResult.LatencyMS reflects the WALL-CLOCK time spent in
// the transport.Send call, NOT the transport's self-reported
// resp.Latency field. The InMemoryTransport never populates
// resp.Latency (it leaves the field at zero — see transport.go:223,
// 233), so a previous version of this code that read result.LatencyMS
// from resp.Latency silently reported 0ms for every test even when
// real wall-clock time elapsed. Devin Review round-8 BUG_0002 on
// PR #127. We use a handler that sleeps for a known minimum duration
// and assert the reported LatencyMS is >= that duration in wall-clock
// terms.
func TestTransportHooks_LatencyMS_LocallyMeasured(t *testing.T) {
	const sleepFloor = 25 * time.Millisecond
	handler := func(_ context.Context, _ string, _ []byte, _ map[string]string) (*DispatchResponse, error) {
		time.Sleep(sleepFloor)
		// resp.Latency intentionally left at zero — this is what
		// the old buggy path would have surfaced as LatencyMS.
		return &DispatchResponse{Status: 200, Body: nil, Header: map[string]string{}}, nil
	}
	tr := &InMemoryTransport{Handler: handler}
	hooks := NewTransportHooks(tr, nil, fixedClock(time.Unix(1700000000, 0).UTC()))

	in := validLifecycleDispatch(t)
	in.Phase = PhasePreInstall

	res, err := hooks.Dispatch(context.Background(), in)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if res.Status != 200 || res.Aborted {
		t.Fatalf("res = %+v, want status=200 aborted=false", res)
	}
	// LatencyMS must reflect the locally-measured wall-clock time
	// around transport.Send (sleepFloor is the minimum), NOT the
	// zero-valued resp.Latency that the InMemoryTransport returns.
	// A small 5ms safety margin absorbs sleep-quantization jitter
	// on slow CI machines without weakening the assertion that the
	// local-measurement path is in effect (any 0 reading would be
	// the bug regressing).
	minMS := int((sleepFloor - 5*time.Millisecond) / time.Millisecond)
	if res.LatencyMS < minMS {
		t.Fatalf("LatencyMS = %d, want >= %d (sleepFloor=%v) — local wall-clock measurement broken; reading from resp.Latency=0 instead?", res.LatencyMS, minMS, sleepFloor)
	}
}

func TestLifecyclePayload_DefaultEmpty(t *testing.T) {
	b, err := MarshalLifecyclePayload(nil)
	if err != nil {
		t.Fatalf("marshal nil: %v", err)
	}
	if string(b) != "{}" {
		t.Fatalf("nil → %q, want '{}'", string(b))
	}
	b, err = MarshalLifecyclePayload(map[string]string{"a": "b"})
	if err != nil {
		t.Fatalf("marshal map: %v", err)
	}
	if !strings.Contains(string(b), `"a":"b"`) {
		t.Fatalf("marshal map → %q", string(b))
	}
}
