# Workstation reference

A `Workstation` is the machine: one Pod with retained `/data`, a `/workspace`,
and the providers, tools, and identity that run inside it. Every field except
`spec.providers` has a working default, so the smallest useful object is:

```yaml
apiVersion: t3code.janpuc.com/v1alpha1
kind: Workstation
metadata:
  name: t3-code
spec:
  providers:
    codex:
      enabled: true
```

That renders a Pod on the runtime image that shipped with the operator
release, a retained 20Gi `/data` claim and a retained 50Gi `/workspace` claim
on the cluster's default storage class, a `WaitForIdle` drain policy, and one
Codex provider instance. The environment shows up in every T3 client as
`t3-code`, the Workstation name.

## Providers

`spec.providers` is a map keyed by provider instance ID. Every entry needs an
explicit `enabled` value: nothing runs unless you opt in, and `enabled: false`
keeps a configured instance parked without deleting its settings.

```yaml
spec:
  providers:
    codex:
      enabled: true
    claude:
      enabled: true
      environment:
        - name: CLAUDE_CODE_OAUTH_TOKEN
          valueFrom:
            secretKeyRef:
              name: t3-code-config
              key: CLAUDE_CODE_OAUTH_TOKEN
    review:
      enabled: true
      driver: codex
      displayName: Codex (review)
```

| Field | Default | Notes |
|---|---|---|
| `driver` | derived from the key | `codex`, `claude`, `opencode`, `cursor`, `grok`, and `antigravity` resolve to their upstream driver. Any other key needs an explicit driver. |
| `enabled` | required | Explicit opt-in. |
| `displayName` | the driver's name | Shown in the T3 client. |
| `accentColor` | none | Passed through to upstream. |
| `environment` | none | Secret-backed values reach the provider process only; the rendered manifest never contains them. |
| `models` | none | Shorthand for `config.customModels`. Setting both is an error. |
| `config` | none | Opaque upstream provider settings, including the adapter-owned `file` overlay. |
| `attachmentPolicy` | `SameNamespace` | Whether Extensions and MCPServers in the namespace may attach. |

Keys are lowercase DNS labels that start with a letter, so an Extension or
MCPServer can reference them by name. A `Harness` object is still the way to
share one provider definition across several Workstations or to use an
instance ID that is not a DNS label; a Harness named after a built-in provider
gets the same driver and display-name defaults.

Provider content is not pod shape. Adding, editing, or disabling a provider
publishes a new rendered revision that the sidecar applies while the Pod keeps
running. Only `image`, `env`, `resources`, `securityContext`, storage, and SMB
changes roll the Pod, and those go through the drain policy.

## Image

`spec.image` is optional. When it is empty, the operator uses the runtime image
passed through `--default-workstation-image`, which the chart sets from
`workstation.image` and which each release pins to the runtime digest built
alongside the operator. Set `spec.image` to a digest-pinned reference to run a
different track, for example the nightly runtime that carries the Antigravity
driver.

## Storage

Both volumes default to an operator-managed `ClaimTemplate` with the `Retain`
retention policy. Override only what differs:

```yaml
spec:
  storage:
    data:
      claimTemplate:
        spec:
          storageClassName: fast-nvme
          resources:
            requests:
              storage: 30Gi
    workspace:
      type: NFS
      nfs:
        server: nas.internal
        exportPath: /export/workspace
```

| Volume | Default type | Default size | Other types |
|---|---|---|---|
| `data` | `ClaimTemplate` | 20Gi | `ExistingClaim`, `EmptyDir` (needs `disposable: true`) |
| `workspace` | `ClaimTemplate` | 50Gi | `ExistingClaim`, `NFS`, `EmptyDir` |

A `claimTemplate` without `accessModes` gets `ReadWriteOnce`; without a storage
request it gets the default size. `type` is inferred from the branch you set
and only needs spelling out when no branch is present, which is never
required: an empty volume means a defaulted `ClaimTemplate`.

## SMB workspace sharing

`spec.workspaceSharing.smb: {}` shares a claim-backed workspace through the
SMB sidecar with the user `t3`, the share name `workspace`, a `ClusterIP`
Service, and the password in the `<workstation>-smb` Secret under the key
`password`. Read [the SMB workspace guide](workspace-smb.md) for LoadBalancer
exposure and source ranges.

## Environment name

T3 clients label an environment from `/etc/machine-info` `PRETTY_HOSTNAME`.
The operator writes the Workstation name there unless
`spec.machineInfo.prettyHostname` says otherwise, so the name in
`kubectl get workstations` and the name in the T3 client are the same string.

## Other fields

| Field | Default |
|---|---|
| `serviceAccountName` | `default` |
| `securityContext` | non-root, no privilege escalation, `RuntimeDefault` seccomp |
| `resources` | none |
| `env` | none; runtime-owned variables such as `HOME` and `PATH` are rejected |
| `git` | none; see the [providers guide](providers.md) for identity and signing |
| `tools` | none; pinned mise tools that install without a pod rollout |
| `drain` | `WaitForIdle`, `30m`, `Block` |
