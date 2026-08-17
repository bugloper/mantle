# Mantle is one static binary and a database. The image reflects that.
#
# Two final stages, built with --target:
#   registry  (default)  mantled, the registry daemon
#   ui                   mantle-ui, the optional web interface
#
# They are separate images on purpose (§14.3). Baking both into one would tie
# their versions together, and the whole point of mantle-ui being a separate
# artifact is that a registry upgrade never requires a UI upgrade, or the
# reverse.

# --platform=$BUILDPLATFORM pins the toolchain to the machine doing the
# building rather than the architecture being built for. Without it, a
# multi-architecture build runs the Go compiler under QEMU emulation once per
# target — minutes of emulated compilation per arch, for no benefit. CGO is
# already off, so cross-compiling is a matter of setting GOOS and GOARCH.
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build
WORKDIR /src

# Dependencies first, so a source-only change does not re-download the module
# cache on every build.
COPY go.mod go.sum ./
RUN go mod download

COPY . .
ARG VERSION=dev
# Supplied by buildx for each --platform entry.
ARG TARGETOS TARGETARCH
# CGO is off so the results run on a minimal base with no libc.
ENV CGO_ENABLED=0
RUN GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build \
      -ldflags "-s -w -X github.com/mantle-sh/mantle/internal/server.Version=${VERSION} -X main.Version=${VERSION}" \
      -o /out/mantled ./cmd/mantled && \
    GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build \
      -ldflags "-s -w -X github.com/mantle-sh/mantle/internal/server.Version=${VERSION} -X main.Version=${VERSION}" \
      -o /out/mantle ./cmd/mantle && \
    GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build \
      -ldflags "-s -w -X main.Version=${VERSION}" \
      -o /out/mantle-ui ./cmd/mantle-ui

# --- the registry ---------------------------------------------------------

FROM alpine:3.20 AS registry
# ca-certificates for outbound TLS; wget for the compose health check.
RUN apk add --no-cache ca-certificates wget && \
    adduser -D -u 10001 -h /var/lib/mantle mantle && \
    mkdir -p /var/lib/mantle/keys && chown -R mantle:mantle /var/lib/mantle

COPY --from=build /out/mantled /usr/local/bin/mantled
COPY --from=build /out/mantle  /usr/local/bin/mantle

# Never run the registry as root: it writes files derived from client-supplied
# content, and the storage layout is the one place that must not be able to
# escape its directory.
USER mantle
WORKDIR /var/lib/mantle
EXPOSE 5000 9090
ENTRYPOINT ["/usr/local/bin/mantled"]

# --- the web interface ----------------------------------------------------

FROM alpine:3.20 AS ui
RUN apk add --no-cache ca-certificates wget && \
    adduser -D -u 10002 mantleui

# The interface's assets are embedded in the binary, so this image is one file.
# It holds no state, no key, and no database connection.
COPY --from=build /out/mantle-ui /usr/local/bin/mantle-ui

USER mantleui
EXPOSE 5180
ENTRYPOINT ["/usr/local/bin/mantle-ui"]
