# t3-code-operator

A Kubernetes operator for [t3-code](https://www.npmjs.com/package/t3): agent
harnesses, the skills and plugins they load, and the MCP servers they talk to,
all as CRDs — reloaded **without restarting sessions**.

## Status

Scaffold. No Go code yet. Read [PLAN.md](PLAN.md) for the implementation brief
and [AGENTS.md](AGENTS.md) for the rules any agent working here must follow.

## The one invariant

A content change must never restart the runtime pod. Adding a skill, editing an
MCP server or enabling a plugin does not kill a running agent session. Only an
image or pod-shape change rolls the workload, and that goes through a drain
policy.

## Why

Configuring three harnesses in one pod today means a large hand-maintained
ConfigMap where the same seven MCP servers appear three times in three
syntaxes, a shell init container that installs plugins imperatively, and
`strategy: Recreate` — so every configuration change destroys in-flight work.

This operator makes that configuration declarative, validated, rendered once
from a single model, and delivered without a restart.

## Kinds

`t3code.janpuc.com/v1alpha1`

| Kind | Purpose |
|---|---|
| `MCPServer` | One MCP endpoint, rendered into all three harness dialects |
| `Skill` | One skill, from inline content, git or OCI |
| `SkillBundle` | A versioned batch, pinned by version and digest |
| `Plugin` | A harness plugin and its marketplace source |
| `Harness` | A configured agent CLI and what it selects |
| `Runtime` | The workload, its image track and its drain policy |

## Ecosystem

- **t3-code-operator** — this repository: operator, CRDs, chart, sidecar, image
- **[agent-kit](https://github.com/janpuc/agent-kit)** — skill content, consumed
  via `SkillBundle`

## License

AGPL-3.0-or-later, matching koment.
