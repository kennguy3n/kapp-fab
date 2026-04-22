package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"github.com/kennguy3n/kapp-fab/internal/ktype"
)

type ktypeHandlers struct {
	registry *ktype.PGRegistry
}

type registerKTypeRequest struct {
	Name    string          `json:"name"`
	Version int             `json:"version"`
	Schema  json.RawMessage `json:"schema"`
}

func (h *ktypeHandlers) register(w http.ResponseWriter, r *http.Request) {
	var req registerKTypeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	if err := h.registry.Register(r.Context(), ktype.KType{
		Name:    req.Name,
		Version: req.Version,
		Schema:  req.Schema,
	}); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"name":    req.Name,
		"version": req.Version,
	})
}

func (h *ktypeHandlers) list(w http.ResponseWriter, r *http.Request) {
	kts, err := h.registry.List(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, kts)
}

func (h *ktypeHandlers) get(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	version := 0
	if v := r.URL.Query().Get("version"); v != "" {
		parsed, err := strconv.Atoi(v)
		if err != nil || parsed <= 0 {
			http.Error(w, "invalid version", http.StatusBadRequest)
			return
		}
		version = parsed
	}
	kt, err := h.registry.Get(r.Context(), name, version)
	if err != nil {
		if errors.Is(err, ktype.ErrNotFound) {
			http.Error(w, "ktype not found", http.StatusNotFound)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, kt)
}
