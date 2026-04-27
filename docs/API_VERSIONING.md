# API Versioning Strategy

This document defines how the Kapp HTTP API evolves without breaking
the customer integrations, KChat bridge, web UI, and external partners
that depend on it. It is the canonical contract referenced by
`services/api/main.go` and enforced by the
`.github/workflows/api-versioning-check.yml` CI gate.

## Principles

1. **Every public REST route lives under `/api/v1/`.** A route mounted
   anywhere else is a bug — CI rejects it. The single-version prefix
   keeps clients pinned to a stable surface and lets internal control-
   plane traffic (`/internal/...`, `/healthz`, `/metrics`) evolve
   independently without touching the customer-facing surface.
2. **Backward-compatible additions are unversioned.** Adding a new
   endpoint, a new optional request field, a new response field, or a
   new query parameter is a v1-compatible change and ships under
   `/api/v1/`.
3. **Breaking changes ship under a new major version.** A change that
   removes a field, renames a field, narrows a type, removes an
   endpoint, or alters status-code semantics requires a new prefix
   (`/api/v2/`) and a deprecation timeline for `/api/v1/` (see
   below). v2 routes co-exist with v1 routes during the deprecation
   window.
4. **No silent semantic changes.** A handler MUST NOT change the
   meaning of an existing field even if the wire shape is identical.
   When the meaning needs to change, it is a breaking change.

## What counts as backward-compatible?

The following changes are safe to ship under `/api/v1/` without
versioning:

- Adding a new endpoint.
- Adding a new optional query parameter that defaults to historical
  behaviour.
- Adding a new field to a request body that is optional and defaults
  to historical behaviour.
- Adding a new field to a response body. Clients are required (per
  this document) to ignore unknown fields.
- Loosening a request-side validation (e.g. accepting longer strings,
  more characters, more values for an enum where the new values
  default to safe behaviour).
- Adding a new HTTP status code value the handler can return for a
  scenario it could not previously detect (e.g. adding `429 Too Many
  Requests` to a route that previously only returned `200`/`500`).
- Adding a new error code string in an existing error envelope, as
  long as the envelope's outer shape (`error`, `code`, `message`) is
  unchanged.

## What counts as breaking?

The following changes require a new major prefix:

- Removing an endpoint.
- Removing or renaming a field in a request or response body.
- Narrowing a type (e.g. `string` → `enum`, `number` → `integer`,
  `optional` → `required`).
- Tightening a request-side validation (e.g. shortening a max length).
- Changing the HTTP status code returned for an existing scenario.
- Changing the meaning of an existing field.
- Changing the auth requirement for an endpoint (e.g. public → admin).
- Changing the rate-limit or idempotency contract on an endpoint
  in a way that affects retries or replays.

## Deprecation timeline

When a v2 surface is introduced:

1. v2 ships alongside v1 on the same release.
2. v1 endpoints add a `Deprecation` and `Sunset` HTTP response header
   per RFC 8594 / draft-ietf-httpapi-deprecation-header. The
   `Sunset` header carries the planned removal date, which is at
   least **3 minor releases** (≈ 6 months at the current cadence)
   after v2 ships.
3. v1 endpoints continue to be fully supported for the duration of
   the deprecation window — no behaviour change, no rate-limit
   tightening, no error-message degradation.
4. The release notes call out every v1 → v2 mapping.
5. After the `Sunset` date, v1 endpoints return `410 Gone` for one
   minor release, then are removed.

A tenant on the per-tenant pin (see below) follows the same timeline
but does NOT get auto-upgraded — the tenant must explicitly bump
their pin before the `Sunset` date.

## Per-tenant version pinning

Some tenants integrate against the API via a Frappe plugin, an
internal ETL job, or a fixed-version SDK and cannot upgrade in lock-
step with the platform. The control plane supports per-tenant version
pinning via the `tenant_features` table:

```
INSERT INTO tenant_features (tenant_id, feature_key, enabled)
VALUES ($1, 'api_version_pin:v1', true);
```

When a request arrives with the matching pin, `services/api/main.go`'s
version negotiation middleware:

1. Reads the pin from `tenant_features` keyed on the tenant id resolved
   by `TenantMiddleware`.
2. Forces the request prefix to the pinned version, even if the client
   sent `/api/v2/...`. The request is rewritten to `/api/v1/...`
   internally, the response is converted back into the v1 shape
   before serialisation, and the response carries an
   `X-Kapp-API-Version: v1` header so the client can confirm.
3. Continues to fire the `Deprecation`/`Sunset` headers so the tenant
   sees the upgrade pressure.

Pinning is a control-plane operation: tenants do NOT pin themselves
through the regular UI. The platform team flips the pin via a
support ticket so the trail is auditable.

When `Sunset` arrives the pin row is removed by a control-plane
migration. After that point, requests against the pinned version
return `410 Gone` like any other client.

## Version negotiation

The default negotiation rule is **path-prefix**: the URL prefix wins.
There is no `Accept` header parsing, no query-string `?api=v2`, no
custom header. This keeps caches, CDNs, and browser dev tools all
agreeing on the version of a given resource without having to vary
on a non-`Vary` axis.

The exceptions are:

- **Per-tenant pin (above).** Server-side rewrite of the prefix.
- **Internal control plane** (`/internal/...`). Versioned by code
  imports rather than URL — these routes are not customer-facing
  and the gateway rejects them at the cell boundary.

## Adding a new endpoint

1. Mount the new endpoint under `/api/v1/`. CI fails if you mount it
   anywhere else.
2. Default any new optional fields to behaviour-preserving values.
3. Document the endpoint in the OpenAPI spec checked into the repo.
4. If the endpoint is gated by a feature flag, register the flag in
   `internal/tenant/plans.go` and the `DynamicFeatureMiddleware`
   path-to-feature map in `internal/platform/feature_middleware.go`.

## Adding a v2 surface

1. Mount the new endpoints under `/api/v2/`. The router in
   `services/api/main.go` may share handler code between v1 and v2
   when the only difference is the wire shape — keep the
   business-logic store calls identical, and write a thin
   request/response adapter in the `v2` route group.
2. Wire the deprecation headers on the v1 group via the
   `platform.DeprecationMiddleware(sunsetDate)` middleware.
3. Update this document with the v1 → v2 mapping section.
4. Open the support pin migration so tenants can opt out of the
   auto-upgrade.

## Enforcement

`.github/workflows/api-versioning-check.yml` runs on every pull
request and fails if a new HTTP route is mounted outside `/api/v1/`,
`/api/v2/`, `/internal/`, or the small allow-list of platform routes
(`/healthz`, `/metrics`, `/.well-known/...`).

The check parses `services/api/main.go` and any other file that calls
into `chi.Router.Method(...)` so the policy applies to every package
that registers an HTTP handler.
