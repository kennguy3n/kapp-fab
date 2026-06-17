import { describe, it, expect } from "vitest";
import { lmsStatusToScorm, cmiScoreRaw, scormStatusLabel } from "./ScormPlayer";

// LMS lesson_progress.status vocabulary must be translated into valid
// SCORM CMI status values before resume hydration; an unmapped value
// (e.g. "in_progress") would be rejected by the SCO.
describe("lmsStatusToScorm", () => {
  it("maps completed to completed for both versions", () => {
    expect(lmsStatusToScorm("completed", "scorm_12")).toBe("completed");
    expect(lmsStatusToScorm("completed", "scorm_2004")).toBe("completed");
  });

  it("maps in_progress to incomplete (a valid SCORM value)", () => {
    expect(lmsStatusToScorm("in_progress", "scorm_12")).toBe("incomplete");
    expect(lmsStatusToScorm("in_progress", "scorm_2004")).toBe("incomplete");
  });

  it("maps not_started / empty / undefined to 'not attempted'", () => {
    expect(lmsStatusToScorm("not_started", "scorm_12")).toBe("not attempted");
    expect(lmsStatusToScorm("", "scorm_2004")).toBe("not attempted");
    expect(lmsStatusToScorm(undefined, "scorm_12")).toBe("not attempted");
  });

  it("never leaks an unknown LMS value verbatim into CMI", () => {
    expect(lmsStatusToScorm("weird", "scorm_12")).toBe("incomplete");
    expect(lmsStatusToScorm("weird", "scorm_2004")).toBe("unknown");
  });
});

// The Go RuntimeState.Score (*decimal.Decimal) arrives as a JSON number,
// but the SCORM CMI store is string-typed. cmiScoreRaw must stringify it
// (without dropping a legitimate 0) so SCOs doing `typeof === "string"`
// checks don't malfunction.
describe("cmiScoreRaw", () => {
  it("stringifies a numeric score", () => {
    expect(cmiScoreRaw(90)).toBe("90");
    expect(cmiScoreRaw(87.5)).toBe("87.5");
  });

  it("keeps a legitimate zero as \"0\" rather than dropping it", () => {
    expect(cmiScoreRaw(0)).toBe("0");
  });

  it("returns undefined when there is no score to hydrate", () => {
    expect(cmiScoreRaw(undefined)).toBeUndefined();
  });
});

// The player's internal lifecycle token must never be surfaced verbatim
// to learners; scormStatusLabel maps it to human copy + a semantic tone.
describe("scormStatusLabel", () => {
  it("maps each lifecycle state to learner-facing copy", () => {
    expect(scormStatusLabel("loading")).toEqual({
      label: "Loading",
      tone: "muted",
    });
    expect(scormStatusLabel("ready")).toEqual({ label: "Ready", tone: "info" });
    expect(scormStatusLabel("running")).toEqual({
      label: "In progress",
      tone: "accent",
    });
    expect(scormStatusLabel("terminated")).toEqual({
      label: "Finished",
      tone: "success",
    });
  });

  it("humanizes an unexpected token rather than leaking it raw", () => {
    expect(scormStatusLabel("some_state")).toEqual({
      label: "Some State",
      tone: "muted",
    });
  });
});
