import { useEffect, useState } from "react";
import { useNavigate, useParams, useSearchParams } from "react-router-dom";
import {
  portalApi,
  PORTAL_EMAIL_KEY,
  PORTAL_TENANT_KEY,
  PORTAL_TOKEN_KEY,
} from "../../lib/portalApi";

// PortalLoginPage runs the magic-link flow: the customer enters
// their email, we POST /portal/auth/request, the backend mails the
// token, and the customer returns via the emailed link which hits
// the same page with ?token=… so we swap it for a portal JWT.
export function PortalLoginPage() {
  const { tenant_slug } = useParams<{ tenant_slug: string }>();
  const [params] = useSearchParams();
  const nav = useNavigate();
  const [email, setEmail] = useState("");
  const [status, setStatus] = useState<string | null>(null);
  const [err, setErr] = useState<string | null>(null);

  const incomingToken = params.get("token");
  const incomingEmail = params.get("email");

  useEffect(() => {
    if (!incomingToken || !incomingEmail || !tenant_slug) return;
    (async () => {
      try {
        const out = await portalApi.verifyLink(
          tenant_slug,
          incomingEmail,
          incomingToken
        );
        localStorage.setItem(PORTAL_TOKEN_KEY, out.token);
        localStorage.setItem(PORTAL_TENANT_KEY, tenant_slug);
        localStorage.setItem(PORTAL_EMAIL_KEY, out.user.email);
        nav(`/portal/${tenant_slug}/tickets`);
      } catch (e) {
        setErr((e as Error).message);
      }
    })();
  }, [incomingToken, incomingEmail, tenant_slug, nav]);

  const onSubmit = async (e: React.FormEvent) => {
    e.preventDefault();
    setStatus(null);
    setErr(null);
    try {
      await portalApi.requestLink(tenant_slug!, email);
      setStatus("Check your email for a sign-in link.");
    } catch (ex) {
      setErr((ex as Error).message);
    }
  };

  return (
    <main style={wrap}>
      <h1>Customer portal</h1>
      <p>
        Enter your email to receive a sign-in link. We'll email you a
        one-time link valid for 15 minutes.
      </p>
      <form onSubmit={onSubmit} style={{ display: "grid", gap: 8 }}>
        <input
          type="email"
          required
          value={email}
          onChange={(e) => setEmail(e.target.value)}
          placeholder="you@example.com"
          style={inp}
        />
        <button type="submit" style={btn}>
          Send sign-in link
        </button>
      </form>
      {status && <p style={{ color: "#065f46" }}>{status}</p>}
      {err && <p style={{ color: "#991b1b" }}>{err}</p>}
    </main>
  );
}

const wrap: React.CSSProperties = {
  maxWidth: 420,
  margin: "64px auto",
  fontFamily: "system-ui, sans-serif",
  padding: 16,
};
const inp: React.CSSProperties = {
  padding: 8,
  border: "1px solid #d1d5db",
  borderRadius: 6,
};
const btn: React.CSSProperties = {
  padding: "8px 14px",
  background: "#111827",
  color: "white",
  border: 0,
  borderRadius: 6,
  cursor: "pointer",
};
