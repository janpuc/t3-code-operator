# Providers

A provider is one upstream t3 provider instance. Declare it inline under
`Workstation.spec.providers`, or as a `Harness` when several Workstations share
it. Every provider is opt-in through an explicit `enabled` value.

| Key | Upstream driver | Support | Credential |
|---|---|---|---|
| `codex` | `codex` | Supported | ChatGPT device login inside the pod, or an API key in `environment` |
| `claude` | `claudeAgent` | Supported | `CLAUDE_CODE_OAUTH_TOKEN` in `environment` |
| `opencode` | `opencode` | Supported | Whatever the configured provider needs, usually a gateway key in `environment` |
| `cursor` | `cursor` | Alpha | Cursor login inside the pod |
| `grok` | `grok` | Alpha | Grok login inside the pod |
| `antigravity` | `antigravity` | Alpha | Google sign-in from the T3 client, or an API key in `config` |

Supported drivers receive rendered MCP servers and Extension activations in
their own dialect. Alpha drivers keep upstream provider settings only; the
operator does not program MCP or Extension files for them until their dialects
have live proof.

## Codex

```yaml
codex:
  enabled: true
  models:
    - gpt-5.6-sol
```

Codex authenticates with a ChatGPT device login that persists on the retained
`/data` claim. `models` becomes `customModels`. The adapter manages `homePath`;
a conflicting value is rejected. Codex receives MCP servers in `config.toml`
and skills, marketplaces, and release bundles through Extensions.

## Claude

```yaml
claude:
  enabled: true
  environment:
    - name: CLAUDE_CODE_OAUTH_TOKEN
      valueFrom:
        secretKeyRef:
          name: t3-code-config
          key: CLAUDE_CODE_OAUTH_TOKEN
  config:
    file:
      effortLevel: xhigh
```

The OAuth token is a sensitive variable: it must come from a Secret and the
rendered manifest carries only the reference. `config.file` merges into the
managed `settings.json`; `mcpServers`, `enabledPlugins`, and
`extraKnownMarketplaces` are adapter-owned and rejected there.

## OpenCode

```yaml
opencode:
  enabled: true
  models:
    - litellm/minimax/MiniMax-M3
  config:
    file:
      provider:
        litellm:
          npm: "@ai-sdk/openai-compatible"
          options:
            baseURL: http://litellm.ai.svc.cluster.local:4000/v1
            apiKey: "{env:LITELLM_API_KEY}"
          models:
            minimax/MiniMax-M3:
              name: MiniMax M3
  environment:
    - name: LITELLM_API_KEY
      valueFrom:
        secretKeyRef:
          name: t3-code-config
          key: LITELLM_API_KEY
```

`config.file` is the OpenCode configuration file. Provider tables, plugins, and
`{env:VAR}` references live there; `mcp` is adapter-owned. The operator keeps
OpenCode's XDG directories on `/data`.

## Cursor and Grok

Both are alpha. Enable them with `enabled: true` and sign in from a terminal in
the Workstation. The runtime image pins `cursor-agent` and `grok`.

## Antigravity

Upstream t3 ships the Antigravity driver in its nightly track first. Point
`spec.image` at a nightly runtime digest to use it; the stable runtime that a
release pins does not include the driver yet, and a provider whose driver the
runtime lacks shows up in the T3 client as unavailable.

```yaml
antigravity:
  enabled: true
```

T3 downloads Google's official ACP runtime into the environment on
**Install Antigravity** and keeps it on the retained `/data` claim; the
operator does not bake the multi-gigabyte runtime into its image. Sign in with
Google from the T3 client, or set `authMethod`, `apiKey`, `gcpProject`, and
`gcpLocation` in `config` for the key-based methods. Each instance owns its
Google profile, so several Antigravity instances can coexist.

## Git identity

```yaml
git:
  userName: Example User
  userEmail: user@example.test
  githubUser: example-user
  credentialSecretRef:
    name: t3-code-config
    key: GH_TOKEN
  signingKeySecretRef:
    name: t3-code-git-signing
    privateKeyKey: id_signing
    publicKeyKey: id_signing.pub
```

The operator writes the managed `.gitconfig` block, the GitHub CLI credential
skeleton, and the signing key pair with `allowed_signers`. It rewraps a signing
key that a secret store flattened onto one line. `githubUser` and
`credentialSecretRef` go together; a signing key requires `userEmail`.

## Attaching MCP servers and Extensions

An `MCPServer` or `Extension` without `harnessRefs` attaches to every provider
in the namespace whose driver can program it. Name providers in `harnessRefs`
to narrow that: a Workstation provider key or a Harness name.

```yaml
apiVersion: t3code.janpuc.com/v1alpha1
kind: MCPServer
metadata:
  name: kubectl
spec:
  config:
    url: http://litellm.ai.svc.cluster.local:4000/mcp/kubectl/mcp
  bearerTokenSecretRef:
    name: t3-code-config
    key: LITELLM_API_KEY
```

`transport` is inferred from `config.url` (`http`) or `config.command`
(`stdio`). `bearerTokenSecretRef` renders an `Authorization: Bearer` header in
each dialect and is mutually exclusive with an explicit `Authorization` header.
