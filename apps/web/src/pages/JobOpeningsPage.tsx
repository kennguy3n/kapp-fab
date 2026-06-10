import { useMemo, useState } from "react";
import { useNavigate } from "react-router-dom";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import type {
  JobOpening,
  JobOpeningInput,
  JobOpeningStatus,
  KRecord,
} from "@kapp/client";
import {
  Badge,
  Button,
  Input,
  Select,
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@kapp/ui";
import { api } from "../lib/api";

const EMPLOYMENT_TYPES: Array<{ value: string; label: string }> = [
  { value: "full_time", label: "Full-time" },
  { value: "part_time", label: "Part-time" },
  { value: "contract", label: "Contract" },
  { value: "intern", label: "Intern" },
];

// STATUS_BADGE maps an opening's lifecycle status to a Badge variant so
// the table communicates state at a glance, mirroring the colour
// language used across the other HR surfaces.
const STATUS_BADGE: Record<
  JobOpeningStatus,
  "default" | "accent" | "outline" | "success" | "warning" | "danger"
> = {
  draft: "outline",
  open: "success",
  on_hold: "warning",
  closed: "default",
  filled: "accent",
};

interface EmployeeData {
  name?: string;
}

/**
 * JobOpeningsPage lists recruitment job openings with their fill
 * progress and exposes the publish / close lifecycle actions plus an
 * inline create form. Openings are typed-table rows served by
 * /hr/recruitment/job-openings, so this page talks to the dedicated
 * client methods rather than the generic KRecord surface.
 */
export function JobOpeningsPage() {
  const qc = useQueryClient();
  const nav = useNavigate();
  const [statusFilter, setStatusFilter] = useState("");

  const openingsQ = useQuery({
    queryKey: ["recruitment", "job-openings", statusFilter],
    queryFn: () =>
      api.listJobOpenings(statusFilter ? { status: statusFilter } : undefined),
  });
  const employeesQ = useQuery({
    queryKey: ["records", "hr.employee"],
    queryFn: () => api.listRecords("hr.employee"),
  });

  const employeeName = useMemo(() => {
    const m = new Map<string, string>();
    (employeesQ.data ?? []).forEach((r: KRecord) => {
      const d = r.data as EmployeeData;
      if (d?.name) m.set(r.id, d.name);
    });
    return m;
  }, [employeesQ.data]);

  const lifecycleMut = useMutation({
    mutationFn: ({ id, action }: { id: string; action: "publish" | "close" }) =>
      action === "publish"
        ? api.publishJobOpening(id)
        : api.closeJobOpening(id),
    onSuccess: () =>
      qc.invalidateQueries({ queryKey: ["recruitment", "job-openings"] }),
  });

  const openings = openingsQ.data ?? [];

  return (
    <section className="flex flex-col gap-3">
      <h1 className="text-2xl font-semibold tracking-tight text-fg">
        Job Openings
      </h1>
      <p className="text-sm text-fg-muted">
        Requisitions tracked through draft → open → closed/filled. Publish
        opens an opening to applicants; close stops intake.
      </p>

      <CreateOpeningForm
        employees={employeesQ.data ?? []}
        onCreated={() =>
          qc.invalidateQueries({ queryKey: ["recruitment", "job-openings"] })
        }
      />

      <div className="flex items-center gap-2">
        <label className="text-sm text-fg-muted" htmlFor="status-filter">
          Status
        </label>
        <Select
          id="status-filter"
          value={statusFilter}
          onChange={(e) => setStatusFilter(e.target.value)}
          className="w-auto"
        >
          <option value="">All</option>
          <option value="draft">Draft</option>
          <option value="open">Open</option>
          <option value="on_hold">On hold</option>
          <option value="closed">Closed</option>
          <option value="filled">Filled</option>
        </Select>
      </div>

      {openingsQ.isLoading && <p className="text-sm text-fg-muted">Loading…</p>}
      {openingsQ.isError && (
        <p className="text-sm text-danger">{String(openingsQ.error)}</p>
      )}
      {!openingsQ.isLoading && openings.length === 0 ? (
        <p className="text-sm text-fg-muted">No job openings yet.</p>
      ) : (
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead>Title</TableHead>
              <TableHead>Department</TableHead>
              <TableHead>Type</TableHead>
              <TableHead>Hiring manager</TableHead>
              <TableHead>Filled</TableHead>
              <TableHead>Status</TableHead>
              <TableHead className="text-end">Actions</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {openings.map((o: JobOpening) => (
              <TableRow key={o.id}>
                <TableCell className="font-medium">{o.title}</TableCell>
                <TableCell>{o.department || "—"}</TableCell>
                <TableCell>{humanEmploymentType(o.employment_type)}</TableCell>
                <TableCell>
                  {o.hiring_manager_id
                    ? employeeName.get(o.hiring_manager_id) ?? "—"
                    : "—"}
                </TableCell>
                <TableCell>
                  {o.positions_filled}/{o.max_positions}
                </TableCell>
                <TableCell>
                  <Badge variant={STATUS_BADGE[o.status]}>{o.status}</Badge>
                </TableCell>
                <TableCell className="text-end">
                  <div className="flex justify-end gap-1">
                    <Button
                      size="sm"
                      variant="ghost"
                      onClick={() =>
                        nav(
                          `/hr/recruitment/applications?job_opening_id=${o.id}`,
                        )
                      }
                    >
                      Applications
                    </Button>
                    {(o.status === "draft" || o.status === "on_hold") && (
                      <Button
                        size="sm"
                        variant="outline"
                        disabled={lifecycleMut.isPending}
                        onClick={() =>
                          lifecycleMut.mutate({ id: o.id, action: "publish" })
                        }
                      >
                        Publish
                      </Button>
                    )}
                    {o.status !== "closed" && o.status !== "filled" && (
                      <Button
                        size="sm"
                        variant="outline"
                        disabled={lifecycleMut.isPending}
                        onClick={() =>
                          lifecycleMut.mutate({ id: o.id, action: "close" })
                        }
                      >
                        Close
                      </Button>
                    )}
                  </div>
                </TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      )}
      {lifecycleMut.isError && (
        <p className="text-sm text-danger">{String(lifecycleMut.error)}</p>
      )}
    </section>
  );
}

function CreateOpeningForm({
  employees,
  onCreated,
}: {
  employees: KRecord[];
  onCreated: () => void;
}) {
  const [open, setOpen] = useState(false);
  const [form, setForm] = useState<JobOpeningInput>({
    title: "",
    department: "",
    employment_type: "full_time",
    currency: "USD",
    max_positions: 1,
  });
  const [error, setError] = useState<string | null>(null);

  const createMut = useMutation({
    mutationFn: (input: JobOpeningInput) => api.createJobOpening(input),
    onSuccess: () => {
      setForm({
        title: "",
        department: "",
        employment_type: "full_time",
        currency: "USD",
        max_positions: 1,
      });
      setOpen(false);
      setError(null);
      onCreated();
    },
    onError: (e) => setError(String(e)),
  });

  if (!open) {
    return (
      <div>
        <Button size="sm" onClick={() => setOpen(true)}>
          New opening
        </Button>
      </div>
    );
  }

  return (
    <form
      className="flex flex-col gap-2 rounded-lg border border-border bg-bg-subtle p-3"
      onSubmit={(e) => {
        e.preventDefault();
        if (!form.title.trim()) {
          setError("Title is required");
          return;
        }
        createMut.mutate(form);
      }}
    >
      <div className="grid grid-cols-1 gap-2 sm:grid-cols-2">
        <Input
          placeholder="Title (e.g. Senior Engineer)"
          value={form.title}
          onChange={(e) => setForm({ ...form, title: e.target.value })}
          required
        />
        <Input
          placeholder="Department"
          value={form.department ?? ""}
          onChange={(e) => setForm({ ...form, department: e.target.value })}
        />
        <Select
          value={form.employment_type}
          onChange={(e) =>
            setForm({ ...form, employment_type: e.target.value })
          }
        >
          {EMPLOYMENT_TYPES.map((t) => (
            <option key={t.value} value={t.value}>
              {t.label}
            </option>
          ))}
        </Select>
        <Input
          placeholder="Location"
          value={form.location ?? ""}
          onChange={(e) => setForm({ ...form, location: e.target.value })}
        />
        <Select
          value={form.hiring_manager_id ?? ""}
          onChange={(e) =>
            setForm({ ...form, hiring_manager_id: e.target.value || undefined })
          }
        >
          <option value="">Hiring manager…</option>
          {employees.map((emp) => (
            <option key={emp.id} value={emp.id}>
              {(emp.data as EmployeeData)?.name ?? emp.id}
            </option>
          ))}
        </Select>
        <Input
          type="number"
          min={1}
          placeholder="Max positions"
          value={form.max_positions ?? 1}
          onChange={(e) =>
            setForm({ ...form, max_positions: Number(e.target.value) || 1 })
          }
        />
        <Input
          placeholder="Currency"
          value={form.currency ?? ""}
          onChange={(e) => setForm({ ...form, currency: e.target.value })}
        />
      </div>
      {error && <p className="text-sm text-danger">{error}</p>}
      <div className="flex gap-2">
        <Button size="sm" type="submit" disabled={createMut.isPending}>
          {createMut.isPending ? "Creating…" : "Create"}
        </Button>
        <Button
          size="sm"
          type="button"
          variant="ghost"
          onClick={() => {
            setOpen(false);
            setError(null);
          }}
        >
          Cancel
        </Button>
      </div>
    </form>
  );
}

function humanEmploymentType(t: string): string {
  return EMPLOYMENT_TYPES.find((e) => e.value === t)?.label ?? t;
}
