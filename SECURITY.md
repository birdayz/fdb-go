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

The CI scan excludes `pkg/testcontainers` and `pkg/relational/sqltest`, whose
shared `testkit` imports the container helpers. These helpers wrap Docker's
client SDK for tests and developer/stress tools (including `factory-run`,
`factory-migrate`, `fdb-binding-stress`, and `sql-diff-stress`); neither the
production library nor the released `frl` CLI imports them. The exclusion is
not a clean bill of health for the dependency graph or developer workloads.

The legacy `github.com/docker/docker` module has open advisories concerning
Docker **daemon** archive/mount handling and plugin privilege validation, not
the imported client/API implementations. Some have no patched version listed
for that legacy Go module, which does not mean upstream daemon fixes are
unavailable. Fixture operations still contact a real Docker Engine: use a
patched daemon and trusted container images. Updating Go SDK dependencies does
**not** update or patch the host daemon. Available dependency security fixes
must be applied independently of that daemon/client distinction.
