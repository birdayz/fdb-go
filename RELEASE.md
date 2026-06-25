# Release & support policy

`fdb-record-layer-go` is **pre-1.0**. This document defines how versions, support windows, and
releases work until a `v1`. See `CHANGELOG.md` for what changed, `SECURITY.md` for vulnerability
handling, and `PRODUCTION_READINESS.md` for the current readiness snapshot.

## Versioning

Tags are `v0.MINOR.PATCH`. Two axes, deliberately decoupled:

- **Go API (unstable pre-1.0).** A minor bump (`v0.N.0`) may include **breaking Go API changes**
  (renamed/removed exported symbols, changed signatures). Patch bumps (`v0.N.P`) are bug fixes and
  additive only. Pin a version and read `CHANGELOG.md` before upgrading.
- **FDB wire format (the hard line — stable across every tag).** Records, indexes, record versions,
  continuations, and split records stay **byte-compatible with Java `fdb-record-layer-core`
  4.12.11.0** in *every* release, pre-1.0 included. Go, C, and Java apps share one cluster and read
  each other's data; a release that broke that would be a bug, not a minor bump. Any change that
  *did* touch persisted bytes would be called out explicitly in the `CHANGELOG.md` **Compatibility**
  block and gated on conformance + cross-engine differential + binding-stress proof.

The required dependency versions for a release (Java Record Layer, FDB C++ client, Go) are the pins
in `MODULE.bazel` / `go.mod`; the CI doc-consistency guard (`pkg/docscheck`) fails if a living doc
asserts a version other than those pins.

## Support window

- The **latest tagged minor** is supported; security and correctness fixes land there.
- Older minors are best-effort only until `v1`.
- **Until the first tag is cut, the only supported ref is the latest `master`** — pin a commit and
  run the conformance + cross-engine differential + binding-stress suites against your workload
  before relying on it (consistent with `SECURITY.md`).
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
5. `git tag vX.Y.Z` + publish a GitHub release pointing at the changelog entry.

No tag is cut automatically by CI or by this document.
