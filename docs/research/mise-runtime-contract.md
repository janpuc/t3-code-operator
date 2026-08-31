# mise runtime contract

Verified on 2026-08-28 with mise `2026.8.12` on Linux x64.

## Scope

This contract applies only to `Workstation.spec.tools`.

Repository-local toolchains remain owned by mise and each repository.
`t3-coded` does not edit repository configuration.

## Upstream facts

- `mise install` installs configured tools without activating them.
- `mise bin-paths --json` returns the active executable names and paths.
- `mise.lock` can pin versions, artifact URLs, and checksums.
- Strict lock mode requires a lock URL only for backends that support URLs.
- `aqua`, `http`, `github`, and `gitlab` have full lockfile support.
- `mise reshim` includes every installed tool, not only active tools.
- An unresolved shim can use a same-named system executable by default.
- `not_found_auto_install=false` disables automatic installation.
- `not_found_system_fallback=false` disables system fallback.
- `MISE_DATA_DIR` contains installs and shims.
- `MISE_CACHE_DIR` contains disposable download data.
- `MISE_GLOBAL_CONFIG_FILE` selects the global configuration file.

Primary references:

- [mise.lock](https://mise.jdx.dev/dev-tools/mise-lock.html)
- [mise shims](https://mise.jdx.dev/dev-tools/shims.html)
- [mise configuration](https://mise.jdx.dev/configuration.html)
- [mise install](https://mise.jdx.dev/cli/install.html)
- [mise bin-paths](https://mise.jdx.dev/cli/bin-paths.html)
- [mise HTTP backend](https://mise.jdx.dev/dev-tools/backends/http.html)

## Executed probes

The probe used isolated data, cache, state, system, and global paths.
It did not read the repository's mise configuration.

Two configurations selected different `path:` versions of one probe tool.
`mise bin-paths --json` returned the correct executable for each version.
An empty configuration returned no executables.

The lock probe used this exact source:

```toml
[tools]
"aqua:BurntSushi/ripgrep" = "14.1.1"
```

`mise lock --platform linux-x64` resolved this artifact:

```text
https://github.com/BurntSushi/ripgrep/releases/download/14.1.1/ripgrep-14.1.1-x86_64-unknown-linux-musl.tar.gz
sha256:4cf9f2741e6c465ffdb7c26f38056a59e2a2544b51f7cc128ef28337eeae4d8e
```

`mise --locked install --jobs 1` installed the artifact.
`mise bin-paths --json` then returned the installed `rg` executable.

The Go integration test generated the configuration and lockfile itself.
It installed the same locked artifact through `MiseRuntime` successfully.

## Runtime design

The operator resolves each backend, version, platform URL, and SHA-256 value.
The renderer places only that resolved, secret-free data in the manifest.

`t3-coded` generates one isolated mise configuration and lockfile per revision.
It runs mise in safe mode with hooks disabled and strict lock mode enabled.

The mise data and cache directories stay on retained `/data`.
Removing a tool does not delete its installed version or download cache.

Mise keys an install by backend and version, not by artifact checksum.
Therefore, each resolved tool-set digest uses an isolated mise data directory.
All tool sets share only the download cache.

A same-version artifact change receives a different install directory.
It cannot overwrite an executable used by the live revision.

`t3-coded` uses `mise bin-paths --json` to enumerate the desired executables.
It creates one link directory for the desired revision.
It rejects duplicate executable names.

The workload PATH contains `/data/t3-coded/tools/current/bin`.
`t3-coded` changes `current` with one atomic symbolic-link replacement.

A failed install does not change `current`.
A rollback restores the prior `current` target.
A removal activates a revision that omits the removed executable.

This design does not use the mise shim directory.
That directory represents all installed tools and cannot express removal.

Repository activation can place repository tool paths before the runtime layer.
Therefore, repository-local versions override Workstation runtime additions.

## MVP backend boundary

Runtime additions accept only backends with full lockfile asset support:

- `aqua`
- `http`
- `github`
- `gitlab`

Other mise backends do not guarantee both a URL and checksum.
The operator must reject them until mise provides that guarantee.

This boundary does not affect the fixed image baseline.
