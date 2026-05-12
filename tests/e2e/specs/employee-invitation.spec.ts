/**
 * TC083 – TC087: Employee invitation email flow.
 *
 * These tests verify the full lifecycle of the invitation email that
 * is sent when an admin invites an employee to the organisation:
 *
 *   TC083 – Employee receives invitation email in their inbox
 *   TC084 – Invitation email contains the correct installation link
 *   TC085 – Invitation link is unique per employee
 *   TC086 – Expired invitation link shows an error page
 *   TC087 – Resending invite sends a new unique link
 *
 * Prerequisites:
 *   • Mailosaur API key & server ID (env: MAILOSAUR_API_KEY, MAILOSAUR_SERVER_ID)
 *   • Admin auth (env: AUTH_TOKEN or ADMIN_EMAIL + ADMIN_PASSWORD)
 *   • API endpoint (env: API_BASE_URL)
 *
 * Customisation:
 *   INVITE_LINK_PATTERN – regex pattern to identify the installation link
 *                         in the email (default: /install|setup|join|accept/i)
 *   INVITATION_EXPIRY_SECONDS – if the backend supports a time-travel
 *                               API for testing, set the expiry window
 */

import { test, expect, APP_BASE_URL } from "../fixtures/auth.fixture";
import {
  generateEmailAddress,
  waitForEmail,
  findLink,
  extractLinks,
  getHtmlBody,
  getTextBody,
  purgeInbox,
} from "../helpers/mailosaur";
import type { Message } from "mailosaur/lib/models";

// ---------------------------------------------------------------------------
// Config
// ---------------------------------------------------------------------------

const INVITE_LINK_PATTERN = process.env.INVITE_LINK_PATTERN
  ? new RegExp(process.env.INVITE_LINK_PATTERN, "i")
  : /install|setup|join|accept|invite/i;

// ---------------------------------------------------------------------------
// Shared state
// ---------------------------------------------------------------------------

let employeeAEmail: string;
let employeeBEmail: string;
let invitationEmailA: Message;
let invitationEmailB: Message;
let inviteLinkA: string;
let inviteLinkB: string;

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

/**
 * Send an employee invitation via the API.
 */
async function sendInvitation(
  apiContext: import("@playwright/test").APIRequestContext,
  email: string,
  role = "member"
) {
  const response = await apiContext.post("/v1/organizations/members", {
    data: {
      members: [{ email, role }],
    },
  });

  // Accept 200 / 201 / 207 (multi-status for batch).
  expect([200, 201, 207]).toContain(response.status());
  return response;
}

/**
 * Extract the primary invitation / installation link from the email.
 */
function extractInviteLink(message: Message): string {
  const link = findLink(message, INVITE_LINK_PATTERN);
  expect(link, "Invitation email must contain an invite link").toBeTruthy();
  expect(link!.href).toBeTruthy();
  return link!.href!;
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

test.describe.serial("Employee invitation email (TC083–TC087)", () => {
  test.beforeAll(async () => {
    await purgeInbox();
    employeeAEmail = generateEmailAddress("emp-a");
    employeeBEmail = generateEmailAddress("emp-b");
  });

  // -------------------------------------------------------------------------
  // TC083 – Employee receives invitation email in their inbox
  // -------------------------------------------------------------------------
  test("TC083 – Employee receives invitation email in their inbox", async ({
    apiContext,
  }) => {
    // Send invitation to employee A.
    await sendInvitation(apiContext, employeeAEmail);

    // Wait for the invitation email.
    invitationEmailA = await waitForEmail({
      sentTo: employeeAEmail,
      subject: "invite",
      timeout: 60_000,
    });

    expect(invitationEmailA).toBeTruthy();
    expect(invitationEmailA.subject).toBeTruthy();
    expect(invitationEmailA.to).toBeTruthy();
    expect(invitationEmailA.to![0].email).toBe(employeeAEmail);

    // Verify the email body is non-empty.
    const body = getTextBody(invitationEmailA);
    expect(body.length).toBeGreaterThan(20);
  });

  // -------------------------------------------------------------------------
  // TC084 – Invitation email contains the correct installation link
  // -------------------------------------------------------------------------
  test("TC084 – Invitation email contains the correct installation link", async () => {
    expect(invitationEmailA).toBeTruthy();

    inviteLinkA = extractInviteLink(invitationEmailA);

    // The link should be an absolute URL.
    const url = new URL(inviteLinkA);
    expect(["http:", "https:"]).toContain(url.protocol);

    // The link should point to the application domain (not a third-party).
    const appHost = new URL(APP_BASE_URL).hostname;
    // Allow both the exact host and subdomains.
    expect(
      url.hostname === appHost || url.hostname.endsWith(`.${appHost}`)
    ).toBe(true);
  });

  // -------------------------------------------------------------------------
  // TC085 – Invitation link is unique per employee
  // -------------------------------------------------------------------------
  test("TC085 – Invitation link is unique per employee", async ({
    apiContext,
  }) => {
    // Send a second invitation to employee B.
    await sendInvitation(apiContext, employeeBEmail);

    invitationEmailB = await waitForEmail({
      sentTo: employeeBEmail,
      subject: "invite",
      timeout: 60_000,
    });

    inviteLinkB = extractInviteLink(invitationEmailB);

    // The two links must be different (unique tokens).
    expect(inviteLinkA).not.toBe(inviteLinkB);

    // Even though the domain is the same, the path/query (token) should differ.
    const urlA = new URL(inviteLinkA);
    const urlB = new URL(inviteLinkB);
    expect(urlA.pathname + urlA.search).not.toBe(
      urlB.pathname + urlB.search
    );
  });

  // -------------------------------------------------------------------------
  // TC086 – Expired invitation link shows an error page
  // -------------------------------------------------------------------------
  test("TC086 – Expired invitation link shows an error page", async ({
    apiContext,
    page,
  }) => {
    expect(inviteLinkA).toBeTruthy();

    // Strategy 1: Call the time-travel / expire API if available.
    // This is the preferred approach; the backend should expose an internal
    // test endpoint that force-expires an invitation token.
    const expireResponse = await apiContext
      .post("/v1/internal/test/expire-invitation", {
        data: { link: inviteLinkA },
      })
      .catch(() => null);

    if (!expireResponse || expireResponse.status() === 404) {
      // Strategy 2: If no time-travel API exists, we try to manipulate
      // the token directly. Append an obviously-wrong segment so the
      // backend rejects it as invalid/expired.
      //
      // NOTE: This is a fallback. For a production-grade setup, the
      // backend should expose a test helper. You can also manipulate
      // the DB directly:
      //   UPDATE invitations SET expires_at = NOW() - INTERVAL '1 day'
      //   WHERE token = '<token>';
      test.info().annotations.push({
        type: "info",
        description:
          "No time-travel API found; using tampered link to simulate expiry.",
      });

      // Navigate to a clearly-invalid variant of the invitation link.
      const expiredUrl = inviteLinkA.replace(
        /([?&]token=)[^&]+/,
        "$1expired-token-e2e"
      );
      await page.goto(expiredUrl.includes("expired-token-e2e") ? expiredUrl : `${inviteLinkA}__expired`);
    } else {
      expect(expireResponse.status()).toBeLessThan(400);
      await page.goto(inviteLinkA);
    }

    // The page should show an error / expiry message.
    await page.waitForLoadState("domcontentloaded");
    const bodyText = await page.textContent("body");
    const hasErrorMessage =
      /expired|invalid|no longer valid|error|not found/i.test(bodyText ?? "");

    expect(
      hasErrorMessage,
      "Page should display an expiry or error message"
    ).toBe(true);
  });

  // -------------------------------------------------------------------------
  // TC087 – Resending invite sends a new unique link
  // -------------------------------------------------------------------------
  test("TC087 – Resending invite sends a new unique link", async ({
    apiContext,
  }) => {
    // Record the current time so we only match emails received after
    // the resend — avoids picking up the stale TC083 email.
    const beforeResend = new Date();

    // Resend invitation to employee A (same email as TC083).
    await sendInvitation(apiContext, employeeAEmail);

    // Wait for the NEW invitation email (received after the resend).
    const resendEmail = await waitForEmail({
      sentTo: employeeAEmail,
      subject: "invite",
      timeout: 60_000,
      receivedAfter: beforeResend,
    });

    const resendLink = extractInviteLink(resendEmail);

    // The new link must differ from the original one.
    expect(resendLink).not.toBe(inviteLinkA);

    // Verify the new link is structurally valid.
    const url = new URL(resendLink);
    expect(["http:", "https:"]).toContain(url.protocol);
  });
});
