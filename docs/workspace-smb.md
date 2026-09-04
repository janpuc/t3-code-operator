# Share a PVC workspace through SMB

Use a claim-backed workspace for fast node storage. Enable SMB when a developer machine must mount the same workspace.

SMB is an access path. It is not a storage type. The runtime and SMB server mount the same PVC in one Pod.

The workspace PVC must differ from the `/data` PVC. The operator rejects a configuration that could expose runtime authentication state.

## Configure the Workstation

Create a password Secret. Do not store the password in chart values. The
default reference is `<workstation-name>-smb` under the key `password`, so a
Workstation named `nvme` reads `nvme-smb` unless `passwordSecretRef` says
otherwise.

```sh
kubectl -n agents create secret generic nvme-smb \
  --from-literal=password='replace-with-a-long-random-password'
```

Configure the PVC and SMB share:

```yaml
apiVersion: t3code.janpuc.com/v1alpha1
kind: Workstation
metadata:
  name: nvme
  namespace: agents
spec:
  image: ghcr.io/janpuc/t3-code-operator@sha256:...
  storage:
    data:
      type: ExistingClaim
      existingClaim:
        name: t3-code-data
    workspace:
      type: ClaimTemplate
      claimTemplate:
        spec:
          accessModes: [ReadWriteOnce]
          storageClassName: fast-nvme
          resources:
            requests:
              storage: 200Gi
        retentionPolicy: Retain
  workspaceSharing:
    smb:
      username: t3
      shareName: workspace
      passwordSecretRef:
        name: nvme-workspace-smb
        key: password
      resources:
        requests:
          cpu: 25m
          memory: 64Mi
        limits:
          memory: 256Mi
      service:
        type: LoadBalancer
        externalTrafficPolicy: Cluster
        loadBalancerSourceRanges:
          - 192.0.2.0/24
        annotations:
          lb.example.test/pool: lan
```

The operator creates the `nvme-smb` Service. The Service maps port 445 to an unprivileged container port.

Use `ClusterIP` when another in-cluster resource handles exposure. Use `LoadBalancer` for direct client access on port 445.

Most desktop SMB clients require port 445. A `NodePort` Service needs an external port 445 mapping.

Use `extraObjects` to add a NetworkPolicy or site-specific network resource. Keep port 445 on a trusted LAN or VPN.

## Mount the share

Use the assigned Service address and the configured username.

- macOS: open `smb://ADDRESS/workspace` in Finder.
- Windows: run `net use W: \\ADDRESS\workspace /user:t3 *`.
- Linux: mount with `-t cifs` and `vers=3.1.1`.

The server does not advertise through NetBIOS. Use an explicit address or DNS name.

The operator derives a stable Samba identity from the Workstation UID. Pod replacement does not change the server SID.

Deleting and recreating the Workstation creates a new identity, even when it reuses a retained PVC.

## Security and lifecycle

The SMB server requires SMB 3 encryption. It disables guest access, leases, and opportunistic locks.

The optional SMB container runs as UID 0. It drops all capabilities except `SETUID` and `SETGID`.

The container cannot mount `/data`. It has a read-only root filesystem and cannot enable privilege escalation.

Samba stores its runtime databases in an `EmptyDir`. The workspace PVC does not contain hidden Samba state.

A namespace that enforces the Kubernetes Restricted Pod Security Standard rejects this optional container. Baseline policy is sufficient.

The Secret is projected into the SMB container. Its value never enters a ConfigMap, argument, or environment variable.

The password must contain 1 to 1024 bytes. It must not contain a null byte or a line break.

Kubelet updates the projected Secret eventually. `t3-smbd` then updates Samba within five seconds. Existing SMB sessions remain connected.

Changing only the SMB Service does not roll the Workstation. Changing the SMB sidecar shape uses the normal idle drain policy.

The PVC remains retained according to its claim policy. Deleting or disabling SMB does not delete workspace data.

## Concurrent access

Samba leases and opportunistic locks are disabled because local agent processes also edit the PVC.

This setting reduces stale client caches. It cannot make simultaneous edits to one file safe.

Do not edit the same file from an agent and an SMB client at the same time. Use Git to resolve intentional concurrent changes.

See the [Samba `smb.conf` reference](https://www.samba.org/samba/docs/current/man-html/smb.conf.5) for the underlying server settings.
