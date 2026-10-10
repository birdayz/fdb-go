#!/usr/bin/env bash
# Print the GitHub release body for a tag: docs/release-notes/<tag>.md when present, then
# the install and verify instructions. frl-release.yml publishes exactly this output.
set -euo pipefail

cd "$(dirname "$0")/.."

tag="${1:?usage: scripts/release-notes.sh vX.Y.Z}"
repo="${GITHUB_REPOSITORY:-birdayz/fdb-go}"
notes="docs/release-notes/${tag}.md"

if [[ -f "$notes" ]]; then
	cat "$notes"
	printf '\n---\n\n'
fi

cat <<EOF
Changes and compatibility notes: [CHANGELOG.md](https://github.com/${repo}/blob/${tag}/CHANGELOG.md).

Static, dependency-free binaries of the \`frl\` CLI (pure Go, no cgo; the
linux binaries run on glibc, musl, and \`FROM scratch\`).

**Install**

    curl -fsSL https://fdb.dev/install.sh | sh

or with the Go toolchain (builds the identical version):

    go install fdb.dev/cmd/frl@${tag}

**Verify**: every asset is sha256-listed in \`checksums.txt\` and carries
build provenance:

    gh attestation verify frl_${tag}_linux_amd64.tar.gz -R ${repo}
EOF
