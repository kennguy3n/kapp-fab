import { useQuery } from "@tanstack/react-query";
import { api } from "./api";

/**
 * Tenant identity helpers — the shell must show the tenant's DISPLAY
 * NAME and never leak a raw machine value (UUID) to the user.
 *
 * Resolution is best-effort: we try the control-plane `getTenant`
 * endpoint, but that is admin-scoped on the live backend, so non-admin
 * users get a 403.  In every failure / loading case we fall back to a
 * humanized label that is guaranteed not to be a UUID.
 */
const UUID_RE =
  /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;

/** The raw tenant id the app stores at login (UUID or dev slug). */
export function tenantKey(): string {
  if (typeof localStorage === "undefined") return "default";
  return localStorage.getItem("kapp.tenant") ?? "default";
}

export function isUuid(value: string): boolean {
  return UUID_RE.test(value);
}

/**
 * Turn a tenant identifier into a human label that never exposes a
 * machine value.  A slug like "acme-corp" becomes "Acme Corp"; a raw
 * UUID, the literal "default", or an empty value falls back to a
 * neutral "Workspace".
 */
export function humanizeTenant(raw: string | null | undefined): string {
  if (!raw || raw === "default" || isUuid(raw)) return "Workspace";
  return raw
    .replace(/[-_]+/g, " ")
    .replace(/\b\w/g, (c) => c.toUpperCase())
    .trim();
}

export interface TenantName {
  /** Display-safe tenant name (resolved from the API or humanized). */
  name: string;
  /** True once the real name came back from the API. */
  isResolved: boolean;
}

/**
 * Resolve the current tenant's display name for use anywhere in the
 * shell (sidebar, profile menu, dashboard greeting).  Other
 * workstreams should reuse this hook rather than reading
 * `kapp.tenant` directly so no screen ever renders the UUID.
 */
export function useTenantName(): TenantName {
  const id = tenantKey();
  const q = useQuery({
    queryKey: ["tenant-name", id],
    queryFn: () => api.getTenant(id),
    // getTenant is admin-only on the live backend; non-admin users get
    // a 403, so don't retry and cache the (failed) result so we don't
    // hammer the endpoint on every navigation. The humanized fallback
    // below covers that case.
    retry: false,
    staleTime: Infinity,
    gcTime: Infinity,
    enabled: typeof window !== "undefined" && id !== "default",
  });
  const resolved = q.data?.name?.trim();
  if (resolved) return { name: resolved, isResolved: true };
  return { name: humanizeTenant(id), isResolved: false };
}
