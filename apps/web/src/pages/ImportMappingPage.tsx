import { useState, useMemo, useEffect } from "react";
import { Link, useParams, useNavigate } from "react-router-dom";
import { useQuery, useMutation, useQueryClient } from "@tanstack/react-query";
import type { KType } from "@kapp/client";
import { api } from "../lib/api";

/**
 * ImportMappingPage is the drag-free advanced mapping editor reached
 * from ImportPage's step 2 when the operator needs per-field control.
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
    const progress = jobQ.data?.progress as {
      source?: { entities?: SourceEntity[] };
    } | undefined;
    return progress?.source?.entities ?? [];
  }, [jobQ.data]);

  const existing = (jobQ.data?.mapping as { entities?: Record<string, EntityMapping> } | undefined)?.entities ?? {};

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

  return (
    <section>
      <div style={{ marginBottom: 8 }}>
        <Link to={`/imports/${id}`}>← Back to wizard</Link>
      </div>
      <h1>Field mapping</h1>
      {jobQ.isLoading && <p>Loading…</p>}
      {jobQ.data && entities.length === 0 && (
        <p style={{ color: "#6b7280" }}>
          No discovered entities yet — re-run discovery from the wizard.
        </p>
      )}
      {entities.map((e) => {
        const em = mapping[e.name] ?? { target_ktype: "", fields: {} };
        const targetKType = ktypeByName.get(em.target_ktype);
        const targetFields = targetKType?.schema.fields.map((f) => f.name) ?? [];
        const sourceFields = e.fields ?? [];
        return (
          <div
            key={e.name}
            style={{
              border: "1px solid #e5e7eb",
              borderRadius: 4,
              padding: 12,
              marginBottom: 16,
            }}
          >
            <div style={{ display: "flex", justifyContent: "space-between" }}>
              <h3 style={{ margin: 0 }}>{e.name}</h3>
              <span style={{ fontSize: 12, color: "#6b7280" }}>
                {e.row_count ?? 0} rows
              </span>
            </div>
            <label style={{ display: "block", marginTop: 8 }}>
              Target KType
              <select
                value={em.target_ktype}
                onChange={(ev) => {
                  const v = ev.target.value;
                  setMapping((m) => ({
                    ...m,
                    [e.name]: { target_ktype: v, fields: m[e.name]?.fields ?? {} },
                  }));
                }}
                style={{ marginLeft: 8 }}
              >
                <option value="">(select)</option>
                {(ktypesQ.data ?? []).map((k) => (
                  <option key={k.name} value={k.name}>
                    {k.name}
                  </option>
                ))}
              </select>
            </label>
            {sourceFields.length > 0 && (
              <table
                style={{
                  width: "100%",
                  borderCollapse: "collapse",
                  fontSize: 13,
                  marginTop: 8,
                }}
              >
                <thead>
                  <tr style={{ textAlign: "left", borderBottom: "1px solid #e5e7eb" }}>
                    <th style={{ padding: "4px 6px" }}>Source field</th>
                    <th style={{ padding: "4px 6px" }}>Target field</th>
                  </tr>
                </thead>
                <tbody>
                  {sourceFields.map((sf) => (
                    <tr key={sf} style={{ borderBottom: "1px solid #f3f4f6" }}>
                      <td style={{ padding: "4px 6px" }}>{sf}</td>
                      <td style={{ padding: "4px 6px" }}>
                        <select
                          value={em.fields[sf] ?? ""}
                          onChange={(ev) => {
                            const v = ev.target.value;
                            setMapping((m) => ({
                              ...m,
                              [e.name]: {
                                ...em,
                                fields: { ...em.fields, [sf]: v },
                              },
                            }));
                          }}
                        >
                          <option value="">(skip)</option>
                          {targetFields.map((tf) => (
                            <option key={tf} value={tf}>
                              {tf}
                            </option>
                          ))}
                        </select>
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            )}
          </div>
        );
      })}
      <div>
        <button onClick={() => save.mutate()} disabled={save.isPending}>
          {save.isPending ? "Saving…" : "Save mapping"}
        </button>
      </div>
    </section>
  );
}
