#!/bin/sh
set -eu

expected_t3_version=$1
expected_codex_version=$2
expected_claude_version=$3
expected_cursor_version=$4
expected_grok_version=$5
expected_opencode_version=$6
expected_gh_version=$7

test "$(node -p 'require("/opt/t3-runtime/node_modules/t3/package.json").version')" = "$expected_t3_version"
codex --version | grep -F "codex-cli $expected_codex_version" >/dev/null
claude --version | grep -F "$expected_claude_version" >/dev/null
cursor-agent --version | grep -F "$expected_cursor_version" >/dev/null
grok --version | grep -F "grok $expected_grok_version" >/dev/null
test "$(opencode --version)" = "$expected_opencode_version"
gh --version | grep -F "gh version $expected_gh_version" >/dev/null
test "$(stat -c %s /opt/t3-runtime/node_modules/@anthropic-ai/claude-code/bin/claude.exe)" -gt 4096
node -e 'require("/opt/t3-runtime/node_modules/node-pty"); require("/opt/t3-runtime/node_modules/msgpackr-extract")'
test -x /usr/local/bin/t3-smbd
test -x /usr/sbin/smbd
test -x /usr/bin/smbpasswd
test -x /usr/bin/net

probe_root=$(mktemp -d)
probe_pid=
cleanup() {
	if [ -n "$probe_pid" ]; then
		kill "$probe_pid" 2>/dev/null || true
		wait "$probe_pid" 2>/dev/null || true
	fi
	rm -rf "$probe_root"
}
trap cleanup EXIT INT TERM

mkdir -p "$probe_root/home" "$probe_root/t3" "$probe_root/workspace"
github_config_directory="$probe_root/gh"
github_contract_token=fixture-token
mkdir -p "$github_config_directory"
printf 'version: 1\n' >"$github_config_directory/config.yml"
printf '%s\n' \
	'github.com:' \
	'  git_protocol: https' \
	'  user: fixture-user' \
	"  oauth_token: $github_contract_token" \
	'  users:' \
	'    fixture-user:' \
	"      oauth_token: $github_contract_token" \
	>"$github_config_directory/hosts.yml"
test "$(GH_CONFIG_DIR="$github_config_directory" GH_TOKEN= GITHUB_TOKEN= gh auth token --hostname github.com)" = "$github_contract_token"
printf 'protocol=https\nhost=github.com\n\n' |
	GH_CONFIG_DIR="$github_config_directory" GH_TOKEN= GITHUB_TOKEN= gh auth git-credential get \
		>"$probe_root/gh-credential"
grep -Fx 'username=fixture-user' "$probe_root/gh-credential" >/dev/null
grep -Fx "password=$github_contract_token" "$probe_root/gh-credential" >/dev/null
HOME="$probe_root/home" T3CODE_HOME="$probe_root/t3" timeout --signal=TERM --kill-after=5s 40s \
	t3 start --mode web --host 127.0.0.1 --port 3774 --base-dir "$probe_root/t3" --no-browser "$probe_root/workspace" \
	>"$probe_root/t3.log" 2>&1 &
probe_pid=$!

ready=false
attempt=0
while [ "$attempt" -lt 60 ]; do
	if curl -fsS --max-time 1 http://127.0.0.1:3774/ >/dev/null 2>&1; then
		ready=true
		break
	fi
	if ! kill -0 "$probe_pid" 2>/dev/null; then
		break
	fi
	attempt=$((attempt + 1))
	sleep 0.5
done
test "$ready" = true
