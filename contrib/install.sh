#!/bin/sh
#
# Install the Mantle CLI from GitHub Releases.
#
#   curl -fsSL https://raw.githubusercontent.com/bugloper/mantle/main/contrib/install.sh | sh
#
# Or, having read it first — which you should, because the pattern above runs
# whatever the server returns:
#
#   curl -fsSLO https://raw.githubusercontent.com/bugloper/mantle/main/contrib/install.sh
#   less install.sh && sh install.sh
#
# This installs `mantle`, the CLI, and nothing else. The registry daemon is
# distributed as a container image — see docs/docker.md — because a daemon needs
# a database and a config file, and dropping one onto a laptop from a pipe would
# be presumptuous.
#
# Environment:
#   MANTLE_VERSION       version to install (default: the newest release)
#   MANTLE_INSTALL_DIR   where to put the binary (default: see below)
#   MANTLE_BASE_URL      override the download host, for mirrors or testing
#
# POSIX sh on purpose: this runs before Mantle is installed, on whatever the
# machine already has, and `sh` is the only shell guaranteed to be there.

set -eu

REPO="${MANTLE_REPO:-bugloper/mantle}"
BASE_URL="${MANTLE_BASE_URL:-}"
VERSION="${MANTLE_VERSION:-}"
INSTALL_DIR="${MANTLE_INSTALL_DIR:-}"

if [ -t 1 ]; then
  bold=$(printf '\033[1m'); dim=$(printf '\033[2m')
  red=$(printf '\033[31m'); green=$(printf '\033[32m'); reset=$(printf '\033[0m')
else
  bold=""; dim=""; red=""; green=""; reset=""
fi

say()  { printf '  %s\n' "$*"; }
ok()   { printf '  %s✓%s %s\n' "$green" "$reset" "$*"; }
die()  { printf '\n  %s✗ %s%s\n\n' "$red" "$*" "$reset" >&2; exit 1; }

TMP=""
cleanup() { [ -n "$TMP" ] && rm -rf "$TMP"; }
trap cleanup EXIT INT TERM

# --- what are we running on? ----------------------------------------------

detect_platform() {
  os=$(uname -s)
  case "$os" in
    Darwin) os=darwin ;;
    Linux)  os=linux ;;
    *) die "unsupported operating system: $os
    Mantle publishes binaries for macOS and Linux. On anything else, build
    from source with 'go build ./cmd/mantle'." ;;
  esac

  arch=$(uname -m)
  case "$arch" in
    x86_64|amd64)  arch=amd64 ;;
    arm64|aarch64) arch=arm64 ;;
    *) die "unsupported architecture: $arch
    Mantle publishes amd64 and arm64. On anything else, build from source." ;;
  esac

  PLATFORM="${os}_${arch}"
}

# --- which version? -------------------------------------------------------

# resolve_version finds the newest release when none was named.
#
# Deliberately not /releases/latest: that endpoint excludes pre-releases and
# 404s when every release so far is one, which is exactly the state a young
# project is in. Listing and taking the first entry gets the newest release
# whether or not it is a pre-release.
resolve_version() {
  [ -n "$VERSION" ] && return 0

  api="https://api.github.com/repos/${REPO}/releases?per_page=1"
  body=$(download_to_stdout "$api") || die "could not reach the GitHub API at $api"

  VERSION=$(printf '%s' "$body" \
    | tr ',' '\n' \
    | grep '"tag_name"' \
    | head -1 \
    | sed -e 's/.*"tag_name"[[:space:]]*:[[:space:]]*"//' -e 's/".*//')

  [ -n "$VERSION" ] || die "no releases found for ${REPO}.
    Nothing has been published yet. Install from source instead:
      git clone https://github.com/${REPO} && cd mantle && make build"
}

# --- fetching -------------------------------------------------------------

# One of curl or wget is present on essentially every machine, but not always
# the same one, so both are supported rather than assumed.
have() { command -v "$1" >/dev/null 2>&1; }

download_to_stdout() {
  if have curl; then curl -fsSL "$1"
  elif have wget; then wget -qO- "$1"
  else die "neither curl nor wget is available"
  fi
}

download_to_file() {
  if have curl; then curl -fsSL -o "$2" "$1"
  elif have wget; then wget -qO "$2" "$1"
  else die "neither curl nor wget is available"
  fi
}

sha256_of() {
  if have sha256sum; then sha256sum "$1" | cut -d' ' -f1
  elif have shasum; then shasum -a 256 "$1" | cut -d' ' -f1
  else echo ""
  fi
}

# --- where does it go? ----------------------------------------------------

# resolve_install_dir prefers a directory already on PATH that needs no
# privilege escalation. A binary installed somewhere not on PATH is a support
# question, and one installed with sudo when it did not need to be is a
# needless risk.
resolve_install_dir() {
  if [ -n "$INSTALL_DIR" ]; then
    NEED_SUDO=false
    [ -d "$INSTALL_DIR" ] || mkdir -p "$INSTALL_DIR" 2>/dev/null || NEED_SUDO=true
    [ -w "$INSTALL_DIR" ] 2>/dev/null || NEED_SUDO=true
    return 0
  fi

  for candidate in "$HOME/.local/bin" "$HOME/bin"; do
    case ":$PATH:" in
      *":$candidate:"*)
        if [ -d "$candidate" ] && [ -w "$candidate" ]; then
          INSTALL_DIR="$candidate"; NEED_SUDO=false; return 0
        fi
        ;;
    esac
  done

  # Nothing suitable on PATH. ~/.local/bin is the modern default and needs no
  # privileges, so create it rather than reaching for sudo and /usr/local/bin.
  INSTALL_DIR="$HOME/.local/bin"
  mkdir -p "$INSTALL_DIR"
  NEED_SUDO=false
}

on_path() {
  case ":$PATH:" in *":$1:"*) return 0 ;; *) return 1 ;; esac
}

# --- go ------------------------------------------------------------------

printf '\n%sInstalling the Mantle CLI%s\n\n' "$bold" "$reset"

detect_platform
ok "platform    $PLATFORM"

resolve_version
ok "version     $VERSION"

[ -n "$BASE_URL" ] || BASE_URL="https://github.com/${REPO}/releases/download/${VERSION}"

ARCHIVE="mantle_${VERSION}_${PLATFORM}.tar.gz"
CHECKSUMS="mantle_${VERSION}_checksums.txt"

TMP=$(mktemp -d)

say "downloading ${dim}${BASE_URL}/${ARCHIVE}${reset}"
download_to_file "${BASE_URL}/${ARCHIVE}" "$TMP/$ARCHIVE" \
  || die "could not download $ARCHIVE
    Check that $VERSION exists and publishes a $PLATFORM build:
      https://github.com/${REPO}/releases"

# Verify before extracting. A tarball from the network is untrusted input, and
# the checksum is the only thing standing between a wrong byte — corrupted,
# truncated, or substituted — and an executable on your PATH.
if download_to_file "${BASE_URL}/${CHECKSUMS}" "$TMP/$CHECKSUMS" 2>/dev/null; then
  want=$(grep " \*\{0,1\}${ARCHIVE}\$" "$TMP/$CHECKSUMS" | head -1 | cut -d' ' -f1)
  got=$(sha256_of "$TMP/$ARCHIVE")
  if [ -z "$want" ]; then
    say "${dim}no checksum listed for $ARCHIVE — skipping verification${reset}"
  elif [ -z "$got" ]; then
    say "${dim}no sha256 tool available — skipping verification${reset}"
  elif [ "$want" != "$got" ]; then
    die "checksum mismatch for $ARCHIVE
    expected $want
    got      $got
    Do not use this download."
  else
    ok "checksum    verified"
  fi
else
  say "${dim}no checksums file published — skipping verification${reset}"
fi

tar -xzf "$TMP/$ARCHIVE" -C "$TMP" || die "could not extract $ARCHIVE"
[ -f "$TMP/mantle" ] || die "the archive did not contain a 'mantle' binary"
chmod +x "$TMP/mantle"

resolve_install_dir

if [ "$NEED_SUDO" = true ]; then
  have sudo || die "$INSTALL_DIR is not writable and sudo is unavailable.
    Choose somewhere you own:  MANTLE_INSTALL_DIR=\$HOME/.local/bin sh install.sh"
  say "installing to $INSTALL_DIR ${dim}(needs sudo)${reset}"
  sudo mkdir -p "$INSTALL_DIR"
  sudo install -m 0755 "$TMP/mantle" "$INSTALL_DIR/mantle"
else
  install -m 0755 "$TMP/mantle" "$INSTALL_DIR/mantle"
fi
ok "installed   $INSTALL_DIR/mantle"

# Run what was just installed. Reporting success without checking that the
# binary executes on this machine is how a broken architecture match gets
# discovered by the user rather than by the installer.
if ! reported=$("$INSTALL_DIR/mantle" version 2>&1); then
  die "the installed binary did not run:
    $reported"
fi
ok "verified    $reported"

printf '\n'
if on_path "$INSTALL_DIR"; then
  say "Run ${bold}mantle --help${reset} to get started."
  say "${dim}If your shell says 'command not found', run 'hash -r' or open a new tab.${reset}"
else
  say "${bold}$INSTALL_DIR is not on your PATH.${reset} Add it:"
  say ""
  say "  echo 'export PATH=\"$INSTALL_DIR:\$PATH\"' >> ~/.zshrc"
  say ""
  say "Then run: mantle --help"
fi
printf '\n'
