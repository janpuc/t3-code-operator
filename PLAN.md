# t3-code-operator — implementation plan

Status: scaffold. No Go code exists yet. This document is the brief for the next
agent session.

API group: `t3code.janpuc.com`, version `v1alpha1`.

## 1. Why this exists

t3-code runs three agent harnesses in one pod: Claude Code, Codex and OpenCode.
Configuring them today means hand-maintaining one large ConfigMap and a shell
init container. That arrangement has four problems which are really one problem.

| Today | Consequence |
|---|---|
| The same seven MCP servers are written three times, in three syntaxes, in one ConfigMap | Every change is a three-place edit and drift is invisible until a harness misbehaves |
| Roughly a hundred lines of shell install plugins with `gh release download`, `sha256sum` and `plugin add` | Imperative, untestable, and every failure branch ends in `\|\| echo "... unavailable"`, so a broken install looks like a successful boot |
| Skills are provisioned by upstream Python with a path bug | Half the harnesses get the skills and nobody is told |
| `strategy: Recreate` with `replicas: 1` | **Every configuration change destroys in-flight agent sessions** |

The last row is the indictment. Adding a skill should not kill work.

The operator's job is to make configuration declarative, validated, rendered
once from a single model, and delivered without restarting anything.

## 2. Requirements

1. Kubernetes operator plus Helm chart plus a fully custom image.
2. The image is built on t3-code's **nightly and release** channels.
3. Skills and plugins are CRDs, plug and play, **auto-reloading when modified**.
4. Harness configuration is CRDs too.
5. It is an ecosystem, not a single controller.

## 3. Verified ground truth

Verified on 2026-08-26 against upstream sources and a live container. Do not
re-derive; do challenge if an upstream release lands.

### 3.1 What t3-code actually is

t3-code is the npm package **`t3`**. It is not a binary release and not a
container-only artifact, which makes a custom image straightforward.

- dist-tags: `latest` = `0.0.34`, `nightly` = `0.0.35-nightly.20260826.1195`,
  `alpha` = `0.0.2`. 432 published versions.
- The nightly tag moves **several times a day**. Build 1195 and build 1193 were
  both published on 2026-08-26.
- The harness CLIs are also npm: `@openai/codex`, `@anthropic-ai/claude-code`,
  `opencode-ai`.

**Gotcha that will silently produce a broken image:**
`@anthropic-ai/claude-code` ships a stub; `node install.cjs` inside the package
directory must run afterwards to materialise `bin/claude.exe`, which is roughly
342 MB. Upstream's installer asserts the result is larger than 4096 bytes and
fails the build otherwise. Reproduce that assertion.

### 3.2 Filesystem contract inside the runtime

| Variable | Value | Owner |
|---|---|---|
| `T3CODE_HOME` | `/data/t3` | t3 |
| `T3CODE_CONFIG_PATH` | `/config/t3code.toml` | t3 |
| `HOME` | `/data/home` | shared |
| `CODEX_HOME` | `/data/codex` | Codex |
| `CLAUDE_CONFIG_DIR` | `/data/claude-home` | Claude Code |
| OpenCode config | `/data/home/.config/opencode/opencode.jsonc` | OpenCode |

### 3.3 Skill discovery differs per harness

| Path | Claude | Codex | OpenCode |
|---|:--:|:--:|:--:|
| `$CLAUDE_CONFIG_DIR/skills/`, `.claude/skills/` | yes | no | yes |
| `~/.agents/skills/`, `.agents/skills/` | no | yes | yes |
| `~/.config/opencode/skills/`, `.opencode/skills/` | no | no | yes |
| `$CODEX_HOME/skills`, `/etc/codex/skills` | no | yes | no |

Evidence: `AGENTS_DIR_NAME = ".agents"` in
`codex-rs/ext/skills/src/host_roots.rs:24`; `CLAUDE_EXTERNAL_DIR` and
`AGENTS_EXTERNAL_DIR` in `packages/opencode/src/skill/index.ts:21-22`; the
Claude binary contains `.claude/skills` and zero occurrences of
`.agents/skills`.

**No single directory serves all three.** The sidecar writes to two, and
OpenCode deduplicates by skill name so the overlap is free.

`$CLAUDE_CONFIG_DIR` is the config root itself. Appending `.claude` to it
produces a path Claude does not read. That mistake is live upstream in
`traktuner/docker-t3-code`; do not reproduce it.

### 3.4 Reload capability differs per harness

| Harness | Mechanism | Honest strategy |
|---|---|---|
| Codex | `skills_watcher.rs` in the app-server watches for skill changes | `Watch` |
| Claude Code | `/reload-plugins`; skills are read per session | `NextSession` |
| OpenCode | managed server, discovery per session | `NextSession` or `Signal` |

So the guarantee this project can honestly make is **not** "hot reload into a
running conversation". It is: *new sessions see new state immediately, and the
pod never restarts for a content change.* Model this as a per-harness
`reloadStrategy` and report what is live versus pending in status. Do not
promise more than the harness can do.

### 3.5 The three MCP dialects

One logical server renders three ways. This is the single largest source of
duplication in the current setup.

| | Claude (`.mcp.json`) | Codex (`config.toml`) | OpenCode (`opencode.jsonc`) |
|---|---|---|---|
| Kind | `"type": "http"` | table `[mcp_servers.x]` | `"type": "remote"` |
| Secret | `"Authorization": "Bearer ${VAR}"` | `bearer_token_env_var = "VAR"` | `"Bearer {env:VAR}"` |
| Extra headers | `headers` map | `[mcp_servers.x.env_http_headers]` | `headers` map |

All three take an environment **reference**, never a literal. That is what lets
rendered output stay non-sensitive.

A bonus the operator removes for free: the current ConfigMap must write
`$${LITELLM_API_KEY}` because Flux performs variable substitution on the
manifest. CRDs carry no such escaping hazard.

### 3.6 t3-code syncs config itself, and will fight you

`t3code.toml` contains, per provider, `config_dir_source`, `config_source`,
`config_path` and `config_sync_mode` (currently `preserve-mcp` for OpenCode).
t3 copies configuration from those sources into the provider home directories.

If the sidecar writes directly into provider homes while t3 also syncs into
them, the two will overwrite each other, and the loser will vary with timing.
**This is the highest-risk unknown in the whole design** and Phase 0 must settle
it before any controller is written. See open question 1.

### 3.7 The workload today

`replicas: 1`, `strategy: Recreate`. Volumes: `data` (RWO PVC), `workspace`
(NFS, `bigmouth.internal:/mnt/Vault/Workspace`), `config` (emptyDir),
`config-src` (ConfigMap), `git-signing` (Secret). Seven MCP servers, two
plugins (`koment@koment-dev`, `memini@memini`).

## 4. Architecture

Two planes. The split is the design.

```
  CRDs ──▶ operator ──▶ rendered ConfigMap ──▶ t3-coded ──▶ filesystem ──▶ reload
           (decides)      (one per Runtime)      (applies)
                                  │
  OCI bundles ────────────────────┴──────────▶ pulled by digest
```

**Operator** — watches CRDs, validates, resolves OCI tags to digests, renders
one logical model into three harness dialects, writes exactly one ConfigMap per
`Runtime`, and owns the Deployment.

**`t3-coded`** — the in-pod sidecar. Watches that one ConfigMap, pulls OCI
bundles by digest, materialises files, triggers the per-harness reload, reports
what is actually live.

Why render centrally instead of letting the sidecar watch CRDs directly:

- All dialect knowledge lives in one testable package instead of in a process
  that is hard to reach.
- The sidecar needs RBAC on one object, not on six kinds.
- `kubectl get cm t3-code-rendered-<runtime> -o yaml` shows exactly what the pod
  will see, which makes debugging a diff rather than an investigation.

**Fail open.** If the operator stops, the sidecar keeps the last materialised
state and sessions keep working. Nothing in the session path may require the
control plane to be healthy.

## 5. CRDs

Six kinds. Each one has to earn its place; `Workspace` deliberately does not
ship in v1alpha1.

### 5.1 MCPServer

The largest de-duplication win: define once, render three ways.

```yaml
apiVersion: t3code.janpuc.com/v1alpha1
kind: MCPServer
metadata:
  name: kubectl
  labels: {tier: cluster}
spec:
  transport: http
  url: http://litellm.ai.svc.cluster.local:4000/mcp/kubectl/mcp
  auth:
    bearerToken:
      envVar: LITELLM_API_KEY
      secretRef: {name: t3-code-secret, key: litellm-api-key}
  headers:
    X-Memini-Home: {envVar: MEMINI_HOME}
```

The operator ensures `envVar` is present in the pod from `secretRef`, and emits
only the reference into rendered config.

### 5.2 Skill

```yaml
kind: Skill
spec:
  source:                       # exactly one of
    inline: {skillMd: "---\nname: ...\n---\n..."}
    git:    {repo: ..., ref: ..., path: ...}
    oci:    {ref: ..., digest: ...}
  compatibility: [claude, codex, opencode]
```

`compatibility` is a fact about the skill, not a policy. A skill whose value
depends on a Claude-only `agents/*.md` subagent says so here and degrades
honestly on the other two.

### 5.3 SkillBundle

How `janpuc/agent-kit` plugs in, and exactly what its one-version-plus-one-sha
design was built for.

```yaml
kind: SkillBundle
spec:
  source:
    oci: {ref: ghcr.io/janpuc/agent-kit, version: "0.1.0", digest: "sha256:..."}
  include: [tdd, research]      # optional subset; empty means all
status:
  skills: [...]
  resolvedDigest: sha256:...
```

### 5.4 Plugin

Replaces the init-container shell entirely.

```yaml
kind: Plugin
spec:
  name: koment
  harnesses: [claude, codex]
  marketplace:
    claude: {type: github, repo: koment-dev/koment}
    codex:  {type: release, repo: koment-dev/koment, assetPattern: "koment-plugin-codex_v*.tar.gz"}
  disableBundledMCPServers: [koment]
```

`disableBundledMCPServers` mirrors what the current Codex config does by hand,
so a plugin's own MCP server does not duplicate one already declared as an
`MCPServer`.

### 5.5 Harness

```yaml
kind: Harness
metadata: {name: claude}
spec:
  provider: claude              # claude | codex | opencode
  model: ...
  effort: xhigh
  reloadStrategy: NextSession   # Watch | Signal | NextSession | Restart
  selectors:
    skills:     {matchLabels: {set: core}}
    plugins:    {matchLabels: {}}
    mcpServers: {matchLabels: {tier: cluster}}
```

Skills declare compatibility; Harnesses select by label. That is the
Prometheus-operator pattern and it reads naturally to anyone who has run it.

### 5.6 Runtime

The workload, and the only kind that may cause a rollout.

```yaml
kind: Runtime
spec:
  image:
    repository: ghcr.io/janpuc/t3-code
    track: stable               # stable | nightly
    digest: sha256:...
  harnessSelector: {matchLabels: {runtime: primary}}
  drain:
    policy: WaitForIdle         # WaitForIdle | Immediate
    timeout: 30m
```

`drain` is the direct answer to `strategy: Recreate` destroying sessions. An
image bump waits for the harnesses to go idle rather than guillotining them.

## 6. What may and may not cause a rollout

This table is the contract. Encode it as a test.

| Change | Rollout? |
|---|---|
| Skill, SkillBundle, Plugin, MCPServer content | **Never** |
| Harness config (model, effort, selectors) | **Never** |
| `Runtime.spec.image` | Yes, via drain policy |
| Pod shape: volumes, resources, env set | Yes, via drain policy |

If a controller bumps a pod template hash for anything in the first two rows,
the project has failed at its one job.

## 7. Delivery and reload

Two content channels, chosen by size and shape:

- **Rendered config** (`t3code.toml`, Claude `settings.json` and `.mcp.json`,
  Codex `config.toml`, `opencode.jsonc`) travels in the rendered ConfigMap.
  Small, text, benefits from being diffable.
- **Skill and plugin trees** are multi-file and unbounded, so they travel as
  **OCI artifacts pulled by digest**. No ConfigMap size ceiling, no key-name
  restriction on nested paths, and content addressing that matches the
  one-version-plus-one-sha model already used by `agent-kit`.

The sidecar materialises to an **emptyDir**, not the RWO PVC. This is
deliberate: it keeps per-pod state per-pod, so a future replica count above one
remains possible even though today the workload is `Recreate`/1.

Skills are written to **both** discovery roots, because section 3.3 shows no
single root serves all three harnesses:

- `$HOME/.agents/skills/<name>/` for Codex and OpenCode
- `$CLAUDE_CONFIG_DIR/skills/<name>/` for Claude Code

Writes are atomic: stage into a temporary directory, then rename. A harness must
never observe a half-written `SKILL.md`.

The renderer touches config files by **explicit allow-list**. Harness home
directories hold authentication and session state beside configuration; the
sidecar does not sync directories and does not delete what it did not write.

## 8. The image

`images/runtime/Dockerfile`, built and released from this repository so the
sidecar and the image share one release train.

- Base `node:26-bookworm-slim`, matching what upstream proved works.
- `npm i -g t3@${T3_VERSION}` plus the three harness CLIs, each pinned by its
  own build argument so Renovate can move them independently.
- Run `node install.cjs` for `@anthropic-ai/claude-code` and assert the
  resulting binary exceeds 4096 bytes. See section 3.1.
- Bake in `t3-coded`, plus `koment`, `memini` and the mise-managed cluster tools
  already relied on.

Two tracks:

| Track | Source | Cadence | Tags |
|---|---|---|---|
| `stable` | `t3@latest`, pinned to an exact version | on release | `:0.0.34`, `:stable` |
| `nightly` | `t3@nightly` | cron, daily | `:nightly-20260826`, `:nightly` |

`Runtime` pins a **digest**, never a floating tag, so a bad nightly is a
one-field revert. Keep the last-known-good digest in `Runtime.status` so that
revert does not require going and looking it up.

**What the image loses is the point.** Every provisioning script upstream ships
(`provision-*.py`, `configure-*-mcp.py`, `provision-harness-mcp.sh`) is deleted.
The operator owns that behaviour now, in Go, with tests. The image becomes a
thin, boring runtime.

## 9. The chart

`charts/t3-code-operator` installs the CRDs, the operator, and its RBAC.
A `Runtime` may optionally be templated from values so a fresh install is plug
and play, but the operator and the workload stay separable: someone should be
able to run the operator and manage `Runtime` objects with Flux instead.

CRDs ship in `charts/t3-code-operator/crds/` following the layout already used
by `miroir`.

## 10. Phases

Each phase states what it accepts on. A phase is done when its criterion is
demonstrated, not argued.

### Phase 0 — Settle the unknowns

Answer every question in section 12 empirically, in a live container, before any
controller is written. Record each answer with `koment add`.

Question 1 in particular can invalidate section 7's delivery design. Do not
build on an assumption here.

*Accepts when:* every open question has an observed command output attached.

### Phase 1 — API types

`api/v1alpha1` for all six kinds, with kubebuilder validation markers carrying
as much of the schema as CRD validation can express. No controllers yet.

*Accepts when:* `make manifests generate` is clean, CRDs install into a kind
cluster, and an intentionally invalid `MCPServer` is rejected by the API server
rather than by a controller.

### Phase 2 — The renderer

`internal/render`: pure functions from a resolved model to the three dialects.
No Kubernetes client, no I/O, table-driven golden tests.

Start here rather than with controllers. It is the highest-value, most testable
part, and it is where the duplication actually dies.

*Accepts when:* the seven MCP servers and two plugins from the current
production ConfigMap render byte-identically to what is deployed today, modulo
the `$${...}` Flux escaping. That is the migration proof.

### Phase 3 — Operator

Controllers for all six kinds, digest resolution, the rendered ConfigMap, and
the Deployment with the drain policy.

*Accepts when:* the rollout table in section 6 is enforced by a test that
mutates each kind and asserts whether the pod template hash moved.

### Phase 4 — `t3-coded`

The sidecar: watch, pull, materialise atomically, reload, report status.

*Accepts when:* editing a `Skill` causes the skill to appear in a **new**
session in all three harnesses, with the pod's start time unchanged.

### Phase 5 — Image

Both tracks, the `install.cjs` assertion, Renovate wiring, and cron for nightly.

*Accepts when:* a nightly and a stable image both boot, all three harnesses
report healthy, and a deliberately bad nightly digest can be reverted by editing
one field.

### Phase 6 — Chart and migration proof

*Accepts when:* the chart stands up the whole stack on a kind cluster from
nothing, and a documented migration path exists from the current HelmRelease
without a window where agent sessions are unavailable.

## 11. Risks

1. **t3-code's own config sync fights the sidecar.** Section 3.6. Highest risk;
   Phase 0 blocker.
2. **`t3` is 0.0.x and nightly moves several times a day.** Expect breakage on
   the nightly track. Digest pinning plus last-known-good in status is the
   mitigation, and it must exist before anyone runs nightly in anger.
3. **Reload is not universally hot.** Section 3.4. Under-promise in the API:
   `reloadStrategy` and honest status conditions.
4. **The operator becomes critical-path for all agent work.** Hence fail open;
   test it by killing the operator with a session running.
5. **Harness auth state lives beside config.** Allow-list writes only.
6. **CRD churn.** `v1alpha1` means it. Do not promise stability until the
   renderer has survived a real upstream harness upgrade.
7. **Single-writer filesystem today.** Materialise to emptyDir so this is not
   baked in permanently.

## 12. Open questions for Phase 0

1. **Does t3 overwrite what the sidecar writes?** Set `config_dir_source`,
   start t3, write a config file directly into the provider home, restart t3,
   and observe. Then determine whether writing to the *source* directory and
   letting t3 sync is viable, and whether that sync can be triggered without a
   process restart. **This determines the delivery mechanism.**
2. Does Codex's `skills_watcher` genuinely pick up a new skill in a running
   app-server, or only at session start?
3. Does OpenCode's managed server need a bounce to see a new skill, or does a
   new session suffice?
4. Does Claude's `/reload-plugins` cover skills, or only plugins? Is there a
   non-interactive equivalent the sidecar can trigger?
5. Can `codex plugin marketplace add` take a **git** source directly, avoiding
   the release-archive round trip? The config schema shows `source_type = "git"`.
6. What is the actual latency and failure behaviour of a projected ConfigMap
   update in this cluster? It sets the floor for reload latency.
7. Does t3 expose a session-activity signal the drain policy can read? If not,
   `WaitForIdle` needs a different source of truth and that changes the
   `Runtime` API.

## 13. Non-goals

- **No `home-ops` changes from this repository.** Migration is documented, not
  performed here.
- **No changes to upstream `t3-code` or `traktuner/docker-t3-code`.** This
  replaces the latter rather than patching it.
- **Not a fork of t3-code.** Where upstream can do something, call it.
- **No agent scheduling, queueing or session orchestration.** This project
  configures harnesses and the workload they run in. What wakes an agent and
  what it works on is a different problem and deliberately out of scope.
- **No MCP server implementations.** MCP servers are referenced, never hosted.
