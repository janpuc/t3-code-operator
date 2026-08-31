#!/usr/bin/env bash
set -euo pipefail

readonly t3_version="0.0.34"
readonly t3_archive_sha256="abe4ccfbe656dcdeb846ffc59df79ab3dfd4f656efec3a16909695e133534684"
probe_dir="$(mktemp -d)"

cleanup_probe_dir() {
  if [[ -n "${probe_dir:-}" && -d "$probe_dir" ]]; then
    rm -rf -- "$probe_dir"
  fi
}

fail_probe() {
  printf 'contract failure: %s\n' "$1" >&2
  exit 1
}

require_command() {
  command -v "$1" >/dev/null 2>&1 || fail_probe "missing command: $1"
}

require_literal() {
  local literal="$1"
  local file="$2"
  local label="$3"
  rg -q --fixed-strings -- "$literal" "$file" || fail_probe "$label"
}

reject_literal() {
  local literal="$1"
  local file="$2"
  local label="$3"
  if rg -q --fixed-strings -- "$literal" "$file"; then
    fail_probe "$label"
  fi
}

extract_source() {
  local suffix="$1"
  local output="$2"
  jq -r --arg suffix "$suffix" '
    .sources as $sources
    | .sourcesContent as $contents
    | [range(0; $sources | length)
       | select($sources[.] | endswith($suffix))
       | $contents[.]]
    | first // ""
  ' "$source_map" >"$output"
  [[ -s "$output" ]] || fail_probe "source map is missing $suffix"
}

trap cleanup_probe_dir EXIT

for required_command in npm tar jq rg sha256sum awk; do
  require_command "$required_command"
done

npm pack --silent --pack-destination "$probe_dir" "t3@$t3_version" >/dev/null
archive="$probe_dir/t3-$t3_version.tgz"
[[ -f "$archive" ]] || fail_probe "npm did not produce $archive"

actual_sha256="$(sha256sum "$archive" | awk '{print $1}')"
[[ "$actual_sha256" == "$t3_archive_sha256" ]] || fail_probe "archive SHA-256 changed: $actual_sha256"

tar -xzf "$archive" -C "$probe_dir"
package_dir="$probe_dir/package"
package_json="$package_dir/package.json"
bundle="$package_dir/dist/bin.mjs"
source_map="$package_dir/dist/bin.mjs.map"

jq -e --arg version "$t3_version" '
  .name == "t3"
  and .version == $version
  and .bin.t3 == "./dist/bin.mjs"
  and .repository.url == "https://github.com/pingdotgg/t3code"
' "$package_json" >/dev/null || fail_probe "package metadata changed"

extract_source "packages/contracts/src/providerInstance.ts" "$probe_dir/providerInstance.ts"
extract_source "packages/contracts/src/settings.ts" "$probe_dir/settings.ts"
extract_source "../src/serverSettings.ts" "$probe_dir/serverSettings.ts"
extract_source "../src/provider/builtInDrivers.ts" "$probe_dir/builtInDrivers.ts"

for driver_class in CodexDriver ClaudeDriver CursorDriver GrokDriver OpenCodeDriver; do
  require_literal "$driver_class" "$probe_dir/builtInDrivers.ts" "built-in driver missing: $driver_class"
done

for driver_kind in codex claudeAgent cursor grok opencode; do
  require_literal "ProviderDriverKind.make(\"$driver_kind\")" "$bundle" "driver kind missing: $driver_kind"
done

require_literal 'config: Schema.optionalKey(Schema.Unknown)' "$probe_dir/providerInstance.ts" "provider config is no longer opaque"
require_literal 'environment: Schema.optionalKey(ProviderInstanceEnvironment)' "$probe_dir/providerInstance.ts" "provider environment envelope changed"
require_literal 'const PROVIDER_SLUG_PATTERN = /^[a-zA-Z][a-zA-Z0-9_-]*$/' "$bundle" "provider slug rules changed"
require_literal 'providerInstances: Schema.optionalKey(' "$probe_dir/settings.ts" "providerInstances patch disappeared"
require_literal 'enableProviderUpdateChecks: Schema.optionalKey(Schema.Boolean)' "$probe_dir/settings.ts" "provider update-check setting disappeared"
require_literal 'Whole-map replacement' "$probe_dir/settings.ts" "providerInstances patch semantics changed"
require_literal 'persistProviderEnvironmentSecrets' "$probe_dir/serverSettings.ts" "sensitive environment persistence changed"
require_literal 'serverUpdateSettings: "server.updateSettings"' "$bundle" "settings RPC method changed"
require_literal 'serverSettings.updateSettings(patch)' "$bundle" "settings RPC handler changed"
require_literal 'join(stateDir, "settings.json")' "$bundle" "settings path contract changed"
require_literal 'const AuthOrchestrationReadScope = "orchestration:read"' "$bundle" "orchestration read scope changed"
require_literal 'const AuthOrchestrationOperateScope = "orchestration:operate"' "$bundle" "orchestration operation scope changed"
require_literal '[WS_METHODS.serverGetSettings]: AuthOrchestrationReadScope' "$bundle" "settings read authorization changed"
require_literal '[WS_METHODS.serverUpdateSettings]: AuthOrchestrationOperateScope' "$bundle" "settings update authorization changed"
require_literal '/api/auth/pairing-token' "$bundle" "pairing-token endpoint changed"
require_literal '/oauth/token' "$bundle" "token-exchange endpoint changed"
require_literal 'const DEFAULT_SESSION_TTL = days(30)' "$bundle" "session TTL default changed"
require_literal 'title: "CODEX_HOME path"' "$bundle" "Codex home-path setting changed"
require_literal 'title: "CLAUDE_CONFIG_DIR path"' "$bundle" "Claude home-path setting changed"

for orchestration_field in activeTurnId hasPendingApprovals hasPendingUserInput backgroundLiveness; do
  require_literal "$orchestration_field" "$bundle" "orchestration field missing: $orchestration_field"
done

require_literal '/api/orchestration/shell' "$bundle" "orchestration shell endpoint changed"
require_literal 'makeBinaryPathSetting("codex")' "$bundle" "Codex executable default changed"
require_literal 'makeBinaryPathSetting("claude")' "$bundle" "Claude executable default changed"
require_literal 'makeBinaryPathSetting("cursor-agent")' "$bundle" "Cursor executable default changed"
require_literal 'makeBinaryPathSetting("grok")' "$bundle" "Grok executable default changed"
require_literal 'args: ["agent", "stdio"]' "$bundle" "Grok ACP launch arguments changed"
require_literal 'makeBinaryPathSetting("opencode")' "$bundle" "OpenCode executable default changed"
require_literal 'T3CODE_OTLP_TRACES_URL' "$bundle" "OTLP traces variable changed"
require_literal 'T3CODE_OTLP_METRICS_URL' "$bundle" "OTLP metrics variable changed"

for wrapper_token in t3code.toml T3CODE_CONFIG_PATH config_dir_source config_sync_mode; do
  reject_literal "$wrapper_token" "$bundle" "wrapper token entered upstream bundle: $wrapper_token"
done

printf 'verified t3@%s sha256=%s\n' "$t3_version" "$actual_sha256"
printf 'verified provider envelope, five drivers, settings RPC, scoped auth, Secret handling, orchestration fields, executables, and OTLP variables\n'
