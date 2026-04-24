import { useEffect, useState } from "react";
import { useNavigate, useSearchParams } from "react-router-dom";

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
    <form onSubmit={submit} style={{ maxWidth: 360 }}>
      <h1>Sign in</h1>
      <p>
        <a href="/api/v1/auth/kchat/start">Sign in with KChat</a>
      </p>
      <label>
        KChat auth code
        <input value={code} onChange={(e) => setCode(e.target.value)} />
      </label>
      <hr />
      <p style={{ color: "#666", fontSize: 12 }}>Dev mode (tenant + token)</p>
      <label>
        Tenant
        <input value={tenant} onChange={(e) => setTenant(e.target.value)} />
      </label>
      <label>
        Token (optional)
        <input value={token} onChange={(e) => setToken(e.target.value)} />
      </label>
      <button type="submit" disabled={busy}>
        {busy ? "Signing in…" : "Continue"}
      </button>
      {err && <p style={{ color: "red" }}>{err}</p>}
    </form>
  );
}
