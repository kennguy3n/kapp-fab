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
import { initTheme } from "./lib/theme";

// Apply the persisted / system colour scheme synchronously before the
// first React render so there's no flash of the wrong theme.
initTheme();

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
