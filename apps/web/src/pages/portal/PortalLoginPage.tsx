import { useEffect, useState } from "react";
import { useNavigate, useParams, useSearchParams } from "react-router-dom";
import { Button, Field, Input, Spinner } from "@kapp/ui";
import {
  portalApi,
  PORTAL_EMAIL_KEY,
  PORTAL_TENANT_KEY,
  PORTAL_TOKEN_KEY,
} from "../../lib/portalApi";
import { AuthScaffold, AuthAlert } from "../auth/AuthScaffold";
import { friendlyPortalError } from "./portalStrings";

// PortalLoginPage runs the magic-link flow: the customer enters
// their email, we POST /portal/auth/request, the backend mails the
// token, and the customer returns via the emailed link which hits
// the same page with ?token=… so we swap it for a portal JWT.
export function PortalLoginPage() {
  const { tenant_slug } = useParams<{ tenant_slug: string }>();
  const [params] = useSearchParams();
  const nav = useNavigate();
  const [email, setEmail] = useState("");
  const [sent, setSent] = useState(false);
  const [submitting, setSubmitting] = useState(false);
  const [err, setErr] = useState<string | null>(null);

  const incomingToken = params.get("token");
  const incomingEmail = params.get("email");
  // While we exchange a magic-link token from the email, show a
  // branded "Signing you in…" state rather than the request form.
  const [verifying, setVerifying] = useState(
    () => Boolean(incomingToken && incomingEmail),
  );

  useEffect(() => {
    if (!incomingToken || !incomingEmail || !tenant_slug) return;
    (async () => {
      try {
        const out = await portalApi.verifyLink(
          tenant_slug,
          incomingEmail,
          incomingToken,
        );
        localStorage.setItem(PORTAL_TOKEN_KEY, out.token);
        localStorage.setItem(PORTAL_TENANT_KEY, tenant_slug);
        localStorage.setItem(PORTAL_EMAIL_KEY, out.user.email);
        nav(`/portal/${tenant_slug}/tickets`);
      } catch (e) {
        setErr(
          friendlyPortalError(
            e,
            "That sign-in link is no longer valid. Please request a new one.",
          ),
        );
        setVerifying(false);
        // Drop the consumed (now-invalid) magic-link params from the URL
        // so a refresh doesn't re-attempt the dead token and the customer
        // lands cleanly on the request form (mirrors CallbackPage).
        const url = new URL(window.location.href);
        url.searchParams.delete("token");
        url.searchParams.delete("email");
        window.history.replaceState({}, "", `${url.pathname}${url.search}`);
      }
    })();
  }, [incomingToken, incomingEmail, tenant_slug, nav]);

  const onSubmit = async (e: React.FormEvent) => {
    e.preventDefault();
    if (!email.trim() || submitting) return;
    setSubmitting(true);
    setErr(null);
    try {
      await portalApi.requestLink(tenant_slug!, email);
      setSent(true);
    } catch (ex) {
      setErr(friendlyPortalError(ex));
    } finally {
      setSubmitting(false);
    }
  };

  // Branded redirect state while a magic link from the email is
  // exchanged for a session. The customer never sees a raw error.
  if (verifying) {
    return (
      <AuthScaffold bare>
        <Spinner size="lg" />
        <p className="text-sm text-fg-muted">Signing you in…</p>
      </AuthScaffold>
    );
  }

  return (
    <AuthScaffold
      title="Customer support"
      description="Sign in to open a request and track its progress."
      footer={
        <span>
          We'll only use your email to send a secure, one-time sign-in link.
        </span>
      }
    >
      {sent ? (
        <div className="flex flex-col gap-4">
          <AuthAlert tone="success">
            Check your inbox — we've sent a sign-in link to{" "}
            <span className="font-medium">{email}</span>. It's valid for 15
            minutes.
          </AuthAlert>
          <Button
            type="button"
            variant="outline"
            className="w-full"
            onClick={() => {
              setSent(false);
              setErr(null);
            }}
          >
            Use a different email
          </Button>
        </div>
      ) : (
        <form onSubmit={onSubmit} className="flex flex-col gap-4" noValidate>
          <Field
            label="Email address"
            required
            help="Enter the email your support contact is registered to."
          >
            <Input
              type="email"
              required
              value={email}
              onChange={(e) => setEmail(e.target.value)}
              placeholder="name@example.com"
              autoComplete="email"
              autoFocus
            />
          </Field>
          <Button type="submit" size="lg" className="w-full" disabled={submitting}>
            {submitting ? "Sending link…" : "Email me a sign-in link"}
          </Button>
          {err && <AuthAlert tone="danger">{err}</AuthAlert>}
        </form>
      )}
    </AuthScaffold>
  );
}
