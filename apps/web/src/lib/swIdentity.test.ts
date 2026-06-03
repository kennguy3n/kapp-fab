import { afterEach, describe, expect, it, vi } from "vitest";
import { postIdentityToServiceWorker } from "./swIdentity";

describe("postIdentityToServiceWorker", () => {
  afterEach(() => {
    vi.unstubAllGlobals();
    localStorage.clear();
  });

  it("no-ops when the Service Worker API is unavailable", async () => {
    // jsdom has no navigator.serviceWorker by default.
    await expect(postIdentityToServiceWorker()).resolves.toBeUndefined();
  });

  it("posts a stable, non-reversible identity hash to the active worker", async () => {
    localStorage.setItem("kapp.tenant", "acme");
    localStorage.setItem("kapp.token", "secret-token");
    const postMessage = vi.fn();
    vi.stubGlobal("navigator", {
      serviceWorker: {
        ready: Promise.resolve({ active: { postMessage } }),
        controller: null,
      },
    });

    await postIdentityToServiceWorker();

    expect(postMessage).toHaveBeenCalledTimes(1);
    const msg = postMessage.mock.calls[0][0] as { type: string; id: string };
    expect(msg.type).toBe("kapp:identity");
    // SHA-256 hex digest: 64 hex chars, and never the raw token.
    expect(msg.id).toMatch(/^[0-9a-f]{64}$/);
    expect(msg.id).not.toContain("secret-token");
  });

  it("does not throw when crypto.subtle is unavailable", async () => {
    const postMessage = vi.fn();
    vi.stubGlobal("navigator", {
      serviceWorker: {
        ready: Promise.resolve({ active: { postMessage } }),
        controller: null,
      },
    });
    vi.stubGlobal("crypto", {});

    await expect(postIdentityToServiceWorker()).resolves.toBeUndefined();
    expect(postMessage).not.toHaveBeenCalled();
  });
});
