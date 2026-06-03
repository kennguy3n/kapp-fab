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

// Register the PWA service worker (public/sw.js) for offline support
// and installability. Only in production builds: in dev, Vite's HMR
// and the unhashed module graph make a caching SW actively harmful
// (it would serve stale modules), and the test/SSR environments have
// no `navigator.serviceWorker`. Registration failures are swallowed —
// the app must work without the SW.
if (import.meta.env.PROD && "serviceWorker" in navigator) {
  window.addEventListener("load", () => {
    navigator.serviceWorker.register("/sw.js").catch(() => {
      // Registration is best-effort; the app still works uncached.
    });
  });
}
