import { describe, it, expect, beforeEach, afterEach } from "vitest";
import { render, screen } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { CallbackPage } from "./CallbackPage";

// CallbackPage reads the token fragment from window.location (set by the
// backend redirect) rather than react-router's location, so each test
// primes window.location via history.replaceState before rendering. The
// in-app navigation afterwards is driven by react-router, so we mount a
// MemoryRouter with destination routes and assert which one renders.

function base64url(obj: unknown): string {
  return btoa(JSON.stringify(obj))
    .replace(/=/g, "")
    .replace(/\+/g, "-")
    .replace(/\//g, "_");
}

function jwtWithTenant(tenant: string): string {
  return `e30.${base64url({ kapp_tenant_id: tenant })}.sig`;
}

function renderCallback(url: string) {
  window.history.replaceState({}, "", url);
  return render(
    <MemoryRouter initialEntries={[url]}>
      <Routes>
        <Route path="/callback" element={<CallbackPage />} />
        <Route path="/" element={<div>Home dashboard</div>} />
        <Route path="/dashboard" element={<div>Dashboard page</div>} />
      </Routes>
    </MemoryRouter>,
  );
}

describe("CallbackPage", () => {
  beforeEach(() => localStorage.clear());
  afterEach(() => {
    localStorage.clear();
    window.history.replaceState({}, "", "/");
  });

  it("persists the fragment tokens and navigates to the forwarded return_to", async () => {
    const token = jwtWithTenant("tenant-xyz");
    renderCallback(
      `/callback?return_to=%2Fdashboard#access_token=${token}&token_type=Bearer&expires_in=3600`,
    );

    await screen.findByText("Dashboard page");
    expect(localStorage.getItem("kapp.token")).toBe(token);
    expect(localStorage.getItem("kapp.tenant")).toBe("tenant-xyz");
    expect(localStorage.getItem("kapp.expires_at")).not.toBeNull();
  });

  it("falls back to the app root when no return_to is present", async () => {
    const token = jwtWithTenant("tenant-1");
    renderCallback(`/callback#access_token=${token}&token_type=Bearer`);

    await screen.findByText("Home dashboard");
    expect(localStorage.getItem("kapp.token")).toBe(token);
  });

  it("ignores an unsafe (open-redirect) return_to and lands on root", async () => {
    const token = jwtWithTenant("tenant-1");
    renderCallback(
      `/callback?return_to=https:%2F%2Fevil.example#access_token=${token}`,
    );

    await screen.findByText("Home dashboard");
    expect(localStorage.getItem("kapp.token")).toBe(token);
  });

  it("shows an error and does not store tokens when the fragment is missing the access token", async () => {
    renderCallback(`/callback#token_type=Bearer`);

    expect(await screen.findByText(/Sign-in failed/i)).toBeInTheDocument();
    expect(localStorage.getItem("kapp.token")).toBeNull();
  });
});
