# Phase 2 renderer protocol

Verified 2026-08-27 against `t3@0.0.34`, Codex CLI 0.149.0, and the current
home-ops configuration.

## Boundary

`internal/render.Render` accepts one resolved Workstation graph. It returns one
`RenderedWorkstation` manifest.

The renderer has no Kubernetes client. It performs no network or filesystem
I/O.

The protocol version is `t3code.janpuc.com/rendered/v1alpha1`.

The manifest contains:

- the fixed upstream provider update-check policy;
- the complete upstream `providerInstances` map;
- explicit file targets, write modes, owned paths, and apply policies;
- pinned Extension sources and adapter-selected activation actions;
- pinned mise backends, versions, platform URLs, and SHA-256 values;
- Secret references and environment variable names;
- deterministic warnings and a SHA-256 desired revision.

Runtime tools use an idle-only apply policy.
The renderer accepts only fully lockable mise backends for runtime additions.

The renderer rejects a manifest larger than 900 KiB. This leaves space for the
ConfigMap envelope below Kubernetes' object limit.

## Provider adapters

| Driver | Support | Managed state | MCP target |
|---|---|---|---|
| `codex` | Supported | `/data/harnesses/<id>/codex` | `config.toml`, `Merge` |
| `claudeAgent` | Supported | `/data/harnesses/<id>/claude` | `.claude.json`, `Merge` |
| `opencode` | Supported | `/data/harnesses/<id>/opencode` | `opencode.jsonc`, `Merge` |
| `cursor` | Alpha | Upstream provider config only | No unverified file dialect |
| `grok` | Alpha | Upstream provider config only | No unverified file dialect |

Codex and Claude can have multiple instances. The renderer rejects multiple
Cursor, Grok, or OpenCode instances until their state isolation has live proof.

Codex and Claude receive an adapter-managed `homePath`. A conflicting user path
is an error.

OpenCode receives managed XDG paths and `OPENCODE_CONFIG`. These paths keep its
config, skills, data, and state on retained `/data`.

### File overlays

The opaque config for Codex, Claude, and OpenCode can contain an adapter-owned
`file` object. The adapter removes this object before it renders upstream t3
settings.

The adapter merges the object into the harness config file. MCP-owned roots are
reserved and must come from `MCPServer` attachments.

This seam keeps OpenCode models and plugins out of the upstream provider schema.
It also preserves unknown upstream provider settings.

## MCP dialects

Remote HTTP and local stdio transports are implemented.

HTTP Secret headers use the proven dialect for each harness:

- Claude emits `${VAR}`.
- Codex emits `bearer_token_env_var` or `env_http_headers`.
- OpenCode emits `{env:VAR}`.

The renderer derives collision-resistant names for HTTP header bindings.

Codex stdio uses `env_vars` to forward Secret variables. Codex has no target-key
remapping field. The renderer therefore preserves the requested variable name.
It deduplicates equal bindings and rejects conflicting bindings. The
[official Codex MCP documentation](https://developers.openai.com/codex/mcp/)
defines `env` and `env_vars` for stdio servers.

Unknown non-sensitive transport fields survive dialect rendering. Adapter-owned
fields are rejected in opaque config.

## Secret rules

The rendered manifest never contains a resolved Secret value.

The renderer:

- accepts only same-namespace Secret references;
- rejects inline values for known credential fields;
- rejects inline values for sensitive environment variable names;
- rejects inline values for sensitive HTTP headers;
- allows harness-native `${VAR}` and `{env:VAR}` references in file overlays;
- emits `Sensitive=true` for every Secret-backed provider variable.

The sidecar will resolve values in memory. It must not write those values to a
file, ConfigMap, status, event, command argument, or log.

## Extensions

The renderer validates Git commits, OCI digests, Marketplace commits, and
GitHub release checksums.

Git and OCI skill content receives explicit `Replace` destinations under each
adapter's skill root. Destination collisions are errors.

Marketplace and release sources receive structured installer actions. The
sidecar will execute these actions against the pinned cached source. It will not
select an installer.

OpenCode release bundles remain unsupported. The renderer emits an
`UnsupportedExtensionSource` warning and keeps other activations usable.

Cursor and Grok emit `AlphaDialect`. They do not receive unverified MCP files or
Extension activations.

## Safe apply

Provider settings, managed config files, and Extension changes use an `Idle`
apply point. The sidecar must let active work finish before it applies them.

The renderer sets `enableProviderUpdateChecks` to `false`. Manifest validation
rejects any attempt to enable it.

The manifest selects one reload mechanism for each adapter:

- Codex skills use its watcher.
- Claude and OpenCode changes apply to the next session.
- provider environment or config changes rebuild the provider after it is idle.

No renderer output changes the runtime pod template.

## Tests

Run:

```text
go test ./internal/render
```

Coverage includes:

- exact goldens for all five built-in adapters;
- seven remote proxy MCP servers and two local MCP servers;
- four configured provider instances, including two Claude instances;
- Memini and Komment plugin paths;
- Git skill activation for Codex, Claude, and OpenCode;
- Secret rejection and environment indirection;
- deterministic ordering and numeric canonicalization;
- destination collisions, managed path escapes, and output size bounds.
