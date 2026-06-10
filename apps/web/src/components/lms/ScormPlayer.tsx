import { useEffect, useRef, useState } from "react";
import type { ScormCMIData } from "@kapp/client";
import { api } from "../../lib/api";

/**
 * ScormPlayer (Session 17, Deliverable 2/12).
 *
 * Hosts a SCORM SCO in a sandboxed iframe and exposes the SCORM
 * Run-Time Environment (RTE) JS API the content discovers by walking
 * up `window.parent`. Two adapters are installed depending on the
 * package version:
 *
 *   - SCORM 1.2   → `window.API`           (LMSInitialize, LMSGetValue,
 *                                            LMSSetValue, LMSCommit,
 *                                            LMSFinish, …)
 *   - SCORM 2004  → `window.API_1484_11`   (Initialize, GetValue,
 *                                            SetValue, Commit, Terminate, …)
 *
 * The adapter keeps the CMI data model in memory, hydrates resume
 * state from `POST /scorm/{lesson}/initialize`, and flushes to
 * `POST /scorm/{lesson}/commit` on LMSCommit and
 * `POST /scorm/{lesson}/terminate` on LMSFinish/Terminate. The backend
 * maps the CMI fields onto lms.progress (status, score, time, suspend
 * data) — see internal/lms/scorm.go.
 */
export type ScormVersion = "scorm_12" | "scorm_2004";

export interface ScormPlayerProps {
  lessonId: string;
  enrollmentId: string;
  /** URL of the SCO launch file (imsmanifest entrypoint) to load. */
  contentUrl: string;
  version: ScormVersion;
  /** Notified after a successful commit/terminate so callers can
   *  refresh progress views. */
  onProgress?: () => void;
}

// Minimal CMI store: the keys SCORM content reads/writes are a flat
// dotted namespace. We keep them in a Map and project the subset the
// backend cares about into ScormCMIData on flush.
type Store = Map<string, string>;

declare global {
  interface Window {
    API?: unknown;
    API_1484_11?: unknown;
  }
}

// LMS lesson_progress.status vocabulary ("not_started" | "in_progress" |
// "completed") is NOT valid SCORM CMI status vocabulary. When hydrating
// resume state we must translate it into the version-appropriate SCORM
// value, otherwise the SCO receives an unrecognized status string.
export function lmsStatusToScorm(
  lmsStatus: string | undefined,
  version: ScormVersion,
): string {
  switch (lmsStatus) {
    case "completed":
      return "completed";
    case "in_progress":
      return "incomplete";
    case "not_started":
    case "":
    case undefined:
      // Both SCORM 1.2 (lesson_status) and 2004 (completion_status)
      // accept "not attempted".
      return "not attempted";
    default:
      // Unknown LMS value: fall back to the version's neutral default
      // rather than leaking it verbatim into the CMI model.
      return version === "scorm_12" ? "incomplete" : "unknown";
  }
}

function projectCMI(store: Store, version: ScormVersion): ScormCMIData {
  const cmi: ScormCMIData = {
    version: version === "scorm_12" ? "scorm_12" : "scorm_2004",
  };
  if (version === "scorm_12") {
    cmi.lesson_status = store.get("cmi.core.lesson_status") ?? "";
    const raw = store.get("cmi.core.score.raw");
    if (raw) cmi.score_raw = Number(raw);
    cmi.session_time = store.get("cmi.core.session_time") ?? "";
  } else {
    cmi.completion_status = store.get("cmi.completion_status") ?? "";
    cmi.success_status = store.get("cmi.success_status") ?? "";
    const raw = store.get("cmi.score.raw");
    if (raw) cmi.score_raw = Number(raw);
    const scaled = store.get("cmi.score.scaled");
    if (scaled) cmi.score_scaled = Number(scaled);
    cmi.session_time = store.get("cmi.session_time") ?? "";
  }
  cmi.suspend_data = store.get("cmi.suspend_data") ?? "";
  return cmi;
}

export function ScormPlayer({
  lessonId,
  enrollmentId,
  contentUrl,
  version,
  onProgress,
}: ScormPlayerProps) {
  const [status, setStatus] = useState<string>("loading");
  const [hydrated, setHydrated] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const storeRef = useRef<Store>(new Map());

  // Keep the latest onProgress in a ref so the RTE-install effect does NOT
  // depend on its identity. A non-memoized callback from the parent would
  // otherwise re-run the effect on every parent render, tearing down and
  // re-installing window.API mid-session (breaking in-flight SCO calls).
  const onProgressRef = useRef(onProgress);
  useEffect(() => {
    onProgressRef.current = onProgress;
  }, [onProgress]);

  useEffect(() => {
    let cancelled = false;
    const store = storeRef.current;
    let lastError = "0";

    const flush = async (terminate: boolean) => {
      const cmi = projectCMI(store, version);
      try {
        if (terminate) {
          await api.scormTerminate(lessonId, enrollmentId, cmi);
        } else {
          await api.scormCommit(lessonId, enrollmentId, cmi);
        }
        onProgressRef.current?.();
      } catch (e) {
        lastError = "101"; // general exception
        setError((e as Error).message);
      }
    };

    const init = (): string => {
      setStatus("running");
      return "true";
    };
    const getValue = (key: string): string => store.get(key) ?? "";
    const setValue = (key: string, value: string): string => {
      store.set(key, value);
      return "true";
    };
    const commit = (): string => {
      void flush(false);
      return "true";
    };
    const finish = (): string => {
      void flush(true);
      setStatus("terminated");
      return "true";
    };
    const getLastError = (): string => lastError;

    // The SCORM RTE lives on global singletons (window.API /
    // window.API_1484_11); the spec assumes a single active SCO. Warn if a
    // sibling player already installed one so a double-mount (two SCOs on
    // one page) is diagnosable rather than silently clobbering each other.
    const existing =
      version === "scorm_12" ? window.API : window.API_1484_11;
    if (existing) {
      console.warn(
        "ScormPlayer: a SCORM RTE is already installed on this window; " +
          "only one SCO can be active at a time.",
      );
    }

    // SCORM 1.2 and 2004 expose the same semantics under different
    // method names; we install whichever the requested version needs.
    if (version === "scorm_12") {
      window.API = {
        LMSInitialize: init,
        LMSGetValue: getValue,
        LMSSetValue: setValue,
        LMSCommit: commit,
        LMSFinish: finish,
        LMSGetLastError: getLastError,
        LMSGetErrorString: () => "",
        LMSGetDiagnostic: () => "",
      };
    } else {
      window.API_1484_11 = {
        Initialize: init,
        GetValue: getValue,
        SetValue: setValue,
        Commit: commit,
        Terminate: finish,
        GetLastError: getLastError,
        GetErrorString: () => "",
        GetDiagnostic: () => "",
      };
    }

    // Hydrate resume state before the SCO initializes so the first
    // LMSGetValue("cmi.suspend_data") returns where the learner left off.
    api
      .scormInitialize(lessonId, enrollmentId)
      .then((rt) => {
        if (cancelled) return;
        if (rt.exists) {
          store.set("cmi.suspend_data", rt.suspend_data ?? "");
          const scormStatus = lmsStatusToScorm(rt.status, version);
          if (version === "scorm_12") {
            store.set("cmi.core.lesson_status", scormStatus);
            if (rt.score) store.set("cmi.core.score.raw", rt.score);
          } else {
            store.set("cmi.completion_status", scormStatus);
            if (rt.score) store.set("cmi.score.raw", rt.score);
          }
        }
        // Only now (resume state populated) do we allow the iframe to load,
        // so the SCO's first LMSGetValue("cmi.suspend_data") cannot race
        // ahead of hydration and read an empty store.
        setHydrated(true);
        setStatus("ready");
      })
      .catch((e) => {
        if (!cancelled) setError((e as Error).message);
      });

    return () => {
      cancelled = true;
      if (version === "scorm_12") delete window.API;
      else delete window.API_1484_11;
    };
  }, [lessonId, enrollmentId, version]);

  return (
    <div className="flex flex-col gap-2">
      <div className="flex items-center gap-2 text-[12px] text-fg-muted">
        <span>SCORM runtime: {version === "scorm_12" ? "1.2" : "2004"}</span>
        <span>·</span>
        <span>status: {status}</span>
      </div>
      {error && (
        <p className="text-danger" role="alert">
          SCORM error: {error}
        </p>
      )}
      {hydrated ? (
        <iframe
          title="SCORM content"
          src={contentUrl}
          className="h-[640px] w-full rounded-lg border border-border bg-bg"
          sandbox="allow-scripts allow-same-origin allow-forms"
        />
      ) : (
        <div className="flex h-[640px] w-full items-center justify-center rounded-lg border border-border bg-bg text-[12px] text-fg-muted">
          Loading SCORM content…
        </div>
      )}
    </div>
  );
}

export default ScormPlayer;
