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
  from that target. The unreleased tree changes the old Go SQL keyspace without an
  automatic migration. Persisted-format changes must be explicit in the changelog's
  **Compatibility** block and validated by the relevant conformance, differential
  and stress suites before release.

The required dependency versions for a release (Java Record Layer, FDB C++ client, Go) are the pins
in `MODULE.bazel` / `go.mod`.

## Support window

- The **latest tagged minor** is supported; security and correctness fixes land there.
- Older minors are best-effort only until `v1`.
- The first tag is **v0.1.0** (2026-08-26). The development tree is newer and has
  breaking storage/API changes; documentation at HEAD is not documentation of
  v0.1.0. A support policy is not evidence that a fix has been backported or a new
  tag published. Pin a revision and read its changelog and upgrade guidance.
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
2. **Checks on the merged cut commit.** Every CI job is green, and `MODULE.bazel` / `go.mod` pins
   are current. If the release changes the wire format or the Java target (v0.2.0 does: Java
   4.14.2.0 and the HNSW layout), the Compatibility block says so and the Java conformance,
   cross-client differential CI jobs pass, and `just test-full` passes on that commit (the CI `ci`
   job skips the stress and manual targets).
3. **Dry run** on that commit: `gh workflow run frl-release.yml --ref master -f version=vX.Y.Z`
   and wait for success; check that the run's commit (`gh run view <id> --json headSha`) is the cut
   commit. It builds, smoke-tests and previews the notes; a dispatch never publishes.
4. **Tag** the cut commit by SHA:
   `git fetch origin && git tag -a vX.Y.Z -m vX.Y.Z <sha> && git push origin vX.Y.Z`.
   `frl-release.yml` publishes the GitHub release; do not create it by hand. For v0.2.0, continue
   with the section below straight away.
5. **Check what users get**, in a clean environment:

   ```sh
   export GOPROXY=https://proxy.golang.org GOFLAGS=-modcacherw GOMODCACHE="$(mktemp -d)" GOBIN="$(mktemp -d)"
   go list -m fdb.dev@latest                                          # fdb.dev vX.Y.Z; the proxy's list can lag ~30 min
   go install fdb.dev/cmd/frl@vX.Y.Z && "$GOBIN/frl" version --short  # vX.Y.Z
   ```

   `curl -fsSL https://fdb.dev/install.sh | sh` installs vX.Y.Z.

No tag is cut automatically by CI or by this document.

### Once, with v0.2.0: retire the nested `fdb.dev/cmd/frl` module

v0.1.0 shipped `frl` as the nested module `fdb.dev/cmd/frl` (tag `cmd/frl/v0.1.0`). As the longer
module path it wins `go install fdb.dev/cmd/frl@latest` over the root module until every version
of it is retracted. Commit `948f2d3b69e0aa7fe85d2b25552b63a114384d7d` (branch `release/cmd-frl-v0.1.1-retract`) holds the
retraction: a `cmd/frl/go.mod` retracting every version up to and including v0.1.1. It must never merge
into master.

1. Before item 3, the fdb.dev stub for `cmd/frl` is live (`website.yml` deploys it when the
   release-prep change reaches master): `GOPROXY=direct go list -m -versions fdb.dev/cmd/frl`
   prints `fdb.dev/cmd/frl v0.1.0`. Without the stub the proxy cannot fetch the new tag.
2. After checklist step 4, wait until `GOPROXY=https://proxy.golang.org go list -m fdb.dev@latest` prints
   `fdb.dev v0.2.0`. Retracting earlier makes `@latest` fall back to root v0.1.0, which does not
   contain `cmd/frl`.
3. Tag the reviewed commit, then delete the branch so nobody merges it:

   ```sh
   git fetch origin
   git tag -a cmd/frl/v0.1.1 -m "Retract the nested frl module" 948f2d3b69e0aa7fe85d2b25552b63a114384d7d
   git push origin cmd/frl/v0.1.1
   git push origin --delete release/cmd-frl-v0.1.1-retract
   ```

   No workflow runs for this tag. Do not create a GitHub release for it: `install.sh` would offer
   it as the newest release.
4. In a clean environment (checklist step 5's `export`), make the proxy fetch the tag, then check:

   ```sh
   go list -m -retracted fdb.dev/cmd/frl@v0.1.1   # fdb.dev/cmd/frl v0.1.1 (retracted)
   go install fdb.dev/cmd/frl@latest && go version -m "$GOBIN/frl" | awk '$1=="mod"'   # mod fdb.dev v0.2.0 ...
   ```
