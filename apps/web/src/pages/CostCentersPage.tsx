import { useMemo, useState, type FormEvent } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Plus } from "lucide-react";
import type { KRecord } from "@kapp/client";
import {
  Badge,
  Button,
  Eyebrow,
  Field,
  Input,
  Select,
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
  toast,
} from "@kapp/ui";
import { api } from "../lib/api";
import { FinanceError, TableSkeleton } from "../lib/finance/presentation";

// KType for cost centres; mirrors the constant in
// internal/ledger/cost_center.go. Hard-coded here because the page
// only ever drives this one KType.
const KTYPE = "finance.cost_center";

interface CostCenter {
  code: string;
  name: string;
  parent_code: string;
  active: boolean;
}

// Read the loosely-typed KRecord payload into a narrow shape without
// casting through `unknown` (every field is validated by type).
function readCostCenter(r: KRecord): CostCenter {
  const d = r.data;
  return {
    code: typeof d.code === "string" ? d.code : "",
    name: typeof d.name === "string" ? d.name : "",
    parent_code: typeof d.parent_code === "string" ? d.parent_code : "",
    active: d.active !== false,
  };
}

interface FlatNode {
  record: KRecord;
  cc: CostCenter;
  depth: number;
}

/**
 * CostCentersPage renders the tenant's cost-centre tree and supports
 * inline create / toggle-active. The hierarchy is materialised from
 * the flat `parent_code` pointer on the server rows in a single
 * client-side pass.
 */
export function CostCentersPage() {
  const qc = useQueryClient();
  const q = useQuery<KRecord[]>({
    queryKey: ["records", KTYPE],
    queryFn: () => api.listRecords(KTYPE),
  });

  const records = useMemo(() => q.data ?? [], [q.data]);
  const flat = useMemo(() => flatten(records), [records]);
  const codes = useMemo(
    () => new Set(records.map((r) => readCostCenter(r).code)),
    [records],
  );

  const createMutation = useMutation({
    mutationFn: (data: CostCenter) =>
      api.createRecord(KTYPE, {
        code: data.code,
        name: data.name,
        parent_code: data.parent_code || undefined,
        active: data.active,
      }),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["records", KTYPE] });
      toast.success("Cost centre added");
    },
    onError: (err) =>
      toast.error("Couldn't add cost centre", {
        description: err instanceof Error ? err.message : undefined,
      }),
  });

  const toggleMutation = useMutation({
    mutationFn: (r: KRecord) => {
      const cc = readCostCenter(r);
      return api.updateRecord(KTYPE, r.id, {
        code: cc.code,
        name: cc.name,
        parent_code: cc.parent_code || undefined,
        active: !cc.active,
      });
    },
    onSuccess: () => qc.invalidateQueries({ queryKey: ["records", KTYPE] }),
    onError: (err) =>
      toast.error("Couldn't update cost centre", {
        description: err instanceof Error ? err.message : undefined,
      }),
  });

  const [code, setCode] = useState("");
  const [name, setName] = useState("");
  const [parentCode, setParentCode] = useState("");
  const [touched, setTouched] = useState(false);

  const duplicate = code.trim() !== "" && codes.has(code.trim());
  const codeError = touched && !code.trim()
    ? "A code is required."
    : duplicate
      ? "That code is already in use."
      : undefined;
  const nameError = touched && !name.trim() ? "A name is required." : undefined;
  const canSubmit = !!code.trim() && !!name.trim() && !duplicate;

  const submit = (e: FormEvent) => {
    e.preventDefault();
    setTouched(true);
    if (!canSubmit) return;
    createMutation.mutate(
      {
        code: code.trim(),
        name: name.trim(),
        parent_code: parentCode,
        active: true,
      },
      {
        onSuccess: () => {
          setCode("");
          setName("");
          setParentCode("");
          setTouched(false);
        },
      },
    );
  };

  return (
    <section className="flex flex-col gap-5">
      <header className="flex flex-col gap-1">
        <Eyebrow>Finance</Eyebrow>
        <h1 className="text-2xl font-semibold tracking-tight text-fg">
          Cost Centers
        </h1>
        <p className="text-sm text-fg-muted">
          Tags that split your reports by team, project, or location. Nest
          them by choosing a parent.
        </p>
      </header>

      <form
        onSubmit={submit}
        className="flex flex-col gap-4 rounded-lg border border-border bg-bg-subtle p-4"
        noValidate
      >
        <h2 className="text-sm font-semibold text-fg">Add a cost centre</h2>
        <div className="grid gap-3 sm:grid-cols-3">
          <Field label="Code" required error={codeError} help="A short unique tag, e.g. SALES.">
            <Input
              value={code}
              onChange={(e) => setCode(e.target.value)}
              placeholder="SALES"
              invalid={!!codeError}
              required
            />
          </Field>
          <Field label="Name" required error={nameError}>
            <Input
              value={name}
              onChange={(e) => setName(e.target.value)}
              placeholder="Sales team"
              invalid={!!nameError}
              required
            />
          </Field>
          <Field label="Parent" help="Optional — leave blank for a top-level centre.">
            <Select
              value={parentCode}
              onChange={(e) => setParentCode(e.target.value)}
            >
              <option value="">No parent (top level)</option>
              {flat.map(({ cc }) => (
                <option key={cc.code} value={cc.code}>
                  {cc.code} — {cc.name}
                </option>
              ))}
            </Select>
          </Field>
        </div>
        <div className="flex items-center gap-3">
          <Button
            type="submit"
            leadingIcon={<Plus aria-hidden />}
            disabled={createMutation.isPending}
          >
            {createMutation.isPending ? "Adding…" : "Add cost centre"}
          </Button>
        </div>
      </form>

      {q.isLoading && <TableSkeleton columns={4} />}

      {q.isError && (
        <FinanceError
          title="Couldn't load cost centres"
          error={q.error}
          onRetry={() => void q.refetch()}
        />
      )}

      {q.data && records.length === 0 && (
        <div className="rounded-lg border border-border p-8">
          <p className="text-center text-sm text-fg-muted">
            No cost centres yet. Add your first one above to start splitting
            reports.
          </p>
        </div>
      )}

      {records.length > 0 && (
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead className="w-40">Code</TableHead>
              <TableHead>Name</TableHead>
              <TableHead className="w-28">Status</TableHead>
              <TableHead className="w-28 text-right">Actions</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {flat.map(({ record, cc, depth }) => (
              <TableRow key={record.id}>
                <TableCell className="font-mono text-xs text-fg-muted">
                  {cc.code}
                </TableCell>
                <TableCell>
                  <span
                    className={cc.active ? "text-fg" : "text-fg-subtle"}
                    style={{ paddingInlineStart: `${depth * 1.25}rem` }}
                  >
                    {cc.name}
                  </span>
                </TableCell>
                <TableCell>
                  {cc.active ? (
                    <Badge variant="success">Active</Badge>
                  ) : (
                    <Badge variant="outline">Inactive</Badge>
                  )}
                </TableCell>
                <TableCell className="text-right">
                  <Button
                    variant="ghost"
                    size="sm"
                    disabled={toggleMutation.isPending}
                    onClick={() => toggleMutation.mutate(record)}
                  >
                    {cc.active ? "Deactivate" : "Activate"}
                  </Button>
                </TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      )}
    </section>
  );
}

// flatten produces a depth-first, indented list from the flat
// parent_code pointers. Entries whose parent is missing are roots, so
// orphaned rows still render rather than vanishing.
function flatten(records: KRecord[]): FlatNode[] {
  const byParent = new Map<string, KRecord[]>();
  const codes = new Set(records.map((r) => readCostCenter(r).code));
  for (const r of records) {
    const cc = readCostCenter(r);
    const parent =
      cc.parent_code && codes.has(cc.parent_code) ? cc.parent_code : "";
    const list = byParent.get(parent);
    if (list) list.push(r);
    else byParent.set(parent, [r]);
  }
  for (const list of byParent.values()) {
    list.sort((a, b) =>
      readCostCenter(a).code.localeCompare(readCostCenter(b).code),
    );
  }
  const out: FlatNode[] = [];
  const walk = (parent: string, depth: number) => {
    for (const r of byParent.get(parent) ?? []) {
      const cc = readCostCenter(r);
      out.push({ record: r, cc, depth });
      walk(cc.code, depth + 1);
    }
  };
  walk("", 0);
  return out;
}
