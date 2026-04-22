package main

import (
	"net/http"

	"github.com/kennguy3n/kapp-fab/internal/ktype"
)

type openAPIHandler struct {
	registry *ktype.PGRegistry
}

func (h *openAPIHandler) serve(w http.ResponseWriter, r *http.Request) {
	kts, err := h.registry.List(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	body, err := ktype.GenerateOpenAPISpec(kts)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(body)
}
