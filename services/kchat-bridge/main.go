// Command kchat-bridge is the integration service that translates between
// KChat (Kapp's parent messaging product) and the Kapp platform. For Phase A
// it is a skeleton: it stands up an HTTP server, wires card rendering and
// slash-command dispatching, and leaves the actual KChat transport to later
// phases.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/kennguy3n/kapp-fab/internal/ktype"
	"github.com/kennguy3n/kapp-fab/internal/platform"
)

func main() {
	if err := run(); err != nil {
		log.Fatalf("kchat-bridge: %v", err)
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

	cache := platform.NewLRUCache(512, 5*time.Minute)
	registry := ktype.NewPGRegistry(pool, cache)
	cards := &CardRenderer{registry: registry}
	commands := &CommandDispatcher{registry: registry}

	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.Recoverer)
	r.Use(middleware.Timeout(30 * time.Second))

	r.Post("/kchat/cards/{ktype}", func(w http.ResponseWriter, req *http.Request) {
		var body struct {
			Data map[string]any `json:"data"`
		}
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		card, err := cards.RenderCard(req.Context(), chi.URLParam(req, "ktype"), body.Data)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, card)
	})

	r.Post("/kchat/commands", func(w http.ResponseWriter, req *http.Request) {
		var body CommandRequest
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		resp, err := commands.Dispatch(req.Context(), body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, resp)
	})

	r.Get("/healthz", func(w http.ResponseWriter, req *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})

	addr := cfg.ListenAddr
	if addr == ":8080" {
		addr = ":8082"
	}
	srv := &http.Server{
		Addr:              addr,
		Handler:           r,
		ReadHeaderTimeout: 10 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		log.Printf("kchat-bridge: listening on %s", addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
		close(errCh)
	}()

	select {
	case <-ctx.Done():
		log.Printf("kchat-bridge: shutdown signal received")
	case err := <-errCh:
		if err != nil {
			return err
		}
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}

func writeJSON(w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}
