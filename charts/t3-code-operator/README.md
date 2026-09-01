# t3-code-operator chart

This chart installs the operator, its RBAC, and the four CRDs.

Install a released chart from GHCR:

```sh
helm install t3-code-operator \
  oci://ghcr.io/janpuc/charts/t3-code-operator \
  --namespace t3-code-system \
  --create-namespace \
  --version 0.1.3
```

Set both operator and Workstation image digests in production values. Release
notes list the immutable runtime image digest.

The chart can also render Workstation, Harness, Extension, and MCPServer objects. Each list is empty by default.

The chart grants the operator `get` and `watch` only for Secret names referenced by chart-managed resources. Kubernetes requires these permissions before the operator can create narrower sidecar Roles. The operator does not fetch Secret values.

Add Secret names to `rbac.secretResourceNames` when `extraObjects` or separately managed custom resources reference them.

Use `extraObjects` for site resources such as HTTPRoutes, ServiceAccounts, Roles, RoleBindings, ExternalSecrets, and backup objects. Helm owns these objects. Their native controllers own their runtime behavior.

The chart includes NFS and PVC-with-SMB test values. The NFS values also set both HTTPRoute stream timeouts to `0s`.

Use `spec.storage.workspace.type: ClaimTemplate` for a fast, operator-managed PVC. Add `spec.workspaceSharing.smb` to share that PVC.

Create the password Secret before the Workstation:

```sh
kubectl -n agents create secret generic nvme-workspace-smb \
  --from-literal=password='replace-with-a-long-random-password'
```

The operator creates `<workstation-name>-smb`. Set its type to `LoadBalancer` for direct access on port 445.

A `NodePort` Service usually needs an external port 445 mapping because desktop SMB clients expect the standard port.

See [`tests/values-smb.yaml`](tests/values-smb.yaml) for a complete chart example. Restrict the source ranges to trusted networks.

Helm installs files from `crds/` only during the first installation. Helm does not upgrade them. Apply compatible CRDs before an operator upgrade, or let Flux manage them separately.

Always pin `Workstation.spec.image` by digest.

Use the [idle migration procedure](../../docs/migration-from-helmrelease.md)
when an existing workload already owns the retained data claim.
