/**
 * Typed client for the admin-only consolidation + FX-revaluation
 * endpoints that the shared `@kapp/client` SDK does not yet wrap
 * (`/admin/consolidation/groups/{id}/statements` and
 * `/admin/finance/fx-revaluation/run`). It also models the richer
 * request/response fields the backend already returns — CTA,
 * residual, per-entity contributions, per-account elimination
 * accounts — which the current SDK types omit.
 *
 * It deliberately mirrors the `jsonFetch` pattern used elsewhere in
 * this app (see `RoleManagementPage.tsx`): tenant + bearer headers
 * are read from localStorage, and a non-2xx response throws an Error
 * carrying the backend's plain-text diagnostic. Keeping this in
 * `apps/web` (rather than editing the shared SDK) scopes the change
 * to the frontend, as required for this work.
 */

const API_BASE = "/api/v1";

function authHeaders(): Record<string, string> {
  const tenant = localStorage.getItem("kapp.tenant") ?? "";
  const token = localStorage.getItem("kapp.token");
  const headers: Record<string, string> = {
    "Content-Type": "application/json",
    "X-Tenant-ID": tenant,
  };
  if (token) headers.Authorization = `Bearer ${token}`;
  return headers;
}

async function jsonFetch<T>(path: string, init?: RequestInit): Promise<T> {
  const res = await fetch(`${API_BASE}${path}`, {
    ...init,
    headers: { ...authHeaders(), ...(init?.headers ?? {}) },
  });
  if (!res.ok) {
    const text = await res.text().catch(() => "");
    throw new Error(text || `${res.status} ${res.statusText}`);
  }
  if (res.status === 204) return undefined as T;
  return (await res.json()) as T;
}

// --- Types (mirror internal/ledger JSON shapes) -----------------------

export interface EliminationPair {
  from_tenant: string;
  to_tenant: string;
  account_code: string;
  from_account?: string;
  to_account?: string;
}

export interface ConsolidationGroup {
  id: string;
  name: string;
  presentation_currency: string;
  member_tenant_ids: string[];
  elimination_pairs?: EliminationPair[];
  cta_account_code?: string;
  created_at?: string;
  updated_at?: string;
}

export interface TenantBalanceRow {
  tenant_id: string;
  debit: string;
  credit: string;
}

export interface ConsolidatedRow {
  account_code: string;
  account_name?: string;
  type?: string;
  debit: string;
  credit: string;
  balance: string;
  contributions?: TenantBalanceRow[];
}

export interface ConsolidatedTrialBalance {
  group_id: string;
  as_of: string;
  presentation_currency: string;
  rows: ConsolidatedRow[];
  eliminated: ConsolidatedRow[];
  total_debit: string;
  total_credit: string;
  residual?: string;
  cta?: string;
}

export interface ConsolidatedStatementRow {
  account_code: string;
  account_name?: string;
  amount: string;
}

export interface ConsolidatedIncomeStatement {
  group_id: string;
  as_of: string;
  presentation_currency: string;
  revenue: ConsolidatedStatementRow[];
  expense: ConsolidatedStatementRow[];
  total_revenue: string;
  total_expense: string;
  net_income: string;
}

export interface ConsolidatedBalanceSheet {
  group_id: string;
  as_of: string;
  presentation_currency: string;
  assets: ConsolidatedStatementRow[];
  liabilities: ConsolidatedStatementRow[];
  equity: ConsolidatedStatementRow[];
  total_assets: string;
  total_liabilities: string;
  total_equity: string;
  net_income: string;
  difference: string;
  balanced: boolean;
}

export interface ConsolidatedStatements {
  trial_balance: ConsolidatedTrialBalance;
  income_statement: ConsolidatedIncomeStatement;
  balance_sheet: ConsolidatedBalanceSheet;
}

export interface RevaluationLine {
  account_code: string;
  currency: string;
  base_currency: string;
  foreign_net: string;
  current_rate: string;
  recorded_base: string;
  revalued_base: string;
  delta: string;
  gain_loss_account: string;
  entry_id: string;
}

export interface RevaluationSkip {
  account_code: string;
  currency: string;
  base_currency: string;
  foreign_net: string;
  reason: string;
}

export interface RevaluationResult {
  tenant_id: string;
  as_of: string;
  lines: RevaluationLine[];
  skipped: RevaluationSkip[];
  total_gain: string;
  total_loss: string;
  net: string;
}

export interface CreateConsolidationGroupInput {
  name: string;
  presentation_currency: string;
  member_tenant_ids: string[];
  elimination_pairs?: EliminationPair[];
  cta_account_code?: string;
}

export interface ConsolidationStatementsInput {
  as_of?: string;
  /** Period-average rates keyed by member base currency. */
  average_rates?: Record<string, string>;
}

export interface FxRevaluationInput {
  tenant_id: string;
  as_of?: string;
  gain_account?: string;
  loss_account?: string;
  account_allow_list?: string[];
}

// --- Endpoints --------------------------------------------------------

export const consolidationApi = {
  createGroup(input: CreateConsolidationGroupInput): Promise<ConsolidationGroup> {
    return jsonFetch("/admin/consolidation/groups", {
      method: "POST",
      headers: { "Idempotency-Key": crypto.randomUUID() },
      body: JSON.stringify(input),
    });
  },

  runConsolidation(groupId: string, asOf?: string): Promise<ConsolidatedTrialBalance> {
    return jsonFetch(
      `/admin/consolidation/groups/${encodeURIComponent(groupId)}/run`,
      {
        method: "POST",
        body: JSON.stringify(asOf ? { as_of: asOf } : {}),
      },
    );
  },

  runStatements(
    groupId: string,
    input?: ConsolidationStatementsInput,
  ): Promise<ConsolidatedStatements> {
    return jsonFetch(
      `/admin/consolidation/groups/${encodeURIComponent(groupId)}/statements`,
      {
        method: "POST",
        body: JSON.stringify(input ?? {}),
      },
    );
  },

  runFxRevaluation(input: FxRevaluationInput): Promise<RevaluationResult> {
    return jsonFetch("/admin/finance/fx-revaluation/run", {
      method: "POST",
      body: JSON.stringify(input),
    });
  },
};
