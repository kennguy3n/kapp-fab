/**
 * Mailosaur email testing helper for Playwright E2E tests.
 *
 * Wraps the Mailosaur Node SDK and exposes convenience methods for
 * waiting on emails, extracting links, and downloading attachments.
 *
 * Required env vars:
 *   MAILOSAUR_API_KEY  – Mailosaur API key
 *   MAILOSAUR_SERVER_ID – Mailosaur server (sandbox) ID
 */

import Mailosaur from "mailosaur";
import type { Message, Link } from "mailosaur/lib/models";

// ---------------------------------------------------------------------------
// Config
// ---------------------------------------------------------------------------

const API_KEY = process.env.MAILOSAUR_API_KEY ?? "";
const SERVER_ID = process.env.MAILOSAUR_SERVER_ID ?? "";

if (!API_KEY) {
  throw new Error(
    "MAILOSAUR_API_KEY is not set. " +
      "Get one at https://mailosaur.com/app/account/api-access"
  );
}
if (!SERVER_ID) {
  throw new Error(
    "MAILOSAUR_SERVER_ID is not set. " +
      "Find yours at https://mailosaur.com/app/servers"
  );
}

const client = new Mailosaur(API_KEY);

// ---------------------------------------------------------------------------
// Public helpers
// ---------------------------------------------------------------------------

/**
 * Generate a unique Mailosaur-routed email address.
 * Every call returns a distinct address so tests never collide.
 */
export function generateEmailAddress(prefix = "test"): string {
  const tag = `${prefix}-${Date.now()}-${Math.random().toString(36).slice(2, 8)}`;
  return `${tag}@${SERVER_ID}.mailosaur.net`;
}

export interface WaitForEmailOptions {
  /** Email address the message was sent to. */
  sentTo: string;
  /** Optional subject substring to match. */
  subject?: string;
  /** Timeout in milliseconds (default 30 000). */
  timeout?: number;
}

/**
 * Poll Mailosaur until an email matching the criteria arrives.
 * Returns the full `Message` object.
 */
export async function waitForEmail(
  opts: WaitForEmailOptions
): Promise<Message> {
  const { sentTo, subject, timeout = 30_000 } = opts;

  const searchCriteria: { sentTo: string; subject?: string } = { sentTo };
  if (subject) {
    searchCriteria.subject = subject;
  }

  const message = await client.messages.get(SERVER_ID, searchCriteria, {
    timeout,
  });

  return message;
}

/**
 * Return all links found in the HTML body of an email.
 */
export function extractLinks(message: Message): Link[] {
  return message.html?.links ?? [];
}

/**
 * Find the first link whose `href` matches the given pattern.
 */
export function findLink(
  message: Message,
  pattern: string | RegExp
): Link | undefined {
  const regex = typeof pattern === "string" ? new RegExp(pattern) : pattern;
  return extractLinks(message).find((l) => l.href && regex.test(l.href));
}

/**
 * Extract all links matching a pattern from the email.
 */
export function findAllLinks(
  message: Message,
  pattern: string | RegExp
): Link[] {
  const regex = typeof pattern === "string" ? new RegExp(pattern) : pattern;
  return extractLinks(message).filter((l) => l.href && regex.test(l.href));
}

/**
 * Return the plain-text body of the email (fallback to HTML stripped of tags).
 */
export function getTextBody(message: Message): string {
  if (message.text?.body) return message.text.body;
  // Rough strip if only HTML is available.
  return (message.html?.body ?? "").replace(/<[^>]+>/g, "");
}

/**
 * Return the raw HTML body of the email.
 */
export function getHtmlBody(message: Message): string {
  return message.html?.body ?? "";
}

/**
 * Delete all messages in the Mailosaur server.
 * Useful in `beforeAll` / `afterAll` to keep the inbox clean.
 */
export async function purgeInbox(): Promise<void> {
  await client.messages.deleteAll(SERVER_ID);
}

/**
 * Delete a single message by ID.
 */
export async function deleteMessage(messageId: string): Promise<void> {
  await client.messages.del(messageId);
}

export { client, SERVER_ID, API_KEY };
