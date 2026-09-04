# Upstream t3 contract for the operator

Verified 2026-08-27 against the published `t3@0.0.34` npm artifact and
re-checked on 2026-09-04 against `t3@0.0.38` (stable) and
`t3@0.0.39-nightly.20260904.1276` (nightly).

## 2026-09-04 re-verification

- Every control surface below still exists in `0.0.38`: `server.updateSettings`,
  `/api/auth/pairing-token`, `/oauth/token`, the `orchestration:operate`
  scope, and `/api/orchestration/shell`.
- The stable `BUILT_IN_DRIVERS` list is unchanged: `codex`, `claudeAgent`,
  `cursor`, `grok`, `opencode`.
- The nightly adds a sixth driver, `antigravity`. It runs Google's official
  ACP agent, which t3 downloads into the environment on request (about 682 MB
  compressed, 2 GB extracted, per platform) and keeps under the server state
  directory. Auth methods are `oauth-personal`, `oauth-business`,
  `gemini-api-key`, and `agent-platform`; `enabled` defaults to false upstream.
  Each instance owns its Google profile, so multiple instances are supported.
- The environment label still resolves from `/etc/machine-info`
  `PRETTY_HOSTNAME`, then `hostnamectl --pretty`, then the hostname, then the
  working directory name. No environment variable overrides it. The operator
  therefore writes the Workstation name into `machine-info` by default.

This document separates upstream t3 facts from container-wrapper behavior. It
records the package contract and isolated auth, settings, provider, recovery,
and active-state probes. Section 17 of `PLAN.md` lists the remaining live
behavior probes.

## Pinned artifact

| Field | Observed value |
|---|---|
| Package | `t3@0.0.34` |
| npm tag | `latest` on 2026-08-26 |
| Tarball | `https://registry.npmjs.org/t3/-/t3-0.0.34.tgz` |
| SHA-256 | `abe4ccfbe656dcdeb846ffc59df79ab3dfd4f656efec3a16909695e133534684` |
| npm integrity | `sha512-n0wANFpl7KufqmVCXI1mYZJ2WFORBo+C5dfEiG7NGDZWmUUc0p9FQ/Ic4VkK+ToVxA1uSxVgUVI8gXNU62k3LA==` |
| Upstream repository | `https://github.com/pingdotgg/t3code` |
| Server entry point | `t3` → `./dist/bin.mjs` |
| Node engine | `^22.16 || ^23.11 || >=24.10` |

The published source maps contain the TypeScript sources used for this build.
They make the tarball the primary source for the pinned runtime contract.

## Findings

### Provider instances are the correct API seam

`ServerSettings.providerInstances` is an open record keyed by
`ProviderInstanceId`. Each value has this common envelope:

- `driver`
- optional `displayName`
- optional `accentColor`
- optional `environment`
- optional `enabled`
- optional opaque `config`

Driver and instance IDs use the same open slug. It is 1–64 characters, starts
with a letter, and then permits letters, digits, `_`, and `-`. Driver config
uses `Schema.Unknown`. This lets future drivers and unknown config round-trip
without a contract-layer schema change.

The runtime registry owns availability. An unknown driver remains persisted
but appears unavailable. This supports a string `Harness.spec.driver` and an
opaque `Harness.spec.config`.

Codex and Claude expose native `homePath` settings. Upstream turns them into
`CODEX_HOME` and `CLAUDE_CONFIG_DIR` for that instance. The other three drivers
do not share a common home setting. Their independent state layout needs an
adapter-specific live proof before two instances can share one Workstation.

### Five built-in drivers ship

The pinned `BUILT_IN_DRIVERS` list contains:

| Driver kind | Default executable | First-release status |
|---|---|---|
| `codex` | `codex` | supported |
| `claudeAgent` | `claude` | supported |
| `cursor` | `cursor-agent` | alpha |
| `grok` | `grok agent stdio` | alpha |
| `opencode` | `opencode` | supported |

Cursor uses ACP. Grok also uses ACP and invokes `grok agent stdio`. The alpha
labels reflect unavailable authenticated test environments. They do not mean
that the upstream adapters are absent.

The isolated registration probe exercised each executable and materialised one
enabled upstream entry for each driver. All five appeared as available registry
snapshots. Cursor was unauthenticated. Grok had unknown auth after its 15-second
model-discovery timeout. Both outcomes preserve their alpha status.

The OpenCode registration path used an explicit unreachable server URL. This
isolated registry hydration from local CLI health. A separate clean-home run of
`opencode agent list` did not stop within 20 seconds and ignored `SIGTERM`.
The pinned t3 source starts its three local inventory commands without a
timeout. Local OpenCode health and child cleanup remain release blockers.

A loopback `opencode serve` trial started and stopped cleanly. Its health
endpoint responded immediately. Its provider inventory took about 16 seconds
and returned about 4.6 MB. The response schema includes raw provider options,
which can contain credentials. No probe may log or persist that response.

Setting upstream `serverUrl` skips the unbounded CLI commands, but the pinned
t3 SDK inventory requests also have no timeout. A managed loopback server is
therefore not yet a bounded release workaround.

### Settings updates use upstream control

The authenticated websocket method is `server.updateSettings`. Its payload is
a `ServerSettingsPatch`. The method needs the orchestration operation scope.

`providerInstances` is a whole-map replacement inside that patch. The protocol
does not expose a compare-and-swap revision. Preserving UI-created instances
would therefore risk a lost update. The operator uses the attached Harnesses
as the complete desired map and preserves unrelated server settings. A live
probe must settle convergence under concurrent UI writes.

The isolated live probe confirmed whole-map replacement. It also confirmed
that a provider-only patch preserves an unrelated server setting. Its instance
IDs used the full upstream slug shape, including underscores.

The managed-setting drift probe ran ten authorized UI-style map writes while a
prototype reconciled the authoritative map. The last drift converged in 24 ms
in the recorded run. The reconciler removed the UI-only instance, restored the
managed field, preserved an unrelated setting, and preserved unknown desired
config. This proves eventual convergence after a writer stops. It does not make
continuous UI edits supported.

The pinned release has no separate provider-settings write scope or managed
settings lock. The client makes provider controls inert only when its session
lacks `orchestration:operate`. The server also requires that scope to dispatch
agent work. A read-only provider UI would therefore make the Workstation
read-only too. MVP must detect and revert provider-map drift until upstream
adds a narrower control.

The server watches and writes `settings.json` itself. With
`T3CODE_HOME=/data/t3`, the production path is
`/data/t3/userdata/settings.json`. The operator must use the upstream command
instead of editing that file.

### Sensitive provider environment is native

Each provider environment entry has `name`, `value`, `sensitive`, and optional
`valueRedacted` fields. The server moves sensitive values to its
`ServerSecretStore`. It creates the Secret directory with mode `0700` and
Secret files with mode `0600`.

The settings response and `settings.json` redact those values. Runtime provider
creation materializes the values and merges them into the child process
environment.

This supports dynamic Kubernetes Secret resolution without placing Secret
values in a rendered ConfigMap. `t3-coded` can resolve an exact Secret reference
and send the value as a sensitive environment entry.

The isolated settings probe sent and rotated a sensitive value. Both RPC
responses redacted the value and set `valueRedacted=true`.

A second probe materialised two Codex instances and two Claude instances with
distinct `homePath` values and sensitive environment entries. Each child
observed only its own value and its expected `CODEX_HOME` or
`CLAUDE_CONFIG_DIR`. The probe then rotated one value for each driver and
verified each hash inside a rebuilt child. It wrote no raw value to disk or
output. The lifetime of an already-running provider process remains
unverified.

### Provider state survives an idle server restart

The restart-persistence probe uses a fresh base directory and two enabled
instances. One uses Codex and one uses Claude. Each instance has an opaque
config field, an isolated home, and a sensitive environment value.

Runtime-generated wrappers observed each environment before and after the
restart. The settings remained redacted. The opaque config and the original
administrative session also survived. The first idle server exited on
`SIGTERM` without escalation. This proves provider-state persistence. It does
not prove conversation history by itself. The authenticated-turn probe below
proves that separate contract.

The first readiness request exposed a startup boundary. The server can accept
the TCP connection before it completes the HTTP response. The probe therefore
bounds each request, then verifies the authenticated session and WebSocket RPC.
Production `t3-coded` needs the same ordered readiness checks.

### Least-privilege sidecar auth uses upstream APIs

Settings reads require `orchestration:read`. Settings writes require
`orchestration:operate`. The websocket ticket endpoint preserves the scopes of
the bearer session that requests it.

The `t3 auth session issue` CLI command has no scope flag in `t3@0.0.34`. It
always issues an administrative bearer session. The authenticated
`/api/auth/pairing-token` endpoint accepts delegated scopes, and `/oauth/token`
exchanges the one-time credential for a bearer with the requested subset.
The token response contains its expiry. The pinned default session lifetime is
30 days, but a client must use the returned value instead of assuming it.

The isolated auth probe proved this sequence:

1. Issue a two-minute administrative bootstrap session through the upstream
   CLI and shared base directory.
2. Request a pairing credential with only the two orchestration scopes.
3. Exchange it through the upstream OAuth token endpoint.
4. Verify the narrow session, then revoke the administrative session.
5. Repeat the bootstrap and exchange before expiry.
6. Revoke the prior narrow session and prove its bearer is invalid.
7. Run the settings and provider probes with the renewed bearer.
8. Remove the renewed session through the upstream CLI.

Production `t3-coded` will keep the narrow bearer only in memory. It will renew
before expiry and revoke the prior session. The probe verified an early renewal
and prior-session revocation. It has not verified cleanup after an ungraceful
sidecar exit.

### The orchestration snapshot can inform drain

The authenticated `/api/orchestration/shell` response contains these fields:

- session status
- `activeTurnId`
- `hasPendingApprovals`
- `hasPendingUserInput`
- `backgroundLiveness`, with `working` and `monitoring` values

The full orchestration snapshot also contains session and active-turn state.
The package does not define a universal t3 session-wait route. A wait route in
the bundled OpenCode SDK is an OpenCode contract, not the t3 drain contract.

The pinned RPC and HTTP contracts expose no quiesce or turn-start gate. The
server sets its HTTP preemptive shutdown grace to zero and then closes runtime
scopes. Provider adapters expose internal stop-all operations, but those are
not a public fence.

The authenticated live probe observed an eight-second Codex tool call as
active. It observed Codex and Claude sessions as idle after turn completion.
It then restarted idle t3 and recovered both conversation histories through a
second turn on the same thread. The idle `SIGTERM` completed in about one
second.

A separate active-turn signal proved that `SIGTERM` is not a drain fence. T3
exited in 1.3 seconds. After restart, the interrupted turn and session both had
state `error`. The operator must finish snapshot-visible work before it signals
t3. The public API still cannot reject a new turn during the final idle-to-signal
race. MVP records and inherits this pinned upstream bug.

The WebSocket `orchestration.dispatchCommand` method implements bootstrap
thread creation. The HTTP dispatch route accepts the same bootstrap schema but
does not execute the bootstrap program. The live no-model check received status
500. The operator uses the WebSocket method that the upstream client uses.

### OTLP support is upstream

The server accepts `T3CODE_OTLP_TRACES_URL` and
`T3CODE_OTLP_METRICS_URL`. It builds OTLP trace and metric layers when those
settings exist. The package does not expose a Prometheus `/metrics` contract.

Observability remains outside MVP. A later stage should expose upstream OTLP
before it adds another telemetry path.

### Wrapper configuration is not upstream configuration

The server bundle contains none of these wrapper tokens:

- `t3code.toml`
- `T3CODE_CONFIG_PATH`
- `config_dir_source`
- `config_sync_mode`

Those names belong to `traktuner/docker-t3-code`. The wrapper can inform image
construction, but it cannot define the operator API or settings model.

## Design consequences

1. One `Harness` maps to one upstream provider instance.
2. The renderer produces the full desired provider envelope without Secret
   values.
3. `t3-coded` resolves Secret references and calls upstream control methods.
4. Attached Harnesses own the complete provider map; unrelated server settings
   survive.
5. The Workstation reconciler aggregates all child state before rendering.
6. Cursor and Grok ship as alpha until authenticated tests run.
7. Drain uses upstream orchestration state and needs a live quiesce proof.

## Repeatable verification

Run:

```console
./hack/phase0/t3-package-contract.sh
```

Expected output:

```text
verified t3@0.0.34 sha256=abe4ccfbe656dcdeb846ffc59df79ab3dfd4f656efec3a16909695e133534684
verified provider envelope, five drivers, settings RPC, scoped auth, Secret handling, orchestration fields, executables, and OTLP variables
```

The probe downloads the npm artifact. It validates the tarball before it reads
the package or embedded source.

### Isolated live auth and settings probe

Start the pinned server with an isolated base directory. Use its installed
`t3` executable for the probe.

Run:

```console
T3_PHASE0_T3_BIN=<path-to-pinned-t3> \
T3_PHASE0_BASE_DIR=<isolated-base-dir> \
T3_PHASE0_ALLOW_MUTATION=isolated \
T3_PHASE0_HTTP_URL=http://127.0.0.1:39773 \
node hack/phase0/t3-sidecar-auth-probe.mjs
```

Observed output:

```text
verified authenticated settings update, upstream-slug instance IDs, opaque config preservation, whole-map replacement, unrelated setting preservation, and sensitive value rotation
verified authoritative provider-map convergence after 10 competing writes in 9ms with 10 corrective writes while preserving unrelated settings and unknown desired config
verified two Codex and two Claude home paths, isolated process environments, sensitive-value redaction, and rotated-value injection into new provider processes
verified Codex and Claude settings, opaque config, sensitive environments, and provider processes across an idle t3 server restart
verified five executable versions; Grok stdio remained active until the probe terminated it: {"codex":"codex-cli 0.149.0","claudeAgent":"2.1.241 (Claude Code)","cursor":"2026.08.11-e8db854","grok":"grok 1.0.5 (5115b46bc9)","opencode":"1.18.21"}
verified five upstream provider registrations with isolated provider environments: [{"driver":"codex","installed":true,"status":"error","auth":"unknown","version":null},{"driver":"claudeAgent","installed":true,"status":"ready","auth":"authenticated","version":"2.1.241"},{"driver":"cursor","installed":true,"status":"error","auth":"unauthenticated","version":"2026.08.11-e8db854"},{"driver":"grok","installed":true,"status":"error","auth":"unknown","version":"1.0.5"},{"driver":"opencode","installed":true,"status":"error","auth":"unknown","version":null}]
verified upstream bootstrap, immediate admin revocation, narrow orchestration scopes, credential renewal, prior-session revocation, settings authorization, and credential cleanup
```

The settings probe adds two disabled Codex instances with unique IDs. The
sensitive-environment probe adds two enabled Codex instances and two enabled
Claude instances. The restart probe creates and removes its own base and state
directories. The provider probe adds five temporary enabled instances. It uses
an external OpenCode URL from `T3_PHASE0_OPENCODE_SERVER_URL`, or a closed
loopback port by default. All probes restore the original map. They refuse to
run without explicit `isolated` mutation consent. The wrapper removes every
uniquely labelled auth session and pairing credential after the probes finish.

### Authenticated recovery and active-state probe

Run:

```console
T3_PHASE0_T3_BIN=<path-to-pinned-t3> \
T3_PHASE0_ALLOW_MUTATION=isolated \
T3_PHASE0_ALLOW_MODEL_USAGE=authenticated \
node hack/phase0/t3-authenticated-turn-probe.mjs
```

Observed output:

```text
verified authenticated Codex and Claude conversation recovery across an idle t3 restart; idle SIGTERM completed in 1049ms
verified the public shell snapshot reports active work during a Codex tool call and reports idle after completion
verified the pinned HTTP dispatch bootstrap bug remains; authenticated turns use the upstream WebSocket RPC path
observed active-turn SIGTERM exit in 1307ms with restarted state {"active":false,"latestTurnState":"error","sessionStatus":"error"}
```

The probe requires explicit model-usage consent. It removes its isolated t3
state and workspace. The authenticated providers can keep their native session
history in the existing Codex and Claude homes.

The 2026-08-27 run used Node `v26.7.0` and an isolated
`t3@0.0.34` installation. It also confirmed the authenticated
`/api/orchestration/shell` endpoint on an empty server. The response had no
projects or threads and `snapshotSequence=0`.

The first server start failed after database migrations. Npm 12 had blocked the
install scripts for `node-pty@1.1.0` and `msgpackr-extract@3.0.4`. Approving and
rebuilding those exact packages produced the native modules and let t3 listen.
The runtime image must make this allowlist explicit and verify server startup.

Claude Code has the same install-script class of failure. Its postinstall
materializes the native binary over a small stub. The image must allow that
script and verify the executable instead of trusting npm's exit code.

The run did not verify crash cleanup, local OpenCode health, an already-running
process's Secret lifetime, pending approval, pending user input, or tracked
background-work state. Codex and Claude same-driver isolation, provider-state
persistence, conversation recovery, normal active state, idle state, and
active-shutdown failure are proven.

## Primary sources

- [Published npm artifact](https://registry.npmjs.org/t3/-/t3-0.0.34.tgz)
- [Upstream provider internals](https://github.com/pingdotgg/t3code/blob/main/docs/internals/providers.md)
- [Upstream provider installation](https://github.com/pingdotgg/t3code/blob/main/docs/user/install.md)
- [Upstream settings contract](https://github.com/pingdotgg/t3code/blob/main/packages/contracts/src/settings.ts)
- [Upstream remote execution model](https://github.com/pingdotgg/t3code/blob/main/docs/internals/remote.md)
- [Upstream npm install-script failure](https://github.com/pingdotgg/t3code/issues/5627)

The GitHub links track `main` and can move. The npm tarball and its SHA-256
remain the authority for this baseline.
