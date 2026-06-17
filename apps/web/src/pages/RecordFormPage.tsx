import { useState } from "react";
import { useParams, useNavigate } from "react-router-dom";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { AlertTriangle, ArrowLeft } from "lucide-react";
import { Button, EmptyState, Eyebrow, Skeleton, toast } from "@kapp/ui";
import { api } from "../lib/api";
import { KTypeForm } from "../components/KTypeForm";
import { humanizeToken, ktypeSingular } from "../lib/ktypeView";

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
    onError: (err) => {
      toast.error("Couldn't create record", {
        description: (err as Error).message,
      });
    },
  });

  const createAnotherMut = useMutation({
    mutationFn: (data: Record<string, unknown>) =>
      api.createRecord(ktype!, data),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["records", ktype] });
      toast.success("Record created");
    },
    onError: (err) => {
      toast.error("Couldn't create record", {
        description: (err as Error).message,
      });
    },
  });

  const updateMut = useMutation({
    mutationFn: (data: Record<string, unknown>) =>
      api.updateRecord(ktype!, id!, data),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["records", ktype] });
      qc.invalidateQueries({ queryKey: ["record", ktype, id] });
      navigate(`/records/${ktype}`);
    },
    onError: (err) => {
      toast.error("Couldn't save changes", {
        description: (err as Error).message,
      });
    },
  });

  if (!ktype) return null;
  // Edit flow must wait for the record to load too — KTypeForm seeds its
  // state from initialData via useState, which ignores later prop updates,
  // so mounting it before the record arrives would render a blank form.
  if (ktypeQuery.isLoading || (id && recordQuery.isLoading))
    return (
      <section className="space-y-4">
        <Skeleton className="h-8 w-48" />
        <div className="grid grid-cols-1 gap-4 sm:grid-cols-2">
          {Array.from({ length: 4 }).map((_, i) => (
            <Skeleton key={i} className="h-16 w-full" />
          ))}
        </div>
      </section>
    );
  if (!ktypeQuery.data)
    return (
      <EmptyState
        icon={<AlertTriangle />}
        title="We couldn't find that record type"
        description="This record type doesn't exist, or you don't have access to it. Check the link and try again."
      />
    );
  if (id && !recordQuery.data)
    return (
      <EmptyState
        icon={<AlertTriangle />}
        title="We couldn't find that record"
        description="It may have been deleted, or you don't have access to it."
        action={
          <Button
            variant="secondary"
            leadingIcon={<ArrowLeft className="size-4" />}
            onClick={() => navigate(`/records/${ktype}`)}
          >
            Back to list
          </Button>
        }
      />
    );

  const kt = ktypeQuery.data;
  const singular = ktypeSingular(kt.name);
  const area = humanizeToken(kt.name.split(".")[0] ?? kt.name);
  const saving =
    createMut.isPending || updateMut.isPending || createAnotherMut.isPending;

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
    <section className="mx-auto max-w-3xl space-y-6">
      <header className="flex flex-wrap items-start justify-between gap-3">
        <div className="min-w-0">
          <Eyebrow>{area}</Eyebrow>
          <h1 className="mt-1 text-2xl font-semibold tracking-tight text-fg">
            {id ? "Edit" : "New"} {singular}
          </h1>
        </div>
        {id && (
          <div className="flex flex-wrap items-center gap-2">
            <Button
              type="button"
              variant="outline"
              size="sm"
              onClick={() => runPrint("pdf")}
              disabled={printing !== null}
            >
              {printing === "pdf" ? "Preparing PDF…" : "Download PDF"}
            </Button>
            <Button
              type="button"
              variant="outline"
              size="sm"
              onClick={() => runPrint("html")}
              disabled={printing !== null}
            >
              {printing === "html"
                ? "Preparing preview…"
                : "Print preview (HTML)"}
            </Button>
          </div>
        )}
      </header>
      {printError && (
        <p className="text-sm text-danger" role="alert">
          {printError}
        </p>
      )}
      <KTypeForm
        ktype={kt}
        initialData={recordQuery.data?.data}
        submitting={saving}
        onCancel={() => navigate(`/records/${ktype}`)}
        onSubmit={async (data) => {
          // mutateAsync so KTypeForm can await the result and only clear
          // its dirty guard once the save succeeds — a failed save rejects
          // (and surfaces a toast via onError) while the guard stays armed.
          if (id) await updateMut.mutateAsync(data);
          else await createMut.mutateAsync(data);
        }}
        onSubmitAndAddAnother={
          id
            ? undefined
            : async (data) => {
                // mutateAsync so KTypeForm can await the save and only
                // reset once it resolves — a failure rejects, the form
                // keeps the input, and onError surfaces the toast.
                await createAnotherMut.mutateAsync(data);
              }
        }
      />
    </section>
  );
}
