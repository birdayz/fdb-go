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

Cutting a tag is a **one-way stability assertion and is the maintainer's decision** — this repo
provides the machinery, not the act. When the maintainer chooses to cut `vX.Y.Z`:

1. Every required check is green on the commit to tag.
2. `MODULE.bazel` / `go.mod` version pins are confirmed current.
3. `CHANGELOG.md`: rename `## [Unreleased]` → `## [vX.Y.Z] - <date>` with all four **Compatibility**
   notes filled (wire format, SQL, FDB options, required versions); open a fresh `## [Unreleased]`.
   Pages that describe "the development tree" or "the unreleased tree" (`STATUS.md`,
   `docs/compatibility.md`, `docs/upgrade.md`, this file, `website/content/docs/maturity.md`)
   say what the tag contains.
4. If anything touched the wire format, it is called out and backed by passing conformance +
   differential + stress runs (default expectation: nothing did).
5. After dependency, toolchain or target changes, regenerate and review notices with
   `python3 scripts/update-third-party-notices.py`. Include `LICENSE`, `NOTICE` and
   `THIRD_PARTY_NOTICES.txt` in every binary archive.
6. Release notes: write `docs/release-notes/vX.Y.Z.md` for a minor release. Preview the body
   with `scripts/release-notes.sh vX.Y.Z`, then dry-run the pipeline on the commit to tag:
   `gh workflow run frl-release.yml --ref master -f version=vX.Y.Z`.
7. `git tag -a vX.Y.Z -m vX.Y.Z <commit> && git push origin vX.Y.Z`. `frl-release.yml` builds,
   smoke-tests, attests and publishes the GitHub release; do not create it by hand.
8. Check what users get, from a clean module cache:
   `GOPROXY=https://proxy.golang.org go list -m fdb.dev@vX.Y.Z`, then
   `go install fdb.dev/cmd/frl@vX.Y.Z && frl version --short` and `curl -fsSL https://fdb.dev/install.sh | sh`.

### Once, with v0.2.0: retire the nested `fdb.dev/cmd/frl` module

v0.1.0 shipped `frl` as the nested module `fdb.dev/cmd/frl` (tag `cmd/frl/v0.1.0`). As the longer
module path it wins `go install fdb.dev/cmd/frl@latest` over the root module until every version
of it is retracted. Branch `release/cmd-frl-v0.1.1-retract` holds the retraction: a
`cmd/frl/go.mod` retracting v0.1.0 and v0.1.1. It must never merge into master.

1. Before tagging, `curl -s 'https://fdb.dev/cmd/frl?go-get=1'` answers 200 with a `go-import`
   meta; the proxy cannot fetch the new version without it.
2. Tag v0.2.0 first and wait until `GOPROXY=https://proxy.golang.org go list -m fdb.dev@v0.2.0`
   resolves. Retracting the nested module earlier would make `@latest` fall back to root
   v0.1.0, which does not contain `cmd/frl`.
3. `git tag -a cmd/frl/v0.1.1 -m "Retract the nested frl module" origin/release/cmd-frl-v0.1.1-retract`
   and push it. No workflow runs for this tag. Do not create a GitHub release for it:
   `install.sh` would offer it as the newest release.
4. `GOPROXY=https://proxy.golang.org go list -m -retracted fdb.dev/cmd/frl@v0.1.1`, then from a
   clean `GOMODCACHE`: `go install fdb.dev/cmd/frl@latest` and
   `go version -m "$(go env GOPATH)/bin/frl"` shows `mod fdb.dev v0.2.0`.

No tag is cut automatically by CI or by this document.
