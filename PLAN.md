# t3-code-operator — implementation plan

Status: scaffold. No Go code exists yet. This document is the brief for the next
agent session.

API group: `t3code.janpuc.com`, version `v1alpha1`. Every kind is namespaced.

## 1. Why this exists

t3-code runs several agent harnesses in one pod. Configuring them today means a
hand-maintained ConfigMap plus a shell init container. That arrangement has four
problems which are really one problem.

| Today | Consequence |
|---|---|
| The same seven MCP servers are written three times, in three syntaxes | Every change is a three-place edit and drift is invisible until a harness misbehaves |
| ~180 lines of shell install plugins and skills | Imperative, untestable, and every failure branch ends in `\|\| echo "... unavailable"`, so a broken install looks like a successful boot |
| `reloader.stakater.com/auto: "true"` | Any ConfigMap edit restarts the pod, by design |
| `strategy: Recreate` with `replicas: 1` | **Every configuration change destroys in-flight agent sessions** |

The last two rows compound: the current setup is *wired* to restart on config
change. Adding a skill should not kill work.

## 2. Requirements

1. Plug and play with **all** t3-supported harnesses, not a fixed three.
2. Scales by running **more Workstations**, not more replicas.
3. Controlled by GitOps: Flux applies CRs, Renovate bumps pins.
4. CRDs must be **very future proof** — schema change is costly, so the schema
   opens doors it does not yet walk through.
5. **Everything the current t3-code app does must work in the first release.**
   Section 10 is the checklist, and it is a release gate.

### The tension, resolved

"Very future proof" and "no speculative flexibility" only conflict if you
confuse two things:

- **Schema** is expensive to change later and cheap to open now. Open it.
- **Features** are cheap to add later and expensive to build now. Don't build
  them.

So the API admits drivers we have not implemented, and we implement none of
them until a Workstation actually needs one.

## 3. Verified ground truth

Verified 2026-08-26 against upstream source and the published `t3@0.0.34`
tarball. Do not re-derive; do challenge if an upstream release lands.

### 3.1 t3 models provider *instances*, not provider types

This is the most important fact in this document.

```js
Object.entries(settings.providerInstances).map(([instanceId, instance]) =>
  [instanceId, instance.enabled === void 0 && (instance.driver === "cursor" || ...
```

`providerInstances` is an **open map keyed by an arbitrary `instanceId`**, and
each instance carries a **`driver`**. Sessions bind to a `providerInstanceId`,
so t3 tracks work per instance.

Consequence: two instances may share a driver. Running `claude` against
Anthropic and `claudel` against LiteLLM in the same pod is the native model, not
a workaround. Each instance has its own home path, so their config directories
do not collide.

`Harness` therefore maps one-to-one onto a t3 provider instance. We are
modelling t3's own model rather than inventing one over the top of it.

**Not yet proven:** that the server-side `t3code.toml` `[providers.*]` table
accepts arbitrary keys with a `driver` field. The deployed config uses fixed
names. This is Phase 0 question 1 and the `Harness` shape depends on it.

### 3.2 What t3 is, and how it is built

t3-code is the npm package **`t3`**. dist-tags: `latest` (0.0.34), `nightly`
(published several times a day), `alpha`. The harness CLIs are npm too:
`@openai/codex`, `@anthropic-ai/claude-code`, `opencode-ai`.

**Gotcha that silently produces a broken image:** `@anthropic-ai/claude-code`
ships a stub. `node install.cjs` must run inside the package directory
afterwards to materialise `bin/claude.exe` (~342 MB). Upstream asserts the
result exceeds 4096 bytes and fails the build otherwise. Reproduce that
assertion.

### 3.3 Filesystem contract

| Variable | Value | Owner |
|---|---|---|
| `T3CODE_HOME` | `/data/t3` | t3 |
| `T3CODE_CONFIG_PATH` | `/config/t3code.toml` | t3 |
| `HOME` | `/data/home` | shared |
| `CODEX_HOME` | `/data/codex` | Codex instance |
| `CLAUDE_CONFIG_DIR` | `/data/claude-home` | Claude instance |
| OpenCode config | `$HOME/.config/opencode/opencode.jsonc` | OpenCode instance |

Per-instance home paths are what make two claude instances possible.

### 3.4 Skill discovery differs per driver

| Path | claude | codex | opencode |
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

No single root serves all drivers. Write to both; OpenCode deduplicates by
skill name so the overlap is free.

`$CLAUDE_CONFIG_DIR` **is** the config root. Appending `.claude` to it yields a
path Claude does not read — a live bug in `traktuner/docker-t3-code`. The
current home-ops init container gets this right; do not regress it.

### 3.5 Reload capability differs per driver

| Driver | Mechanism | Realistic default |
|---|---|---|
| codex | `skills_watcher.rs` in the app-server | watch |
| claude | `/reload-plugins`; skills read per session | next session |
| opencode | managed server, discovery per session | next session or signal |

The guarantee this project makes is therefore narrow and honest: **new sessions
see new state immediately, and the pod never restarts for a content change.**

### 3.6 The three MCP dialects

| | claude (`.mcp.json`) | codex (`config.toml`) | opencode (`opencode.jsonc`) |
|---|---|---|---|
| Kind | `"type": "http"` | `[mcp_servers.x]` | `"type": "remote"` |
| Secret | `"Bearer ${VAR}"` | `bearer_token_env_var = "VAR"` | `"Bearer {env:VAR}"` |
| Extra headers | `headers` map | `[mcp_servers.x.env_http_headers]` | `headers` map |

All three take an environment **reference**, never a literal. That is what lets
rendered output stay non-sensitive.

Bonus the operator removes: the current ConfigMap must write
`$${LITELLM_API_KEY}` because Flux substitutes `${...}`. CRs carry no such
hazard.

### 3.7 t3's HTTP surface

Routes that matter to us: `/api/orchestration/snapshot`,
`/api/orchestration/threads/:threadId`, `/api/session`,
`/api/session/{sessionID}/wait`, `/api/observability/v1/traces`, and a full auth
surface (pairing links, pairing tokens, browser sessions, websocket tickets).

`/api/session/{sessionID}/wait` and `/api/orchestration/snapshot` are the
candidate drain signal. Their exact semantics need a live probe.

### 3.8 Telemetry: t3 publishes none

t3 imports `@opentelemetry/api` **and only that** — no SDK, no exporter, and it
does not read `OTEL_EXPORTER_OTLP_*`. It reads `OTEL_SERVICE_NAME`,
`OTEL_SERVICE_VERSION`, `OTEL_RESOURCE_ATTRIBUTES`. Spans are emitted only if
something else registers a tracer provider.

There are **no Prometheus metrics**: no `/metrics`, no `prom-client`.

So observability is not captured, it is *built*, and it is deferred out of v1.
See section 16.

### 3.9 t3 syncs config itself

`t3code.toml` carries `config_dir_source`, `config_source`, `config_path` and
`config_sync_mode` per provider. t3 copies configuration from those sources into
provider homes. If the sidecar also writes there, the two contend and the loser
varies with timing. Phase 0 question 2.

### 3.10 What is and is not standardised

Researched 2026-08-26. This decides `Extension`'s source types, so it is fact,
not preference.

| Layer | Standard | Maturity |
|---|---|---|
| Skill **format** | `SKILL.md` + frontmatter, `agentskills/agentskills` | 24,726 stars, pushed 2026-08-09. ~56,800 `SKILL.md` directories across 1,133 repositories. Every major harness reads it. |
| **Distribution** | Agent Skills OCI Artifacts spec, `ThomasVitale/agents-skills-oci-artifacts-spec` | 14 stars, pushed 2026-04-02. Draft 0.1.0, proposed to the core spec, **not merged**. |
| **Registry** | `skills.sh`, `npx skills add <owner/repo>` | Real; 1.2M+ installs; spans Claude Code, Codex, Cursor, Copilot, Windsurf, Gemini, Cline. |
| **Plugins** | none | No cross-harness standard exists. |

Two conclusions follow, and they are the reason this project exists.

**The format is settled; adopt it and change nothing.** The canonical spec repo
contains no `distribution/`, `registry/` or `oci/` paths. It covers the format
and client implementation and stops.

**No harness natively consumes any distribution standard.** The OCI draft
defines media types (`application/vnd.agent-skills.skill.v1`, config
`application/vnd.agent-skills.skill.config.v1+json`, collections as an OCI Image
Index) and reference tooling (Arconia CLI, `skills-oci`, ORAS), but Claude Code,
Codex and OpenCode read none of it. Adopting it would not make this project more
standard; it would make it bespoke with a specification attached, and it would
bet on a draft that has not moved in over four months.

What every mechanism *does* consume is **a plain git repository containing
`skills/<name>/SKILL.md`**. That is what `skills.sh` installs from, what
`claude plugin marketplace add` takes, and what `codex plugin marketplace add`
takes. The de-facto distribution unit is a repository, not an artifact.

**Therefore the sidecar is the standardisation layer.** One declarative source
in, each harness's native location out. That is the gap in the ecosystem, and
filling it is what earns this operator its existence.

## 4. The four kinds

`Extension` · `MCPServer` · `Harness` · `Workstation`

Each survives the deletion test: remove it and complexity reappears across
callers rather than vanishing.

### 4.1 Workstation

The only kind that becomes a Pod. A machine with an identity, storage, network,
tools and several agent harnesses installed on it — which is why it is not
called `Runtime`. It is deliberately **not** called `Instance`: t3 owns that
word for `providerInstances`.

```yaml
apiVersion: t3code.janpuc.com/v1alpha1
kind: Workstation
metadata: {name: primary}
spec:
  image: ghcr.io/janpuc/t3-code@sha256:...
  git:
    userName: janpuc
    userEmail: janpuc@proton.me
    signingKeySecretRef: {name: t3-code-git-signing}
  tools:
    - {name: kubectl, version: "1.34.2"}
    - {name: talosctl, version: "1.11.3"}
  drain: {policy: WaitForIdle, timeout: 30m}
```

`image` is one string, not a struct. A digest is the truth; a `track` enum would
be a second source of it and an enum that grows.

### 4.2 Harness

One t3 provider instance. The object name is the `instanceId`.

```yaml
kind: Harness
metadata: {name: claudel}
spec:
  driver: claude               # string, never an enum
  homePath: /data/claudel-home
  model: ...
  workstationRefs: [{name: primary}]
  allowedExtensions: {}        # permission direction; defaults to same namespace
```

`driver` is a string so a new t3 driver never forces a schema change. We
implement adapters for the drivers actually in use and no others.

A custom endpoint — claude pointed at an Anthropic-compatible API — is **config,
not a new driver**. Same format, same skill roots, same reload. It is a second
`Harness` with the same `driver` and a different `homePath`.

### 4.3 Extension

Anything a harness loads: skills, plugins, or both. One kind rather than two,
because a single upstream artifact is routinely both at once — a repository that
carries `skills/` and a `.claude-plugin/` manifest is one thing, and two CRs
pointing at the same reference would be a smell.

`Git` is the primary source type, because section 3.10 shows a git repository is
what the ecosystem actually consumes:

```yaml
kind: Extension
metadata: {name: mattpocock}
spec:
  source:
    type: Git                  # discriminated union, never implicit oneOf
    git:
      repo: mattpocock/skills
      ref: 9f2c1ab...          # a SHA, not a tag: tags move, and an Extension
      path: skills             # is instructions for a credentialed agent
    include: [tdd, research]   # empty means everything found
  harnessRefs: [{name: claude}, {name: claudel}, {name: codex}]
```

Because the format is universal, this consumes **any** upstream directly with no
repackaging. That is the integration story: adopting a new skill set is one CR,
not a vendoring exercise.

Other source types, in order of expected use:

- `OCI` — better inside a cluster than git: digest-pinned, no GitHub API auth in
  the pod, cosign-verifiable, native to Flux and Renovate. Tag published
  artifacts with the draft spec's media types from 3.10. It costs nothing and
  buys compatibility if that draft ever lands, but do not depend on its tooling.
- `Marketplace` — delegate to the harness's own installer for things that are
  genuinely harness-specific, such as `koment@koment-dev` on Codex.
- `Inline` — a single skill authored in the CR, for one-offs.

The install *verb* differs per driver — write files, or delegate to
`claude plugin install` — but that is adapter knowledge and stays behind the
seam. The caller says what and where, never how.

Because Extensions attach to a Harness rather than a Workstation, `claude` and
`claudel` can carry different extension sets. That falls out of the model.

**Pin SHAs, not tags.** An Extension is instructions executed by an agent that
holds a GitHub token and a kubectl MCP connection. A moving tag on a third-party
repository is a supply-chain hole, and a SHA closes it as well as a checksum
would.

### 4.4 MCPServer

One endpoint, rendered into every dialect its selected harnesses speak. The
largest single de-duplication win in the migration.

```yaml
kind: MCPServer
metadata: {name: kubectl}
spec:
  transport: http
  url: http://litellm.ai.svc.cluster.local:4000/mcp/kubectl/mcp
  auth:
    bearerToken: {envVar: LITELLM_API_KEY, secretRef: {name: t3-code-config, key: LITELLM_API_KEY}}
  harnessRefs: [...]
```

## 5. Ownership and attachment

Everything points **upward** at its parent, the way Gateway API routes attach to
gateways:

```
Extension.spec.harnessRefs   ──▶  Harness
MCPServer.spec.harnessRefs   ──▶  Harness
Harness.spec.workstationRefs ──▶  Workstation   (plural: share one harness
Workstation                       = the Pod      config across primary + canary)
```

And the permission direction, mirroring `allowedRoutes`:

```
Harness.spec.allowedExtensions    # defaults to same-namespace
```

Children declare **intent**; parents declare **policy**. That second half is
also the security control: a Harness can refuse extensions it does not trust,
which matters because an Extension is instructions for an agent holding a
GitHub token and a kubectl MCP.

The payoff: **adding a skill means creating one file. Nothing gets edited.**
That is what makes it simultaneously plug-and-play and GitOps-clean.

Refs accept a name or a label selector. Never a hardcoded list of names only.

## 6. Write modes

The renderer needs three, and today's init container proves all three are
load-bearing. A renderer that only knows how to write files will destroy your
Claude authentication on first reconcile.

| Mode | Behaviour | Today's precedent |
|---|---|---|
| `Replace` | Overwrite unconditionally | `t3code.toml`, `codex/config.toml` |
| `SeedIfAbsent` | Write only if missing or unparseable | `/data/t3/userdata/settings.json` |
| `Merge` | Deep-merge into existing, preserving unknown keys | `.claude.json`, which holds OAuth state beside MCP config |

The mode is a property of the target file, chosen by the adapter, not by the
user. Users should never have to know that `.claude.json` is dangerous.

Writes are atomic: stage to a temporary path, then rename. A harness must never
observe a half-written file. The renderer touches files by explicit allow-list
and never deletes what it did not write.

## 7. Tools

`Workstation.spec.tools`, backed by mise.

**A field, not a kind.** Field-to-ref is additive; kind-to-deleted is breaking.
You run one Workstation today, so start narrow. Promote it to a kind only if
per-entry status and its own reconcile loop become necessary.

**Opinionated about the mechanism, unopinionated about the OS.** mise ships
glibc and musl builds across x86_64/arm64/armv7 and resolves ~20 backends
(aqua, ubi, github, npm, cargo, pipx, go…). It is already the platform
abstraction, so we do not build one. Swapping the base image costs nothing.

**Versions are pinned.** Fourteen of the sixteen tools in the current mise
config are `"latest"`, so two pod restarts can produce different `kubectl`. For
a GitOps system that is a correctness bug. Explicit versions are the default
path and Renovate bumps them in git.

**Baked and reconciled from one declaration.** The image pre-warms the declared
set at build time; the sidecar reconciles at runtime and finds a cache hit when
they match, installing only drift. Fresh Workstations start fast, changed lists
converge without a rollout, and `curl mise.run | sh` at every boot disappears.

**Repo-local toolchains are out of scope.** miroir, koment and agent-kit each
carry their own mise config and mise activates them on `cd`. The Workstation
list is only the global baseline. An operator that manages per-repo toolchains
is competing with mise and with the repositories themselves.

## 8. What may and may not cause a rollout

This table is the contract. Encode it as a test.

| Change | Rollout? |
|---|---|
| `Extension`, `MCPServer` content | **Never** |
| `Harness` config | **Never** |
| `Workstation.spec.tools` | **Never** — reconciled into the persistent cache |
| `Workstation.spec.image` | Yes, via drain policy |
| Pod shape: volumes, resources, env set | Yes, via drain policy |

If a controller bumps a pod template hash for anything in the first two rows,
the project has failed at its one job.

## 9. Removal and drain

Addition and removal are not symmetric. Adding is additive and safe. Removal
cannot be applied retroactively: a session that already loaded an extension has
it in context, and draining does not clean that session, it only ends it.

So drain-on-removal buys something **only for drivers that cache at the process
level** — the OpenCode managed server, the Codex app-server. For claude, where
skills are read per session, deleting the files is sufficient and instant.

Therefore: removal behaviour is **per-adapter with an optional override**, not a
global policy. Most removals are instant. Making every skill deletion wait on an
idle window it did not need would be a regression.

Two traps to design around:

- **Flux pruning versus finalizers.** Flux deletes the CR; the operator holds it
  with a finalizer until idle; the Kustomization reports stuck, possibly for
  hours if a session stays open. Bounded drain with force-after-timeout is
  mandatory, and the pending state must surface as a condition.
- **Never-idle Workstations.** If someone always has a session open, an
  unbounded drain never completes. The timeout is not optional.

## 10. v1 parity checklist

**This is a release gate, not a wish list.** Everything the current
`kubernetes/apps/ai/t3-code` app does must work before v1 ships. Each row names
where it lands.

### Covered by the four kinds

| Capability today | Lands as |
|---|---|
| 7 MCP servers × 3 dialects | `MCPServer` × 7 |
| memini + koment plugins, codex marketplace + claude marketplace | `Extension` × 2 |
| koment codex plugin: GH release, sha256 verify, local marketplace, `plugin add` | `Extension` source type `GitHubRelease` |
| agent-kit skill batch, version-pinned, checksummed, installed to both skill roots, previous batch removed first | `Extension` with a `Git` source; the uninstall path is preserved, the bespoke tarball format is not |
| claude / codex / opencode provider config | `Harness` × 3 (plus `claudel` = 4) |

### Covered by Workstation

| Capability today | Lands as |
|---|---|
| image pinned by digest | `spec.image` |
| resources 500m/2Gi request, 8Gi limit | `spec.resources` |
| securityContext: uid/gid 1000, non-root, seccomp RuntimeDefault, drop ALL | chart defaults, overridable |
| `terminationGracePeriodSeconds: 120` | `spec.drain` informs it |
| PVC `t3-code` at `/data` | `spec.storage.data` |
| NFS `bigmouth.internal:/mnt/Vault/Workspace` at `/workspace` | `spec.storage.workspace` |
| emptyDir `/config`, emptyDir `/tmp` | operator-managed |
| `machine-info` at `/etc/machine-info` | `spec.machineInfo` |
| ~20 env vars on the app container | `spec.env` |
| `envFrom` secret `t3-code-config` | `spec.envFrom` |
| gitconfig: user, `gpg.format=ssh`, commit/tag signing, gh credential helper, `init.defaultBranch`, `push.autoSetupRemote`, `pull.rebase` | `spec.git` |
| SSH signing key normalisation (OpenSSH rewrap via Python) | `spec.git.signingKeySecretRef`, operator does the rewrap |
| `allowed_signers` derived from gitconfig email + pubkey | derived by the operator |
| mise self-install + 16 tools + reshim + koment symlink | `spec.tools`, per section 7 |
| Service on 3773 | operator-managed |
| HTTPRoute `t3.janpuc.com` via `envoy-internal`, `timeouts: 0s` | `spec.route`, optional |
| liveness/readiness/startup on `/`, startup `failureThreshold: 60` | chart defaults |

The `timeouts: "0s"` is not incidental — agent streams are long-lived and a
default timeout severs them. Carry it.

### Deliberately staying outside the operator

| Capability | Why it stays in Flux |
|---|---|
| Two `ExternalSecret`s from 1Password Connect | Secret plumbing is external-secrets' job; CRs reference secrets by name |
| ServiceAccount + read-all ClusterRole + pod-delete role | Standard RBAC objects; re-inventing them in a CRD adds nothing |
| kopiur backup component, `KOPIUR_CAPACITY: 30Gi` | Backup is kopiur's domain |
| `dependsOn: litellm, memini` | Flux dependency ordering |

The operator creates the Deployment and Service, optionally the Route. RBAC,
secrets and backup remain Flux-managed. That seam keeps the operator out of
business it has no advantage in.

### Explicitly dropped

| Dropped | Because |
|---|---|
| `reloader.stakater.com/auto: "true"` | Restart-on-config-change is the behaviour being replaced |
| Every `\|\| echo "... unavailable"` soft failure | Failures become status conditions |
| `provision-*.py`, `configure-*-mcp.py`, `provision-harness-mcp.sh` from the base image | The operator owns this now, in Go, with tests |

## 11. The image

`images/runtime/Dockerfile`, released from this repository so the sidecar and
the image share one release train.

Base `node:26-bookworm-slim`. `npm i -g t3@${T3_VERSION}` plus the harness CLIs,
each pinned by its own build argument so Renovate moves them independently. Run
`node install.cjs` for claude-code and assert the binary size, per 3.2. Bake
`t3-coded`, plus the Workstation's declared tool set.

Two tracks: `stable` from a pinned release, `nightly` from the `nightly`
dist-tag on a cron. `Workstation.spec.image` pins a **digest**, never a floating
tag, so a bad nightly is a one-field revert. Keep last-known-good in status so
the revert does not require going and looking it up.

## 12. The chart

`charts/t3-code-operator` installs CRDs, the operator and its RBAC. A
`Workstation` may be templated from values so a fresh install is plug and play,
but operator and workload stay separable — running the operator and managing
`Workstation` objects from Flux must work.

CRDs ship in `charts/t3-code-operator/crds/`, matching miroir's layout.

## 13. Future-proofing contract

Schema change is costly, so:

1. **No enums for open sets.** `driver`, transport and reload mode are strings
   with documented values.
2. **No bare booleans** that could become tri-state. Optional pointers or string
   modes.
3. **Discriminated unions** — `source.type` plus a per-type struct — never
   implicit `oneOf`.
4. **Selection by ref or label selector**, never a name list only.
5. **Never put derived data in `spec`.** Resolved digests live in `status`.
6. **Everything namespaced.** This is the one irreversible decision:
   namespaced-to-cluster-scoped is impossible later, while namespaced plus an
   opt-in cross-namespace field is additive. It also bounds the blast radius of
   an Extension being agent instructions.
7. **Design as if it were v1.** `v1alpha1` is not a licence to churn.

## 14. GitOps rules

1. **The operator never mutates `spec`.** Resolution goes to `status`. Otherwise
   it fights Flux forever.
2. **Standard `Ready` conditions on every kind.** Flux health checks read them,
   so this is required by the GitOps goal, not a nicety.
3. **Bounded finalizers.** Flux prunes on file deletion; removal must clean up
   without hanging the Kustomization. See section 9.
4. **Every pin is Renovate-parseable** — image digests, extension versions, tool
   versions.

## 15. Phases

Each phase states what it accepts on. A phase is done when its criterion is
demonstrated, not argued.

### Phase 0 — Settle the unknowns

Answer every question in section 17 empirically, in a live container, before any
controller is written. Record each answer with `koment add`.

Question 1 determines the `Harness` shape and question 2 determines the delivery
mechanism. Do not build on an assumption about either.

*Accepts when:* every open question has an observed command output attached.

### Phase 1 — API types

`api/v1alpha1` for all four kinds, with kubebuilder validation markers carrying
as much schema as CRD validation can express. No controllers.

*Accepts when:* `make manifests generate` is clean, CRDs install into a kind
cluster, and an intentionally invalid `MCPServer` is rejected by the API server
rather than by a controller.

### Phase 2 — The renderer

`internal/render`: pure functions from a resolved model to each dialect. No
Kubernetes client, no I/O, table-driven golden tests.

Start here rather than with controllers. It is the highest-value, most testable
module, and it is where the duplication actually dies.

*Accepts when:* the seven MCP servers, two plugins and three provider configs
from the current production ConfigMap render byte-identically to what is
deployed today, modulo the `$${...}` Flux escaping. That is the migration proof,
obtained before committing to controllers.

### Phase 3 — Operator

Controllers for all four kinds, ref and selector resolution, digest resolution,
the rendered ConfigMap, the Deployment, and the drain policy.

*Accepts when:* the rollout table in section 8 is enforced by a test that
mutates each kind and asserts whether the pod template hash moved.

### Phase 4 — `t3-coded`

Watch, pull, materialise atomically with the three write modes, reload, report.

*Accepts when:* editing an `Extension` makes the skill appear in a **new**
session on every attached Harness, with the pod's start time unchanged. The
three probes are `claude plugin details`, `codex debug prompt-input` and
`opencode debug skill` — the same commands agent-kit used to prove its own
loading.

### Phase 5 — Image

Both tracks, the `install.cjs` assertion, tool pre-warm, Renovate wiring, cron
for nightly.

*Accepts when:* a nightly and a stable image both boot with all harnesses
healthy, and a deliberately bad nightly digest reverts by editing one field.

### Phase 6 — Parity and migration

*Accepts when:* every row of section 10 is demonstrated on a real Workstation,
and a documented migration path exists from the current HelmRelease.

**Note on the migration criterion.** An earlier draft of this plan demanded
migration "without a window where agent sessions are unavailable." That is
probably impossible: the current workload is `replicas: 1`, `strategy:
Recreate`, on an **RWO** PVC, so old and new pods cannot run against it
simultaneously. Either the storage model changes or the criterion becomes a
short, announced window. Decide deliberately; do not discover it during cutover.

## 16. Deferred

Not in v1, recorded so the design does not preclude them.

- **Observability.** t3 publishes no metrics and its OTel instrumentation is
  inert (3.8), so this is a build, not a capture: `t3-coded` polls
  `/api/orchestration/snapshot` and exposes Prometheus metrics, and the chart
  ships a ServiceMonitor and a Grafana dashboard matching miroir's convention.
  The useful part: **the drain signal and the metrics come from the same API
  client**, so this is one piece of work with two consumers, not two.
- **OIDC.** t3 has its own auth model (pairing links, tokens, websocket
  tickets), and the pod is already fronted by a Gateway API route, so auth
  belongs at that layer — oauth2-proxy or ext_authz — as an optional
  `Workstation.spec.auth` later. Purely additive, nothing to reserve. One
  constraint today: do not bake a no-auth assumption into Service and Route
  templating.
- **A `ToolSet` kind.** Section 7. Promote from a field only if per-entry status
  and reuse across Workstations actually hurt.
- **A `Workspace` kind.** Per-project scoping may already be solved by in-repo
  `AGENTS.md` and `.claude/skills`. A kind you do not need is worse than one you
  are missing.

## 17. Phase 0 open questions

1. **Does `t3code.toml` accept arbitrary provider-instance keys with a
   `driver` field?** `settings.providerInstances` is an open map keyed by
   `instanceId` (3.1), but the deployed server config uses fixed names. Write
   `[providers.claudel] driver = "claude"` with its own `home_path` and see
   whether t3 starts it. **The `Harness` shape depends on this.**
2. **Does t3 overwrite what the sidecar writes?** Set `config_dir_source`, start
   t3, write directly into a provider home, restart, observe. Then determine
   whether writing to the *source* and letting t3 sync is viable, and whether
   that sync can be triggered without a process restart. **This determines the
   delivery mechanism.**
3. What does `/api/orchestration/snapshot` actually report, and does it
   distinguish active work from idle? Does `/api/session/{id}/wait` block until
   completion?
4. Does Codex's `skills_watcher` pick up a new skill in a running app-server, or
   only at session start? Same question for OpenCode's managed server.
5. Is there a non-interactive equivalent of Claude's `/reload-plugins` the
   sidecar can trigger?
6. Can `codex plugin marketplace add` take a git source directly, avoiding the
   release-archive round trip? The config schema shows `source_type = "git"`.
7. Does `mise` handle the declared tool set without network when the persistent
   cache is already warm? This decides whether a fresh Workstation is slow only
   once or every time.

## 18. Risks

1. **Question 1 is wrong.** If arbitrary instance keys do not work server-side,
   `claudel` needs another mechanism and the `Harness` model changes shape.
2. **t3 fights the sidecar over config.** Section 3.9.
3. **`t3` is 0.0.x with a nightly that moves several times a day.** Digest
   pinning plus last-known-good in status must exist before anyone runs nightly
   in anger.
4. **Reload is not universally hot.** Under-promise in the API and report
   live-versus-pending honestly.
5. **The operator becomes critical-path for all agent work.** Hence fail open:
   test it by killing the operator with a session running.
6. **Harness auth state lives beside config.** The three write modes in section
   6 are the mitigation, and `Merge` on `.claude.json` is the specific one.
7. **Extensions are agent instructions.** Whoever can create one in a namespace
   can steer an agent holding a GitHub token and a kubectl MCP. Namespacing plus
   `allowedExtensions` bounds it; it does not eliminate it.

## 19. Non-goals

- **No `home-ops` changes from this repository.** Migration is documented here,
  performed there.
- **No changes to upstream `t3-code` or `traktuner/docker-t3-code`.** This
  replaces the latter rather than patching it.
- **Not a fork of t3-code.** Where upstream can do something, call it.
- **No agent scheduling, queueing or session orchestration.** This configures
  harnesses and the machine they run on. What wakes an agent is a different
  problem.
- **No MCP server implementations.** Referenced, never hosted.
- **No per-repository toolchain management.** Section 7.
