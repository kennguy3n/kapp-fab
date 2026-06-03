import { useMemo } from "react";
import { useNavigate } from "react-router-dom";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import type { KRecord } from "@kapp/client";
import { api } from "../lib/api";

// OnboardingChecklistPage renders the persistent "Getting Started"
// checklist that Workstream 8 seeds for every new tenant
// (internal/tenant/wizard.go seedGettingStartedChecklist). It tracks
// first-use milestones — create a contact, send an invoice, import
// data — links each step to the page that completes it, lets the
// operator tick steps off, and can be dismissed once everything is
// done.
//
// The checklist is stored as a tasks.task KRecord with
// data.onboarding === "checklist" and a data.steps array. We round-
// trip the whole steps array through PATCH /records so toggling a step
// (or dismissing the checklist) is an ordinary record update with the
// usual tenant-scoped RLS — no bespoke endpoint required.

// CHECKLIST_KTYPE mirrors tenant.gettingStartedKType in
// internal/tenant/wizard.go.
const CHECKLIST_KTYPE = "tasks.task";

interface ChecklistStep {
  key: string;
  label: string;
  done: boolean;
  link?: string;
}

interface ChecklistData {
  title?: string;
  description?: string;
  onboarding?: string;
  dismissed?: boolean;
  steps?: ChecklistStep[];
}

// findChecklist returns the onboarding checklist record from a task
// list, preferring the most recently created one if (defensively)
// more than one exists.
function findChecklist(records: KRecord[]): KRecord | undefined {
  return records
    .filter((r) => (r.data as ChecklistData).onboarding === "checklist")
    .sort((a, b) => b.created_at.localeCompare(a.created_at))[0];
}

function readSteps(record: KRecord | undefined): ChecklistStep[] {
  const data = (record?.data ?? {}) as ChecklistData;
  return Array.isArray(data.steps) ? data.steps : [];
}

export function OnboardingChecklistPage() {
  const navigate = useNavigate();
  const qc = useQueryClient();

  // We fetch the tasks.task list and pick the onboarding record
  // client-side (findChecklist). A server-side `data.onboarding =
  // 'checklist'` predicate would be leaner, but the REST list endpoint
  // (services/api/records.go) intentionally exposes only
  // status/cursor/limit/offset — arbitrary JSONB field filtering lives
  // in record.PGStore.ListByField, which is reachable from Go callers
  // (the /getting-started KChat command uses it) but not over REST.
  // Threading a field predicate through the handler + ListFilter +
  // client SDK is the right long-term fix, but those are shared files
  // outside this workstream; RecordListPage carries the same caveat.
  // The cost here is bounded: exactly one checklist task exists per
  // tenant and the list is fetched once on mount.
  const tasksQuery = useQuery({
    queryKey: ["records", CHECKLIST_KTYPE],
    queryFn: () => api.listRecords(CHECKLIST_KTYPE),
  });

  const checklist = useMemo(
    () => findChecklist(tasksQuery.data ?? []),
    [tasksQuery.data],
  );
  const steps = useMemo(() => readSteps(checklist), [checklist]);
  const data = (checklist?.data ?? {}) as ChecklistData;

  const completed = steps.filter((s) => s.done).length;
  const total = steps.length;
  const allDone = total > 0 && completed === total;

  // updateChecklist PATCHes the whole data payload (merging the new
  // steps / dismissed flag) and refetches the task list so every
  // surface that reads the checklist stays in sync.
  const updateChecklist = useMutation({
    mutationFn: (patch: Partial<ChecklistData>) => {
      if (!checklist) {
        return Promise.reject(new Error("no checklist to update"));
      }
      return api.updateRecord(CHECKLIST_KTYPE, checklist.id, {
        ...(checklist.data as Record<string, unknown>),
        ...patch,
      });
    },
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["records", CHECKLIST_KTYPE] });
    },
  });

  const toggleStep = (key: string, done: boolean) => {
    updateChecklist.mutate({
      steps: steps.map((s) => (s.key === key ? { ...s, done } : s)),
    });
  };

  if (tasksQuery.isLoading) {
    return <div>Loading…</div>;
  }
  if (tasksQuery.error) {
    return <div>Error loading your Getting Started checklist.</div>;
  }
  if (!checklist) {
    return (
      <section style={{ maxWidth: 640 }}>
        <h1>Getting Started</h1>
        <p style={{ color: "#6b7280" }}>
          No onboarding checklist found for this workspace. It is created
          automatically when a tenant is set up.
        </p>
      </section>
    );
  }

  if (data.dismissed) {
    return (
      <section style={{ maxWidth: 640 }}>
        <h1>Getting Started</h1>
        <p style={{ color: "#6b7280" }}>
          You dismissed your Getting Started checklist. Nice work setting up
          your workspace!
        </p>
        <button
          type="button"
          onClick={() => updateChecklist.mutate({ dismissed: false })}
          disabled={updateChecklist.isPending}
        >
          Show it again
        </button>
      </section>
    );
  }

  return (
    <section style={{ maxWidth: 640 }}>
      <header
        style={{
          display: "flex",
          justifyContent: "space-between",
          alignItems: "baseline",
          gap: 12,
        }}
      >
        <h1 style={{ margin: 0 }}>{data.title || "Getting Started"}</h1>
        <span style={{ color: "#6b7280", fontSize: 13 }}>
          {completed} / {total} done
        </span>
      </header>
      {data.description ? (
        <p style={{ color: "#6b7280" }}>{data.description}</p>
      ) : null}

      {/* Progress bar — a simple, dependency-free fill so the operator
          sees momentum as they tick steps off. */}
      <div
        aria-hidden="true"
        style={{
          height: 8,
          borderRadius: 999,
          background: "#e5e7eb",
          overflow: "hidden",
          margin: "8px 0 16px",
        }}
      >
        <div
          style={{
            height: "100%",
            width: total > 0 ? `${(completed / total) * 100}%` : "0%",
            background: "#10b981",
            transition: "width 150ms ease",
          }}
        />
      </div>

      <ul style={{ listStyle: "none", padding: 0, margin: 0, display: "grid", gap: 8 }}>
        {steps.map((step) => (
          <li
            key={step.key}
            style={{
              display: "flex",
              alignItems: "center",
              gap: 10,
              padding: "10px 12px",
              border: "1px solid #e5e7eb",
              borderRadius: 8,
              background: step.done ? "#f0fdf4" : "#fff",
            }}
          >
            <input
              type="checkbox"
              checked={step.done}
              aria-label={step.label}
              onChange={(e) => toggleStep(step.key, e.target.checked)}
              disabled={updateChecklist.isPending}
            />
            <span
              style={{
                flex: 1,
                textDecoration: step.done ? "line-through" : "none",
                color: step.done ? "#6b7280" : "#111827",
              }}
            >
              {step.label}
            </span>
            {step.link ? (
              <button type="button" onClick={() => navigate(step.link!)}>
                {step.done ? "Open" : "Do it"}
              </button>
            ) : null}
          </li>
        ))}
      </ul>

      <div style={{ marginTop: 16 }}>
        <button
          type="button"
          onClick={() => updateChecklist.mutate({ dismissed: true })}
          disabled={!allDone || updateChecklist.isPending}
          title={
            allDone
              ? "Hide the checklist"
              : "Finish every step to dismiss the checklist"
          }
        >
          Dismiss checklist
        </button>
      </div>
      {updateChecklist.isError ? (
        <p style={{ color: "#b91c1c", fontSize: 13 }}>
          Couldn't save your change. Please try again.
        </p>
      ) : null}
    </section>
  );
}
