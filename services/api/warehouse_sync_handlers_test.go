package main

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/kennguy3n/kapp-fab/internal/warehouse"
)

// TestWriteWarehouseError verifies the HTTP error mapper routes each
// sentinel class to the right status via errors.Is, including across
// an fmt.Errorf("%w: …") wrap (the form ConfigStore uses for invalid
// configs). A regression here would surface client-correctable
// problems (e.g. a duplicate name) as opaque 500s.
func TestWriteWarehouseError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"config not found", warehouse.ErrConfigNotFound, http.StatusNotFound},
		{"run not found", warehouse.ErrRunNotFound, http.StatusNotFound},
		{"invalid config sentinel", warehouse.ErrInvalidConfig, http.StatusBadRequest},
		{
			name: "invalid config wrapped",
			err:  fmt.Errorf("%w: name already in use", warehouse.ErrInvalidConfig),
			want: http.StatusBadRequest,
		},
		{
			name: "config not found wrapped",
			err:  fmt.Errorf("warehouse: update config: %w", warehouse.ErrConfigNotFound),
			want: http.StatusNotFound,
		},
		{"unknown error", errors.New("boom"), http.StatusInternalServerError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			writeWarehouseError(rec, tc.err)
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d", rec.Code, tc.want)
			}
		})
	}
}
