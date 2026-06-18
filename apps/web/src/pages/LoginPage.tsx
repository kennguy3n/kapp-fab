import { useEffect, useState } from "react";
import { useNavigate, useSearchParams } from "react-router-dom";
import { ChevronDown, KeyRound, MessagesSquare } from "lucide-react";
import { Button, Input, Spinner, cn } from "@kapp/ui";
import { AuthScaffold, AuthAlert } from "./auth/AuthScaffold";

// LoginPage drives the Phase H JWT auth flow. The dev path still
// accepts a hand-pasted tenant slug + token for local work, but the
// SSO path posts a KChat auth code to POST /api/v1/auth/sso and
// stores the returned access/refresh tokens plus resolved tenant id.
export function LoginPage() {
  const navigate = useNavigate();
  const [params] = useSearchParams();
  // Capture once whether we arrived back from the identity provider with
  // an auth code in the URL — that path shows a branded "Signing you in…"
  // state rather than the sign-in form.
  const [autoExchanging] = useState(() => Boolean(params.get("code")));
  const [code, setCode] = useState("");
  const [tenant, setTenant] = useState(localStorage.getItem("kapp.tenant") ?? "");
  const [token, setToken] = useState("");
  const [err, setErr] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const [devOpen, setDevOpen] = useState(false);

  useEffect(() => {
    const qcode = params.get("code");
    if (!qcode) return;
    setBusy(true);
    void exchange(qcode);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  async function exchange(authCode: string) {
    try {
      const r = await fetch("/api/v1/auth/sso", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({
          code: authCode,
          redirect_uri: window.location.origin + "/login",
        }),
      });
      if (!r.ok) {
        throw new Error(`SSO failed (${r.status})`);
      }
      const body = (await r.json()) as {
        access_token: string;
        refresh_token: string;
        tenant_id: string;
        expires_in: number;
      };
      localStorage.setItem("kapp.token", body.access_token);
      localStorage.setItem("kapp.refresh", body.refresh_token);
      localStorage.setItem("kapp.tenant", body.tenant_id);
      localStorage.setItem(
        "kapp.expires_at",
        String(Date.now() + body.expires_in * 1000),
      );
      navigate("/");
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e));
      setBusy(false);
    }
  }

  const submit = (e: React.FormEvent) => {
    e.preventDefault();
    if (code) {
      setBusy(true);
      void exchange(code);
      return;
    }
    localStorage.setItem("kapp.tenant", tenant);
    if (token) localStorage.setItem("kapp.token", token);
    navigate("/");
  };

  // Branded redirect state: the user has come back from KChat/SSO with a
  // `?code=` and we are exchanging it. The end user never sees a raw auth
  // error here — only friendly copy with a way to retry.
  if (autoExchanging) {
    return (
      <AuthScaffold bare>
        {err ? (
          <>
            <h1 className="text-xl font-medium tracking-tight text-fg">
              We couldn't sign you in
            </h1>
            <p className="max-w-sm text-sm text-fg-muted">
              Your sign-in link may have expired. Please try signing in again.
            </p>
            <Button asChild className="mt-1">
              <a href="/login">Back to sign in</a>
            </Button>
          </>
        ) : (
          <>
            <Spinner size="lg" />
            <p className="text-sm text-fg-muted">Signing you in…</p>
          </>
        )}
      </AuthScaffold>
    );
  }

  return (
    <AuthScaffold
      title="Sign in"
      description="Welcome back — sign in to your Kapp workspace."
      footer={
        <span>
          Need a customer account?{" "}
          <span className="text-fg">Ask your provider for a portal link.</span>
        </span>
      }
    >
      <div className="flex flex-col gap-3">
        <Button asChild size="lg" className="w-full">
          <a href="/api/v1/auth/kchat/start">
            <MessagesSquare aria-hidden="true" className="h-4 w-4" />
            Sign in with KChat
          </a>
        </Button>

        <Button asChild variant="outline" size="lg" className="w-full">
          <a href="/api/v1/auth/login">
            <KeyRound aria-hidden="true" className="h-4 w-4" />
            Use single sign-on
          </a>
        </Button>
        <p className="text-center text-xs text-fg-muted">
          Single sign-on supports passkeys, MFA, and social login.
        </p>
      </div>

      <div className="border-t border-border pt-1">
        <button
          type="button"
          onClick={() => setDevOpen((o) => !o)}
          aria-expanded={devOpen}
          aria-controls="dev-signin-panel"
          className="flex w-full items-center justify-between rounded-md py-1.5 text-sm font-medium text-fg-muted transition-colors hover:text-fg"
        >
          Developer sign-in
          <ChevronDown
            aria-hidden="true"
            className={cn(
              "h-4 w-4 transition-transform",
              devOpen && "rotate-180",
            )}
          />
        </button>

        {devOpen && (
          <form
            id="dev-signin-panel"
            onSubmit={submit}
            className="mt-3 flex flex-col gap-3"
          >
            <label className="flex flex-col gap-1.5 text-sm font-medium text-fg">
              KChat auth code
              <Input
                value={code}
                onChange={(e) => setCode(e.target.value)}
                placeholder="Paste a KChat auth code"
                autoComplete="off"
              />
            </label>

            <div className="h-px bg-border" role="separator" />
            <p className="text-xs text-fg-muted">
              Local dev only — sign in with a tenant slug and optional token.
            </p>

            <label className="flex flex-col gap-1.5 text-sm font-medium text-fg">
              Tenant
              <Input
                value={tenant}
                onChange={(e) => setTenant(e.target.value)}
                placeholder="acme"
                autoComplete="off"
              />
            </label>
            <label className="flex flex-col gap-1.5 text-sm font-medium text-fg">
              Token (optional)
              <Input
                value={token}
                onChange={(e) => setToken(e.target.value)}
                autoComplete="off"
              />
            </label>

            <Button type="submit" disabled={busy} className="w-full">
              {busy ? "Signing in…" : "Continue"}
            </Button>
            {err && <AuthAlert tone="danger">{err}</AuthAlert>}
          </form>
        )}
      </div>
    </AuthScaffold>
  );
}
