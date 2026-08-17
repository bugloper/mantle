#!/usr/bin/env bash
#
# Build and publish the Mantle container images to Docker Hub.
#
#   contrib/release.sh v0.1.0          build and push
#   contrib/release.sh --dry-run       build, verify, push nothing
#
# Two images come out of one Dockerfile, because §14.3 wants the registry and
# the web interface versioned independently:
#
#   <namespace>/mantle      the registry daemon    (target: registry)
#   <namespace>/mantle-ui   the web interface      (target: ui)
#
# Both are built for linux/amd64 and linux/arm64 and pushed as a manifest list,
# so `docker pull` picks the right one on a Graviton box, a Pi, or a laptop
# without anybody choosing a tag by architecture.
#
# Publishing is public and immediate. Nothing here pushes without asking first.

set -euo pipefail

NAMESPACE="${MANTLE_NAMESPACE:-bugloper}"
PLATFORMS="${MANTLE_PLATFORMS:-linux/amd64,linux/arm64}"
BUILDER="${MANTLE_BUILDER:-mantle-release}"

REGISTRY_IMAGE="$NAMESPACE/mantle"
UI_IMAGE="$NAMESPACE/mantle-ui"

DRY_RUN=false
ASSUME_YES=false
ALLOW_DIRTY=false
VERSION=""

cd "$(dirname "$0")/.."

# --- output helpers -------------------------------------------------------

if [ -t 1 ]; then
  bold=$'\033[1m'; dim=$'\033[2m'; red=$'\033[31m'; green=$'\033[32m'; reset=$'\033[0m'
else
  bold=""; dim=""; red=""; green=""; reset=""
fi

say()  { printf '  %s\n' "$*"; }
ok()   { printf '  %s✓%s %s\n' "$green" "$reset" "$*"; }
step() { printf '\n%s%s%s\n' "$bold" "$*" "$reset"; }
die()  { printf '\n  %s✗ %s%s\n\n' "$red" "$*" "$reset" >&2; exit 1; }

usage() {
  cat <<'EOF'
Build and publish the Mantle images to Docker Hub.

Usage:
  contrib/release.sh [version] [flags]

Arguments:
  version              Version to publish, e.g. v0.1.0. Defaults to the git tag
                       pointing at HEAD.

Flags:
  --dry-run            Build and verify without pushing anything.
  --yes                Do not ask for confirmation before pushing.
  --allow-dirty        Permit a release from a dirty working tree (testing only).
  -h, --help           Show this help.

Environment:
  MANTLE_NAMESPACE     Docker Hub namespace          (default: bugloper)
  MANTLE_PLATFORMS     Comma-separated platforms     (default: linux/amd64,linux/arm64)
  MANTLE_BUILDER       buildx builder name           (default: mantle-release)

Examples:
  contrib/release.sh --dry-run
  contrib/release.sh v0.1.0
  MANTLE_NAMESPACE=acme contrib/release.sh v0.1.0
EOF
}

while [ $# -gt 0 ]; do
  case "$1" in
    --dry-run)     DRY_RUN=true ;;
    --yes|-y)      ASSUME_YES=true ;;
    --allow-dirty) ALLOW_DIRTY=true ;;
    -h|--help)     usage; exit 0 ;;
    -*)            die "unknown flag: $1 (try --help)" ;;
    *)
      [ -z "$VERSION" ] || die "version given twice: $VERSION and $1"
      VERSION="$1"
      ;;
  esac
  shift
done

# --- what are we releasing? -----------------------------------------------

step "Release"

if [ -z "$VERSION" ]; then
  # An exact tag, not `git describe`: a release published from an untagged
  # commit cannot be found again from the version string it reports, which
  # makes "which commit is production running?" unanswerable — the one
  # question this whole project exists to answer.
  VERSION="$(git describe --tags --exact-match 2>/dev/null || true)"

  # A dry run is what you do *before* tagging, to find out whether the build
  # works at all. Demanding a tag first would make it useless for that.
  if [ -z "$VERSION" ] && [ "$DRY_RUN" = true ]; then
    VERSION="0.0.0-dryrun"
    say "${dim}HEAD is not tagged — using $VERSION for this dry run${reset}"
  fi

  [ -n "$VERSION" ] || die "HEAD is not tagged, and no version was given.
    Tag the commit first:  git tag -a v0.1.0 -m 'v0.1.0'
    Or pass one:           contrib/release.sh v0.1.0 --allow-dirty"
fi

# Reject anything that is not a plain semantic version, with an optional
# pre-release suffix. A tag like "latest" or "release-final-2" published as an
# immutable version is a mess nobody can unwind afterwards.
if ! printf '%s' "$VERSION" | grep -Eq '^v?[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$'; then
  die "'$VERSION' is not a semantic version (expected v1.2.3 or v1.2.3-rc1)"
fi

# `latest` moves, so it may only ever point at a stable release. Someone who
# pulls :latest and gets a release candidate has been handed a surprise they
# did not ask for.
IS_PRERELEASE=false
case "$VERSION" in *-*) IS_PRERELEASE=true ;; esac

COMMIT="$(git rev-parse HEAD)"
DIRTY="$(git status --porcelain)"

if [ -n "$DIRTY" ]; then
  if [ "$ALLOW_DIRTY" = true ]; then
    say "${dim}working tree is dirty — proceeding because --allow-dirty${reset}"
  else
    die "the working tree is dirty.
    A published image must correspond to a commit anyone can check out.
    Commit or stash first, or pass --allow-dirty for a throwaway build."
  fi
fi

say "version    $bold$VERSION$reset"
say "commit     $COMMIT"
say "platforms  $PLATFORMS"
say "images     $REGISTRY_IMAGE:$VERSION"
say "           $UI_IMAGE:$VERSION"
if [ "$IS_PRERELEASE" = true ]; then
  say "           ${dim}(pre-release: 'latest' will not be moved)${reset}"
else
  say "           ${dim}also tagged :latest${reset}"
fi

# --- preflight ------------------------------------------------------------

step "Preflight"

command -v docker >/dev/null 2>&1 || die "docker is not installed"
docker buildx version >/dev/null 2>&1 || die "docker buildx is not available.
    It ships with Docker Desktop and recent Docker Engine; without it there is
    no way to produce a multi-architecture image."
ok "docker and buildx present"

# A builder using the docker-container driver is required for multi-platform
# output. The default "docker" driver builds one architecture and cannot
# assemble a manifest list, and its failure message does not say so.
if ! docker buildx inspect "$BUILDER" >/dev/null 2>&1; then
  say "creating buildx builder '$BUILDER'"
  docker buildx create --name "$BUILDER" --driver docker-container --bootstrap >/dev/null
fi
docker buildx use "$BUILDER"
ok "buildx builder '$BUILDER' ready"

if [ "$DRY_RUN" = false ]; then
  # Docker has no command that answers "am I logged in", so this reads the
  # config directly. It is a warning rather than a hard failure: credentials
  # can come from a helper this check cannot see.
  if ! grep -q 'index.docker.io' "${DOCKER_CONFIG:-$HOME/.docker}/config.json" 2>/dev/null; then
    say "${dim}no Docker Hub credentials found in the docker config${reset}"
    say "${dim}if the push fails, run: docker login${reset}"
  else
    ok "Docker Hub credentials present"
  fi
fi

# --- confirm --------------------------------------------------------------

if [ "$DRY_RUN" = false ] && [ "$ASSUME_YES" = false ]; then
  step "Confirm"
  say "This publishes ${bold}public${reset} images to Docker Hub under '$NAMESPACE'."
  say "A published tag is visible immediately and may already be pulled by others."
  printf '\n  Type the version to continue: '
  read -r reply
  [ "$reply" = "$VERSION" ] || die "got '$reply', expected '$VERSION' — nothing was pushed"
fi

# --- build ----------------------------------------------------------------

# build <target> <image>
build() {
  target="$1"
  image="$2"

  set -- \
    --file Dockerfile \
    --target "$target" \
    --platform "$PLATFORMS" \
    --build-arg "VERSION=$VERSION" \
    --tag "$image:$VERSION" \
    --label "org.opencontainers.image.revision=$COMMIT" \
    --label "org.opencontainers.image.version=$VERSION" \
    --label "org.opencontainers.image.source=https://github.com/bugloper/mantle" \
    --label "org.opencontainers.image.licenses=Apache-2.0"

  if [ "$IS_PRERELEASE" = false ]; then
    set -- "$@" --tag "$image:latest"
  fi

  if [ "$DRY_RUN" = true ]; then
    # No --push and no --load: buildx still builds every platform and reports
    # any failure, it simply discards the result. --load cannot be used here
    # because the local image store holds one architecture, not a manifest list.
    say "building $image:$VERSION ${dim}(not pushed)${reset}"
    docker buildx build "$@" .
  else
    say "building and pushing $image:$VERSION"
    docker buildx build "$@" --push .
  fi
}

step "Build — registry"
build registry "$REGISTRY_IMAGE"
ok "$REGISTRY_IMAGE"

step "Build — web interface"
build ui "$UI_IMAGE"
ok "$UI_IMAGE"

# --- verify ---------------------------------------------------------------

step "Verify"

# The stamped version is the thing most likely to be silently wrong: a missing
# --build-arg produces a working image that reports "dev", and nobody notices
# until someone asks what is running in production. Building for the host
# architecture alone is enough to check it, and it is nearly free after the
# multi-platform build has warmed the cache.
host_platform="linux/$(docker version --format '{{.Server.Arch}}' 2>/dev/null || echo amd64)"
docker buildx build \
  --file Dockerfile --target registry \
  --platform "$host_platform" \
  --build-arg "VERSION=$VERSION" \
  --tag "$REGISTRY_IMAGE:$VERSION-verify" \
  --load . >/dev/null

stamped="$(docker run --rm "$REGISTRY_IMAGE:$VERSION-verify" --version 2>/dev/null || true)"
docker image rm "$REGISTRY_IMAGE:$VERSION-verify" >/dev/null 2>&1 || true

case "$stamped" in
  *"$VERSION"*) ok "image reports: $stamped" ;;
  "")           die "the image did not report a version — check the entrypoint" ;;
  *)            die "image reports '$stamped', expected '$VERSION'" ;;
esac

if [ "$DRY_RUN" = false ]; then
  # Confirm the manifest list actually carries every requested platform. A
  # single-platform push succeeds quietly and is only noticed by the person
  # whose architecture is missing.
  for image in "$REGISTRY_IMAGE" "$UI_IMAGE"; do
    say "$image:$VERSION"
    docker buildx imagetools inspect "$image:$VERSION" \
      --format '{{range .Manifest.Manifests}}    {{.Platform.OS}}/{{.Platform.Architecture}}
{{end}}' 2>/dev/null || say "    (could not inspect the manifest)"
  done
fi

# --- done -----------------------------------------------------------------

if [ "$DRY_RUN" = true ]; then
  step "Dry run complete — nothing was pushed"
  say "Re-run without --dry-run to publish."
  exit 0
fi

step "Published"
cat <<EOF

  docker pull $REGISTRY_IMAGE:$VERSION
  docker pull $UI_IMAGE:$VERSION

  Self-hosting from here needs a PostgreSQL and one command; docs/docker.md
  has the compose file and the bootstrap step.

  Remember to push the tag if you have not already:
    git push origin $VERSION

EOF
