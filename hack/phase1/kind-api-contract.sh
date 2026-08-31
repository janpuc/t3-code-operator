#!/usr/bin/env bash
set -euo pipefail

for required_command in kind kubectl; do
  if ! command -v "${required_command}" >/dev/null 2>&1; then
    echo "${required_command} is required" >&2
    exit 1
  fi
done

phase1_cluster_name="t3-code-operator-phase1-$$"
phase1_state_directory="$(mktemp -d)"
phase1_kubeconfig="${phase1_state_directory}/kubeconfig"

cleanup() {
  kind delete cluster --name "${phase1_cluster_name}" >/dev/null 2>&1 || true
  rm -r "${phase1_state_directory}"
}
trap cleanup EXIT

kind create cluster \
  --name "${phase1_cluster_name}" \
  --kubeconfig "${phase1_kubeconfig}" \
  --wait 90s

phase1_kubectl=(kubectl --kubeconfig "${phase1_kubeconfig}")
"${phase1_kubectl[@]}" apply -k config/crd
"${phase1_kubectl[@]}" wait --for=condition=Established --timeout=60s \
  crd/extensions.t3code.janpuc.com \
  crd/harnesses.t3code.janpuc.com \
  crd/mcpservers.t3code.janpuc.com \
  crd/workstations.t3code.janpuc.com
"${phase1_kubectl[@]}" create namespace phase1-api

if "${phase1_kubectl[@]}" apply -f - <<'YAML'
apiVersion: t3code.janpuc.com/v1alpha1
kind: MCPServer
metadata:
  name: invalid-header
  namespace: phase1-api
spec:
  transport: http
  headers:
    - name: Authorization
      value: inline
      valueFrom:
        secretKeyRef:
          name: credentials
          key: token
  harnessRefs:
    - name: codex
YAML
then
  echo "the API server accepted an MCPServer with two header value sources" >&2
  exit 1
fi

if "${phase1_kubectl[@]}" apply -f - <<'YAML'
apiVersion: t3code.janpuc.com/v1alpha1
kind: MCPServer
metadata:
  name: duplicate-headers
  namespace: phase1-api
spec:
  transport: http
  headers:
    - name: Authorization
      value: first
    - name: authorization
      value: second
  harnessRefs:
    - name: codex
YAML
then
  echo "the API server accepted duplicate case-insensitive header names" >&2
  exit 1
fi

"${phase1_kubectl[@]}" apply -f - <<'YAML'
apiVersion: t3code.janpuc.com/v1alpha1
kind: Harness
metadata:
  name: future-driver
  namespace: phase1-api
spec:
  instanceId: future_driver
  driver: futureDriver
  config:
    future:
      nested:
        enabled: true
        weights: [1, 2, 3]
  workstationRefs:
    - name: primary
---
apiVersion: t3code.janpuc.com/v1alpha1
kind: MCPServer
metadata:
  name: future-transport
  namespace: phase1-api
spec:
  transport: futureTransport
  config:
    future:
      nested:
        enabled: true
        weights: [1, 2, 3]
  harnessRefs:
    - name: codex
YAML

"${phase1_kubectl[@]}" patch harness future-driver -n phase1-api \
  --type=merge \
  --patch '{"spec":{"displayName":"Future driver"}}'
"${phase1_kubectl[@]}" patch mcpserver future-transport -n phase1-api \
  --type=merge \
  --patch '{"spec":{"headers":[]}}'

harness_nested_value="$("${phase1_kubectl[@]}" get harness future-driver -n phase1-api -o jsonpath='{.spec.config.future.nested.enabled}')"
mcp_nested_value="$("${phase1_kubectl[@]}" get mcpserver future-transport -n phase1-api -o jsonpath='{.spec.config.future.nested.enabled}')"

if [[ "${harness_nested_value}" != "true" ]]; then
  echo "the API server pruned nested Harness config" >&2
  exit 1
fi
if [[ "${mcp_nested_value}" != "true" ]]; then
  echo "the API server pruned nested MCPServer config" >&2
  exit 1
fi

echo "kind accepted all four CRDs, rejected invalid MCPServers, and preserved opaque config"
