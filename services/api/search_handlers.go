package main

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/kennguy3n/kapp-fab/internal/platform"
	"github.com/kennguy3n/kapp-fab/internal/record"
)

type searchHandlers struct {
	store *record.PGStore
}

// search answers GET /api/v1/search?q=...&ktype=...&limit=...
// The ktype param may be repeated (?ktype=crm.lead&ktype=crm.deal) or
// comma-separated so the UI can pin the result set to specific
// KTypes without a per-domain endpoint.
func (h *searchHandlers) search(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" {
		writeJSON(w, http.StatusOK, map[string]any{
			"query":   "",
			"results": []record.SearchResult{},
		})
		return
	}
	ktypes := []string{}
	for _, raw := range r.URL.Query()["ktype"] {
		for _, k := range strings.Split(raw, ",") {
			k = strings.TrimSpace(k)
			if k != "" {
				ktypes = append(ktypes, k)
			}
		}
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	results, err := h.store.Search(r.Context(), t.ID, q, ktypes, limit)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"query":   q,
		"results": results,
	})
}
