#!/bin/sh
set -eu

baseline_dir=/opt/t3-code-baseline/source
binary_dir=/opt/t3-code-baseline/bin
data_dir=/opt/mise
state_root=/tmp/t3-baseline-mise

mkdir -p "$binary_dir" "$data_dir" "$state_root/cache" "$state_root/config" "$state_root/state" "$state_root/system"
: >"$state_root/global.toml"

export MISE_DATA_DIR="$data_dir"
export MISE_CACHE_DIR="$state_root/cache"
export MISE_CONFIG_DIR="$state_root/config"
export MISE_STATE_DIR="$state_root/state"
export MISE_SYSTEM_CONFIG_DIR="$state_root/system"
export MISE_GLOBAL_CONFIG_FILE="$state_root/global.toml"
export MISE_TRUSTED_CONFIG_PATHS="$baseline_dir"
export MISE_SAFE=1
export MISE_NO_HOOKS=1
export MISE_YES=1
export CI=1
export NO_COLOR=1

mise --locked --no-hooks --yes -C "$baseline_dir" install --jobs 1
mise --no-hooks -C "$baseline_dir" bin-paths --json >"$state_root/bin-paths.json"

for name in actionlint flux gh koment minijinja-cli helm helmfile just k9s kubeconform kubectl kustomize lefthook rg stern talosctl zizmor; do
	path=$(jq -er --arg name "$name" '[.[] | select(.name == $name)][0].path' "$state_root/bin-paths.json")
	test -x "$path"
	test ! -e "$binary_dir/$name"
	ln -s "$path" "$binary_dir/$name"
done

rm -rf "$state_root"
