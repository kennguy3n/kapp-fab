// Command worker is the Kapp async worker process. It drains the event
// outbox and publishes messages to NATS. Later phases add workflow timer
// advancement, retries, and background job handlers.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/kennguy3n/kapp-fab/internal/events"
	"github.com/kennguy3n/kapp-fab/internal/platform"
)

const (
	tickInterval = 2 * time.Second
	drainBatch   = 100
)

func main() {
	if err := run(); err != nil {
		log.Fatalf("worker: %v", err)
	}
}

func run() error {
	cfg, err := platform.LoadConfig()
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := platform.NewPool(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()

	natsURL := cfg.EventBusURL
	if natsURL == "" {
		natsURL = nats.DefaultURL
	}
	nc, err := nats.Connect(natsURL,
		nats.Name("kapp-worker"),
		nats.ReconnectWait(2*time.Second),
		nats.MaxReconnects(-1),
	)
	if err != nil {
		return fmt.Errorf("connect nats: %w", err)
	}
	defer nc.Drain()

	publisher := events.NewPGPublisher(pool)

	log.Printf("worker: started; draining every %s; nats=%s", tickInterval, natsURL)
	ticker := time.NewTicker(tickInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Printf("worker: shutdown signal received")
			return nil
		case <-ticker.C:
			if _, err := publisher.DrainBatch(ctx, drainBatch, deliver(nc)); err != nil {
				log.Printf("worker: drain batch: %v", err)
			}
		}
	}
}

func deliver(nc *nats.Conn) func(ctx context.Context, batch []events.Event) error {
	return func(_ context.Context, batch []events.Event) error {
		for _, e := range batch {
			subject := fmt.Sprintf("kapp.events.%s", e.Type)
			payload, err := json.Marshal(e)
			if err != nil {
				return fmt.Errorf("marshal event %s: %w", e.ID, err)
			}
			if err := nc.Publish(subject, payload); err != nil {
				return fmt.Errorf("publish %s: %w", subject, err)
			}
		}
		return nc.Flush()
	}
}
