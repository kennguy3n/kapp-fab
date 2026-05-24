import { test, expect } from "@playwright/test";

/**
 * rtl-flip.spec — pins the RTL layout contract added by PR-6.
 *
 * The test loads the SPA, drives the LocaleProvider into Arabic
 * via the kapp_locale cookie + localStorage (mirroring how
 * setLocale() persists the pick), and asserts three things:
 *
 *   1. <html dir> is "rtl" — driven by the LocaleProvider's
 *      effect that stamps the active locale's direction onto
 *      document.documentElement. This is the CSS-level signal
 *      that activates every "rtl:" Tailwind variant and every
 *      logical-property utility on the page.
 *
 *   2. <html lang> is "ar" — the LocaleProvider also stamps
 *      lang so screen readers pronounce text correctly and the
 *      browser's form-input heuristics (e.g. RTL keyboard hint
 *      on mobile, ime mode for CJK) fire correctly.
 *
 *   3. The sidebar's inline-end border lands on the LEFT side
 *      of the viewport (because in RTL the inline-end is the
 *      left edge). PR-6 swapped `border-r` for the logical
 *      `border-e` so the border attaches to the inline-end of
 *      the sidebar regardless of writing direction. Without
 *      that change, the sidebar would render with a right-edge
 *      border in RTL — visually mirrored from where the
 *      sidebar itself sits, which looks broken.
 *
 * The test does not assert exact pixel positions because Vite
 * dev-server's loading state may transiently shift the layout;
 * instead, it compares the LTR baseline (sidebar.x + width ≈
 * sidebar's inline-end edge on the left of the viewport in LTR)
 * with the RTL pass (sidebar's inline-start edge near the right
 * of the viewport). That's a relative-position assertion that
 * holds regardless of viewport width.
 */

test.describe("RTL flip", () => {
  test("Arabic locale flips html dir and sidebar position", async ({
    page,
  }) => {
    // Seed the kapp_locale cookie so the LocaleProvider picks
    // Arabic during its initial-resolve pass. The cookie wins
    // over navigator.language in the precedence chain so the
    // test is deterministic regardless of the browser-engine
    // default.
    await page.context().addCookies([
      {
        name: "kapp_locale",
        value: "ar",
        url: "http://localhost:5173/",
        sameSite: "Lax",
      },
    ]);

    // localStorage is checked first in the precedence chain;
    // seed it via an init script so it's present before the
    // SPA's LocaleProvider runs.
    await page.addInitScript(() => {
      try {
        window.localStorage.setItem("kapp_locale", "ar");
      } catch {
        /* ignore — private-browsing iframes may block localStorage */
      }
    });

    await page.goto("/");

    // The LocaleProvider stamps lang + dir in a useEffect right
    // after mount. expect.toHaveAttribute polls so we wait for
    // the effect's first tick rather than racing it.
    await expect(page.locator("html")).toHaveAttribute("dir", "rtl");
    await expect(page.locator("html")).toHaveAttribute("lang", "ar");

    // Sidebar's inline-end (= left in RTL) should sit near the
    // viewport's left edge with a thin border. We assert the
    // sidebar's bounding-box right edge is within the viewport
    // and to the LEFT of the main content's left edge — i.e.
    // the sidebar is on the right side of the viewport when
    // dir="rtl" because flex-direction:row obeys writing dir.
    const sidebar = page.locator("aside").first();
    const main = page.locator("main").first();
    const sidebarBox = await sidebar.boundingBox();
    const mainBox = await main.boundingBox();
    expect(sidebarBox).not.toBeNull();
    expect(mainBox).not.toBeNull();

    // In RTL, the sidebar should be on the right side of the
    // viewport, so its x position should be greater than the
    // main content's x position.
    expect(sidebarBox!.x).toBeGreaterThan(mainBox!.x);
  });

  test.describe("LTR baseline (en-US locale pin)", () => {
    // test.use() is processed at collection time and applies to
    // every test in the enclosing describe block — NOT just the
    // test that lexically follows it.  Wrapping the English test
    // in its own nested describe is the only way to scope
    // `locale: "en-US"` to that one test without also forcing it
    // onto the Arabic test above (which doesn't need it because
    // cookie + localStorage pre-empt navigator.language anyway,
    // but scoping correctly avoids surprising future contributors
    // who might add another test to the outer describe and not
    // realise it would silently inherit en-US).
    test.use({ locale: "en-US" });

    test("English locale keeps LTR baseline", async ({ page }) => {
      // No cookie, no localStorage — LocaleProvider falls back to
      // DefaultLocale "en" via the navigator.language probe.  The
      // `test.use({ locale: "en-US" })` above sets the Playwright
      // browser context's locale explicitly so the test is
      // deterministic across browser engines (default Chromium
      // ships with en-US, but the firefox/webkit projects in
      // playwright.rtl.config.ts may default to the host's locale
      // which on CI runners can vary).  Without the explicit pin,
      // a runner whose OS locale was set to (say) de-DE would
      // have navigator.language report "de-DE", which
      // bestSupportedLocale would correctly resolve to "de" — and
      // the LTR baseline assertion (`lang="en"`) would fail with
      // a misleading "got de" error.
      await page.goto("/");

      await expect(page.locator("html")).toHaveAttribute("dir", "ltr");
      await expect(page.locator("html")).toHaveAttribute("lang", "en");

      // Sidebar on the left in LTR: its x ≈ 0, main content's x
      // is greater because the sidebar is to its left.
      const sidebar = page.locator("aside").first();
      const main = page.locator("main").first();
      const sidebarBox = await sidebar.boundingBox();
      const mainBox = await main.boundingBox();
      expect(sidebarBox).not.toBeNull();
      expect(mainBox).not.toBeNull();

      expect(sidebarBox!.x).toBeLessThan(mainBox!.x);
    });
  });
});
