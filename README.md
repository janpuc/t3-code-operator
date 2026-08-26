# t3-code-operator

A Kubernetes operator for [t3-code](https://www.npmjs.com/package/t3): agent
harnesses, the extensions they load, and the MCP servers they talk to, all as
CRDs — reloaded **without restarting sessions**.

## Status

Scaffold. No Go code yet. Read [PLAN.md](PLAN.md) for the implementation brief
and [AGENTS.md](AGENTS.md) for the rules any agent working here must follow.

## The one invariant

A content change must never restart the Workstation pod. Adding an extension,
editing an MCP server or changing a harness does not kill a running agent
session. Only an image or pod-shape change rolls the workload, and that goes
through a drain policy.

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
| `Workstation` | The machine: image, storage, identity, CLI tools, network. The only kind that becomes a Pod. |
| `Harness` | One t3 provider instance. Maps to t3's own `providerInstances`, so two claude instances with different endpoints is the native model. |
| `Extension` | Anything a harness loads — skills, plugins, or both. |
| `MCPServer` | One endpoint, rendered into every dialect its harnesses speak. |

Attachment points **upward**, the way Gateway API routes attach to gateways:

```
Extension.spec.harnessRefs   ──▶  Harness
MCPServer.spec.harnessRefs   ──▶  Harness
Harness.spec.workstationRefs ──▶  Workstation  (= the Pod)
```

Children declare intent; parents declare policy via
`Harness.spec.allowedExtensions`. Adding a skill means creating one file —
nothing gets edited.

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

AGPL-3.0-or-later, matching koment.
