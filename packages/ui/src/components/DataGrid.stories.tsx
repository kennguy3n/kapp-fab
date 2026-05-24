import type { Meta, StoryObj } from "@storybook/react";
import { useState, type Key } from "react";
import { DataGrid, type DataGridColumn } from "./DataGrid";
import { Badge } from "./Badge";

interface Invoice {
  id: string;
  customer: string;
  amount: number;
  status: "paid" | "pending" | "overdue";
  due: string;
}

const data: Invoice[] = [
  { id: "INV-001", customer: "Acme Corp", amount: 1840.5, status: "paid", due: "2025-05-01" },
  { id: "INV-002", customer: "Globex", amount: 2400.0, status: "pending", due: "2025-05-15" },
  { id: "INV-003", customer: "Initech", amount: 950.0, status: "overdue", due: "2025-04-20" },
  { id: "INV-004", customer: "Hooli", amount: 3200.0, status: "paid", due: "2025-05-22" },
  { id: "INV-005", customer: "Stark Industries", amount: 14500.0, status: "pending", due: "2025-06-05" },
  { id: "INV-006", customer: "Wayne Enterprises", amount: 8200.0, status: "paid", due: "2025-05-30" },
  { id: "INV-007", customer: "Tyrell Corp", amount: 670.0, status: "overdue", due: "2025-04-15" },
  { id: "INV-008", customer: "Cyberdyne Systems", amount: 5400.0, status: "pending", due: "2025-06-12" },
  { id: "INV-009", customer: "Massive Dynamic", amount: 12300.0, status: "paid", due: "2025-05-10" },
  { id: "INV-010", customer: "Soylent Corp", amount: 990.0, status: "overdue", due: "2025-04-25" },
  { id: "INV-011", customer: "Pied Piper", amount: 4400.0, status: "paid", due: "2025-05-28" },
  { id: "INV-012", customer: "Aperture Science", amount: 7200.0, status: "pending", due: "2025-06-15" },
];

const columns: DataGridColumn<Invoice>[] = [
  {
    key: "id",
    header: "Invoice",
    cell: (row) => <span className="font-medium">{row.id}</span>,
    sortable: true,
  },
  {
    key: "customer",
    header: "Customer",
    cell: (row) => row.customer,
    sortable: true,
  },
  {
    key: "amount",
    header: "Amount",
    headerClassName: "text-right",
    className: "text-right",
    cell: (row) => `$${row.amount.toFixed(2)}`,
    sortable: true,
    compare: (a, b) => a.amount - b.amount,
  },
  {
    key: "status",
    header: "Status",
    cell: (row) => {
      const variant =
        row.status === "paid"
          ? "success"
          : row.status === "pending"
            ? "warning"
            : "danger";
      return <Badge variant={variant}>{row.status}</Badge>;
    },
  },
  { key: "due", header: "Due", cell: (row) => row.due, sortable: true },
];

const meta: Meta<typeof DataGrid<Invoice>> = {
  title: "UI/DataGrid",
  parameters: { layout: "padded" },
};

export default meta;
type Story = StoryObj;

export const Sortable: Story = {
  render: () => (
    <DataGrid<Invoice>
      data={data}
      columns={columns}
      rowKey={(row) => row.id}
      pageSize={5}
    />
  ),
};

export const WithSelection: Story = {
  render: () => {
    const [selected, setSelected] = useState<Set<Key>>(new Set());
    return (
      <div className="flex flex-col gap-2">
        <div className="text-sm text-fg-muted">
          {selected.size} of {data.length} selected
        </div>
        <DataGrid<Invoice>
          data={data}
          columns={columns}
          rowKey={(row) => row.id}
          pageSize={5}
          selectedKeys={selected}
          onSelectionChange={setSelected}
        />
      </div>
    );
  },
};

export const Empty: Story = {
  render: () => (
    <DataGrid<Invoice>
      data={[]}
      columns={columns}
      rowKey={(row) => row.id}
      pageSize={5}
      emptyState="No invoices yet. Click 'New invoice' to get started."
    />
  ),
};
