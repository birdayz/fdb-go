# Security Policy

## Reporting a vulnerability

Please report security vulnerabilities **privately** — do not open a public issue
for an unfixed vulnerability.

Use [GitHub's private vulnerability reporting form](https://github.com/birdayz/fdb-go/security/advisories/new)
(**Security → Report a vulnerability**). If the form is unavailable, open a public
issue requesting a private reporting channel **without vulnerability details**.
Do not assume a commit author's email is a security reporting address.

Include: affected version/commit, a description of the issue and its impact, and a
reproduction (a failing test, a SQL statement, or a wire-level trace is ideal).

We aim to acknowledge reports within a few business days. Please allow a reasonable
window for a fix before public disclosure.

## Scope

This project is **pre-1.0 and not yet declared production-ready** (see
`PRODUCTION_READINESS.md` and `TODO-production.md`). Of particular interest:

- **Wire-format / data-integrity** issues — anything where Go and Java
  (`fdb-record-layer` 4.14.2.0) would read/write incompatible bytes, or where
  records/indexes/versions/continuations could be corrupted.
- **Pure-Go FDB client** (`pkg/fdbgo`) — it reimplements the FoundationDB wire
  protocol from scratch; correctness divergences from libfdb_c (RYW, retries,
  `commit_unknown_result`, conflict handling) are in scope.
- **Crash / DoS** — input that panics the process rather than returning an error
  (the library follows a "don't leak panics" policy: untrusted input must produce
  errors, never crash; see `docs/panic-audit.md`).
- **SQL engine** — query inputs that produce wrong results or crash.

## Supported versions

See **`RELEASE.md`** for the full versioning & support-window policy. In short: the latest tagged
minor is supported — currently **v0.1.0** — and `master` is supported as the development ref.
Older minors are best-effort until `v1`. Security fixes land on the latest supported ref.

## Dependencies

`govulncheck` runs in CI (the **Vulnerability scan** job) over the shipped
packages. Dependencies are pinned in `go.mod` / `MODULE.bazel`; report any
known-vulnerable dependency you spot.

Known accepted exception: `pkg/testcontainers` wraps the Docker SDK
(`github.com/docker/docker`) **for tests only** (it is not part of the shipped
library). That SDK currently has open upstream advisories with **no fixed
release** (e.g. GO-2026-4887, GO-2026-4883), so the CI vulnerability scan excludes
`pkg/testcontainers` and `pkg/relational/sqltest` (the end-to-end SQL test suite,
whose shared `testkit` library imports it). These do not affect production use; they will be picked up
when an upstream fix ships.
