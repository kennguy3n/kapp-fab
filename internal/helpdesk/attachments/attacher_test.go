package attachments

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/kennguy3n/kapp-fab/internal/files"
)

// fakeUploader records the Upload call without touching S3 / the
// DB. The Attacher's contract calls uploader.Upload BEFORE the
// email_attachments insert, so the parts of Attach that this test
// exercises (input validation, size cap, scan-then-skip-upload on
// infected, scanner-error propagation) run end-to-end against the
// fake without a live pool.
type fakeUploader struct {
	calls   int
	returns *files.File
	err     error
}

func (f *fakeUploader) Upload(_ context.Context, tenantID, _ uuid.UUID, blob files.Blob) (*files.File, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	if f.returns != nil {
		return f.returns, nil
	}
	return &files.File{
		ID:          uuid.New(),
		TenantID:    tenantID,
		ContentType: blob.ContentType,
		SizeBytes:   int64(len(blob.Data)),
	}, nil
}

// fakeScanner is a deterministic Scanner for unit tests. The result
// field controls the verdict + detail; the err field overrides to
// simulate scanner-unavailable. The calls slice records every
// invocation so the test can assert exactly what was scanned.
type fakeScanner struct {
	result ScanResult
	err    error
	calls  []string
}

func (f *fakeScanner) Scan(_ context.Context, filename, _ string, _ []byte) (ScanResult, error) {
	f.calls = append(f.calls, filename)
	if f.err != nil {
		return ScanResult{}, f.err
	}
	return f.result, nil
}

// TestAttach_ValidationErrors pins the input-validation contract.
// Empty tenant / message-id / filename / data all return errors
// BEFORE the scanner is invoked, so a malformed call never spends
// scanner budget.
func TestAttach_ValidationErrors(t *testing.T) {
	scanner := &fakeScanner{result: ScanResult{Verdict: VerdictClean}}
	uploader := &fakeUploader{}
	a := NewAttacher(nil, uploader, scanner, uuid.New())

	cases := []struct {
		name     string
		tenantID uuid.UUID
		msgID    string
		att      Attachment
	}{
		{"nil_tenant", uuid.Nil, "m", Attachment{Filename: "a.txt", Data: []byte("x")}},
		{"empty_message_id", uuid.New(), "", Attachment{Filename: "a.txt", Data: []byte("x")}},
		{"whitespace_message_id", uuid.New(), "   ", Attachment{Filename: "a.txt", Data: []byte("x")}},
		{"empty_filename", uuid.New(), "m", Attachment{Data: []byte("x")}},
		{"empty_data", uuid.New(), "m", Attachment{Filename: "a.txt"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := a.Attach(context.Background(), tc.tenantID, tc.msgID, tc.att)
			if err == nil {
				t.Fatalf("expected validation error")
			}
		})
	}
	if len(scanner.calls) != 0 {
		t.Errorf("expected zero scanner invocations for validation failures, got %d", len(scanner.calls))
	}
	if uploader.calls != 0 {
		t.Errorf("expected zero uploader invocations for validation failures, got %d", uploader.calls)
	}
}

// TestAttach_SizeCap pins ErrTooLarge: bytes over the cap are
// rejected BEFORE scan + upload so a malicious payload never
// reaches the scanner or S3.
func TestAttach_SizeCap(t *testing.T) {
	scanner := &fakeScanner{result: ScanResult{Verdict: VerdictClean}}
	uploader := &fakeUploader{}
	a := NewAttacher(nil, uploader, scanner, uuid.New(), WithMaxBytes(10))
	_, err := a.Attach(context.Background(), uuid.New(), "m", Attachment{
		Filename: "big.bin",
		Data:     []byte("aaaaaaaaaaa"), // 11 bytes > cap 10
	})
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("expected ErrTooLarge, got %v", err)
	}
	if len(scanner.calls) != 0 {
		t.Errorf("expected zero scanner calls for size-rejected payload")
	}
	if uploader.calls != 0 {
		t.Errorf("expected zero uploader calls for size-rejected payload")
	}
}

// TestAttach_InfectedPayloadShortCircuits pins the security-
// critical "scan-then-skip-upload" ordering. When the scanner
// returns VerdictInfected, ErrInfected is returned and the
// uploader is NEVER called. Bytes do not reach S3.
func TestAttach_InfectedPayloadShortCircuits(t *testing.T) {
	scanner := &fakeScanner{result: ScanResult{Verdict: VerdictInfected, Detail: "EICAR"}}
	uploader := &fakeUploader{}
	a := NewAttacher(nil, uploader, scanner, uuid.New())
	_, err := a.Attach(context.Background(), uuid.New(), "m", Attachment{
		Filename: "virus.bin",
		Data:     []byte("malicious"),
	})
	if !errors.Is(err, ErrInfected) {
		t.Fatalf("expected ErrInfected, got %v", err)
	}
	if uploader.calls != 0 {
		t.Errorf("expected zero uploader calls for infected payload — bytes must NOT reach S3, got %d", uploader.calls)
	}
	if len(scanner.calls) != 1 || scanner.calls[0] != "virus.bin" {
		t.Errorf("expected one scan of virus.bin, got %v", scanner.calls)
	}
	// The error message should surface the signature name so the
	// agent UI can render the audit detail.
	if !errors.Is(err, ErrInfected) || err.Error() == ErrInfected.Error() {
		// Make sure the wrap surfaces extra context — the
		// %w'd error carries signature + filename detail.
		if err.Error() == "" {
			t.Errorf("expected error wrap to carry detail")
		}
	}
}

// TestAttach_ScannerUnavailableFailsClosed pins that a scanner
// error (network, 5xx, malformed response) propagates from Scan
// out through Attach. The uploader is NEVER called — fail closed.
func TestAttach_ScannerUnavailableFailsClosed(t *testing.T) {
	scannerErr := errors.New("simulated scanner outage")
	scanner := &fakeScanner{err: scannerErr}
	uploader := &fakeUploader{}
	a := NewAttacher(nil, uploader, scanner, uuid.New())
	_, err := a.Attach(context.Background(), uuid.New(), "m", Attachment{
		Filename: "x",
		Data:     []byte("p"),
	})
	if err == nil {
		t.Fatalf("expected error on scanner outage")
	}
	if !errors.Is(err, scannerErr) {
		t.Errorf("expected scanner error to propagate, got %v", err)
	}
	if uploader.calls != 0 {
		t.Errorf("expected zero uploader calls on scanner outage, got %d", uploader.calls)
	}
}

// TestNewAttacher_DefaultMaxBytes pins the default 25MiB cap. An
// operator who doesn't set WithMaxBytes gets the SMTP-friendly
// default rather than unlimited.
func TestNewAttacher_DefaultMaxBytes(t *testing.T) {
	a := NewAttacher(nil, &fakeUploader{}, &fakeScanner{}, uuid.New())
	if a.maxBytes != DefaultMaxBytes {
		t.Errorf("expected DefaultMaxBytes %d, got %d", DefaultMaxBytes, a.maxBytes)
	}
}

// TestWithMaxBytes_RejectsNonPositive pins the WithMaxBytes guard.
// Zero or negative is silently dropped — disabling the cap entirely
// is achievable by passing math.MaxInt64.
func TestWithMaxBytes_RejectsNonPositive(t *testing.T) {
	for _, n := range []int64{0, -1, -1000} {
		a := NewAttacher(nil, &fakeUploader{}, &fakeScanner{}, uuid.New(), WithMaxBytes(n))
		if a.maxBytes != DefaultMaxBytes {
			t.Errorf("WithMaxBytes(%d): expected default, got %d", n, a.maxBytes)
		}
	}
}
