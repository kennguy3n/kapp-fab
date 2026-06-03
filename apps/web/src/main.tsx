import React from "react";
import ReactDOM from "react-dom/client";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { BrowserRouter } from "react-router-dom";
import { App } from "./App";
// Design-system tokens + Tailwind v4 entry-point are owned by
// @kapp/ui (packages/ui/src/styles/globals.css).  Importing it
// through the package's exports map keeps apps/web a pure
// consumer of the design system — moving the file inside the
// package later (or splitting it across multiple files) doesn't
// require an apps/web code change.
import "@kapp/ui/styles/globals.css";

const queryClient = new QueryClient();

const rootEl = document.getElementById("root");
if (!rootEl) throw new Error("#root not found");

ReactDOM.createRoot(rootEl).render(
  <React.StrictMode>
    <QueryClientProvider client={queryClient}>
      <BrowserRouter>
        <App />
      </BrowserRouter>
    </QueryClientProvider>
  </React.StrictMode>
);

// A non-reversible marker for the currently signed-in identity. The SW
// caches GET /api/* by URL only, so on a shared device it would
// otherwise serve one user's cached reads to the next. We hand the SW a
// hash of (tenant + token) so it can detect an identity change and drop
// the API cache; the raw token never leaves the page.
async function identityHash(): Promise<string> {
  const tenant = localStorage.getItem("kapp.tenant") ?? "";
  const token = localStorage.getItem("kapp.token") ?? "";
  const data = new TextEncoder().encode(`${tenant}\u0000${token}`);
  const digest = await crypto.subtle.digest("SHA-256", data);
  return Array.from(new Uint8Array(digest))
    .map((b) => b.toString(16).padStart(2, "0"))
    .join("");
}

// Tell the SW who's using it so it can isolate the read cache per
// identity (no logout flow exists, so this signal is what bounds one
// user's cached data). We post it as EARLY as possible — at module eval,
// not inside the `load` handler — so that on a returning (already
// controlled) visitor the SW drops a stale API cache before the app's
// first reads can be served from it. On the very first visit the page
// isn't controlled yet, so the SW isn't intercepting fetches and there
// is nothing to leak. `serviceWorker.ready` resolves as soon as there's
// an active registration, independent of the fresh register() below.
async function postIdentityToServiceWorker(): Promise<void> {
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

// Register the PWA service worker (public/sw.js) for offline support
// and installability. Only in production builds: in dev, Vite's HMR
// and the unhashed module graph make a caching SW actively harmful
// (it would serve stale modules), and the test/SSR environments have
// no `navigator.serviceWorker`. Registration failures are swallowed —
// the app must work without the SW.
if (import.meta.env.PROD && "serviceWorker" in navigator) {
  // Kick off the identity handshake immediately (not gated on `load`) so
  // a controlling SW can invalidate another user's cache as early as
  // possible. Re-post if the controller changes mid-session.
  void postIdentityToServiceWorker();
  navigator.serviceWorker.addEventListener("controllerchange", () => {
    void postIdentityToServiceWorker();
  });
  window.addEventListener("load", () => {
    navigator.serviceWorker.register("/sw.js").catch(() => {
      // Registration is best-effort; the app still works uncached.
    });
  });
}
