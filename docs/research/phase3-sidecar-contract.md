# Phase 3 sidecar contract

Verified on 2026-08-28 against upstream `t3@0.0.34` and mise `2026.8.12`.

## Responsibility boundary

`t3-coded` applies one renderer manifest. It does not select drivers, sources,
permissions, rollout policy, or tool versions.

The sidecar reads one exact-name ConfigMap. It resolves exact-name Secrets in
the Workstation namespace. It writes one bounded report to one exact-name
status ConfigMap.

The rendered ConfigMap and sidecar report never contain Secret values. The
sidecar keeps resolved values in memory and sends sensitive provider values
through upstream t3.

Each new sidecar report contains the Pod revision from its Pod annotation. The
operator accepts `Programmed` only when the report and Deployment revisions
match. This rule prevents a stale report from the previous Pod from marking a
replacement Pod ready.

## Apply transaction

The sidecar stages files, tools, and Extensions before it reads activity.
Staging writes only isolated, inactive revisions.

It commits a revision in this order:

1. Commit managed harness files.
2. Activate the resolved mise tool revision.
3. Activate Extension links and native installer state.
4. Replace the upstream t3 provider instance map.
5. Persist the secret-free live manifest.

Each step records a local journal on retained `/data`. A failed step restores
the previous files, tools, Extensions, and upstream provider map.

The sidecar keeps the last resolved provider settings in memory. This snapshot
lets one running sidecar restore the previous value after a failed Secret
rotation. It never writes this snapshot to disk.

A restart reads an unfinished journal. Recovery waits while affected provider
instances remain active. It then restores the last persisted revision.

Upstream t3 redacts sensitive values when the sidecar reads settings. Therefore,
a restarted sidecar cannot recover an older Secret value after Kubernetes has
replaced that value. Recovery can restore the previous manifest with the current
Secret value, but it cannot restore the previous materialization exactly. This
limit avoids storing Secret values in the journal or state file.

Journal paths reject symbolic links. Journal input is bounded to the renderer
manifest limit. A completed state write can be retried after journal cleanup
fails without colliding with the previous transaction directory.

SSH Git sources use OpenSSH `accept-new` verification. The sidecar stores the
first host key in `/data/t3-coded/ssh/known_hosts` and rejects later key changes.
The pinned Git commit still verifies the fetched content identity.

## Activity and continuity

The upstream activity reader treats these states as active:

- a starting or running session;
- a non-null active turn;
- a pending approval;
- pending user input;
- working or monitoring background liveness;
- an unknown upstream state.

Activity must remain idle for five continuous seconds. A session that waits for
its next human message is idle and can accept a disruptive apply or rollout.

Upstream `t3@0.0.34` has no public turn-start fence. A new turn can race the
short commit window after the last idle sample. The operator does not add a
proxy because a proxy would put the control plane on the session data path.

## Tool activation

The operator resolves each tool to an exact backend, version, artifact URL,
and SHA-256 value. `t3-coded` gives each complete tool-set digest an isolated
mise data directory.

The retained download cache is shared. Installed executable directories are
not shared between different resolved tool sets.

Extension fetches, native plugin commands, and mise commands have fixed
deadlines. A stalled external source fails the pending revision and leaves the
live revision unchanged.

Archive and OCI sources have a 512 MiB content limit and a 100,000-entry
limit. OCI transfer descriptors use the same aggregate limits. The sidecar
preflights compressed OCI directory layers before ORAS extracts them.

The Workstation PATH includes `/data/t3-coded/tools/current/bin`. The sidecar
changes `current` with an atomic symbolic-link replacement.

See [the mise runtime contract](mise-runtime-contract.md) for the backend and
lockfile boundary.

## Upstream control

The sidecar checks the exact pinned t3 version before it applies a manifest. It
uses upstream pairing and OAuth APIs to create a narrow session with these
scopes:

- `orchestration:read`
- `orchestration:operate`

It holds the bearer only in memory. It renews the session before expiry and
revokes the previous session. Renewal failure blocks new applies. It does not
stop t3 or a running provider process.

Provider changes use the authenticated upstream WebSocket settings RPC. The
sidecar applies the update-check policy and provider map from the rendered
manifest in one patch. Drift detection verifies both managed fields. The
sidecar does not edit upstream `settings.json`.

## Executed verification

These checks passed:

```text
go test -race ./internal/render ./internal/t3client ./internal/apply ./internal/sidecar ./cmd/t3-coded
go vet ./...
T3_MISE_INTEGRATION=1 T3_MISE_BINARY=/data/home/.local/bin/mise \
  go test ./internal/apply -run TestMiseRuntimeInstallsResolvedArtifact -count=1 -v
T3_CLIENT_INTEGRATION=1 T3_CLIENT_T3_BINARY=<isolated-t3-0.0.34> \
  go test ./internal/t3client -run TestRealT3ControlClient -count=1 -v
```

The real t3 test started an isolated web server. It issued and revoked the
sidecar session. It replaced and read back a disabled Codex provider instance.
It also confirmed that the isolated instance was idle.

The cached 0.0.34 npm package lacked its Linux `node-pty` native module because
of the documented upstream install bug. The probe used a disposable package
copy with the matching native module. Production code contains no workaround
for that upstream bug.

## Remaining live acceptance

The local and fake-client tests cover rollback, Secret rotation, crash
recovery, all four Extension source types, installer rollback, exact-name
Kubernetes access, stable idle, auth renewal, and report redaction.

These Phase 3 acceptance checks still need a Kubernetes runtime:

- rotate a real Kubernetes Secret and start a provider process with the value;
- add an Extension and observe it in a new turn without changing pod start time;
- defer a disruptive apply through a long live turn;
- interrupt the sidecar between real filesystem and native installer commits;
- confirm stale-session cleanup after an ungraceful sidecar exit.

This host has no container runtime. Therefore, it cannot run the kind-based
checks here.
