import { useState, useMemo, useEffect } from "react";
import { Link, useParams, useNavigate } from "react-router-dom";
import { useQuery, useMutation, useQueryClient } from "@tanstack/react-query";
import { ArrowLeft, Database } from "lucide-react";
import type { KType } from "@kapp/client";
import {
  Badge,
  Button,
  Card,
  CardContent,
  CardHeader,
  CardTitle,
  EmptyState,
  Field,
  Select,
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@kapp/ui";
import { api } from "../lib/api";
import { useFormatter } from "../lib/i18n";
import { humanizeLabel, ktypeSingular } from "../lib/ktypeView";
import {
  AdminErrorState,
  AdminPageHeader,
  AdminTableSkeleton,
} from "./adminKit";

/**
 * ImportMappingPage is the advanced mapping editor reached from
 * ImportPage's step 2 when the operator needs per-field control.
 * Source rows come from the job's `progress.source.entities` payload
 * (written during Discover); target KType field lists come from the
 * KType registry. A field-level save round-trips via
 * POST /api/v1/imports/{id}/map with shape:
 *
 *   {
 *     "mapping": {
 *       "entities": {
 *         "<source_entity>": {
 *           "target_ktype": "<ktype>",
 *           "fields": { "<source_field>": "<target_field>" }
 *         }
 *       }
 *     }
 *   }
 */

interface ImportJob {
  id: string;
  status: string;
  source_type: string;
  progress: Record<string, unknown>;
  mapping: Record<string, unknown>;
}

interface SourceEntity {
  name: string;
  row_count?: number;
  fields?: string[];
  target_ktype?: string;
}

interface EntityMapping {
  target_ktype: string;
  fields: Record<string, string>;
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
  return (await res.json()) as T;
}

export function ImportMappingPage() {
  const { id } = useParams<{ id: string }>();
  const navigate = useNavigate();
  const qc = useQueryClient();
  const fmt = useFormatter();

  const jobQ = useQuery({
    queryKey: ["imports", id],
    queryFn: () => apiFetch<ImportJob>(`/imports/${id}`),
    enabled: !!id,
  });
  const ktypesQ = useQuery({
    queryKey: ["ktypes"],
    queryFn: () => api.listKTypes(),
  });

  const entities = useMemo(() => {
    const progress = jobQ.data?.progress as
      | { source?: { entities?: SourceEntity[] } }
      | undefined;
    return progress?.source?.entities ?? [];
  }, [jobQ.data]);

  const existing =
    (jobQ.data?.mapping as { entities?: Record<string, EntityMapping> } | undefined)
      ?.entities ?? {};

  const [mapping, setMapping] = useState<Record<string, EntityMapping>>({});

  useEffect(() => {
    if (Object.keys(mapping).length > 0) return;
    const next: Record<string, EntityMapping> = {};
    for (const e of entities) {
      next[e.name] = existing[e.name] ?? {
        target_ktype: e.target_ktype ?? "",
        fields: {},
      };
    }
    if (Object.keys(next).length > 0) setMapping(next);
  }, [entities, existing, mapping]);

  const save = useMutation({
    mutationFn: () =>
      apiFetch<ImportJob>(`/imports/${id}/map`, {
        method: "POST",
        body: JSON.stringify({ mapping: { entities: mapping } }),
      }),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["imports", id] });
      navigate(`/imports/${id}`);
    },
  });

  const ktypeByName = useMemo(() => {
    const m = new Map<string, KType>();
    (ktypesQ.data ?? []).forEach((k) => m.set(k.name, k));
    return m;
  }, [ktypesQ.data]);

  const ktypeOptions = useMemo(
    () =>
      (ktypesQ.data ?? [])
        .map((k) => ({ value: k.name, label: ktypeSingular(k.name) }))
        .sort((a, b) => a.label.localeCompare(b.label)),
    [ktypesQ.data],
  );

  return (
    <section className="flex flex-col gap-6">
      <AdminPageHeader
        area="Onboarding"
        title="Field mapping"
        description="Match each column in your source to a field on the matching record type. Columns left as “Skip” won't be imported."
        actions={
          <Button asChild variant="ghost">
            <Link to={`/imports/${id}`}>
              <ArrowLeft className="h-4 w-4" />
              Back to wizard
            </Link>
          </Button>
        }
      />

      {jobQ.isLoading ? (
        <Card>
          <CardContent className="pt-4">
            <AdminTableSkeleton columns={["Your column", "Maps to field"]} rows={4} />
          </CardContent>
        </Card>
      ) : jobQ.isError ? (
        <AdminErrorState
          title="Couldn't load this import"
          error={jobQ.error}
          onRetry={() => jobQ.refetch()}
        />
      ) : entities.length === 0 ? (
        <Card>
          <CardContent className="py-10">
            <EmptyState
              icon={<Database />}
              title="No discovered entities yet"
              description="Re-run discovery from the wizard to pull in your source tables, then come back to match their columns."
              action={
                <Button asChild variant="secondary">
                  <Link to={`/imports/${id}`}>
                    <ArrowLeft className="h-4 w-4" />
                    Back to wizard
                  </Link>
                </Button>
              }
            />
          </CardContent>
        </Card>
      ) : (
        <div className="flex flex-col gap-4">
          {entities.map((e) => {
            const em = mapping[e.name] ?? { target_ktype: "", fields: {} };
            const targetKType = ktypeByName.get(em.target_ktype);
            const targetFields = targetKType?.schema.fields.map((f) => f.name) ?? [];
            const sourceFields = e.fields ?? [];
            const label = humanizeLabel(e.name);
            return (
              <Card key={e.name}>
                <CardHeader className="flex flex-row flex-wrap items-end justify-between gap-3">
                  <div className="flex items-center gap-2">
                    <CardTitle>{label}</CardTitle>
                    <Badge variant="neutral">
                      {fmt.number(e.row_count ?? 0)} rows
                    </Badge>
                  </div>
                  <Field label="Record type" className="w-full sm:w-64">
                    <Select
                      aria-label={`Record type for ${label}`}
                      value={em.target_ktype}
                      onChange={(ev) => {
                        const v = ev.target.value;
                        setMapping((m) => ({
                          ...m,
                          [e.name]: {
                            target_ktype: v,
                            fields: m[e.name]?.fields ?? {},
                          },
                        }));
                      }}
                    >
                      <option value="">Choose a record type…</option>
                      {ktypeOptions.map((k) => (
                        <option key={k.value} value={k.value}>
                          {k.label}
                        </option>
                      ))}
                    </Select>
                  </Field>
                </CardHeader>
                <CardContent>
                  {sourceFields.length === 0 ? (
                    <p className="text-sm text-fg-muted">
                      No columns were discovered for this table.
                    </p>
                  ) : !em.target_ktype ? (
                    <p className="text-sm text-fg-muted">
                      Choose a record type above to start matching columns.
                    </p>
                  ) : (
                    <Table>
                      <TableHeader>
                        <TableRow>
                          <TableHead>Your column</TableHead>
                          <TableHead>Maps to field</TableHead>
                        </TableRow>
                      </TableHeader>
                      <TableBody>
                        {sourceFields.map((sf) => (
                          <TableRow key={sf}>
                            <TableCell className="font-mono text-sm text-fg">
                              {sf}
                            </TableCell>
                            <TableCell>
                              <Field label={`Target field for ${sf}`} hideLabel>
                                <Select
                                  value={em.fields[sf] ?? ""}
                                  onChange={(ev) => {
                                    const v = ev.target.value;
                                    setMapping((m) => {
                                      const current = m[e.name] ?? {
                                        target_ktype: "",
                                        fields: {},
                                      };
                                      return {
                                        ...m,
                                        [e.name]: {
                                          ...current,
                                          fields: { ...current.fields, [sf]: v },
                                        },
                                      };
                                    });
                                  }}
                                >
                                  <option value="">Skip this column</option>
                                  {targetFields.map((tf) => (
                                    <option key={tf} value={tf}>
                                      {humanizeLabel(tf)}
                                    </option>
                                  ))}
                                </Select>
                              </Field>
                            </TableCell>
                          </TableRow>
                        ))}
                      </TableBody>
                    </Table>
                  )}
                </CardContent>
              </Card>
            );
          })}

          <div className="flex flex-wrap items-center gap-3">
            <Button onClick={() => save.mutate()} disabled={save.isPending}>
              {save.isPending ? "Saving…" : "Save mapping"}
            </Button>
            <Button asChild variant="ghost">
              <Link to={`/imports/${id}`}>Cancel</Link>
            </Button>
          </div>
        </div>
      )}
    </section>
  );
}
