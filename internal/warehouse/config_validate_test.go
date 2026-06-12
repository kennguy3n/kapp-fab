package warehouse

import (
	"strings"
	"testing"

	"github.com/google/uuid"
)

func baseConfig() Config {
	return Config{
		TenantID:                uuid.New(),
		Name:                    "nightly",
		DestinationDataSourceID: uuid.New(),
		CronExpression:          "0 2 * * *",
		Sources:                 []string{"ktype:crm.contact"},
	}
}

func TestValidate_Defaults(t *testing.T) {
	c := baseConfig()
	if err := c.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if c.DestinationSchema != DefaultDestinationSchema {
		t.Fatalf("schema = %q, want default %q", c.DestinationSchema, DefaultDestinationSchema)
	}
	if c.Mode != ModeIncremental {
		t.Fatalf("mode = %q, want default incremental", c.Mode)
	}
}

func TestValidate_DedupesSources(t *testing.T) {
	c := baseConfig()
	c.Sources = []string{"ktype:crm.contact", "ledger.journal_lines", "ktype:crm.contact"}
	if err := c.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if len(c.Sources) != 2 {
		t.Fatalf("sources = %v, want deduped to 2", c.Sources)
	}
	// Order preserved: first occurrence wins.
	if c.Sources[0] != "ktype:crm.contact" || c.Sources[1] != "ledger.journal_lines" {
		t.Fatalf("dedupe did not preserve order: %v", c.Sources)
	}
}

func TestValidate_Rejects(t *testing.T) {
	cases := map[string]func(*Config){
		"empty name":       func(c *Config) { c.Name = "" },
		"nil datasource":   func(c *Config) { c.DestinationDataSourceID = uuid.Nil },
		"bad schema":       func(c *Config) { c.DestinationSchema = "Bad-Schema" },
		"bad mode":         func(c *Config) { c.Mode = "sideways" },
		"empty cron":       func(c *Config) { c.CronExpression = "" },
		"bad cron":         func(c *Config) { c.CronExpression = "not a cron" },
		"no sources":       func(c *Config) { c.Sources = nil },
		"unknown source":   func(c *Config) { c.Sources = []string{"ledger.secrets"} },
		"bad ktype in mix": func(c *Config) { c.Sources = []string{"ktype:crm.contact", "ktype:BAD"} },
	}
	for name, mut := range cases {
		t.Run(name, func(t *testing.T) {
			c := baseConfig()
			mut(&c)
			if err := c.Validate(); err == nil {
				t.Fatalf("Validate(%s): expected error, got nil", name)
			}
		})
	}
}

func TestValidate_TooManySources(t *testing.T) {
	c := baseConfig()
	c.Sources = make([]string, MaxSourcesPerConfig+1)
	for i := range c.Sources {
		c.Sources[i] = "ktype:crm.contact"
	}
	err := c.Validate()
	// Dedupe collapses the duplicates to one BEFORE the count check is
	// reached only if the count check runs after dedupe — it runs
	// before, so a >max raw list is rejected.
	if err == nil || !strings.Contains(err.Error(), "too many sources") {
		t.Fatalf("expected too-many-sources error, got %v", err)
	}
}
