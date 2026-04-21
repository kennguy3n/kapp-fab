// Command worker is the Kapp async worker process. It drains the event
// outbox, advances workflow timers, and runs background jobs. Phase A
// ships a placeholder tick loop; later phases add real job handlers.
package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/kennguy3n/kapp-fab/internal/platform"
)

const tickInterval = 10 * time.Second

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

	log.Printf("worker: started; tick interval %s", tickInterval)

	ticker := time.NewTicker(tickInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Printf("worker: shutdown signal received")
			return nil
		case <-ticker.C:
			log.Printf("worker tick")
		}
	}
}
