package main

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/kennguy3n/kapp-fab/internal/notifications"
	"github.com/kennguy3n/kapp-fab/internal/platform"
)

// notificationsHandlers serves the in-app bell / inbox endpoints. List
// is filtered to the authenticated user plus tenant-wide notices;
// mark-read and mark-all-read flip the read flag so the bell badge
// count drops.
type notificationsHandlers struct {
	store *notifications.Store
}

func newNotificationsHandlers(store *notifications.Store) *notificationsHandlers {
	return &notificationsHandlers{store: store}
}

// list returns the most recent notifications visible to the caller.
// Query params: ?unread=1 (default: all), ?limit=N (default: 50).
func (h *notificationsHandlers) list(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant missing", http.StatusUnauthorized)
		return
	}
	userID := platform.UserIDFromContext(r.Context())
	filter := notifications.ListFilter{UnreadOnly: r.URL.Query().Get("unread") == "1"}
	if userID != uuid.Nil {
		filter.UserID = &userID
	}
	if n, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil {
		filter.Limit = n
	}
	rows, err := h.store.List(r.Context(), t.ID, filter)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, rows)
}

// markRead flips a single notification read. 404 if not visible.
func (h *notificationsHandlers) markRead(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant missing", http.StatusUnauthorized)
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	if err := h.store.MarkRead(r.Context(), t.ID, id); err != nil {
		if errors.Is(err, notifications.ErrNotFound) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// markAllRead flips every unread row scoped to the caller. Returns
// the number of rows updated.
func (h *notificationsHandlers) markAllRead(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant missing", http.StatusUnauthorized)
		return
	}
	userID := platform.UserIDFromContext(r.Context())
	var uid *uuid.UUID
	if userID != uuid.Nil {
		uid = &userID
	}
	n, err := h.store.MarkAllRead(r.Context(), t.ID, uid)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"updated": n})
}
