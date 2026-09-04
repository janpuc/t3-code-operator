# syntax=docker/dockerfile:1.7

ARG GO_IMAGE=golang:1.27.1-bookworm@sha256:648f440f42a0958804efb24df176f806f9d353b41f1c0627f666428e40310f6b
ARG NODE_IMAGE=node:24.20.0-bookworm-slim@sha256:ba849c60be29959425b8734d57b8b4b7d56f98edd9504c9af091d5281095a71e
ARG DEBIAN_IMAGE=debian:bookworm-slim@sha256:88200866dfff7ea7f5cbcb6ec7c8a701889efe6fe859fe64d6990e4b07ea4171

FROM ${GO_IMAGE} AS go-builder
ARG TARGETOS=linux
ARG TARGETARCH
ARG T3_VERSION=0.0.38
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
ARG T3_VERSION=0.0.38
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
ARG MISE_VERSION=2026.9.1
RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates curl jq xz-utils unzip && \
    rm -rf /var/lib/apt/lists/*
RUN case "$TARGETARCH" in \
      amd64) mise_arch=x64; mise_sha=c98423c8470d6dc416d9f7036d0646d8ef5ae92ad9186907f8fcc84cbe7db4ea ;; \
      arm64) mise_arch=arm64; mise_sha=0ef0a778eaa8599f3e90a8a0979c9fc3f79922cafb5fa6d39f366d974da33bba ;; \
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

FROM ${DEBIAN_IMAGE} AS operator
ARG VCS_REF=unknown
ARG BUILD_DATE=unknown
LABEL org.opencontainers.image.source="https://github.com/janpuc/t3-code-operator" \
      org.opencontainers.image.title="t3-code-operator" \
      org.opencontainers.image.revision="$VCS_REF" \
      org.opencontainers.image.created="$BUILD_DATE"
RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates tini && \
    rm -rf /var/lib/apt/lists/*
COPY --from=go-builder /out/t3-code-operator /usr/local/bin/t3-code-operator
COPY --from=baseline-builder /usr/local/bin/mise /usr/local/bin/mise
RUN mkdir -p /var/cache/t3-code-operator/mise && \
    chown -R 1000:1000 /var/cache/t3-code-operator
USER 1000:1000
STOPSIGNAL SIGTERM
ENTRYPOINT ["/usr/bin/tini", "--"]
CMD ["/usr/local/bin/t3-code-operator"]

FROM ${DEBIAN_IMAGE} AS smbd
ARG VCS_REF=unknown
ARG BUILD_DATE=unknown
LABEL org.opencontainers.image.source="https://github.com/janpuc/t3-code-operator" \
      org.opencontainers.image.title="t3-code-smbd" \
      org.opencontainers.image.revision="$VCS_REF" \
      org.opencontainers.image.created="$BUILD_DATE"
RUN apt-get update && apt-get install -y --no-install-recommends \
    samba tini && \
    rm -rf /var/lib/apt/lists/* && \
    useradd --uid 1000 --user-group --no-create-home --shell /usr/sbin/nologin node
COPY --from=go-builder /out/t3-smbd /usr/local/bin/t3-smbd
COPY images/runtime/verify-smb-runtime.sh /usr/local/bin/verify-t3-smb-runtime
RUN chmod 0755 /usr/local/bin/verify-t3-smb-runtime && \
    verify-t3-smb-runtime
EXPOSE 1445
STOPSIGNAL SIGTERM
ENTRYPOINT ["/usr/bin/tini", "--"]
CMD ["/usr/local/bin/t3-smbd"]

FROM ${NODE_IMAGE} AS runtime
ARG TARGETARCH
ARG T3_CHANNEL=stable
ARG T3_VERSION=0.0.38
ARG CODEX_VERSION=0.153.2
ARG CLAUDE_VERSION=2.1.260
ARG CURSOR_VERSION=2026.09.02-c22c1a3
ARG GROK_VERSION=1.0.7
ARG OPENCODE_VERSION=1.18.27
ARG GH_VERSION=2.100.0
ARG VCS_REF=unknown
ARG BUILD_DATE=unknown

LABEL org.opencontainers.image.source="https://github.com/janpuc/t3-code-operator" \
      org.opencontainers.image.revision="$VCS_REF" \
      org.opencontainers.image.created="$BUILD_DATE" \
      org.opencontainers.image.version="$T3_CHANNEL-$T3_VERSION" \
      t3code.janpuc.com/t3-version="$T3_VERSION" \
      t3code.janpuc.com/track="$T3_CHANNEL"

RUN apt-get update && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
    ca-certificates coreutils curl git git-lfs gnupg jq openssh-client python3 tini unzip xz-utils && \
    rm -rf /var/lib/apt/lists/*

COPY --from=go-builder /out/t3-coded /usr/local/bin/t3-coded
COPY --from=npm-builder /opt/t3-runtime /opt/t3-runtime
COPY --from=baseline-builder /usr/local/bin/mise /usr/local/bin/mise
COPY --from=baseline-builder /opt/mise /opt/mise
COPY --from=baseline-builder /opt/t3-code-baseline /opt/t3-code-baseline

RUN case "$TARGETARCH" in \
      amd64) cursor_arch=x64; cursor_sha=b73b59854762535c0fc20d7ccc51c3b5a356a851491088d60a362be48750f53c; grok_arch=x86_64; grok_sha=4ebb2a74a616f6b136fb50faa9794c715bfb14e4691c98e2a3c1109b4eb02623 ;; \
      arm64) cursor_arch=arm64; cursor_sha=fb7bc635be6172ebcf68f907fd9217e3614da51916455c6d7fdb66690997884c; grok_arch=aarch64; grok_sha=1bff7a2ed02871a20442d1c71fcf1e89ac800e75a0dcd1dd8e73fb725ac1dc00 ;; \
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
RUN chmod 0755 /usr/local/bin/verify-t3-runtime
USER node
RUN verify-t3-runtime "$T3_VERSION" "$CODEX_VERSION" "$CLAUDE_VERSION" "$CURSOR_VERSION" "$GROK_VERSION" "$OPENCODE_VERSION" "$GH_VERSION"

WORKDIR /workspace
EXPOSE 3773
STOPSIGNAL SIGTERM
ENTRYPOINT ["/usr/bin/tini", "--"]
CMD ["t3", "start", "--mode", "web", "--host", "0.0.0.0", "--port", "3773", "--base-dir", "/data/t3", "--no-browser", "/workspace"]
