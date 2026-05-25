# ADR-008: gRPC plugin SDK over WASM / native plugins

- Status:   Accepted
- Date:     2024-07-18
- Deciders: Platform leads, security
- Tags:     plugins, sdk, extensibility

## Context

Third parties want to extend Kapp without forking it: custom agent
tools, integration adapters, workflow guards, event consumers, report
sources. Three plugin-isolation models compete:

1. **In-process Go plugins** via Go's `plugin` package.
2. **WASM** — plugins compiled to WASM, loaded by a host runtime
   (Wasmtime / Wasmer).
3. **Out-of-process** plugins speaking gRPC over a local socket or
   network.

Sandbox guarantees, language-of-choice flexibility, and operational
tractability are the dominant constraints.

## Decision

Use **gRPC over a Unix domain socket or TCP** as the plugin
interface:

- Plugins are independent processes / containers exposing a
  well-known gRPC service set defined in `proto/kapp/v1/*.proto`.
- The kernel calls plugins with per-call deadlines (30 s default),
  per-tenant rate limits, and a short-lived JWT scoped to the
  invocation.
- Plugins run in their own process / pod (Kubernetes scheduling) so
  a crash, memory leak, or CPU hog is contained.
- The SDK ships in Go (`plugin-sdk/`) and Rust (`packages/sdk-rs/`),
  with reference implementations for the major extension points.

## Alternatives considered

1. **Go `plugin` package (in-process .so loading)**. Rejected:
   plugins MUST match the host Go version exactly, can't be
   sandboxed (full process access), and ship as native binaries —
   no isolation, no portability.
2. **WASM**. Considered seriously. Rejected for v1 because the
   ecosystem for "outbound HTTP + gRPC client + DB access" is still
   immature (each adds non-trivial host functions to the runtime).
   We will revisit when those gaps close; the gRPC plugin model can
   coexist with WASM-based plugins later.
3. **Native shared libraries (C ABI)**. Rejected: same isolation
   problem as Go plugins, plus a C ABI for every interface.

## Consequences

- **Positive**:
  - Plugin authors choose any language with gRPC bindings
    (Go, Rust, Python, Node, Java).
  - Crash / OOM / CPU hog in the plugin doesn't take down the
    kernel.
  - Plugin updates roll independently of the kernel deployment.
  - Auditing is uniform: every gRPC call is a logged + metered event.
- **Negative**:
  - Higher per-call overhead than in-process (~100 µs vs. < 1 µs).
    Mitigation: gRPC streaming and batching for high-frequency calls;
    avoid using plugins for hot inner loops.
  - More processes to deploy and monitor.
- **Operational**:
  - Plugin certification checklist: [PLUGIN_DEVELOPER_GUIDE.md §13.5](../PLUGIN_DEVELOPER_GUIDE.md#135-testing-and-certification).
  - Security model: [PLUGIN_DEVELOPER_GUIDE.md §13.4](../PLUGIN_DEVELOPER_GUIDE.md#134-security-model).

## References

- `proto/kapp/v1/`
- [PLUGIN_DEVELOPER_GUIDE.md](../PLUGIN_DEVELOPER_GUIDE.md)
- [AGENT_PLATFORM_OVERVIEW.md](../AGENT_PLATFORM_OVERVIEW.md)
