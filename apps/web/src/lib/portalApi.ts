// Thin portal-scoped API helper. The portal uses a different auth
// path than the standard AppShell (magic-link → portal-scoped JWT
// with scope="portal"), so we store the token under its own
// localStorage key and do NOT send the X-Tenant-ID header — the
// portal token already carries the tenant claim.

const BASE = "/api/v1";

export const PORTAL_TOKEN_KEY = "kapp.portal.token";
export const PORTAL_TENANT_KEY = "kapp.portal.tenant";
export const PORTAL_EMAIL_KEY = "kapp.portal.email";

export interface PortalAuthResponse {
  token: string;
  expires_at: number;
  user: {
    id: string;
    tenant_id: string;
    email: string;
    display_name: string;
  };
}

export interface PortalTicket {
  id: string;
  ktype: string;
  data: Record<string, unknown>;
  status: string;
  created_at: string;
  updated_at: string;
}

async function req<T>(path: string, init?: RequestInit): Promise<T> {
  const tok = localStorage.getItem(PORTAL_TOKEN_KEY);
  const headers: Record<string, string> = {
    "Content-Type": "application/json",
    ...(init?.headers as Record<string, string> | undefined),
  };
  if (tok) headers.Authorization = `Bearer ${tok}`;
  const res = await fetch(`${BASE}${path}`, { ...init, headers });
  if (!res.ok) {
    throw new Error(`${res.status}: ${await res.text()}`);
  }
  if (res.status === 204) return undefined as T;
  return (await res.json()) as T;
}

export const portalApi = {
  requestLink(tenantSlug: string, email: string): Promise<void> {
    return req(`/portal/auth/request`, {
      method: "POST",
      body: JSON.stringify({ tenant_slug: tenantSlug, email }),
    });
  },
  verifyLink(
    tenantSlug: string,
    email: string,
    token: string
  ): Promise<PortalAuthResponse> {
    return req(`/portal/auth/verify`, {
      method: "POST",
      body: JSON.stringify({ tenant_slug: tenantSlug, email, token }),
    });
  },
  listTickets(): Promise<{ tickets: PortalTicket[] }> {
    return req(`/portal/tickets/`);
  },
  getTicket(id: string): Promise<PortalTicket> {
    return req(`/portal/tickets/${encodeURIComponent(id)}`);
  },
  createTicket(subject: string, description: string, priority?: string): Promise<PortalTicket> {
    return req(`/portal/tickets/`, {
      method: "POST",
      body: JSON.stringify({ subject, description, priority }),
    });
  },
  reply(id: string, body: string): Promise<PortalTicket> {
    return req(`/portal/tickets/${encodeURIComponent(id)}/reply`, {
      method: "POST",
      body: JSON.stringify({ body }),
    });
  },
};
