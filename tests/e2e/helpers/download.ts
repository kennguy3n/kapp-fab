/**
 * File download helper for Playwright E2E tests.
 *
 * Downloads a file from a URL using Playwright's APIRequestContext
 * and returns the response body as text or buffer.
 */

import type { APIRequestContext } from "@playwright/test";

export interface DownloadResult {
  /** HTTP status code. */
  status: number;
  /** Response body as UTF-8 text. */
  text: string;
  /** Response body as raw buffer. */
  buffer: Buffer;
  /** Content-Type header value. */
  contentType: string;
  /** Content-Disposition header value (if present). */
  contentDisposition: string | null;
}

/**
 * Download a file via HTTP GET and return its contents.
 *
 * Uses Playwright's `request` fixture so cookies / auth state
 * from the browser context are automatically forwarded.
 */
export async function downloadFile(
  request: APIRequestContext,
  url: string
): Promise<DownloadResult> {
  const response = await request.get(url);
  const buffer = await response.body();

  return {
    status: response.status(),
    text: buffer.toString("utf-8"),
    buffer,
    contentType: response.headers()["content-type"] ?? "",
    contentDisposition: response.headers()["content-disposition"] ?? null,
  };
}
