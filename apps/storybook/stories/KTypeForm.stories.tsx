import type { Meta, StoryObj } from "@storybook/react";
import type { KType } from "@kapp/client";
import { KTypeForm } from "../../web/src/components/KTypeForm";

const dealKType: KType = {
  name: "crm.deal",
  version: 1,
  schema: {
    name: "crm.deal",
    version: 1,
    fields: [
      { name: "name", type: "string", required: true, max_length: 200 },
      { name: "amount", type: "number", required: true, min: 0 },
      {
        name: "stage",
        type: "enum",
        required: true,
        values: ["lead", "qualified", "proposal", "won", "lost"],
      },
      { name: "close_date", type: "date" },
      { name: "notes", type: "text", max_length: 2000 },
    ],
  },
};

const employeeKType: KType = {
  name: "hr.employee",
  version: 1,
  schema: {
    name: "hr.employee",
    version: 1,
    fields: [
      { name: "first_name", type: "string", required: true },
      { name: "last_name", type: "string", required: true },
      { name: "email", type: "string", required: true },
      {
        name: "department",
        type: "enum",
        values: ["Engineering", "Sales", "Operations", "Finance"],
      },
      { name: "start_date", type: "date", required: true },
      { name: "active", type: "boolean" },
      {
        name: "manager",
        type: "ref",
        ref: "hr.employee",
        ktype: "hr.employee",
      },
    ],
  },
};

const meta: Meta<typeof KTypeForm> = {
  title: "Kernel/KTypeForm",
  component: KTypeForm,
  args: {
    onSubmit: (data) => {
      // eslint-disable-next-line no-console
      console.log("submit", data);
    },
  },
};

export default meta;
type Story = StoryObj<typeof KTypeForm>;

export const Deal: Story = {
  args: {
    ktype: dealKType,
  },
};

export const DealPrefilled: Story = {
  args: {
    ktype: dealKType,
    initialData: {
      name: "Acme Corp renewal",
      amount: 12500,
      stage: "proposal",
      close_date: "2026-06-30",
      notes: "Renewal discussion scheduled for Q2.",
    },
  },
};

export const Employee: Story = {
  args: {
    ktype: employeeKType,
  },
};

export const EmployeePrefilled: Story = {
  args: {
    ktype: employeeKType,
    initialData: {
      first_name: "Ada",
      last_name: "Lovelace",
      email: "ada@example.com",
      department: "Engineering",
      start_date: "2025-09-01",
      active: true,
    },
  },
};
