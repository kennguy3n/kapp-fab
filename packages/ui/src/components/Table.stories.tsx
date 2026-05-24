import type { Meta, StoryObj } from "@storybook/react";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
  TableCaption,
} from "./Table";

const meta: Meta<typeof Table> = {
  title: "UI/Table",
  component: Table,
  parameters: { layout: "padded" },
};

export default meta;
type Story = StoryObj<typeof Table>;

const invoices = [
  { id: "INV-001", customer: "Acme Corp", amount: 1840.5, status: "Paid" },
  { id: "INV-002", customer: "Globex", amount: 2400.0, status: "Pending" },
  { id: "INV-003", customer: "Initech", amount: 950.0, status: "Overdue" },
  { id: "INV-004", customer: "Hooli", amount: 3200.0, status: "Paid" },
];

export const Basic: Story = {
  render: () => (
    <Table>
      <TableCaption>A list of recent invoices.</TableCaption>
      <TableHeader>
        <TableRow>
          <TableHead>Invoice</TableHead>
          <TableHead>Customer</TableHead>
          <TableHead className="text-right">Amount</TableHead>
          <TableHead>Status</TableHead>
        </TableRow>
      </TableHeader>
      <TableBody>
        {invoices.map((row) => (
          <TableRow key={row.id}>
            <TableCell className="font-medium">{row.id}</TableCell>
            <TableCell>{row.customer}</TableCell>
            <TableCell className="text-right">
              ${row.amount.toFixed(2)}
            </TableCell>
            <TableCell>{row.status}</TableCell>
          </TableRow>
        ))}
      </TableBody>
    </Table>
  ),
};
