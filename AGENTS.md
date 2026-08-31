# Agent rules for t3-code-operator

## What this repository is

A Kubernetes operator that manages t3-code: agent harnesses, the skills and
plugins they load, the MCP servers they talk to, and the workload they run in.
It exists so that changing an agent's configuration does not destroy the agent's
work.

It is a tribute to and a consumer of [t3-code](https://www.npmjs.com/package/t3),
not a fork of it. When upstream t3-code can do something, call it; do not
reimplement it.

## The one invariant

**A content change must never restart the runtime pod.**

Adding a skill, editing an MCP server, enabling a plugin: none of these may kill
a running agent session. If you find yourself writing a controller that bumps a
pod template hash for a content change, you have broken the reason this project
exists. Only an image or a pod-shape change may roll the workload, and even then
it goes through the drain policy.

## Fail open

If the operator is down, running harnesses keep working from the state already
materialised on disk. The control plane is not on the critical path for a
session that has already started. Never write a controller whose failure mode is
"the agent stops working" when "the agent keeps working with slightly stale
config" is available.

## Two planes, and what belongs in each

- **Operator** (`internal/controller`, `internal/render`): watches CRDs,
  validates, resolves digests, renders. All dialect knowledge lives in
  `internal/render` and nowhere else.
- **`t3-coded`** (`cmd/t3-coded`): the in-pod sidecar. Watches one rendered
  ConfigMap, pulls OCI bundles, writes files, triggers reload, reports what is
  live.

If the sidecar starts making policy decisions, the split has failed. It applies;
it does not decide.

## Rendered output must never contain a secret

All three harness dialects support environment indirection: Claude interpolates
`${VAR}`, Codex takes `bearer_token_env_var`, OpenCode takes `{env:VAR}`. The
renderer emits references, never values, so the rendered ConfigMap stays
non-sensitive and auditable. A test asserts this; do not weaken it.

## Never clobber harness state

A harness home directory holds authentication and session state next to its
config. The renderer writes config files by explicit allow-list. It does not
sync directories, and it does not delete what it did not write.

Concretely, every target file has one of three write modes, chosen by the
adapter and never by the user:

- `Replace` — overwrite unconditionally.
- `SeedIfAbsent` — write only when missing or unparseable.
- `Merge` — deep-merge, preserving unknown keys.

`.claude.json` is `Merge` because it holds OAuth state beside MCP config. A
renderer that only knows how to write files will destroy authentication on the
first reconcile. Users should never have to know which files are dangerous.

## Model t3's model, do not invent one over it

A `Harness` is one t3 provider instance. t3 keys `providerInstances` by an
arbitrary `instanceId` and each instance carries a `driver`, so two instances
may share a driver and differ only in config and home path.

`driver` is therefore a **string, never an enum**. A new t3 driver must never
force a schema change. Implement adapters for the drivers actually in use; the
schema admits the rest without them.

A custom endpoint is config, not a driver.
