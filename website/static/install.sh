#!/bin/sh
# fdb.dev CLI installer
#
#   curl -fsSL https://fdb.dev/install.sh | sh
#
# Installs `frl`, the FoundationDB Record Layer CLI, as a single static
# binary (pure Go, CGO_ENABLED=0; the same binary runs on glibc, musl,
# and FROM scratch). Downloads the release archive from GitHub, verifies
# its sha256 against the release's checksums.txt, and installs atomically.
# Never uses sudo.
#
# Prefer the Go toolchain? This is equivalent (slower, builds from source):
#   go install fdb.dev/cmd/frl@latest
#
# The whole script runs from main() at the bottom, so a truncated download
# can't execute a half-script.

set -u

REPO_URL="${FRL_BASE_URL:-https://github.com/birdayz/fdb-go}"
API_URL="${FRL_API_URL:-https://api.github.com/repos/birdayz/fdb-go}"
# Release tags follow the Go convention for modules in a subdirectory:
# cmd/frl/vX.Y.Z. That is what lets `go install fdb.dev/cmd/frl@vX.Y.Z`
# resolve the exact same version the binary reports.
TAG_PREFIX="cmd/frl/"

# Default resolved in main() (needs $HOME, which set -u would trip on if unset).
INSTALL_DIR="${FRL_INSTALL_DIR:-}"
VERSION="${FRL_VERSION:-latest}"
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
fdb.dev CLI installer: installs the static frl binary

usage:
  curl -fsSL https://fdb.dev/install.sh | sh
  sh install.sh [options]

options:
  --version <vX.Y.Z>   install a specific version   (env: FRL_VERSION)
  --dir <path>         install directory            (env: FRL_INSTALL_DIR)
                       default: ~/.local/bin
  --uninstall          remove an installed frl and exit
  -h, --help           show this help

environment:
  NO_COLOR             disable colored output (https://no-color.org)
  FRL_BASE_URL         release download base, for mirrors/testing
                       default: https://github.com/birdayz/fdb-go
  FRL_API_URL          GitHub API base for "latest" resolution
                       default: https://api.github.com/repos/birdayz/fdb-go

prefer the Go toolchain? equivalent, builds from source:
  go install fdb.dev/cmd/frl@latest
EOF
}

# ---- helpers --------------------------------------------------------------

have() { command -v "$1" >/dev/null 2>&1; }

cleanup() { [ -n "$WORK_DIR" ] && rm -rf "$WORK_DIR"; }

# fetch <url> <dest-file>; fetch_stdout <url>
fetch() {
    if have curl; then
        curl -fsSL --retry 3 --connect-timeout 15 -o "$2" "$1"
    else
        wget -q -O "$2" "$1"
    fi
}
fetch_stdout() {
    if have curl; then
        curl -fsSL --retry 3 --connect-timeout 15 "$1"
    else
        wget -q -O - "$1"
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
    # Accept v0.1.0, 0.1.0, or the full tag cmd/frl/v0.1.0.
    VERSION="${VERSION#"$TAG_PREFIX"}"
    case "$VERSION" in
        latest|v*) ;;
        *) VERSION="v$VERSION" ;;
    esac
}

detect_platform() {
    case "$(uname -s)" in
        Linux)  OS=linux ;;
        Darwin) OS=darwin ;;
        MINGW*|MSYS*|CYGWIN*|Windows_NT)
            die "FoundationDB has no native Windows client story, so neither does frl.
       Use WSL and run this installer inside it (the linux binary is static)." ;;
        *)  die "unsupported OS: $(uname -s). Try: go install fdb.dev/cmd/frl@latest" ;;
    esac
    case "$(uname -m)" in
        x86_64|amd64)  ARCH=amd64 ;;
        arm64|aarch64) ARCH=arm64 ;;
        *)  die "unsupported architecture: $(uname -m). Try: go install fdb.dev/cmd/frl@latest" ;;
    esac
    # One static binary per OS/arch. No glibc/musl split needed: pure Go,
    # no cgo, so the linux binary runs on Debian, Alpine, NixOS, scratch.
    info "detected ${OS}/${ARCH}"
}

resolve_version() {
    [ "$VERSION" != "latest" ] && return 0
    # Newest stable cmd/frl/v* release. The API returns releases newest-first;
    # take the first stable-looking tag (the [0-9.]* pattern stops at the
    # hyphen in -rc/-beta prereleases, so those never match).
    VERSION=$(fetch_stdout "$API_URL/releases?per_page=100" 2>/dev/null |
        grep -o '"tag_name"[^,]*' |
        sed -n 's|.*"'"$TAG_PREFIX"'\(v[0-9][0-9.]*\)".*|\1|p' |
        head -n1)
    [ -n "$VERSION" ] || die "could not find a frl release at $API_URL.
       If you're rate-limited or offline, pin one: FRL_VERSION=v0.1.0
       Releases: $REPO_URL/releases. Or build from source: go install fdb.dev/cmd/frl@latest"
    info "latest release: frl $VERSION"
}

download_and_verify() {
    ASSET="frl_${VERSION}_${OS}_${ARCH}.tar.gz"
    WORK_DIR=$(mktemp -d "${TMPDIR:-/tmp}/frl-install.XXXXXX") || die "mktemp failed"
    trap cleanup EXIT
    trap 'exit 130' INT TERM

    # GitHub accepts the slashed tag raw in download URLs; fall back to the
    # %2F-encoded form just in case.
    base_raw="$REPO_URL/releases/download/${TAG_PREFIX}${VERSION}"
    base_enc="$REPO_URL/releases/download/$(printf '%s' "$TAG_PREFIX" | sed 's|/|%2F|g')${VERSION}"

    info "downloading $ASSET"
    if ! fetch "$base_raw/$ASSET" "$WORK_DIR/$ASSET" 2>/dev/null; then
        fetch "$base_enc/$ASSET" "$WORK_DIR/$ASSET" ||
            die "download failed: $base_raw/$ASSET
       Does the release exist for ${OS}/${ARCH}? See $REPO_URL/releases"
    fi
    if ! fetch "$base_raw/checksums.txt" "$WORK_DIR/checksums.txt" 2>/dev/null; then
        fetch "$base_enc/checksums.txt" "$WORK_DIR/checksums.txt" ||
            die "download failed: checksums.txt (refusing to install an unverified binary)"
    fi

    want=$(awk -v a="$ASSET" '$2 == a { print $1 }' "$WORK_DIR/checksums.txt")
    [ -n "$want" ] || die "no entry for $ASSET in checksums.txt"
    got=$(sha256_of "$WORK_DIR/$ASSET")
    [ "$got" = "$want" ] || die "sha256 mismatch for $ASSET
       expected $want
       got      $got
       The download may be corrupted or tampered with. Nothing was installed."
    ok "checksum verified ${c_dim}(sha256 $want)${c_reset}"

    tar -xzf "$WORK_DIR/$ASSET" -C "$WORK_DIR" frl || die "could not extract frl from $ASSET"
}

install_binary() {
    mkdir -p "$INSTALL_DIR" 2>/dev/null ||
        die "cannot create $INSTALL_DIR (try --dir <writable-path>; this script never sudos)"
    [ -w "$INSTALL_DIR" ] ||
        die "$INSTALL_DIR is not writable (try --dir <writable-path>; this script never sudos)"

    prev=""
    if [ -x "$INSTALL_DIR/frl" ]; then
        prev=$("$INSTALL_DIR/frl" version --short 2>/dev/null) || prev="unknown"
    fi

    # macOS Gatekeeper: clear the quarantine bit some setups stamp onto
    # downloads. Harmless no-op elsewhere.
    if [ "$OS" = darwin ]; then
        xattr -d com.apple.quarantine "$WORK_DIR/frl" 2>/dev/null || true
    fi

    # Atomic swap: stage next to the target so mv is rename(2), never a
    # partial copy over a binary someone is running.
    staged="$INSTALL_DIR/.frl.new.$$"
    cp "$WORK_DIR/frl" "$staged" || die "failed writing to $INSTALL_DIR"
    chmod 755 "$staged"
    mv -f "$staged" "$INSTALL_DIR/frl" || { rm -f "$staged"; die "failed installing to $INSTALL_DIR/frl"; }

    # Prove the binary actually runs on this machine before declaring victory.
    installed=$("$INSTALL_DIR/frl" version --short 2>/dev/null) ||
        die "$INSTALL_DIR/frl was installed but does not run on this system"

    if [ -n "$prev" ] && [ "$prev" != "$installed" ]; then
        ok "upgraded frl $prev ${c_dim}→${c_reset} ${c_bold}$installed${c_reset} at $INSTALL_DIR/frl"
    else
        ok "installed ${c_bold}frl $installed${c_reset} at $INSTALL_DIR/frl"
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
    printf '  frl fdb up     %s\n' "${c_dim}# start single-node FoundationDB in Docker${c_reset}" >&2
    printf '  frl sql        %s\n' "${c_dim}# interactive SQL shell${c_reset}" >&2
    printf '  %s\n' "${c_dim}docs: https://fdb.dev/docs/${c_reset}" >&2
}

uninstall() {
    if [ -e "$INSTALL_DIR/frl" ]; then
        rm -f "$INSTALL_DIR/frl" || die "could not remove $INSTALL_DIR/frl"
        ok "removed $INSTALL_DIR/frl"
        info "config (if any) is left at ~/.frl; remove it yourself if you're done"
    else
        info "nothing to remove at $INSTALL_DIR/frl"
    fi
    exit 0
}

main() {
    parse_args "$@"
    # Resolve the default install dir here so --dir / FRL_INSTALL_DIR win first,
    # and an unset $HOME dies with a clear message instead of a set -u crash.
    if [ -z "$INSTALL_DIR" ]; then
        [ -n "${HOME:-}" ] || die "cannot determine an install dir: \$HOME is unset. Pass --dir <path> or set FRL_INSTALL_DIR."
        INSTALL_DIR="$HOME/.local/bin"
    fi
    have curl || have wget || die "need curl or wget"
    have tar || die "need tar"
    [ "$UNINSTALL" = 1 ] && uninstall
    detect_platform
    resolve_version
    download_and_verify
    install_binary
    path_guidance
    next_steps
}

main "$@"
