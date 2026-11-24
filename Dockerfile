# syntax=docker/dockerfile:1

# Multi-Exit VLESS Gateway image.
#
# Stages:
#   1. web    - builds the React admin UI (embedded into the Go binary)
#   2. build  - builds the static Go binary (CGO disabled, modernc sqlite)
#   3. singbox - downloads the sing-box release for the target architecture
#   4. runtime - minimal Debian with sing-box, runs as an unprivileged user
#
# The runtime is Debian rather than Alpine because the sing-box binaries are
# linked against glibc and cannot run on musl.
#
# The engine is sing-box-lx, a sing-box 1.14.4 fork with the with_xhttp build
# tag: upstream sing-box has no XHTTP transport, while Xray subscriptions use
# it. Override SING_BOX_REPO/SING_BOX_VERSION to pin another build.
#
# sing-box is the transport engine and is distributed under GPLv3; see README
# for the licensing implications of redistributing this image.

ARG NODE_VERSION=22
ARG GO_VERSION=1.25
ARG SING_BOX_REPO=Leadaxe/sing-box-lx
ARG SING_BOX_VERSION=1.14.4-lx.36

FROM node:${NODE_VERSION}-alpine AS web
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN npm ci --no-audit --no-fund
COPY web/ ./
RUN npm run build

FROM golang:${GO_VERSION}-alpine AS build
ARG VERSION=dev
RUN apk add --no-cache git ca-certificates
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# The UI is generated in the web stage; never trust a stale local build.
COPY --from=web /src/internal/web/dist ./internal/web/dist
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath \
        -ldflags "-s -w -X main.version=${VERSION}" \
        -o /out/multivless ./cmd/multivless

FROM alpine:3.21 AS singbox
ARG TARGETARCH
ARG SING_BOX_REPO
ARG SING_BOX_VERSION
RUN apk add --no-cache curl ca-certificates tar \
    && base="https://github.com/${SING_BOX_REPO}/releases/download/v${SING_BOX_VERSION}" \
    && pkg="sing-box-${SING_BOX_VERSION}-linux-${TARGETARCH}.tar.gz" \
    && curl -fsSL -o "/tmp/${pkg}" "${base}/${pkg}" \
    && curl -fsSL -o /tmp/SHA256SUMS "${base}/SHA256SUMS" \
    && (cd /tmp && grep " ${pkg}$" SHA256SUMS | sha256sum -c -) \
    && tar -xzf "/tmp/${pkg}" -C /tmp \
    && install -Dm 0755 "/tmp/sing-box-${SING_BOX_VERSION}-linux-${TARGETARCH}/sing-box" /out/sing-box \
    && rm -rf /tmp/sing-box* /tmp/SHA256SUMS

FROM debian:12-slim AS runtime

RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates tzdata \
    && rm -rf /var/lib/apt/lists/* \
    && groupadd -g 10001 multivless \
    && useradd -u 10001 -g multivless -M -d /var/lib/multivless -s /usr/sbin/nologin multivless \
    && mkdir -p /etc/multivless /var/lib/multivless /var/log/multivless \
    && chown -R multivless:multivless /var/lib/multivless /var/log/multivless

COPY --from=singbox /out/sing-box /usr/local/bin/sing-box
COPY --from=build /out/multivless /usr/local/bin/multivless
COPY deploy/docker/config.yaml /etc/multivless/config.yaml

USER multivless
WORKDIR /var/lib/multivless

# 1080/tcp  - SOCKS5 entry point
# 8080/tcp  - REST API + web UI
EXPOSE 1080 8080

HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD ["multivless", "healthcheck", "--config", "/etc/multivless/config.yaml"]

ENTRYPOINT ["multivless"]
CMD ["run", "--config", "/etc/multivless/config.yaml"]
