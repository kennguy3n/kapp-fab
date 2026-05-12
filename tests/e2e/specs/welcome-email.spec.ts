/**
 * TC034 – TC037: Welcome email flow after account creation.
 *
 * These tests verify the full lifecycle of the welcome email that is
 * sent when a new organisation account is created:
 *
 *   TC034 – Welcome email is received after account creation
 *   TC035 – Welcome email contains a valid .ovpn download link
 *   TC036 – Downloaded .ovpn file contains correct server config
 *   TC037 – Welcome email body matches design specification
 *
 * Prerequisites:
 *   • Mailosaur API key & server ID (env: MAILOSAUR_API_KEY, MAILOSAUR_SERVER_ID)
 *   • A running instance of the app (env: APP_BASE_URL) or the API (env: API_BASE_URL)
 *
 * Customisation points (all env-driven):
 *   EXPECTED_OVPN_SERVER_HOST – expected `remote` hostname in the .ovpn
 *   EXPECTED_OVPN_SERVER_PORT – expected port (default "1194")
 *   EXPECTED_OVPN_PROTO       – expected protocol (default "udp")
 */

import { test, expect } from "../fixtures/auth.fixture";
import {
  generateEmailAddress,
  waitForEmail,
  findLink,
  getHtmlBody,
  getTextBody,
  purgeInbox,
} from "../helpers/mailosaur";
import { downloadFile } from "../helpers/download";
import { parseOvpnConfig } from "../helpers/ovpn-parser";
import type { Message } from "mailosaur/lib/models";

// ---------------------------------------------------------------------------
// Config from env
// ---------------------------------------------------------------------------

const EXPECTED_HOST = process.env.EXPECTED_OVPN_SERVER_HOST ?? "";
const EXPECTED_PORT = process.env.EXPECTED_OVPN_SERVER_PORT ?? "1194";
const EXPECTED_PROTO = process.env.EXPECTED_OVPN_PROTO ?? "udp";

// ---------------------------------------------------------------------------
// Shared state across ordered tests
// ---------------------------------------------------------------------------

let welcomeEmail: Message;
let ovpnDownloadUrl: string;
let testEmail: string;

// ---------------------------------------------------------------------------
// Hooks
// ---------------------------------------------------------------------------

test.describe.serial("Welcome email (TC034–TC037)", () => {
  test.beforeAll(async () => {
    await purgeInbox();
    testEmail = generateEmailAddress("welcome");
  });

  // -------------------------------------------------------------------------
  // TC034 – Welcome email is received after account creation
  // -------------------------------------------------------------------------
  test("TC034 – Welcome email is received after account creation", async ({
    apiContext,
  }) => {
    // Step 1: Create a new account using the Mailosaur email address.
    const registrationPayload = {
      organization: {
        company_name: `E2E Test Org ${Date.now()}`,
        company_size: "1-10",
        country: "VN",
      },
      user: {
        email: testEmail,
        password: "TestP@ss1234!",
        first_name: "E2E",
        last_name: "Tester",
      },
    };

    const registerResponse = await apiContext.post("/v1/register", {
      data: registrationPayload,
    });

    // Allow both 200 and 201 – backend may use either.
    expect([200, 201]).toContain(registerResponse.status());

    // Step 2: Wait for the welcome email to arrive.
    welcomeEmail = await waitForEmail({
      sentTo: testEmail,
      subject: "Welcome",
      timeout: 60_000,
    });

    // Step 3: Assertions
    expect(welcomeEmail).toBeTruthy();
    expect(welcomeEmail.subject).toBeTruthy();
    expect(welcomeEmail.subject!.toLowerCase()).toContain("welcome");
    expect(welcomeEmail.to).toBeTruthy();
    expect(welcomeEmail.to![0].email).toBe(testEmail);
  });

  // -------------------------------------------------------------------------
  // TC035 – Welcome email contains a valid .ovpn download link
  // -------------------------------------------------------------------------
  test("TC035 – Welcome email contains a valid .ovpn download link", async () => {
    expect(welcomeEmail).toBeTruthy();

    // Look for a link whose href contains ".ovpn" or "download" or "config".
    const ovpnLink =
      findLink(welcomeEmail, /\.ovpn/) ??
      findLink(welcomeEmail, /download.*config/i) ??
      findLink(welcomeEmail, /config.*download/i);

    expect(ovpnLink).toBeTruthy();
    expect(ovpnLink!.href).toBeTruthy();

    // Validate it is an absolute URL with a scheme.
    const url = new URL(ovpnLink!.href!);
    expect(["http:", "https:"]).toContain(url.protocol);

    ovpnDownloadUrl = ovpnLink!.href!;
  });

  // -------------------------------------------------------------------------
  // TC036 – Downloaded .ovpn file contains correct server config
  // -------------------------------------------------------------------------
  test("TC036 – Downloaded .ovpn file contains correct server config", async ({
    apiContext,
  }) => {
    expect(ovpnDownloadUrl).toBeTruthy();

    const download = await downloadFile(apiContext, ovpnDownloadUrl);

    // The server should return 200 and a non-empty body.
    expect(download.status).toBe(200);
    expect(download.text.length).toBeGreaterThan(0);

    // Parse the .ovpn content.
    const config = parseOvpnConfig(download.text);

    // Must have a `remote` directive.
    expect(config.remote).toBeTruthy();
    expect(config.serverHost).toBeTruthy();

    // If env expectations are provided, validate them.
    if (EXPECTED_HOST) {
      expect(config.serverHost).toBe(EXPECTED_HOST);
    }
    expect(config.serverPort).toBe(EXPECTED_PORT);
    expect(config.proto).toBe(EXPECTED_PROTO);

    // Structural checks – every valid .ovpn should have these.
    expect(config.dev).toBeTruthy();
    expect(config.hasCaCert).toBe(true);
  });

  // -------------------------------------------------------------------------
  // TC037 – Welcome email body matches design specification
  // -------------------------------------------------------------------------
  test("TC037 – Welcome email body matches design specification", async () => {
    expect(welcomeEmail).toBeTruthy();

    const html = getHtmlBody(welcomeEmail);
    const text = getTextBody(welcomeEmail);

    // The email should contain the recipient name or email.
    const bodyLower = (html || text).toLowerCase();
    expect(
      bodyLower.includes("e2e") || bodyLower.includes(testEmail)
    ).toBe(true);

    // Required design elements (adjust patterns to match your actual template).
    const requiredElements = [
      // Company branding / product name
      /kinshield|shieldnet|sn360/i,
      // A call-to-action link (at least one link in the HTML)
      /<a\s/i,
      // The .ovpn or download reference
      /download|\.ovpn|configuration|config/i,
    ];

    for (const pattern of requiredElements) {
      expect(
        pattern.test(html) || pattern.test(text),
        `Email body should match: ${pattern}`
      ).toBe(true);
    }

    // Check the email is not empty / template-only.
    expect(text.length).toBeGreaterThan(50);

    // Verify no raw template placeholders leaked (e.g. {{.Name}}).
    expect(html).not.toMatch(/\{\{\.?\w+\}\}/);
    expect(text).not.toMatch(/\{\{\.?\w+\}\}/);
  });
});
