import { useEffect, useState } from "react";
import { useNavigate, useSearchParams } from "react-router-dom";
import { Button, Input } from "@kapp/ui";

// LoginPage drives the Phase H JWT auth flow. The dev path still
// accepts a hand-pasted tenant slug + token for local work, but the
// SSO path posts a KChat auth code to POST /api/v1/auth/sso and
// stores the returned access/refresh tokens plus resolved tenant id.
export function LoginPage() {
  const navigate = useNavigate();
  const [params] = useSearchParams();
  const [code, setCode] = useState("");
  const [tenant, setTenant] = useState(localStorage.getItem("kapp.tenant") ?? "");
  const [token, setToken] = useState("");
  const [err, setErr] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

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

  return (
    <form onSubmit={submit} className="flex max-w-[360px] flex-col gap-3">
      <h1>Sign in</h1>
      {/* iam-core Universal Login (OAuth2/OIDC). The backend's
          GET /api/v1/auth/login starts the PKCE Authorization-Code
          flow and redirects to iam-core; on return the /callback
          route stores the tokens. When iam-core is not configured the
          backend responds 503 and this link is a no-op, so the KChat
          path below remains the default. A full-page navigation (not
          fetch) is required so the browser follows the 302 to the
          identity provider. */}
      <p>
        <a href="/api/v1/auth/login">Sign in with SSO (MFA, passkeys, social)</a>
      </p>
      <p>
        <a href="/api/v1/auth/kchat/start">Sign in with KChat</a>
      </p>
      <label className="flex flex-col gap-1">
        KChat auth code
        <Input value={code} onChange={(e) => setCode(e.target.value)} />
      </label>
      <hr />
      <p className="text-xs text-fg-muted">Dev mode (tenant + token)</p>
      <label className="flex flex-col gap-1">
        Tenant
        <Input value={tenant} onChange={(e) => setTenant(e.target.value)} />
      </label>
      <label className="flex flex-col gap-1">
        Token (optional)
        <Input value={token} onChange={(e) => setToken(e.target.value)} />
      </label>
      <Button type="submit" disabled={busy} className="self-start">
        {busy ? "Signing in…" : "Continue"}
      </Button>
      {err && <p className="text-danger">{err}</p>}
    </form>
  );
}
