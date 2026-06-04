import { useState, useMemo } from "react";
import { Link, useParams, useNavigate } from "react-router-dom";
import { useQuery, useMutation, useQueryClient } from "@tanstack/react-query";

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
      <div style={{ display: "flex", justifyContent: "space-between" }}>
        <h1>Imports</h1>
        <Link to="/imports/new">
          <button>New import</button>
        </Link>
      </div>
      <p style={{ color: "#6b7280" }}>
        Phase F data onboarding pipeline. Supports CSV, JSON, and Frappe
        REST sources (ERPNext, HRMS, CRM, LMS).
      </p>
      {jobs.isLoading && <p>Loading…</p>}
      {jobs.isError && (
        <p style={{ color: "#b91c1c" }}>
          Failed to load jobs: {(jobs.error as Error).message}
        </p>
      )}
      {jobs.data && jobs.data.length === 0 && (
        <p style={{ color: "#6b7280" }}>No imports yet.</p>
      )}
      {jobs.data && jobs.data.length > 0 && (
        <table style={{ width: "100%", borderCollapse: "collapse", fontSize: 13 }}>
          <thead>
            <tr style={{ textAlign: "left", borderBottom: "1px solid #e5e7eb" }}>
              <th style={{ padding: "6px 8px" }}>Job</th>
              <th style={{ padding: "6px 8px" }}>Source</th>
              <th style={{ padding: "6px 8px" }}>Status</th>
              <th style={{ padding: "6px 8px" }}>Updated</th>
            </tr>
          </thead>
          <tbody>
            {jobs.data.map((j) => (
              <tr key={j.id} style={{ borderBottom: "1px solid #f3f4f6" }}>
                <td style={{ padding: "6px 8px" }}>
                  <Link to={`/imports/${j.id}`}>{j.id.slice(0, 8)}…</Link>
                </td>
                <td style={{ padding: "6px 8px" }}>{j.source_type}</td>
                <td style={{ padding: "6px 8px" }}>{j.status}</td>
                <td style={{ padding: "6px 8px" }}>{j.updated_at}</td>
              </tr>
            ))}
          </tbody>
        </table>
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
      <div style={{ marginBottom: 8 }}>
        <Link to="/imports">← All imports</Link>
      </div>
      <h1>Import {jobId ? jobId.slice(0, 8) + "…" : ""}</h1>
      <Stepper current={currentStep} />
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

function Stepper({ current }: { current: 1 | 2 | 3 | 4 | 5 }) {
  const labels = ["Source", "Mapping", "Validate", "Review", "Complete"];
  return (
    <ol
      style={{
        display: "flex",
        listStyle: "none",
        padding: 0,
        margin: "8px 0 16px",
        gap: 12,
      }}
    >
      {labels.map((label, i) => {
        const n = (i + 1) as 1 | 2 | 3 | 4 | 5;
        const active = n === current;
        const done = n < current;
        return (
          <li
            key={label}
            style={{
              padding: "4px 10px",
              borderRadius: 12,
              background: active ? "#2563eb" : done ? "#bfdbfe" : "#e5e7eb",
              color: active ? "white" : "#111827",
              fontSize: 12,
            }}
          >
            {n}. {label}
          </li>
        );
      })}
    </ol>
  );
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
    <div>
      <h2>Step 1. Source</h2>
      <label style={{ display: "block", marginBottom: 12 }}>
        Source type
        <select
          value={sourceType}
          onChange={(e) => setSourceType(e.target.value as SourceType)}
          style={{ marginLeft: 8 }}
        >
          <option value="csv">CSV</option>
          <option value="json">JSON</option>
          <option value="frappe">Frappe REST</option>
          <option value="quickbooks">QuickBooks Online</option>
          <option value="xero">Xero</option>
          <option value="tally">Tally Prime (file)</option>
          <option value="sage">Sage Business Cloud</option>
        </select>
      </label>
      {isFileSource && (
        <>
          <label style={{ display: "block", marginBottom: 8 }}>
            Entity (source table/sheet)
            <input
              value={csvEntity}
              onChange={(e) => setCsvEntity(e.target.value)}
              style={{ marginLeft: 8, width: 240 }}
            />
          </label>
          <label style={{ display: "block", marginBottom: 8 }}>
            Target KType (default for this entity)
            <input
              value={csvKType}
              onChange={(e) => setCsvKType(e.target.value)}
              placeholder="crm.lead"
              style={{ marginLeft: 8, width: 240 }}
            />
          </label>
          <label style={{ display: "block" }}>
            Payload ({sourceType})
            <textarea
              value={csvPayload}
              onChange={(e) => setCsvPayload(e.target.value)}
              rows={12}
              style={{ display: "block", width: "100%", fontFamily: "monospace" }}
            />
          </label>
        </>
      )}
      {sourceType === "frappe" && (
        <>
          <label style={{ display: "block", marginBottom: 8 }}>
            Frappe base URL
            <input
              value={frappeURL}
              onChange={(e) => setFrappeURL(e.target.value)}
              placeholder="https://erp.example.com"
              style={{ marginLeft: 8, width: 320 }}
            />
          </label>
          <label style={{ display: "block", marginBottom: 8 }}>
            API key
            <input
              value={frappeKey}
              onChange={(e) => setFrappeKey(e.target.value)}
              style={{ marginLeft: 8, width: 240 }}
            />
          </label>
          <label style={{ display: "block", marginBottom: 8 }}>
            API secret
            <input
              type="password"
              value={frappeSecret}
              onChange={(e) => setFrappeSecret(e.target.value)}
              style={{ marginLeft: 8, width: 240 }}
            />
          </label>
          <label style={{ display: "block" }}>
            DocTypes (comma-separated)
            <input
              value={frappeDocTypes}
              onChange={(e) => setFrappeDocTypes(e.target.value)}
              style={{ marginLeft: 8, width: 400 }}
            />
          </label>
        </>
      )}
      {sourceType === "quickbooks" && (
        <label style={{ display: "block", marginBottom: 8 }}>
          Realm ID (company)
          <input
            value={qbRealmId}
            onChange={(e) => setQbRealmId(e.target.value)}
            placeholder="1234567890"
            style={{ marginLeft: 8, width: 240 }}
          />
        </label>
      )}
      {sourceType === "xero" && (
        <label style={{ display: "block", marginBottom: 8 }}>
          Xero tenant ID
          <input
            value={xeroTenantId}
            onChange={(e) => setXeroTenantId(e.target.value)}
            placeholder="organisation uuid"
            style={{ marginLeft: 8, width: 320 }}
          />
        </label>
      )}
      {isOAuthSource && (
        <>
          <p style={{ color: "#6b7280", marginBottom: 8 }}>
            Provide an access token, or a refresh token plus client
            credentials to mint one.
          </p>
          <label style={{ display: "block", marginBottom: 8 }}>
            Access token
            <input
              type="password"
              value={accessToken}
              onChange={(e) => setAccessToken(e.target.value)}
              style={{ marginLeft: 8, width: 320 }}
            />
          </label>
          <label style={{ display: "block", marginBottom: 8 }}>
            Refresh token
            <input
              type="password"
              value={refreshToken}
              onChange={(e) => setRefreshToken(e.target.value)}
              style={{ marginLeft: 8, width: 320 }}
            />
          </label>
          <label style={{ display: "block", marginBottom: 8 }}>
            Client ID
            <input
              value={clientId}
              onChange={(e) => setClientId(e.target.value)}
              style={{ marginLeft: 8, width: 240 }}
            />
          </label>
          <label style={{ display: "block", marginBottom: 8 }}>
            Client secret
            <input
              type="password"
              value={clientSecret}
              onChange={(e) => setClientSecret(e.target.value)}
              style={{ marginLeft: 8, width: 240 }}
            />
          </label>
        </>
      )}
      {sourceType === "tally" && (
        <>
          <label style={{ display: "block", marginBottom: 8 }}>
            Export format
            <select
              value={tallyFormat}
              onChange={(e) => setTallyFormat(e.target.value as "xml" | "json")}
              style={{ marginLeft: 8 }}
            >
              <option value="xml">XML</option>
              <option value="json">JSON</option>
            </select>
          </label>
          <label style={{ display: "block" }}>
            Tally export payload (masters + vouchers)
            <textarea
              value={tallyPayload}
              onChange={(e) => setTallyPayload(e.target.value)}
              rows={12}
              style={{ display: "block", width: "100%", fontFamily: "monospace" }}
            />
          </label>
        </>
      )}
      <div style={{ marginTop: 16 }}>
        <button onClick={submit}>Create job + stage rows</button>
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
      <p style={{ color: "#6b7280" }}>
        Set the target KType for each discovered source entity. Per-field
        renames happen automatically via the built-in concept map
        (PROPOSAL §5.3).
      </p>
      <table style={{ width: "100%", borderCollapse: "collapse", fontSize: 13 }}>
        <thead>
          <tr style={{ textAlign: "left", borderBottom: "1px solid #e5e7eb" }}>
            <th style={{ padding: "6px 8px" }}>Source entity</th>
            <th style={{ padding: "6px 8px" }}>Source rows</th>
            <th style={{ padding: "6px 8px" }}>Target KType</th>
          </tr>
        </thead>
        <tbody>
          {entities.map((e) => (
            <tr key={e.name} style={{ borderBottom: "1px solid #f3f4f6" }}>
              <td style={{ padding: "6px 8px" }}>{e.name}</td>
              <td style={{ padding: "6px 8px" }}>
                {((e as unknown as { row_count?: number }).row_count) ?? "—"}
              </td>
              <td style={{ padding: "6px 8px" }}>
                <input
                  value={mapping[e.name]?.target_ktype ?? ""}
                  onChange={(ev) =>
                    setMapping((m) => ({
                      ...m,
                      [e.name]: { target_ktype: ev.target.value },
                    }))
                  }
                  placeholder="crm.lead"
                />
              </td>
            </tr>
          ))}
        </tbody>
      </table>
      <div style={{ marginTop: 16 }}>
        <button onClick={() => onSubmit({ entities: mapping })}>Save mapping</button>
        <Link to={`/imports/${job.id}/mapping`} style={{ marginLeft: 12 }}>
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
      <p style={{ color: "#6b7280" }}>
        Runs schema + referential integrity checks over every staged row
        and returns the per-row error report.
      </p>
      <button onClick={onRun} disabled={pending}>
        {pending ? "Validating…" : "Run validation"}
      </button>
      {errors.length > 0 && (
        <>
          <h3 style={{ marginTop: 16 }}>{errors.length} invalid rows</h3>
          <table style={{ width: "100%", borderCollapse: "collapse", fontSize: 13 }}>
            <thead>
              <tr style={{ textAlign: "left", borderBottom: "1px solid #e5e7eb" }}>
                <th style={{ padding: "6px 8px" }}>Source ID</th>
                <th style={{ padding: "6px 8px" }}>Target KType</th>
                <th style={{ padding: "6px 8px" }}>Errors</th>
              </tr>
            </thead>
            <tbody>
              {errors.map((row) => (
                <tr key={row.id} style={{ borderBottom: "1px solid #f3f4f6" }}>
                  <td style={{ padding: "6px 8px" }}>{row.source_id ?? ""}</td>
                  <td style={{ padding: "6px 8px" }}>{row.target_ktype}</td>
                  <td style={{ padding: "6px 8px" }}>
                    <ul style={{ margin: 0, paddingLeft: 16 }}>
                      {(row.validation_errors ?? []).map((e, i) => (
                        <li key={i}>
                          <code>{e.code}</code>
                          {e.field ? ` @ ${e.field}` : ""}: {e.message}
                        </li>
                      ))}
                    </ul>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </>
      )}
      {job.status === "reconciling" && errors.length === 0 && (
        <p style={{ marginTop: 12, color: "#065f46" }}>
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
      <dl style={{ display: "grid", gridTemplateColumns: "max-content 1fr", gap: "4px 16px" }}>
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
        <div style={{ marginTop: 12, color: "#b91c1c" }}>
          <strong>Discrepancies:</strong>
          <ul>
            {rec.discrepancies.map((d) => (
              <li key={d}>{d}</li>
            ))}
          </ul>
        </div>
      )}
      <button onClick={onAccept} disabled={pending} style={{ marginTop: 16 }}>
        {pending ? "Importing…" : `Accept & cutover (${rec.valid_count ?? 0} rows)`}
      </button>
      {errors.length > 0 && (
        <p style={{ marginTop: 8, color: "#6b7280", fontSize: 12 }}>
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
