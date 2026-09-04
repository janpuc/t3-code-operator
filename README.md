# t3-code-operator

A Kubernetes operator for upstream
[t3-code](https://www.npmjs.com/package/t3): provider instances, extensions,
MCP servers, and persistent Workstations as CRDs.

## Status

Phases 1 through 4 are implemented and pass local contracts. These phases cover
the API, five adapters, transactional sidecar, upstream t3 control, and four
controllers.

Phase 5 has a pinned multi-architecture image definition, stable and nightly
locks, signed-image workflows, and a Helm chart. The chart contract covers NFS,
PVC workspace sharing through SMB, HTTPRoute, project RBAC, and arbitrary
`extraObjects`.

Phase 6 has workstation Git identity, GitHub CLI credentials, SSH signing,
machine information, repository safe directories, secure pod defaults, and a
documented idle migration. Read
[the migration procedure](docs/migration-from-helmrelease.md).

A canary Workstation on a real cluster now passes live acceptance: image
rollouts gated by the drain policy, the SMB share, pinned runtime tools,
extensions installed in all three dialects, and a content change reaching a
new live revision without a pod restart. The production Workstation runs on
this operator. Version 0.2.0 made the API opinionated: providers live inline
on the Workstation and opt in explicitly, every other field defaults, and
Extensions and MCPServers attach to every capable provider unless narrowed.
Cursor, Grok, and Antigravity remain alpha until authenticated end-to-end
environments are available. Read [PLAN.md](PLAN.md) for the original design
and [AGENTS.md](AGENTS.md) for repository rules.

## The one invariant

A content change must never restart the Workstation pod. Adding an extension,
editing an MCP server or changing a harness does not kill a running agent
session. Only an image or pod-shape change rolls the workload, and that goes
through a drain policy.

The drain waits while t3 runs a turn, tool call, or tracked background work. It
may roll after the turn finishes and the session waits for its next human
message. The default timeout action blocks the rollout instead of terminating
work.

## Why

Configuring several harnesses in one pod today means a hand-maintained ConfigMap
where the same seven MCP servers appear three times in three syntaxes, ~180
lines of shell installing plugins imperatively, `reloader` wired to restart on
every ConfigMap edit, and `strategy: Recreate` — so changing configuration
destroys in-flight work.

## Quick start

Install the operator, then create one object:

```yaml
apiVersion: t3code.janpuc.com/v1alpha1
kind: Workstation
metadata:
  name: t3-code
  namespace: agents
spec:
  providers:
    codex:
      enabled: true
```

Everything else has a default: the runtime image pinned by the operator
release, retained `/data` and `/workspace` claims on the default storage class,
the drain policy, and the environment name shown in T3 clients, which is the
Workstation name. Providers are opt-in; each one needs an explicit `enabled`.
The [examples](examples/) grow from that object to a shared-workspace,
multi-provider setup. Read the [Workstation reference](docs/workstation.md)
and the [providers guide](docs/providers.md) for every field and default.

## Kinds

`t3code.janpuc.com/v1alpha1` — all namespaced.

| Kind | Purpose |
|---|---|
| `Workstation` | The machine: image, retained data, PVC or NFS workspace, optional SMB sharing, identity, tools, network, and its inline providers. The only kind that becomes a Pod. |
| `Harness` | One upstream t3 provider instance shared across Workstations, or one whose instance ID is not a DNS label. |
| `Extension` | Anything a provider loads — skills, plugins, or both. |
| `MCPServer` | One endpoint, rendered into every dialect its providers speak. |

Attachment points **upward**, the way Gateway API routes attach to gateways:

```
Extension.spec.harnessRefs   ──▶  provider (Workstation.spec.providers key or Harness)
MCPServer.spec.harnessRefs   ──▶  provider
Harness.spec.workstationRefs ──▶  Workstation  (= the Pod)
```

Children declare intent; parents declare policy via `attachmentPolicy`. A
child that names no target attaches to every provider in its namespace whose
driver can program it, so adding a skill or an MCP server means creating one
file and touching nothing else. References use exact names in one namespace.

The operator implements all six upstream drivers: `codex`, `claudeAgent`,
`opencode`, `cursor`, `grok`, and `antigravity`. Cursor, Grok, and Antigravity
are alpha until authenticated end-to-end environments are available; the
Antigravity driver ships in t3's nightly track first, so it needs a nightly
runtime image.

Upstream t3 owns settings, sessions, and provider orchestration. The existing
container wrapper can inform image construction, but its `t3code.toml` contract
does not define this API.

The runtime image has a fixed, pinned tool baseline. `Workstation.spec.tools`
adds pinned tools through mise without a pod rollout. Retained `/data` keeps
the tool cache and session state.

The chart installs the operator and CRDs. It can also template project CRs and
arbitrary `extraObjects`, including HTTPRoutes, ServiceAccounts, Roles, and
RoleBindings.

A Workstation can keep `/workspace` on a fast PVC and share it directly through
SMB. Read [the SMB workspace guide](docs/workspace-smb.md).

## Install

The release publishes three signed images — the operator
(`ghcr.io/janpuc/t3-code-operator`), the Workstation runtime with the
`t3-coded` sidecar (`ghcr.io/janpuc/t3-code-runtime`), and the SMB
workspace sidecar (`ghcr.io/janpuc/t3-code-smbd`) — plus a signed OCI
Helm chart:

```sh
helm install t3-code-operator \
  oci://ghcr.io/janpuc/charts/t3-code-operator \
  --namespace t3-code-system \
  --create-namespace \
  --version 0.1.7
```

Pin the operator and SMB images to the digests from the release notes. The
published chart already pins `workstation.image` to the runtime digest of the
same release, so a Workstation without `spec.image` runs that runtime.

The stable runtime tracks the latest t3 release. A nightly runtime with the
newest upstream drivers is published daily as
`ghcr.io/janpuc/t3-code-runtime:nightly`; pin its digest in `spec.image` to
use it.

## Ecosystem

- **t3-code-operator** — this repository: operator, CRDs, chart, sidecar, images

Skill content is **not** vendored into a repository of its own. `SKILL.md` is a
settled standard that every major harness reads, and a plain git repository of
`skills/<name>/SKILL.md` is what the ecosystem consumes, so an `Extension` with
a `Git` source points at any upstream directly — no repackaging step. See
PLAN.md section 3.10 for what is and is not standardised, and why the Agent
Skills OCI Artifacts draft is an optional additive rather than the primary
path.

## License

AGPL-3.0-or-later.
