import { useState } from "react";
import { useMutation } from "@tanstack/react-query";
import {
  Badge,
  Button,
  Card,
  CardContent,
  CardHeader,
  CardTitle,
  Input,
} from "@kapp/ui";
import {
  consolidationApi,
  type ConsolidationGroup,
  type EliminationPair,
} from "./ConsolidationApi";
import { ct, ctp } from "./ConsolidationStrings";

/** Split a free-form members textarea (newlines or commas) into ids. */
function parseMembers(raw: string): string[] {
  return raw
    .split(/[\n,]/)
    .map((s) => s.trim())
    .filter(Boolean);
}

interface PairDraft {
  from_tenant: string;
  to_tenant: string;
  account_code: string;
  from_account: string;
  to_account: string;
}

const emptyPair: PairDraft = {
  from_tenant: "",
  to_tenant: "",
  account_code: "",
  from_account: "",
  to_account: "",
};

export interface ConsolidationGroupsPanelProps {
  groups: ConsolidationGroup[];
  activeGroupId: string | null;
  onSelect: (id: string) => void;
  onForget: (id: string) => void;
  onCreated: (group: ConsolidationGroup) => void;
  onTrack: (id: string) => void;
}

/**
 * Manages consolidation groups: a create form (members, ownership via
 * intercompany elimination pairs, optional CTA account) plus a list of
 * groups this console is tracking. The backend exposes no list-groups
 * endpoint, so the console keeps a per-browser registry (lifted to the
 * page and persisted to localStorage) of the groups you create or add
 * by id.
 */
export function ConsolidationGroupsPanel({
  groups,
  activeGroupId,
  onSelect,
  onForget,
  onCreated,
  onTrack,
}: ConsolidationGroupsPanelProps) {
  const [name, setName] = useState("");
  const [currency, setCurrency] = useState("USD");
  const [members, setMembers] = useState("");
  const [ctaAccount, setCtaAccount] = useState("");
  const [pairs, setPairs] = useState<PairDraft[]>([]);
  const [existingId, setExistingId] = useState("");

  const createMut = useMutation({
    mutationFn: () => {
      const elimination_pairs: EliminationPair[] = pairs
        .filter((p) => p.from_tenant && p.to_tenant && p.account_code)
        .map((p) => ({
          from_tenant: p.from_tenant.trim(),
          to_tenant: p.to_tenant.trim(),
          account_code: p.account_code.trim(),
          ...(p.from_account.trim() ? { from_account: p.from_account.trim() } : {}),
          ...(p.to_account.trim() ? { to_account: p.to_account.trim() } : {}),
        }));
      return consolidationApi.createGroup({
        name: name.trim(),
        presentation_currency: currency.trim().toUpperCase(),
        member_tenant_ids: parseMembers(members),
        ...(elimination_pairs.length ? { elimination_pairs } : {}),
        ...(ctaAccount.trim() ? { cta_account_code: ctaAccount.trim() } : {}),
      });
    },
    onSuccess: (g) => {
      onCreated(g);
      setName("");
      setCurrency("USD");
      setMembers("");
      setCtaAccount("");
      setPairs([]);
    },
  });

  const updatePair = (i: number, patch: Partial<PairDraft>) =>
    setPairs((prev) => prev.map((p, idx) => (idx === i ? { ...p, ...patch } : p)));

  return (
    <div className="grid gap-4 lg:grid-cols-2">
      <Card>
        <CardHeader>
          <CardTitle>{ct("consolidation.groups.create")}</CardTitle>
        </CardHeader>
        <CardContent>
          <form
            className="grid gap-3"
            onSubmit={(e) => {
              e.preventDefault();
              createMut.mutate();
            }}
          >
            <label className="grid gap-1 text-sm">
              {ct("consolidation.groups.name")}
              <Input
                value={name}
                onChange={(e) => setName(e.target.value)}
                required
              />
            </label>
            <label className="grid gap-1 text-sm">
              {ct("consolidation.groups.presentationCurrency")}
              <Input
                value={currency}
                onChange={(e) => setCurrency(e.target.value.toUpperCase())}
                maxLength={3}
                required
                className="w-24"
              />
            </label>
            <label className="grid gap-1 text-sm">
              {ct("consolidation.groups.members")}
              <textarea
                value={members}
                onChange={(e) => setMembers(e.target.value)}
                rows={3}
                className="rounded-md border border-border bg-bg px-3 py-2 text-sm"
              />
            </label>
            <label className="grid gap-1 text-sm">
              {ct("consolidation.groups.ctaAccount")}
              <Input
                value={ctaAccount}
                onChange={(e) => setCtaAccount(e.target.value)}
                placeholder="3900"
                className="w-32"
              />
              <span className="text-xs text-fg-subtle">
                {ct("consolidation.groups.ctaAccountHint")}
              </span>
            </label>

            <fieldset className="grid gap-2 rounded-md border border-border p-3">
              <legend className="px-1 text-sm font-medium">
                {ct("consolidation.groups.eliminations")}
              </legend>
              {pairs.map((p, i) => (
                <div
                  key={i}
                  className="grid grid-cols-1 gap-2 rounded border border-border/60 p-2 sm:grid-cols-2"
                >
                  <Input
                    placeholder={ct("consolidation.groups.fromTenant")}
                    value={p.from_tenant}
                    onChange={(e) => updatePair(i, { from_tenant: e.target.value })}
                  />
                  <Input
                    placeholder={ct("consolidation.groups.toTenant")}
                    value={p.to_tenant}
                    onChange={(e) => updatePair(i, { to_tenant: e.target.value })}
                  />
                  <Input
                    placeholder={ct("consolidation.groups.accountCode")}
                    value={p.account_code}
                    onChange={(e) => updatePair(i, { account_code: e.target.value })}
                  />
                  <div className="flex gap-2">
                    <Input
                      placeholder={ct("consolidation.groups.fromAccount")}
                      value={p.from_account}
                      onChange={(e) =>
                        updatePair(i, { from_account: e.target.value })
                      }
                    />
                    <Input
                      placeholder={ct("consolidation.groups.toAccount")}
                      value={p.to_account}
                      onChange={(e) => updatePair(i, { to_account: e.target.value })}
                    />
                  </div>
                  <div className="sm:col-span-2">
                    <Button
                      type="button"
                      variant="ghost"
                      size="sm"
                      onClick={() =>
                        setPairs((prev) => prev.filter((_, idx) => idx !== i))
                      }
                    >
                      {ct("consolidation.groups.removeElimination")}
                    </Button>
                  </div>
                </div>
              ))}
              <Button
                type="button"
                variant="secondary"
                size="sm"
                onClick={() => setPairs((prev) => [...prev, { ...emptyPair }])}
              >
                {ct("consolidation.groups.addElimination")}
              </Button>
            </fieldset>

            <Button type="submit" disabled={createMut.isPending}>
              {createMut.isPending
                ? ct("consolidation.groups.creating")
                : ct("consolidation.groups.create")}
            </Button>
            {createMut.error ? (
              <p className="text-sm text-danger">
                {(createMut.error as Error).message}
              </p>
            ) : null}
          </form>
        </CardContent>
      </Card>

      <Card>
        <CardHeader>
          <CardTitle>{ct("consolidation.groups.known")}</CardTitle>
          <p className="text-xs text-fg-subtle">
            {ct("consolidation.groups.listEndpointNote")}
          </p>
        </CardHeader>
        <CardContent className="grid gap-3">
          {groups.length === 0 ? (
            <p className="text-sm italic text-fg-subtle">
              {ct("consolidation.groups.empty")}
            </p>
          ) : (
            <ul className="grid gap-2">
              {groups.map((g) => {
                const active = g.id === activeGroupId;
                return (
                  <li
                    key={g.id}
                    className={`flex items-center justify-between gap-2 rounded-md border p-2 text-sm ${
                      active ? "border-accent bg-accent/5" : "border-border"
                    }`}
                  >
                    <div className="min-w-0">
                      <div className="flex items-center gap-2">
                        <span className="truncate font-medium">{g.name || g.id}</span>
                        {active ? (
                          <Badge variant="accent" size="xs">
                            {ct("consolidation.groups.selected")}
                          </Badge>
                        ) : null}
                      </div>
                      <div className="text-xs text-fg-subtle">
                        <code title={g.id}>{g.id}</code> ·{" "}
                        {g.presentation_currency} ·{" "}
                        {ctp("consolidation.groups.membersCount", {
                          count: g.member_tenant_ids?.length ?? 0,
                        })}
                      </div>
                    </div>
                    <div className="flex shrink-0 gap-1">
                      <Button
                        type="button"
                        size="sm"
                        variant={active ? "secondary" : "primary"}
                        onClick={() => onSelect(g.id)}
                      >
                        {ct("consolidation.groups.select")}
                      </Button>
                      <Button
                        type="button"
                        size="sm"
                        variant="ghost"
                        onClick={() => onForget(g.id)}
                      >
                        {ct("consolidation.groups.forget")}
                      </Button>
                    </div>
                  </li>
                );
              })}
            </ul>
          )}

          <form
            className="flex items-end gap-2"
            onSubmit={(e) => {
              e.preventDefault();
              const id = existingId.trim();
              if (!id) return;
              onTrack(id);
              setExistingId("");
            }}
          >
            <label className="grid flex-1 gap-1 text-sm">
              {ct("consolidation.groups.addExisting")}
              <Input
                value={existingId}
                onChange={(e) => setExistingId(e.target.value)}
                placeholder="group id (uuid)"
              />
            </label>
            <Button type="submit" variant="secondary" disabled={!existingId.trim()}>
              {ct("consolidation.groups.add")}
            </Button>
          </form>
        </CardContent>
      </Card>
    </div>
  );
}
