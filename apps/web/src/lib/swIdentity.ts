// Service-worker identity handshake.
//
// The PWA service worker (public/sw.js) caches `GET /api/*` by URL
// only, so on a shared device it would otherwise serve one user's
// cached reads to the next. We hand the SW a non-reversible hash of
// (tenant + token) so it can detect an identity change and drop the
// API cache; the raw token never leaves the page.
//
// This lives in its own side-effect-free module (rather than inside
// main.tsx, which bootstraps React on import) so callers can import
// `postIdentityToServiceWorker` without re-running app startup — in
// particular the auth layer should call it after a credential change
// (login/logout without a full reload) so the SW's read-cache
// isolation tracks the live identity.

async function identityHash(): Promise<string> {
  const tenant = localStorage.getItem("kapp.tenant") ?? "";
  const token = localStorage.getItem("kapp.token") ?? "";
  const data = new TextEncoder().encode(`${tenant}\u0000${token}`);
  const digest = await crypto.subtle.digest("SHA-256", data);
  return Array.from(new Uint8Array(digest))
    .map((b) => b.toString(16).padStart(2, "0"))
    .join("");
}

/**
 * Post the current identity hash to the active service worker so it can
 * isolate its read cache per identity. Safe to call repeatedly — it is
 * idempotent from the SW's perspective (the SW only drops the API cache
 * when the hash actually changes). Degrades gracefully (no-op) when the
 * SW or `crypto.subtle` is unavailable.
 */
export async function postIdentityToServiceWorker(): Promise<void> {
  if (typeof navigator === "undefined" || !("serviceWorker" in navigator))
    return;
  try {
    const id = await identityHash();
    const reg = await navigator.serviceWorker.ready;
    (reg.active ?? navigator.serviceWorker.controller)?.postMessage({
      type: "kapp:identity",
      id,
    });
  } catch {
    // Isolation degrades gracefully if crypto.subtle is unavailable.
  }
}
