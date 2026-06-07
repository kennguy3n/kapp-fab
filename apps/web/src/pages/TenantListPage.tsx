import { useQuery } from "@tanstack/react-query";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@kapp/ui";
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
      <Table>
        <TableHeader>
          <TableRow>
            <TableHead>Slug</TableHead>
            <TableHead>Name</TableHead>
            <TableHead>Plan</TableHead>
            <TableHead>Status</TableHead>
          </TableRow>
        </TableHeader>
        <TableBody>
          {tenants.map((t) => (
            <TableRow key={t.id}>
              <TableCell>{t.slug}</TableCell>
              <TableCell>{t.name}</TableCell>
              <TableCell>{t.plan}</TableCell>
              <TableCell>{t.status}</TableCell>
            </TableRow>
          ))}
        </TableBody>
      </Table>
    </section>
  );
}
