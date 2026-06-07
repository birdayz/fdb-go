# RFC-089 — Conformance CI failures show no detail (Java-stdout flood truncates the log)

**Status:** Implemented — Torvalds ACK, bradfitz ACK.
**Found:** PR #269's CI hit a `//conformance:conformance_test` failure whose CI log
showed only `FAILED TestConformance` — no scenario, no Go-vs-Java mismatch. A flake
you can't see is a flake you can't root-cause. That opacity is itself a bug.

## Problem

`java_invoker_test.go:339/344` forwards the pooled Java server's **stdout and stderr**
to our `os.Stderr`. The Java server calls `printStackTrace()` on **every** caught
exception (~5-10KB each) — and over a full A3 run Java rejects *thousands* of shapes
it can't plan (`UnableToPlanException`), so the forwarded flood is multiple MB. On a
test failure, `.bazelrc`'s `--test_output=errors` dumps the whole test.log; GitHub
Actions **truncates** the oversized step log, dropping the Ginkgo failure summary —
which Ginkgo prints at the *end* — and leaving only the bazel one-liner. Result:
a red CI with zero diagnosable detail.

Confirmed: the CI step log for the failing run had **0** `java-stdout` lines, **0**
Ginkgo markers (no `Summarizing` / `[FAILED]` / `Ran N specs`), just `FAILED
TestConformance`. Locally the same test floods the log.

## Constraint

The Java stderr MUST keep being drained continuously — if the 64KB pipe fills, Java's
next `printStackTrace()` **blocks mid-write** before sending the HTTP response and the
Go POST hangs to its 120s timeout (documented deadlock at `:325-338`). So the fix
can't simply stop reading.

## Fix

Keep draining (deadlock-safe) but **don't forward** the Java server's stdout/stderr to
our stderr by default — discard it. Forward only under `CONFORMANCE_DEBUG` (interactive
debugging, where the Ginkgo failure is read live anyway). The test.log then stays small
and the Ginkgo `FAIL!` + `Summarizing N Failures:` (scenario + Go-vs-Java mismatch)
survives in CI.

~12 lines in `java_invoker_test.go`. No production code, no test-logic change — pure
harness output hygiene.

## Verification

- After: a full conformance run's test.log dropped from a multi-MB Java flood to
  **508 lines**, **0** flood lines, Ginkgo `SUCCESS!` summary visible.
- Forced one scenario to fail (`qualified_star` expected → wrong value): the test.log
  now clearly shows
  `• [FAILED] scenario qualified_star [It] SELECT a.* FROM a ORDER BY id` /
  `[Go vs expected] row 1 col 1 / Expected … to equal … <wrong>` /
  `Summarizing 1 Failure: [FAIL] … qualified_star` / `FAIL! -- 1204 Passed | 1 Failed`.
  Exactly the detail that was missing in CI.
- `CONFORMANCE_DEBUG=1` still forwards the Java logs for live debugging.

## Out of scope

- The underlying A3 flake that triggered this (the join-enum row-count nondeterminism,
  RFC-082 follow-up / line-54) — now *diagnosable* with this fix, root-caused separately.
