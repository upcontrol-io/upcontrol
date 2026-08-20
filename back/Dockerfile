# Multi-stage build for all three upcontrol services. A single image produces
# ucapi, ucworker and ucprobe; compose/compose selects which runs via `command`.
#
# GOEXPERIMENT=jsonv2 is baked in via ARG and GOFLAGS so it applies to every
# `go build` in the builder (§1). The runtime is a minimal scratch-style image on
# alpine so we keep ca-certificates (HTTPS probes) and timezone data.

# Pinned to the exact patch go.mod requires, not the floating 1.26 tag: the image
# ships GOTOOLCHAIN=local, so a base older than the `go` directive cannot fetch
# the toolchain and the build fails outright (1.26-alpine was 1.26.3 vs go.mod's
# 1.26.5). Bump this line and go.mod together.
#
# --platform=$BUILDPLATFORM: the compile stage always runs on the builder's own
# architecture and CROSS-compiles via GOOS/GOARCH — Go does this natively. The
# alternative (running the Go compiler under QEMU for the arm64 half of a
# multi-arch build) took 40+ minutes per release where this takes ~3.
FROM --platform=$BUILDPLATFORM golang:1.26.5-alpine AS build
ARG GOEXPERIMENT=jsonv2
ARG TARGETOS
ARG TARGETARCH
ENV GOEXPERIMENT=${GOEXPERIMENT} \
    CGO_ENABLED=0 \
    GOFLAGS=-trimpath \
    GOPROXY=https://proxy.golang.org,direct
WORKDIR /src
# Cache deps first: copy only manifests, download, then copy the rest.
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# Build all three binaries into /out, for the TARGET platform (buildx supplies
# TARGETOS/TARGETARCH per platform in a multi-arch build; empty on a plain
# `docker build`, where Go's own defaults are exactly right). Migrations and
# db/ are embedded later; for now the binaries ship standalone.
RUN mkdir -p /out && \
    for svc in ucapi ucworker ucprobe; do \
      GOOS=$TARGETOS GOARCH=$TARGETARCH go build -ldflags "-s -w" -o /out/$svc ./cmd/$svc; \
    done

FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata && \
    addgroup -S uc && adduser -S -G uc uc
# The services read secrets from files (POSTGRES_PASSWORD_FILE etc.), mounted by
# compose under /run/secrets. /spool holds the ingest WAL/spool.
RUN mkdir -p /spool /run/secrets && chown -R uc:uc /spool
COPY --from=build /out/ucapi   /usr/local/bin/ucapi
COPY --from=build /out/ucworker /usr/local/bin/ucworker
COPY --from=build /out/ucprobe  /usr/local/bin/ucprobe
USER uc
# No ENTRYPOINT: compose selects which binary to run via `command`.
# Default to ucapi for bare `docker run`.
CMD ["ucapi"]
EXPOSE 8080
