# syntax=docker/dockerfile:1.7

ARG GO_VERSION=1.23
ARG ALPINE_VERSION=3.20

FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-alpine AS builder
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=docker
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" \
    -o /out/proxmoxvue-agent ./cmd/proxmoxvue-agent

FROM alpine:${ALPINE_VERSION}
RUN apk add --no-cache ca-certificates tini \
    && mkdir -p /etc/proxmoxvue-agent /var/log \
    && chmod 0700 /etc/proxmoxvue-agent
COPY --from=builder /out/proxmoxvue-agent /usr/local/bin/proxmoxvue-agent
COPY scripts/docker-entrypoint.sh /usr/local/bin/docker-entrypoint.sh
RUN chmod 0755 /usr/local/bin/docker-entrypoint.sh

VOLUME ["/etc/proxmoxvue-agent"]

# Pin the config-dir + log-path so HEALTHCHECK and entrypoint share one
# fixed location. Operators who override either env var should also
# override HEALTHCHECK at run-time (see docs/DOCKER.md).
ENV PROXMOXVUE_CONFIG_DIR=/etc/proxmoxvue-agent

# Liveness signal: the agent writes an INFO log line every poll
# (default 30s, see scripts/docker-entrypoint.sh). A stale file mtime
# (>2 min) means the poll loop has frozen even though the process is
# still up — exactly what `docker ps` / Uptime Kuma can't see otherwise.
HEALTHCHECK --interval=30s --timeout=5s --start-period=90s --retries=3 \
  CMD test -n "$(find ${PROXMOXVUE_CONFIG_DIR}/agent.log -mmin -2 2>/dev/null)" || exit 1

ENTRYPOINT ["/sbin/tini", "--", "/usr/local/bin/docker-entrypoint.sh"]
