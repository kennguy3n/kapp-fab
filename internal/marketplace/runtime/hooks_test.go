package runtime

import (
	"context"
	"errors"
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
	hooks := NewTransportHooks(tr, fixedClock(time.Unix(1700000000, 0).UTC()))

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
	if got := len(tr.Audit); got != 1 {
		t.Fatalf("audit len = %d, want 1", got)
	}
	if !strings.HasSuffix(tr.Audit[0].Target, "/lifecycle/pre_install") {
		t.Fatalf("target = %q, want suffix /lifecycle/pre_install", tr.Audit[0].Target)
	}
	if tr.Audit[0].Headers[SignatureHeaderName] == "" {
		t.Fatalf("audit lacks signature header")
	}
}

func TestTransportHooks_PreInstall_404_NotAborted(t *testing.T) {
	tr := &InMemoryTransport{Handler: StaticResponseHandler(404, nil)}
	hooks := NewTransportHooks(tr, fixedClock(time.Unix(1700000000, 0).UTC()))

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
	if len(tr.Audit) != 1 {
		t.Fatalf("expected single attempt, got %d", len(tr.Audit))
	}
}

func TestTransportHooks_PreInstall_4xxOther_Aborts(t *testing.T) {
	tr := &InMemoryTransport{Handler: StaticResponseHandler(403, []byte(`{"error":"denied"}`))}
	hooks := NewTransportHooks(tr, fixedClock(time.Unix(1700000000, 0).UTC()))

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
	if len(tr.Audit) != 1 {
		t.Fatalf("4xx should not retry; got %d attempts", len(tr.Audit))
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
	hooks := NewTransportHooks(tr, fixedClock(time.Unix(1700000000, 0).UTC()))

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
	if len(tr.Audit) != 3 {
		t.Fatalf("audit len = %d, want 3 (all retries)", len(tr.Audit))
	}
}

func TestTransportHooks_PreInstall_5xxThen2xx_Succeeds(t *testing.T) {
	responses := []*DispatchResponse{
		{Status: 502, Header: map[string]string{}},
		{Status: 200, Header: map[string]string{}},
	}
	tr := &InMemoryTransport{Handler: SequenceHandler(responses, nil)}
	hooks := NewTransportHooks(tr, fixedClock(time.Unix(1700000000, 0).UTC()))

	in := validLifecycleDispatch(t)

	res, err := hooks.Dispatch(context.Background(), in)
	if err != nil {
		t.Fatalf("expected nil err, got %v", err)
	}
	if res.Aborted || res.Status != 200 || res.Attempt != 2 {
		t.Fatalf("res = %+v, want aborted=false status=200 attempt=2", res)
	}
	if len(tr.Audit) != 2 {
		t.Fatalf("audit len = %d, want 2", len(tr.Audit))
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
	hooks := NewTransportHooks(tr, fixedClock(time.Unix(1700000000, 0).UTC()))

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
	hooks := NewTransportHooks(tr, fixedClock(time.Unix(1700000000, 0).UTC()))

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
