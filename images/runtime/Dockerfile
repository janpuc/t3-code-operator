# syntax=docker/dockerfile:1.7

ARG GO_IMAGE=golang:1.26.7-bookworm@sha256:e8c859f5632dcfde7b32d2012b4351728f6437930887c2f6a91ea242459e5514
ARG NODE_IMAGE=node:24.10.0-bookworm-slim@sha256:b8d2197aff9129d16c801a3e3e1b2a873c4946480f5a310f38056df2268c38d9

FROM ${GO_IMAGE} AS go-builder
ARG TARGETOS=linux
ARG TARGETARCH
ARG T3_VERSION=0.0.34
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY api ./api
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 GOOS="$TARGETOS" GOARCH="$TARGETARCH" \
    go build -buildvcs=false -trimpath -ldflags="-s -w -X github.com/janpuc/t3-code-operator/internal/sidecar.UpstreamT3Version=${T3_VERSION}" \
    -o /out/t3-coded ./cmd/t3-coded && \
    CGO_ENABLED=0 GOOS="$TARGETOS" GOARCH="$TARGETARCH" \
    go build -buildvcs=false -trimpath -ldflags="-s -w" \
    -o /out/t3-smbd ./cmd/t3-smbd && \
    CGO_ENABLED=0 GOOS="$TARGETOS" GOARCH="$TARGETARCH" \
    go build -buildvcs=false -trimpath -ldflags="-s -w -X github.com/janpuc/t3-code-operator/internal/sidecar.UpstreamT3Version=${T3_VERSION}" \
    -o /out/t3-code-operator ./cmd/manager

FROM ${NODE_IMAGE} AS npm-builder
ARG T3_CHANNEL=stable
ARG T3_VERSION=0.0.34
WORKDIR /opt/t3-runtime
RUN apt-get update && apt-get install -y --no-install-recommends \
    build-essential ca-certificates curl python3 && \
    rm -rf /var/lib/apt/lists/*
COPY images/runtime/npm/${T3_CHANNEL}/package.json images/runtime/npm/${T3_CHANNEL}/package-lock.json ./
RUN npm ci --ignore-scripts --no-audit --no-fund && \
    npm rebuild node-pty msgpackr-extract --foreground-scripts --ignore-scripts=false && \
    cd node_modules/@anthropic-ai/claude-code && node install.cjs && \
    cd /opt/t3-runtime/node_modules/opencode-ai && node postinstall.mjs && \
    cd /opt/t3-runtime && \
    test "$(node -p 'require("./node_modules/t3/package.json").version')" = "$T3_VERSION" && \
    test "$(stat -c %s node_modules/@anthropic-ai/claude-code/bin/claude.exe)" -gt 4096 && \
    node -e 'require("./node_modules/node-pty"); require("./node_modules/msgpackr-extract")'

FROM ${NODE_IMAGE} AS baseline-builder
ARG TARGETARCH
ARG MISE_VERSION=2026.8.12
RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates curl jq xz-utils unzip && \
    rm -rf /var/lib/apt/lists/*
RUN case "$TARGETARCH" in \
      amd64) mise_arch=x64; mise_sha=f2092b1e67f0abc8803d3be120dd2bc5b656dd99680ba3159f710e149da10d05 ;; \
      arm64) mise_arch=arm64; mise_sha=071e2d16905360fa04762422a2a889692bb3a4d30f27650de50bc1ac0564840b ;; \
      *) echo "unsupported architecture: $TARGETARCH" >&2; exit 1 ;; \
    esac && \
    curl -fsSL --retry 3 -o /usr/local/bin/mise \
      "https://github.com/jdx/mise/releases/download/v${MISE_VERSION}/mise-v${MISE_VERSION}-linux-${mise_arch}" && \
    echo "${mise_sha}  /usr/local/bin/mise" | sha256sum -c - && \
    chmod 0755 /usr/local/bin/mise && \
    mise --version
COPY images/runtime/baseline /opt/t3-code-baseline/source
COPY images/runtime/install-baseline.sh /usr/local/bin/install-t3-baseline
RUN chmod 0755 /usr/local/bin/install-t3-baseline && /usr/local/bin/install-t3-baseline

FROM ${NODE_IMAGE}
ARG TARGETARCH
ARG T3_CHANNEL=stable
ARG T3_VERSION=0.0.34
ARG CODEX_VERSION=0.149.0
ARG CLAUDE_VERSION=2.1.241
ARG CURSOR_VERSION=2026.08.11-e8db854
ARG GROK_VERSION=1.0.5
ARG OPENCODE_VERSION=1.18.21
ARG GH_VERSION=2.98.0
ARG VCS_REF=unknown
ARG BUILD_DATE=unknown

LABEL org.opencontainers.image.source="https://github.com/janpuc/t3-code-operator" \
      org.opencontainers.image.revision="$VCS_REF" \
      org.opencontainers.image.created="$BUILD_DATE" \
      org.opencontainers.image.version="$T3_CHANNEL-$T3_VERSION" \
      t3code.janpuc.com/t3-version="$T3_VERSION" \
      t3code.janpuc.com/track="$T3_CHANNEL"

RUN apt-get update && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
    ca-certificates coreutils curl git git-lfs gnupg jq openssh-client python3 samba tini unzip xz-utils && \
    rm -rf /var/lib/apt/lists/*

COPY --from=go-builder /out/t3-coded /usr/local/bin/t3-coded
COPY --from=go-builder /out/t3-smbd /usr/local/bin/t3-smbd
COPY --from=go-builder /out/t3-code-operator /usr/local/bin/t3-code-operator
COPY --from=npm-builder /opt/t3-runtime /opt/t3-runtime
COPY --from=baseline-builder /usr/local/bin/mise /usr/local/bin/mise
COPY --from=baseline-builder /opt/mise /opt/mise
COPY --from=baseline-builder /opt/t3-code-baseline /opt/t3-code-baseline

RUN case "$TARGETARCH" in \
      amd64) cursor_arch=x64; cursor_sha=bfff4bf6f4e9dd30c1d0ef0a70b6077b074015dd2948e4c50685d53afdcfce5a; grok_arch=x86_64; grok_sha=9ba87444e1819e8f6104adbbf4676a870c204380aa5c3e1c38a926c4ea677238 ;; \
      arm64) cursor_arch=arm64; cursor_sha=ea13f92e295f523a99ce8d8f57d6894d21e5d1e2d030ffad718ccd5955ca2eed; grok_arch=aarch64; grok_sha=1c1fe67d7c35497fb09f44a451f57acc3787add4c9aea2c56f5c7c75dc5ffcf1 ;; \
      *) echo "unsupported architecture: $TARGETARCH" >&2; exit 1 ;; \
    esac && \
    curl -fsSL --retry 3 -o /tmp/cursor.tar.gz \
      "https://downloads.cursor.com/lab/${CURSOR_VERSION}/linux/${cursor_arch}/agent-cli-package.tar.gz" && \
    echo "${cursor_sha}  /tmp/cursor.tar.gz" | sha256sum -c - && \
    mkdir -p "/opt/cursor-agent/${CURSOR_VERSION}" && \
    tar --strip-components=1 -xzf /tmp/cursor.tar.gz -C "/opt/cursor-agent/${CURSOR_VERSION}" && \
    ln -s "/opt/cursor-agent/${CURSOR_VERSION}/cursor-agent" /usr/local/bin/cursor-agent && \
    curl -fsSL --retry 3 -o /usr/local/bin/grok \
      "https://x.ai/cli/grok-${GROK_VERSION}-linux-${grok_arch}" && \
    echo "${grok_sha}  /usr/local/bin/grok" | sha256sum -c - && \
    chmod 0755 /usr/local/bin/grok && \
    rm -f /tmp/cursor.tar.gz && \
    ln -s /opt/t3-runtime/node_modules/.bin/t3 /usr/local/bin/t3 && \
    ln -s /opt/t3-runtime/node_modules/.bin/codex /usr/local/bin/codex && \
    ln -s /opt/t3-runtime/node_modules/.bin/claude /usr/local/bin/claude && \
    ln -s /opt/t3-runtime/node_modules/.bin/opencode /usr/local/bin/opencode && \
    ln -s /opt/t3-runtime/node_modules/.bin/oxfmt /usr/local/bin/oxfmt

ENV HOME=/data/home \
    T3CODE_HOME=/data/t3 \
    XDG_CACHE_HOME=/data/home/.cache \
	XDG_CONFIG_HOME=/data/home/.config \
	XDG_DATA_HOME=/data/home/.local/share \
	GH_CONFIG_DIR=/data/t3-coded/gh \
	GH_HOST=github.com \
    GH_GIT_PROTOCOL=https \
    PATH=/data/t3-coded/tools/current/bin:/opt/t3-code-baseline/bin:/opt/t3-runtime/node_modules/.bin:/usr/local/bin:/usr/bin:/bin

RUN mkdir -p /data/home /data/t3 /data/t3-coded /workspace /var/cache/t3-code-operator/mise && \
    ln -sfn /data/t3-coded/machine-info /etc/machine-info && \
    chown -R node:node /data /workspace /var/cache/t3-code-operator
COPY images/runtime/verify-runtime.sh /usr/local/bin/verify-t3-runtime
COPY images/runtime/verify-smb-runtime.sh /usr/local/bin/verify-t3-smb-runtime
RUN chmod 0755 /usr/local/bin/verify-t3-runtime /usr/local/bin/verify-t3-smb-runtime && \
    verify-t3-smb-runtime
USER node
RUN verify-t3-runtime "$T3_VERSION" "$CODEX_VERSION" "$CLAUDE_VERSION" "$CURSOR_VERSION" "$GROK_VERSION" "$OPENCODE_VERSION" "$GH_VERSION"

WORKDIR /workspace
EXPOSE 3773
STOPSIGNAL SIGTERM
ENTRYPOINT ["/usr/bin/tini", "--"]
CMD ["t3", "start", "--mode", "web", "--host", "0.0.0.0", "--port", "3773", "--base-dir", "/data/t3", "--no-browser", "/workspace"]
