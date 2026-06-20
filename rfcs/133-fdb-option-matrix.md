# RFC-133 — Public FDB client option honored/unsupported/no-op matrix

**Status:** Implemented (PR #331 — FDB C++ dev + Torvalds + codex + @claude ACK; CI green)
**Item:** prod-readiness-audit-2026-06-19.md **P2** — "FDB Option Semantics Need A Public
Honored/Unsupported/No-Op Matrix."
**Reviewers:** FDB C++ client developer (the classification is a `libfdb_c` 7.3.75 spec call — their
ACK is required) + Torvalds + codex + @claude. Client/wire-adjacent → **fdb-client-review gate**.

---

## 1. Problem (verified against C++ 7.3.75)

`pkg/fdbgo/fdb/options.go` exposes ~50 `Set*` option methods. Some are honored, some return
`UnsupportedOptionError`, and many `return nil` while doing nothing. The audit's concern: a user can
"mistakenly believe a libfdb option is active when the pure-Go backend ignores it" — **the contract
is not visible.**

The audit *also* recommended "convert any unsafe silent no-op into `UnsupportedOptionError`." I
verified every option against the C++ 7.3.75 source (`/tmp/fdbsrc`; `Transaction::setOption`
@ `NativeAPI.actor.cpp:6948`, `ReadYourWritesTransaction::setOptionImpl` @ `ReadYourWrites.actor.cpp:2534`,
`DatabaseContext::setOption` @ `NativeAPI.actor.cpp:2114`). **Finding: no conversion is needed —
every *actually-unsafe* option already errors.** The silent no-ops split cleanly into safe buckets:

- **Already `UnsupportedOptionError`** (ignoring would silently grant/deny access, auth, idempotency,
  quota, or a conflict read-back the caller relies on): `report_conflicting_keys` (Go has no
  read-back), `raw_access`, `automatic_idempotency`, `bypass_storage_quota`, `authorization_token`,
  and their DB-default twins. **This is the dangerous family, and it already fails loudly.**
- **No-op in `libfdb_c` too** — Go matches C *exactly* (both hit `default: break`, zero GRV/commit
  references; several are marked Deprecated in `fdb.options`): `causal_read_disable`,
  `durability_risky`, `durability_datacenter`, `durability_dev_null_is_web_scale`. Erroring on these
  would be *more* divergent than the no-op.
- **Fail-safe (Go keeps the STRONGER guarantee)**: `causal_write_risky` and the DB-level
  `transaction_causal_read_risky` default — C *weakens* a guarantee (commit dedup-on-fault / causal
  consistency); Go ignoring them keeps the stronger one. Never "actually wrong"; converting them to
  errors would reject a legal, strictly-safer call.
- **Telemetry / perf-hints / locality / resource-caps / tools-only / unmodeled subsystems**: debug
  identifiers, trace/log options, tags, read-priority + server-side-cache hints, GRV-cache hints
  (Go's default matches), location-cache size, max-watches, datacenter/machine id, special-key-space
  + read-ahead + used-during-commit-protection (subsystems Go doesn't model). None can change query
  results, durability, isolation, conflict ranges, or key visibility.

So the *real* gap is **documentation + a regression guard**, not behaviour. The asymmetry — Go
**errors** on the access/auth/idempotency/quota/conflict-readback family but **accepts-and-ignores**
the causal/durability-*weakening* family — is the deliberate, correct design; it just isn't written
down or pinned by a test.

## 2. Change

1. **`pkg/fdbgo/fdb/OPTIONS.md`** — the public matrix, next to `options.go` so it's discoverable. One
   row per `Set*` method, columns the audit asked for: **option · pure-Go behavior · libfdb_c behavior
   (C++ file:line) · unsafe if ignored? · why safe / why it errors · test**. A header states the
   design rule: *Go errors on options whose silent omission would change access/auth/idempotency/
   quota/conflict-visibility; it accepts-and-ignores options that are pure hints/telemetry, no-ops in
   libfdb_c too, or strictly-safer-when-ignored (durability/causal weakening).* `README.md` /
   `DIVERGENCES.md` link to it.

2. **`options_contract_test.go`** (real FDB integration test where a backing tx is needed, plus pure
   unit assertions): the load-bearing **regression guard** —
   - the **unsafe family** (`report_conflicting_keys`, `raw_access`, `automatic_idempotency`,
     `bypass_storage_quota`, `authorization_token`, + DB twins) returns `*UnsupportedOptionError`
     (`errors.As`). If a refactor ever makes one of these silently `return nil`, this goes red — the
     exact "user thinks it's active but it's ignored" failure, for the *dangerous* options.
   - **one** honored option wired (`SetTimeout` or `SetSizeLimit`) — proves the honored path takes
     effect; not an enumeration of all honored options (Torvalds: padding).
   - a **completeness guard** (pure unit, name-presence only — Torvalds): enumerate every `Set*`
     method name on `goTransactionOptions` / `DatabaseOptions` (AST walk over options.go) and assert
     each name token appears in `OPTIONS.md`. Adding an option without a matrix row fails CI — the
     matrix cannot silently rot as options are added. It does **not** parse columns or validate C++
     citations (fragile); name-presence is the load-bearing minimum.

   **OPTIONS.md is self-guarded by this completeness test** — it is deliberately NOT added to the
   RFC-131/132 `docs_consistency` living-docs set (it carries no version pins to anchor; its anti-rot
   mechanism is "every option has a row," which the completeness guard enforces).

3. **Production-code change** (the review gauntlet caught it; per §5 it lands here, not a follow-up).
   **Three** *database-level* defaults were mis-classified as safe — each changes read semantics on
   every new transaction in `libfdb_c` and has an **honored per-tx form**, so the DB default must
   propagate, not be dropped: `SetSnapshotRywDisable`/`Enable` (snapshot-read-after-own-write),
   `SetTransactionBypassUnreadable` (`accessed_unreadable`→read), and `SetTransactionCausalReadRisky`
   (GRV read-version relaxation; FDB-C-dev review). Fix: **honor** them via `txDefaults` (the faithful
   match — C honors them, doesn't error) applied in `applyTxDefaults`, exactly like the existing
   honored DB defaults (`SetTransactionTimeout`/`SetReadSystemKeys`). snapshot-RYW is a **cumulative
   counter** (libfdb_c `snapshotRywEnabled++/--`, `NativeAPI.actor.cpp:2156/2160`, seeded per-tx) —
   `enable+disable` nets to enabled — not a last-wins bool (codex; settled against the C++ source).
   bypass-unreadable / causal-read-risky are set-once flags. Pinned by `options_dbdefault_test.go`
   (incl. the counter semantics). An exhaustive 20-option sweep (FDB-C-dev) confirmed no 4th drop.
   The rest of the verification holds: access/auth/quota already errors; the causal/durability
   *weakening* knobs whose per-tx form is a fail-safe no-op (`causal_write_risky`) stay
   accept-and-ignore. The unsafe-no-op→error part of the audit was already done.

## 3. Executable spec (tests)

- Unsafe family → `UnsupportedOptionError` (revert-proven: make one `return nil` → red).
- Completeness: a `Set*` method missing from `OPTIONS.md` → red.
- A spot-check that an honored option (`SetTimeout`/`SetSizeLimit`) changes observable tx state.

## 4. Wire/behaviour impact

**None.** Documentation + tests over existing, C++-verified behaviour. No persisted bytes, no option
semantics change.

## 5. Scope

One PR: `OPTIONS.md` + `options_contract_test.go`. If the FDB-C-dev review finds an option I
mis-classified as safe that is *actually* unsafe, that conversion lands in the same PR with its row +
test (DFS, not a follow-up).
