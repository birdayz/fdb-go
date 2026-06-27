---
name: hunt-divergences
description: Find behavioral divergences between the pure-Go FDB client (pkg/fdbgo) and libfdb_c via differential + fuzz testing, root-cause each against the C++ source, fix Go to match (C++ is the spec), and pin with a red→green differential. Use when auditing client correctness, when a differential/fuzz run mismatches, or when asked to "prove we're identical to the C client."
---

# Hunt Divergences (pure-Go FDB client vs libfdb_c)

The whole point of `pkg/fdbgo` is **wire + behavioral compatibility with libfdb_c**: Go
and C/Java apps share a cluster and read/write each other's data. **C++ is the spec.**
Any place the Go client behaves differently from `libfdb_c` is a **bug in Go**, not a
"Go choice" — until proven otherwise by reading the C++ source.

This skill is the method that has found 8+ real client divergences (size limits,
raw-access key limit, no-conflict-flag leak, empty-value getRange, atomic-on-present-
empty, versionstamp phantom, getKey-ignores-RYW, SVK conflict-range). It is
**differential testing**: run the same operation through both clients against ONE real
FDB and byte-compare. The oracle is libfdb_c; divergence = a concrete mismatch, not a
guess.

## The core idea

> Divergences are **dimensional, not volumetric.** You don't find them by adding more
> tests to a covered axis — you find them by probing an axis nothing has compared yet.
> 100 tests on a feature can all pass while one unprobed dimension is silently wrong.

So hunting = **enumerate axes, find the unprobed one, build a differential for it.**

## The harness (already built — reuse it)

`pkg/fdbgo/bench/` runs a dual-client fixture: `TestMain` spins **one** FDB
testcontainer and two clients — `goClient` (pure-Go) and `cgoClient` (Apple CGo binding
over `libfdb_c`). Key helpers (read these before writing a new probe):

- `differential_test.go` — fixture, `mustCGo`, `seedKeys`.
- `differential_fuzz_test.go` — the `fuzzOp` model (`fzSet/fzClear/fzClearRange/fzAdd/
  …/fzCompareAndClear/fzCommit`), `decodeFuzzOps(bytes) [][]fuzzOp`, `applyGo`/`applyC`
  (apply an op list under a key prefix), `clearPrefix`, `fuzzKeys = {a,b,c,d}`.
- `differential_read_test.go` — `freshSharedVersion(t)` (a read version inside the MVCC
  window so both clients observe identical storage), `goGetKeyAt`/`cgoGetKeyAt`.
- `differential_ryw_test.go` — `runRYWReadDifferential` (uncommitted txn per client at a
  shared version, identical pending ops, compare Get/GetRange).
- `differential_getkey_ryw_test.go` — `runGetKeyRYWDifferential` (the GetKey-over-RYW
  axis + `FuzzDifferential_GetKeyRYW`).

**Determinism contract** (so a mismatch is a *pure* divergence, not noise):
1. Clear two per-test prefixes (`os.Getpid()` + `t.Name()` → parallel-safe, never
   collide). One prefix per client (`..._go_`, `..._c_`).
2. Seed identical committed storage into both prefixes.
3. Capture **one shared read version V** (`freshSharedVersion`) — both clients read at V.
4. Apply the **same** ops to both (as committed seed, or as pending writes in an
   uncommitted txn per client).
5. Compare byte-for-byte: persisted KV state, `Get`, `GetRange` (count + each pair),
   `GetKey` resolved key, returned error class.
6. **`Cancel()` cgo txns explicitly** (the C handle needs it, not GC). Never commit for
   read-axis probes.
7. **Clamp to the prefix:** a selector/range that escapes `[prefix, prefix+\xff)`
   resolves into the concurrently-mutated shared keyspace — skip the comparison there
   (log it; don't assert). Note this in the test so the clamp isn't mistaken for full
   coverage.

## Step 1 — pick an unprobed axis

Axes where divergences hide (✓ = a differential exists; grep `bench/` to confirm):
- ✓ committed write coalescing (RYW *writes*) — `differential_fuzz_test.go`.
- ✓ RYW *reads*: `Get`/`GetRange` over pending — `differential_ryw_test.go`.
- ⏳ `GetKey` over pending writes — **OPEN** (RFC-056). The Go client's `GetKey`
  resolves selectors against storage only, ignoring pending writes; the differential
  (`differential_getkey_ryw_test.go`) + the faithful `resolveKeySelectorFromCache` port
  land together. Until then this is a known gap, not covered — keep hunting it.
- ✓ size-limit rejection (key/value/txn), raw-access key limit — `differential_test.go`.
- atomic-op edge cases on empty/missing/present-empty values (per-op, all of Atomic.h).
- conflict ranges: which ops add read/write conflicts (and the no-conflict-flag), and
  the exact ranges (e.g. getKey adds a base↔resolved RANGE, not a single key).
- error codes / messages: `1007 transaction_too_old`, `1020 not_committed`, `1004`,
  `2000 client_invalid_operation`, size/legal-range errors — same code, same trigger.
- key-encoding / tuple packing, versionstamp offset validation, continuation tokens.
- option semantics: `RAW_ACCESS`, `ACCESS_SYSTEM_KEYS`, snapshot RYW enable/disable,
  `NEXT_WRITE_NO_WRITE_CONFLICT_RANGE`.
- read-version handling near the 5s MVCC edge (a suspected go-vs-cgo asymmetry is open).

Don't re-test a ✓ axis volumetrically. Find one with no `bench/` comparison.

## Step 2 — write the differential + a fuzzer

Mirror `runRYWReadDifferential` / `runGetKeyRYWDifferential`: a deterministic
`TestDifferential_<Axis>` with hand-picked shapes that *should* expose the divergence,
**plus** a `FuzzDifferential_<Axis>` driven by `decodeFuzzOps` so it minimizes a real
seed. Seed the fuzzer corpus (`f.Add(...)`) with the divergent shapes you suspect.

```go
func runXDifferential(t *testing.T, label string, seed, pending []fuzzOp) {
    // clearPrefix(goPfx); clearPrefix(cPfx); seed both; v := freshSharedVersion(t)
    // goTxn/cTxn at v; applyGo/applyC(pending); compare the axis; Cancel() both.
}
```

## Step 3 — run it; capture the mismatch + a minimized seed

```sh
# deterministic cases:
bazelisk test //pkg/fdbgo/bench:bench_test \
  --test_arg="--test.run=TestDifferential_<Axis>" --test_arg="--test.v" --test_output=all

# fuzzer ISOLATED (skip unit tests so it actually mutates/minimizes):
bazelisk test //pkg/fdbgo/bench:bench_test \
  --test_arg="-test.run=^$" --test_arg="-test.fuzz=^FuzzDifferential_<Axis>$" \
  --test_arg="-test.fuzztime=25s" --test_arg="-test.fuzzcachedir=/tmp/fuzz-cache" \
  --sandbox_writable_path=/tmp/fuzz-cache --test_output=streamed
```

A `go="…" cgo="…"` mismatch IS the divergence. The fuzzer's failing `seed#N` (or a
mutated input) is your minimized reproducer — record the exact `fuzzOp` bytes in the
commit/RFC. Docker required (`docker ps`); if down, say so — don't fake it.

## Step 4 — root-cause against the C++ source (NEVER guess)

C++ reference: the **FoundationDB C++ source, version 7.3.77** — the `foundationdb` pin
in `MODULE.bazel` (`bazel_dep(name = "foundationdb", version = "7.3.77")`), which is what
`libfdb_c` (the cgo binding) is built from. (Do NOT confuse this with `4.11.1.0`, the
*Java* `fdb-record-layer-core` artifact version — a different project.) Check it out at
`/tmp/fdbsrc` (`git clone --branch 7.3.77 https://github.com/apple/foundationdb`). Key
files: `fdbclient/NativeAPI.actor.cpp`, `ReadYourWrites.actor.cpp`, `WriteMap.cpp`,
`RYWIterator.cpp`, `include/fdbclient/Atomic.h`, `ClientKnobs.cpp`, `SystemData.cpp`.
Read the **actual** C++ function that handles the case, understand the algorithm, then
port it 1:1. Cite `file:line` in the fix. A divergence's "why" lives in the C++ — e.g.
`is_unreadable` is sticky (`WriteMap.cpp:97,125`); doMinV2/doAndV2 gate on
`Optional.present()` (`Atomic.h`); `rawAccess = RAW_ACCESS||ACCESS_SYSTEM_KEYS||
READ_SYSTEM_KEYS` (`NativeAPI:7159`).

## Step 5 — fix Go to match, and BEWARE shortcut fixes

Port the C++ semantics exactly. **A "pragmatic" shortcut can introduce a NEW
divergence** — e.g. resolving getKey-RYW via a merged `GetRange` + offset index was
verified WRONG on `{orEqual, offset>1}` because it skipped FDB's `removeOrEqual`
normalization and per-segment offset stepping. After any fix, **re-run the
differential + fuzzer** to confirm you didn't trade one divergence for another. If the
faithful port is large, write an RFC (see `rfcs/056`) and get the design reviewed
before implementing.

## Step 6 — pin it (red→green is the proof)

- Add the differential/fuzzer to the suite. It was **red** before the fix and **green**
  after — that transition is the proof. **Never commit it red** (no red CI): capture the
  proof locally, then commit the probe green together with the fix.
- Add a focused regression test (often a client-level unit test over `rywCache`, no FDB)
  and **revert-prove** it: temporarily back out the fix, confirm the test fails, restore.
- If a divergence genuinely can't be closed now (upstream bug, deep architecture,
  multi-shift), DON'T silently drop the probe — document it as an explicit known
  divergence with a `TODO.md` entry + a comment at the call site, and keep a probe that
  records the gap. (E.g. a pending versionstamp reads as ABSENT in Go vs THROWS
  `accessed_unreadable` in C++ — a documented read-side approximation, commit-safe.)

## Step 7 — review gauntlet (these are wire/client changes)

Run the full gate set; client divergences are subtle and reviewers catch different
classes:
- **FDB C++ client developer** (substitute for Graefe on client/wire items) — validates
  the fix against the C++ spec, file:line.
- **Torvalds** — dead code, papered-over regressions, revert-proofs, scope honesty.
- **codex** (`codex -s read-only -a never review --base <sha>`) — the gate that
  repeatedly caught edges the others missed (storage-shadow, cleared-base, sticky-
  unreadable). Do not skip it. Re-review the delta after each fix.
- **@claude** on the GitHub PR — final gate; LGTM must be on the final HEAD.

Re-request every reviewer after a new commit; a stale ACK doesn't cover later commits.

## Hard rules (from CLAUDE.md — non-negotiable)

- **C++ is the spec.** Read it first; port 1:1; no invented shortcuts.
- **No mocks** — real FDB via testcontainers. **No `t.Skip`** except the Docker check.
- **No red CI, no unrelated flakes** — a flake is a real concurrency/ordering bug; root-
  cause it now.
- **Every divergence gets a regression test.** A green suite with the bug still latent is
  the danger. Ask "what dimension was unprobed that let this through?" and pin that axis.
- **Never paper over** a mismatch at the surface (string check, tolerance gate). Fix the
  root cause in the code path the C++ uses.
- **`t.Parallel()` + unique prefixes** on every test; container setup gets a 2-min
  timeout (`context.WithTimeout`), never bare `context.Background()`.
- **`bazelisk`, never `bazel`; never `--no-verify`.** Don't run binding-stress
  concurrently with `just test` (both spin containers).
