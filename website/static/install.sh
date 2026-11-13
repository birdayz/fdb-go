#!/bin/sh
# fdb.dev CLI installer
#
#   curl -fsSL https://fdb.dev/install.sh | sh
#
# Installs `fdb`, the FoundationDB, Record Layer, and SQL CLI, as a single
# static binary (pure Go, CGO_ENABLED=0; the same binary runs on glibc, musl,
# and FROM scratch). Downloads the release archive from GitHub, verifies
# its sha256 against the release's checksums.txt, and installs atomically.
# Never uses sudo.
#
# To build from a current checkout instead, see:
#   https://fdb.dev/docs/cli/#install
#
# The whole script runs from main() at the bottom, so a truncated download
# can't execute a half-script.

set -u

REPO_URL="${FDB_BASE_URL:-https://github.com/birdayz/fdb-go}"
API_URL="${FDB_API_URL:-https://api.github.com/repos/birdayz/fdb-go}"
# fdb is a package of the root module, so a release is the project's tag
# vX.Y.Z, the same tag `go install fdb.dev/cmd/fdb@vX.Y.Z` resolves.

# Default resolved in main() (needs $HOME, which set -u would trip on if unset).
INSTALL_DIR="${FDB_INSTALL_DIR:-}"
VERSION="${FDB_CLI_VERSION:-latest}"
UNINSTALL=0

WORK_DIR=""

# ---- output ---------------------------------------------------------------

# Colors only when stderr is an interactive terminal and nobody opted out.
if [ -t 2 ] && [ "${TERM:-}" != "dumb" ] && [ -z "${NO_COLOR:-}" ]; then
    c_dim=$(printf '\033[2m'); c_green=$(printf '\033[32m')
    c_red=$(printf '\033[31m'); c_bold=$(printf '\033[1m')
    c_reset=$(printf '\033[0m')
else
    c_dim=""; c_green=""; c_red=""; c_bold=""; c_reset=""
fi

info() { printf '%s\n' "${c_dim}$1${c_reset}" >&2; }
ok()   { printf '%s\n' "${c_green}✓${c_reset} $1" >&2; }
die()  { printf '%s\n' "${c_red}error:${c_reset} $1" >&2; exit 1; }

usage() {
    cat >&2 <<EOF
fdb.dev CLI installer: installs the static fdb binary

usage:
  curl -fsSL https://fdb.dev/install.sh | sh
  sh install.sh [options]

options:
  --version <vX.Y.Z>   install a specific version   (env: FDB_CLI_VERSION)
  --dir <path>         install directory            (env: FDB_INSTALL_DIR)
                       default: ~/.local/bin
  --uninstall          remove an installed fdb and exit
  -h, --help           show this help

environment:
  NO_COLOR             disable colored output (https://no-color.org)
  FDB_BASE_URL         release download base, for mirrors/testing
                       default: https://github.com/birdayz/fdb-go
  FDB_API_URL          GitHub API base for "latest" resolution
                       default: https://api.github.com/repos/birdayz/fdb-go

build from a current checkout:
  https://fdb.dev/docs/cli/#install
EOF
}

# ---- helpers --------------------------------------------------------------

have() { command -v "$1" >/dev/null 2>&1; }

cleanup() { [ -n "$WORK_DIR" ] && rm -rf "$WORK_DIR"; }

# fetch <url> <dest-file>. Everything this script downloads lands in a file,
# including the release-list JSON: a to-stdout variant can only be consumed
# through a pipe, and a pipe's exit status hides the fetch's (see
# resolve_version).
fetch() {
    if have curl; then
        curl -fsSL --retry 6 --connect-timeout 15 --speed-limit 1024 --speed-time 60 -o "$2" "$1"
    else
        wget -q -O "$2" "$1"
    fi
}

sha256_of() {
    if have sha256sum; then sha256sum "$1" | cut -d' ' -f1
    elif have shasum;    then shasum -a 256 "$1" | cut -d' ' -f1
    elif have openssl;   then openssl dgst -sha256 "$1" | sed 's/.*= //'
    else die "need sha256sum, shasum, or openssl to verify the download"
    fi
}

# ---- steps ----------------------------------------------------------------

parse_args() {
    while [ $# -gt 0 ]; do
        case "$1" in
            --version)   [ $# -ge 2 ] || die "--version needs a value (e.g. --version v0.1.0)"
                         VERSION="$2"; shift ;;
            --dir|-d)    [ $# -ge 2 ] || die "--dir needs a path"
                         INSTALL_DIR="$2"; shift ;;
            --uninstall) UNINSTALL=1 ;;
            -h|--help)   usage; exit 0 ;;
            *)           usage; die "unknown option: $1" ;;
        esac
        shift
    done
    # Accept vX.Y.Z or X.Y.Z, never a nested-module tag or a path.
    case "$VERSION" in
        latest|v*) ;;
        *) VERSION="v$VERSION" ;;
    esac
    if [ "$VERSION" != latest ]; then
        case "$VERSION" in
            *[!0-9A-Za-z.-]*) die "invalid version: $VERSION (expected vX.Y.Z)" ;;
        esac
        printf '%s\n' "$VERSION" | grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$' ||
            die "invalid version: $VERSION (expected vX.Y.Z)"
    fi
}

detect_platform() {
    case "$(uname -s)" in
        Linux)  OS=linux ;;
        Darwin) OS=darwin ;;
        MINGW*|MSYS*|CYGWIN*|Windows_NT)
            die "FoundationDB has no native Windows client story, so neither does fdb.
       Use WSL and run this installer inside it (the linux binary is static)." ;;
        *)  die "unsupported OS: $(uname -s). Build from source: https://fdb.dev/docs/cli/#install" ;;
    esac
    case "$(uname -m)" in
        x86_64|amd64)  ARCH=amd64 ;;
        arm64|aarch64) ARCH=arm64 ;;
        *)  die "unsupported architecture: $(uname -m). Build from source: https://fdb.dev/docs/cli/#install" ;;
    esac
    # One static binary per OS/arch. No glibc/musl split needed: pure Go,
    # no cgo, so the linux binary runs on Debian, Alpine, NixOS, scratch.
    info "detected ${OS}/${ARCH}"
}

resolve_version() {
    [ "$VERSION" != "latest" ] && return 0

    # Keep API failure, an empty release list, and a list with no matching CLI
    # release distinct. Fetch to a file so a pipeline cannot hide a failed request.
    releases="$WORK_DIR/releases.json"
    if ! fetch "$API_URL/releases?per_page=100" "$releases"; then
        die "could not reach the GitHub release API at $API_URL.
       Offline, behind a proxy, or rate-limited (60 requests/hour unauthenticated)?
       Retry, or skip the lookup by pinning a version:
         FDB_CLI_VERSION=vX.Y.Z sh install.sh
       Releases: $REPO_URL/releases
       Build from a current checkout: https://fdb.dev/docs/cli/#install"
    fi

    # The API returns releases newest-first. Only root vX.Y.Z tags qualify;
    # prerelease-suffixed and nested-module tags are not install candidates.
    tags=$(grep -o '"tag_name"[^,]*' "$releases")
    VERSION=$(printf '%s\n' "$tags" |
        sed -n 's|.*"tag_name"[^"]*"\(v[0-9][0-9]*\.[0-9][0-9]*\.[0-9][0-9]*\)".*|\1|p' |
        head -n1)

    if [ -z "$VERSION" ]; then
        if [ -z "$tags" ]; then
            die "$REPO_URL has published no releases, so there is no binary to install.
       The release list was fetched successfully and is empty -- this is not a
       network or rate-limit problem, and no FDB_CLI_VERSION pin will help.
       Build from a current checkout:
         https://fdb.dev/docs/cli/#install"
        fi
        die "$REPO_URL has releases, but none tagged vX.Y.Z --
       the fdb CLI has not been released from this repository yet.
       Build from a current checkout: https://fdb.dev/docs/cli/#install"
    fi
    info "latest release: fdb $VERSION"
}

# make_workdir runs before resolve_version, which needs somewhere to land the
# release-list response: reading that body is what lets an empty list and a
# failed request be reported as the different things they are.
make_workdir() {
    WORK_DIR=$(mktemp -d "${TMPDIR:-/tmp}/fdb-install.XXXXXX") || die "mktemp failed"
    trap cleanup EXIT
    trap 'exit 130' INT TERM
}

download_and_verify() {
    ASSET="fdb_${VERSION}_${OS}_${ARCH}.tar.gz"
    base="$REPO_URL/releases/download/$VERSION"

    info "downloading $ASSET"
    fetch "$base/$ASSET" "$WORK_DIR/$ASSET" ||
        die "download failed: $base/$ASSET
       Does the fdb release exist for ${OS}/${ARCH}? See $REPO_URL/releases"
    fetch "$base/checksums.txt" "$WORK_DIR/checksums.txt" ||
        die "download failed: checksums.txt (refusing to install an unverified binary)"

    want=$(awk -v a="$ASSET" '$2 == a { print $1 }' "$WORK_DIR/checksums.txt")
    [ -n "$want" ] || die "no entry for $ASSET in checksums.txt"
    got=$(sha256_of "$WORK_DIR/$ASSET")
    [ "$got" = "$want" ] || die "sha256 mismatch for $ASSET
       expected $want
       got      $got
       The download may be corrupted or tampered with. Nothing was installed."
    ok "checksum verified ${c_dim}(sha256 $want)${c_reset}"

    tar -xzf "$WORK_DIR/$ASSET" -C "$WORK_DIR" fdb || die "could not extract fdb from $ASSET"
}

install_binary() {
    mkdir -p "$INSTALL_DIR" 2>/dev/null ||
        die "cannot create $INSTALL_DIR (try --dir <writable-path>; this script never sudos)"
    [ -w "$INSTALL_DIR" ] ||
        die "$INSTALL_DIR is not writable (try --dir <writable-path>; this script never sudos)"

    prev=""
    if [ -x "$INSTALL_DIR/fdb" ]; then
        prev=$("$INSTALL_DIR/fdb" version --short 2>/dev/null) || prev="unknown"
    fi

    # macOS Gatekeeper: clear the quarantine bit some setups stamp onto
    # downloads. Harmless no-op elsewhere.
    if [ "$OS" = darwin ]; then
        xattr -d com.apple.quarantine "$WORK_DIR/fdb" 2>/dev/null || true
    fi

    # Atomic swap: stage next to the target so mv is rename(2), never a
    # partial copy over a binary someone is running.
    staged="$INSTALL_DIR/.fdb.new.$$"
    cp "$WORK_DIR/fdb" "$staged" || die "failed writing to $INSTALL_DIR"
    chmod 755 "$staged"
    mv -f "$staged" "$INSTALL_DIR/fdb" || { rm -f "$staged"; die "failed installing to $INSTALL_DIR/fdb"; }

    # Prove the binary actually runs on this machine before declaring victory.
    installed=$("$INSTALL_DIR/fdb" version --short 2>/dev/null) ||
        die "$INSTALL_DIR/fdb was installed but does not run on this system"

    if [ -n "$prev" ] && [ "$prev" != "$installed" ]; then
        ok "upgraded fdb $prev ${c_dim}→${c_reset} ${c_bold}$installed${c_reset} at $INSTALL_DIR/fdb"
    else
        ok "installed ${c_bold}fdb $installed${c_reset} at $INSTALL_DIR/fdb"
    fi
}

path_guidance() {
    case ":$PATH:" in
        *":$INSTALL_DIR:"*) return 0 ;;
    esac
    shell_name=$(basename "${SHELL:-sh}")
    printf '\n%s\n' "${c_bold}$INSTALL_DIR is not on your PATH.${c_reset} Add it:" >&2
    # shellcheck disable=SC2016 # the printed command must contain a literal $PATH
    case "$shell_name" in
        fish) printf '  fish_add_path %s\n' "$INSTALL_DIR" >&2 ;;
        zsh)  printf '  echo '\''export PATH="%s:$PATH"'\'' >> ~/.zshrc && exec zsh\n' "$INSTALL_DIR" >&2 ;;
        *)    printf '  echo '\''export PATH="%s:$PATH"'\'' >> ~/.bashrc && exec bash\n' "$INSTALL_DIR" >&2 ;;
    esac
}

next_steps() {
    printf '\n%s\n' "${c_dim}next:${c_reset}" >&2
    printf '  fdb dev up     %s\n' "${c_dim}# start single-node FoundationDB in Docker${c_reset}" >&2
    printf '  fdb sql        %s\n' "${c_dim}# interactive SQL shell${c_reset}" >&2
    printf '  %s\n' "${c_dim}docs: https://fdb.dev/docs/${c_reset}" >&2
}

uninstall() {
    if [ -e "$INSTALL_DIR/fdb" ]; then
        rm -f "$INSTALL_DIR/fdb" || die "could not remove $INSTALL_DIR/fdb"
        ok "removed $INSTALL_DIR/fdb"
        info "config (if any) is left at ~/.fdb; remove it yourself if you're done"
    else
        info "nothing to remove at $INSTALL_DIR/fdb"
    fi
    exit 0
}

main() {
    parse_args "$@"
    # Resolve the default install dir here so --dir / FDB_INSTALL_DIR win first,
    # and an unset $HOME dies with a clear message instead of a set -u crash.
    if [ -z "$INSTALL_DIR" ]; then
        [ -n "${HOME:-}" ] || die "cannot determine an install dir: \$HOME is unset. Pass --dir <path> or set FDB_INSTALL_DIR."
        INSTALL_DIR="$HOME/.local/bin"
    fi
    have curl || have wget || die "need curl or wget"
    have tar || die "need tar"
    [ "$UNINSTALL" = 1 ] && uninstall
    detect_platform
    make_workdir
    resolve_version
    download_and_verify
    install_binary
    path_guidance
    next_steps
}

main "$@"
