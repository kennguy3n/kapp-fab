import { describe, it, expect } from "vitest";
import { lmsStatusToScorm } from "./ScormPlayer";

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
