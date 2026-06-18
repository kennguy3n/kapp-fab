import { useMemo, useState } from "react";
import { useNavigate } from "react-router-dom";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import type { JobOpening, JobOpeningInput, KRecord } from "@kapp/client";
import {
  Badge,
  Button,
  ConfirmDialog,
  EmptyState,
  Eyebrow,
  Field,
  Input,
  Modal,
  ModalContent,
  ModalDescription,
  ModalFooter,
  ModalHeader,
  ModalTitle,
  Select,
  Skeleton,
  Table,
  TableBody,
  TableCaption,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
  Textarea,
  toast,
} from "@kapp/ui";
import {
  AlertTriangle,
  Briefcase,
  Plus,
  RefreshCw,
} from "lucide-react";
import { api } from "../lib/api";
import { useFormatter } from "../lib/i18n/useFormatter";
import { humanizeToken } from "../lib/ktypeView";
import { openingVariant } from "../lib/recruitmentStatus";

const EMPLOYMENT_TYPES: Array<{ value: string; label: string }> = [
  { value: "full_time", label: "Full-time" },
  { value: "part_time", label: "Part-time" },
  { value: "contract", label: "Contract" },
  { value: "intern", label: "Intern" },
];

const CURRENCIES = ["USD", "EUR", "GBP", "CAD", "AUD", "INR", "SGD"];

const STATUS_FILTERS: Array<{ value: string; label: string }> = [
  { value: "", label: "All statuses" },
  { value: "draft", label: "Draft" },
  { value: "open", label: "Open" },
  { value: "on_hold", label: "On hold" },
  { value: "closed", label: "Closed" },
  { value: "filled", label: "Filled" },
];

interface EmployeeData {
  name?: string;
}

/**
 * JobOpeningsPage lists recruitment requisitions with their fill
 * progress and exposes the publish / close lifecycle plus a guided
 * create form. Openings are typed-table rows, so this page talks to the
 * dedicated recruitment client methods rather than the generic record
 * surface.
 */
export function JobOpeningsPage() {
  const fmt = useFormatter();
  const qc = useQueryClient();
  const nav = useNavigate();
  const [statusFilter, setStatusFilter] = useState("");
  const [createOpen, setCreateOpen] = useState(false);
  const [closeTarget, setCloseTarget] = useState<JobOpening | null>(null);

  const openingsQ = useQuery<JobOpening[]>({
    queryKey: ["recruitment", "job-openings", statusFilter],
    queryFn: () =>
      api.listJobOpenings(statusFilter ? { status: statusFilter } : undefined),
  });
  const employeesQ = useQuery<KRecord[]>({
    queryKey: ["records", "hr.employee"],
    queryFn: () => api.listRecords("hr.employee"),
  });

  const employeeName = useMemo(() => {
    const m = new Map<string, string>();
    (employeesQ.data ?? []).forEach((r) => {
      const d = r.data as EmployeeData;
      if (d?.name) m.set(r.id, d.name);
    });
    return m;
  }, [employeesQ.data]);

  const publishMut = useMutation({
    mutationFn: (id: string) => api.publishJobOpening(id),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["recruitment", "job-openings"] });
      toast.success("Opening published");
    },
    onError: (e) => toast.error("Couldn't publish", { description: String(e) }),
  });

  const closeMut = useMutation({
    mutationFn: (id: string) => api.closeJobOpening(id),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["recruitment", "job-openings"] });
      setCloseTarget(null);
      toast.success("Opening closed");
    },
    onError: (e) => toast.error("Couldn't close", { description: String(e) }),
  });

  const openings = openingsQ.data ?? [];

  return (
    <section className="flex flex-col gap-4">
      <header className="flex flex-wrap items-start justify-between gap-3">
        <div className="min-w-0">
          <Eyebrow>Human Resources</Eyebrow>
          <h1 className="mt-1 truncate text-2xl font-semibold tracking-tight text-fg">
            Job Openings
          </h1>
          <p className="mt-1 text-sm text-fg-muted">
            Track each role from draft to filled. Publish to open it to
            applicants; close to stop taking applications.
          </p>
        </div>
        <Button
          size="sm"
          leadingIcon={<Plus className="h-4 w-4" />}
          onClick={() => setCreateOpen(true)}
        >
          New opening
        </Button>
      </header>

      <div className="flex items-center gap-2">
        <label className="text-sm font-medium text-fg-muted" htmlFor="status-filter">
          Status
        </label>
        <Select
          id="status-filter"
          size="sm"
          value={statusFilter}
          onChange={(e) => setStatusFilter(e.target.value)}
          className="w-auto"
        >
          {STATUS_FILTERS.map((s) => (
            <option key={s.value} value={s.value}>
              {s.label}
            </option>
          ))}
        </Select>
      </div>

      {openingsQ.isError && (
        <div
          role="alert"
          className="flex flex-wrap items-center gap-3 rounded-md border border-danger/30 bg-danger/5 px-3 py-2 text-sm text-fg"
        >
          <AlertTriangle className="h-4 w-4 shrink-0 text-danger" aria-hidden />
          <span className="min-w-0 flex-1">We couldn't load job openings.</span>
          <Button
            size="sm"
            variant="outline"
            leadingIcon={<RefreshCw className="h-4 w-4" />}
            onClick={() => openingsQ.refetch()}
          >
            Retry
          </Button>
        </div>
      )}

      {!openingsQ.isError && (
        <Table>
          <TableCaption>
            {openingsQ.isLoading
              ? "Loading job openings…"
              : `${fmt.number(openings.length)} ${
                  openings.length === 1 ? "opening" : "openings"
                }${statusFilter ? " in this status" : ""}`}
          </TableCaption>
          <TableHeader>
            <TableRow>
              <TableHead>Role</TableHead>
              <TableHead>Department</TableHead>
              <TableHead>Type</TableHead>
              <TableHead>Hiring manager</TableHead>
              <TableHead className="text-end">Filled</TableHead>
              <TableHead>Status</TableHead>
              <TableHead className="text-end">Actions</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {openingsQ.isLoading ? (
              Array.from({ length: 4 }).map((_, i) => (
                <TableRow key={i}>
                  {Array.from({ length: 7 }).map((__, j) => (
                    <TableCell key={j}>
                      <Skeleton className="h-4 w-full" />
                    </TableCell>
                  ))}
                </TableRow>
              ))
            ) : openings.length === 0 ? (
              <TableRow>
                <TableCell colSpan={7}>
                  <EmptyState
                    icon={<Briefcase />}
                    title={
                      statusFilter
                        ? "No openings in this status"
                        : "No job openings yet"
                    }
                    description={
                      statusFilter
                        ? "Try a different status filter, or create a new opening."
                        : "Create your first opening to start receiving applications."
                    }
                    action={
                      <Button size="sm" onClick={() => setCreateOpen(true)}>
                        New opening
                      </Button>
                    }
                  />
                </TableCell>
              </TableRow>
            ) : (
              openings.map((o) => (
                <TableRow key={o.id}>
                  <TableCell>
                    <div className="font-medium text-fg">{o.title}</div>
                    {o.location && (
                      <div className="text-xs text-fg-subtle">{o.location}</div>
                    )}
                  </TableCell>
                  <TableCell>{o.department || "—"}</TableCell>
                  <TableCell>{humanEmploymentType(o.employment_type)}</TableCell>
                  <TableCell>
                    {o.hiring_manager_id
                      ? employeeName.get(o.hiring_manager_id) ?? "—"
                      : "—"}
                  </TableCell>
                  <TableCell className="text-end tabular-nums">
                    {fmt.number(o.positions_filled)} of{" "}
                    {fmt.number(o.max_positions)}
                  </TableCell>
                  <TableCell>
                    <Badge variant={openingVariant(o.status)}>
                      {humanizeToken(o.status)}
                    </Badge>
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
                        Applicants
                      </Button>
                      {(o.status === "draft" || o.status === "on_hold") && (
                        <Button
                          size="sm"
                          variant="outline"
                          disabled={
                            publishMut.isPending && publishMut.variables === o.id
                          }
                          onClick={() => publishMut.mutate(o.id)}
                        >
                          Publish
                        </Button>
                      )}
                      {o.status !== "closed" && o.status !== "filled" && (
                        <Button
                          size="sm"
                          variant="outline"
                          onClick={() => setCloseTarget(o)}
                        >
                          Close
                        </Button>
                      )}
                    </div>
                  </TableCell>
                </TableRow>
              ))
            )}
          </TableBody>
        </Table>
      )}

      <CreateOpeningModal
        open={createOpen}
        onOpenChange={setCreateOpen}
        employees={employeesQ.data ?? []}
        onCreated={() =>
          qc.invalidateQueries({ queryKey: ["recruitment", "job-openings"] })
        }
      />

      <ConfirmDialog
        open={closeTarget !== null}
        onOpenChange={(o) => !o && setCloseTarget(null)}
        title="Close this opening?"
        description={
          closeTarget
            ? `“${closeTarget.title}” will stop accepting new applications. You can't reopen a closed opening.`
            : ""
        }
        confirmLabel="Close opening"
        destructive
        loading={closeMut.isPending}
        onConfirm={() => closeTarget && closeMut.mutate(closeTarget.id)}
      />
    </section>
  );
}

function CreateOpeningModal({
  open,
  onOpenChange,
  employees,
  onCreated,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  employees: KRecord[];
  onCreated: () => void;
}) {
  return (
    <Modal open={open} onOpenChange={onOpenChange}>
      <ModalContent className="max-w-2xl">
        <ModalHeader>
          <ModalTitle>New job opening</ModalTitle>
          <ModalDescription>
            Describe the role. You can publish it to applicants once it's
            ready.
          </ModalDescription>
        </ModalHeader>
        {/* Rendered inside ModalContent so it mounts fresh on each open,
            resetting the form and the create mutation without seed state. */}
        <CreateOpeningForm
          employees={employees}
          onCreated={onCreated}
          onClose={() => onOpenChange(false)}
        />
      </ModalContent>
    </Modal>
  );
}

function CreateOpeningForm({
  employees,
  onCreated,
  onClose,
}: {
  employees: KRecord[];
  onCreated: () => void;
  onClose: () => void;
}) {
  const empty: JobOpeningInput = {
    title: "",
    department: "",
    employment_type: "full_time",
    currency: "USD",
    max_positions: 1,
  };
  const [form, setForm] = useState<JobOpeningInput>(empty);
  const [submitted, setSubmitted] = useState(false);

  const createMut = useMutation({
    mutationFn: (input: JobOpeningInput) => api.createJobOpening(input),
    onSuccess: () => {
      toast.success("Opening created");
      onClose();
      onCreated();
    },
    onError: (e) =>
      toast.error("Couldn't create opening", { description: String(e) }),
  });

  const titleError = submitted && !form.title.trim();

  function patch(next: Partial<JobOpeningInput>) {
    setForm((f) => ({ ...f, ...next }));
  }

  return (
    <form
      className="flex flex-col gap-4"
      onSubmit={(e) => {
        e.preventDefault();
        setSubmitted(true);
        if (!form.title.trim()) return;
        createMut.mutate({
          ...form,
          title: form.title.trim(),
          department: form.department?.trim() || undefined,
          location: form.location?.trim() || undefined,
        });
      }}
    >
      <Field
        label="Role title"
        required
        error={titleError ? "Give the role a title." : undefined}
      >
        <Input
          value={form.title}
          onChange={(e) => patch({ title: e.target.value })}
          placeholder="e.g. Senior Software Engineer"
          invalid={titleError || undefined}
        />
      </Field>

      <div className="grid grid-cols-1 gap-4 sm:grid-cols-2">
        <Field label="Department">
          <Input
            value={form.department ?? ""}
            onChange={(e) => patch({ department: e.target.value })}
            placeholder="e.g. Engineering"
          />
        </Field>
        <Field label="Location">
          <Input
            value={form.location ?? ""}
            onChange={(e) => patch({ location: e.target.value })}
            placeholder="e.g. Remote · London"
          />
        </Field>
        <Field label="Employment type">
          <Select
            value={form.employment_type}
            onChange={(e) => patch({ employment_type: e.target.value })}
          >
            {EMPLOYMENT_TYPES.map((t) => (
              <option key={t.value} value={t.value}>
                {t.label}
              </option>
            ))}
          </Select>
        </Field>
        <Field label="Hiring manager">
          <Select
            value={form.hiring_manager_id ?? ""}
            onChange={(e) =>
              patch({ hiring_manager_id: e.target.value || undefined })
            }
          >
            <option value="">Unassigned</option>
            {employees.map((emp) => (
              <option key={emp.id} value={emp.id}>
                {(emp.data as EmployeeData)?.name ?? "Unnamed"}
              </option>
            ))}
          </Select>
        </Field>
      </div>

      <fieldset className="grid grid-cols-1 gap-4 sm:grid-cols-3">
        <Field label="Openings" help="How many people to hire.">
          <Input
            type="number"
            min={1}
            value={form.max_positions ?? 1}
            onChange={(e) =>
              patch({ max_positions: Number(e.target.value) || 1 })
            }
          />
        </Field>
        <Field label="Pay range (min)">
          <Input
            type="number"
            min={0}
            inputMode="decimal"
            value={form.salary_range_min ?? ""}
            onChange={(e) =>
              patch({ salary_range_min: e.target.value || undefined })
            }
            placeholder="e.g. 80000"
          />
        </Field>
        <Field label="Pay range (max)">
          <div className="flex gap-2">
            <Input
              type="number"
              min={0}
              inputMode="decimal"
              value={form.salary_range_max ?? ""}
              onChange={(e) =>
                patch({ salary_range_max: e.target.value || undefined })
              }
              placeholder="e.g. 120000"
            />
            <Select
              className="w-24"
              aria-label="Currency"
              value={form.currency}
              onChange={(e) => patch({ currency: e.target.value })}
            >
              {CURRENCIES.map((c) => (
                <option key={c} value={c}>
                  {c}
                </option>
              ))}
            </Select>
          </div>
        </Field>
      </fieldset>

      <Field label="Description" help="What the role is about (optional).">
        <Textarea
          rows={3}
          value={form.description ?? ""}
          onChange={(e) => patch({ description: e.target.value })}
          placeholder="Summarise the role and what you're looking for."
        />
      </Field>

      {createMut.isError && (
        <p className="text-sm text-danger">
          Couldn't create the opening: {String(createMut.error)}
        </p>
      )}

      <ModalFooter>
        <Button type="button" variant="ghost" onClick={onClose}>
          Cancel
        </Button>
        <Button type="submit" disabled={createMut.isPending}>
          {createMut.isPending ? "Creating…" : "Create opening"}
        </Button>
      </ModalFooter>
    </form>
  );
}

function humanEmploymentType(t: string): string {
  return EMPLOYMENT_TYPES.find((e) => e.value === t)?.label ?? humanizeToken(t);
}
