import { describe, it, expect, beforeEach, afterEach } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { server } from "../test/msw/server";
import { LoginPage } from "./LoginPage";

// LoginPage talks to POST /api/v1/auth/sso through the raw fetch API
// (not the ApiClient), so we exercise it through MSW rather than a
// module mock. The default handler in src/test/msw/handlers.ts returns
// a valid token bundle; individual tests override it to assert the
// failure path.

function renderLogin(initialEntry = "/login") {
  return render(
    <MemoryRouter initialEntries={[initialEntry]}>
      <Routes>
        <Route path="/login" element={<LoginPage />} />
        <Route path="/" element={<div>Home dashboard</div>} />
      </Routes>
    </MemoryRouter>,
  );
}

describe("LoginPage", () => {
  beforeEach(() => {
    localStorage.clear();
  });
  afterEach(() => {
    localStorage.clear();
  });

  it("dev path persists tenant + token and navigates to the dashboard", async () => {
    const user = userEvent.setup();
    renderLogin();

    await user.click(
      screen.getByRole("button", { name: /Developer sign-in/i }),
    );
    await user.type(screen.getByLabelText(/^Tenant$/i), "acme");
    await user.type(screen.getByLabelText(/Token/i), "dev-token-123");
    await user.click(screen.getByRole("button", { name: /Continue/i }));

    await screen.findByText("Home dashboard");
    expect(localStorage.getItem("kapp.tenant")).toBe("acme");
    expect(localStorage.getItem("kapp.token")).toBe("dev-token-123");
  });

  it("exchanges a manually pasted KChat auth code via POST /auth/sso", async () => {
    const user = userEvent.setup();
    renderLogin();

    await user.click(
      screen.getByRole("button", { name: /Developer sign-in/i }),
    );
    await user.type(screen.getByLabelText(/KChat auth code/i), "kchat-code");
    await user.click(screen.getByRole("button", { name: /Continue/i }));

    await screen.findByText("Home dashboard");
    // Tokens from the default SSO handler are persisted for session
    // continuity across reloads.
    expect(localStorage.getItem("kapp.token")).toBe("test-access-token");
    expect(localStorage.getItem("kapp.refresh")).toBe("test-refresh-token");
    expect(localStorage.getItem("kapp.tenant")).toBe(
      "00000000-0000-4000-8000-000000000001",
    );
    expect(localStorage.getItem("kapp.expires_at")).not.toBeNull();
  });

  it("auto-exchanges the ?code= query param on mount", async () => {
    renderLogin("/login?code=sso-redirect-code");

    await screen.findByText("Home dashboard");
    expect(localStorage.getItem("kapp.token")).toBe("test-access-token");
  });

  it("surfaces the SSO error and stays on the login form when exchange fails", async () => {
    server.use(
      http.post("/api/v1/auth/sso", () =>
        HttpResponse.json({ error: "bad code" }, { status: 401 }),
      ),
    );
    const user = userEvent.setup();
    renderLogin();

    await user.click(
      screen.getByRole("button", { name: /Developer sign-in/i }),
    );
    await user.type(screen.getByLabelText(/KChat auth code/i), "bad");
    await user.click(screen.getByRole("button", { name: /Continue/i }));

    expect(await screen.findByText(/SSO failed \(401\)/i)).toBeInTheDocument();
    expect(screen.queryByText("Home dashboard")).not.toBeInTheDocument();
    expect(localStorage.getItem("kapp.token")).toBeNull();
    // Button is re-enabled so the user can retry.
    expect(screen.getByRole("button", { name: /Continue/i })).not.toBeDisabled();
  });

  it("renders the KChat SSO start link", () => {
    renderLogin();
    const link = screen.getByRole("link", { name: /Sign in with KChat/i });
    expect(link).toHaveAttribute("href", "/api/v1/auth/kchat/start");
  });
});
