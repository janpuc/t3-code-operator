# Komment integration pain points

The repository removed its Komment policy integration on 2026-08-27.
These issues came from the `v3.2.0` integration:

- The MCP pre-write gate rejected valid Kubebuilder validation, default,
  pruning, and root markers as ordinary comments.
- `koment comments check` accepted the same markers. The pre-write gate and
  repository check therefore gave different results for identical content.
- One rejected API patch produced a denial for every marker. The long output
  hid the first useful failure.
- The repository required stop checks, but the executable was unavailable on
  `PATH` when mise was unavailable. The only working copy was an opaque Go
  build-cache path.

The operator still supports Komment as a normal Extension and MCP integration.
Only the repository policy integration was removed.
