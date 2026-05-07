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

ENTRYPOINT ["/sbin/tini", "--", "/usr/local/bin/docker-entrypoint.sh"]
