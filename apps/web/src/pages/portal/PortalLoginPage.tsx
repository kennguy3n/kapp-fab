import { useEffect, useState } from "react";
import { useNavigate, useParams, useSearchParams } from "react-router-dom";
import { Button, Input } from "@kapp/ui";
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
    <main className="mx-auto mt-16 max-w-[420px] p-4">
      <h1>Customer portal</h1>
      <p>
        Enter your email to receive a sign-in link. We'll email you a
        one-time link valid for 15 minutes.
      </p>
      <form onSubmit={onSubmit} className="grid gap-2">
        <Input
          type="email"
          required
          value={email}
          onChange={(e) => setEmail(e.target.value)}
          placeholder="you@example.com"
        />
        <Button type="submit" className="justify-self-start">
          Send sign-in link
        </Button>
      </form>
      {status && <p className="text-success">{status}</p>}
      {err && <p className="text-danger">{err}</p>}
    </main>
  );
}
