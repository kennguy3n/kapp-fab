import { describe, it, expect } from "vitest";
import { sortVersionsByPublishedDesc } from "./lib";
import type { MarketplaceExtensionVersion } from "@kapp/client";

// Minimal factory that fills in the irrelevant fields with
// deterministic placeholders. Only published_at + version are
// load-bearing for sortVersionsByPublishedDesc.
function ver(
  id: string,
  version: string,
  publishedAt: string,
): MarketplaceExtensionVersion {
  return {
    id,
    extension_id: "ext-test",
    version,
    bundle_hash: "0".repeat(64),
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
    published_at: publishedAt,
  };
}

describe("sortVersionsByPublishedDesc", () => {
  it("orders newest-first by published_at, NOT SemVer (backport scenario)", () => {
    // 1.0.4 was published AFTER 1.1.0 (chronological backport),
    // so it MUST surface first even though SemVer-desc would
    // put 1.1.0 first. This is the load-bearing behaviour that
    // BUG_0002 in MarketplaceInstallationsPage and the upgrade
    // panel on InstallationDetailPage both depend on.
    const sorted = sortVersionsByPublishedDesc([
      ver("a", "1.1.0", "2025-02-01T00:00:00Z"),
      ver("b", "1.0.4", "2025-05-01T00:00:00Z"), // newer publish
      ver("c", "1.2.0", "2025-01-15T00:00:00Z"),
    ]);
    expect(sorted.map((v) => v.id)).toEqual(["b", "a", "c"]);
  });

  it("falls back to SemVer-desc on identical timestamps for determinism", () => {
    const same = "2025-03-01T00:00:00Z";
    const sorted = sortVersionsByPublishedDesc([
      ver("a", "1.0.0", same),
      ver("b", "1.2.0", same),
      ver("c", "1.1.0", same),
    ]);
    // localeCompare descending → 1.2.0 > 1.1.0 > 1.0.0
    expect(sorted.map((v) => v.id)).toEqual(["b", "c", "a"]);
  });

  it("does NOT return NaN from the comparator on unparseable published_at (ANALYSIS_0004)", () => {
    // ANALYSIS_0004 (round 2): if published_at is unparseable
    // (mock, replay, broken backend), new Date(x).getTime()
    // returns NaN. The previous comparator returned NaN -
    // NaN = NaN, which violates Array.prototype.sort's contract
    // (must return a number, not NaN) and produces
    // implementation-defined ordering. Pin both: the comparator
    // never returns NaN, and the relative order is deterministic
    // (NaN inputs sort last, SemVer-desc tiebreaker on ties).
    const versions = [
      ver("nan-a", "1.0.0", "not-a-date"),
      ver("good-1", "1.1.0", "2025-02-01T00:00:00Z"),
      ver("nan-b", "1.2.0", "garbage"),
      ver("good-0", "1.0.5", "2025-01-15T00:00:00Z"),
    ];
    // Repeated calls must produce the same order (deterministic).
    const a = sortVersionsByPublishedDesc(versions).map((v) => v.id);
    const b = sortVersionsByPublishedDesc(versions).map((v) => v.id);
    expect(a).toEqual(b);
    // Finite-timestamp rows sort first, NaN rows go to the end.
    expect(a.slice(0, 2)).toEqual(["good-1", "good-0"]);
    // Two NaN rows fall back to SemVer-desc tiebreaker (1.2.0 > 1.0.0).
    expect(a.slice(2)).toEqual(["nan-b", "nan-a"]);
  });

  it("does not mutate the input array", () => {
    const input = [
      ver("a", "1.0.0", "2025-01-01T00:00:00Z"),
      ver("b", "1.1.0", "2025-02-01T00:00:00Z"),
    ];
    const before = input.map((v) => v.id).join(",");
    sortVersionsByPublishedDesc(input);
    expect(input.map((v) => v.id).join(",")).toBe(before);
  });
});
