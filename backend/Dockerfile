# Helix Foundry on one machine: the foundry binary (cmd/foundry) and its
# configuration. Built from the Go module's root:
#
#   docker build -f Dockerfile -t helix-foundry/foundry:local .
#
# for this machine, or for both architectures:
#
#   docker buildx build --platform linux/amd64,linux/arm64 -f Dockerfile .
#
# Releases are ghcr.io/helixdb/foundry.

# --platform=$BUILDPLATFORM keeps the toolchain native and cross-compiles.
FROM --platform=$BUILDPLATFORM golang:1.26-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
# No defaults: BuildKit sets them from --platform.
ARG TARGETOS
ARG TARGETARCH
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w" -o /out/foundry ./cmd/foundry

FROM debian:bookworm-slim
# Foundry runs as 65532, and the compose file runs the ontology agent as the
# same user: it reads the owner-only files Foundry writes to /work. A new
# /work volume takes its owner from here. curl serves the health check.
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates curl \
    && rm -rf /var/lib/apt/lists/* \
    && groupadd --system --gid 65532 foundry \
    && useradd --system --uid 65532 --gid 65532 --create-home --shell /bin/false foundry \
    && mkdir /work && chown foundry:foundry /work
USER 65532:65532
WORKDIR /home/foundry
COPY --from=build /out/foundry /usr/local/bin/foundry
COPY cmd/foundry/foundry.yaml /etc/foundry/foundry.yaml
# The site calls REST on 8081; health and gRPC listen on loopback.
EXPOSE 8081
HEALTHCHECK --interval=5s --timeout=3s --start-period=10s --retries=60 \
    CMD curl -fsS http://127.0.0.1:8083/readyz || exit 1
ENTRYPOINT ["/usr/local/bin/foundry", "--config", "/etc/foundry/foundry.yaml"]
