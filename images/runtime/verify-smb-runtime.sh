#!/bin/sh
set -eu

probe_root=$(mktemp -d)
probe_pid=

cleanup() {
	if [ -n "${probe_pid:-}" ]; then
		kill "$probe_pid" 2>/dev/null || true
		wait "$probe_pid" 2>/dev/null || true
	fi
	if [ -n "${probe_root:-}" ] && [ -d "$probe_root" ]; then
		rm -rf -- "$probe_root"
	fi
}
trap cleanup EXIT INT TERM

chmod 0755 "$probe_root"
mkdir -p "$probe_root/workspace"
chown node:node "$probe_root/workspace"
printf %s 'runtime-image-fixture-password' >"$probe_root/password"
chmod 0400 "$probe_root/password"

/usr/local/bin/t3-smbd \
	--username t3 \
	--share-name workspace \
	--server-identity runtime-image-contract \
	--password-file "$probe_root/password" \
	--workspace "$probe_root/workspace" \
	--state-directory "$probe_root/state" \
	--port 1445 \
	--password-poll-interval 100ms \
	>"$probe_root/smb.log" 2>&1 &
probe_pid=$!

ready=false
attempt=0
while [ "$attempt" -lt 60 ]; do
	if python3 -c 'import socket; socket.create_connection(("127.0.0.1", 1445), 0.2).close()' >/dev/null 2>&1; then
		ready=true
		break
	fi
	if ! kill -0 "$probe_pid" 2>/dev/null; then
		break
	fi
	attempt=$((attempt + 1))
	sleep 0.25
done

if [ "$ready" != true ]; then
	cat "$probe_root/smb.log" >&2
	exit 1
fi

/usr/bin/net --configfile="$probe_root/state/smb.conf" getlocalsid | grep -E 'S-1-5-21-[0-9]+-[0-9]+-[0-9]+' >/dev/null
if grep -F 'runtime-image-fixture-password' "$probe_root/smb.log" >/dev/null; then
	printf '%s\n' 'SMB runtime log exposed the fixture password' >&2
	exit 1
fi

kill "$probe_pid"
wait "$probe_pid"
probe_pid=
