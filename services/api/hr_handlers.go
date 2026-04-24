package main

import (
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/kennguy3n/kapp-fab/internal/hr"
	"github.com/kennguy3n/kapp-fab/internal/platform"
)

// hrHandlers exposes the Phase J pay-run engine to the HTTP layer.
// The actual engine lives in internal/hr; this file is only wiring.
type hrHandlers struct {
	engine *hr.PayrollEngine
}

// generatePayslips materialises draft payslips for every eligible
// employee against a pay_run. Idempotent per (pay_run_id,
// employee_id).
func (h *hrHandlers) generatePayslips(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid pay_run id", http.StatusBadRequest)
		return
	}
	result, err := h.engine.GeneratePayslips(r.Context(), t.ID, id, actorOrDefault(r.Context()))
	if err != nil {
		writePayrollError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// postPayRun posts all approved payslips for the run as a single
// journal entry (Dr salary expense / Cr salary payable).
func (h *hrHandlers) postPayRun(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid pay_run id", http.StatusBadRequest)
		return
	}
	entry, err := h.engine.PostPayRun(r.Context(), t.ID, id, actorOrDefault(r.Context()))
	if err != nil {
		writePayrollError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, entry)
}

// listPayRunPayslips returns every payslip KRecord attached to a
// pay_run. Exists because the generic records list route caps at
// 500 rows and defaults to 50 — on tenants with >50 total payslips
// across all runs the frontend's "View slips" panel would silently
// truncate. The engine-backed path walks every row via ListAll and
// filters in the tenant transaction, so it's always complete.
func (h *hrHandlers) listPayRunPayslips(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid pay_run id", http.StatusBadRequest)
		return
	}
	slips, err := h.engine.ListPayslipsForRun(r.Context(), t.ID, id)
	if err != nil {
		writePayrollError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, slips)
}

// writePayrollError maps engine sentinels onto HTTP status codes so
// the UI can distinguish "no employees in scope" (422) from "pay_run
// not found" (404) from generic failures (500).
func writePayrollError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, hr.ErrPayRunNotFound):
		http.Error(w, err.Error(), http.StatusNotFound)
	case errors.Is(err, hr.ErrPayRunWrongStatus),
		errors.Is(err, hr.ErrNoActiveEmployees),
		errors.Is(err, hr.ErrNoApprovedSlips),
		errors.Is(err, hr.ErrMissingAccounts):
		http.Error(w, err.Error(), http.StatusUnprocessableEntity)
	default:
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}
