#!/usr/bin/env bash
set -euo pipefail

readonly chart_dir="${1:-charts/t3-code-operator}"
readonly nfs_values_file="$chart_dir/tests/values-nfs.yaml"
readonly smb_values_file="$chart_dir/tests/values-smb.yaml"
readonly kubernetes_schema_version=1.35.0
render_dir="$(mktemp -d)"

cleanup() {
  if [[ -n "${render_dir:-}" && -d "$render_dir" ]]; then
    rm -rf -- "$render_dir"
  fi
}
trap cleanup EXIT

for command_name in helm kubeconform rg cmp; do
  command -v "$command_name" >/dev/null 2>&1 || {
    printf 'missing command: %s\n' "$command_name" >&2
    exit 1
  }
done

helm lint "$chart_dir"
helm template contract "$chart_dir" --namespace agents --include-crds --values "$nfs_values_file" >"$render_dir/nfs.yaml"
helm template contract "$chart_dir" --namespace agents --values "$nfs_values_file" \
  --show-only templates/clusterrole.yaml >"$render_dir/operator-role.yaml"
helm template contract "$chart_dir" --namespace agents --values "$smb_values_file" >"$render_dir/smb.yaml"

kubeconform -strict -ignore-missing-schemas -kubernetes-version "$kubernetes_schema_version" "$render_dir/nfs.yaml" "$render_dir/smb.yaml"

for kind in HTTPRoute ServiceAccount Role RoleBinding Workstation Harness; do
  rg -q "^kind: $kind$" "$render_dir/nfs.yaml" || {
    printf 'render lacks kind: %s\n' "$kind" >&2
    exit 1
  }
done

for required in \
  'type: NFS' \
  'server: bigmouth.internal' \
  'exportPath: /mnt/Vault/Workspace' \
  '/usr/bin/tini' \
  'request: 0s' \
  'backendRequest: 0s'; do
  rg -q --fixed-strings "$required" "$render_dir/nfs.yaml" || {
    printf 'render lacks value: %s\n' "$required" >&2
    exit 1
  }
done

for required in \
  'type: ClaimTemplate' \
  'storageClassName: fast-nvme' \
  'workspaceSharing:' \
  'shareName: workspace' \
  'name: nvme-workspace-smb' \
  'type: LoadBalancer' \
  'externalTrafficPolicy: Cluster' \
  '192.0.2.0/24'; do
  rg -q --fixed-strings "$required" "$render_dir/smb.yaml" || {
    printf 'SMB render lacks value: %s\n' "$required" >&2
    exit 1
  }
done

if rg -q 'secrets' "$render_dir/operator-role.yaml"; then
  printf 'operator ClusterRole must not read Secrets\n' >&2
  exit 1
fi

for crd in config/crd/bases/*.yaml; do
  cmp "$crd" "$chart_dir/crds/$(basename "$crd")"
done

printf 'verified chart CRDs, operator RBAC, NFS and SMB Workstations, custom resources, and zero stream timeouts\n'
