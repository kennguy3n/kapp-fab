import { useState } from "react";
import { useParams, useNavigate } from "react-router-dom";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { api } from "../lib/api";
import { KTypeForm } from "../components/KTypeForm";

export function RecordFormPage() {
  const { ktype, id } = useParams<{ ktype: string; id?: string }>();
  const navigate = useNavigate();
  const qc = useQueryClient();
  const [printError, setPrintError] = useState<string | null>(null);
  const [printing, setPrinting] = useState<"pdf" | "html" | null>(null);

  const ktypeQuery = useQuery({
    queryKey: ["ktype", ktype],
    queryFn: () => api.getKType(ktype!),
    enabled: !!ktype,
  });

  const recordQuery = useQuery({
    queryKey: ["record", ktype, id],
    queryFn: () => api.getRecord(ktype!, id!),
    enabled: !!ktype && !!id,
  });

  const createMut = useMutation({
    mutationFn: (data: Record<string, unknown>) =>
      api.createRecord(ktype!, data),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["records", ktype] });
      navigate(`/records/${ktype}`);
    },
  });

  const updateMut = useMutation({
    mutationFn: (data: Record<string, unknown>) =>
      api.updateRecord(ktype!, id!, data),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["records", ktype] });
      qc.invalidateQueries({ queryKey: ["record", ktype, id] });
    },
  });

  if (!ktype) return null;
  // Edit flow must wait for the record to load too — KTypeForm seeds its
  // state from initialData via useState, which ignores later prop updates,
  // so mounting it before the record arrives would render a blank form.
  if (ktypeQuery.isLoading || (id && recordQuery.isLoading))
    return <div>Loading…</div>;
  if (!ktypeQuery.data) return <div>KType not found.</div>;
  if (id && !recordQuery.data) return <div>Record not found.</div>;

  // Print routes require X-Tenant-ID + Authorization headers, which
  // browser anchor navigation does not send. The buttons therefore
  // fetch the response through the API client (which injects the
  // auth headers) and pipe the Blob into a programmatic download so
  // the file or preview still lands in a new tab / on disk.
  const runPrint = async (variant: "pdf" | "html") => {
    if (!id || !ktype) return;
    setPrintError(null);
    setPrinting(variant);
    try {
      const blob =
        variant === "pdf"
          ? await api.recordPdf(ktype, id)
          : await api.recordHtml(ktype, id);
      const url = URL.createObjectURL(blob);
      if (variant === "pdf") {
        const a = document.createElement("a");
        a.href = url;
        a.download = `${ktype}-${id}.pdf`;
        document.body.appendChild(a);
        a.click();
        a.remove();
      } else {
        // HTML preview opens in a new tab so the user can print
        // from the browser's native dialog.
        window.open(url, "_blank", "noopener");
      }
      // Revoke after a tick so the new tab has time to load the
      // blob URL before it becomes invalid.
      window.setTimeout(() => URL.revokeObjectURL(url), 60_000);
    } catch (err) {
      setPrintError((err as Error).message);
    } finally {
      setPrinting(null);
    }
  };

  return (
    <section>
      <h1>
        {id ? "Edit" : "New"} {ktypeQuery.data.name}
      </h1>
      {id && (
        <div style={{ marginBottom: 12, display: "flex", gap: 8, alignItems: "center" }}>
          <button
            type="button"
            onClick={() => runPrint("pdf")}
            disabled={printing !== null}
          >
            {printing === "pdf" ? "Preparing PDF…" : "Download PDF"}
          </button>
          <button
            type="button"
            onClick={() => runPrint("html")}
            disabled={printing !== null}
          >
            {printing === "html" ? "Preparing preview…" : "Print preview (HTML)"}
          </button>
          {printError && (
            <span style={{ color: "#b91c1c", fontSize: 12 }}>{printError}</span>
          )}
        </div>
      )}
      <KTypeForm
        ktype={ktypeQuery.data}
        initialData={recordQuery.data?.data}
        onSubmit={(data) => (id ? updateMut.mutate(data) : createMut.mutate(data))}
      />
    </section>
  );
}
