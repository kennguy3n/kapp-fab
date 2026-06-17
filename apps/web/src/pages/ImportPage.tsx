import { useState, useMemo } from "react";
import { Link, useParams, useNavigate } from "react-router-dom";
import { useQuery, useMutation, useQueryClient } from "@tanstack/react-query";
import {
  ArrowLeft,
  CheckCircle2,
  Database,
  Plus,
  XCircle,
} from "lucide-react";
import {
  Badge,
  Button,
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
  EmptyState,
  Field,
  Input,
  Select,
  StatCard,
  Stepper,
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
  Textarea,
  type BadgeProps,
} from "@kapp/ui";
import { useFormatter } from "../lib/i18n";
import { humanizeLabel, humanizeToken } from "../lib/ktypeView";
import {
  AdminErrorState,
  AdminPageHeader,
  AdminTableSkeleton,
  CopyableId,
} from "./adminKit";

/**
 * ImportPage drives the Phase F import wizard. Five steps:
 *
 *   1. Source selection   — CSV / JSON / Frappe REST / cloud accounting
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

type BadgeVariant = NonNullable<BadgeProps["variant"]>;

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

const SOURCE_LABEL: Record<string, string> = {
  csv: "CSV file",
  json: "JSON file",
  frappe: "Frappe / ERPNext",
  quickbooks: "QuickBooks Online",
  xero: "Xero",
  tally: "Tally Prime",
  sage: "Sage Business Cloud",
};

function sourceLabel(raw: string): string {
  return SOURCE_LABEL[raw] ?? humanizeToken(raw);
}

// Import job lifecycle → badge styling + plain-language label.
const STATUS_META: Record<string, { label: string; variant: BadgeVariant }> = {
  pending: { label: "Pending", variant: "neutral" },
  discovering: { label: "Discovering", variant: "info" },
  exporting: { label: "Exporting", variant: "info" },
  normalizing: { label: "Normalizing", variant: "info" },
  mapping: { label: "Mapping", variant: "warning" },
  staging: { label: "Staging", variant: "info" },
  validating: { label: "Validating", variant: "info" },
  reconciling: { label: "Ready to review", variant: "warning" },
  accepting: { label: "Importing", variant: "info" },
  cutting_over: { label: "Importing", variant: "info" },
  completed: { label: "Completed", variant: "success" },
  failed: { label: "Failed", variant: "danger" },
};

function statusMeta(raw: string): { label: string; variant: BadgeVariant } {
  return STATUS_META[raw] ?? { label: humanizeToken(raw), variant: "neutral" };
}

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
  const fmt = useFormatter();
  const jobs = useQuery({
    queryKey: ["imports"],
    queryFn: () => apiFetch<ImportJob[]>("/imports"),
  });

  const data = jobs.data ?? [];

  return (
    <section className="flex flex-col gap-6">
      <AdminPageHeader
        area="Onboarding"
        title="Data import"
        description="Bring your existing data into Kapp from a file or another system. Each import is checked row by row before anything goes live."
        actions={
          <Button asChild>
            <Link to="/imports/new">
              <Plus className="h-4 w-4" />
              New import
            </Link>
          </Button>
        }
      />

      {jobs.isLoading ? (
        <Card>
          <CardContent className="pt-4">
            <AdminTableSkeleton columns={["Reference", "Source", "Status", "Updated"]} />
          </CardContent>
        </Card>
      ) : jobs.isError ? (
        <AdminErrorState
          title="Couldn't load imports"
          error={jobs.error}
          onRetry={() => jobs.refetch()}
        />
      ) : data.length === 0 ? (
        <Card>
          <CardContent className="py-10">
            <EmptyState
              icon={<Database />}
              title="No imports yet"
              description="Start your first import to bring customers, invoices, or any other records into your workspace."
              action={
                <Button asChild>
                  <Link to="/imports/new">
                    <Plus className="h-4 w-4" />
                    New import
                  </Link>
                </Button>
              }
            />
          </CardContent>
        </Card>
      ) : (
        <Card>
          <CardHeader className="flex flex-row flex-wrap items-center justify-between gap-2">
            <CardTitle>Recent imports</CardTitle>
            <Badge variant="neutral">
              {fmt.number(data.length)} {data.length === 1 ? "import" : "imports"}
            </Badge>
          </CardHeader>
          <CardContent>
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>Reference</TableHead>
                  <TableHead>Source</TableHead>
                  <TableHead>Status</TableHead>
                  <TableHead>Updated</TableHead>
                  <TableHead className="text-end">Actions</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {data.map((j) => {
                  const meta = statusMeta(j.status);
                  return (
                    <TableRow key={j.id}>
                      <TableCell>
                        <CopyableId value={j.id} label="import reference" />
                      </TableCell>
                      <TableCell>{sourceLabel(j.source_type)}</TableCell>
                      <TableCell>
                        <Badge variant={meta.variant}>{meta.label}</Badge>
                      </TableCell>
                      <TableCell className="text-fg-muted">
                        {fmt.dateTime(new Date(j.updated_at))}
                      </TableCell>
                      <TableCell className="text-end">
                        <Button asChild variant="outline" size="sm">
                          <Link to={`/imports/${j.id}`}>Open</Link>
                        </Button>
                      </TableCell>
                    </TableRow>
                  );
                })}
              </TableBody>
            </Table>
          </CardContent>
        </Card>
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
    <section className="flex flex-col gap-6">
      <AdminPageHeader
        area="Onboarding"
        title="Data import"
        description={
          jobId ? (
            <span className="inline-flex items-center gap-2">
              Reference <CopyableId value={jobId} label="import reference" />
            </span>
          ) : (
            "Follow the steps to bring your data in safely. You can review and fix problems before anything is saved."
          )
        }
        actions={
          <Button asChild variant="ghost">
            <Link to="/imports">
              <ArrowLeft className="h-4 w-4" />
              All imports
            </Link>
          </Button>
        }
      />

      <Stepper
        current={currentStep - 1}
        steps={[
          { label: "Source", description: "Where data comes from" },
          { label: "Mapping", description: "Match to records" },
          { label: "Validate", description: "Check every row" },
          { label: "Review", description: "Confirm the totals" },
          { label: "Complete", description: "Bring it live" },
        ]}
      />

      {!jobId && (
        <StepSource
          onCreate={(body) => createJob.mutate(body)}
          pending={createJob.isPending}
          error={createJob.error}
        />
      )}
      {jobId && jobQ.isLoading && (
        <Card>
          <CardContent className="pt-4">
            <AdminTableSkeleton columns={["Loading"]} rows={3} />
          </CardContent>
        </Card>
      )}
      {jobId && jobQ.isError && (
        <AdminErrorState
          title="Couldn't load this import"
          error={jobQ.error}
          onRetry={() => jobQ.refetch()}
        />
      )}
      {jobId && jobQ.data && currentStep === 2 && (
        <StepMapping
          job={jobQ.data}
          onSubmit={(m) => submitMapping.mutate(m)}
          pending={submitMapping.isPending}
          error={submitMapping.error}
        />
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
  pending,
  error,
}: {
  onCreate: (body: { source_type: string; config: unknown }) => void;
  pending: boolean;
  error: unknown;
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

  const submit = (e: React.FormEvent) => {
    e.preventDefault();
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
  const fileLabel = sourceType === "csv" ? "CSV" : "JSON";

  return (
    <Card className="max-w-2xl">
      <CardHeader>
        <CardTitle>Step 1. Source</CardTitle>
        <CardDescription>
          Choose where your data comes from and provide it below.
        </CardDescription>
      </CardHeader>
      <CardContent>
        <form onSubmit={submit} className="flex flex-col gap-4">
          <Field label="Source type" help="Pick the system or file you're importing from.">
            <Select
              value={sourceType}
              onChange={(e) => setSourceType(e.target.value as SourceType)}
            >
              <option value="csv">CSV file</option>
              <option value="json">JSON file</option>
              <option value="frappe">Frappe / ERPNext</option>
              <option value="quickbooks">QuickBooks Online</option>
              <option value="xero">Xero</option>
              <option value="tally">Tally Prime (file)</option>
              <option value="sage">Sage Business Cloud</option>
            </Select>
          </Field>

          {isFileSource && (
            <>
              <Field
                label="Source name"
                help="The table or sheet these rows come from, e.g. Customers."
              >
                <Input
                  value={csvEntity}
                  onChange={(e) => setCsvEntity(e.target.value)}
                  placeholder="Customers"
                />
              </Field>
              <Field
                label="Default record type"
                help="The record type rows import into by default. You can fine-tune this in the next step."
              >
                <Input
                  value={csvKType}
                  onChange={(e) => setCsvKType(e.target.value)}
                  placeholder="crm.account"
                  className="font-mono"
                />
              </Field>
              <Field label={`Paste your ${fileLabel} data`}>
                <Textarea
                  value={csvPayload}
                  onChange={(e) => setCsvPayload(e.target.value)}
                  rows={10}
                  className="font-mono"
                />
              </Field>
            </>
          )}

          {sourceType === "frappe" && (
            <>
              <Field label="Frappe base URL">
                <Input
                  value={frappeURL}
                  onChange={(e) => setFrappeURL(e.target.value)}
                  placeholder="https://erp.example.com"
                  type="url"
                />
              </Field>
              <Field label="API key">
                <Input
                  value={frappeKey}
                  onChange={(e) => setFrappeKey(e.target.value)}
                />
              </Field>
              <Field label="API secret">
                <Input
                  type="password"
                  value={frappeSecret}
                  onChange={(e) => setFrappeSecret(e.target.value)}
                />
              </Field>
              <Field
                label="DocTypes"
                help="Comma-separated list of record types to pull, e.g. Customer, Sales Invoice."
              >
                <Input
                  value={frappeDocTypes}
                  onChange={(e) => setFrappeDocTypes(e.target.value)}
                />
              </Field>
            </>
          )}

          {sourceType === "quickbooks" && (
            <Field label="Realm ID (company)">
              <Input
                value={qbRealmId}
                onChange={(e) => setQbRealmId(e.target.value)}
                placeholder="1234567890"
              />
            </Field>
          )}
          {sourceType === "xero" && (
            <Field label="Xero tenant ID">
              <Input
                value={xeroTenantId}
                onChange={(e) => setXeroTenantId(e.target.value)}
                placeholder="Organisation ID"
              />
            </Field>
          )}
          {isOAuthSource && (
            <>
              <p className="text-sm text-fg-muted">
                Provide an access token, or a refresh token plus client
                credentials to mint one.
              </p>
              <Field label="Access token">
                <Input
                  type="password"
                  value={accessToken}
                  onChange={(e) => setAccessToken(e.target.value)}
                />
              </Field>
              <Field label="Refresh token">
                <Input
                  type="password"
                  value={refreshToken}
                  onChange={(e) => setRefreshToken(e.target.value)}
                />
              </Field>
              <Field label="Client ID">
                <Input
                  value={clientId}
                  onChange={(e) => setClientId(e.target.value)}
                />
              </Field>
              <Field label="Client secret">
                <Input
                  type="password"
                  value={clientSecret}
                  onChange={(e) => setClientSecret(e.target.value)}
                />
              </Field>
            </>
          )}
          {sourceType === "tally" && (
            <>
              <Field label="Export format">
                <Select
                  value={tallyFormat}
                  onChange={(e) => setTallyFormat(e.target.value as "xml" | "json")}
                >
                  <option value="xml">XML</option>
                  <option value="json">JSON</option>
                </Select>
              </Field>
              <Field label="Tally export data (masters + vouchers)">
                <Textarea
                  value={tallyPayload}
                  onChange={(e) => setTallyPayload(e.target.value)}
                  rows={10}
                  className="font-mono"
                />
              </Field>
            </>
          )}

          {error != null && (
            <p className="text-sm text-danger">{(error as Error).message}</p>
          )}
          <div>
            <Button type="submit" disabled={pending}>
              {pending ? "Creating job…" : "Create job and continue"}
            </Button>
          </div>
        </form>
      </CardContent>
    </Card>
  );
}

function StepMapping({
  job,
  onSubmit,
  pending,
  error,
}: {
  job: ImportJob;
  onSubmit: (mapping: unknown) => void;
  pending: boolean;
  error: unknown;
}) {
  const fmt = useFormatter();
  const entities = useMemo(() => {
    const p = job.progress as {
      source?: { entities?: Array<{ name: string; target_ktype?: string }> };
    };
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
    <Card>
      <CardHeader>
        <CardTitle>Step 2. Mapping</CardTitle>
        <CardDescription>
          Set the record type for each thing we found in your source. Individual
          field names are matched automatically — fine-tune them under Advanced
          field mapping.
        </CardDescription>
      </CardHeader>
      <CardContent className="flex flex-col gap-4">
        {entities.length === 0 ? (
          <EmptyState
            icon={<Database />}
            title="Nothing to map yet"
            description="We didn't find any source tables to map. Go back and check the source data."
          />
        ) : (
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>Source</TableHead>
                <TableHead className="text-end">Rows</TableHead>
                <TableHead>Record type</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {entities.map((e) => {
                const rowCount = (e as { row_count?: number }).row_count;
                return (
                  <TableRow key={e.name}>
                    <TableCell className="font-medium text-fg">
                      {humanizeLabel(e.name)}
                    </TableCell>
                    <TableCell className="text-end font-tabular">
                      {rowCount == null ? "—" : fmt.number(rowCount)}
                    </TableCell>
                    <TableCell>
                      <Input
                        aria-label={`Record type for ${humanizeLabel(e.name)}`}
                        value={mapping[e.name]?.target_ktype ?? ""}
                        onChange={(ev) =>
                          setMapping((m) => ({
                            ...m,
                            [e.name]: { target_ktype: ev.target.value },
                          }))
                        }
                        placeholder="crm.account"
                        className="font-mono"
                      />
                    </TableCell>
                  </TableRow>
                );
              })}
            </TableBody>
          </Table>
        )}
        {error != null && (
          <p className="text-sm text-danger">{(error as Error).message}</p>
        )}
        <div className="flex flex-wrap items-center gap-3">
          <Button
            onClick={() => onSubmit({ entities: mapping })}
            disabled={pending || entities.length === 0}
          >
            {pending ? "Saving…" : "Save mapping"}
          </Button>
          <Button asChild variant="link">
            <Link to={`/imports/${job.id}/mapping`}>Advanced field mapping</Link>
          </Button>
        </div>
      </CardContent>
    </Card>
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
  const fmt = useFormatter();
  return (
    <Card>
      <CardHeader>
        <CardTitle>Step 3. Validate</CardTitle>
        <CardDescription>
          We check every row against your schema and links between records, then
          list anything that needs fixing.
        </CardDescription>
      </CardHeader>
      <CardContent className="flex flex-col gap-4">
        <div>
          <Button onClick={onRun} disabled={pending}>
            {pending ? "Checking…" : "Run validation"}
          </Button>
        </div>

        {errors.length > 0 ? (
          <div className="flex flex-col gap-3">
            <div className="flex items-center gap-2">
              <Badge variant="danger">
                {fmt.number(errors.length)}{" "}
                {errors.length === 1 ? "row needs fixing" : "rows need fixing"}
              </Badge>
            </div>
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>Source row</TableHead>
                  <TableHead>Record type</TableHead>
                  <TableHead>What needs fixing</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {errors.map((row) => (
                  <TableRow key={row.id}>
                    <TableCell className="font-medium text-fg">
                      {row.source_id || "—"}
                    </TableCell>
                    <TableCell>{row.target_ktype}</TableCell>
                    <TableCell>
                      <ul className="flex flex-col gap-1">
                        {(row.validation_errors ?? []).map((e, i) => (
                          <li key={i} className="flex flex-wrap items-center gap-1.5">
                            {e.field && (
                              <Badge variant="outline" size="xs">
                                {humanizeLabel(e.field)}
                              </Badge>
                            )}
                            <span className="text-sm">{e.message}</span>
                          </li>
                        ))}
                      </ul>
                    </TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          </div>
        ) : job.status === "reconciling" ? (
          <div className="flex items-center gap-2 rounded-md border border-success/40 bg-success/10 p-3 text-sm text-success">
            <CheckCircle2 className="h-4 w-4" aria-hidden="true" />
            Every row passed — continue to review.
          </div>
        ) : null}
      </CardContent>
    </Card>
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
  const fmt = useFormatter();
  const rec = job.reconciliation as {
    source_count?: number;
    staged_count?: number;
    valid_count?: number;
    invalid_count?: number;
    discrepancies?: string[];
  };
  const validCount = rec.valid_count ?? 0;
  const stat = (n?: number) => (n == null ? "—" : fmt.number(n));
  return (
    <Card>
      <CardHeader>
        <CardTitle>Step 4. Review &amp; Accept</CardTitle>
        <CardDescription>
          Confirm the totals below. Only valid rows are brought live; you can fix
          and re-import the rest later.
        </CardDescription>
      </CardHeader>
      <CardContent className="flex flex-col gap-4">
        <div className="grid grid-cols-2 gap-3 sm:grid-cols-4">
          <StatCard label="In source" value={stat(rec.source_count)} />
          <StatCard label="Staged" value={stat(rec.staged_count)} />
          <StatCard label="Valid" value={stat(rec.valid_count)} />
          <StatCard label="Invalid" value={stat(rec.invalid_count)} />
        </div>

        {rec.discrepancies && rec.discrepancies.length > 0 && (
          <div className="rounded-md border border-danger/40 bg-danger/10 p-3">
            <p className="flex items-center gap-1.5 text-sm font-medium text-danger">
              <XCircle className="h-4 w-4" aria-hidden="true" />
              Discrepancies found
            </p>
            <ul className="mt-1 list-disc pl-5 text-sm text-danger">
              {rec.discrepancies.map((d) => (
                <li key={d}>{d}</li>
              ))}
            </ul>
          </div>
        )}

        <div>
          <Button onClick={onAccept} disabled={pending}>
            {pending
              ? "Importing…"
              : `Accept & import (${fmt.number(validCount)} ${
                  validCount === 1 ? "row" : "rows"
                })`}
          </Button>
        </div>
        {errors.length > 0 && (
          <p className="text-sm text-fg-muted">
            {fmt.number(errors.length)} invalid{" "}
            {errors.length === 1 ? "row" : "rows"} will be skipped. Fix the
            source and re-import them later.
          </p>
        )}
      </CardContent>
    </Card>
  );
}

function StepComplete({ job }: { job: ImportJob }) {
  const fmt = useFormatter();
  const imported = (job.progress as { imported?: number }).imported ?? 0;
  const ok = job.status === "completed";
  return (
    <Card>
      <CardContent className="py-10">
        <EmptyState
          icon={ok ? <CheckCircle2 /> : <XCircle />}
          title={ok ? "Step 5. Complete" : "Step 5. Failed"}
          description={
            ok
              ? `Imported ${fmt.number(imported)} ${
                  imported === 1 ? "record" : "records"
                } on ${fmt.dateTime(new Date(job.completed_at ?? job.updated_at))}.`
              : `This import failed on ${fmt.dateTime(
                  new Date(job.completed_at ?? job.updated_at),
                )}. Review the source data and try again.`
          }
          action={
            <Button asChild variant="secondary">
              <Link to="/imports">
                <ArrowLeft className="h-4 w-4" />
                Back to imports
              </Link>
            </Button>
          }
        />
      </CardContent>
    </Card>
  );
}
