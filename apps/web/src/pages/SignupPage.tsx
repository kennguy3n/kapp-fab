import { useEffect, useMemo, useState } from "react";
import { useNavigate, useSearchParams } from "react-router-dom";

// SignupPage drives Workstream 1 self-service tenant creation. The
// flow is:
//
//   1. The visitor picks a plan and enters a company name.
//   2. They authenticate with KChat ("Sign in with KChat"), which
//      redirects back to /signup?code=<auth-code>. The plan + company
//      choice is stashed in sessionStorage so it survives the
//      round-trip.
//   3. We POST the code + plan + company to POST /api/v1/signup. The
//      backend verifies the KChat identity, creates the tenant, and
//      runs the provisioning wizard (CoA / roles / features / ZK
//      fabric) in one call (tenant.SignupService.Signup ->
//      Wizard.AutoProvision).
//
// Signup never mints a Kapp session itself (it does not weaken the
// fail-closed JWT posture): once the tenant exists the visitor signs
// in through the normal /login SSO flow, which is where the JWT — now
// carrying their resolvable tenant membership — is issued.

// The sessionStorage key under which the in-progress signup form is
// stashed across the KChat redirect.
const DRAFT_KEY = "kapp.signup.draft";

interface PlanOption {
  name: string;
  display_name: string;
  trial_days?: number;
}

interface SignupDraft {
  companyName: string;
  slug: string;
  plan: string;
  country: string;
  currencyCode: string;
}

interface SignupResult {
  tenant_id: string;
  slug: string;
  plan: string;
  user_id: string;
  provision_complete: boolean;
}

const emptyDraft: SignupDraft = {
  companyName: "",
  slug: "",
  plan: "free",
  country: "",
  currencyCode: "",
};

function loadDraft(): SignupDraft {
  try {
    const raw = sessionStorage.getItem(DRAFT_KEY);
    if (!raw) return emptyDraft;
    return { ...emptyDraft, ...(JSON.parse(raw) as Partial<SignupDraft>) };
  } catch {
    return emptyDraft;
  }
}

export function SignupPage() {
  const navigate = useNavigate();
  const [params] = useSearchParams();
  const code = params.get("code") ?? "";

  const [draft, setDraft] = useState<SignupDraft>(loadDraft);
  const [plans, setPlans] = useState<PlanOption[]>([]);
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  const [result, setResult] = useState<SignupResult | null>(null);

  // Load the available plans for the picker. /api/v1/plans is shared
  // metadata (not tenant-scoped) so it needs no auth.
  useEffect(() => {
    let cancelled = false;
    void (async () => {
      try {
        const r = await fetch("/api/v1/plans");
        if (!r.ok) return;
        const body = (await r.json()) as { plans?: PlanOption[] };
        if (!cancelled && Array.isArray(body.plans)) {
          setPlans(body.plans);
        }
      } catch {
        // Plan list is a progressive-enhancement; the radio list
        // falls back to the free plan if the fetch fails.
      }
    })();
    return () => {
      cancelled = true;
    };
  }, []);

  // When KChat redirects back with ?code=, restore the stashed draft
  // so the company/plan the visitor chose before the redirect is
  // submitted rather than lost.
  useEffect(() => {
    if (code) {
      setDraft(loadDraft());
    }
  }, [code]);

  const update = <K extends keyof SignupDraft>(key: K, value: SignupDraft[K]) =>
    setDraft((d) => ({ ...d, [key]: value }));

  const planChoices: PlanOption[] = useMemo(
    () =>
      plans.length > 0
        ? plans
        : [{ name: "free", display_name: "Free" }],
    [plans],
  );

  const canSubmit = draft.companyName.trim().length > 0 && code.length > 0;

  // Persist the draft and hand off to KChat. The redirect target is
  // /signup so KChat returns the visitor here (LoginPage uses /login
  // for the sign-in flow); the backend's KChat start endpoint reads
  // the redirect from the request.
  const startKChat = () => {
    sessionStorage.setItem(DRAFT_KEY, JSON.stringify(draft));
    const redirect = encodeURIComponent(window.location.origin + "/signup");
    window.location.href = `/api/v1/auth/kchat/start?redirect_uri=${redirect}`;
  };

  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    if (!canSubmit) return;
    setBusy(true);
    setErr(null);
    try {
      const r = await fetch("/api/v1/signup", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({
          kchat_code: code,
          redirect_uri: window.location.origin + "/signup",
          company_name: draft.companyName.trim(),
          slug: draft.slug.trim(),
          plan: draft.plan,
          country: draft.country.trim(),
          currency_code: draft.currencyCode.trim(),
        }),
      });
      const body = (await r.json().catch(() => null)) as SignupResult | null;
      if (!r.ok) {
        // A 500 with a body still carries the created tenant id +
        // provision_complete:false — surface that rather than a bare
        // error so the visitor knows the tenant exists.
        if (body && body.tenant_id) {
          setResult(body);
          setErr(
            "Your workspace was created but setup did not fully complete. Our team has been notified.",
          );
          sessionStorage.removeItem(DRAFT_KEY);
          return;
        }
        throw new Error(`Signup failed (${r.status})`);
      }
      if (body) setResult(body);
      sessionStorage.removeItem(DRAFT_KEY);
    } catch (e2) {
      setErr(e2 instanceof Error ? e2.message : String(e2));
    } finally {
      setBusy(false);
    }
  };

  if (result) {
    return (
      <div style={{ maxWidth: 420 }}>
        <h1>Workspace ready</h1>
        <p>
          <strong>{result.slug}</strong> is set up on the{" "}
          <strong>{result.plan}</strong> plan.
        </p>
        {!result.provision_complete && err && (
          <p style={{ color: "#a15c00" }}>{err}</p>
        )}
        <p>Sign in to start using Kapp.</p>
        <button type="button" onClick={() => navigate("/login")}>
          Go to sign in
        </button>
      </div>
    );
  }

  return (
    <form onSubmit={submit} style={{ maxWidth: 420 }}>
      <h1>Create your workspace</h1>

      <label>
        Company name
        <input
          value={draft.companyName}
          onChange={(e) => update("companyName", e.target.value)}
          placeholder="Acme, Inc."
        />
      </label>

      <fieldset>
        <legend>Plan</legend>
        {planChoices.map((p) => (
          <label key={p.name} style={{ display: "block" }}>
            <input
              type="radio"
              name="plan"
              value={p.name}
              checked={draft.plan === p.name}
              onChange={() => update("plan", p.name)}
            />
            {p.display_name}
            {p.trial_days && p.trial_days > 0
              ? ` — ${p.trial_days}-day trial`
              : ""}
          </label>
        ))}
      </fieldset>

      <details>
        <summary>Optional details</summary>
        <label>
          Workspace slug (optional)
          <input
            value={draft.slug}
            onChange={(e) => update("slug", e.target.value)}
            placeholder="acme"
          />
        </label>
        <label>
          Country (ISO 3166-1 alpha-2, optional)
          <input
            value={draft.country}
            onChange={(e) => update("country", e.target.value.toUpperCase())}
            placeholder="US"
            maxLength={2}
          />
        </label>
        <label>
          Currency (ISO 4217, optional)
          <input
            value={draft.currencyCode}
            onChange={(e) =>
              update("currencyCode", e.target.value.toUpperCase())
            }
            placeholder="USD"
            maxLength={3}
          />
        </label>
      </details>

      {code ? (
        <button type="submit" disabled={!canSubmit || busy}>
          {busy ? "Creating workspace…" : "Create workspace"}
        </button>
      ) : (
        <button
          type="button"
          onClick={startKChat}
          disabled={draft.companyName.trim().length === 0}
        >
          Continue with KChat
        </button>
      )}

      {err && !result && <p style={{ color: "red" }}>{err}</p>}
      <p style={{ color: "#666", fontSize: 12 }}>
        Already have a workspace? <a href="/login">Sign in</a>
      </p>
    </form>
  );
}
