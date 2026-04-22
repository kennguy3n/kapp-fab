import { useParams, useNavigate } from "react-router-dom";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { api } from "../lib/api";
import { KTypeForm } from "../components/KTypeForm";

export function RecordFormPage() {
  const { ktype, id } = useParams<{ ktype: string; id?: string }>();
  const navigate = useNavigate();
  const qc = useQueryClient();

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

  return (
    <section>
      <h1>
        {id ? "Edit" : "New"} {ktypeQuery.data.name}
      </h1>
      <KTypeForm
        ktype={ktypeQuery.data}
        initialData={recordQuery.data?.data}
        onSubmit={(data) => (id ? updateMut.mutate(data) : createMut.mutate(data))}
      />
    </section>
  );
}
