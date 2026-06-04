# CDN & Edge Caching Setup

This guide explains how Kapp's caching layer works and how to put a CDN
(Cloudflare, AWS CloudFront, or any standards-compliant CDN) in front of
the platform. It is written for operators deploying the production
`docker-compose.prod.yml` stack, but the cache-header contract applies
to any deployment topology.

## TL;DR

| Surface        | Path            | `Cache-Control`                          | Why |
| -------------- | --------------- | ---------------------------------------- | --- |
| API            | `/api/v1/*`     | `no-store`                               | Tenant-scoped, authenticated, RLS-isolated — must **never** be cached by a browser, proxy, or CDN. |
| Static bundles | `/assets/*`     | `public, max-age=31536000, immutable`    | Vite emits content-hashed filenames; the URL is the cache key, so they can be cached forever. |
| SPA shell      | `/` (index.html)| `no-cache`                               | Must revalidate so a new deploy (which rewrites `index.html` to point at the new hashed bundles) is picked up immediately. |

If your CDN does only one thing, make it **bypass the cache for
`/api/*`**. Caching a tenant-scoped API response and serving it to a
different tenant is a data-isolation incident.

## How the layers fit together

```
client ──► CDN (optional) ──► Caddy (TLS edge) ──► api:8080 (origin)
                                                     ├─ React SPA (Vite build)
                                                     └─ /api/v1 REST + gRPC-gateway
```

There are **three** places the cache policy is expressed; they are
intentionally redundant so the contract holds even if one layer is
misconfigured:

1. **Origin** — `internal/platform/cache_control.go`
   (`CacheControlMiddleware`) sets the authoritative `Cache-Control`
   header on every response as a default. Handlers that serve
   content-addressed payloads (the marketplace bundle download, which is
   `immutable` + `ETag`) or long-lived streams (SSE, `no-cache`)
   override it.
2. **Edge** — `Caddyfile.prod` mirrors the policy for `/assets/*` and
   `/` and negotiates `gzip`/`zstd` compression. It proxies everything
   to the origin and leaves `/api/*` headers untouched (so the origin's
   `no-store` wins).
3. **CDN** — optional, configured per the sections below.

### ETag / conditional requests

Strong validators (`ETag`, `Last-Modified`) are emitted by the layer
that serves the bytes:

- **Content-hashed `/assets/*` bundles** are `immutable`, so browsers
  never revalidate them — an `ETag` would be redundant. A new deploy
  changes the filename hash, not the bytes under a fixed URL.
- **`index.html`** is `no-cache`, so the browser revalidates on every
  navigation. The origin's static file server emits an `ETag`/
  `Last-Modified`; Caddy and the CDN forward `If-None-Match` /
  `If-Modified-Since` to the origin, which answers `304 Not Modified`
  with no body when unchanged. Keep "respect origin headers" /
  "origin cache control" enabled on the CDN so these validators are not
  stripped.
- **Marketplace bundles** (`/api/v1/marketplace/bundles/{hash}`) are
  content-addressed and already serve a strong `ETag` (the SHA-256
  hash) plus `immutable` — see
  `services/api/marketplace_publisher_bundle_handlers.go`.

## Cloudflare

### DNS

1. Add an `A`/`AAAA` (or `CNAME`) record for your domain pointing at the
   Caddy edge host.
2. Set the record to **Proxied** (orange cloud) so traffic flows through
   Cloudflare's edge.
3. Set SSL/TLS mode to **Full (strict)** — Caddy presents a real
   Let's Encrypt certificate, so strict validation works end-to-end.

> **Origin certificates:** with the proxy enabled, Cloudflare terminates
> TLS at its edge and re-originates to Caddy. Caddy's automatic HTTPS
> still issues a publicly-trusted cert for `KAPP_DOMAIN`, which
> satisfies Full (strict). Alternatively, install a Cloudflare Origin
> CA cert on Caddy.

### Cache Rules (recommended over legacy Page Rules)

Create two rules in **Caching → Cache Rules**, ordered so the API rule
is evaluated first:

1. **Bypass cache for the API**
   - When incoming requests match: `URI Path starts with "/api/"`
   - Then: **Bypass cache**.

2. **Cache static assets aggressively**
   - When incoming requests match: `URI Path starts with "/assets/"`
   - Then: **Eligible for cache**, **Edge TTL → Use cache-control
     header if present** (the origin sends `max-age=31536000,
     immutable`), **Browser TTL → Respect origin**.

Leave everything else (the `/` SPA shell) on **Respect origin headers**
so the `no-cache` revalidation contract is preserved.

### Compression

Cloudflare compresses automatically (Brotli/gzip). Caddy's `encode`
still applies on the Caddy↔origin hop and for direct (un-proxied)
access.

## AWS CloudFront

### Origin

- **Origin domain:** the Caddy edge hostname (e.g.
  `origin.kapp.example.com`).
- **Protocol:** HTTPS only; **Origin SSL protocols:** TLSv1.2+.
- **Origin Shield:** optional, reduces origin load for multi-region
  viewers.

### Cache behaviors

Order matters — CloudFront evaluates path patterns most-specific first.

| Precedence | Path pattern   | Cache policy                                    | Origin request policy        | Notes |
| ---------- | -------------- | ----------------------------------------------- | ---------------------------- | ----- |
| 0          | `/api/*`       | **CachingDisabled** (managed)                   | **AllViewer** (managed)      | Forward all headers/cookies/query; never cache. |
| 1          | `/assets/*`    | **CachingOptimized** (managed)                  | CORS-S3Origin or minimal     | Honors origin `Cache-Control: immutable`; long TTL. |
| 2 (default)| `*`            | **CachingDisabled** or a short-TTL custom policy| AllViewer                    | SPA shell — keep `no-cache` from origin; do not cache HTML at the edge. |

- Use the AWS-managed **`CachingDisabled`** policy for `/api/*` and the
  default behavior, and **`CachingOptimized`** for `/assets/*`.
- Enable **Compress objects automatically** on the `/assets/*` and
  default behaviors (gzip + Brotli).
- For the default (SPA) behavior, prefer forwarding `If-None-Match` so
  CloudFront can serve `304`s from the origin; the managed
  `CachingDisabled` policy already forwards the conditional headers.

## Generic CDN checklist

Any CDN works as long as it:

1. **Respects origin `Cache-Control`** (does not override it globally).
2. **Bypasses cache for `/api/*`** — non-negotiable for tenant isolation.
3. **Caches `/assets/*`** for a long TTL, keyed on the full path
   (the content hash makes the path unique per build).
4. **Forwards conditional request headers** (`If-None-Match`,
   `If-Modified-Since`) to the origin for `no-cache` resources so
   `index.html` revalidation returns `304`.
5. **Does not strip `ETag` / `Vary` / `Content-Encoding`** from origin
   responses.
6. **Forwards the `Authorization` header and auth cookies** to the
   origin for `/api/*` (implied by "bypass cache", but verify — some
   CDNs strip `Authorization` by default).

Required origin headers (already emitted by Kapp — see the TL;DR table)
are the only contract the CDN needs to honor.

## Infrastructure as Code

### Terraform — Cloudflare

```hcl
variable "zone_id" { type = string }
variable "domain"  { type = string } # e.g. "kapp.example.com"

# API: never cache (tenant-scoped, authenticated).
resource "cloudflare_ruleset" "kapp_cache" {
  zone_id = var.zone_id
  name    = "kapp-edge-caching"
  kind    = "zone"
  phase   = "http_request_cache_settings"

  rules {
    description = "Bypass cache for the API"
    expression  = "(starts_with(http.request.uri.path, \"/api/\"))"
    action      = "set_cache_settings"
    action_parameters {
      cache = false
    }
  }

  rules {
    description = "Cache content-hashed static assets"
    expression  = "(starts_with(http.request.uri.path, \"/assets/\"))"
    action      = "set_cache_settings"
    action_parameters {
      cache = true
      edge_ttl {
        mode = "respect_origin" # honor max-age=31536000, immutable
      }
      browser_ttl {
        mode = "respect_origin"
      }
    }
  }
}
```

### Terraform — AWS CloudFront

```hcl
locals {
  managed_caching_disabled  = "4135ea2d-6df8-44a3-9df3-4b5a84be39ad" # CachingDisabled
  managed_caching_optimized = "658327ea-f89d-4fab-a63d-7e88639e58f6" # CachingOptimized
  managed_all_viewer        = "216adef6-5c7f-47e4-b989-5492eafa07d3" # AllViewer (origin request)
}

resource "aws_cloudfront_distribution" "kapp" {
  enabled = true

  origin {
    domain_name = var.origin_domain # Caddy edge hostname
    origin_id   = "kapp-origin"
    custom_origin_config {
      http_port              = 80
      https_port             = 443
      origin_protocol_policy = "https-only"
      origin_ssl_protocols   = ["TLSv1.2"]
    }
  }

  # Default behavior — SPA shell, do not cache HTML at the edge.
  default_cache_behavior {
    target_origin_id       = "kapp-origin"
    viewer_protocol_policy = "redirect-to-https"
    allowed_methods        = ["GET", "HEAD", "OPTIONS"]
    cached_methods         = ["GET", "HEAD"]
    cache_policy_id        = local.managed_caching_disabled
    origin_request_policy_id = local.managed_all_viewer
    compress               = true
  }

  # API — never cache, forward everything.
  ordered_cache_behavior {
    path_pattern             = "/api/*"
    target_origin_id         = "kapp-origin"
    viewer_protocol_policy   = "https-only"
    allowed_methods          = ["GET", "HEAD", "OPTIONS", "PUT", "POST", "PATCH", "DELETE"]
    cached_methods           = ["GET", "HEAD"]
    cache_policy_id          = local.managed_caching_disabled
    origin_request_policy_id = local.managed_all_viewer
    compress                 = false
  }

  # Static assets — cache aggressively, honor immutable.
  ordered_cache_behavior {
    path_pattern           = "/assets/*"
    target_origin_id       = "kapp-origin"
    viewer_protocol_policy = "redirect-to-https"
    allowed_methods        = ["GET", "HEAD", "OPTIONS"]
    cached_methods         = ["GET", "HEAD"]
    cache_policy_id        = local.managed_caching_optimized
    compress               = true
  }

  restrictions {
    geo_restriction { restriction_type = "none" }
  }

  viewer_certificate {
    acm_certificate_arn      = var.acm_certificate_arn
    ssl_support_method       = "sni-only"
    minimum_protocol_version = "TLSv1.2_2021"
  }
}
```

### Pulumi (TypeScript) — Cloudflare

```ts
import * as cloudflare from "@pulumi/cloudflare";

new cloudflare.Ruleset("kapp-edge-caching", {
  zoneId: zoneId,
  name: "kapp-edge-caching",
  kind: "zone",
  phase: "http_request_cache_settings",
  rules: [
    {
      description: "Bypass cache for the API",
      expression: 'starts_with(http.request.uri.path, "/api/")',
      action: "set_cache_settings",
      actionParameters: { cache: false },
    },
    {
      description: "Cache content-hashed static assets",
      expression: 'starts_with(http.request.uri.path, "/assets/")',
      action: "set_cache_settings",
      actionParameters: {
        cache: true,
        edgeTtl: { mode: "respect_origin" },
        browserTtl: { mode: "respect_origin" },
      },
    },
  ],
});
```

> The managed cache-policy IDs above are global AWS constants and are
> stable, but always confirm against
> `aws cloudfront list-cache-policies --type managed` for your account.

## Verifying the configuration

After deploying, confirm the headers end-to-end:

```bash
# API must be no-store
curl -sSI https://$KAPP_DOMAIN/api/v1/ | grep -i cache-control
# -> cache-control: no-store

# A hashed asset must be immutable (grab a real filename from index.html)
asset=$(curl -sS https://$KAPP_DOMAIN/ | grep -oE '/assets/[^"]+\.js' | head -1)
curl -sSI "https://$KAPP_DOMAIN$asset" | grep -i cache-control
# -> cache-control: public, max-age=31536000, immutable

# The SPA shell must revalidate
curl -sSI https://$KAPP_DOMAIN/ | grep -i cache-control
# -> cache-control: no-cache

# Compression is negotiated
curl -sSI -H 'Accept-Encoding: gzip' "https://$KAPP_DOMAIN$asset" | grep -i content-encoding
# -> content-encoding: gzip   (or zstd)
```

If a CDN sits in front, also confirm the cache decision via its debug
header (Cloudflare: `cf-cache-status` — expect `DYNAMIC`/`BYPASS` for
`/api/*` and `HIT` for `/assets/*`; CloudFront: `x-cache` — expect
`Hit from cloudfront` for assets).
