package record

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestFilterFields(t *testing.T) {
	schema := json.RawMessage(`{
		"type":"object",
		"field_permissions":{
			"salary":{"read":["hr.admin","owner"],"write":["hr.admin"]},
			"ssn":{"read":["hr.admin"],"write":["hr.admin"]}
		}
	}`)
	data := json.RawMessage(`{"name":"Alice","salary":100000,"ssn":"123-45-6789"}`)

	t.Run("hr.admin sees everything", func(t *testing.T) {
		got := FilterFields(data, schema, []string{"hr.admin"})
		if !strings.Contains(string(got), "salary") || !strings.Contains(string(got), "ssn") {
			t.Errorf("expected unredacted, got %s", got)
		}
	})

	t.Run("tenant.member loses both restricted fields", func(t *testing.T) {
		got := FilterFields(data, schema, []string{"tenant.member"})
		if strings.Contains(string(got), "salary") {
			t.Errorf("salary leaked: %s", got)
		}
		if strings.Contains(string(got), "ssn") {
			t.Errorf("ssn leaked: %s", got)
		}
		if !strings.Contains(string(got), "Alice") {
			t.Errorf("non-restricted field dropped: %s", got)
		}
	})

	t.Run("owner sees salary but not ssn", func(t *testing.T) {
		got := FilterFields(data, schema, []string{"owner"})
		if !strings.Contains(string(got), "salary") {
			t.Errorf("owner should see salary: %s", got)
		}
		if strings.Contains(string(got), "ssn") {
			t.Errorf("owner should not see ssn: %s", got)
		}
	})

	t.Run("schema without block is no-op", func(t *testing.T) {
		got := FilterFields(data, json.RawMessage(`{"type":"object"}`), []string{"tenant.member"})
		if string(got) != string(data) {
			t.Errorf("expected passthrough, got %s", got)
		}
	})
}

func TestFieldsForbiddenForWrite(t *testing.T) {
	schema := json.RawMessage(`{
		"field_permissions":{
			"salary":{"write":["hr.admin"]}
		}
	}`)

	t.Run("non-admin writing salary is rejected", func(t *testing.T) {
		fb := FieldsForbiddenForWrite(json.RawMessage(`{"name":"Alice","salary":1}`), schema, []string{"tenant.member"})
		if len(fb) != 1 || fb[0] != "salary" {
			t.Errorf("expected [salary], got %v", fb)
		}
	})

	t.Run("hr.admin can write salary", func(t *testing.T) {
		fb := FieldsForbiddenForWrite(json.RawMessage(`{"salary":1}`), schema, []string{"hr.admin"})
		if len(fb) != 0 {
			t.Errorf("expected nothing forbidden, got %v", fb)
		}
	})

	t.Run("non-admin not touching salary is fine", func(t *testing.T) {
		fb := FieldsForbiddenForWrite(json.RawMessage(`{"name":"Alice"}`), schema, []string{"tenant.member"})
		if len(fb) != 0 {
			t.Errorf("expected nothing forbidden, got %v", fb)
		}
	})
}
