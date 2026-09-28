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
# frl is a package of the root module, so a release is the project's tag
# vX.Y.Z, the same tag `go install fdb.dev/cmd/frl@vX.Y.Z` resolves. v0.1.0
# shipped frl from a nested module under the tag cmd/frl/v0.1.0; that release
# stays installable, so both tag forms are accepted (TAG_FORMS, tag_of).
LEGACY_TAG_PREFIX="cmd/frl/"
TAG_FORMS="root legacy"

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

# fetch <url> <dest-file>. Everything this script downloads lands in a file,
# including the release-list JSON: a to-stdout variant can only be consumed
# through a pipe, and a pipe's exit status hides the fetch's (see
# resolve_version).
fetch() {
    if have curl; then
        curl -fsSL --retry 3 --connect-timeout 15 -o "$2" "$1"
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
    # Accept v0.1.0, 0.1.0, or the legacy full tag cmd/frl/v0.1.0.
    case "$VERSION" in
        "$LEGACY_TAG_PREFIX"*) VERSION="${VERSION#"$LEGACY_TAG_PREFIX"}"; TAG_FORMS=legacy ;;
    esac
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

    # Three outcomes have to stay TELLABLE APART, because they have three
    # different fixes: the query never got an answer, the query was answered and
    # the project has published nothing at all, and the query was answered with
    # releases that are not this CLI's.
    #
    # Piping the fetch straight into grep collapses all three. A pipeline's exit
    # status is its LAST command's, so `curl | grep | sed | head` reports head's
    # 0 no matter how the fetch went, and every outcome arrives here identically:
    # as an empty VERSION. Whatever single message that emptiness then printed
    # was a guess, and for the whole period this repository had no releases it
    # guessed wrong -- it blamed rate-limiting and prescribed FRL_VERSION=v0.1.0,
    # a tag that did not exist, so following the advice produced a 404 and a
    # second wrong diagnosis on top of the first.
    #
    # So: fetch to a file, check the fetch on its own, then classify the body.
    releases="$WORK_DIR/releases.json"
    if ! fetch "$API_URL/releases?per_page=100" "$releases"; then
        die "could not reach the GitHub release API at $API_URL.
       Offline, behind a proxy, or rate-limited (60 requests/hour unauthenticated)?
       Retry, or skip the lookup by pinning a version:
         FRL_VERSION=vX.Y.Z sh install.sh
       Releases: $REPO_URL/releases
       Or build from source: go install fdb.dev/cmd/frl@latest"
    fi

    # Newest stable release, in either tag form. The API returns releases
    # newest-first; take the first stable-looking tag (the [0-9.]* pattern
    # stops at the hyphen in -rc/-beta prereleases, so those never match, and
    # the tag must be exactly vX.Y.Z or cmd/frl/vX.Y.Z).
    tags=$(grep -o '"tag_name"[^,]*' "$releases")
    latest=$(printf '%s\n' "$tags" |
        sed -n 's|.*"tag_name"[^"]*"\(\('"$LEGACY_TAG_PREFIX"'\)\{0,1\}v[0-9][0-9.]*\)".*|\1|p' |
        head -n1)
    case "$latest" in
        "$LEGACY_TAG_PREFIX"*) VERSION="${latest#"$LEGACY_TAG_PREFIX"}"; TAG_FORMS=legacy ;;
        *) VERSION="$latest"; TAG_FORMS=root ;;
    esac

    if [ -z "$VERSION" ]; then
        if [ -z "$tags" ]; then
            die "$REPO_URL has published no releases, so there is no binary to install.
       The release list was fetched successfully and is empty -- this is not a
       network or rate-limit problem, and no FRL_VERSION pin will help.
       Build from source instead (same binary, needs the Go toolchain):
         go install fdb.dev/cmd/frl@latest"
        fi
        die "$REPO_URL has releases, but none tagged vX.Y.Z (or ${LEGACY_TAG_PREFIX}vX.Y.Z) --
       the frl CLI has not been released from this repository yet.
       Build from source instead: go install fdb.dev/cmd/frl@latest"
    fi
    info "latest release: frl $VERSION"
}

# make_workdir runs before resolve_version, which needs somewhere to land the
# release-list response: reading that body is what lets an empty list and a
# failed request be reported as the different things they are.
make_workdir() {
    WORK_DIR=$(mktemp -d "${TMPDIR:-/tmp}/frl-install.XXXXXX") || die "mktemp failed"
    trap cleanup EXIT
    trap 'exit 130' INT TERM
}

# tag_of <form>: $VERSION's release tag in one of the two tag forms.
tag_of() {
    case "$1" in
        root)   printf '%s' "$VERSION" ;;
        legacy) printf '%s%s' "$LEGACY_TAG_PREFIX" "$VERSION" ;;
    esac
}

download_and_verify() {
    ASSET="frl_${VERSION}_${OS}_${ARCH}.tar.gz"

    # Each candidate tag in turn: an explicit --version does not say which form
    # its release was published under (v0.1.0 is cmd/frl/v0.1.0, later ones are
    # vX.Y.Z). GitHub accepts a slashed tag raw in download URLs; the
    # %2F-encoded form is the fallback. checksums.txt comes from the release
    # that served the asset.
    info "downloading $ASSET"
    base=""
    tried=""
    for form in $TAG_FORMS; do
        tag=$(tag_of "$form")
        raw="$REPO_URL/releases/download/$tag"
        enc="$REPO_URL/releases/download/$(printf '%s' "$tag" | sed 's|/|%2F|g')"
        if fetch "$raw/$ASSET" "$WORK_DIR/$ASSET" 2>/dev/null; then base="$raw"; break; fi
        if [ "$enc" != "$raw" ] && fetch "$enc/$ASSET" "$WORK_DIR/$ASSET" 2>/dev/null; then base="$enc"; break; fi
        tried="$tried
         $raw/$ASSET"
    done
    [ -n "$base" ] || die "download failed:$tried
       Does the release exist for ${OS}/${ARCH}? See $REPO_URL/releases"
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
    make_workdir
    resolve_version
    download_and_verify
    install_binary
    path_guidance
    next_steps
}

main "$@"
