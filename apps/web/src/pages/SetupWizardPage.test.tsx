import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { LocaleProvider } from "../lib/i18n";
import { SetupWizardPage } from "./SetupWizardPage";

const TENANT_ID = "11111111-1111-4111-8111-111111111111";

function renderWizard() {
  // The wizard pulls /:id from the route. MemoryRouter with a
  // single matching route gives us a real param without the
  // login redirect that BrowserRouter+App would apply.
  const qc = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  });
  return render(
    <QueryClientProvider client={qc}>
      <LocaleProvider>
        <MemoryRouter initialEntries={[`/setup/${TENANT_ID}`]}>
          <Routes>
            <Route path="/setup/:id" element={<SetupWizardPage />} />
          </Routes>
        </MemoryRouter>
      </LocaleProvider>
    </QueryClientProvider>,
  );
}

describe("SetupWizardPage", () => {
  beforeEach(() => {
    // Reset any global fetch override from a prior test.
    vi.unstubAllGlobals();
    // Wipe persisted locale so each test starts from the
    // LocaleProvider's deterministic default ("en"). Without
    // this, an earlier test's setLocale would bleed across.
    if (typeof localStorage !== "undefined") {
      localStorage.removeItem("kapp_locale");
    }
  });

  it("blocks the Next button on step 0 until the company name is filled in", async () => {
    renderWizard();
    const next = await screen.findByRole("button", { name: /Next/i });
    expect(next).toBeDisabled();

    const user = userEvent.setup();
    await user.type(screen.getByLabelText(/Company name/i), "Acme Co");
    await waitFor(() => expect(next).not.toBeDisabled());
  });

  it("advances from the company step straight to the optional team step", async () => {
    renderWizard();
    const user = userEvent.setup();
    await user.type(screen.getByLabelText(/Company name/i), "Acme Co");
    // Clear any browser-detected country so the test is deterministic.
    await user.clear(screen.getByLabelText(/Country/i));
    await user.click(screen.getByRole("button", { name: /Next/i }));

    // The two-step wizard has no CoA selection step — advancing from
    // the company step lands directly on the team-invite step.
    expect(
      await screen.findByPlaceholderText(/name@example/i),
    ).toBeInTheDocument();
    expect(
      screen.getByRole("button", { name: /Finish setup/i }),
    ).toBeInTheDocument();
  });

  it("derives coa_template + locale from country and submits them to /api/v1/tenants/{id}/setup", async () => {
    const fetchMock = vi.fn().mockResolvedValue({
      ok: true,
      status: 200,
      text: async () => "",
      json: async () => ({
        tenant_id: TENANT_ID,
        accounts_inserted: 42,
        roles_inserted: 14,
        users_inserted: 1,
        coa_template_used: "sg_basic",
        locale_used: "en",
      }),
    });
    vi.stubGlobal("fetch", fetchMock);

    renderWizard();
    const user = userEvent.setup();
    await user.type(screen.getByLabelText(/Company name/i), "Acme Co");
    await user.clear(screen.getByLabelText(/Country/i));
    await user.type(screen.getByLabelText(/Country/i), "SG");
    await user.click(screen.getByRole("button", { name: /Next/i }));

    // Team step: fill the first invited user's email, then submit.
    const emailInput = await screen.findByPlaceholderText(/name@example/i);
    await user.type(emailInput, "owner@acme.example");
    await user.click(screen.getByRole("button", { name: /Finish setup/i }));

    await waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(1));
    const [url, init] = fetchMock.mock.calls[0]!;
    expect(url).toBe(`/api/v1/tenants/${TENANT_ID}/setup`);
    expect(init.method).toBe("POST");
    const payload = JSON.parse(init.body as string);
    expect(payload.company_name).toBe("Acme Co");
    expect(payload.country).toBe("SG");
    // CoA template resolves from the country without a manual step.
    expect(payload.coa_template).toBe("sg_basic");
    expect(payload.users).toHaveLength(1);
    expect(payload.users[0].email).toBe("owner@acme.example");
  });

  it("lets the operator skip team invites entirely", async () => {
    const fetchMock = vi.fn().mockResolvedValue({
      ok: true,
      status: 200,
      text: async () => "",
      json: async () => ({
        tenant_id: TENANT_ID,
        accounts_inserted: 42,
        roles_inserted: 14,
        users_inserted: 0,
        coa_template_used: "ifrs_basic",
        locale_used: "en",
      }),
    });
    vi.stubGlobal("fetch", fetchMock);

    renderWizard();
    const user = userEvent.setup();
    await user.type(screen.getByLabelText(/Company name/i), "Acme Co");
    await user.clear(screen.getByLabelText(/Country/i));
    await user.click(screen.getByRole("button", { name: /Next/i }));

    // Skip without entering any team members.
    await user.click(await screen.findByRole("button", { name: /Skip for now/i }));

    await waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(1));
    const [, init] = fetchMock.mock.calls[0]!;
    const payload = JSON.parse(init.body as string);
    expect(payload.users).toHaveLength(0);
  });

  it("renders the missing-tenant-id error when the route param is empty", async () => {
    const qc = new QueryClient({
      defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
    });
    render(
      <QueryClientProvider client={qc}>
        <LocaleProvider>
          <MemoryRouter initialEntries={["/setup/"]}>
            <Routes>
              <Route path="/setup/:id?" element={<SetupWizardPage />} />
            </Routes>
          </MemoryRouter>
        </LocaleProvider>
      </QueryClientProvider>,
    );
    expect(
      await screen.findByText(/Missing tenant id in route/i),
    ).toBeInTheDocument();
    // The body of that error block must mention the expected route
    // pattern so an operator copying it into Slack has the fix in
    // the same message.
    expect(
      within(screen.getByRole("heading", { name: /Tenant Setup/i }).parentElement!).getByText(
        /\/setup\/:id/,
      ),
    ).toBeInTheDocument();
  });
});
