import { describe, expect, it } from "vitest";
import {
  DefaultLocale,
  SupportedLocales,
  bestSupportedLocale,
  bestSupportedLocaleForCountry,
  defaultLocaleForCountry,
  isSupportedLocale,
  localeInfo,
} from "./locales";

// The locale registry is the single source of truth for which UI
// languages the frontend serves to all tenants, and the resolver
// (bestSupportedLocale) is what maps a browser/cookie/country signal
// onto a shipped catalogue. A regression here silently routes whole
// jurisdictions to the wrong language (the zh-TW→Simplified bug the
// REGION_SCRIPT_OVERRIDES table was added to fix), so the resolution
// rules are pinned exhaustively below.

describe("isSupportedLocale", () => {
  it("accepts every tag in the shipped catalogue set", () => {
    for (const info of SupportedLocales) {
      expect(isSupportedLocale(info.tag)).toBe(true);
    }
  });

  it("is case-sensitive against canonical BCP 47 casing", () => {
    expect(isSupportedLocale("zh-Hant")).toBe(true);
    expect(isSupportedLocale("zh-hant")).toBe(false);
    expect(isSupportedLocale("EN")).toBe(false);
  });

  it("rejects unknown / downgrade-only tags", () => {
    expect(isSupportedLocale("de-AT")).toBe(false);
    expect(isSupportedLocale("zh-Hans")).toBe(false);
    expect(isSupportedLocale("hi")).toBe(false);
    expect(isSupportedLocale("")).toBe(false);
  });
});

describe("localeInfo", () => {
  it("returns the matching entry for a shipped tag", () => {
    expect(localeInfo("ar")).toMatchObject({ tag: "ar", direction: "rtl" });
    expect(localeInfo("ja").name).toBe("日本語");
  });

  it("falls back to the default-locale entry for an unshipped tag", () => {
    expect(localeInfo("xx-YY")).toMatchObject({ tag: DefaultLocale });
  });

  it("marks Arabic (and only Arabic) as rtl in the current catalogue", () => {
    const rtl = SupportedLocales.filter((l) => l.direction === "rtl").map(
      (l) => l.tag,
    );
    expect(rtl).toEqual(["ar"]);
  });
});

describe("bestSupportedLocale", () => {
  it("returns the exact tag when it is shipped", () => {
    expect(bestSupportedLocale("de")).toBe("de");
    expect(bestSupportedLocale("fr-CA")).toBe("fr-CA");
    expect(bestSupportedLocale("zh-Hant")).toBe("zh-Hant");
  });

  it("applies region→script overrides for ambiguous Chinese regions", () => {
    expect(bestSupportedLocale("zh-TW")).toBe("zh-Hant");
    expect(bestSupportedLocale("zh-HK")).toBe("zh-Hant");
    expect(bestSupportedLocale("zh-MO")).toBe("zh-Hant");
  });

  it("funnels legacy Norwegian macrolanguage tags to Bokmål", () => {
    expect(bestSupportedLocale("no")).toBe("nb");
    expect(bestSupportedLocale("nn")).toBe("nb");
  });

  it("progressively drops trailing subtags until a catalogue matches", () => {
    expect(bestSupportedLocale("de-AT")).toBe("de");
    expect(bestSupportedLocale("fr-FR")).toBe("fr");
    // CN's canonical tag is zh-Hans, which is not shipped; the
    // progressive drop lands on the Simplified `zh` catalogue.
    expect(bestSupportedLocale("zh-Hans")).toBe("zh");
    // Script-tagged region drops to the script catalogue, not zh.
    expect(bestSupportedLocale("zh-Hant-TW")).toBe("zh-Hant");
  });

  it("prefers an exact regional catalogue over the primary subtag", () => {
    // pt-BR ships its own catalogue, so a pt-BR browser must NOT be
    // downgraded to the European pt catalogue.
    expect(bestSupportedLocale("pt-BR")).toBe("pt-BR");
    // ...while an unshipped pt region drops to European pt.
    expect(bestSupportedLocale("pt-PT")).toBe("pt");
  });

  it("returns null when nothing in the chain matches", () => {
    expect(bestSupportedLocale("xx")).toBeNull();
    expect(bestSupportedLocale("hi")).toBeNull();
    expect(bestSupportedLocale("")).toBeNull();
  });
});

describe("defaultLocaleForCountry", () => {
  it("maps countries to their canonical (possibly unshipped) locale", () => {
    expect(defaultLocaleForCountry("DE")).toBe("de");
    expect(defaultLocaleForCountry("AT")).toBe("de");
    expect(defaultLocaleForCountry("BR")).toBe("pt-BR");
    expect(defaultLocaleForCountry("TW")).toBe("zh-Hant");
    // Canonical tag is returned even when no catalogue ships for it.
    expect(defaultLocaleForCountry("CN")).toBe("zh-Hans");
    expect(defaultLocaleForCountry("IN")).toBe("hi");
  });

  it("normalises casing / surrounding whitespace", () => {
    expect(defaultLocaleForCountry(" de ")).toBe("de");
    expect(defaultLocaleForCountry("fr")).toBe("fr");
  });

  it("defaults unmapped or empty countries to English", () => {
    expect(defaultLocaleForCountry("US")).toBe(DefaultLocale);
    expect(defaultLocaleForCountry("ZZ")).toBe(DefaultLocale);
    expect(defaultLocaleForCountry("")).toBe(DefaultLocale);
  });
});

describe("bestSupportedLocaleForCountry", () => {
  it("returns a tag the frontend can actually render", () => {
    // CN canonical (zh-Hans) is unshipped → resolves to the zh catalogue.
    expect(bestSupportedLocaleForCountry("CN")).toBe("zh");
    // IN canonical (hi) is unshipped and has no fallback chain → English.
    expect(bestSupportedLocaleForCountry("IN")).toBe(DefaultLocale);
    // TW resolves to the Traditional catalogue exactly.
    expect(bestSupportedLocaleForCountry("TW")).toBe("zh-Hant");
    expect(bestSupportedLocaleForCountry("DE")).toBe("de");
  });
});
