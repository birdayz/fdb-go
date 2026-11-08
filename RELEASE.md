# Release & support policy

`fdb-record-layer-go` is **pre-1.0**. This document defines how versions, support windows, and
releases work until a `v1`. See `CHANGELOG.md` for what changed, `SECURITY.md` for vulnerability
handling, [STATUS.md](STATUS.md) for readiness, and [docs/upgrade.md](docs/upgrade.md)
for upgrade and rollback boundaries.

## Versioning

Tags are `v0.MINOR.PATCH`. Two axes, deliberately decoupled:

- **Go API (unstable pre-1.0).** A minor bump (`v0.N.0`) may include **breaking Go API changes**
  (renamed/removed exported symbols, changed signatures). Patch bumps (`v0.N.P`) are bug fixes and
  additive only. Pin a version and read `CHANGELOG.md` before upgrading.
- **Persisted data compatibility is release-specific.** The current Java target is
  **Record Layer 4.14.2.0**, with the exceptions in [docs/compatibility.md](docs/compatibility.md).
  Do not infer cross-release SQL storage or cross-engine continuation compatibility
  from that target. v0.2.0 changes v0.1.0's Go SQL keyspace without an
  automatic migration. Persisted-format changes must be explicit in the changelog's
  **Compatibility** block and validated by the relevant conformance, differential
  and stress suites before release.

The required dependency versions for a release (Java Record Layer, FDB C++ client, Go) are the pins
in `MODULE.bazel` / `go.mod`.

## Support window

- The **latest tagged minor** is supported; security and correctness fixes land there.
- Older minors are best-effort only until `v1`.
- The first tag is **v0.1.0** (2026-08-26). The latest tag is **v0.2.0** (2026-11-07,
  commit `aef9515`). Documentation at HEAD describes HEAD, which can include unreleased
  changes after the latest tag. A support policy is not evidence of a backport.
  Pin a revision and read its changelog and upgrade guidance.
- Security fixes follow `SECURITY.md` (private report → fix on the latest minor / `master` → disclose).

## Cutting a release (checklist)

Cutting a tag is a **one-way stability assertion and is the maintainer's decision**: this repo
provides the machinery, not the act. The Go module proxy keeps every tagged version forever. When
the maintainer chooses to cut `vX.Y.Z`:

1. **Cut PR** to master, opened once everything the release should carry has merged:
   - `CHANGELOG.md`: rename `## [Unreleased]` → `## [vX.Y.Z] - <date>` with all four
     **Compatibility** notes filled (wire format, SQL, FDB options, required versions); open a
     fresh `## [Unreleased]`.
   - Pages that describe "the development tree" or "the unreleased tree" (`STATUS.md`,
     `docs/compatibility.md`, `docs/upgrade.md`, this file, `website/content/docs/maturity.md`)
     say what the tag contains.
   - A minor release (`vX.Y.0`) has `docs/release-notes/vX.Y.0.md`; read the body with
     `scripts/release-notes.sh --check vX.Y.Z`.
   - After dependency, toolchain or target changes, regenerate and review notices with
     `python3 scripts/update-third-party-notices.py`. The workflow packs `LICENSE`, `NOTICE` and
     `THIRD_PARTY_NOTICES.txt` into every archive.
2. **Checks on the merged cut commit.** Two independent final reviewers ACK the
   exact cut-PR head, every required PR check succeeds on that head, and every
   master CI job succeeds on the merged cut SHA. `MODULE.bazel` / `go.mod` pins
   remain current. For wire-format or Java-target changes (v0.2.0: Java 4.14.2.0
   and HNSW layout), the Compatibility block is explicit and Java conformance,
   cross-client differential and the full lane pass **in CI on that commit**.
   Local validation is `just test` only; do not run `just test-full` locally.
   The ordinary CI suite skips stress/manual targets, so it is not the full-lane gate.
   Opt in with `gh workflow run ci.yml --ref <candidate-branch> -f full=true`:
   rehearse the dispatch path on the candidate branch, then repeat with `--ref master`
   after the cut merges. Check `gh run view <id> --json headSha,jobs` against the exact
   cut SHA and require the **Full release lane (default budgets)** job to succeed,
   not be skipped. A branch push does not replace a completed full dispatch.
   Download `full-lane-evidence`; retain its report, BEP, tested-input manifests and
   exit metadata, separating executed tests from cached results. This lane includes
   manual targets but excludes `realcluster` targets and does not establish every
   extended seed, race or active-fuzz mode. It runs only on explicit dispatch;
   ordinary PR/push CI does not run the full lane.
3. **Dry run** on that commit: `gh workflow run fdb-release.yml --ref master -f version=vX.Y.Z`
   and wait for success; check that the run's commit (`gh run view <id> --json headSha`) is the cut
   commit. It builds, smoke-tests and previews the notes; a dispatch never publishes.
4. **Tag** the cut commit by SHA:
   `git fetch origin && git tag -a vX.Y.Z -m vX.Y.Z <sha> && git push origin vX.Y.Z`.
   Report the exact cut SHA and verified gates to the maintainer and obtain
   explicit tagging authorization first. `fdb-release.yml` publishes the GitHub
   release; do not create it by hand.
5. **Check what users get**, in a clean environment:

   ```sh
   export GOPROXY=https://proxy.golang.org GOFLAGS=-modcacherw GOMODCACHE="$(mktemp -d)" GOBIN="$(mktemp -d)"
   go list -m fdb.dev@latest                                          # fdb.dev vX.Y.Z; the proxy's list can lag ~30 min
   go install fdb.dev/cmd/fdb@vX.Y.Z && "$GOBIN/fdb" version --short  # vX.Y.Z
   ```

   `curl -fsSL https://fdb.dev/install.sh | sh` installs vX.Y.Z.
6. **Record publication** in a follow-up PR: pages that say publication is pending
   (step 1's pages, `docs/release-notes/vX.Y.0.md`) say the tag is published, and
   `SECURITY.md` names the new supported minor. If the notes change, regenerate the
   release body: `scripts/release-notes.sh vX.Y.Z | gh release edit vX.Y.Z --notes-file -`.

The CLI is the root module's `cmd/fdb` package. Releases contain
`fdb_<version>_<os>_<arch>.tar.gz` with the `fdb` binary and license notices,
plus `checksums.txt`. The installer accepts root `vX.Y.Z` release tags only;
it does not install legacy binaries or nested-module releases.

No tag is cut automatically by CI or by this document.
