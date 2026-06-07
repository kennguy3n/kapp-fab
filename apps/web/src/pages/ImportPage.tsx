import { useState, useMemo } from "react";
import { Link, useParams, useNavigate } from "react-router-dom";
import { useQuery, useMutation, useQueryClient } from "@tanstack/react-query";
import {
  Button,
  Input,
  Select,
  Stepper,
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@kapp/ui";

/**
 * ImportPage drives the Phase F import wizard. Five steps:
 *
 *   1. Source selection   — CSV / JSON / Frappe REST
 *   2. Concept mapping    — source DocType → KType, source field → KType field
 *   3. Validation report  — per-row errors from POST /imports/{id}/validate
 *   4. Review             — reconciliation summary + counts
 *   5. Cutover            — POST /imports/{id}/accept
 *
 * Route shape:
 *   /imports               — index: recent import jobs for the tenant
 *   /imports/new           — wizard starting at step 1
 *   /imports/:id           — wizard resuming a specific job from the
 *                            step matching its current status
 *
 * The page uses /api/v1/imports directly (not the generated client)
 * because the Phase F REST surface is still shaping up — keeping the
 * fetch calls inline avoids churn on the shared packages/client while
 * the contract stabilizes.
 */

interface ImportJob {
  id: string;
  tenant_id: string;
  source_type: string;
  status: string;
  config: Record<string, unknown>;
  mapping: Record<string, unknown>;
  progress: Record<string, unknown>;
  errors: unknown;
  reconciliation: Record<string, unknown>;
  created_by: string;
  created_at: string;
  updated_at: string;
  completed_at?: string | null;
}

interface StagingRow {
  id: number;
  job_id: string;
  source_type: string;
  source_id?: string;
  target_ktype: string;
  data: Record<string, unknown>;
  validation_errors: Array<{ field?: string; code: string; message: string }>;
  status: string;
}

const baseUrl = "/api/v1";

function tenantId(): string {
  return localStorage.getItem("kapp.tenant") ?? "default";
}

async function apiFetch<T>(path: string, init?: RequestInit): Promise<T> {
  const res = await fetch(`${baseUrl}${path}`, {
    ...init,
    headers: {
      "Content-Type": "application/json",
      "X-Tenant-ID": tenantId(),
      ...(init?.method && init.method !== "GET"
        ? { "Idempotency-Key": crypto.randomUUID() }
        : {}),
      ...(init?.headers ?? {}),
    },
  });
  if (!res.ok) throw new Error(`${res.status} ${res.statusText}`);
  if (res.status === 204) return undefined as T;
  return (await res.json()) as T;
}

export function ImportPage() {
  const { id } = useParams<{ id?: string }>();
  if (id && id !== "new") return <ImportWizard jobId={id} />;
  if (id === "new") return <ImportWizard jobId={undefined} />;
  return <ImportIndex />;
}

function ImportIndex() {
  const jobs = useQuery({
    queryKey: ["imports"],
    queryFn: () => apiFetch<ImportJob[]>("/imports"),
  });

  return (
    <section>
      <div className="flex justify-between">
        <h1>Imports</h1>
        <Button asChild>
          <Link to="/imports/new">New import</Link>
        </Button>
      </div>
      <p className="text-fg-muted">
        Phase F data onboarding pipeline. Supports CSV, JSON, and Frappe
        REST sources (ERPNext, HRMS, CRM, LMS).
      </p>
      {jobs.isLoading && <p>Loading…</p>}
      {jobs.isError && (
        <p className="text-danger">
          Failed to load jobs: {(jobs.error as Error).message}
        </p>
      )}
      {jobs.data && jobs.data.length === 0 && (
        <p className="text-fg-muted">No imports yet.</p>
      )}
      {jobs.data && jobs.data.length > 0 && (
        <Table className="text-[13px]">
          <TableHeader>
            <TableRow>
              <TableHead>Job</TableHead>
              <TableHead>Source</TableHead>
              <TableHead>Status</TableHead>
              <TableHead>Updated</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {jobs.data.map((j) => (
              <TableRow key={j.id}>
                <TableCell>
                  <Link to={`/imports/${j.id}`}>{j.id.slice(0, 8)}…</Link>
                </TableCell>
                <TableCell>{j.source_type}</TableCell>
                <TableCell>{j.status}</TableCell>
                <TableCell>{j.updated_at}</TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      )}
    </section>
  );
}

function ImportWizard({ jobId }: { jobId: string | undefined }) {
  const navigate = useNavigate();
  const qc = useQueryClient();
  const jobQ = useQuery({
    queryKey: ["imports", jobId],
    queryFn: () => apiFetch<ImportJob>(`/imports/${jobId}`),
    enabled: !!jobId,
  });
  const errorsQ = useQuery({
    queryKey: ["imports", jobId, "errors"],
    queryFn: () => apiFetch<StagingRow[]>(`/imports/${jobId}/errors`),
    enabled: !!jobId,
  });

  const currentStep = stepForStatus(jobQ.data?.status);

  const createJob = useMutation({
    mutationFn: (body: { source_type: string; config: unknown }) =>
      apiFetch<ImportJob>("/imports", {
        method: "POST",
        body: JSON.stringify(body),
      }),
    onSuccess: (job) => {
      qc.invalidateQueries({ queryKey: ["imports"] });
      navigate(`/imports/${job.id}`);
    },
  });

  const submitMapping = useMutation({
    mutationFn: (mapping: unknown) =>
      apiFetch<ImportJob>(`/imports/${jobId}/map`, {
        method: "POST",
        body: JSON.stringify({ mapping }),
      }),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["imports", jobId] }),
  });

  const validate = useMutation({
    mutationFn: () =>
      apiFetch<{ job: ImportJob }>(`/imports/${jobId}/validate`, {
        method: "POST",
        body: "{}",
      }),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["imports", jobId] });
      qc.invalidateQueries({ queryKey: ["imports", jobId, "errors"] });
    },
  });

  const accept = useMutation({
    mutationFn: () =>
      apiFetch<{ job: ImportJob; imported: number }>(`/imports/${jobId}/accept`, {
        method: "POST",
        body: "{}",
      }),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["imports", jobId] }),
  });

  return (
    <section>
      <div className="mb-2">
        <Link to="/imports">← All imports</Link>
      </div>
      <h1>Import {jobId ? jobId.slice(0, 8) + "…" : ""}</h1>
      <Stepper
        className="my-3"
        current={currentStep - 1}
        steps={[
          { label: "Source" },
          { label: "Mapping" },
          { label: "Validate" },
          { label: "Review" },
          { label: "Complete" },
        ]}
      />
      {!jobId && <StepSource onCreate={(body) => createJob.mutate(body)} />}
      {jobId && jobQ.isLoading && <p>Loading…</p>}
      {jobId && jobQ.data && currentStep === 2 && (
        <StepMapping job={jobQ.data} onSubmit={(m) => submitMapping.mutate(m)} />
      )}
      {jobId && jobQ.data && currentStep === 3 && (
        <StepValidate
          job={jobQ.data}
          errors={errorsQ.data ?? []}
          onRun={() => validate.mutate()}
          pending={validate.isPending}
        />
      )}
      {jobId && jobQ.data && currentStep === 4 && (
        <StepReview
          job={jobQ.data}
          errors={errorsQ.data ?? []}
          onAccept={() => accept.mutate()}
          pending={accept.isPending}
        />
      )}
      {jobId && jobQ.data && currentStep === 5 && <StepComplete job={jobQ.data} />}
    </section>
  );
}

function stepForStatus(status: string | undefined): 1 | 2 | 3 | 4 | 5 {
  switch (status) {
    case undefined:
    case "pending":
      return 1;
    case "discovering":
    case "exporting":
    case "normalizing":
    case "mapping":
    case "staging":
      return 2;
    case "validating":
      return 3;
    case "reconciling":
      return 4;
    case "accepting":
    case "cutting_over":
      return 4;
    case "completed":
    case "failed":
      return 5;
    default:
      return 1;
  }
}

type SourceType =
  | "csv"
  | "json"
  | "frappe"
  | "quickbooks"
  | "xero"
  | "tally"
  | "sage";

// OAuth2-backed cloud-accounting sources share a common credential
// block (access token, or refresh token + client credentials).
const OAUTH_SOURCES: SourceType[] = ["quickbooks", "xero", "sage"];

// pruneEmpty drops blank string fields so optional credentials are
// omitted from the submitted config rather than sent as "".
function pruneEmpty(obj: Record<string, string>): Record<string, string> {
  return Object.fromEntries(
    Object.entries(obj).filter(([, v]) => v.trim() !== ""),
  );
}

function StepSource({
  onCreate,
}: {
  onCreate: (body: { source_type: string; config: unknown }) => void;
}) {
  const [sourceType, setSourceType] = useState<SourceType>("csv");
  const [csvPayload, setCsvPayload] = useState("");
  const [csvEntity, setCsvEntity] = useState("");
  const [csvKType, setCsvKType] = useState("");
  const [frappeURL, setFrappeURL] = useState("");
  const [frappeKey, setFrappeKey] = useState("");
  const [frappeSecret, setFrappeSecret] = useState("");
  const [frappeDocTypes, setFrappeDocTypes] = useState("Customer,Sales Invoice");
  // Shared OAuth2 credentials for QuickBooks / Xero / Sage.
  const [accessToken, setAccessToken] = useState("");
  const [refreshToken, setRefreshToken] = useState("");
  const [clientId, setClientId] = useState("");
  const [clientSecret, setClientSecret] = useState("");
  // Provider-specific identifiers.
  const [qbRealmId, setQbRealmId] = useState("");
  const [xeroTenantId, setXeroTenantId] = useState("");
  // Tally file import.
  const [tallyFormat, setTallyFormat] = useState<"xml" | "json">("xml");
  const [tallyPayload, setTallyPayload] = useState("");

  const oauthConfig = () =>
    pruneEmpty({
      access_token: accessToken,
      refresh_token: refreshToken,
      client_id: clientId,
      client_secret: clientSecret,
    });

  const submit = () => {
    switch (sourceType) {
      case "frappe":
        onCreate({
          source_type: "frappe",
          config: {
            base_url: frappeURL,
            api_key: frappeKey,
            api_secret: frappeSecret,
            doctypes: frappeDocTypes
              .split(",")
              .map((s) => s.trim())
              .filter(Boolean)
              .map((name) => ({ name })),
          },
        });
        return;
      case "quickbooks":
        onCreate({
          source_type: "quickbooks",
          config: { realm_id: qbRealmId, ...oauthConfig() },
        });
        return;
      case "xero":
        onCreate({
          source_type: "xero",
          config: { xero_tenant_id: xeroTenantId, ...oauthConfig() },
        });
        return;
      case "sage":
        onCreate({ source_type: "sage", config: oauthConfig() });
        return;
      case "tally":
        onCreate({
          source_type: "tally",
          config: { format: tallyFormat, payload: tallyPayload },
        });
        return;
      default:
        onCreate({
          source_type: sourceType,
          config: {
            format: sourceType,
            entity: csvEntity,
            target_ktype: csvKType,
            payload: csvPayload,
          },
        });
    }
  };

  const isFileSource = sourceType === "csv" || sourceType === "json";
  const isOAuthSource = OAUTH_SOURCES.includes(sourceType);

  return (
    <div className="max-w-2xl">
      <h2>Step 1. Source</h2>
      <label className="mb-3 block text-sm">
        Source type
        <Select
          className="mt-1"
          value={sourceType}
          onChange={(e) => setSourceType(e.target.value as SourceType)}
        >
          <option value="csv">CSV</option>
          <option value="json">JSON</option>
          <option value="frappe">Frappe REST</option>
          <option value="quickbooks">QuickBooks Online</option>
          <option value="xero">Xero</option>
          <option value="tally">Tally Prime (file)</option>
          <option value="sage">Sage Business Cloud</option>
        </Select>
      </label>
      {isFileSource && (
        <>
          <label className="mb-2 block text-sm">
            Entity (source table/sheet)
            <Input
              className="mt-1"
              value={csvEntity}
              onChange={(e) => setCsvEntity(e.target.value)}
            />
          </label>
          <label className="mb-2 block text-sm">
            Target KType (default for this entity)
            <Input
              className="mt-1"
              value={csvKType}
              onChange={(e) => setCsvKType(e.target.value)}
              placeholder="crm.lead"
            />
          </label>
          <label className="block text-sm">
            Payload ({sourceType})
            <textarea
              value={csvPayload}
              onChange={(e) => setCsvPayload(e.target.value)}
              rows={12}
              className="mt-1 block w-full rounded-md border border-border bg-bg-elevated p-2 font-mono text-sm focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-(--focus-ring)"
            />
          </label>
        </>
      )}
      {sourceType === "frappe" && (
        <>
          <label className="mb-2 block text-sm">
            Frappe base URL
            <Input
              className="mt-1"
              value={frappeURL}
              onChange={(e) => setFrappeURL(e.target.value)}
              placeholder="https://erp.example.com"
            />
          </label>
          <label className="mb-2 block text-sm">
            API key
            <Input
              className="mt-1"
              value={frappeKey}
              onChange={(e) => setFrappeKey(e.target.value)}
            />
          </label>
          <label className="mb-2 block text-sm">
            API secret
            <Input
              type="password"
              className="mt-1"
              value={frappeSecret}
              onChange={(e) => setFrappeSecret(e.target.value)}
            />
          </label>
          <label className="block text-sm">
            DocTypes (comma-separated)
            <Input
              className="mt-1"
              value={frappeDocTypes}
              onChange={(e) => setFrappeDocTypes(e.target.value)}
            />
          </label>
        </>
      )}
      {sourceType === "quickbooks" && (
        <label className="mb-2 block text-sm">
          Realm ID (company)
          <Input
            className="mt-1"
            value={qbRealmId}
            onChange={(e) => setQbRealmId(e.target.value)}
            placeholder="1234567890"
          />
        </label>
      )}
      {sourceType === "xero" && (
        <label className="mb-2 block text-sm">
          Xero tenant ID
          <Input
            className="mt-1"
            value={xeroTenantId}
            onChange={(e) => setXeroTenantId(e.target.value)}
            placeholder="organisation uuid"
          />
        </label>
      )}
      {isOAuthSource && (
        <>
          <p className="mb-2 text-fg-muted">
            Provide an access token, or a refresh token plus client
            credentials to mint one.
          </p>
          <label className="mb-2 block text-sm">
            Access token
            <Input
              type="password"
              className="mt-1"
              value={accessToken}
              onChange={(e) => setAccessToken(e.target.value)}
            />
          </label>
          <label className="mb-2 block text-sm">
            Refresh token
            <Input
              type="password"
              className="mt-1"
              value={refreshToken}
              onChange={(e) => setRefreshToken(e.target.value)}
            />
          </label>
          <label className="mb-2 block text-sm">
            Client ID
            <Input
              className="mt-1"
              value={clientId}
              onChange={(e) => setClientId(e.target.value)}
            />
          </label>
          <label className="mb-2 block text-sm">
            Client secret
            <Input
              type="password"
              className="mt-1"
              value={clientSecret}
              onChange={(e) => setClientSecret(e.target.value)}
            />
          </label>
        </>
      )}
      {sourceType === "tally" && (
        <>
          <label className="mb-2 block text-sm">
            Export format
            <Select
              className="mt-1"
              value={tallyFormat}
              onChange={(e) => setTallyFormat(e.target.value as "xml" | "json")}
            >
              <option value="xml">XML</option>
              <option value="json">JSON</option>
            </Select>
          </label>
          <label className="block text-sm">
            Tally export payload (masters + vouchers)
            <textarea
              value={tallyPayload}
              onChange={(e) => setTallyPayload(e.target.value)}
              rows={12}
              className="mt-1 block w-full rounded-md border border-border bg-bg-elevated p-2 font-mono text-sm focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-(--focus-ring)"
            />
          </label>
        </>
      )}
      <div className="mt-4">
        <Button onClick={submit}>Create job + stage rows</Button>
      </div>
    </div>
  );
}

function StepMapping({
  job,
  onSubmit,
}: {
  job: ImportJob;
  onSubmit: (mapping: unknown) => void;
}) {
  const entities = useMemo(() => {
    const p = job.progress as { source?: { entities?: Array<{ name: string; target_ktype?: string }> } };
    return p?.source?.entities ?? [];
  }, [job.progress]);
  const initial = useMemo(() => {
    const out: Record<string, { target_ktype: string }> = {};
    for (const e of entities) {
      out[e.name] = { target_ktype: e.target_ktype ?? "" };
    }
    return out;
  }, [entities]);
  const [mapping, setMapping] = useState(initial);

  return (
    <div>
      <h2>Step 2. Mapping</h2>
      <p className="text-fg-muted">
        Set the target KType for each discovered source entity. Per-field
        renames happen automatically via the built-in concept map
        (PROPOSAL §5.3).
      </p>
      <Table className="text-[13px]">
        <TableHeader>
          <TableRow>
            <TableHead>Source entity</TableHead>
            <TableHead>Source rows</TableHead>
            <TableHead>Target KType</TableHead>
          </TableRow>
        </TableHeader>
        <TableBody>
          {entities.map((e) => (
            <TableRow key={e.name}>
              <TableCell>{e.name}</TableCell>
              <TableCell>
                {((e as unknown as { row_count?: number }).row_count) ?? "—"}
              </TableCell>
              <TableCell>
                <Input
                  value={mapping[e.name]?.target_ktype ?? ""}
                  onChange={(ev) =>
                    setMapping((m) => ({
                      ...m,
                      [e.name]: { target_ktype: ev.target.value },
                    }))
                  }
                  placeholder="crm.lead"
                />
              </TableCell>
            </TableRow>
          ))}
        </TableBody>
      </Table>
      <div className="mt-4 flex items-center">
        <Button onClick={() => onSubmit({ entities: mapping })}>
          Save mapping
        </Button>
        <Link to={`/imports/${job.id}/mapping`} className="ml-3">
          Advanced field mapping →
        </Link>
      </div>
    </div>
  );
}

function StepValidate({
  job,
  errors,
  onRun,
  pending,
}: {
  job: ImportJob;
  errors: StagingRow[];
  onRun: () => void;
  pending: boolean;
}) {
  return (
    <div>
      <h2>Step 3. Validate</h2>
      <p className="text-fg-muted">
        Runs schema + referential integrity checks over every staged row
        and returns the per-row error report.
      </p>
      <Button onClick={onRun} disabled={pending}>
        {pending ? "Validating…" : "Run validation"}
      </Button>
      {errors.length > 0 && (
        <>
          <h3 className="mt-4">{errors.length} invalid rows</h3>
          <Table className="text-[13px]">
            <TableHeader>
              <TableRow>
                <TableHead>Source ID</TableHead>
                <TableHead>Target KType</TableHead>
                <TableHead>Errors</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {errors.map((row) => (
                <TableRow key={row.id}>
                  <TableCell>{row.source_id ?? ""}</TableCell>
                  <TableCell>{row.target_ktype}</TableCell>
                  <TableCell>
                    <ul className="m-0 pl-4">
                      {(row.validation_errors ?? []).map((e, i) => (
                        <li key={i}>
                          <code>{e.code}</code>
                          {e.field ? ` @ ${e.field}` : ""}: {e.message}
                        </li>
                      ))}
                    </ul>
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        </>
      )}
      {job.status === "reconciling" && errors.length === 0 && (
        <p className="mt-3 text-success">
          All rows valid — proceed to review.
        </p>
      )}
    </div>
  );
}

function StepReview({
  job,
  errors,
  onAccept,
  pending,
}: {
  job: ImportJob;
  errors: StagingRow[];
  onAccept: () => void;
  pending: boolean;
}) {
  const rec = job.reconciliation as {
    source_count?: number;
    staged_count?: number;
    valid_count?: number;
    invalid_count?: number;
    discrepancies?: string[];
  };
  return (
    <div>
      <h2>Step 4. Review &amp; Accept</h2>
      <dl className="grid grid-cols-[max-content_1fr] gap-x-4 gap-y-1">
        <dt>Source count</dt>
        <dd>{rec.source_count ?? "—"}</dd>
        <dt>Staged count</dt>
        <dd>{rec.staged_count ?? "—"}</dd>
        <dt>Valid</dt>
        <dd>{rec.valid_count ?? "—"}</dd>
        <dt>Invalid</dt>
        <dd>{rec.invalid_count ?? "—"}</dd>
      </dl>
      {rec.discrepancies && rec.discrepancies.length > 0 && (
        <div className="mt-3 text-danger">
          <strong>Discrepancies:</strong>
          <ul>
            {rec.discrepancies.map((d) => (
              <li key={d}>{d}</li>
            ))}
          </ul>
        </div>
      )}
      <div className="mt-4">
        <Button onClick={onAccept} disabled={pending}>
          {pending
            ? "Importing…"
            : `Accept & cutover (${rec.valid_count ?? 0} rows)`}
        </Button>
      </div>
      {errors.length > 0 && (
        <p className="mt-2 text-xs text-fg-muted">
          {errors.length} invalid rows will be skipped. Fix the source and
          re-import them later.
        </p>
      )}
    </div>
  );
}

function StepComplete({ job }: { job: ImportJob }) {
  const imported =
    (job.progress as { imported?: number }).imported ?? 0;
  return (
    <div>
      <h2>Step 5. {job.status === "completed" ? "Complete" : "Failed"}</h2>
      <p>
        Import {job.status === "completed" ? "completed" : "failed"} at{" "}
        {job.completed_at ?? job.updated_at}.
      </p>
      <p>
        Imported <strong>{imported}</strong> records.
      </p>
      <Link to="/imports">← Back to imports</Link>
    </div>
  );
}
