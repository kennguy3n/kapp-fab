# Plugin Developer Guide

Kapp's runtime is open at well-defined seams: KTypes, agent tools,
workflow guards, event consumers, integration adapters, and report
sources. This document is for **plugin authors** (third parties
extending Kapp without modifying the kernel).

The runtime extension surface is realised by:

- gRPC services in `proto/kapp/v1/*.proto`
- In-tree adapters under `internal/plugins/` (planned in v0.2)
- Sandboxed execution via the agent tool registry
  (`internal/agents/registry.go`)

Cross-references:

- KType authoring: [KTYPE_AUTHORING_GUIDE.md](./KTYPE_AUTHORING_GUIDE.md)
- Agent platform: [AGENT_PLATFORM_OVERVIEW.md](./AGENT_PLATFORM_OVERVIEW.md)
- API patterns: [API_REFERENCE.md](./API_REFERENCE.md)
- Architectural rationale: [adr/008-grpc-plugin-sdk.md](./adr/008-grpc-plugin-sdk.md)

---

## 13.1 Getting Started

### Install the SDK

```bash
# Go SDK (mirrors kapp's own proto definitions):
go get github.com/kennguy3n/kapp-fab/plugin-sdk@latest

# Rust SDK:
cargo add kapp-plugin-sdk
```

### Hello-World plugin

```go
package main

import (
    "context"
    "log"

    pb       "github.com/kennguy3n/kapp-fab/gen/kapp/v1"
    sdk      "github.com/kennguy3n/kapp-fab/plugin-sdk"
)

func main() {
    sdk.RegisterAgentTool(sdk.AgentTool{
        Name:        "hello.world",
        Description: "Replies with hello",
        Invoke: func(ctx context.Context, req *pb.AgentToolRequest) (*pb.AgentToolResponse, error) {
            return &pb.AgentToolResponse{
                Result: &pb.AgentToolResponse_Json{
                    Json: `{"message":"hello"}`,
                },
            }, nil
        },
    })
    log.Fatal(sdk.Serve())
}
```

Run locally against a Kapp dev server:

```bash
KAPP_PLUGIN_ID=hello-world \
KAPP_PLUGIN_SECRET=$(openssl rand -hex 32) \
KAPP_API_BASE=http://localhost:8080 \
  go run ./examples/hello
```

Register the plugin with a tenant:

```bash
curl -XPOST -H "Authorization: Bearer $ADMIN_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"name":"hello","endpoint":"grpc://hello-world.svc:9000","secret":"<token>"}' \
  https://api.kapp.example.com/api/v1/admin/plugins
```

---

## 13.2 Plugin Architecture

```
   ┌───────────────┐
   │ Kapp Kernel   │              ┌──────────────────┐
   │ ───────────── │  gRPC + JWT  │ Plugin           │
   │ Plugin Bus    │◀────────────▶│ (your code)      │
   │ Sandbox       │              │ implements one   │
   │ Audit         │              │ or more services │
   └───────┬───────┘              └──────────────────┘
           │
           │  Per-call:
           │   • Tenant context injected (tenant_id, user_id)
           │   • gRPC deadline (30s default)
           │   • Outbound rate-limit token
           │
           ▼
     Audit log entry per invocation:
       action = "plugin.invoke"
       payload.plugin_id, payload.tool, payload.duration_ms
```

**Lifecycle.**

1. **Register** — admin onboards the plugin via
   `POST /api/v1/admin/plugins`. Endpoint URL and signing secret are
   stored in `plugins` (`migrations/000054_plugins.sql`).
2. **Health check** — kernel calls `Plugin.HealthCheck` every 30 s;
   3 consecutive failures mark the plugin `unhealthy` and stop
   routing requests to it.
3. **Invoke** — kernel sends a `PluginRequest` with tenant context;
   plugin responds with `PluginResponse` (or error).
4. **Audit** — every invocation appends to `audit_log`.
5. **Suspend / remove** — admin can disable; existing invocations
   complete, new ones rejected.

**Tenant context.** Every gRPC call carries metadata:

```
kapp-tenant-id:   <UUID>
kapp-user-id:     <UUID>
kapp-roles:       comma-separated role names
kapp-request-id:  request correlation ID
kapp-trace-id:    OpenTelemetry trace ID
authorization:    Bearer <short-lived JWT signed for this plugin>
```

The plugin SDK validates the JWT and exposes the tenant context via
`sdk.TenantContext(ctx)`.

---

## 13.3 Extension Points

| Extension                | Interface                                       | Use case                                                       |
| ------------------------ | ----------------------------------------------- | -------------------------------------------------------------- |
| Custom KType             | KType definition (REST `POST /api/v1/ktypes`)   | Add a new record type with custom fields and workflows         |
| Agent tool               | `pb.AgentTool` (gRPC `Invoke`)                  | A capability the agent can call (e.g. "send-email")            |
| Workflow guard           | `pb.WorkflowGuard` (gRPC `EvaluateTransition`)  | Conditional logic on workflow transitions                      |
| Workflow action          | `pb.WorkflowAction` (gRPC `Execute`)            | Side-effecting step in a workflow                              |
| Event consumer           | `pb.EventConsumer` (gRPC `Consume`)             | React to platform events without polling                        |
| Integration adapter      | `pb.IntegrationAdapter` (gRPC `Connect`, `Sync`)| OAuth flow + recurring sync (CRM, accounting, etc.)            |
| Report source            | `pb.ReportSource` (gRPC `Query`)                | Plug a custom data source into the insights query builder       |
| Custom widget            | React component (registered via web SDK)        | Add a dashboard widget rendering plugin-sourced data           |
| Notification channel     | `pb.NotificationChannel` (gRPC `Send`)          | Add a delivery channel (SMS, Telegram, etc.)                   |

Every extension is named, versioned (semver), and namespaced by
publisher ID so two plugins can ship overlapping capabilities
without conflicts.

---

## 13.4 Security Model

**Permissions.** A plugin declares the permissions it needs at
registration time. The kernel rejects an invocation if the plugin
attempts to use a permission outside its grants. Permission scopes:

- `records.read|write:<ktype>` — KRecord access scoped to a KType
- `audit.read` — read audit_log
- `events.subscribe:<event-name>` — subscribe to events
- `webhook.send` — send outbound webhooks
- `integration.oauth:<provider>` — issue OAuth tokens
- `agent.invoke` — be invocable as an agent tool
- `files.read|write` — file storage access

**Tenant isolation.** A plugin call carries the originating tenant
context; the SDK injects `app.tenant_id` into every DB call. The
plugin **cannot** access other tenants unless the kernel grants an
explicit cross-tenant permission via a platform-admin endpoint.

**Audit.** Every invocation creates an `audit_log` row:

```sql
SELECT created_at, payload->>'plugin_id', payload->>'tool', payload->>'duration_ms'
FROM audit_log
WHERE tenant_id = $1 AND action = 'plugin.invoke'
ORDER BY created_at DESC LIMIT 50;
```

**Sandboxing.** The plugin runs in a separate process (your container
/ binary). The kernel never trusts plugin output:

- gRPC deadlines (30 s default)
- Per-tenant rate limits
- Schema validation on every response
- Quotas: invocations/hour, payload size cap, error budget
- Outbound network egress is whitelisted by the operator (see
  [INFRASTRUCTURE.md §15.3](./INFRASTRUCTURE.md#153-network-policies))

---

## 13.5 Testing and Certification

### Local test harness

The SDK ships `sdk.TestHarness` — an in-memory kernel that exposes
the same gRPC surface for unit tests:

```go
func TestHelloWorld(t *testing.T) {
    h := sdk.NewTestHarness(t)
    defer h.Close()

    h.RegisterAgentTool(sdk.AgentTool{Name: "hello.world", Invoke: invoke})

    resp, err := h.Invoke(ctx, "hello.world", &pb.AgentToolRequest{
        TenantId: "00000000-0000-0000-0000-000000000000",
        UserId:   "00000000-0000-0000-0000-000000000001",
        Input:    &pb.AgentToolRequest_Json{Json: `{}`},
    })
    require.NoError(t, err)
    assert.JSONEq(t, `{"message":"hello"}`, resp.GetJson())
}
```

### Marketplace certification checklist

A plugin must satisfy all of the following before being published in
the Kapp Marketplace:

- [ ] Signed by a publisher key registered with the marketplace.
- [ ] Permission requests minimal — every permission listed in the
      manifest is actually invoked by at least one code path
      (verified by the publish CI).
- [ ] gRPC services pass `buf breaking` against the previously
      published version.
- [ ] Test coverage on the public extension methods ≥ 80 %.
- [ ] Returns a stable error payload (`code`, `message`).
- [ ] Documentation: README, CHANGELOG, permission rationale.
- [ ] Security review by the marketplace team for plugins that
      request `audit.read` or any cross-tenant scope.
- [ ] Performance: p99 invocation latency < 500 ms on the marketplace
      bench (measured against the harness with a 100-rps profile).
- [ ] Tenant-data hygiene: no logging of `tenant_id` together with
      PII unless the operator opts in; verified by static analysis.
- [ ] Compliance attestation: SOC 2 letter for publishers handling
      financial data; GDPR DPA for EU-region operators.

### Publishing flow

```bash
# 1. Build a release artefact (multi-arch container)
docker buildx build --platform linux/amd64,linux/arm64 \
  -t ghcr.io/<publisher>/<plugin>:v1.0.0 --push .

# 2. Submit manifest to the marketplace
kapp-cli marketplace submit \
  --plugin "<publisher>/<plugin>" \
  --version v1.0.0 \
  --manifest plugin.yaml

# 3. Wait for the security + perf review to pass.
# 4. Promote to the registry:
kapp-cli marketplace promote --plugin "<publisher>/<plugin>" --version v1.0.0
```

Manifest example:

```yaml
# plugin.yaml
api:        v1
id:         publisher/plugin
version:    v1.0.0
description: Example plugin
entrypoint:
  image:    ghcr.io/publisher/plugin:v1.0.0
  port:     9000
permissions:
  - records.read:crm.deal
  - events.subscribe:record.created
extensions:
  agent_tools:
    - name: hello.world
      description: Replies with hello
```
