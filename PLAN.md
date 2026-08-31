# t3-code-operator — implementation plan

Status: Phases 1 through 6 have static implementations and local contract
coverage. Phase 1 passes against a Kubernetes 1.36 API server. The renderer,
sidecar, controller, chart, and image contracts pass locally. Live image,
drain, Extension reload, and NAS acceptance remain pending because this host
has no container runtime. Cursor and Grok remain alpha.

This document is the design and acceptance contract for implementation.

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

1. Support all five built-in t3 drivers: `codex`, `claudeAgent`, `cursor`,
   `grok`, and `opencode`.
2. Scales by running **more Workstations**, not more replicas.
3. Controlled by GitOps: Flux applies CRs, Renovate bumps pins.
4. CRDs must be **very future proof** — schema change is costly, so the schema
   opens doors it does not yet walk through.
5. **Everything the current t3-code app does must work in the first release.**
   Section 10 is the checklist, and it is a release gate.
6. Treat upstream t3 as the authority for settings, sessions, and provider
   instances. The existing container wrapper is implementation evidence only.
7. Preserve an active turn through configuration changes and planned pod
   rollouts. A session waiting for its next human message does not block a
   rollout.
8. Make claim-backed, NFS, and NAS-backed `/workspace` storage first-class.
9. Let a Workstation share its claim-backed workspace through optional SMB.
10. Let the chart install arbitrary additional Kubernetes objects, including
   HTTPRoutes and RBAC.

`cursor` and `grok` are alpha in the first release. Their schemas and adapters
ship with the other three drivers. Their release gate is a repeatable smoke
test because no authenticated end-to-end environment is available today.

### The tension, resolved

"Very future proof" and "no speculative flexibility" only conflict if you
confuse two things:

- **Schema** is expensive to change later and cheap to open now. Open it.
- **Features** are cheap to add later and expensive to build now. Don't build
  them.

The API admits future drivers without a CRD change. The operator implements the
five drivers that upstream t3 currently ships. An unknown driver remains valid
API input, but its status reports `UnsupportedDriver` until an adapter exists.

## 3. Verified ground truth

Verified 2026-08-27 against upstream source and the published `t3@0.0.34`
tarball. Do not re-derive; do challenge if an upstream release lands.

### 3.1 t3 models provider *instances*, not provider types

This is the most important fact in this document.

`providerInstances` is an **open map keyed by a user-defined `instanceId`**,
and each instance carries a **`driver`**. Both use upstream's open slug: 1–64
characters, starting with a letter, followed by letters, digits, `_`, or `-`.
Sessions bind to a `providerInstanceId`, so t3 tracks work per instance.

Consequence: two instances may share a driver. Running `claude` against
Anthropic and `claudel` against LiteLLM in the same pod is the native model, not
a workaround. Codex and Claude have native per-instance `homePath` settings.
Other drivers need a verified config or environment path before two instances
can share one Workstation.

`Harness` therefore maps one-to-one onto a t3 provider instance. We are
modelling t3's own model rather than inventing one over the top of it.

The authenticated `server.updateSettings` command accepts a settings patch.
The isolated settings probe verified upstream-slug instance IDs, opaque config,
whole-map replacement, unrelated-setting preservation, and sensitive-value
rotation. A second probe materialised one enabled instance for each built-in
driver and observed all five in the live registry.

The sensitive-environment probe then materialised two Codex instances and two
Claude instances at the same time. It verified distinct `CODEX_HOME` and
`CLAUDE_CONFIG_DIR` values, isolated process environments, response redaction,
and rotated-value injection into rebuilt provider processes. Restart
persistence then preserved both drivers, their opaque config, their sensitive
environments, and their provider processes. The authenticated-turn probe then
recovered Codex and Claude conversation history across an idle restart. The
lifetime of an already-running process remains unverified.

### 3.2 What t3 is, and how it is built

t3-code is the npm package **`t3`**. The fixed baseline is `t3@0.0.34` until a
Renovate change passes the upstream contract probes. Its built-in driver kinds
are `codex`, `claudeAgent`, `cursor`, `grok`, and `opencode`.

Codex, Claude Code, and OpenCode have independently installable CLIs. Cursor
and Grok have different upstream launch contracts. The runtime image must use
the executable and arguments expected by the pinned t3 release. It must not
invent a common wrapper command.

The live registration probe exercised all five executables and upstream
registry entries. Cursor and Grok returned degraded snapshots without
authenticated environments, as their alpha status permits. The local OpenCode
inventory path remains a blocker because `opencode agent list` did not stop
within the probe deadline or after `SIGTERM` in a clean home.

**Gotcha that silently produces a broken image:** `@anthropic-ai/claude-code`
ships a stub. `node install.cjs` must run inside the package directory
afterwards to materialise `bin/claude.exe` (~342 MB). Upstream asserts the
result exceeds 4096 bytes and fails the build otherwise. Reproduce that
assertion.

Npm 12 also blocks dependency install scripts unless they are allowlisted. An
isolated t3 start failed after migrations because `node-pty` had no Linux native
build. The image must allowlist `node-pty`, `msgpackr-extract`, and Claude Code,
run their scripts, and verify the resulting native artifacts.

### 3.3 Filesystem contract

| Variable | Value | Owner |
|---|---|---|
| `T3CODE_HOME` | `/data/t3` | t3 |
| t3 settings | `/data/t3/userdata/settings.json` | t3 |
| `HOME` | `/data/home` | shared |
| Codex home | `/data/harnesses/<instanceId>/codex` | Codex `config.homePath` |
| Claude home | `/data/harnesses/<instanceId>/claude` | Claude `config.homePath` |
| Other driver state | `/data/harnesses/<instanceId>/...` | verified adapter config or environment |

The adapter derives managed paths from `instanceId`; user config cannot escape
`/data/harnesses`. A driver without a verified independent state path rejects a
second instance instead of silently sharing authentication or sessions.

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

No single root serves all drivers. The adapter writes each Extension to the
required roots for its selected Harness. OpenCode deduplicates overlapping
skills by name.

`$CLAUDE_CONFIG_DIR` **is** the config root. Appending `.claude` to it yields a
path Claude does not read — a live bug in `traktuner/docker-t3-code`. The
current home-ops init container gets this right; do not regress it.

### 3.5 Reload capability differs per driver

| Driver | Mechanism | Realistic default |
|---|---|---|
| codex | `skills_watcher.rs` in the app-server | watch |
| claudeAgent | `/reload-plugins`; skills read per session | next session |
| opencode | managed server, discovery per session | next session or signal |
| cursor | upstream provider adapter | next session; alpha until verified |
| grok | upstream provider adapter | next session; alpha until verified |

The guarantee is narrow: after `Programmed=True`, a new turn sees the reported
live revision. A content change never restarts the pod. An existing turn keeps
the revision with which it started.

### 3.6 The proven MCP dialects

| | claude (`.mcp.json`) | codex (`config.toml`) | opencode (`opencode.jsonc`) |
|---|---|---|---|
| Kind | `"type": "http"` | `[mcp_servers.x]` | `"type": "remote"` |
| Secret | `"Bearer ${VAR}"` | `bearer_token_env_var = "VAR"` | `"Bearer {env:VAR}"` |
| Extra headers | `headers` map | `[mcp_servers.x.env_http_headers]` | `headers` map |

All three take an environment **reference**, never a literal. That is what lets
rendered output stay non-sensitive.

Cursor and Grok support is alpha until their pinned adapters pass the same
reference, reload, and unknown-setting preservation probes. The renderer must
not silently claim parity for an unverified dialect.

Bonus the operator removes: the current ConfigMap must write
`$${LITELLM_API_KEY}` because Flux substitutes `${...}`. CRs carry no such
hazard.

### 3.7 t3's control surface

The upstream surface that matters is the authenticated orchestration snapshot
and the `server.updateSettings` command. Pairing links, pairing tokens, browser
sessions, and websocket tickets protect that surface. `t3-coded` must use the
same authenticated contract as an upstream client.

The settings read needs `orchestration:read`. The settings update needs
`orchestration:operate`. Those are the only persistent scopes that `t3-coded`
needs.

Upstream `t3 auth session issue` always grants administrative scopes and has no
scope flag in `t3@0.0.34`. `t3-coded` uses that command only to create a
two-minute bootstrap session against the shared t3 base directory. It then:

1. Calls upstream `/api/auth/pairing-token` with only the two orchestration
   scopes.
2. Exchanges that one-time credential through upstream `/oauth/token`.
3. Revokes the administrative session.
4. Keeps the narrow bearer credential only in process memory.
5. Renews before expiry and revokes the prior narrow session.

The client label contains the Workstation UID. On every start, `t3-coded`
revokes stale sessions with that label before it keeps the new narrow session.
It uses the returned expiry instead of assuming upstream's current 30-day
default.

No credential enters a ConfigMap, rendered manifest, Kubernetes Secret,
command argument, status, event, or log. `t3-coded` does not write a raw
credential to disk. Upstream remains the sole owner of its auth database.

The isolated Phase 0 auth probe completed this bootstrap and verified the
narrow session. It revoked the administrative session before it ran the
settings and provider probes. It then repeated the bootstrap, replaced the
narrow session, proved the prior bearer was invalid, and removed every probe
session. Cleanup after an ungraceful sidecar exit still needs a live proof.

A fresh t3 process can accept a TCP connection before its first HTTP response
completes. `t3-coded` must bound every readiness request. It must verify an
authenticated session and RPC connection before it applies settings. A TCP or
descriptor-only check is insufficient.

The bundled `/api/session/{id}/wait` code belongs to the OpenCode SDK. It is not
a universal t3 drain API. The authenticated Phase 0 probe observed an active
Codex tool call and the later idle session through the real t3 orchestration
snapshot. Pending approvals, user input, and background work still need live
coverage.

### 3.8 Telemetry is deferred

The pinned t3 package includes OTLP trace and metric export paths configured by
`T3CODE_OTLP_TRACES_URL` and `T3CODE_OTLP_METRICS_URL`. It does not expose a
Prometheus `/metrics` endpoint.

Observability is after MVP. The MVP does not add polling metrics, a
ServiceMonitor, dashboards, or a second telemetry implementation.

### 3.9 Upstream t3 versus the container wrapper

`t3code.toml`, `T3CODE_CONFIG_PATH`, `config_dir_source`, `config_source`,
`config_path`, and `config_sync_mode` are contracts from
`traktuner/docker-t3-code`. They are not the upstream t3 settings model.

The wrapper can inform image layout and CLI installation. It cannot define the
operator API. `t3-coded` updates upstream `providerInstances` through the
authenticated t3 control surface. It materialises only the driver files and
extension paths that upstream harnesses consume.

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
  serviceAccountName: t3-code
  securityContext:
    runAsUser: 1000
    runAsGroup: 1000
  storage:
    data:
      type: ExistingClaim
      existingClaim: {name: t3-code}
    workspace:
      type: ClaimTemplate
      claimTemplate:
        spec:
          accessModes: [ReadWriteOnce]
          storageClassName: fast-nvme
          resources:
            requests: {storage: 200Gi}
        retentionPolicy: Retain
  workspaceSharing:
    smb:
      username: t3
      shareName: workspace
      passwordSecretRef: {name: workspace-smb, key: password}
      service:
        type: LoadBalancer
        externalTrafficPolicy: Cluster
        loadBalancerSourceRanges: [192.0.2.0/24]
  git:
    userName: janpuc
    userEmail: janpuc@proton.me
    githubUser: janpuc
    credentialSecretRef: {name: t3-code-config, key: GH_TOKEN}
    signingKeySecretRef:
      name: t3-code-git-signing
      privateKeyKey: id_signing
      publicKeyKey: id_signing.pub
  tools:
    - {name: kubectl, backend: "aqua:kubernetes/kubectl", version: "1.34.2"}
    - {name: talosctl, backend: "aqua:siderolabs/talos", version: "1.11.3"}
  drain: {policy: WaitForIdle, timeout: 30m}
```

`image` is one string, not a struct. A digest is the truth; a `track` enum would
be a second source of it and an enum that grows.

`data` always mounts at `/data`. `workspace` always mounts at `/workspace`.
Each uses a discriminated union. `data` supports `ExistingClaim` and
`ClaimTemplate`; an explicit `EmptyDir` is allowed only for disposable tests.
Direct NFS is rejected for `data` because t3 keeps SQLite and auth state there.

`workspace` supports `ExistingClaim`, `ClaimTemplate`, `NFS`, and `EmptyDir`.
A NAS CSI driver uses an existing claim. Direct NFS uses `server`,
`exportPath`, and `readOnly` without a pre-created PersistentVolume. Kubernetes
does not expose mount options on a direct pod NFS volume. Use a claim when the
NAS needs custom mount options or credentials.

An `ExistingClaim` is never operator-owned. A `ClaimTemplate` defaults to
`Retain`. Its deterministic PVC has no owner reference. Its recorded identity
includes the Workstation name, UID, and volume. A recreated Workstation must
reference a retained PVC through `ExistingClaim`; it cannot adopt or delete the
previous UID's claim. `Delete` uses the Workstation finalizer to remove only
that exact PVC after drain. NFS identity often needs a supplemental group, so
the Workstation exposes that pod security field directly.

The ClaimTemplate storage request is a minimum. The operator can expand the
generated PVC, but it never shrinks it. Other template fields are creation-time
identity and reject incompatible changes.

When storage changes, the operator waits for the replacement Pod to become
available. It then removes a detached generated PVC only when that PVC records
`Delete`. A generated PVC reused through `ExistingClaim` becomes protected from
operator deletion.

`workspaceSharing.smb` exports a claim-backed workspace from the same Pod. It
does not replace the storage source. This design keeps agent I/O on the PVC and
uses SMB only for developer access. The operator creates a separate Service on
port 445. The chart can configure ClusterIP, NodePort, or LoadBalancer exposure.
The workspace claim must differ from the data claim, so SMB cannot expose
runtime authentication or session state.

The password comes from a projected Secret. `t3-smbd` updates Samba when the
projected value changes. Secret values never enter the rendered ConfigMap, Pod
arguments, or environment. The SMB container uses the pinned Workstation image.
It derives a stable Samba machine SID and NetBIOS name from the Workstation UID.
Samba runtime databases stay on an ephemeral volume instead of the workspace
PVC. Recreating the Workstation creates a new SMB server identity.

Samba needs root to create its password database and change to the runtime UID.
The optional container drops all capabilities except `SETUID` and `SETGID`. It
does not mount `/data`. A namespace that enforces Restricted Pod Security does
not admit this optional container.

### 4.2 Harness

One t3 provider instance. `spec.instanceId` is explicit because upstream permits
uppercase letters and underscores, while a Kubernetes object name does not.
The reconciler rejects duplicate instance IDs within one Workstation.

```yaml
kind: Harness
metadata: {name: claudel}
spec:
  instanceId: claudel
  driver: claudeAgent
  displayName: Claude via LiteLLM
  config:
    homePath: /data/harnesses/claudel/claude
    customModels: [claude-opus-4-1]
  environment:
    - name: ANTHROPIC_AUTH_TOKEN
      valueFrom:
        secretKeyRef: {name: t3-code-config, key: LITELLM_API_KEY}
  workstationRefs: [{name: primary}]
  attachmentPolicy:
    extensions: SameNamespace
    mcpServers: SameNamespace
```

`driver` is a string so a new t3 driver never forces a schema change. We
implement the five built-in drivers. Cursor and Grok start with alpha status.

`config` is an opaque driver-owned object. The renderer validates it with the
adapter for the pinned t3 version. The common envelope mirrors upstream:
`driver`, `displayName`, `accentColor`, `environment`, `enabled`, and `config`.
The CRD must not pull `homePath`, `model`, or endpoint fields into a false
cross-driver abstraction.

`config` contains non-sensitive values only. Each supported adapter rejects a
known secret field when it appears inline. Secret values use
`environment.valueFrom` so upstream t3 can store and redact them. A driver that
accepts a secret only inside opaque config remains unsupported until its
adapter has a safe reference mechanism.

A custom endpoint is **config, not a new driver**. It is a second `Harness`
with the same `driver` and different driver config. The API rejects home paths
outside the Workstation's managed roots and rejects path collisions between
attached Harnesses.

Attached Harnesses define the complete `providerInstances` map for a
Workstation. UI additions, removals, and edits to that map are unsupported and
revert to GitOps state with `DriftDetected`. Owning the full map avoids losing
an unowned UI instance during upstream's whole-map replacement. Every unrelated
server setting and every unknown field inside desired entries must survive.

The pinned t3 release cannot make only provider settings read-only. Its UI
becomes read-only when a session lacks `orchestration:operate`, but that same
scope starts agent turns. Removing it would disable the Workstation. If
upstream adds a dedicated lock or settings-write scope, use it. Do not add an
HTTP proxy only to hide the settings control in MVP.

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
    type: Git
    git:
      url: https://github.com/mattpocock/skills.git
      commit: 9f2c1ab...
      path: skills
      credentialSecretRef: {name: github-read-token, key: token}
    include: [tdd, research]
  harnessRefs: [{name: claude}, {name: claudel}, {name: codex}]
```

Because the format is universal, this consumes **any** upstream directly with no
repackaging. That is the integration story: adopting a new skill set is one CR,
not a vendoring exercise.

Other source types, in order of expected use:

- `OCI` — digest-pinned bundles pulled with registry credentials. Draft Agent
  Skills media types are accepted but not required.
- `Marketplace` — delegate to the harness's own installer for things that are
  genuinely harness-specific, such as `koment@koment-dev` on Codex.
- `GitHubRelease` — a release asset with a required SHA-256 checksum. This is
  needed to migrate the current koment installation without weakening it.

`Inline` is deferred. Large instruction payloads do not belong in the API or a
rendered ConfigMap.

The install *verb* differs per driver — write files, or delegate to
`claude plugin install` — but that is adapter knowledge and stays behind the
seam. The caller says what and where, never how.

Because Extensions attach to a Harness rather than a Workstation, `claude` and
`claudel` can carry different extension sets. That falls out of the model.

**Pin full commits, digests, and checksums.** A Git source rejects a tag or
abbreviated commit. MVP rejects submodules and Git LFS pointer files because a
superproject commit does not pin their content by itself. Extraction rejects
absolute paths, `..`, and symlinks that escape the source root. Two Extensions
cannot own the same destination path.

Every Marketplace source also carries an immutable repository commit or
artifact digest. Update checks stay disabled. If an upstream installer cannot
install an immutable local snapshot and roll it back, that Marketplace adapter
is unsupported. Fetch credentials use exact Secret refs and never enter a URL,
command argument, rendered object, or log.

### 4.4 MCPServer

One endpoint, rendered into every dialect its selected harnesses speak. The
largest single de-duplication win in the migration.

```yaml
kind: MCPServer
metadata: {name: kubectl}
spec:
  transport: http
  config:
    url: http://litellm.ai.svc.cluster.local:4000/mcp/kubectl/mcp
  headers:
    - name: Authorization
      prefix: "Bearer "
      valueFrom:
        secretKeyRef: {name: t3-code-config, key: LITELLM_API_KEY}
  harnessRefs: [...]
```

`transport` is an open string. `config` is a non-sensitive opaque object that a
supported transport adapter validates. MVP implements remote HTTP and local
stdio. Stdio config contains `command`, `args`, and an optional working
directory. Local process environment entries use the same value-or-Secret-ref
shape as Harness environment entries.

HTTP headers support plain values and Secret refs. Header names are normalized
case-insensitively, and duplicates are rejected. The renderer derives internal
environment names, so users cannot create cross-MCP collisions. It emits only
Secret references and environment names. `t3-coded` resolves values and sends
them to upstream as sensitive provider environment entries. Secret rotation
updates new provider processes without a pod rollout. Phase 0 must prove the
exact lifetime and all five dialects.

The Workstation Role grants `get` and name-scoped `watch` only for referenced
Secrets through `resourceNames`; it never grants Secret `list`. RBAC changes do
not roll the pod. Every process in one Workstation remains in the same trust
domain.

`t3-coded` supplies `fieldSelector=metadata.name=<exact-name>` on each Secret
watch because Kubernetes requires it with `resourceNames`. Permission changes
use two phases. The controller expands the Role before it publishes a manifest
that adds a Secret. It contracts the Role only after the sidecar reports a live
revision that no longer references the Secret.

## 5. Ownership and attachment

Everything points **upward** at its parent, the way Gateway API routes attach to
gateways:

```
Extension.spec.harnessRefs   ──▶  Harness
MCPServer.spec.harnessRefs   ──▶  Harness
Harness.spec.workstationRefs ──▶  Workstation (= the Pod)
```

Plural Workstation refs let primary and canary machines share one Harness
configuration. Runtime state stays inside each Workstation.

And the permission direction, mirroring `allowedRoutes`:

```
Harness.spec.attachmentPolicy.extensions
Harness.spec.attachmentPolicy.mcpServers
```

Children declare **intent**; parents declare **policy**. A Harness can refuse
extensions or MCP servers it does not trust. This matters because both can
steer an agent that holds strong credentials.

Each attachment policy defaults to `SameNamespace`; `None` denies all
attachments of that kind. An exact child reference is necessary but never
bypasses the parent policy.

The payoff: **adding a skill means creating one file. Nothing gets edited.**
That is what makes it simultaneously plug-and-play and GitOps-clean.

MVP refs contain an exact name and resolve only in the child's namespace. Label
selectors and cross-namespace refs are deferred. They can be added without
changing the name-ref shape.

A Workstation is the effective security boundary. Its harnesses share a pod,
UID, filesystem, network namespace, and provider environment. Separate tenants
need separate Workstations even if Harness attachment policy would allow the
same content.

Each child reports status for every attachment. One broken target does not hide
successful targets or make their live revision ambiguous.

## 6. Write modes

The renderer needs three modes. A renderer that only overwrites files will
destroy authentication and user settings on its first reconcile.

| Mode | Behaviour | Valid use |
|---|---|---|
| `Replace` | Replace one fully operator-owned file | Generated manifests and extension-owned destinations |
| `SeedIfAbsent` | Write only when the target does not exist | Optional defaults whose later ownership belongs to the harness |
| `Merge` | Apply owned fields and preserve every unowned field | Harness config beside authentication or user settings |

The mode is a property of the target file, chosen by the adapter, not by the
user. Users should never have to know that `.claude.json` is dangerous.

`SeedIfAbsent` never overwrites an unparseable file. `Merge` is a managed-fields
three-way merge across the current file, the last applied owned fields, and the
new owned fields. Desired values win for CR-owned fields. Unknown and
user-created fields survive. A parse failure keeps the last-known-good revision
live and reports a condition.

Each rendered ConfigMap contains one versioned, secret-free desired manifest.
It names all target paths, write modes, ownership paths, source digests, reload
actions, Secret references, and a deterministic desired revision. The
renderer never reads the filesystem and never resolves a Secret value.

`t3-coded` stages a complete revision, validates every output, and then commits
atomic file renames. If a later commit step fails, it restores the prior files.
It never reports a partial revision as live. It touches explicit allow-listed
paths and never deletes an unowned path.

Fetched Extension content lives in a content-addressed cache under
`/data/t3-coded`. Activation switches only adapter-owned paths after every
checksum and collision check passes. Marketplace installers must provide the
same rollback property; an imperative partial install is a failed adapter.

No primitive can atomically commit several files and an upstream RPC. Pinned t3
has no turn-start fence. `t3-coded` stages everything first and waits for stable
idle before a disruptive commit. Existing turns continue on their prior state.
A new turn can still race the short file-and-RPC commit window and observe mixed
state. MVP inherits and reports that upstream limitation. It does not claim
revision-level atomicity. A sidecar reverse proxy is forbidden because its
failure would put t3 behind a new critical path.

Upstream t3 settings are not a file target. The rendered manifest owns the
provider update-check policy and the complete `providerInstances` map.
`t3-coded` sends both fields through `server.updateSettings`. The upstream patch
preserves unrelated server settings. The sidecar then verifies the redacted
result. The isolated drift probe converged after ten competing UI writes while
it preserved unrelated settings and unknown desired config. Continuous UI
writes remain unsupported and can keep reporting drift.

## 7. Tools

`Workstation.spec.tools`, backed by mise.

**A field, not a kind.** Field-to-ref is additive; kind-to-deleted is breaking.
You run one Workstation today, so start narrow. Promote it to a kind only if
per-entry status and its own reconcile loop become necessary.

**Opinionated about the mechanism, unopinionated about the OS.** mise ships
glibc and musl builds across x86_64/arm64/armv7 and resolves ~20 backends
(aqua, ubi, github, npm, cargo, pipx, go…). It is already the platform
abstraction, so we do not build one. Swapping the base image costs nothing.

**Versions and sources are pinned.** Fourteen of the sixteen tools in the
current mise config are `"latest"`, so two pod restarts can produce different
`kubectl`. For a GitOps system that is a correctness bug. Each entry uses an
explicit mise backend and version. A resolved URL and checksum enter the
secret-free desired manifest before installation. Renovate updates the source
declaration in Git.

The MVP accepts runtime additions from mise's fully lockable `aqua`, `http`,
`github`, and `gitlab` backends. Other backends do not guarantee both an
artifact URL and checksum. They can enter the runtime API after mise provides
that guarantee. This restriction does not limit the fixed image baseline.

**Fixed image baseline plus runtime additions.** The image contains one fixed,
version-pinned tool baseline that every Workstation receives.
`Workstation.spec.tools` adds or replaces tools through mise at runtime. A CR
cannot change what was baked into its already pinned image.

The mise cache lives on retained `/data`. Removing a runtime tool removes its
active shim after the safe apply point but keeps downloaded cache content.
This makes rollback fast and avoids deleting user data. A failed install keeps
the prior active tool set and reports the failed tool separately.

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
| Harness, MCP, Extension, or Git Secret value/reference | **Never** |
| `Workstation.spec.tools` | **Never** — reconciled into the persistent cache |
| Git identity, signing key, or machine-info content | **Never** |
| SMB password Secret value | **Never** — projected and reloaded in place |
| `Workstation.spec.image` | Yes, via drain policy |
| Pod shape: storage, resources, security, service account, startup environment | Yes, via drain policy |
| SMB enablement, username, share, read-only mode, resources, or Secret reference | Yes, via drain policy |
| Service fields or chart `extraObjects` | No Workstation rollout |

If a controller bumps a pod template hash for anything in the first two rows,
the project has failed at its one job.

“Never rollout” does not mean “interrupt a provider process now.” The renderer
marks every action as additive or disruptive. `t3-coded` can apply an additive
file write during a turn. It defers a provider rebuild, process reload, Secret
environment change, or destructive removal until all turns for that provider
instance finish. The prior live revision remains usable and status reports
`ApplyDeferred`.

The test also proves continuity. It starts a long turn, applies each change,
and verifies that the same turn completes. A pod-hash assertion alone cannot
prove the invariant.

For a pod-shape rollout, `WaitForIdle` uses these terms:

- **Active:** t3 is starting or running a turn, a tool call, or tracked
  background work.
- **Idle:** all active work has stopped. A persisted session may remain open
  while it waits for the next human message.
- **Stable idle:** the idle predicate stays true for five continuous seconds.
  A failed or timed-out snapshot resets this window.
- **Quiesced:** the Workstation rejects new turn starts while existing work
  finishes. Existing streams and control traffic continue.

Each Workstation uses one replica and `Recreate`. The rollout sequence marks
the pod unready, waits for stable idle, persists the live state, and only then
changes the pod template. The replacement reuses retained `/data`, including
session state. The termination grace period starts after drain; it does not
substitute for the drain timeout.

Pinned t3 has no public quiesce or graceful-shutdown fence. An active-turn
probe confirmed that `SIGTERM` stops t3 promptly and recovers the interrupted
turn as `error`. MVP does not patch that upstream behavior. It never signals a
snapshot-visible active Workstation. A client can still dispatch in the race
between the last idle read and `SIGTERM`. MVP inherits and reports this upstream
race. A sidecar proxy is not an acceptable fallback because its failure would
stop an otherwise healthy t3 server.

A pending approval or provider user-input request belongs to an unfinished
turn, so it remains active. “Waiting for the next human message” starts only
after the turn completes and `activeTurnId` clears.

A PodDisruptionBudget with `minAvailable: 1` protects one-replica Workstations
from voluntary eviction. No operator can preserve a process through node loss,
OOM termination, or forced deletion. Retained state limits recovery loss, but
the continuity guarantee covers operator-controlled changes and rollouts.

## 9. Removal and drain

Addition and removal are not symmetric. Adding is additive. Removal cannot
change the context already loaded by a running turn.

So drain-on-removal buys something **only for drivers that cache at the process
level** — the OpenCode managed server, the Codex app-server. For claude, where
skills are read per session, deleting the files is sufficient and instant.

Therefore removal behavior is per adapter. A file-only removal can commit
without a pod rollout. A process reload waits until the affected provider has
no active work. Neither path terminates an active turn.

Two traps to design around:

- **Flux pruning versus finalizers.** Flux can delete a CR while work remains.
  The finalizer holds deletion until the Workstation is idle and reports the
  pending condition.
- **Never-idle Workstations.** An open session does not block, but continuous
  work can. `drain.timeout` reports `DrainTimedOut`. Its default action is
  `Block`, so active work survives. `Force` requires explicit configuration and
  records that continuity was waived.

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
| cursor and grok provider registration | `Harness` adapters, alpha until authenticated end-to-end tests exist |
| custom OpenCode LiteLLM models and small model | opaque OpenCode `Harness.spec.config` |
| CR-managed provider selection and update checks disabled | managed upstream t3 server settings |

### Covered by Workstation

| Capability today | Lands as |
|---|---|
| image pinned by digest | `spec.image` |
| resources 500m/2Gi request, 8Gi limit | `spec.resources` |
| securityContext: uid/gid 1000, non-root, seccomp RuntimeDefault, drop ALL | chart defaults, overridable |
| `terminationGracePeriodSeconds: 120` | pod shutdown setting, applied only after drain |
| PVC `t3-code` at `/data` | `spec.storage.data` |
| NFS `bigmouth.internal:/mnt/Vault/Workspace` at `/workspace` | `spec.storage.workspace` |
| fast PVC workspace mounted on a developer machine | `spec.storage.workspace` plus `spec.workspaceSharing.smb` |
| emptyDir `/config`, emptyDir `/tmp` | operator-managed |
| `machine-info` at `/etc/machine-info` | `spec.machineInfo` |
| non-sensitive app startup environment | image defaults plus `spec.env`; changes drain, while runtime-owned path and GitHub variables are reserved |
| provider and MCP values from `t3-code-config` | exact Harness and MCP Secret refs; rotations do not roll |
| gitconfig: user, `gpg.format=ssh`, commit/tag signing, gh credential helper, `init.defaultBranch`, `push.autoSetupRemote`, `pull.rebase` | `spec.git` |
| GitHub CLI and Git credential from `t3-code-config` | `spec.git.githubUser` plus `credentialSecretRef`, written to private operator-owned files |
| SSH signing key normalisation (OpenSSH rewrap via Python) | `spec.git.signingKeySecretRef`, normalised by `t3-coded` |
| `allowed_signers` derived from gitconfig email + pubkey | derived by `t3-coded` without putting key content in the ConfigMap |
| fixed CLI baseline + current mise tools + koment symlink | image baseline and `spec.tools`, per section 7 |
| repository safe-directory scan | a bounded `t3-coded` reconciliation action |
| Service on 3773 | operator-managed |
| HTTPRoute `t3.janpuc.com` via `envoy-internal`, `timeouts: 0s` | chart `extraObjects` or Flux |
| liveness/readiness/startup on `/`, startup `failureThreshold: 60` | chart defaults |
| local koment and memini MCPs plus seven proxy MCPs | `MCPServer` objects with per-Harness attachment status |

The `timeouts: "0s"` is not incidental — agent streams are long-lived and a
default timeout severs them. Carry it.

### Deliberately staying outside the operator

| Capability | Why it stays in Flux |
|---|---|
| Two `ExternalSecret`s from 1Password Connect | Secret plumbing is external-secrets' job; CRs reference secrets by name |
| ServiceAccount + read-all ClusterRole + pod-delete role | Standard RBAC objects supplied through chart `extraObjects` or Flux |
| kopiur backup component, `KOPIUR_CAPACITY: 30Gi` | Backup is kopiur's domain |
| `dependsOn: litellm, memini` | Flux dependency ordering |

The operator creates the Deployment, Service, PodDisruptionBudget, exact-name
Secret Role, RoleBinding, and sidecar status ConfigMap. The chart can template
arbitrary extra objects, but the operator does not reconcile their domain
behavior. Secrets, Routes, user RBAC, and backup remain owned by their existing
controllers.

### Explicitly dropped

| Dropped | Because |
|---|---|
| `reloader.stakater.com/auto: "true"` | Restart-on-config-change is the behaviour being replaced |
| Every `\|\| echo "... unavailable"` soft failure | Failures become status conditions |
| `provision-*.py`, `configure-*-mcp.py`, `provision-harness-mcp.sh` from the base image | The operator owns this now, in Go, with tests |

## 11. The image

`images/runtime/Dockerfile`, released from this repository so the sidecar and
the image share one release train.

Use a digest-pinned Node base that satisfies the pinned t3 engine range. Install
`t3@${T3_VERSION}` and the exact runtime needed by all five built-in drivers.
Each independently distributed CLI has its own pin. Cursor must expose
`cursor-agent`; Grok must support `grok agent stdio`. Run `node install.cjs`
for Claude Code and assert the binary size, per section 3.2.

Bake `t3-coded` and the fixed tool baseline. Runtime `spec.tools` never changes
the image build. Lock package resolution and record the base digest, npm
integrities, CLI versions, and resulting image digest in build provenance.
Allowlist only required package install scripts. Fail the build unless t3
starts and every required native artifact loads. Also fail the build unless
`t3-smbd` starts Samba, opens its socket, and installs the expected local SID.

At startup, `t3-coded` reports its manifest protocol and exact t3 version. An
unknown protocol keeps the prior rendered revision live. A new image becomes
ready only after its sidecar and t3 contract checks pass; otherwise the
operator reports the prior image digest for rollback.

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

The chart exposes `extraObjects`, a templated list of arbitrary Kubernetes
objects. It supports HTTPRoutes, ServiceAccounts, Roles, RoleBindings, and
site-specific resources without adding them to the CRD. The chart can also
template any of the four project CRs. It never grants extra RBAC by default.

Helm owns the lifecycle of each `extraObjects` resource. The resource's native
controller owns its runtime behavior. The chart documents that Helm does not
upgrade CRDs from `crds/`; Flux must apply new CRDs before an operator upgrade.

## 13. Future-proofing contract

Schema change is costly, so:

1. **No enum for the open driver set.** `driver` uses upstream's slug rules.
   Closed sets can use enum validation when unknown values have no semantics.
2. **Use a mode when behavior has more than two states.** A real binary choice
   can remain a boolean.
3. **Discriminated unions** use `type` plus exactly one matching branch. CEL
   rejects zero branches, multiple branches, and a branch that conflicts with
   `type`.
4. **MVP refs use exact names.** A future selector or cross-namespace branch is
   additive and does not weaken today's authorization boundary.
5. **Never put derived data in `spec`.** Resolved digests live in `status`.
6. **Everything namespaced.** This is the one irreversible decision:
   namespaced-to-cluster-scoped is impossible later, while namespaced plus an
   opt-in cross-namespace field is additive. It also bounds the blast radius of
   an Extension being agent instructions.
7. **Version the renderer-to-sidecar protocol.** Unknown protocol versions fail
   closed while the last-known-good revision stays live.
8. **Preserve opaque adapter config.** Harness and MCP config use
   `x-kubernetes-preserve-unknown-fields`; tests prove nested keys round-trip.
9. **Bound collections and rendered size.** Every list has a documented
   maximum. An oversized desired manifest fails before the ConfigMap limit.
10. **Design as if it were v1.** `v1alpha1` is not a licence to churn.

## 14. GitOps rules

1. **The operator never mutates `spec`.** Resolution goes to `status`. Otherwise
   it fights Flux forever.
2. **Every status has `observedGeneration`.** `Ready=True` only describes the
   current generation.
3. **Desired and live revisions are distinct.** `Resolved=True` can coexist
   with `Programmed=False` while a sidecar applies or drains.
4. **One Workstation reconciler owns aggregate state.** It resolves attached
   children into one `ResolvedWorkstation`, renders one desired manifest, and
   owns workload resources. Child reconcilers validate immutable source
   identities and report attachment status; they do not patch the workload.
5. **The GitOps provider map is authoritative.** Attached Harnesses define the
   full map. The read-modify-write path preserves unrelated server settings.
6. **Finalizers protect active work.** Timeout action defaults to `Block`. See
   section 9.
7. **Every pin is Renovate-parseable** — image digests, extension versions, tool
   versions.

## 15. Phases

Each phase states what it accepts on. A phase is done when its criterion is
demonstrated, not argued.

### Phase 0 — Settle the unknowns

Pin and probe upstream t3 before writing a controller. Static package probes
establish the settings envelope, five built-in drivers, executable contracts,
control method names, Secret handling, and orchestration fields. Live probes
establish behavior that source inspection cannot prove.

Store repeatable commands under `hack/phase0/`. Store observed evidence under
`docs/research/`. A package update must rerun these probes before Renovate can
change the runtime pin.

*Accepts when:* every blocker in section 17 has repeatable output. A failed or
unavailable Cursor or Grok authenticated test keeps that adapter alpha; it does
not block the other three drivers.

### Phase 1 — API types

`api/v1alpha1` for all four kinds, with kubebuilder validation markers carrying
as much schema as CRD validation can express. No controllers.

*Accepts when:* `make manifests generate` is clean, CRDs install into a kind
cluster, and an intentionally invalid `MCPServer` is rejected by the API server
rather than by a controller. Unknown nested Harness and MCP config keys survive
an API read-modify-write cycle.

### Phase 2 — The renderer

`internal/render` accepts one `ResolvedWorkstation` and returns one versioned,
secret-free desired manifest. It has no Kubernetes client and no I/O. Driver
adapters own dialect validation, paths, managed fields, and reload actions.

Start here rather than with controllers. It is the highest-value, most testable
module, and it is where the duplication actually dies.

*Accepts when:* the current seven proxy MCP servers, local MCP servers, two
plugins, and four configured provider instances render semantically equivalent
state without Secret values. Golden tests cover all five built-in adapters.
Cursor and Grok golden tests carry an alpha expectation.

Implemented 2026-08-27. The parity test covers seven proxy MCP servers, two
local MCP servers, two plugin paths, Git skills, and four provider instances.
See `docs/research/phase2-renderer-protocol.md` for the protocol contract.

### Phase 3 — `t3-coded`

Implement the policy-free applier. It validates the manifest protocol, resolves
same-namespace Secrets, pulls pinned sources, applies full revisions, calls the
authenticated upstream t3 settings command, triggers adapter-selected reloads,
and reports desired and live revisions.

After t3 becomes ready, `t3-coded` bootstraps its narrow upstream session as
section 3.7 defines. An auth renewal failure blocks new applies and status
refreshes. It does not stop t3 or an active provider process. The last live
revision continues to work.

`t3-coded` writes a bounded, secret-free report to one status ConfigMap. Its
Role can patch only that named object. The controller copies the report into CR
status. The sidecar cannot patch CRs, and an operator outage does not stop the
last live revision. Kubernetes API loss leaves the last live revision running.
Each report carries its Pod revision. The controller rejects a report from a
previous Pod when it computes `Programmed` and `Ready`.

Keep the upstream control client behind a small interface. Test with the real
local adapter and an in-memory fake. The sidecar applies renderer decisions;
it does not select drivers, permissions, or rollout policy.

*Accepts when:* a failed revision leaves the prior revision live; a Secret
rotation reaches a newly started provider process; and adding an Extension
appears in a new turn without changing the pod start time. A disruptive
Harness or Secret update remains deferred while a long turn completes.
Managed-file tests cover malformed input, user edits, deletion, rollback, and
a crash between commits. Source tests cover all four source types, offline
cache, collision, uninstall, and Marketplace rollback. Fake-clock tests cover
credential renewal and stale-session cleanup. Mise tests prove that a failed
install preserves the prior active tool set.

The policy-free sidecar core was implemented on 2026-08-28. Local tests cover
transaction rollback, crash recovery, all four source types, native installer
rollback, Secret redaction, exact-name Kubernetes access, stable idle, auth
renewal, and locked mise activation. Real upstream t3 0.0.34 and mise probes
pass. The Kubernetes live checks in the acceptance paragraph remain pending.
See `docs/research/phase3-sidecar-contract.md`.

### Phase 4 — Operator

Implement all four controllers. The Workstation reconciler owns aggregate
resolution, the rendered ConfigMap, Deployment, Service, PodDisruptionBudget,
status ConfigMap, exact-name RBAC, and drain state. Other reconcilers validate
content identities and enqueue affected Workstations.

*Accepts when:* every row in section 8 has a pod-template test. Content changes
also pass a live test in which an already running turn completes on the same
pod. A pod-shape change starts only after the live snapshot becomes idle.

Implemented 2026-08-28. Four controllers, aggregate rendering, exact-name
sidecar RBAC, workload resources, content finalizers, and drain decisions pass
local tests. The live content and pod-shape acceptance tests remain pending
because this host has no container runtime.

### Phase 5 — Image and chart

Build both image tracks. Add the CLI assertions, fixed tool baseline, Renovate
wiring, CRDs, operator templates, optional project CR templates, and
`extraObjects`.

*Accepts when:* stable and nightly images boot with Codex, Claude, and OpenCode
healthy. Cursor and Grok pass installation and provider-registration smoke
tests with explicit alpha status. Sample chart renders include NFS and an NVMe
PVC shared through SMB. They also include HTTPRoute, ServiceAccount, Role, and
RoleBinding. Real storage tests write `/workspace`, perform a planned rollout,
and read the same data.

Static implementation completed 2026-08-28. Both image tracks have exact npm
locks, a fixed mise baseline, multi-architecture bake targets, SBOM and
provenance settings, and signed-image workflows. The chart contract passes for
NFS, PVC-with-SMB, HTTPRoute, RBAC, CRDs, and `extraObjects`. Live image, SMB,
and NAS acceptance remain pending because this host has no container runtime.
The image build now includes a native Samba startup, socket, and SID smoke test.

### Phase 6 — Parity and migration

*Accepts when:* every row of section 10 is demonstrated on a real Workstation,
and a documented migration path exists from the current HelmRelease.

The current workload has one replica, `Recreate`, and an RWO data claim. The
migration therefore has an idle cutover window. It waits for active work to
finish, rolls when sessions wait for their next human message, and resumes from
retained `/data`. It does not promise zero connection interruption.

Static parity implementation completed 2026-08-28. It covers GitHub CLI
credentials, SSH signing, machine information, bounded repository discovery,
sidecar readiness, secure pod defaults, and transactional Workstation files.
The migration procedure is in `docs/migration-from-helmrelease.md`. Real
Workstation acceptance remains pending because this host has no container
runtime.

## 16. Deferred

Not in v1, recorded so the design does not preclude them.

- **Observability.** Upstream t3 already supports OTLP endpoints. A later stage
  can expose those settings and decide whether Prometheus translation,
  ServiceMonitor, or dashboards add enough value. MVP adds none of them.
- **OIDC.** Gateway authentication through oauth2-proxy or ext_authz can
  supplement upstream t3 auth later. It cannot replace t3 pairing, sessions,
  or websocket tickets. The HTTPRoute remains a chart `extraObjects` resource.
- **A `ToolSet` kind.** Section 7. Promote from a field only if per-entry status
  and reuse across Workstations actually hurt.
- **A `Workspace` kind.** Per-project scoping may already be solved by in-repo
  `AGENTS.md` and `.claude/skills`. A kind you do not need is worse than one you
  are missing.
- **Inline Extensions.** Keep instruction payloads out of CRs and ConfigMaps
  until a real use case justifies size, update, and audit semantics.
- **Selectors and cross-namespace attachment.** MVP uses exact same-namespace
  names. A future grant model must precede cross-namespace references.

## 17. Phase 0 probes

The repository records eight completed probes:

- The package probe verifies the exact tarball digest, provider envelope, five
  drivers, settings RPC, Secret handling, executable defaults, orchestration
  fields, OTLP variables, and absence of wrapper-only fields.
- The isolated settings probe verifies authenticated updates, upstream-slug
  instance IDs, opaque config preservation, whole-map replacement, unrelated
  setting preservation, sensitive-value redaction, and sensitive-value
  rotation. It restores the original settings after the probe.
- The isolated auth probe uses the upstream CLI, pairing endpoint, and token
  exchange. It proves that a bearer with only `orchestration:read` and
  `orchestration:operate` can run the live probes. It renews that bearer,
  revokes the prior session, revokes each administrative bootstrap first, and
  removes every probe credential.
- The isolated provider probe exercises all five executable contracts. It
  materialises one enabled registry entry per built-in driver, verifies that
  every entry is available, preserves opaque desired config, and restores the
  original map. Cursor and Grok remain alpha because their snapshots lack
  authenticated coverage.
- The managed-setting drift probe races ten authorized whole-map writes against
  an authoritative reconciler. It removes UI-only instances, restores managed
  fields, preserves unrelated settings, and preserves unknown desired config.
- The sensitive-environment probe materialises two Codex instances and two
  Claude instances. It verifies distinct native homes, isolated child
  environments, sensitive-value redaction, and rotated-value injection into
  rebuilt provider processes.
- The restart-persistence probe starts a disposable server twice against one
  base directory. It preserves Codex and Claude settings, opaque config,
  sensitive environments, provider processes, and the administrative session.
  The idle server exits on `SIGTERM` without escalation.
- The authenticated-turn probe completes Codex and Claude turns, restarts t3,
  and verifies both conversation histories through new turns. It observes a
  Codex tool call as active and the completed session as idle. It also confirms
  that active-turn `SIGTERM` recovers as an error and that HTTP bootstrap
  dispatch retains the pinned upstream bug. The working path is the public
  WebSocket RPC used by the upstream client.

These behavioral probes remain release blockers:

1. **Pending-request and background drain states.** Capture
   `/api/orchestration/shell` while a turn waits for approval, waits for user
   input, and runs tracked background work. Verify that every state remains
   active until it finishes. Normal turns, tool calls, completion, idle
   sessions, idle shutdown, and active shutdown are proven. The missing public
   quiesce fence is an accepted pinned upstream limitation for MVP.
2. **Provider reload.** Add and remove one skill and MCP server for Codex,
   Claude, and OpenCode. Record whether the change applies by watcher, new turn,
   provider-process restart, or another upstream action.
3. **Existing-process Secret lifetime.** Rotate a sensitive environment value
   while a provider turn runs. Verify that the active process keeps its value,
   the turn completes, and the next process receives the rotated value.
4. **MCP dialects.** Prove remote HTTP headers, local stdio, Secret indirection,
   unknown config preservation, and reload behavior for Codex, Claude, and
   OpenCode. Record the upstream surface for Cursor and Grok as alpha evidence.
5. **OpenCode health process lifecycle.** Run local inventory under a clean
   home. Prove that `models --verbose`, `agent list`, and `debug skill` are
   bounded. Prove that instance removal terminates every child. The current
   CLI probe required `SIGKILL` after `agent list` ignored `SIGTERM`. A managed
   loopback server skips those commands, but its upstream SDK inventory calls
   also lack a timeout. Bound them without logging raw provider options.

## 18. Risks

1. **`t3` is 0.0.x with a nightly that moves several times a day.** Digest
   pinning plus last-known-good in status must exist before anyone runs nightly
   in anger.
2. **The authenticated control protocol changes.** Pinning and Phase 0 contract
   probes prevent a silent upgrade across that seam.
3. **No upstream quiesce operation exists.** MVP waits for stable public idle
   state and never signals work that the snapshot reports as active. It cannot
   close the final dispatch race. This is an accepted pinned upstream bug. A
   sidecar proxy is forbidden by the fail-open rule.
4. **Reload is not universally hot.** Under-promise in the API and report
   live-versus-pending honestly.
5. **The operator becomes critical-path for all agent work.** Hence fail open:
   test it by killing the operator with a session running.
6. **Harness auth state lives beside config.** The three write modes in section
   6 are the mitigation, and `Merge` on `.claude.json` is the specific one.
7. **Extensions are agent instructions.** Whoever can create one in a namespace
   can steer an agent holding a GitHub token and a kubectl MCP. Namespacing plus
   `Harness.spec.attachmentPolicy` bounds it; it does not eliminate it.
8. **Every Workstation process shares one Kubernetes identity.** Exact
   `resourceNames` limit Secret access, but attached agents can reach those same
   referenced Secrets. The Workstation is the trust boundary.
9. **A Workstation is a shared trust domain.** Harness attachment policy does
   not isolate processes that share UID, filesystem, network, and environment.
10. **Cursor and Grok lack authenticated end-to-end coverage.** Their alpha
    condition must stay visible until those tests run.
11. **OpenCode health inventory is unbounded in the pinned release.** A hung
    inventory child or SDK request can delay status and instance cleanup. Raw
    provider inventory can contain credentials. Phase 0 must settle a bounded,
    non-logging upstream path before image acceptance.
12. **Rendered protocol skew can strand an update.** Version negotiation and
    last-known-good application keep the current runtime usable.
13. **UI provider edits can race GitOps reconciliation.** They are unsupported
    and revert. A continuous writer can keep the map drifting even though the
    isolated probe converged after its writer stopped.
14. **The bootstrap CLI grants administrative scopes.** Its session lasts two
    minutes and is revoked after narrow token exchange. A crash can leave it
    active until expiry. A crashed narrow session can remain until restart or
    token expiry, but its raw bearer existed only in memory.
15. **Direct NFS depends on node DNS and server availability.** It is supported
    only for `/workspace`; `/data` needs storage with safe SQLite locking.
16. **Helm does not upgrade CRDs from `crds/`.** Flux must apply a compatible
    CRD before it upgrades the operator.
17. **Involuntary disruption can kill active work.** The PodDisruptionBudget
    blocks voluntary eviction, but node loss and forced deletion are outside
    the continuity guarantee.
18. **A restart loses resolved Secret history.** The sidecar persists no Secret
    values, and upstream t3 redacts them on reads. After a Secret replacement
    and sidecar restart, recovery cannot restore the older materialization
    exactly. It restores the previous manifest with the current Secret value.
19. **SSH Git sources use trust on first use.** The first connection stores its
    host key on retained `/data`. Later key changes fail. Full commit pinning
    protects content identity, but the first host-key observation is not
    independently anchored.
20. **Git transport size is not byte-bounded.** The exported tree is limited to
    512 MiB and 100,000 entries, and the fetch has a deadline. A remote Git pack
    can use more temporary storage before export. Namespace authors remain part
    of the Workstation trust boundary, and storage quotas remain necessary.
21. **A stalled NFS syscall cannot be canceled by Go context.** Repository scans
    have depth, entry, and interval limits. An unavailable NAS can still block a
    filesystem call until the kernel NFS client returns it. The last live agent
    configuration remains available while the sidecar status becomes stale.
22. **SMB requires a root sidecar.** The container keeps a read-only root
    filesystem, disables privilege escalation, and retains only `SETUID` and
    `SETGID`. Kubernetes Restricted Pod Security rejects it. SMB remains opt-in.
23. **SMB does not serialize local and remote edits.** Leases and opportunistic
    locks are disabled to reduce stale caches. Two writers can still race on one
    file. Users must avoid simultaneous edits or resolve them through Git.
24. **Port 445 must not be public.** SMB requires version 3 encryption and a
    password, but source ranges, NetworkPolicy, firewall policy, or VPN access
    must still limit the Service to trusted clients.
25. **A Pod rollout interrupts SMB clients.** The drain protects active agent
    work, not remote file transfers. The retained PVC preserves data, but clients
    must reconnect after a planned rollout or node failure.
26. **Secret rotation is eventually consistent.** Kubelet first updates the
    projected Secret. `t3-smbd` detects it within five seconds. Existing SMB
    sessions remain valid until the client disconnects.

## 19. Non-goals

- **No `home-ops` changes from this repository.** Migration is documented here,
  performed there.
- **No MVP changes to upstream `t3-code` or `traktuner/docker-t3-code`.** This
  replaces the latter and preserves pinned upstream behavior, including known
  bugs. Contributions can start after this project is usable.
- **Not a fork of t3-code.** Where upstream can do something, call it.
- **The wrapper is not an upstream contract.** It can inspire container layout,
  but its TOML and sync fields do not enter this API.
- **No agent scheduler or queue.** Drain observes upstream work state during a
  pod-shape rollout. Upstream t3 continues to own session orchestration.
- **No MCP server implementations.** Referenced, never hosted.
- **No per-repository toolchain management.** Section 7.
- **No observability stack in MVP.** Upstream OTLP integration comes later.
- **No cross-namespace or selector attachment in MVP.** Exact names only.
- **No user-space NFS mount.** Kubelet mounts NFS or a referenced claim at
  `/workspace`; `t3-coded` never mounts a filesystem.
- **No operator ownership of chart `extraObjects`.** Helm renders them, and
  their native controllers own their behavior.
