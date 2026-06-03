import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { SignupPage } from "./SignupPage";

// Render the page at /signup with an optional auth code in the query
// string (KChat redirects back with ?code=). A sibling /login route
// lets us assert the post-success "Go to sign in" navigation lands
// somewhere real.
function renderSignup(initialEntry = "/signup") {
  return render(
    <MemoryRouter initialEntries={[initialEntry]}>
      <Routes>
        <Route path="/signup" element={<SignupPage />} />
        <Route path="/login" element={<div>Login screen</div>} />
      </Routes>
    </MemoryRouter>,
  );
}

// A fetch double that routes by URL: GET /api/v1/plans returns the
// plan list; POST /api/v1/signup returns the supplied signup result.
function stubFetch(opts: {
  plans?: unknown[];
  signup?: { ok: boolean; status: number; body: unknown };
}) {
  const fetchMock = vi.fn(async (url: string, _init?: RequestInit) => {
    if (url === "/api/v1/plans") {
      return {
        ok: true,
        status: 200,
        json: async () => ({ plans: opts.plans ?? [] }),
      } as Response;
    }
    if (url === "/api/v1/signup") {
      const s = opts.signup ?? { ok: true, status: 201, body: {} };
      return {
        ok: s.ok,
        status: s.status,
        json: async () => s.body,
        text: async () => JSON.stringify(s.body),
      } as Response;
    }
    throw new Error(`unexpected fetch ${url}`);
  });
  vi.stubGlobal("fetch", fetchMock);
  return fetchMock;
}

describe("SignupPage", () => {
  beforeEach(() => {
    vi.unstubAllGlobals();
    sessionStorage.clear();
  });

  it("renders the plan picker from /api/v1/plans", async () => {
    stubFetch({
      plans: [
        { name: "free", display_name: "Free" },
        { name: "starter", display_name: "Starter", trial_days: 14 },
      ],
    });
    renderSignup();
    expect(
      await screen.findByRole("radio", { name: /Free/i }),
    ).toBeInTheDocument();
    // The trial length is surfaced on the plan label.
    expect(
      screen.getByRole("radio", { name: /Starter — 14-day trial/i }),
    ).toBeInTheDocument();
  });

  it("shows the KChat hand-off button (not submit) when there is no auth code", async () => {
    stubFetch({ plans: [{ name: "free", display_name: "Free" }] });
    renderSignup("/signup");
    // Without a code we can't submit yet; the CTA is the KChat
    // hand-off, gated on a company name.
    const cta = await screen.findByRole("button", {
      name: /Continue with KChat/i,
    });
    expect(cta).toBeDisabled();
    await userEvent.type(
      screen.getByLabelText(/Company name/i),
      "Acme Co",
    );
    await waitFor(() => expect(cta).not.toBeDisabled());
  });

  it("posts the signup payload with the auth code and shows the ready state", async () => {
    const fetchMock = stubFetch({
      plans: [
        { name: "free", display_name: "Free" },
        { name: "starter", display_name: "Starter" },
      ],
      signup: {
        ok: true,
        status: 201,
        body: {
          tenant_id: "t-1",
          slug: "acme",
          plan: "starter",
          user_id: "u-1",
          provision_complete: true,
        },
      },
    });
    renderSignup("/signup?code=kc_abc123");

    const user = userEvent.setup();
    await user.type(screen.getByLabelText(/Company name/i), "Acme Co");
    await user.click(screen.getByRole("radio", { name: /Starter/i }));
    await user.click(
      screen.getByRole("button", { name: /Create workspace/i }),
    );

    await waitFor(() => {
      const signupCall = fetchMock.mock.calls.find(
        (c) => c[0] === "/api/v1/signup",
      );
      expect(signupCall).toBeTruthy();
    });
    const init = fetchMock.mock.calls.find(
      (c) => c[0] === "/api/v1/signup",
    )![1] as RequestInit;
    expect(init.method).toBe("POST");
    const payload = JSON.parse(init.body as string);
    expect(payload.kchat_code).toBe("kc_abc123");
    expect(payload.company_name).toBe("Acme Co");
    expect(payload.plan).toBe("starter");

    // Success surface confirms the tenant and routes to sign-in.
    expect(await screen.findByText(/Workspace ready/i)).toBeInTheDocument();
    expect(screen.getByText("acme")).toBeInTheDocument();
  });

  it("surfaces the partial-provision warning when signup returns 500 with a tenant id", async () => {
    stubFetch({
      plans: [{ name: "free", display_name: "Free" }],
      signup: {
        ok: false,
        status: 500,
        body: {
          tenant_id: "t-9",
          slug: "acme",
          plan: "free",
          user_id: "u-9",
          provision_complete: false,
        },
      },
    });
    renderSignup("/signup?code=kc_zzz");
    const user = userEvent.setup();
    await user.type(screen.getByLabelText(/Company name/i), "Acme Co");
    await user.click(
      screen.getByRole("button", { name: /Create workspace/i }),
    );
    expect(
      await screen.findByText(/setup did not fully complete/i),
    ).toBeInTheDocument();
  });
});
