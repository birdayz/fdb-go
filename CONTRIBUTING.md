# Contributing

Bug reports, documentation corrections, regression tests, and focused pull requests
are welcome. For questions and reports, see [SUPPORT.md](SUPPORT.md). Report
security concerns according to [SECURITY.md](SECURITY.md), not in a public issue.

## Development setup

- Read [AGENTS.md](AGENTS.md) and [CLAUDE.md](CLAUDE.md) for repository rules.
- Install `just`, `bazelisk`, and Docker. The Go version is pinned in `go.mod`;
  Bazel downloads the matching Go toolchain. Docker is needed for FDB integration
  tests. See [README.md](README.md) for running the library and CLI.
- Run `just test` for the normal edit/test loop. This is the fast lane, not the
  full suite. Use `just test-full` at workstream/PR boundaries; it also runs
  heavyweight and Java conformance tests and needs their external dependencies.
  Do not substitute `go test`, `go vet`, or hand-picked Bazel targets.
- When adding or removing Go files, run `just gazelle`. For code-generation input
  changes, run `just generate` and include the resulting generated changes.
- Run `just install-hooks` to install the repository's pre-commit checks.

## Pull requests

Keep changes focused. Describe the problem, the behavior change, and the exact
validation performed, including failures or checks you could not run. Distinguish
executed tests from cached results and fast-lane validation from the full suite.
Add a regression test for a bug fix; do not hide failures with test skips.

For compatibility changes, identify the reference behavior you checked:
FoundationDB C++ 7.3.77 for the client protocol and Java Record Layer 4.14.2.0 for
Record Layer behavior. Do not claim compatibility solely from code inspection.
Avoid unrelated generated changes, formatting, or cleanup in the same patch.
