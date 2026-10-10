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

1. CI is green on the tag commit (all jobs, including the doc-consistency guard).
2. `MODULE.bazel` / `go.mod` version pins are confirmed current.
3. `CHANGELOG.md`: rename `## [Unreleased]` → `## [vX.Y.Z] - <date>` with all four **Compatibility**
   notes filled (wire format, SQL, FDB options, required versions); open a fresh `## [Unreleased]`.
4. If anything touched the wire format, it is called out and backed by passing conformance +
   differential + stress runs (default expectation: nothing did).
5. After dependency, toolchain or target changes, regenerate and review notices with
   `python3 scripts/update-third-party-notices.py`. Include `LICENSE`, `NOTICE` and
   `THIRD_PARTY_NOTICES.txt` in every binary archive.
6. `git tag vX.Y.Z` + publish a GitHub release pointing at the changelog entry.

No tag is cut automatically by CI or by this document.
