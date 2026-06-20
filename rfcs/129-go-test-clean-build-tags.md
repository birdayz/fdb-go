# RFC-129 — `go test ./...` clean: build-tag the Bazel-runfiles-only tests

**Status:** Implemented (PR #327)
**Item:** prod-readiness-audit-2026-06-19.md **P1** — "Plain `go test ./...` Is Not A Clean Contributor Or
Adoption Path."
**Reviewers:** Torvalds (code/build quality) + codex/@claude. *Not* a query-engine or wire change, so no
Graefe gate.

---

## 1. Problem (verified)

`go test ./...` from the repo root panics/setup-fails on two packages that depend on **Bazel runfiles**,
which a plain `go` invocation does not provide:

- `conformance/` (`java_invoker_test.go` → `NewJavaInvoker` **panics** when the Java conformance-server jar
  isn't reachable via runfiles; the whole `package conformance_test` suite needs it via its shared setup).
- `cmd/fdb-stacktester/bindingtester/` (`binding_test.go` builds a Docker context from Bazel runfiles).

Both run **fine under Bazel** (`just test` / CI: `//conformance:conformance_test`,
`//cmd/fdb-stacktester/bindingtester:bindingtester_test`) where runfiles exist. The failure is only under
plain `go test`, and it's a contributor-experience / downstream-scanner smell.

## 2. Why build tags, not `t.Skip`

The CLAUDE.md **NO-SKIPS** directive allows exactly one runtime skip (the Docker check); a
"runfiles-absent → `t.Skip`" would add a second skip class. A **build tag** avoids `t.Skip` entirely — the
file is simply *not compiled* under plain `go test` — and (unlike the existing `//go:build integration`
pattern, which excludes from Bazel too) we keep the tests running under Bazel by **defining the tag for all
Bazel builds**. So: directive-compliant *and* zero loss of Bazel coverage.

## 3. Change

1. **Tag** every test file in the two packages with `//go:build bazelrunfiles` (64 conformance + 1 binding).
2. **Define the tag for Bazel** — `.bazelrc`: `build --@rules_go//go/config:tags=bazelrunfiles`. (Only files
   carrying that constraint are affected; everything else is untouched.)
3. **Keep gazelle including the tagged files** in the `go_test` srcs — root `BUILD.bazel`:
   `# gazelle:build_tags bazelrunfiles`.
4. **`doc.go` stub** per package (`package conformance`, `package bindingtester`) — both packages are
   *test-only*, so once their tests are tagged out a plain `go test` would otherwise report "build
   constraints exclude all Go files" (a setup-fail). The stub keeps the package non-empty → `go test`
   prints a clean `[no test files]`.

## 4. Verification

- Plain `go test ./conformance/` and `./cmd/fdb-stacktester/bindingtester/` → `[no test files]` (clean,
  exit 0), no panic.
- `bazelisk build //...` (134 targets) green; both Bazel test targets still carry all 65 test files
  (`grep -c _test.go` unchanged) — Bazel coverage preserved.
- The other runfiles-touching tests (`sift_benchmark_test.go`, `fdb-diff-oracle/fuzz_test.go`) already guard
  cleanly (opt-in `SIFT_BENCH` skip; conditional `TEST_SRCDIR`) — confirmed `[no tests to run]` under plain
  `go test`, so they need no tag.

## 5. Wire/behaviour impact

**None.** Test-selection/build-config only; no product code, no persisted bytes, no Bazel test-coverage
change.
