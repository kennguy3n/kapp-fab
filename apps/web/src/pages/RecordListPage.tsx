import { useParams, useNavigate } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";
import { api } from "../lib/api";
import { KTypeList } from "../components/KTypeList";

export function RecordListPage() {
  const { ktype } = useParams<{ ktype: string }>();
  const navigate = useNavigate();

  const ktypeQuery = useQuery({
    queryKey: ["ktype", ktype],
    queryFn: () => api.getKType(ktype!),
    enabled: !!ktype,
  });

  const recordsQuery = useQuery({
    queryKey: ["records", ktype],
    queryFn: () => api.listRecords(ktype!),
    enabled: !!ktype,
  });

  if (!ktype) return null;
  if (ktypeQuery.isLoading || recordsQuery.isLoading) return <div>Loading…</div>;
  if (ktypeQuery.error) return <div>Error loading KType.</div>;
  if (!ktypeQuery.data) return <div>KType not found.</div>;

  return (
    <section>
      <header style={{ display: "flex", justifyContent: "space-between" }}>
        <h1>{ktypeQuery.data.name}</h1>
        <button onClick={() => navigate(`/records/${ktype}/new`)}>New</button>
      </header>
      <KTypeList
        ktype={ktypeQuery.data}
        records={recordsQuery.data ?? []}
        onRowClick={(r) => navigate(`/records/${ktype}/${r.id}`)}
      />
    </section>
  );
}
