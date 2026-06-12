import { useEffect, useMemo, useState } from "react";
import { useMutation } from "@tanstack/react-query";
import {
  Badge,
  Button,
  Card,
  CardContent,
  CardHeader,
  CardTitle,
  EmptyState,
  Input,
  Tabs,
  TabsContent,
  TabsList,
  TabsTrigger,
} from "@kapp/ui";
import {
  consolidationApi,
  type ConsolidatedStatements,
  type ConsolidatedTrialBalance,
  type ConsolidationGroup,
} from "../components/ConsolidationApi";
import { ConsolidationGroupsPanel } from "../components/ConsolidationGroupsPanel";
import { ConsolidationStatements } from "../components/ConsolidationStatements";
import { ConsolidationTrialBalance } from "../components/ConsolidationTrialBalance";
import { FxReviewPanel } from "../components/FxReviewPanel";
import { ct } from "../components/ConsolidationStrings";

// Per-browser registry of groups the operator created or tracked,
// compensating for the absent list-groups endpoint on the backend.
const STORAGE_KEY = "kapp.consolidation.groups";

function loadGroups(): ConsolidationGroup[] {
  try {
    const raw = localStorage.getItem(STORAGE_KEY);
    if (!raw) return [];
    const parsed = JSON.parse(raw) as unknown;
    return Array.isArray(parsed) ? (parsed as ConsolidationGroup[]) : [];
  } catch {
    return [];
  }
}

interface RateDraft {
  currency: string;
  rate: string;
}

/**
 * ConsolidationPage is the admin-only multi-entity consolidation + FX
 * review console. It manages consolidation groups, triggers a
 * consolidation run and statement pack, renders the consolidated trial
 * balance (per-entity columns, drill-down, CTA, eliminations) and the
 * derived statements, and exposes an FX review surface for current-rate
 * translation and pre-posting unrealized FX review.
 */
export function ConsolidationPage() {
  const [groups, setGroups] = useState<ConsolidationGroup[]>(() => loadGroups());
  const [activeGroupId, setActiveGroupId] = useState<string | null>(
    () => loadGroups()[0]?.id ?? null,
  );
  const [asOf, setAsOf] = useState("");
  const [statementsAsOf, setStatementsAsOf] = useState("");
  const [rates, setRates] = useState<RateDraft[]>([]);

  useEffect(() => {
    try {
      localStorage.setItem(STORAGE_KEY, JSON.stringify(groups));
    } catch {
      // Storage may be unavailable (private mode); the registry just
      // won't persist across reloads in that case.
    }
  }, [groups]);

  const activeGroup = useMemo(
    () => groups.find((g) => g.id === activeGroupId) ?? null,
    [groups, activeGroupId],
  );

  const runMut = useMutation<ConsolidatedTrialBalance>({
    mutationFn: () =>
      consolidationApi.runConsolidation(
        activeGroupId ?? "",
        asOf ? new Date(asOf).toISOString() : undefined,
      ),
  });

  const statementsMut = useMutation<ConsolidatedStatements>({
    mutationFn: () => {
      const average_rates: Record<string, string> = {};
      for (const r of rates) {
        if (r.currency.trim() && r.rate.trim()) {
          average_rates[r.currency.trim().toUpperCase()] = r.rate.trim();
        }
      }
      return consolidationApi.runStatements(activeGroupId ?? "", {
        ...(statementsAsOf
          ? { as_of: new Date(statementsAsOf).toISOString() }
          : {}),
        ...(Object.keys(average_rates).length ? { average_rates } : {}),
      });
    },
  });

  const upsertGroup = (g: ConsolidationGroup) =>
    setGroups((prev) => {
      const without = prev.filter((x) => x.id !== g.id);
      return [g, ...without];
    });

  const handleCreated = (g: ConsolidationGroup) => {
    upsertGroup(g);
    setActiveGroupId(g.id);
  };

  const handleTrack = (id: string) => {
    if (!groups.some((g) => g.id === id)) {
      upsertGroup({
        id,
        name: "",
        presentation_currency: "",
        member_tenant_ids: [],
      });
    }
    setActiveGroupId(id);
  };

  const handleForget = (id: string) => {
    setGroups((prev) => prev.filter((g) => g.id !== id));
    setActiveGroupId((cur) => (cur === id ? null : cur));
  };

  const noGroup = !activeGroupId;

  return (
    <section className="grid gap-4">
      <header className="grid gap-1">
        <h1 className="text-xl font-semibold">{ct("consolidation.title")}</h1>
        <p className="max-w-3xl text-sm text-fg-muted">
          {ct("consolidation.subtitle")}
        </p>
        {activeGroup ? (
          <p className="text-sm">
            {ct("consolidation.groups.activeGroup")}:{" "}
            <Badge variant="accent">
              {activeGroup.name || activeGroup.id}
            </Badge>{" "}
            {activeGroup.presentation_currency ? (
              <span className="text-fg-muted">
                {activeGroup.presentation_currency}
              </span>
            ) : null}
          </p>
        ) : null}
      </header>

      <Tabs defaultValue="groups">
        <TabsList>
          <TabsTrigger value="groups">
            {ct("consolidation.tab.groups")}
          </TabsTrigger>
          <TabsTrigger value="trial-balance">
            {ct("consolidation.tab.trialBalance")}
          </TabsTrigger>
          <TabsTrigger value="statements">
            {ct("consolidation.tab.statements")}
          </TabsTrigger>
          <TabsTrigger value="fx">{ct("consolidation.tab.fx")}</TabsTrigger>
        </TabsList>

        <TabsContent value="groups" className="pt-4">
          <ConsolidationGroupsPanel
            groups={groups}
            activeGroupId={activeGroupId}
            onSelect={setActiveGroupId}
            onForget={handleForget}
            onCreated={handleCreated}
            onTrack={handleTrack}
          />
        </TabsContent>

        <TabsContent value="trial-balance" className="grid gap-4 pt-4">
          <Card>
            <CardHeader>
              <CardTitle>{ct("consolidation.run.heading")}</CardTitle>
            </CardHeader>
            <CardContent>
              <form
                className="flex flex-wrap items-end gap-2"
                onSubmit={(e) => {
                  e.preventDefault();
                  runMut.mutate();
                }}
              >
                <label className="grid gap-1 text-sm">
                  {ct("consolidation.run.asOf")}
                  <Input
                    type="date"
                    value={asOf}
                    onChange={(e) => setAsOf(e.target.value)}
                    className="w-auto"
                  />
                </label>
                <Button type="submit" disabled={noGroup || runMut.isPending}>
                  {runMut.isPending
                    ? ct("consolidation.run.running")
                    : ct("consolidation.run.run")}
                </Button>
                {noGroup ? (
                  <span className="text-sm text-fg-subtle">
                    {ct("consolidation.run.needsGroup")}
                  </span>
                ) : null}
              </form>
              {runMut.error ? (
                <p className="mt-2 text-sm text-danger">
                  {(runMut.error as Error).message}
                </p>
              ) : null}
            </CardContent>
          </Card>

          {runMut.data ? (
            <ConsolidationTrialBalance
              result={runMut.data}
              ctaAccountCode={activeGroup?.cta_account_code}
            />
          ) : (
            <EmptyState
              title={ct("consolidation.tb.heading")}
              description={ct("consolidation.tb.empty")}
            />
          )}
        </TabsContent>

        <TabsContent value="statements" className="grid gap-4 pt-4">
          <Card>
            <CardHeader>
              <CardTitle>{ct("consolidation.run.statements")}</CardTitle>
              <p className="text-sm text-fg-muted">
                {ct("consolidation.run.averageRatesHint")}
              </p>
            </CardHeader>
            <CardContent className="grid gap-3">
              <label className="grid gap-1 text-sm">
                {ct("consolidation.run.asOf")}
                <Input
                  type="date"
                  value={statementsAsOf}
                  onChange={(e) => setStatementsAsOf(e.target.value)}
                  className="w-auto"
                />
              </label>

              <fieldset className="grid gap-2 rounded-md border border-border p-3">
                <legend className="px-1 text-sm font-medium">
                  {ct("consolidation.run.averageRates")}
                </legend>
                {rates.map((r, i) => (
                  <div key={i} className="flex items-center gap-2">
                    <Input
                      aria-label={ct("consolidation.run.currency")}
                      placeholder={ct("consolidation.run.currency")}
                      value={r.currency}
                      maxLength={3}
                      className="w-20"
                      onChange={(e) =>
                        setRates((prev) =>
                          prev.map((x, idx) =>
                            idx === i ? { ...x, currency: e.target.value } : x,
                          ),
                        )
                      }
                    />
                    <Input
                      aria-label={ct("consolidation.run.rate")}
                      placeholder={ct("consolidation.run.rate")}
                      value={r.rate}
                      className="w-28"
                      onChange={(e) =>
                        setRates((prev) =>
                          prev.map((x, idx) =>
                            idx === i ? { ...x, rate: e.target.value } : x,
                          ),
                        )
                      }
                    />
                    <Button
                      type="button"
                      variant="ghost"
                      size="sm"
                      onClick={() =>
                        setRates((prev) => prev.filter((_, idx) => idx !== i))
                      }
                    >
                      {ct("consolidation.groups.removeElimination")}
                    </Button>
                  </div>
                ))}
                <Button
                  type="button"
                  variant="secondary"
                  size="sm"
                  onClick={() =>
                    setRates((prev) => [...prev, { currency: "", rate: "" }])
                  }
                >
                  {ct("consolidation.run.addRate")}
                </Button>
              </fieldset>

              <div className="flex items-center gap-2">
                <Button
                  type="button"
                  disabled={noGroup || statementsMut.isPending}
                  onClick={() => statementsMut.mutate()}
                >
                  {statementsMut.isPending
                    ? ct("consolidation.run.buildingStatements")
                    : ct("consolidation.run.statements")}
                </Button>
                {noGroup ? (
                  <span className="text-sm text-fg-subtle">
                    {ct("consolidation.run.needsGroup")}
                  </span>
                ) : null}
              </div>
              {statementsMut.error ? (
                <p className="text-sm text-danger">
                  {(statementsMut.error as Error).message}
                </p>
              ) : null}
            </CardContent>
          </Card>

          {statementsMut.data ? (
            <ConsolidationStatements statements={statementsMut.data} />
          ) : (
            <EmptyState
              title={ct("consolidation.stmt.heading")}
              description={ct("consolidation.stmt.empty")}
            />
          )}
        </TabsContent>

        <TabsContent value="fx" className="pt-4">
          <FxReviewPanel />
        </TabsContent>
      </Tabs>
    </section>
  );
}
