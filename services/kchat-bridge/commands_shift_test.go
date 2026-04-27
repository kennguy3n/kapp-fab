package main

import (
	"strings"
	"testing"

	"github.com/google/uuid"
)

// TestShiftAssignmentFromArgs covers the Phase M /shift parser:
// happy path, malformed UUIDs, malformed dates, missing args, and
// optional notes joining. Keeps the parser locked in even as the
// rest of the dispatcher evolves.
func TestShiftAssignmentFromArgs(t *testing.T) {
	emp := uuid.NewString()
	st := uuid.NewString()

	t.Run("happy path", func(t *testing.T) {
		got, err := shiftAssignmentFromArgs([]string{emp, st, "2026-04-01"})
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		if got["employee_id"] != emp {
			t.Errorf("employee_id = %v; want %v", got["employee_id"], emp)
		}
		if got["shift_type_id"] != st {
			t.Errorf("shift_type_id = %v; want %v", got["shift_type_id"], st)
		}
		if got["shift_date"] != "2026-04-01" {
			t.Errorf("shift_date = %v", got["shift_date"])
		}
		if got["status"] != "scheduled" {
			t.Errorf("status = %v; want scheduled", got["status"])
		}
		if _, ok := got["notes"]; ok {
			t.Errorf("notes set without args[3]: %v", got["notes"])
		}
	})

	t.Run("notes join", func(t *testing.T) {
		got, err := shiftAssignmentFromArgs([]string{emp, st, "2026-04-01", "covering", "for", "K."})
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		if got["notes"] != "covering for K." {
			t.Errorf("notes = %v; want 'covering for K.'", got["notes"])
		}
	})

	t.Run("too few args", func(t *testing.T) {
		_, err := shiftAssignmentFromArgs([]string{emp, st})
		if err == nil || !strings.Contains(err.Error(), "usage:") {
			t.Errorf("err = %v; want usage error", err)
		}
	})

	t.Run("invalid employee uuid", func(t *testing.T) {
		_, err := shiftAssignmentFromArgs([]string{"not-a-uuid", st, "2026-04-01"})
		if err == nil || !strings.Contains(err.Error(), "employee_id") {
			t.Errorf("err = %v; want employee_id error", err)
		}
	})

	t.Run("invalid date", func(t *testing.T) {
		_, err := shiftAssignmentFromArgs([]string{emp, st, "April 1"})
		if err == nil || !strings.Contains(err.Error(), "shift_date") {
			t.Errorf("err = %v; want shift_date error", err)
		}
	})
}
