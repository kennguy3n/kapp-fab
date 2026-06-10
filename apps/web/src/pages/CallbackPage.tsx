import { useEffect, useRef, useState } from "react";
import { useNavigate } from "react-router-dom";

// CallbackPage completes the iam-core (OAuth2/OIDC) Authorization-Code
// login. The backend's GET /api/v1/auth/callback exchanges the code,
// validates the id_token, sets the httpOnly refresh cookie, and then
// 302-redirects the browser here with the tokens in the URL *fragment*
// (so they never hit the server or server logs):
//
//   /callback#access_token=…&token_type=Bearer&expires_in=3600&id_token=…
//
// This component lifts those out of location.hash, persists them in
// the same localStorage slots the rest of the app already reads
// (kapp.token / kapp.tenant / kapp.expires_at), wipes the fragment
// from the address bar, and navigates into the app. It deliberately
// reuses the existing storage contract so every authenticated fetch
// (Bearer + X-Tenant-ID) keeps working unchanged for iam-core tokens.
export function CallbackPage() {
  const navigate = useNavigate();
  const [err, setErr] = useState<string | null>(null);
  // StrictMode double-invokes effects in dev; guard so we only consume
  // the fragment once.
  const handled = useRef(false);

  useEffect(() => {
    if (handled.current) return;
    handled.current = true;

    // The fragment may arrive as "#access_token=…" or, on an error
    // bounce, as a query string "?error=…". Check both.
    const hash = window.location.hash.replace(/^#/, "");
    const search = window.location.search.replace(/^\?/, "");
    const frag = new URLSearchParams(hash);
    const query = new URLSearchParams(search);

    const providerErr = frag.get("error") ?? query.get("error");
    if (providerErr) {
      setErr(sanitizeError(providerErr));
      return;
    }

    const accessToken = frag.get("access_token");
    if (!accessToken) {
      setErr("login_incomplete");
      return;
    }

    const tenant = tenantFromJWT(accessToken);
    if (tenant) localStorage.setItem("kapp.tenant", tenant);
    localStorage.setItem("kapp.token", accessToken);

    const idToken = frag.get("id_token");
    if (idToken) localStorage.setItem("kapp.id_token", idToken);

    const expiresIn = Number(frag.get("expires_in"));
    if (Number.isFinite(expiresIn) && expiresIn > 0) {
      localStorage.setItem(
        "kapp.expires_at",
        String(Date.now() + expiresIn * 1000),
      );
    }

    // The backend forwards the user's intended landing path as a
    // same-site ?return_to=; navigate there once tokens are stored, or
    // fall back to the app root. Validated to reject open redirects.
    const dest = safeReturnTo(query.get("return_to"));

    // Strip the tokens (and the return_to query) from the address bar so
    // they are not bookmarked, shared, or left in browser history.
    window.history.replaceState(null, "", window.location.pathname);

    navigate(dest, { replace: true });
  }, [navigate]);

  if (err) {
    return (
      <div className="flex max-w-[360px] flex-col gap-3 p-6">
        <h1>Sign-in failed</h1>
        <p className="text-danger">We couldn't complete sign-in ({err}).</p>
        <a href="/login">Try again</a>
      </div>
    );
  }
  return (
    <div className="flex max-w-[360px] flex-col gap-3 p-6">
      <p>Signing you in…</p>
    </div>
  );
}

// tenantFromJWT pulls the Kapp tenant id out of an iam-core access
// token's `kapp_tenant_id` custom claim (the same claim the backend
// validator maps to Claims.TenantID). This is a read-only decode of
// the unverified payload purely to populate the X-Tenant-ID header;
// the server independently verifies the token's signature on every
// request, so a tampered payload here gains nothing. Returns "" when
// the claim is absent or the token is unparseable.
function tenantFromJWT(token: string): string {
  try {
    const parts = token.split(".");
    if (parts.length < 2) return "";
    const json = atob(base64UrlToBase64(parts[1]));
    const claims = JSON.parse(json) as Record<string, unknown>;
    const tid = claims["kapp_tenant_id"] ?? claims["tenant_id"];
    return typeof tid === "string" ? tid : "";
  } catch {
    return "";
  }
}

// safeReturnTo accepts only a same-site absolute path ("/foo/bar"),
// mirroring the backend's sanitizeReturnTo, so a crafted return_to
// cannot turn the post-login navigation into an open redirect. Returns
// "/" for any missing or unsafe value.
function safeReturnTo(p: string | null): string {
  if (!p) return "/";
  if (!p.startsWith("/") || p.startsWith("//")) return "/";
  if (p.includes("\\")) return "/";
  return p;
}

function base64UrlToBase64(s: string): string {
  const padded = s.padEnd(s.length + ((4 - (s.length % 4)) % 4), "=");
  return padded.replace(/-/g, "+").replace(/_/g, "/");
}

// sanitizeError keeps only the OAuth2 error-code charset so a crafted
// ?error= value cannot inject markup when we render it.
function sanitizeError(code: string): string {
  const trimmed = code.slice(0, 64);
  return /^[A-Za-z0-9_]+$/.test(trimmed) ? trimmed : "invalid_request";
}
