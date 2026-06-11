import { useMemo, useRef, useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import type {
  BankFeedConnection,
  BankFeedRule,
  BankFeedRuleInput,
  BankFeedSuggestion,
  KRecord,
} from "@kapp/client";
import {
  Badge,
  Button,
  Input,
  Select,
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
  cn,
  toast,
} from "@kapp/ui";
import { api } from "../lib/api";

const KTYPE_ACCOUNT = "finance.bank_account";

interface BankAccountData {
  name?: string;
  currency?: string;
  account_number?: string;
}

// CONNECTION_BADGE maps a connection's lifecycle status to a Badge
// variant so the table communicates feed health at a glance.
const CONNECTION_BADGE: Record<
  string,
  "default" | "accent" | "outline" | "success" | "warning" | "danger"
> = {
  // Keys mirror the server's StatusActive/StatusExpired/StatusRevoked
  // constants — the only statuses bank_feed_connections ever holds.
  active: "success",
  expired: "warning",
  revoked: "default",
};

// RULE_CONDITION_TYPES are the matchers bank_reconciliation_rules.Validate
// accepts; surfacing them as a closed list keeps the create form aligned
// with the server-side validation rather than free-texting a type.
const RULE_CONDITION_TYPES: Array<{ value: string; label: string }> = [
  { value: "description_contains", label: "Description contains" },
  { value: "description_regex", label: "Description matches regex" },
  { value: "counterparty_equals", label: "Counterparty equals" },
  { value: "amount_equals", label: "Amount equals" },
];

/**
 * BankFeedsPage is the operator console for the Session 15 bank-feed +
 * smart-reconciliation module. The left rail lists the tenant's bank
 * accounts (finance.bank_account KRecords); selecting one drives the
 * per-account surfaces on the right:
 *
 *   - Connections: live provider feeds (Plaid / GoCardless / CSV) with
 *     connect, sync-now and disconnect actions. Only providers the
 *     registry was built with are offered (fail-closed; CSV always
 *     present).
 *   - CSV upload: pushes a raw statement through the same ingest →
 *     categorize → match pipeline as a live feed (idempotent).
 *   - Suggestions: the match-review inbox (accept reconciles against the
 *     suggested journal entry; reject dismisses the candidate).
 *
 * Auto-categorization rules are tenant-wide (optionally account-scoped),
 * so they render below the account-specific panels.
 */
export function BankFeedsPage() {
  const accountsQ = useQuery<KRecord[]>({
    queryKey: ["records", KTYPE_ACCOUNT],
    queryFn: () => api.listRecords(KTYPE_ACCOUNT),
  });
  const [selected, setSelected] = useState<string | null>(null);

  const accounts = accountsQ.data ?? [];
  const selectedAccount = useMemo(
    () => accounts.find((a) => a.id === selected) ?? null,
    [accounts, selected],
  );
  const defaultCurrency =
    (selectedAccount?.data as BankAccountData | undefined)?.currency ?? "";

  return (
    <section className="flex flex-col gap-4">
      <div>
        <h1 className="text-2xl font-semibold tracking-tight text-fg">
          Bank Feeds
        </h1>
        <p className="mt-1 text-sm text-fg-muted">
          Connect a bank feed or upload a statement, then review the
          auto-matched suggestions and reconciliation rules.
        </p>
      </div>

      <div className="flex gap-4">
        <div className="flex-[0_0_240px]">
          <h2 className="text-sm font-semibold text-fg">Accounts</h2>
          {accountsQ.isLoading && (
            <p className="mt-2 text-sm text-fg-muted">Loading…</p>
          )}
          {!accountsQ.isLoading && accounts.length === 0 && (
            <p className="mt-2 text-sm text-fg-muted">
              No bank accounts yet. Create a finance.bank_account record
              first.
            </p>
          )}
          <ul className="mt-2 flex list-none flex-col gap-1 p-0">
            {accounts.map((r) => {
              const d = r.data as BankAccountData;
              const active = selected === r.id;
              return (
                <li key={r.id}>
                  <button
                    type="button"
                    onClick={() => setSelected(r.id)}
                    className={cn(
                      "w-full rounded-md px-2 py-1.5 text-left text-sm transition-colors hover:bg-bg-subtle focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-(--focus-ring)",
                      active && "bg-accent/10",
                    )}
                  >
                    <div className="font-medium text-fg">
                      {d.name ?? "(unnamed)"}
                    </div>
                    <div className="text-xs text-fg-muted">
                      {d.currency ?? ""} {d.account_number ?? ""}
                    </div>
                  </button>
                </li>
              );
            })}
          </ul>
        </div>

        <div className="min-w-0 flex-1">
          {!selected ? (
            <p className="text-sm text-fg-muted">Select a bank account.</p>
          ) : (
            <div className="flex flex-col gap-6">
              <ConnectionsPanel
                bankAccountId={selected}
                defaultCurrency={defaultCurrency}
              />
              <SuggestionsPanel bankAccountId={selected} />
            </div>
          )}
        </div>
      </div>

      <RulesPanel />
    </section>
  );
}

// ---------------------------------------------------------------------------
// Connections + CSV upload
// ---------------------------------------------------------------------------

function ConnectionsPanel({
  bankAccountId,
  defaultCurrency,
}: {
  bankAccountId: string;
  defaultCurrency: string;
}) {
  const qc = useQueryClient();
  const providersQ = useQuery({
    queryKey: ["bankfeed", "providers"],
    queryFn: () => api.listBankFeedProviders(),
  });
  const connectionsQ = useQuery({
    queryKey: ["bankfeed", "connections", bankAccountId],
    queryFn: () => api.listBankFeedConnections(bankAccountId),
  });

  const providers = providersQ.data?.providers ?? [];
  const [provider, setProvider] = useState("");
  const effectiveProvider = provider || providers[0] || "";

  // Invalidation helpers take the account id explicitly so callbacks key
  // off the account the mutation actually ran against (captured in the
  // mutation variables at call time) rather than whatever bankAccountId
  // the component happens to be rendering when the async callback fires —
  // otherwise switching accounts mid-flight would refresh the wrong
  // account's queries.
  const invalidateConnections = (accountId: string) =>
    qc.invalidateQueries({
      queryKey: ["bankfeed", "connections", accountId],
    });
  const invalidateSuggestions = (accountId: string) =>
    qc.invalidateQueries({
      queryKey: ["bankfeed", "suggestions", accountId],
    });

  const connectMut = useMutation({
    mutationFn: async ({
      provider: p,
      accountId,
    }: {
      provider: string;
      accountId: string;
    }) => {
      // CSV is push-based: there is no provider handshake, so we create
      // the credential-less connection row directly. Hosted providers
      // need their link handshake first; we surface the returned link so
      // the operator can complete consent in the provider widget.
      if (p === "csv") {
        await api.completeBankFeedConnect({
          provider: p,
          bank_account_id: accountId,
        });
        return { kind: "csv" as const };
      }
      const link = await api.initiateBankFeedConnect({
        provider: p,
        bank_account_id: accountId,
      });
      return { kind: "link" as const, link: link.link };
    },
    onSuccess: (res, { accountId }) => {
      if (res.kind === "csv") {
        toast.success("CSV feed connected");
        invalidateConnections(accountId);
        return;
      }
      if (res.link) {
        window.open(res.link, "_blank", "noopener,noreferrer");
        toast.success("Provider link opened", {
          description: "Complete consent in the provider window.",
        });
      } else {
        toast.success("Connection initiated");
      }
    },
    onError: (e) =>
      toast.error("Connect failed", { description: (e as Error).message }),
  });

  const syncMut = useMutation({
    mutationFn: ({ id }: { id: string; accountId: string }) =>
      api.syncBankFeedConnection(id),
    onSuccess: (res, { accountId }) => {
      toast.success("Sync complete", {
        description: `${res.fetched} fetched · ${res.inserted} new · ${res.suggested} suggested · ${res.auto_matched} auto-matched`,
      });
      invalidateConnections(accountId);
      invalidateSuggestions(accountId);
    },
    onError: (e) =>
      toast.error("Sync failed", { description: (e as Error).message }),
  });

  const disconnectMut = useMutation({
    mutationFn: ({ id }: { id: string; accountId: string }) =>
      api.disconnectBankFeed(id),
    onSuccess: (res, { accountId }) => {
      if (res.provider_warning) {
        toast.warning("Disconnected with warning", {
          description: res.provider_warning,
        });
      } else {
        toast.success("Disconnected");
      }
      invalidateConnections(accountId);
    },
    onError: (e) =>
      toast.error("Disconnect failed", { description: (e as Error).message }),
  });

  const connections = connectionsQ.data ?? [];

  return (
    <div className="flex flex-col gap-3">
      <header className="flex flex-wrap items-center justify-between gap-2">
        <h2 className="text-base font-semibold text-fg">Connections</h2>
        <div className="flex items-center gap-2">
          <Select
            aria-label="Provider"
            value={effectiveProvider}
            onChange={(e) => setProvider(e.target.value)}
            className="w-auto"
            disabled={providers.length === 0}
          >
            {providers.map((p) => (
              <option key={p} value={p}>
                {p}
              </option>
            ))}
          </Select>
          <Button
            size="sm"
            disabled={!effectiveProvider || connectMut.isPending}
            onClick={() =>
              connectMut.mutate({
                provider: effectiveProvider,
                accountId: bankAccountId,
              })
            }
          >
            Connect
          </Button>
        </div>
      </header>

      <CSVUploader
        bankAccountId={bankAccountId}
        defaultCurrency={defaultCurrency}
        onUploaded={(accountId) => {
          invalidateConnections(accountId);
          invalidateSuggestions(accountId);
        }}
      />

      {connectionsQ.isLoading && (
        <p className="text-sm text-fg-muted">Loading…</p>
      )}
      {connectionsQ.isError && (
        <p className="text-sm text-danger">{String(connectionsQ.error)}</p>
      )}
      {!connectionsQ.isLoading &&
        (connections.length === 0 ? (
          <p className="text-sm text-fg-muted">
            No connections for this account.
          </p>
        ) : (
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>Provider</TableHead>
                <TableHead>Status</TableHead>
                <TableHead>Last sync</TableHead>
                <TableHead className="text-end">Actions</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {connections.map((c: BankFeedConnection) => (
                <TableRow key={c.id}>
                  <TableCell className="font-medium">{c.provider}</TableCell>
                  <TableCell>
                    <Badge variant={CONNECTION_BADGE[c.status] ?? "default"}>
                      {c.status}
                    </Badge>
                    {c.last_error && (
                      <span className="ml-2 text-xs text-danger">
                        {c.last_error}
                      </span>
                    )}
                  </TableCell>
                  <TableCell>
                    {c.last_sync_at
                      ? new Date(c.last_sync_at).toLocaleString()
                      : "never"}
                  </TableCell>
                  <TableCell className="text-end">
                    <div className="flex justify-end gap-1">
                      <Button
                        size="sm"
                        variant="outline"
                        disabled={syncMut.isPending || c.status === "revoked"}
                        onClick={() =>
                          syncMut.mutate({ id: c.id, accountId: bankAccountId })
                        }
                      >
                        Sync now
                      </Button>
                      <Button
                        size="sm"
                        variant="ghost"
                        disabled={
                          disconnectMut.isPending || c.status === "revoked"
                        }
                        onClick={() =>
                          disconnectMut.mutate({
                            id: c.id,
                            accountId: bankAccountId,
                          })
                        }
                      >
                        Disconnect
                      </Button>
                    </div>
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        ))}
    </div>
  );
}

function CSVUploader({
  bankAccountId,
  defaultCurrency,
  onUploaded,
}: {
  bankAccountId: string;
  defaultCurrency: string;
  onUploaded: (accountId: string) => void;
}) {
  const [busy, setBusy] = useState(false);
  const inputRef = useRef<HTMLInputElement>(null);

  const handleFile = async (file: File) => {
    setBusy(true);
    try {
      const text = await file.text();
      const res = await api.uploadBankFeedCSV(
        bankAccountId,
        text,
        defaultCurrency || undefined,
      );
      toast.success("Statement uploaded", {
        description: `${res.fetched} rows · ${res.inserted} new · ${res.suggested} suggested · ${res.auto_matched} auto-matched`,
      });
      // Pass the account we uploaded to (captured in this closure) so the
      // parent refreshes the correct account even if the selection moved
      // on during the upload.
      onUploaded(bankAccountId);
    } catch (e) {
      toast.error("Upload failed", { description: (e as Error).message });
    } finally {
      setBusy(false);
      if (inputRef.current) inputRef.current.value = "";
    }
  };

  return (
    <div className="flex items-center gap-2">
      <span className="text-sm text-fg-muted">Upload CSV statement:</span>
      <input
        ref={inputRef}
        type="file"
        accept=".csv,text/csv"
        disabled={busy}
        onChange={(e) => {
          const f = e.target.files?.[0];
          if (f) handleFile(f);
        }}
        className="text-sm text-fg-muted file:mr-2 file:rounded-md file:border file:border-border file:bg-bg-subtle file:px-3 file:py-1.5 file:text-sm file:font-medium file:text-fg hover:file:bg-bg-muted"
      />
      {busy && <span className="text-xs text-fg-muted">Uploading…</span>}
    </div>
  );
}

// ---------------------------------------------------------------------------
// Match suggestions
// ---------------------------------------------------------------------------

function SuggestionsPanel({ bankAccountId }: { bankAccountId: string }) {
  const qc = useQueryClient();
  const suggestionsQ = useQuery({
    queryKey: ["bankfeed", "suggestions", bankAccountId],
    queryFn: () => api.listBankFeedSuggestions(bankAccountId),
  });

  // Keyed off the account captured in the mutation variables at click
  // time so an in-flight accept/reject refreshes the right account even
  // if the operator switches accounts before it resolves.
  const invalidateSuggestions = (accountId: string) =>
    qc.invalidateQueries({
      queryKey: ["bankfeed", "suggestions", accountId],
    });

  const acceptMut = useMutation({
    mutationFn: ({ id }: { id: string; accountId: string }) =>
      api.acceptBankFeedSuggestion(id),
    onSuccess: (_res, { accountId }) => {
      toast.success("Suggestion accepted");
      invalidateSuggestions(accountId);
    },
    onError: (e) =>
      toast.error("Accept failed", { description: (e as Error).message }),
  });

  const rejectMut = useMutation({
    mutationFn: ({ id }: { id: string; accountId: string }) =>
      api.rejectBankFeedSuggestion(id),
    onSuccess: (_res, { accountId }) => {
      toast.success("Suggestion rejected");
      invalidateSuggestions(accountId);
    },
    onError: (e) =>
      toast.error("Reject failed", { description: (e as Error).message }),
  });

  const suggestions = suggestionsQ.data ?? [];

  return (
    <div className="flex flex-col gap-3">
      <h2 className="text-base font-semibold text-fg">Match suggestions</h2>
      {suggestionsQ.isLoading && (
        <p className="text-sm text-fg-muted">Loading…</p>
      )}
      {suggestionsQ.isError && (
        <p className="text-sm text-danger">{String(suggestionsQ.error)}</p>
      )}
      {!suggestionsQ.isLoading &&
        (suggestions.length === 0 ? (
          <p className="text-sm text-fg-muted">No open suggestions.</p>
        ) : (
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>Transaction</TableHead>
                <TableHead>Journal entry</TableHead>
                <TableHead className="text-right">Confidence</TableHead>
                <TableHead>Reason</TableHead>
                <TableHead className="text-end">Actions</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {suggestions.map((s: BankFeedSuggestion) => (
                <TableRow key={s.id}>
                  <TableCell className="font-mono text-xs">
                    {s.transaction_id}
                  </TableCell>
                  <TableCell className="font-mono text-xs">
                    {s.journal_entry_id}
                  </TableCell>
                  <TableCell className="text-right">
                    {(s.confidence * 100).toFixed(0)}%
                  </TableCell>
                  <TableCell>{s.match_reason}</TableCell>
                  <TableCell className="text-end">
                    <div className="flex justify-end gap-1">
                      <Button
                        size="sm"
                        disabled={acceptMut.isPending}
                        onClick={() =>
                          acceptMut.mutate({ id: s.id, accountId: bankAccountId })
                        }
                      >
                        Accept
                      </Button>
                      <Button
                        size="sm"
                        variant="ghost"
                        disabled={rejectMut.isPending}
                        onClick={() =>
                          rejectMut.mutate({ id: s.id, accountId: bankAccountId })
                        }
                      >
                        Reject
                      </Button>
                    </div>
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        ))}
    </div>
  );
}

// ---------------------------------------------------------------------------
// Auto-categorization rules (tenant-wide)
// ---------------------------------------------------------------------------

const EMPTY_RULE: BankFeedRuleInput = {
  priority: 100,
  condition_type: RULE_CONDITION_TYPES[0].value,
  condition_value: "",
  target_account_code: "",
  target_cost_center: "",
  auto_approve: false,
  enabled: true,
};

function RulesPanel() {
  const qc = useQueryClient();
  const rulesQ = useQuery({
    queryKey: ["bankfeed", "rules"],
    queryFn: () => api.listBankFeedRules(),
  });
  const [draft, setDraft] = useState<BankFeedRuleInput>(EMPTY_RULE);

  const invalidate = () =>
    qc.invalidateQueries({ queryKey: ["bankfeed", "rules"] });

  const createMut = useMutation({
    mutationFn: (input: BankFeedRuleInput) => api.createBankFeedRule(input),
    onSuccess: () => {
      toast.success("Rule created");
      setDraft(EMPTY_RULE);
      invalidate();
    },
    onError: (e) =>
      toast.error("Create failed", { description: (e as Error).message }),
  });

  const deleteMut = useMutation({
    mutationFn: (id: string) => api.deleteBankFeedRule(id),
    onSuccess: () => {
      toast.success("Rule deleted");
      invalidate();
    },
    onError: (e) =>
      toast.error("Delete failed", { description: (e as Error).message }),
  });

  const toggleMut = useMutation({
    mutationFn: (rule: BankFeedRule) =>
      api.updateBankFeedRule(rule.id, {
        priority: rule.priority,
        condition_type: rule.condition_type,
        condition_value: rule.condition_value,
        target_account_code: rule.target_account_code,
        target_cost_center: rule.target_cost_center,
        auto_approve: rule.auto_approve,
        bank_account_id: rule.bank_account_id,
        enabled: !rule.enabled,
      }),
    onSuccess: () => invalidate(),
    onError: (e) =>
      toast.error("Update failed", { description: (e as Error).message }),
  });

  const rules = rulesQ.data ?? [];

  const submit = (e: React.FormEvent) => {
    e.preventDefault();
    if (!draft.condition_value.trim()) {
      toast.error("Condition value is required");
      return;
    }
    if (!draft.target_account_code?.trim()) {
      toast.error("Target account code is required");
      return;
    }
    createMut.mutate(draft);
  };

  return (
    <div className="flex flex-col gap-3">
      <h2 className="text-base font-semibold text-fg">
        Auto-categorization rules
      </h2>
      <p className="text-sm text-fg-muted">
        Rules are evaluated by ascending priority; the first match assigns the
        GL account (and optionally auto-approves the posting).
      </p>

      <form
        onSubmit={submit}
        className="flex flex-wrap items-end gap-2 rounded-md border border-border bg-bg-subtle p-3"
      >
        <label className="flex flex-col gap-1 text-xs text-fg-muted">
          Priority
          <Input
            type="number"
            value={String(draft.priority)}
            onChange={(e) =>
              setDraft({ ...draft, priority: Number(e.target.value) })
            }
            className="w-24"
          />
        </label>
        <label className="flex flex-col gap-1 text-xs text-fg-muted">
          Condition
          <Select
            value={draft.condition_type}
            onChange={(e) =>
              setDraft({ ...draft, condition_type: e.target.value })
            }
            className="w-auto"
          >
            {RULE_CONDITION_TYPES.map((c) => (
              <option key={c.value} value={c.value}>
                {c.label}
              </option>
            ))}
          </Select>
        </label>
        <label className="flex flex-col gap-1 text-xs text-fg-muted">
          Value
          <Input
            value={draft.condition_value}
            onChange={(e) =>
              setDraft({ ...draft, condition_value: e.target.value })
            }
            placeholder="e.g. STRIPE"
          />
        </label>
        <label className="flex flex-col gap-1 text-xs text-fg-muted">
          Account code
          <Input
            value={draft.target_account_code ?? ""}
            onChange={(e) =>
              setDraft({ ...draft, target_account_code: e.target.value })
            }
            placeholder="e.g. 4000"
            className="w-28"
          />
        </label>
        <label className="flex flex-col gap-1 text-xs text-fg-muted">
          Cost center
          <Input
            value={draft.target_cost_center ?? ""}
            onChange={(e) =>
              setDraft({ ...draft, target_cost_center: e.target.value })
            }
            className="w-28"
          />
        </label>
        <label className="flex items-center gap-1 text-xs text-fg-muted">
          <input
            type="checkbox"
            checked={draft.auto_approve}
            onChange={(e) =>
              setDraft({ ...draft, auto_approve: e.target.checked })
            }
          />
          Auto-approve
        </label>
        <Button type="submit" size="sm" disabled={createMut.isPending}>
          Add rule
        </Button>
      </form>

      {rulesQ.isLoading && <p className="text-sm text-fg-muted">Loading…</p>}
      {!rulesQ.isLoading &&
        (rules.length === 0 ? (
          <p className="text-sm text-fg-muted">No rules configured.</p>
        ) : (
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead className="text-right">Priority</TableHead>
                <TableHead>Condition</TableHead>
                <TableHead>Value</TableHead>
                <TableHead>Account</TableHead>
                <TableHead>Auto-approve</TableHead>
                <TableHead>Enabled</TableHead>
                <TableHead className="text-end">Actions</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {rules.map((rule: BankFeedRule) => (
                <TableRow key={rule.id}>
                  <TableCell className="text-right">{rule.priority}</TableCell>
                  <TableCell>{rule.condition_type}</TableCell>
                  <TableCell className="font-mono text-xs">
                    {rule.condition_value}
                  </TableCell>
                  <TableCell>
                    {rule.target_account_code || "—"}
                    {rule.target_cost_center
                      ? ` / ${rule.target_cost_center}`
                      : ""}
                  </TableCell>
                  <TableCell>{rule.auto_approve ? "Yes" : "No"}</TableCell>
                  <TableCell>
                    <Badge variant={rule.enabled ? "success" : "outline"}>
                      {rule.enabled ? "enabled" : "disabled"}
                    </Badge>
                  </TableCell>
                  <TableCell className="text-end">
                    <div className="flex justify-end gap-1">
                      <Button
                        size="sm"
                        variant="outline"
                        disabled={toggleMut.isPending}
                        onClick={() => toggleMut.mutate(rule)}
                      >
                        {rule.enabled ? "Disable" : "Enable"}
                      </Button>
                      <Button
                        size="sm"
                        variant="ghost"
                        disabled={deleteMut.isPending}
                        onClick={() => deleteMut.mutate(rule.id)}
                      >
                        Delete
                      </Button>
                    </div>
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        ))}
    </div>
  );
}

export default BankFeedsPage;
