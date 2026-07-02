# RFC-174: frl CLI v2 — layered store addressing, scriptable SQL, honest writes

**Status:** Implemented (all six slices, PR #435) — RFC reviews: Graefe ACK + FDB C++ dev ACK +
codex folded; implementation reviews: round-1 findings from all three reviewers fixed (see
Review record), re-review in flight.
**Gate:** Graefe + FDB C++ dev + codex (user-requested reviewer set for this RFC) + Torvalds +
@claude on the implementation PRs. This is **not** a query-engine change — no
Cascades/planner/executor code is touched; the `\explain` feature (§3.3) consumes the existing
SQL `EXPLAIN` surface as a client. If any slice turns out to need planner introspection beyond
what `EXPLAIN` already returns, that slice gets a Graefe review before implementation.
**Review record:**
*Graefe ACK* with 4 conditions, all folded: (G1) catalog metadata resolves via `LoadSchema`
(schema-pinned template version), never `LoadSchemaTemplate` (latest) — §3.1; (G2) read-only
commands open stores with `SetSkipPossiblyRebuild(true)` — §3.1, and the audit exposed this as
a **live v1 bug** now in Slice 0; (G3) `\explain` renders EXPLAIN output verbatim — any
plan-*interpretation* feature (scan-type warnings, index-use detection) is pre-declared a
query-engine change requiring Graefe review before implementation — §3.2; (G4) `record put`'s
SQL-constraint bypass documented in the operator guide — §3.3.
*Codex round 1* (PR #435): three P2 findings, all folded — (P2-1) `meta apply` write-target
specified for Path A contexts (§3.3); (P2-2) `record put` gets the same `--yes`/confirm gate as
delete (§3.3); (P2-3) piped-output test asserts ASCII-only, not just ANSI-free (§5). Delta
re-review requested.
*FDB C++ dev ACK* with 4 conditions, all folded: (C1) `index build` defaults `--max-retries`
to Java's 100 — at the Go builder default of 0 the rps throttle and adaptive limit-halving are
dead code (`online_indexer.go:378-379,886`) — §3.3; (C2) `PartlyBuiltError` surfaced with the
rebuild/takeover remediation, kill-and-resume e2e includes a resume-with-different-method case
— §3.3/§5; (C3) `record delete` treats already-deleted-on-maybe-committed-retry as success —
§3.3; (C4) `fdb up`'s `configure new` retry treats "Database already exists" as success (the
command is not idempotent; a success-then-nonzero-exit currently fails a healthy cluster,
`fdb.go:72-80`) — another **live v1 bug**, now in Slice 0.
*Implementation reviews, round 1* (branch at 1b97970d7): Graefe **ACK w/ 3 conditions**, FDB
C++ dev **ACK w/ 1 condition (C5)**, codex **NAK (2×P1 + 3×P2)** — all seven distinct findings
fixed with regression tests, several proven red→green:
1. `withStore` opened the context's cluster file, ignoring `--cluster-file` (Graefe #1 = codex
   P2) — worst case `index rebuild --cluster-file X` clears the index on the default cluster.
   Fixed to `target.clusterFile()`.
2. `meta apply` TOCTOU (Graefe #2 = C++ dev C5): validated in tx1, prompted, raw-persisted in
   tx2. Root cause was a **library divergence** — Go's `SaveRecordMetaData` was a raw persist
   while Java's `saveRecordMetaData → saveAndSetCurrent` validates in the same transaction.
   Ported Java's semantics into `FDBMetaDataStore` (build check, version-must-increase,
   evolution validator, history archive — all in the caller's tx; `SetEvolutionValidator`
   mirrors Java's setter) and made the CLI re-load + compare-to-confirmed + save in ONE
   transaction, with already-current = no-op success (maybe-committed retry semantics).
   `--allow-no-version-change` removed from `apply` (dead per Java's unconditional hard check;
   stays on offline `evolve-check`).
3. `store lock full-store` was permanent (codex P1): `Open()` rejects the locked store and
   `store unlock` had no bypass. Second library divergence underneath: Go's bypass was a bare
   string (`!= ""`), Java's is `@Nullable` — an empty-reason lock was unbypassable even in
   principle. Bypass is now `*string`; `store lock`/`unlock` arm it with the header's stored
   reason (Java's recovery path). Everything else still refuses fully-locked stores.
4. `record delete --type <typo>` silently fell through to the raw primary key (codex P1) —
   wrong-record-delete hazard. `applyTypePrefix` now errors through `lookupRecordType`.
5. A `keyspace_tuple` context + `--meta-file` fell back to the empty `keyspace_path` (codex
   P2): the adoption guard skipped ALL context addressing when `--meta-file` was set. Metadata
   override no longer discards the tuple; a relational context + `--meta-file` now gets the
   explicit two-metadata-sources error.
6. `describe()` named tuple-addressed targets by the context's `keyspace_path` (codex P2) —
   wrong store named in write confirms, `store truncate` type-back impossible. Tuple targets
   render via `tupleToJSON` (round-trips through `tupleFromJSON`); truncate's gate reads full
   lines so quoted tuple elements are typeable.
7. `record_write.go` overclaimed "the CLI never migrates" (Graefe #3): `index build` hands the
   store to OnlineIndexer, whose opens use the regular check-version path — **Java
   `IndexingBase.openRecordStore` parity, kept**; comment + operator guide now say so.
**Origin:** full assessment of `cmd/frl` (all ~10.7k LOC read; live end-to-end run against a
throwaway FDB via `frl fdb up`; file-by-file sweep; comparison against Java's
`fdb-relational-cli`). Four live bugs, three doc/impl contradictions, one structural gap.
**Effort (honest):** ~4–6 focused shifts across 6 slices. Slice 0 (bugs+docs) is under a shift;
Slice 2 (addressing) is the big one (~1.5 shifts); the rest are a shift or less each.

---

## 1. Problem

`frl` serves the two layers of the system, and that is correct and stays: the **record layer**
(stores addressed by `keyspace_path`, metadata from `meta_file` / `meta_store_keyspace`) and the
**relational layer** (`sql`, `meta catalog`, addressed by `--database`/`--schema`, metadata
auto-discovered from `__SYS/CATALOG`). Two layers in the system → two feature families in the
CLI. Not the problem.

The problem is that the CLI's layering does not match the system's layering. In the system the
layers **stack**: every relational schema *is* a record store — it lives at
`RelationalKeyspace.SchemaSubspace(dbPath, schema)` (`pkg/relational/core/keyspace/keyspace.go:50`)
and its `RecordMetaData` is the catalog template (`frl meta catalog get` already loads and renders
exactly that object, `meta_catalog.go:215`). In the CLI the layers are **siloed**: the
record-layer commands (`record scan/get/count`, `index ls/describe/scan`, `store info/dump`)
cannot address a relational store at all. Verified live, and it is *impossible*, not just
unwired:

- The schema subspace's first tuple element is the literal dbPath **including slashes** —
  `tuple("/demo", "main")` — while `parseKeyspacePath` (`internal/cmd/store.go:155`) splits on
  `/` with no escape syntax. No config can express that keyspace.
- Even if it could, the record commands have no way to use the catalog as their metadata source,
  despite `runCatalogQuery` (`meta_catalog.go:302`) containing the entire code path.

Net effect: seed a store through `frl sql`, and `store dump` — the CLI's killer forensic
primitive — can't look at it. The x-ray machine can't be pointed at the flagship workflow
(`frl fdb up` → `frl sql`). Composition is the whole value of having both layers in one binary;
today there is none.

### Bugs found by driving it (all reproduced live)

1. **Garbage hex in store errors** — `store.go:183,187` format an `fdb.Key` with `%x`, but
   `fdb.Key` implements `String()` (`pkg/fdbgo/fdb/key.go:51`) and `fmt` routes `%x` through
   Stringer. Output: `no store header at keyspace 5c7830326465765c…` — hex of the *escaped
   string* `"\x02dev\x00\x14"`, not the key bytes. The "paste into fdbcli" promise is broken.
2. **ANSI + box-drawing on piped stdout** — `frl sql -c … | anything` emits raw `ESC[90m` and
   `─┼─` (verified with `cat -v`). The comment at `sql.go:817` claims piping is safe; it isn't —
   lipgloss styles render unconditionally, no TTY detection. `-c` is unusable in scripts and
   there's no machine format on `sql` to compensate.
3. **`--Database is required`** — fang's error-banner capitalization garbles the flag name
   (`sql.go:86`). Five other error strings in the tree are hand-reworded to dodge exactly this
   (`meta.go`, `store.go:141,159`, `internal/meta/meta.go:50`); this one slipped through.
4. **Dead pointer** — `README.md:196` points at a "repo-root `TODO.md` section `## frl CLI`"
   that no longer exists.

### Doc/impl contradictions

- `docs/operator-guide.md` FAQ: "Does frl write to FDB? **No — every v1 command is read-only**."
  False — `frl sql` executes arbitrary DDL/DML (the demo seeds 1 000 rows with it). The guide
  never mentions `sql` or `meta catalog` at all.
- Operator guide Path B: "commands work identically" — but `meta get`, `meta types ls/describe`,
  and `index describe` explicitly reject FDB-store metadata sources
  (`ErrFDBStoreNotAvailable`, `internal/meta/meta.go:53`). The `withStore` path already supports
  Path B (`openstore.go:146`); only the FDB-less commands pass `nil` and refuse.
- `proto/frl/config/v1/config.proto:4` and the `internal/config` package doc claim an "env-var
  overlay on top" — never implemented (only `$FRL_CONFIG` path override exists,
  `internal/config/config.go:32`).

### Quality gaps

- **`meta diff` under-compares** — indexes diff by type + expression field names only
  (`meta_diff.go:210-230`); a flipped `unique`, changed `options`, `predicate`, or subspace key
  reports as *identical*. Record types diff by PK fields only (`:171-184`). For a tool sold for
  CI/deploy sanity, a diff that under-reports is worse than no diff. JSON output is name-only —
  the old→new detail is text-only, so `jq` consumers can't see *what* changed (`:276-292`).
- **No CI e2e** — the integration suite (`//go:build integration`) is opt-in, not in Bazel, not
  in `ci.yml`. Zero e2e coverage of `sql` (the largest file, 29 KB, and the only write-capable
  command) and `meta catalog`. By this repo's own standard — *E2E or it's not done* — the CLI's
  e2e net exists but never runs.
- **`version` is dead under Bazel** — rules_go strips `debug.ReadBuildInfo`
  (`version.go:61-63` admits it); the shipped artifact prints `frl unknown (unknown …)`.
- **`config use-context` destroys YAML comments** — Load→mutate→Save round-trips through proto
  (`config.go:385`), deleting the guidance comments `config init` just wrote.
- Duplication: PK-to-string ×5 (`pkFieldsOrUnset` exists in meta_diff.go, unused elsewhere),
  collect-and-sort-map-keys ×6, meta-load-for-completion ×2; ad-hoc `map[string]any` JSON in
  `record count` / `meta validate` / `evolve-check` vs typed row structs everywhere else.
- `record count` surfaces the raw internal error (`recordCountKey is nil`) instead of telling
  the operator what to add to their metadata.

### Reference point

Java's CLI (`fdb-relational-cli`) is a sqlline/JDBC wrapper — `frl` already exceeds it, which is
fine (read-side Go extensions are allowed; nothing here touches the wire). Java's one idea worth
stealing: it wires a **planner debugger** into its REPL (`PlannerDebuggerCommandHandler`).

## 2. Goals / non-goals

**Goals**

1. **Composition**: record-layer commands work on relational stores — the CLI's layering matches
   the system's layering. One binary, three altitudes on the same store: SQL row → index entry →
   raw bytes.
2. **Scriptable SQL**: `frl sql` output safe to pipe, machine formats available.
3. **Honest writes**: retire the read-only fiction; ship the deferred write wave with the
   guardrails the v1 docs promised (confirmation, dry-run).
4. Fix the four bugs, the three doc lies, the `meta diff` false negatives; wire e2e into CI.

**Non-goals**

- Merging the two layers into one command set. `sql`/`meta catalog` stay relational-shaped;
  `record`/`index`/`store` stay record-layer-shaped. Only *addressing* is unified.
- Porting Java's sqlline CLI. `frl` is a Go extension; wire compat is untouched by all of this.
- Directory-layer keyspace support (Go relational uses plain tuple keys by design,
  `keyspace.go:8-13`).
- Any query-engine change (see Gate).

## 3. Design

### 3.1 Layered store addressing (the load-bearing change)

Every store-touching command accepts **either** addressing form:

```
# raw record-layer (today's form, unchanged):
frl record scan --context prod --type Order
frl store dump  --context prod --subspace index

# relational (new — resolves keyspace + metadata from the catalog):
frl record scan --database /demo --schema main --type ORDERS
frl index ls    --database /demo --schema main
frl store dump  --database /demo --schema main --subspace index
frl store info  --database /demo --schema main
```

Resolution for the relational form: keyspace = `RelationalKeyspace.SchemaSubspace(db, schema)`;
metadata = **`RecordLayerStoreCatalog.LoadSchema(txn, db, schema)`** — which resolves the
template **at the version the schema is pinned to** (`fdb_store_catalog.go:214-215`, via
`LoadSchemaTemplateAtVersion`), exactly as the SQL executor does. Not
`SchemaTemplateCatalog().LoadSchemaTemplate` (latest version): latest-version metadata against
an older store header would trip `checkPossiblyRebuild` (`store_builder.go:183`) and make a
"read-only" command **write** — header bump, index clears/rebuild marks (Graefe G1).
Implementation is a third `meta.Source` (`CatalogSource`) plus a keyspace resolver — both slot
into the existing `withStore` plumbing (`openstore.go:121`) without touching the command bodies.

**Read commands must never mutate a store** (Graefe G2). All read-only commands open with
`SetSkipPossiblyRebuild(true)` (`store_builder.go:691`). The audit exposed this as a **live v1
bug**, not just a v2 requirement: today's `withStore` (`openstore.go:159-164`) opens with plain
`.Open()` inside a read-write `rec.Run` transaction, so a `--meta-file` newer than the store
header makes `record scan` rewrite the header and clear/rebuild indexes. Fixed in Slice 0 with
a regression e2e: open a store, present newer metadata, run `record scan`, assert the store
header and index states are byte-identical before/after.

Config gains the same duality. `Context` gets optional `database` + `schema` fields as an
alternative to `keyspace_path` + `metadata` (additive proto change, v1 configs stay valid;
validation rejects setting both). The demo's `keyspace_path: /unused` hack dies.

**Config-free invocation + chainable `fdb up`** (owner addition during review round 2):
`--cluster-file` becomes a flag on `sql` and every store-touching command, overriding the
context's `cluster_file`; combined with `--database/--schema` or `--keyspace`, no config file
is required at all. `frl fdb up` adopts the UNIX stdout contract — human progress moves to
stderr, stdout carries exactly the cluster-file path (`-o json`: `{cluster_file, container,
context}`) — so the two compose into a one-liner:

```
frl sql --cluster-file $(frl fdb up) --database /demo -f schema.sql
```

This also retires the demo's `keyspace_path: /unused` hack and the operator-guide FAQ entry
about missing root-level overrides. Lands in Slice 2 with the rest of the addressing flags.

**Typed keyspace segments** (the `parseKeyspacePath` "left for v2" at `store.go:154`): the
relational case is handled natively above, so slash-in-segment pressure is gone; what remains is
real record-layer apps with non-string tuple elements. Add a JSON-tuple form accepted anywhere a
keyspace is (config `keyspace_tuple`, flag `--keyspace-tuple`):

```yaml
keyspace_tuple: ["myapp", 42, {"uuid": "0195c7…"}, {"bytes_hex": "deadbeef"}]
```

Strings, integers, and tagged objects for uuid/bytes. Unambiguous, greppable, no bespoke escape
grammar. The slash-path form stays as sugar for the all-strings case.

### 3.2 `sql` scriptability

- **TTY detection**: colors and box-drawing only when stdout is a terminal; plain ASCII-aligned
  output otherwise. No format switching on pipe — same table, minus styling. This fixes bug 2.
- **`-o table|csv|json|ndjson`** on `sql` (default `table`). `ndjson` composes with the envelope
  philosophy of `record scan`; `csv` covers the spreadsheet crowd. Row-count/timing footer goes
  to stderr in machine formats so stdout stays clean.
- Fix the `--Database` message (bug 3) — and note the pattern: five hand-rewordings to appease
  fang's banner is the framework fighting us. If a sixth instance appears, replace fang's error
  rendering with a plain formatter rather than rewording a seventh string.
- **REPL parity where cheap**: `\d <table>` (columns + PK from the catalog — the data is already
  loaded for `\d`), `\x` expanded mode, `\timing on|off`.
- **`\explain`**: re-run the previous statement wrapped in `EXPLAIN`, render the plan. One
  meta-command, pure client of the existing EXPLAIN surface — the "did my query use the index?"
  loop without leaving the prompt. (Java-CLI parity in spirit: `PlannerDebuggerCommandHandler`.)
  **Boundary rule (Graefe G3): rendering ≠ interpreting.** `\explain` prints EXPLAIN's output
  verbatim. Any feature that *interprets* plan output — full-scan warnings, "index used"
  detection, plan-shape assertions — would either string-match plan text (forbidden by repo
  rules) or need structured plan output from the engine; both are query-engine changes and go
  to Graefe as an RFC before implementation. None ships in this RFC.

### 3.3 The write wave (honest, guarded)

`sql` already writes; the read-only claim is retired everywhere (root Long, README, operator
guide) in Slice 0. The deferred write set ships with the guardrails v1 promised:

- **`frl index build <name>`** — drive `OnlineIndexerBuilder`/`OnlineIndexer`
  (`pkg/recordlayer/online_indexer.go:208,263`) with progress rendering and the builder's own
  throttling/limit knobs exposed as flags. `index set-state <name> <state>` and
  `index rebuild` (clear + build) round it out. This is the #1 operator task the CLI can see
  today (`index ls` shows `WRITE_ONLY`) but cannot fix. All state-mutating forms require
  `--yes` or an interactive confirm. Two conditions from the FDB C++ dev review:
  - **`--max-retries` defaults to 100 (Java's default), not the Go builder's 0** (C1). At 0,
    the rps throttle and adaptive limit-halving never engage (`online_indexer.go:378-379,886`)
    — a single `transaction_too_large`/`transaction_too_old` escaping the client retry loop
    would abort the whole build with no back-off.
  - **`PartlyBuiltError` is surfaced with its remediation** (C2): resuming with different
    build settings throws it by design (`online_indexer.go:1054-1133`); the CLI renders the
    conflict (existing stamp vs requested) and the escape hatches (`index rebuild` to start
    over, or matching flags to take over) instead of dumping the raw error.
- **`frl meta apply --file <f>`** — runs the `MetaDataEvolutionValidator` gate (same code as
  `evolve-check`, same `--allow-*` flags) and on pass writes via
  `FDBMetaDataStore.SaveRecordMetaData` (`metadata_store.go:36`). Refuses on validation failure.
  Completes the schema-evolution story the operator guide sells but can't finish.
  **Write target (codex P2-1):** the command writes to an `FDBMetaDataStore`, so it requires
  one — either the context's `meta_store_keyspace` (Path B) or an explicit
  `--meta-store-keyspace <path>` flag. A Path A context (`meta_file` only) has no store in FDB
  to apply to; the command errors with exactly that explanation and points at the two options
  (the flag, or the operator-guide section on migrating Path A → Path B). The old metadata for
  the validator diff is read from the same store; `--force-initial` covers the
  first-write-to-an-empty-store case.
- **`frl record put --type T <json>`** (protojson parsed against metadata) and
  **`frl record delete <pk>`** — `--dry-run` prints what would be written/deleted (the dry-run
  store primitives exist, `store_api.go:233,353`); **both** require `--yes` or an interactive
  confirm (codex P2-2: put overwrites existing records and bypasses SQL-level constraints —
  it gets the same gate as delete, not a lighter one). The confirm prompt for an overwriting
  put shows the existing record's PK and type.
  **`record delete` treats already-deleted-on-retry as success** (C3): after a
  maybe-committed retry (`commit_unknown_result` resolved by the client's idempotency
  barrier), re-execution sees the record absent — that's the delete having landed, not a
  failure, and must exit 0. **`record put` bypasses SQL-level constraints** not encoded in
  `RecordMetaData` (Graefe G4) — record-layer index maintenance and uniqueness hold
  transactionally, but relational-only invariants don't; documented prominently in the
  operator guide's write-commands section.
- **`frl store lock|unlock|truncate`** — truncate behind interactive confirm **and** `--yes`,
  never one flag alone.

All writes refuse `--database/--schema` addressing for `meta apply` (catalog-owned metadata must
evolve through SQL DDL — mutating a template behind the relational layer's back corrupts the
catalog contract; the existing `meta catalog` never-write rule, `meta_catalog.go:43`, extends to
the whole write wave: **`__SYS/CATALOG` is never a write target**). `record put/delete` and
`index build` against relational stores are fine — they go through the same record-layer APIs
the relational executor itself uses.

### 3.4 Surface cleanup

- **`frl status`** (new): one shot — cluster reachable (GRV), store header present for the
  active context, catalog present/absent, metadata source loadable. The "is everything wired?"
  command. `tx read-version` stays (bare-integer output is script-friendly) but its smoke-check
  framing moves to `status`.
- **Drop `config schema`** — an empty-proto dump; `config init`'s commented template is the real
  discovery surface.
- **`meta diff` compares full protos** — unique/options/predicate/subspace key/added-version for
  indexes; since-version/type-key/PK for types — and emits symmetric JSON with `{field, old,
  new}` per change.
- **Path B completion** — `meta get`, `meta types ls/describe`, `index describe` accept
  FDB-store metadata sources (pass the db handle + resolver instead of `nil`,
  `openstore.go:80-93`).
- **`version` via `-ldflags -X`** (Bazel `x_defs` + goreleaser/`go install` fallback to
  `ReadBuildInfo`).
- **`config use-context` preserves comments** — edit the YAML AST for the single
  `current_context` scalar instead of round-tripping through proto.
- Consolidate: one `formatPrimaryKeyFields` helper, one `sortedKeys` helper, typed JSON row
  structs for `record count` / `meta validate` / `evolve-check`; remap the `record count`
  not-enabled error to "metadata has no record_count_key — add one to enable counting".
- Doc truth pass: operator guide gains `sql` + `meta catalog` sections, Path B support matrix,
  honest read/write table; README's TODO pointer fixed; the unimplemented "env-var overlay"
  claim deleted from config.proto and the package doc (or implemented — deleting is fine, the
  `$FRL_CONFIG` path override covers the real use case).

## 4. Implementation plan

Sliced so every slice merges green and independently useful. E2E harness lands *before* the big
slices so they arrive with a net.

- **Slice 0 — bugs + truth (≈ 1 shift):** fix the **six** live bugs, each with a regression
  test: (1) `%x` on `fdb.Key` hex garbage; (2) ANSI-on-pipe (asserted by scanning captured
  output for `\x1b`); (3) the `--Database` message; (4) README pointer; (5) **read commands
  can mutate stores** — `withStore` opens without `SetSkipPossiblyRebuild(true)` inside a RW
  transaction, so newer `--meta-file` metadata rewrites the store header from `record scan`
  (Graefe G2; e2e: stale-store + newer metadata + scan → header/index states byte-identical);
  (6) **`fdb up` configure retry fails healthy clusters** — `configure new` is not idempotent,
  a success-then-nonzero-exit makes every retry return "Database already exists" until the
  15-attempt loop fails (FDB C++ dev C4; treat that response as success). Plus
  operator-guide/README/config.proto truth pass and `meta diff` full-proto comparison +
  symmetric JSON (correctness bugs, not features).
- **Slice 1 — CI e2e (1 shift):** wire the integration suite into CI (Docker is available there —
  the rest of the repo's FDB tests prove it): either Bazel-ize with the testcontainer pattern the
  repo already uses, or a `just frl-e2e` step in `ci.yml`. Add e2e coverage for `sql`
  (schema/seed/query/tx/meta-commands, `-c`, `-f`, stdin) and `meta catalog`.
- **Slice 2 — layered addressing (~1.5 shifts):** `CatalogSource`, `--database/--schema` on all
  store-touching commands, config `database`/`schema` fields, `keyspace_tuple` typed form,
  `--cluster-file` override + `fdb up` stdout contract (config-free chaining, §3.1),
  demo updated to show the round-trip (seed via `sql`, inspect via `record scan`/`store dump`).
  The e2e pin: create through SQL, read the same rows through `record scan`, dump the same
  index entries through `index scan`.
- **Slice 3 — sql scriptability (1 shift):** TTY detection, `-o` formats, `\d <table>`, `\x`,
  `\timing`, `\explain`.
- **Slice 4 — write wave (1–1.5 shifts):** `index build/rebuild/set-state` first (highest
  operator value), then `meta apply`, then `record put/delete` + `store lock/unlock/truncate`.
  Each lands with its guardrails and e2e (including: build a WRITE_ONLY index to READABLE and
  prove `index scan` sees the entries; `meta apply` refuses an invalid evolution).
- **Slice 5 — cleanup (< 1 shift):** `frl status`, drop `config schema`, Path B completion,
  version stamping, comment-preserving `use-context`, helper dedupe, JSON-shape consistency.

## 5. Test plan

- Slice 1's CI wiring is the foundation; every subsequent slice adds to the integration suite.
- Addressing (S2): cross-layer round-trip test is the headline pin (SQL-created store readable
  via record/index/store commands with both flag and config addressing; mutual-exclusion
  validation errors).
- Scriptability (S3): piped table output contains zero `\x1b` bytes **and zero non-ASCII
  bytes** (codex P2-3 — the ANSI check alone would pass with `─┼─` box-drawing still present;
  off-TTY tables must be plain ASCII per §3.2); `-o ndjson` parses line-by-line with
  `encoding/json`; footer-on-stderr in machine formats.
- Diff (S4→S0): matrix test flipping each compared attribute (unique, options, predicate,
  subspace key, since-version, type-key) — each must surface in text *and* JSON.
- Writes (S4): dry-run writes nothing (assert via `store dump` before/after); confirm gates
  refuse without `--yes`; `__SYS/CATALOG` write-target rejection; kill-and-resume `index
  build` **including a resume-with-different-method case** asserting the rendered
  `PartlyBuiltError` remediation (C2); `record delete` idempotent-retry semantics (C3).
- Unit tests stay FDB-free as today; everything touching a store goes through the
  testcontainer suite per repo rules (`t.Parallel()`, 2-minute container timeouts).

## 6. Risks

- **Writes against production stores.** Mitigated by confirm + `--yes` double gates, dry-run
  defaults where the primitive exists, and the hard `__SYS/CATALOG` never-write rule. `meta
  apply` is gated behind the same validator Java uses — the CLI cannot apply an evolution the
  library would reject.
- **Catalog coupling.** `CatalogSource` reads the same subspace the sqldriver writes
  (`meta_catalog.go:317-322` already navigated this once — there's a pinned test). If the
  relational keyspace scheme changes, one resolver changes with it.
- **`keyspace_tuple` syntax churn.** Additive config fields only; the v1 slash-path form is
  untouched. Tagged-object JSON avoids inventing an escape grammar we'd have to support forever.
- **Long-running `index build` in a CLI process.** The `OnlineIndexer` owns transaction batching
  and retry; the CLI only renders progress. Kill-safety verified against the client
  (FDB C++ dev review): per-range work — index entries, range-set insert, progress counter —
  commits in one transaction (`online_indexer.go:1214-1335`); the pure-Go client implements
  the C++ idempotency barrier for `commit_unknown_result` (`client/commitpath.go:24,106`,
  matching `NativeAPI.actor.cpp:6731-6773`), and the retry re-reads `FirstMissingRange` at a
  fresh read version, so a landed ambiguous commit is skipped — no double-count even for
  non-idempotent COUNT/SUM index types. Pin with a kill-and-resume e2e (§5).
- **fang.** If error-message rewording count hits six, replace the error renderer. Tracked in
  §3.2; not a blocker for any slice.
