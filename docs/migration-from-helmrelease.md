# Migration from the current HelmRelease

This migration uses one idle cutover window.

The retained data claim preserves t3 sessions and authentication state.
The NFS workspace stays mounted at `/workspace`.

This procedure does not promise uninterrupted network connections.
It prevents two t3 workloads from writing the same data claim concurrently.

## Preconditions

Record these values before the cutover:

- The namespace and name of the current HelmRelease.
- The current Deployment and Service names.
- The retained data claim name.
- The NFS server and export path.
- The current runtime image digest and t3 version.
- Each referenced Secret name and key.
- The runtime ServiceAccount and required project RBAC.
- The HTTPRoute name and its current backend.

Create a recoverable backup of the data claim before the cutover.
Back up writable NFS data according to the NAS policy.

Use a new Workstation name, such as `primary`.
Do not reuse the old Deployment name during the first cutover.

Pin `Workstation.spec.image` by digest.
Use the stable image unless a nightly-only feature is required.

Keep the old and new t3 versions compatible during the rollback window.
An older t3 version might not read state changed by a newer version.

## Stage the control plane

1. Apply the CRDs before the operator upgrade.

   ```sh
   kubectl apply --server-side -f config/crd/bases/
   ```

2. Install the operator without a Workstation.

   Keep `workstations`, `harnesses`, `extensions`, and `mcpServers` empty initially.

3. Apply the Harness, Extension, and MCPServer objects.

   Their references can target the future `primary` Workstation.

4. Verify that each object passes API validation.

   ```sh
   kubectl get harnesses,extensions,mcpservers -n <namespace>
   ```

Do not create the Workstation yet.
Creating it starts the new workload immediately.

## Prepare the Workstation

Use `ExistingClaim` for the retained `/data` claim.
Use `NFS` for the NAS-backed `/workspace` mount.

The chart fixture provides a complete storage example:
`charts/t3-code-operator/tests/values-nfs.yaml`.

Configure GitHub credentials with both fields:

```yaml
git:
  userName: Example User
  userEmail: user@example.com
  githubUser: example-user
  credentialSecretRef:
    name: t3-code-config
    key: GH_TOKEN
```

The operator writes GitHub CLI credentials into `/data/t3-coded/gh`.
It does not change user-owned files under `/data/home/.config/gh`.

Keep HTTPRoutes, ExternalSecrets, backup objects, and project RBAC outside the operator domain.
The chart can render these resources through `extraObjects`.

Do not let two Helm releases own the same extra object.
Use new names or transfer ownership in a separate GitOps change.

## Perform the idle cutover

1. Suspend reconciliation of the current HelmRelease.

   This prevents Flux from restoring the old replica count.

2. Wait until all ongoing work finishes.

   Running turns, approvals, user-input requests, and tracked background work are active.
   A session waiting for its next human message is idle.

3. Scale the old Deployment to zero replicas.

   ```sh
   kubectl scale deployment/<old-deployment> -n <namespace> --replicas=0
   kubectl wait pod -n <namespace> -l <old-selector> --for=delete --timeout=5m
   ```

4. Confirm that no Pod mounts the retained data claim.

5. Apply the prepared Workstation.

6. Wait for the Workstation and Deployment.

   ```sh
   kubectl wait workstation/primary -n <namespace> --for=condition=Ready --timeout=10m
   kubectl rollout status deployment/primary -n <namespace> --timeout=10m
   ```

The sidecar readiness probe stays false until two conditions pass.
It requires one live rendered revision and successful upstream authentication.

7. Change the HTTPRoute backend to Service `primary` on port `3773`.

   Keep both stream timeouts at `0s`.

8. Verify the migrated runtime.

   - Open an existing t3 thread.
   - Continue that thread with one new turn.
   - Start one new Codex turn.
   - Start one new Claude turn.
   - Verify the configured OpenCode provider.
   - Verify `/workspace` contains the expected NFS data.
   - Create and read one disposable NFS sentinel file.
   - Verify GitHub CLI authentication without printing its token.
   - Verify commit signing when a signing key is configured.
   - Verify every MCP server and Extension attachment status.

9. Record the Workstation Pod UID.

10. Change one content object and wait for its live revision.

11. Confirm that the Workstation Pod UID did not change.

## Finish GitOps ownership

Move these resources before removing the old HelmRelease:

- HTTPRoute objects.
- ExternalSecret objects.
- Runtime ServiceAccount and project RBAC.
- Backup resources.
- Any policy or network resources owned by the old release.

The operator creates its own Deployment, Service, PodDisruptionBudget, ConfigMaps, and exact-name sidecar RBAC.

Remove the old HelmRelease only after all shared resources have independent ownership.
Then remove the old suspended workload resources.

## Roll back

Use rollback only while the old t3 version remains state-compatible.

1. Stop new user traffic.

2. Wait until the Workstation becomes idle.

3. Change the HTTPRoute backend to the old Service.

4. Delete the `primary` Workstation.

   Its finalizer waits for active work by default.
   Its `ExistingClaim` data volume remains intact.

5. Wait until Deployment `primary` is deleted.

6. Scale the old Deployment to one replica.

7. Resume the old HelmRelease after its service becomes ready.

8. Reopen an existing thread and run one verification turn.

Never run the old and new Deployments against the same `/data` claim.

## Release gates

Do not declare migration complete until all conditions pass:

- The existing session continues after cutover.
- NFS data survives the cutover.
- The sidecar reports the desired revision as live.
- Provider update checks remain disabled.
- Secret values do not appear in rendered ConfigMaps or status.
- A content change does not restart the Pod.
- A pod-shape change waits for active work.
- Cursor and Grok remain explicitly marked alpha without authenticated tests.
