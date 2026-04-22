import type { Meta, StoryObj } from "@storybook/react";
import type { KType, KRecord } from "@kapp/client";
import { KTypeList } from "../../web/src/components/KTypeList";

const dealKType: KType = {
  name: "crm.deal",
  version: 1,
  schema: {
    name: "crm.deal",
    version: 1,
    fields: [
      { name: "name", type: "string" },
      { name: "amount", type: "number" },
      { name: "stage", type: "enum", values: ["lead", "qualified", "won"] },
      { name: "close_date", type: "date" },
    ],
    views: {
      list: { columns: ["name", "amount", "stage", "close_date"] },
    },
  },
};

const dealRecords: KRecord[] = [
  {
    id: "00000000-0000-0000-0000-000000000001",
    tenant_id: "11111111-1111-1111-1111-111111111111",
    ktype: "crm.deal",
    ktype_version: 1,
    status: "active",
    version: 1,
    created_at: "2026-03-01T10:00:00Z",
    updated_at: "2026-03-01T10:00:00Z",
    data: {
      name: "Acme Corp renewal",
      amount: 12500,
      stage: "proposal",
      close_date: "2026-06-30",
    },
  },
  {
    id: "00000000-0000-0000-0000-000000000002",
    tenant_id: "11111111-1111-1111-1111-111111111111",
    ktype: "crm.deal",
    ktype_version: 1,
    status: "active",
    version: 1,
    created_at: "2026-03-02T10:00:00Z",
    updated_at: "2026-03-02T10:00:00Z",
    data: {
      name: "Widgets Inc expansion",
      amount: 48000,
      stage: "qualified",
      close_date: "2026-07-15",
    },
  },
  {
    id: "00000000-0000-0000-0000-000000000003",
    tenant_id: "11111111-1111-1111-1111-111111111111",
    ktype: "crm.deal",
    ktype_version: 1,
    status: "active",
    version: 1,
    created_at: "2026-03-03T10:00:00Z",
    updated_at: "2026-03-03T10:00:00Z",
    data: {
      name: "Globex onboarding",
      amount: 7200,
      stage: "won",
      close_date: "2026-03-31",
    },
  },
];

const meta: Meta<typeof KTypeList> = {
  title: "Kernel/RecordList",
  component: KTypeList,
  args: {
    onRowClick: (record) => {
      // eslint-disable-next-line no-console
      console.log("row click", record.id);
    },
  },
};

export default meta;
type Story = StoryObj<typeof KTypeList>;

export const Populated: Story = {
  args: {
    ktype: dealKType,
    records: dealRecords,
  },
};

export const Empty: Story = {
  args: {
    ktype: dealKType,
    records: [],
  },
};
