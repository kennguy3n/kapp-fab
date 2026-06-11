import { describe, expect, it, vi } from "vitest";
import {
  getCachedCatalogue,
  interpolate,
  loadCatalogue,
  translate,
  type MessageCatalogue,
} from "./bundle";
import { DefaultLocale } from "./locales";

// bundle.ts is the frontend mirror of the Go translation resolver:
// a three-stage fallback (active catalogue → English → literal key)
// plus {placeholder} interpolation. These are pure functions on the
// hot text-rendering path, so the fallback ordering and the
// interpolation edge cases are pinned here.

describe("translate (three-stage fallback)", () => {
  const active: MessageCatalogue = { "common.save": "Speichern" };

  it("returns the active-catalogue value when present", () => {
    expect(translate(active, "common.save")).toBe("Speichern");
  });

  it("falls back to the English baseline when the active catalogue misses", () => {
    // common.cancel exists in en.json but not in the active stub above.
    expect(translate(active, "common.cancel")).toBe("Cancel");
  });

  it("falls back to English when no active catalogue is loaded yet", () => {
    expect(translate(undefined, "common.save")).toBe("Save");
  });

  it("returns the literal key when both catalogues miss (loud-but-safe)", () => {
    expect(translate(active, "totally.unknown.key")).toBe("totally.unknown.key");
    expect(translate(undefined, "totally.unknown.key")).toBe(
      "totally.unknown.key",
    );
  });

  it("treats an empty-string active value as a miss and falls through", () => {
    // An empty translation must not render as a blank label; it should
    // fall back to the English baseline.
    expect(translate({ "common.save": "" }, "common.save")).toBe("Save");
  });
});

describe("interpolate", () => {
  it("substitutes {placeholder} tokens from params", () => {
    expect(
      interpolate("Will use the default for {country}.", { country: "Germany" }),
    ).toBe("Will use the default for Germany.");
  });

  it("substitutes multiple distinct placeholders", () => {
    expect(
      interpolate("Overrides the default for {country} ({default}).", {
        country: "Austria",
        default: "de",
      }),
    ).toBe("Overrides the default for Austria (de).");
  });

  it("stringifies numeric params", () => {
    expect(interpolate("{n} items", { n: 0 })).toBe("0 items");
    expect(interpolate("{n} items", { n: 42 })).toBe("42 items");
  });

  it("leaves unknown placeholders intact so the gap is visible", () => {
    expect(interpolate("Hi {name}", { other: "x" })).toBe("Hi {name}");
  });

  it("returns the template unchanged when params is omitted", () => {
    expect(interpolate("plain {token}")).toBe("plain {token}");
  });

  it("does not treat an inherited prototype property as a param", () => {
    // hasOwnProperty guard: {toString} must not pick up Object.prototype.
    expect(interpolate("{toString}", {})).toBe("{toString}");
  });
});

describe("loadCatalogue / getCachedCatalogue", () => {
  it("serves the English baseline synchronously from cache", async () => {
    expect(getCachedCatalogue(DefaultLocale)).toBeDefined();
    const en = await loadCatalogue(DefaultLocale);
    expect(en["common.save"]).toBe("Save");
  });

  it("lazily loads a shipped non-default catalogue and caches it", async () => {
    // de is not eager-loaded, so it is absent from the cache until
    // loadCatalogue resolves its dynamic import.
    const de = await loadCatalogue("de");
    expect(typeof de["common.save"]).toBe("string");
    expect(de["common.save"]).not.toBe("Save");
    // Subsequent reads are synchronous cache hits.
    expect(getCachedCatalogue("de")).toBe(de);
  });

  it("returns the English catalogue (and warns) for an unshipped tag", async () => {
    const warn = vi.spyOn(console, "warn").mockImplementation(() => {});
    const cat = await loadCatalogue("xx-unknown");
    expect(cat["common.save"]).toBe("Save");
    expect(warn).toHaveBeenCalledWith(
      expect.stringContaining('no catalogue shipped for locale "xx-unknown"'),
    );
    warn.mockRestore();
  });
});
