package insights

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// TestRunRawSQLRejectsMultiStatement verifies the documented guard
// against semicolon-separated SQL bodies. pgx.Query silently
// executes only the first statement, so this guard turns a silent
// drop into a 400 the caller can act on.
func TestRunRawSQLRejectsMultiStatement(t *testing.T) {
	r := &Runner{}
	cases := []string{
		"SELECT 1; DROP TABLE foo",                      // trailing extra
		"SELECT 1;",                                     // trailing terminator
		"BEGIN; SELECT 1; COMMIT",                       // multi-segment
		"SELECT * FROM employees; SELECT * FROM users;", // two real reads
	}
	for _, body := range cases {
		_, err := r.RunRawSQL(context.Background(), uuid.New(), body, nil)
		if err == nil {
			t.Errorf("RunRawSQL(%q) returned nil error; want validation failure", body)
			continue
		}
		if !strings.Contains(err.Error(), "multi-statement SQL") {
			t.Errorf("RunRawSQL(%q) error = %q; want multi-statement message", body, err.Error())
		}
	}
}

// TestRunRawSQLRejectsEmptyBody covers the existing "raw_sql body
// required" guard so the validationErr surface stays exercised.
func TestRunRawSQLRejectsEmptyBody(t *testing.T) {
	r := &Runner{}
	if _, err := r.RunRawSQL(context.Background(), uuid.New(), "", nil); err == nil {
		t.Fatal("RunRawSQL(\"\") returned nil error; want validation failure")
	}
}

// TestRunRawSQLRejectsZeroTenant covers the tenant-id guard.
func TestRunRawSQLRejectsZeroTenant(t *testing.T) {
	r := &Runner{}
	if _, err := r.RunRawSQL(context.Background(), uuid.Nil, "SELECT 1", nil); err == nil {
		t.Fatal("RunRawSQL(uuid.Nil) returned nil error; want validation failure")
	}
}
