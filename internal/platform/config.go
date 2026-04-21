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
	// DatabaseURL is a libpq-style connection string for PostgreSQL.
	DatabaseURL string
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
}

// LoadConfig reads configuration from environment variables and returns a
// validated Config. It returns an error if a required value is missing.
func LoadConfig() (*Config, error) {
	cfg := &Config{
		DatabaseURL: os.Getenv("DB_URL"),
		ListenAddr:  getenv("LISTEN_ADDR", ":8080"),
		S3Endpoint:  os.Getenv("S3_ENDPOINT"),
		S3Bucket:    os.Getenv("S3_BUCKET"),
		S3AccessKey: os.Getenv("S3_ACCESS_KEY"),
		S3SecretKey: os.Getenv("S3_SECRET_KEY"),
		EventBusURL: os.Getenv("NATS_URL"),
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
