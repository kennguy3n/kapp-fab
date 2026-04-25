package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"

	"github.com/google/uuid"

	"github.com/kennguy3n/kapp-fab/internal/lms"
	"github.com/kennguy3n/kapp-fab/internal/record"
	"github.com/kennguy3n/kapp-fab/internal/scheduler"
)

// CertificateActionType is the scheduled_actions.action_type the
// tenant wizard seeds for the certificate auto-issue worker.
const CertificateActionType = "lms_issue_certificates"

// CertificateAutoIssuer is a scheduler ActionHandler that walks
// completed lms.enrollment KRecords for a tenant and issues an
// lms.certificate for each one that does not already have one.
//
// Polling vs. event-driven: enrollment completion events are not
// guaranteed to flow through the outbox (workflow transitions are
// the canonical signal but tests can short-circuit). Polling keeps
// the issuer authoritative and idempotent — the partial unique
// index in 000037_lms_certificates.sql ensures we never duplicate.
type CertificateAutoIssuer struct {
	issuer  *lms.CertificateIssuer
	records *record.PGStore
	actor   uuid.UUID
}

// NewCertificateAutoIssuer wires the worker. systemActor is stamped
// as created_by on every certificate the worker issues.
func NewCertificateAutoIssuer(issuer *lms.CertificateIssuer, records *record.PGStore, systemActor uuid.UUID) *CertificateAutoIssuer {
	return &CertificateAutoIssuer{issuer: issuer, records: records, actor: systemActor}
}

// Handle implements scheduler.ActionHandler. Walks every active
// lms.enrollment for the tenant; for each one whose status is
// "completed", issues a certificate. Errors on individual rows are
// logged and the loop continues — one bad enrollment never blocks
// the rest.
func (a *CertificateAutoIssuer) Handle(ctx context.Context, tenantID uuid.UUID, _ scheduler.ScheduledAction) error {
	if a.records == nil || a.issuer == nil {
		return errors.New("certificate-auto-issuer: not configured")
	}
	rows, err := a.records.ListAll(ctx, tenantID, record.ListFilter{
		KType:  lms.KTypeEnrollment,
		Status: "active",
	})
	if err != nil {
		return fmt.Errorf("certificate-auto-issuer: list enrollments: %w", err)
	}
	issued := 0
	for _, r := range rows {
		var body map[string]any
		if err := json.Unmarshal(r.Data, &body); err != nil {
			log.Printf("certificate-auto-issuer: decode %s: %v", r.ID, err)
			continue
		}
		if status, _ := body["status"].(string); status != "completed" {
			continue
		}
		_, err := a.issuer.IssueCertificate(ctx, tenantID, r.ID, a.actor, lms.CertificateOptions{})
		switch {
		case err == nil:
			issued++
		case errors.Is(err, lms.ErrCertificateAlreadyIssued):
			// expected on every re-run after the first issuance
		default:
			log.Printf("certificate-auto-issuer: enrollment %s: %v", r.ID, err)
		}
	}
	if issued > 0 {
		log.Printf("certificate-auto-issuer: tenant %s issued %d certificates", tenantID, issued)
	}
	return nil
}
