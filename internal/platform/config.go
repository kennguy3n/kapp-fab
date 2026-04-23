// Package platform wires infrastructure primitives (database, config) used
// across Kapp services. It exposes a small set of helpers that services can
// compose without introducing framework-level coupling.
package platform

import (
	"errors"
	"os"
)

// Config holds runtime configuration values shared by the API and worker
// services. Fields are populated from environment variables by LoadConfig.
type Config struct {
	// DatabaseURL is a libpq-style connection string for PostgreSQL. In
	// production this points at the non-superuser `kapp_app` role so that
	// RLS is enforced on the data plane (see migrations).
	DatabaseURL string
	// AdminDatabaseURL optionally points at a BYPASSRLS role (kapp_admin)
	// used only for the narrow set of control-plane reads that legitimately
	// span tenants — notably the user→tenants lookup used by login. Empty
	// is allowed; those reads then fall back to the main pool and return
	// no rows under the default RLS policy.
	AdminDatabaseURL string
	// ListenAddr is the host:port the HTTP server binds to (API only).
	ListenAddr string
	// S3Endpoint is the object-store endpoint (S3 or MinIO compatible).
	S3Endpoint string
	// S3Bucket is the bucket used for Kapp file attachments.
	S3Bucket string
	// S3AccessKey is the object-store access key ID.
	S3AccessKey string
	// S3SecretKey is the object-store secret access key.
	S3SecretKey string
	// EventBusURL is the NATS/Kafka/etc. URL for the event bus.
	EventBusURL string
	// SMTPHost/Port/User/Password/From configure the outbound mail
	// adapter used by the worker for `notification.channel=email`.
	// All five are optional; when SMTPHost is empty the worker falls
	// back to logging the notice instead of dialing an MTA.
	SMTPHost     string
	SMTPPort     string
	SMTPUser     string
	SMTPPassword string
	SMTPFrom     string
}

// LoadConfig reads configuration from environment variables and returns a
// validated Config. It returns an error if a required value is missing.
func LoadConfig() (*Config, error) {
	cfg := &Config{
		DatabaseURL:      os.Getenv("DB_URL"),
		AdminDatabaseURL: os.Getenv("ADMIN_DB_URL"),
		ListenAddr:       getenv("LISTEN_ADDR", ":8080"),
		S3Endpoint:       os.Getenv("S3_ENDPOINT"),
		S3Bucket:         os.Getenv("S3_BUCKET"),
		S3AccessKey:      os.Getenv("S3_ACCESS_KEY"),
		S3SecretKey:      os.Getenv("S3_SECRET_KEY"),
		EventBusURL:      os.Getenv("NATS_URL"),
		SMTPHost:         os.Getenv("SMTP_HOST"),
		SMTPPort:         os.Getenv("SMTP_PORT"),
		SMTPUser:         os.Getenv("SMTP_USER"),
		SMTPPassword:     os.Getenv("SMTP_PASS"),
		SMTPFrom:         os.Getenv("SMTP_FROM"),
	}
	if cfg.DatabaseURL == "" {
		return nil, errors.New("DB_URL is required")
	}
	return cfg, nil
}

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
