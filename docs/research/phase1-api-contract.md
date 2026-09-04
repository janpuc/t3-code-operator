# Phase 1 API contract

Verified on 2026-08-27.

## Baseline

- Go `1.27.1`
- controller-runtime `0.25.0`
- controller-tools `0.22.0`
- Kubernetes API server `1.37.0`

The controller-runtime and controller-tools versions both use Kubernetes
library version `0.37.0` and require Go `1.26`. The original verification on
2026-08-27 used Go `1.26.7`, controller-runtime `0.24.1`, controller-tools
`0.21.0`, and API server `1.36.0`; the same checks were repeated on 2026-09-04
after the upgrade.

## Executed checks

```sh
make manifests generate
go test ./...
go vet ./...
make test-api
```

Generation produced identical file hashes on the second run.

The API-server test installed all four generated CRDs. It then verified these
contracts:

- A header cannot contain both `value` and `valueFrom`.
- Header names must be unique when compared without letter case.
- Unknown nested Harness config survives a typed update.
- Unknown nested MCPServer config survives a typed update.
- A direct NFS `/workspace` configuration is accepted and preserved.
- Drain defaults become `WaitForIdle`, `30m`, and `Block`.
- `/data` cannot use `EmptyDir` unless `disposable` is true.

The test downloads fixed Kubernetes `1.37.0` envtest assets from the
controller-tools `v0.22.0` release index. Normal unit tests do not download or
start an API server.

## Kind replay

`hack/phase1/kind-api-contract.sh` contains the equivalent kind acceptance
test. It creates an isolated cluster, installs the CRDs, verifies rejection,
verifies opaque-field preservation, and deletes only that test cluster.

The current host has no kind binary or container runtime. Therefore the kind
path was not executed here. `make test-kind` fails before cluster creation with
`kind is required`.

## Primary sources

- [Go release history](https://go.dev/doc/devel/release)
- [controller-runtime compatibility](https://github.com/kubernetes-sigs/controller-runtime)
- [controller-tools releases](https://github.com/kubernetes-sigs/controller-tools/releases)
