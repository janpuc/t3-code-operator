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

Real Kubernetes image, drain, SMB, and NAS acceptance remains pending because
this host has no container runtime. Cursor and Grok remain alpha until
authenticated end-to-end environments are available. Read [PLAN.md](PLAN.md)
for every release gate and [AGENTS.md](AGENTS.md) for repository rules.

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

## Kinds

`t3code.janpuc.com/v1alpha1` — all namespaced.

| Kind | Purpose |
|---|---|
| `Workstation` | The machine: image, retained data, PVC or NFS workspace, optional SMB sharing, identity, tools, and network. The only kind that becomes a Pod. |
| `Harness` | One upstream t3 provider instance. Two instances can use the same driver with different config. |
| `Extension` | Anything a harness loads — skills, plugins, or both. |
| `MCPServer` | One endpoint, rendered into every dialect its harnesses speak. |

Attachment points **upward**, the way Gateway API routes attach to gateways:

```
Extension.spec.harnessRefs   ──▶  Harness
MCPServer.spec.harnessRefs   ──▶  Harness
Harness.spec.workstationRefs ──▶  Workstation  (= the Pod)
```

Children declare intent; parents declare policy via
`Harness.spec.attachmentPolicy`. MVP references use exact names in one
namespace. Adding a skill means creating one file. No parent object changes.

The first release implements all five upstream drivers: `codex`,
`claudeAgent`, `cursor`, `grok`, and `opencode`. Cursor and Grok are alpha until
authenticated end-to-end environments are available. Their installation and
provider-registration smoke tests still gate release.

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

The release publishes a signed runtime image and signed OCI Helm chart:

```sh
helm install t3-code-operator \
  oci://ghcr.io/janpuc/charts/t3-code-operator \
  --namespace t3-code-system \
  --create-namespace \
  --version 0.1.3
```

Pin the operator and Workstation images to the digest from the release notes.

## Ecosystem

- **t3-code-operator** — this repository: operator, CRDs, chart, sidecar, image

Skill content is **not** vendored into a repository of its own. `SKILL.md` is a
settled standard that every major harness reads, and a plain git repository of
`skills/<name>/SKILL.md` is what the ecosystem consumes, so an `Extension` with
a `Git` source points at any upstream directly — no repackaging step. See
PLAN.md section 3.10 for what is and is not standardised, and why the Agent
Skills OCI Artifacts draft is an optional additive rather than the primary
path.

## License

AGPL-3.0-or-later.
