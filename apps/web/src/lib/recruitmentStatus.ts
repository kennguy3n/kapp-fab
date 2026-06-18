import type {
  ApplicationStatus,
  InterviewStatus,
  JobOpeningStatus,
} from "@kapp/client";
import type { BadgeProps } from "@kapp/ui";

/**
 * Recruitment status → Badge variant maps. These domain tokens (e.g.
 * `open`/`filled`/`no_show`) aren't in the shared `statusVariant` map, so
 * the recruitment surfaces share their lifecycle colors from here to keep
 * Job Openings, the dashboard, applications, and interviews consistent.
 */

/** Job-opening lifecycle (open = live, filled = goal met). */
export function openingVariant(status: JobOpeningStatus): BadgeProps["variant"] {
  switch (status) {
    case "open":
      return "success";
    case "on_hold":
      return "warning";
    case "filled":
      return "accent";
    case "draft":
      return "info";
    case "closed":
    default:
      return "neutral";
  }
}

/** Application pipeline stage. */
export function appStatusVariant(
  status: ApplicationStatus,
): BadgeProps["variant"] {
  switch (status) {
    case "applied":
      return "neutral";
    case "screening":
    case "shortlisted":
      return "info";
    case "interview":
      return "warning";
    case "offered":
      return "accent";
    case "hired":
      return "success";
    case "rejected":
      return "danger";
    case "withdrawn":
    default:
      return "neutral";
  }
}

/** Interview lifecycle. */
export function interviewVariant(
  status: InterviewStatus,
): BadgeProps["variant"] {
  switch (status) {
    case "scheduled":
      return "info";
    case "completed":
      return "success";
    case "no_show":
      return "danger";
    case "cancelled":
    default:
      return "neutral";
  }
}
