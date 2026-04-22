import { useQuery } from "@tanstack/react-query";
import { api } from "../lib/api";

export function TenantListPage() {
  const tenantsQuery = useQuery({
    queryKey: ["tenants"],
    queryFn: () => api.listTenants(),
  });

  if (tenantsQuery.isLoading) return <div>Loading tenants…</div>;
  if (tenantsQuery.error) return <div>Error loading tenants.</div>;

  const tenants = tenantsQuery.data ?? [];
  if (tenants.length === 0) {
    return (
      <section>
        <h1>Tenants</h1>
        <p>No tenants registered yet.</p>
      </section>
    );
  }

  return (
    <section>
      <h1>Tenants</h1>
      <table style={{ width: "100%", borderCollapse: "collapse" }}>
        <thead>
          <tr>
            <th style={{ textAlign: "left", borderBottom: "1px solid #e5e7eb" }}>
              Slug
            </th>
            <th style={{ textAlign: "left", borderBottom: "1px solid #e5e7eb" }}>
              Name
            </th>
            <th style={{ textAlign: "left", borderBottom: "1px solid #e5e7eb" }}>
              Plan
            </th>
            <th style={{ textAlign: "left", borderBottom: "1px solid #e5e7eb" }}>
              Status
            </th>
          </tr>
        </thead>
        <tbody>
          {tenants.map((t) => (
            <tr key={t.id} style={{ borderBottom: "1px solid #f3f4f6" }}>
              <td style={{ padding: "6px 4px" }}>{t.slug}</td>
              <td style={{ padding: "6px 4px" }}>{t.name}</td>
              <td style={{ padding: "6px 4px" }}>{t.plan}</td>
              <td style={{ padding: "6px 4px" }}>{t.status}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </section>
  );
}
