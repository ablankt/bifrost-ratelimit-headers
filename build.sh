#!/usr/bin/env bash
# Builds the plugin for every Linux target a dynamically linked Bifrost v2.3.0 can run on.
#
# A Go plugin only loads into a host built with the same toolchain, -trimpath setting, libc and
# dependency versions, so each build runs in the image Bifrost's own release pipeline uses:
#   musl  - golang:1.27.0-alpine3.24 (transports/Dockerfile, Makefile DYNAMIC=1, docs "Option A")
#   glibc - golang:1.27.0-bookworm   (docs "Option B" Debian Dockerfile)
# go.mod/go.sum are Bifrost transports/v2.3.0's own, so every shared module resolves to the
# version linked into the host binary. A plugin with its own go.mod targets another Bifrost build:
# it is built as that module, with the images and targets its build.env sets.
#
# Usage: ./build.sh <plugin> [version]   e.g. ./build.sh api-key-mapper v0.1.0
#        (builds ./plugins/<plugin>; artifacts land in dist/)
set -euo pipefail

NAME="${1:?usage: ./build.sh <plugin> [version]; plugins: $(ls "$(dirname "$0")/plugins" | tr '\n' ' ')}"
VERSION="${2:-v0.1.0}"
ROOT="$(cd "$(dirname "$0")" && pwd)"
DIST="$ROOT/dist"
[ -d "$ROOT/plugins/$NAME" ] || { echo "no such plugin: plugins/$NAME" >&2; exit 1; }

MUSL_IMAGE="golang:1.27.0-alpine3.24@sha256:4c9fe60190a2a3350ddc51de80d0224b8a6698d12bdfc999fee45ea9d6c46dbc"
GLIBC_IMAGE="golang:1.27.0-bookworm@sha256:ded31c68586d2e49e760acc2e65a884b23d032e9bbbed0ae0c55abd3fcaf4452"

WORKDIR=/src
PKG="./plugins/$NAME"
if [ -f "$ROOT/plugins/$NAME/go.mod" ]; then
  WORKDIR="/src/plugins/$NAME"
  PKG=.
fi
if [ -f "$ROOT/plugins/$NAME/build.env" ]; then
  . "$ROOT/plugins/$NAME/build.env"
fi

TARGETS=(${TARGETS:-amd64/musl arm64/musl amd64/glibc arm64/glibc})

mkdir -p "$DIST"

for target in "${TARGETS[@]}"; do
  arch="${target%/*}"
  libc="${target#*/}"
  out="$NAME-linux-$arch-$libc.so"
  case "$libc" in
    musl)  image="$MUSL_IMAGE";  deps="apk add --no-cache gcc musl-dev >/dev/null" ;;
    glibc) image="$GLIBC_IMAGE"; deps="true" ;; # gcc and libc6-dev ship in the bookworm image
    *) echo "unknown libc: $libc" >&2; exit 1 ;;
  esac

  echo "==> $out"
  docker run --rm \
    --platform "linux/$arch" \
    -v "$ROOT:/src:ro" \
    -v "$DIST:/dist" \
    -v bifrost-plugin-gomod:/go/pkg/mod \
    -v "bifrost-plugin-gocache-$arch-$libc:/root/.cache/go-build" \
    -w "$WORKDIR" \
    -e CGO_ENABLED=1 -e GOOS=linux -e GOARCH="$arch" -e GOWORK=off -e GOFLAGS=-mod=readonly \
    "$image" \
    sh -c "$deps && go build -buildmode=plugin -trimpath \
             -ldflags='-X main.Version=$VERSION' \
             -o /dist/$out $PKG && go version -m /dist/$out | head -3"
done

(cd "$DIST" && shasum -a 256 ./"$NAME"-*.so | sed 's# \./# #' > "$NAME-SHA256SUMS" && cat "$NAME-SHA256SUMS")
