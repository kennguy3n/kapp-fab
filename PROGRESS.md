# Kapp Business Suite — Development Progress

This file tracks delivery against the roadmap in
[PHASES.md](./PHASES.md). For the per-phase deliverable checklists,
the "Cross-cutting" platform-primitive inventory, and the First-
Coding-Slice acceptance checklist, see
[`docs/DEVELOPMENT_LOG.md`](docs/DEVELOPMENT_LOG.md).

## Current phase

**Phase M — Vertical Depth (~95% complete).** Phases A–G and Phase L
are closed. Phase M has landed across PRs #50–#61: Insights SQL editor
mode, US + AU payroll tax packs, shift scheduling with KChat presence
and a calendar UI, performance-review surface, projects + milestones
with Gantt, the POS module with offline queue, advanced accounting
consolidation, webhook v2 (conditional matching + per-webhook retries
+ delivery log), demo mode with mock data + screenshots, and the RBAC
depth pass behind the `KAPP_AUTHZ_ENFORCE` toggle.

The notebook / exploratory analysis interface (deferred from Phase L,
re-scoped into Phase M) is the only outstanding item.

## Phase summary

| Phase | Focus | Status |
|---|---|---|
| A | Kapp Kernel | Complete |
| B | CRM, Tasks, Approvals, Forms | Complete |
| C | Finance Basics | Complete |
| D | Simple Inventory | Complete |
| E | HR and LMS Starters | Complete |
| F | Importer and Base | Complete |
| G | Hardening, Observability, Scale | Complete |
| L | Insights | Complete |
| M | Vertical Depth | ~95% — notebook UI pending |

See [PHASES.md](./PHASES.md) for the canonical phase definitions and
[`docs/DEVELOPMENT_LOG.md`](docs/DEVELOPMENT_LOG.md) for the
per-deliverable checklists.

## Phase M — outstanding work

- [ ] Notebook / exploratory analysis interface (deferred from
  Phase L; now scoped inside Phase M).

## Known gaps and next steps

- **Notebook UI.** Last open Phase M deliverable; depends on the
  Insights query runner already shipped in PR #48 + #50.
- **Frontend test harness.** Backend coverage is solid (55+ Go test
  files across `internal/` and `services/`); a Vitest + Playwright
  harness for `apps/web/` is the natural next investment but is not
  currently scheduled.
- **Multi-region rollout.** Cells are autoscaler-ready and the
  scoped `kapp_tier_admin` SECURITY DEFINER role shipped in PR #47,
  but the multi-region cell topology + per-region failover runbook
  remain operator-driven.

See [`docs/DEVELOPMENT_LOG.md`](docs/DEVELOPMENT_LOG.md) for the
full historical record.
