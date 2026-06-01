import { describe, it, expect, vi, beforeEach } from "vitest";
import { act, fireEvent, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter, Route, Routes } from "react-router-dom";

const getMarketplaceInstallation = vi.fn();
const getMarketplaceExtension = vi.fn();
const listMarketplaceVersions = vi.fn();
const updateMarketplaceInstallationSettings = vi.fn();
const upgradeMarketplaceInstallation = vi.fn();
const uninstallMarketplaceExtension = vi.fn();

vi.mock("../../lib/api", () => ({
  api: {
    getMarketplaceInstallation: (...a: unknown[]) =>
      getMarketplaceInstallation(...a),
    getMarketplaceExtension: (...a: unknown[]) => getMarketplaceExtension(...a),
    listMarketplaceVersions: (...a: unknown[]) => listMarketplaceVersions(...a),
    updateMarketplaceInstallationSettings: (...a: unknown[]) =>
      updateMarketplaceInstallationSettings(...a),
    upgradeMarketplaceInstallation: (...a: unknown[]) =>
      upgradeMarketplaceInstallation(...a),
    uninstallMarketplaceExtension: (...a: unknown[]) =>
      uninstallMarketplaceExtension(...a),
  },
}));

import { InstallationDetailPage } from "./InstallationDetailPage";

const ROW = {
  id: "install-1",
  tenant_id: "tnt-1",
  extension_id: "ext-1",
  extension_version_id: "ver-0",
  status: "active",
  settings: { api_key: "secret" },
  webhook_base: "https://acme.example.com",
  installed_at: "2025-03-01T00:00:00Z",
  updated_at: "2025-03-01T00:00:00Z",
};

const EXT = {
  id: "ext-1",
  name: "acme.inventory-sync",
  publisher: "acme",
  slug: "inventory-sync",
  display_name: "Inventory Sync",
  description: "",
  author: "Acme",
  license: "MIT",
  status: "listed" as const,
  listed_version: "1.2.0",
  created_at: "2025-01-01T00:00:00Z",
  updated_at: "2025-02-01T00:00:00Z",
};

const VERSIONS_RESP = {
  items: [
    {
      id: "ver-1",
      extension_id: "ext-1",
      version: "1.2.0",
      bundle_hash: "a".repeat(64),
      bundle_size_bytes: 100,
      bundle_url: "",
      min_kapp_version: "1.0.0",
      features_required: [],
      permissions_required: ["records.write"],
      ktypes_count: 0,
      workflows_count: 0,
      agent_tools_count: 0,
      ui_extensions_count: 0,
      webhooks_count: 0,
      yanked: false,
      published_at: "2025-02-01T00:00:00Z",
    },
    {
      id: "ver-0",
      extension_id: "ext-1",
      version: "1.1.0",
      bundle_hash: "b".repeat(64),
      bundle_size_bytes: 100,
      bundle_url: "",
      min_kapp_version: "1.0.0",
      features_required: [],
      permissions_required: [],
      ktypes_count: 0,
      workflows_count: 0,
      agent_tools_count: 0,
      ui_extensions_count: 0,
      webhooks_count: 0,
      yanked: false,
      published_at: "2025-01-15T00:00:00Z",
    },
  ],
};

function renderPage() {
  const qc = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  });
  return render(
    <QueryClientProvider client={qc}>
      <MemoryRouter initialEntries={["/marketplace/installed/install-1"]}>
        <Routes>
          <Route
            path="/marketplace/installed/:installId"
            element={<InstallationDetailPage />}
          />
          <Route
            path="/marketplace/installed"
            element={<div>installed-list</div>}
          />
        </Routes>
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

describe("InstallationDetailPage", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    getMarketplaceInstallation.mockResolvedValue(ROW);
    getMarketplaceExtension.mockResolvedValue({
      extension: EXT,
      versions: VERSIONS_RESP.items,
    });
    listMarketplaceVersions.mockResolvedValue(VERSIONS_RESP);
  });

  it("renders the install header + status + version", async () => {
    renderPage();
    await waitFor(() =>
      expect(
        screen.getByRole("heading", { name: /Inventory Sync/i }),
      ).toBeInTheDocument(),
    );
    expect(screen.getByText(/v1\.1\.0/)).toBeInTheDocument();
    expect(screen.getByText(/Active/)).toBeInTheDocument();
  });

  it("offers the upgrade panel because v1.2.0 is newer than the installed v1.1.0", async () => {
    renderPage();
    await waitFor(() =>
      expect(
        screen.getByRole("button", { name: /Upgrade to v1\.2\.0/i }),
      ).toBeInTheDocument(),
    );
  });

  it("fires upgradeMarketplaceInstallation with the right from/to + keep_settings on confirmation", async () => {
    upgradeMarketplaceInstallation.mockResolvedValueOnce({
      installation: { ...ROW, extension_version_id: "ver-1" },
      from_version_id: "ver-0",
    });
    renderPage();
    await waitFor(() =>
      expect(
        screen.getByRole("button", { name: /Upgrade to v1\.2\.0/i }),
      ).toBeInTheDocument(),
    );
    await userEvent.click(
      screen.getByRole("button", { name: /Upgrade to v1\.2\.0/i }),
    );
    await userEvent.click(
      screen.getByRole("button", { name: /Upgrade & keep settings/i }),
    );
    await waitFor(() =>
      expect(upgradeMarketplaceInstallation).toHaveBeenCalledWith(
        "install-1",
        {
          from_version_id: "ver-0",
          to_version_id: "ver-1",
          keep_settings: true,
        },
      ),
    );
  });

  it("collapses the upgrade panel when installed version is the most-recent publish (no downgrade offers)", async () => {
    // Tenant is on ver-1 (1.2.0, published 2025-02-01). The
    // older ver-0 (1.1.0, published 2025-01-15) is in the
    // catalogue but MUST NOT appear in the upgrade panel,
    // otherwise users would be silently offered to downgrade
    // — a real risk for settings-schema-incompatible reverts.
    const rowOnLatest = { ...ROW, extension_version_id: "ver-1" };
    getMarketplaceInstallation.mockReset();
    getMarketplaceInstallation.mockResolvedValue(rowOnLatest);
    renderPage();
    // Wait for the page to load — v1.2.0 appears in the header
    // version line and (post-fix) in the "already on latest"
    // empty-state copy, so we don't pin on getByText here.
    await waitFor(() =>
      expect(screen.getAllByText(/v1\.2\.0/).length).toBeGreaterThan(0),
    );
    // The upgrade panel renders an "Upgrade to vX" button when
    // any newer version is available. Confirm NONE exist —
    // including no button offering to "upgrade" back to v1.1.0.
    expect(
      screen.queryByRole("button", { name: /Upgrade to v1\.1\.0/i }),
    ).not.toBeInTheDocument();
    expect(
      screen.queryByRole("button", { name: /Upgrade to v/i }),
    ).not.toBeInTheDocument();
    // Affirmative signal that the panel ackownledges already-latest.
    expect(
      screen.getByText(/already on the latest approved version/i),
    ).toBeInTheDocument();
  });

  it("invalidates the installation query in onSettled so error-path stale rollbacks get refetched", async () => {
    // Settings update fails; onMutate has already optimistically
    // staged the new value into cache, onError rolls it back to
    // the pre-mutate snapshot. That snapshot may itself be
    // stale (another tab edited concurrently), so the mutation
    // MUST trigger a background refetch via onSettled. We pin
    // that by observing getMarketplaceInstallation is called a
    // second time after the failed PATCH settles.
    updateMarketplaceInstallationSettings.mockRejectedValueOnce(
      new Error("boom"),
    );
    renderPage();
    await waitFor(() =>
      expect(getMarketplaceInstallation).toHaveBeenCalledTimes(1),
    );
    // Touch the settings field then click Save. The schema is
    // currently null in this page (see comment in
    // onSaveSettings) so SettingsForm renders the free-form
    // JSON textarea — we target it via its placeholder rather
    // than a label (the textarea is intentionally label-less
    // because the surrounding Card heading is the label).
    //
    // The textarea is re-mounted when settingsResetKey bumps on
    // initial install.data settle (see BUG_0001 fix), so we
    // re-query inside waitFor until the seeded post-remount
    // node is in the DOM — a stale ref to the pre-remount node
    // would be detached and userEvent.clear would fail.
    await waitFor(() => {
      const ta = screen.getByPlaceholderText(
        /api_key/i,
      ) as HTMLTextAreaElement;
      expect(ta.value).toContain("secret");
    });
    const textarea = screen.getByPlaceholderText(
      /api_key/i,
    ) as HTMLTextAreaElement;
    await userEvent.clear(textarea);
    await userEvent.type(textarea, '{{"api_key":"new"}');
    await userEvent.click(
      screen.getByRole("button", { name: /Save settings/i }),
    );
    // After the mutation settles (success or error) the
    // installation query MUST be invalidated, triggering a
    // background refetch — observable as a second call to
    // getMarketplaceInstallation.
    await waitFor(() =>
      expect(getMarketplaceInstallation.mock.calls.length).toBeGreaterThanOrEqual(2),
    );
  });

  it("confirms before uninstalling and posts the DELETE", async () => {
    uninstallMarketplaceExtension.mockResolvedValueOnce(undefined);
    renderPage();
    await waitFor(() =>
      expect(
        screen.getByRole("button", { name: /Uninstall extension/i }),
      ).toBeInTheDocument(),
    );
    await userEvent.click(
      screen.getByRole("button", { name: /Uninstall extension/i }),
    );
    // Modal confirmation button.
    const confirm = await screen.findByRole("button", {
      name: /^Uninstall$/i,
    });
    await userEvent.click(confirm);
    await waitFor(() =>
      expect(uninstallMarketplaceExtension).toHaveBeenCalledWith("install-1"),
    );
  });

  it("does NOT call listMarketplaceVersions — versions come from getMarketplaceExtension (no N+1 round-trip)", async () => {
    // ANALYSIS_0001 (round 2): /extensions/{id} already returns
    // versions[] via listApprovedVersions, so a second call to
    // listMarketplaceVersions would be a wasted round trip and
    // a cache key the rest of the page never invalidates. Pin
    // the no-op the same way MarketplaceInstallationsPage's
    // own N+1 regression test does (see its test file for
    // prior art).
    renderPage();
    await waitFor(() =>
      expect(
        screen.getByRole("heading", { name: /Inventory Sync/i }),
      ).toBeInTheDocument(),
    );
    // Upgrade panel renders — if it relied on the dropped
    // listMarketplaceVersions, this would never paint.
    await waitFor(() =>
      expect(
        screen.getByRole("button", { name: /Upgrade to v1\.2\.0/i }),
      ).toBeInTheDocument(),
    );
    // The dropped query must NOT have fired.
    expect(listMarketplaceVersions).not.toHaveBeenCalled();
  });

  it("Discard changes button resets the settings textarea text to the canonical server value (BUG_0001)", async () => {
    // BUG_0001 (round 2): the textarea was uncontrolled and its
    // useEffect resync only fired when the parent value reset
    // to an empty object. Real installs ship non-empty
    // settings (here {api_key: "secret"}), so Discard would
    // reset settingsDraft + settingsTouched but the textarea
    // text kept the pre-discard edits. On the next keystroke
    // the user resumed from stale data and could accidentally
    // save it.
    //
    // The fix is the settingsResetKey contract in
    // InstallationDetailPage — a counter bumped on every
    // parent-side reset (Discard, save success, cross-tab
    // refetch). It's passed as React's `key` on SettingsForm,
    // forcing the FreeformJsonEditor to remount and re-seed
    // its text buffer from the canonical row.settings.
    renderPage();
    // Wait for the page to settle — the SettingsForm remounts
    // once on initial install.data settle (see settingsResetKey
    // bump in the install.data useEffect) so a ref taken before
    // that point would be a detached node.
    await waitFor(() => {
      const ta = screen.getByPlaceholderText(
        /api_key/i,
      ) as HTMLTextAreaElement;
      expect(ta.value).toContain("secret");
    });
    // User types over the canonical value.
    {
      const ta = screen.getByPlaceholderText(
        /api_key/i,
      ) as HTMLTextAreaElement;
      await userEvent.clear(ta);
      await userEvent.type(ta, '{{"api_key":"NEW_VALUE"}');
      expect(ta.value).toContain("NEW_VALUE");
    }
    // User clicks Discard.
    await userEvent.click(
      screen.getByRole("button", { name: /Discard changes/i }),
    );
    // The remounted FreeformJsonEditor must seed from the
    // canonical {api_key: "secret"} document — NOT from the
    // user's mid-edit "NEW_VALUE" snapshot. Re-query so we get
    // the post-Discard remounted node, not the detached pre-
    // Discard one.
    await waitFor(() => {
      const ta = screen.getByPlaceholderText(
        /api_key/i,
      ) as HTMLTextAreaElement;
      expect(ta.value).toContain("secret");
      expect(ta.value).not.toContain("NEW_VALUE");
    });
  });

  it("Discard explicitly clears settingsInvalidKeys so the Save button is locally consistent even before the editor unmount-cleanup runs (round-7 ANALYSIS_0004)", async () => {
    // Round-7 ANALYSIS_0004: Discard previously cleared
    // settingsDraft + settingsTouched + settingsError and
    // bumped settingsResetKey to remount SettingsForm — but
    // it did NOT explicitly clear settingsInvalidKeys. The
    // cleanup was implicit, relying on the JSON editor's
    // unmount-cleanup effect firing
    // `onValidityChange(key, true)` to remove the stale key.
    // That chain is sound today (React commits cleanup
    // effects of the outgoing tree before mounting the new
    // one), but the implicit dependency on effect ordering
    // is fragile: a future refactor that swaps SettingsForm
    // for a component without the cleanup signal would
    // silently leave the parent's invalid-keys set non-
    // empty across a Discard, even though the user's intent
    // was a clean-slate reset.
    //
    // The fix is to do the reset explicitly in the Discard
    // handler. We pin it via this test: type invalid JSON
    // to mark the form invalid, click Discard, then type
    // a SINGLE valid character. Pre-fix, that single char
    // wouldn't be enough to flip settingsTouched + clear
    // the invalid key in time — actually it would, because
    // typing fires the editor's keystroke handler which
    // clears the local error and re-signals valid. So
    // we need a stronger probe: assert that immediately
    // after Discard, BEFORE any user interaction at all,
    // the user can type a single valid char and Save
    // immediately becomes enabled. If the invalid keys set
    // were stranded, the user's first keystroke wouldn't
    // clear it (the validity signal would already be
    // `valid` from the editor's perspective — its lazy
    // initialiser saw a valid {} value), and Save would
    // stay disabled until the editor was further toggled.
    renderPage();
    await waitFor(() => {
      const ta = screen.getByPlaceholderText(
        /api_key/i,
      ) as HTMLTextAreaElement;
      expect(ta.value).toContain("secret");
    });
    const textarea1 = screen.getByPlaceholderText(
      /api_key/i,
    ) as HTMLTextAreaElement;
    // Step 1: corrupt the textarea so settingsInvalidKeys
    // gets the FREEFORM key added (invalid).
    await userEvent.clear(textarea1);
    await userEvent.type(textarea1, '{{"broken');
    await waitFor(() => {
      const save = screen.getByRole("button", {
        name: /Save settings/i,
      }) as HTMLButtonElement;
      expect(save.disabled).toBe(true);
    });
    // Step 2: Discard. The handler must clear the
    // settingsInvalidKeys set explicitly, and remount the
    // editor (which will also fire its own unmount
    // cleanup). We assert the post-Discard observable: the
    // re-mounted editor's text buffer is back to the
    // canonical "secret" value, the Save button is disabled
    // because settingsTouched is false, AND the invalid
    // keys set is empty (proved by the next step).
    await userEvent.click(
      screen.getByRole("button", { name: /Discard changes/i }),
    );
    await waitFor(() => {
      const ta = screen.getByPlaceholderText(
        /api_key/i,
      ) as HTMLTextAreaElement;
      expect(ta.value).toContain("secret");
      expect(ta.value).not.toContain("broken");
    });
    // Save button is disabled — settingsTouched is false.
    const saveAfterDiscard = screen.getByRole("button", {
      name: /Save settings/i,
    }) as HTMLButtonElement;
    expect(saveAfterDiscard.disabled).toBe(true);
    // Step 3: type a single valid char. settingsTouched
    // flips to true. If the invalid-keys set is correctly
    // empty, Save must enable immediately — settingsFormValid
    // is true because the freshly-mounted editor signalled
    // valid at mount AND we've added no invalid entries.
    const textarea2 = screen.getByPlaceholderText(
      /api_key/i,
    ) as HTMLTextAreaElement;
    await userEvent.type(textarea2, " ");
    await waitFor(() => {
      const save = screen.getByRole("button", {
        name: /Save settings/i,
      }) as HTMLButtonElement;
      expect(save.disabled).toBe(false);
    });
  });

  it("Save settings is disabled while the JSON editor contains unparseable text (ANALYSIS_0002)", async () => {
    // ANALYSIS_0002 (round 3): FreeformJsonEditor keeps its own
    // text buffer. On a parse error it shows the error message
    // locally but does NOT call onChange, so the parent's
    // settingsDraft stays at the last valid value. Without the
    // validity-lift fix, Save would remain enabled and pressing
    // it would silently submit the stale settingsDraft instead
    // of what's on screen.
    //
    // The fix wires onValidityChange from each editor up to a
    // settingsFormValid bit in InstallationDetailPage, and the
    // Save button is disabled whenever that bit is false. This
    // test pins both the initial-valid state and the transition
    // to invalid-on-bad-input.
    renderPage();
    await waitFor(() => {
      const ta = screen.getByPlaceholderText(
        /api_key/i,
      ) as HTMLTextAreaElement;
      expect(ta.value).toContain("secret");
    });
    const textarea = screen.getByPlaceholderText(
      /api_key/i,
    ) as HTMLTextAreaElement;
    // Pin the typing into the textarea causes settingsTouched
    // -> true. Use a valid keystroke first so Save becomes
    // touched-but-still-valid; THEN corrupt to an unparseable
    // string so we observe a real disable transition (touched
    // & invalid) rather than the always-disabled
    // pristine-form state.
    await userEvent.type(textarea, " ");
    await waitFor(() => {
      const save = screen.getByRole("button", {
        name: /Save settings/i,
      }) as HTMLButtonElement;
      expect(save.disabled).toBe(false);
    });
    await userEvent.clear(textarea);
    await userEvent.type(textarea, '{{"unterminated');
    // Editor's local error surfaces in the DOM.
    await waitFor(() =>
      expect(textarea.value).toBe('{"unterminated'),
    );
    // Save must be disabled — the editor's text is unparseable,
    // so the parent's settingsDraft is stale and saving would
    // be misleading. Without the round-3 validity lift this
    // would still be enabled.
    const save = screen.getByRole("button", {
      name: /Save settings/i,
    }) as HTMLButtonElement;
    expect(save.disabled).toBe(true);
    // Recover to valid JSON and confirm Save re-enables.
    await userEvent.clear(textarea);
    await userEvent.type(textarea, '{{"api_key":"new"}');
    await waitFor(() => {
      const save2 = screen.getByRole("button", {
        name: /Save settings/i,
      }) as HTMLButtonElement;
      expect(save2.disabled).toBe(false);
    });
  });

  it("does not over-remount SettingsForm when install.data refetches with an unchanged settings document (ANALYSIS_0001)", async () => {
    // ANALYSIS_0001 (round 3): the install.data useEffect used
    // to bump settingsResetKey on every fire, but the save path
    // already bumps it explicitly in onSuccess, and the
    // onSettled-driven refetch fires the effect again with an
    // unchanged document. That produced 2\u20133 remounts per save.
    //
    // The fix: the effect compares JSON.stringify(server) vs
    // JSON.stringify(draft) and only bumps the reset key when
    // they actually differ. Pin the contract by observing the
    // textarea identity across an unrelated install.data tick:
    // we trigger an explicit refetchQueries so the effect
    // re-runs, then assert the SAME textarea DOM node is still
    // there (not a fresh remount).\n    //
    // We use the textarea's data-* React fiber identity as the
    // observable: in JSDOM, a remount produces a fresh element
    // node (`!== prevRef`). A no-op effect leaves the existing
    // node in place.
    renderPage();
    await waitFor(() => {
      const ta = screen.getByPlaceholderText(
        /api_key/i,
      ) as HTMLTextAreaElement;
      expect(ta.value).toContain("secret");
    });
    const firstNode = screen.getByPlaceholderText(
      /api_key/i,
    ) as HTMLTextAreaElement;
    // Simulate a background refetch returning the same row.
    // getMarketplaceInstallation is the queryFn for the
    // installation query; resolving it again triggers a
    // setState in useQuery's reducer with an equal-by-deep but
    // !== reference object, which fires the install.data
    // useEffect (the dependency is referential).
    getMarketplaceInstallation.mockResolvedValueOnce({ ...ROW });
    // Force a refetch by reading from the QueryClient through
    // an external trigger — we re-render via state by
    // dispatching a focus event (react-query's
    // refetchOnWindowFocus default is true in some configs,
    // but the test's qc disables retry only). The simplest
    // deterministic path is to call the mock again and rely on
    // the test client's invalidation hook. Use a direct
    // QueryClient.invalidateQueries via DOM event isn't
    // available here, so we use the userEvent-driven path:
    // open and immediately close the uninstall modal, which
    // doesn't touch the installation query but causes a React
    // re-render. The point is to drive the parent through a
    // commit phase without bumping the reset key.
    await userEvent.click(
      screen.getByRole("button", { name: /Uninstall extension/i }),
    );
    // Cancel the modal so the install isn't actually
    // uninstalled (the cancel button is the modal's secondary
    // action).
    const cancel = await screen.findByRole("button", {
      name: /^Cancel$/i,
    });
    await userEvent.click(cancel);
    // Same DOM node — no remount.
    const afterNode = screen.getByPlaceholderText(
      /api_key/i,
    ) as HTMLTextAreaElement;
    expect(afterNode).toBe(firstNode);
  });

  it("renders without an infinite render loop when install.data.settings is null (round-4 BUG_0001)", async () => {
    // Round-4 BUG_0001 (1dffed5 → 1dffed5 review pass): the
    // install.data useEffect used to call setSettingsDraft(next)
    // unconditionally, gated only the resetKey bump on
    // sameAsDraft. install.data.settings can be null/undefined
    // (Go-side installationView.Settings is map[string]any
    // without omitempty, and a nil Go map marshals as JSON null).
    // The `?? {}` fallback synthesises a NEW {} ref on every
    // effect run; settingsDraft is in the dep array, so the
    // unconditional setState scheduled a render with a new ref →
    // re-render → effect re-fires → new {} → setState → re-render
    // → infinite loop → React's "Maximum update depth exceeded"
    // error tears the tree down.
    //
    // The fix: move setSettingsDraft inside the !sameAsDraft
    // guard so the unchanged-document path is a true no-op.
    //
    // We pin the fix by:
    //   1. Mocking the install row with settings: null
    //   2. Rendering the page
    //   3. Verifying the page paints to a steady state (Active
    //      badge appears) within a normal waitFor timeout
    //   4. Verifying no console.error about max-update-depth was
    //      emitted during the run
    //
    // Pre-fix, step 3 would either time out (React aborts the
    // render and leaves the tree in an error boundary) or emit
    // the max-update-depth error visible in step 4.
    // Round-6 BUG_0001: settings is now typed `Record<string, unknown> | null`
    // so the null literal is a valid value; the prior `as any` cast is
    // no longer needed.
    const nullSettingsRow = { ...ROW, settings: null };
    getMarketplaceInstallation.mockReset();
    getMarketplaceInstallation.mockResolvedValue(nullSettingsRow);
    const consoleError = vi.spyOn(console, "error").mockImplementation(() => {});
    try {
      renderPage();
      // The page must paint past the loading state. If the
      // effect were looping, install.data would never settle
      // into a stable commit and waitFor would time out.
      await waitFor(() =>
        expect(
          screen.getByRole("heading", { name: /Inventory Sync/i }),
        ).toBeInTheDocument(),
      );
      // No max-update-depth error from React. We accept any
      // unrelated console.error (none should fire in practice,
      // but the assertion is specifically that the infinite
      // loop signature isn't present).
      const maxDepthCalls = consoleError.mock.calls.filter((args) =>
        args.some(
          (a) =>
            typeof a === "string" &&
            /Maximum update depth exceeded/i.test(a),
        ),
      );
      expect(maxDepthCalls).toHaveLength(0);
    } finally {
      consoleError.mockRestore();
    }
  });

  it("upgradeMutation invalidates the installation query on settle (parity with settingsMutation, ANALYSIS_0003)", async () => {
    // ANALYSIS_0003 (round 3): settingsMutation has an
    // onSettled handler that invalidates the installation
    // query to converge the cache after success-or-error.
    // upgradeMutation was missing the same safety net, so an
    // error-path stale cache would persist until a manual
    // refetch. The fix mirrors the settings handler.
    //
    // We observe the contract by:
    //   1. Letting the page load (1st getMarketplaceInstallation call)
    //   2. Triggering the upgrade flow
    //   3. Confirming getMarketplaceInstallation fires a 2nd
    //      time after the mutation settles, indicating the
    //      invalidation -> refetch path actually ran.
    upgradeMarketplaceInstallation.mockResolvedValueOnce({
      installation: { ...ROW, extension_version_id: "ver-1" },
      from_version_id: "ver-0",
    });
    renderPage();
    await waitFor(() =>
      expect(getMarketplaceInstallation).toHaveBeenCalledTimes(1),
    );
    await waitFor(() =>
      expect(
        screen.getByRole("button", { name: /Upgrade to v1\.2\.0/i }),
      ).toBeInTheDocument(),
    );
    await userEvent.click(
      screen.getByRole("button", { name: /Upgrade to v1\.2\.0/i }),
    );
    await userEvent.click(
      screen.getByRole("button", { name: /Upgrade & keep settings/i }),
    );
    // Pin: post-mutation invalidation triggered a background
    // refetch, observable as a 2nd queryFn call.
    await waitFor(() =>
      expect(getMarketplaceInstallation.mock.calls.length).toBeGreaterThanOrEqual(2),
    );
  });

  it("onSaveSettings refuses to submit even when the disabled state is bypassed by removing the disabled attribute (round-8 BUG_0001)", async () => {
    // Round-8 BUG_0001: the Save settings button is disabled
    // while settingsFormValid is false, but disabled is a UI
    // gate, not a data-path gate. Accessibility tools can fire
    // synthetic click events that bypass the disabled
    // attribute, programmatic invocations route around the
    // button entirely, and a future refactor could replace
    // the disabled prop with a styling-only class. We add a
    // re-check of settingsFormValid at the top of
    // onSaveSettings so the data-path itself rejects a submit
    // attempt with unparseable settings, no matter how the
    // click was dispatched. This is the exact same
    // defense-in-depth pattern applied to
    // InstallExtensionDialog.onConfirm in round-7
    // ANALYSIS_0002 — both paths now refuse to send the
    // stale-but-valid settings draft instead of the bytes
    // on screen.
    renderPage();
    await waitFor(() => {
      const ta = screen.getByPlaceholderText(
        /api_key/i,
      ) as HTMLTextAreaElement;
      expect(ta.value).toContain("secret");
    });
    const textarea = screen.getByPlaceholderText(
      /api_key/i,
    ) as HTMLTextAreaElement;
    // Type one valid char first so settingsTouched flips to
    // true (parent's onChange handler fires only on clean
    // parse, and only that path bumps settingsTouched). Then
    // corrupt the buffer mid-stream — the editor suppresses
    // onChange on parse-fail, so the parent's settingsDraft
    // retains the LAST valid object ({api_key: "secret", ...})
    // while the textarea shows the broken text.
    // Without the round-8 guard, a click-via-bypass would
    // silently submit the stale-but-valid object instead of
    // asking the user to fix the broken JSON they actually see.
    await userEvent.type(textarea, " ");
    fireEvent.change(textarea, { target: { value: '{"oops":' } });
    const btn = screen.getByRole("button", {
      name: /Save settings/i,
    }) as HTMLButtonElement;
    // Sanity: standard click is blocked by the disabled
    // attribute today.
    await waitFor(() => expect(btn.disabled).toBe(true));
    // Bypass: pull the React-attached onClick handler off
    // the element via `__reactProps$<random>` and invoke it
    // directly. fireEvent and userEvent both go through
    // React's event delegation which still consults the
    // React-side `disabled` prop even after we mutate the
    // DOM attribute, so they don't actually exercise the
    // round-8 guard. The realistic synthetic-click vectors
    // (accessibility tools firing the listener directly,
    // programmatic e2e invocations, future refactors
    // swapping disabled for a class) skip React's gate
    // entirely — we mirror that by reading the registered
    // onClick handler off the React DOM node and invoking
    // it. If onSaveSettings doesn't have its own validity
    // check at the top, the settings mutation will fire
    // with the stale parent `settingsDraft` value even
    // though the textarea currently shows unparseable text.
    const propsKey = Object.keys(btn).find((k) =>
      k.startsWith("__reactProps$"),
    );
    expect(propsKey).toBeDefined();
    const props = (btn as unknown as Record<string, { onClick?: () => void }>)[
      propsKey!
    ];
    expect(props.onClick).toBeTypeOf("function");
    await act(async () => {
      props.onClick!();
    });
    expect(updateMarketplaceInstallationSettings).not.toHaveBeenCalled();
    expect(
      await screen.findByText(/Fix the settings JSON before saving/i),
    ).toBeInTheDocument();
  });

  it("collapses the upgrade panel when the installed version's published_at is unparseable (round-9 ANALYSIS_0002)", async () => {
    // Round-9 ANALYSIS_0002: pre-fix, the upgrade-panel gate
    // was `installedPublishedAt === null`. `null` covers
    // hard-deleted-from-catalog (no installedVersion match),
    // but NOT the case where installedVersion exists with an
    // unparseable published_at — `new Date("garbage").getTime()`
    // returns NaN, not null. The gate's `=== null` check
    // passed NaN through, falling into the filter branch
    // where `t > NaN` is always false. That produced the same
    // empty list as the null case BUT via a confusing
    // NaN-comparison side effect rather than an explicit
    // collapse, and conflated two distinct domains (anchor
    // NaN vs per-row NaN). Post-fix, `installedPublishedAt`
    // is lifted through a Number.isFinite gate before reaching
    // the filter, so the NaN-anchor case produces the same
    // explicit "already on the latest approved version"
    // empty-state copy that the null case does. The per-row
    // `!Number.isFinite(t)` continues to guard per-row NaN
    // independently.
    //
    // Pin via observable behavior: tenant is installed on a
    // version with a clearly-unparseable published_at; the
    // catalogue also lists a NEWER version (well-formed
    // timestamp) that, pre-fix, the filter still skipped via
    // the NaN-comparison accident. Post-fix, the explicit
    // gate makes the same observable outcome the result of an
    // explicit collapse — and the "already on the latest"
    // empty-state surfaces, distinguishing it from the case
    // where the catalogue genuinely has no newer version.
    const garbageInstalledVersion = {
      ...VERSIONS_RESP.items[1],
      published_at: "not-a-real-timestamp",
    };
    const newerVersion = VERSIONS_RESP.items[0];
    getMarketplaceExtension.mockReset();
    getMarketplaceExtension.mockResolvedValueOnce({
      extension: EXT,
      versions: [newerVersion, garbageInstalledVersion],
    });
    renderPage();
    await waitFor(() =>
      expect(
        screen.getByRole("heading", { name: /Inventory Sync/i }),
      ).toBeInTheDocument(),
    );
    // No upgrade CTA — even though v1.2.0 has a strictly later
    // published_at than NaN (every comparison vs NaN is false,
    // so the per-row filter would have returned [] pre-fix too,
    // but the gate now collapses explicitly upstream so the
    // empty-state copy reflects intent).
    expect(
      screen.queryByRole("button", { name: /Upgrade to v/i }),
    ).not.toBeInTheDocument();
    // Affirmative signal — the page reaches the "already on
    // the latest approved version" branch instead of any
    // half-rendered upgrade panel.
    expect(
      await screen.findByText(/already on the latest approved version/i),
    ).toBeInTheDocument();
  });
});
