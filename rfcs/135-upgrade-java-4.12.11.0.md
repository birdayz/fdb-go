# RFC-135 — Upgrade Java compatibility target to fdb-record-layer 4.12.11.0

**Status:** Draft
**Item:** Compatibility-target bump 4.11.1.0 → 4.12.11.0 (handoff
`/tmp/claude-code-handoff.md`). Tracked in TODO.md.
**Reviewers:** Graefe (the synced `record_query_plan.proto` is a Cascades plan-serialization
contract artifact — even though Go does not marshal plans through it, Graefe gates any plan-proto
schema move) + Torvalds (code/process quality) + codex + @claude.

**Gate scope.** *This RFC's deliverable is the mechanical bump only* — version pins, proto sync,
regen, docs. It touches **no planner algorithm, no cost model, no matching/executor logic**: the only
query-engine-adjacent artifact is regenerated plan-proto descriptors (dead code on the Go side, see
§3). The behavioural 4.12 parity items (§4) are **out of scope here** — each is its own follow-up RFC
carrying its own Graefe gate. Bundling them would violate "one logical change per PR" and jam an
un-reviewed planner change into a version bump (the exact PR-#201 failure CLAUDE.md calls out).

---

## 1. Problem (verified)

The port pins Java `fdb-record-layer-core` / `fdb-relational-{api,core}` at **4.11.1.0** in
`MODULE.bazel`, and copies Apple's proto schemas from a `4.11.1.0` submodule checkout into
`proto/apple/`. Upstream's current `4.x.y.0` line is **4.12.11.0**. We want the build, the Java
conformance server, and the proto schemas to track 4.12.11.0 so Go↔Java wire/format compat is
asserted against the version teams actually run, and so the 4.12 SQL/metadata/indexer features can be
ported on top of an aligned base.

The storage format version did **not** change: Java `FormatVersion` still tops out at
`FULL_STORE_LOCK(14)`, matching Go's `formatVersionCurrent`. FDB stays `7.1.26` / `apiVersion=710`
upstream — **this is not an FDB wire-protocol bump.** So the bump is purely Java-artifact + proto-schema.

## 2. Investigation (Java 4.11.1.0 .. 4.12.11.0)

**Maven:** all three artifacts publish at 4.12.11.0 (verified each `.pom` is `200`). Submodule tag
`4.12.11.0` fetched and checked out (`257aa83ca`).

**Proto delta** — exactly two core files differ; `fdb-relational-core` protos are unchanged:

- `record_metadata.proto` — **comment-only** change on `omit_unsplit_record_suffix` (no schema
  change; confirmed the regenerated `record_metadata.pb.go` diff is pure Go doc-comment).
- `record_query_plan.proto` — real schema moves:
  - removes `message PVersionValue`; **reserves** `PValue` oneof tag `38`;
  - replaces `PExistsPredicate` (tag 4) with `PExistentialValuePredicate { PValuePredicate super = 1 }`;
  - adds `PExistsValue.value = 3` (additive); adds `PRecordQueryExplodePlan.with_ordinality = 2`
    (additive).

**Behavioural deltas** (upstream release notes + source) — SQL/planner: LEFT/RIGHT OUTER JOIN, EXISTS
in the projection list, `AT ordinality` array unnest, `CARDINALITY()` (incl. in index definitions),
boolean-simplification / null / outer-join fixes, plan-hash stabilisation. Metadata evolution:
`allowFieldRenames` / `allowDeprecatedFieldRenames` / `allowUndeprecatingFields` + `RenameFieldsVisitor`.
Indexing: clear indexing metadata after indexes are readable, typed-record build-range preset,
sliding-window cleanup/admission fixes. These are **out of scope for this RFC** — see §4.

## 3. The bump (this RFC's deliverable) + safety analysis

1. `MODULE.bazel` — all three artifacts → `4.12.11.0`; `bazel mod tidy` (no lock file; resolution is
   build-time).
2. `proto/apple/{record_metadata,record_query_plan}.proto` ← submodule 4.12.11.0 (verbatim copy; no
   local-only content drift remains).
3. Regenerate Go proto code (`rm -rf gen/ && buf generate && gazelle`). Result: only
   `record_metadata.pb.go`, `record_query_plan.pb.go`, `record_query_plan_vtproto.pb.go` change.
   `PVersionValue`/`PExistsPredicate` refs → **0**; `PExistentialValuePredicate` present;
   `PExistsValue.GetValue` + `GetWithOrdinality` present.
4. Prose — **version-target** references flip now: `CLAUDE.md` (source tag), `TODO.md` header,
   `NOTICE`, `README`, `RELEASE`, `SECURITY`, `PRODUCTION_READINESS`, `CHANGELOG`, `DIVERGENCES.md`.
   The RFC-131 doc-consistency guard (`pkg/docscheck`, `TestLivingDocsCiteCurrentJavaTarget`) forces
   *every* 4-part version in a **living doc** (README, PRODUCTION_READINESS, TODO, DIVERGENCES,
   CHANGELOG, RELEASE) to equal the MODULE.bazel pin — so the bump *requires* flipping their labels,
   and a stale one goes red (this is the intended bump mechanism). `DIVERGENCES.md` is a living doc,
   so its version *label* flips with the pin — but its *behavioural* content (which features Java
   rejects) is still 4.11-validated, so it carries an explicit **"4.12.11.0 rebaseline in progress
   (§4 R8)"** banner rather than a silent relabel that would over-claim. **`SQL_CONFORMANCE.md`,
   `CASCADES_DIVERGENCE.md`, and the conformance `*.go` limitation comments are NOT living docs and
   are NOT flipped here** — their behavioural reclassification is the §4 R8 conformance rebaseline,
   done from a live 4.12.11.0 conformance run.

**Why the proto removals are safe on the Go side (load-bearing claim):** Go **does not serialize query
plans through `record_query_plan.proto`.** Evidence: no hand-written Go references `PRecordQueryPlan`,
nor the oneof wrappers `PValue_VersionValue` / `PQueryPredicate_ExistsPredicate` (every hit is in
`gen/`). Go's `VersionValue` / `ExistsPredicate` are in-memory Cascades constructs that never round-trip
to these messages. So removing the messages drops **dead generated code** — zero functional impact, no
call-site repair. `go build ./...` is clean (the lone `cmd/fdb-diff-oracle` "func main undeclared" error
is pre-existing — identical files on master — that package is C++/Bazel-driven, not `go build`-built).

**No persisted-data / wire impact.** These protos are query-time plan structures, not stored bytes.
Continuations use a different proto (the magic-`6773487359078157740` wrapper), unaffected. Even Java
4.12 no longer emits the removed messages, and the reserved tag makes any stray 4.11-written tag-38 byte
silently ignored — Java's own backward-compat handling, not Go's concern.

## 3.5 Forced conformance reclassification (the bump's only behavioural edit)

Pointing the Java conformance server at 4.12.11.0 makes the cross-engine SeedRunCorpus run against 4.12,
and its RFC-082 regression LOCK (`run_sql_conformance_test.go:515`) immediately reported **7 now-stale
divergence annotations** — Go-vs-Java divergences pinned against 4.11 that 4.12 lifted. The bump cannot
be green (the conformance test is a pre-commit gate) without reclassifying exactly these 7, so this
**minimal slice of R8 is forced into the bump PR**. Each is a real, verified 4.12 behavioural change:

- **4 Java correctness bugs 4.12 fixed** → annotation removed, entry now runs as plain cross-engine
  equivalence (both engines return the same correct rows): `pk_literal_eq_in_join` and
  `three_way_join_shared_driver` (4.11 dropped an ANDed join predicate, returning inflated counts;
  4.12 applies both); `agg_empty_count_having_passes` and `having_count_star_eq_zero_empty` (4.11
  skipped the empty-table implicit group under HAVING, returning 0 rows instead of `[0]`).
- **2 `JavaErrorsGoCorrect` lifted** → Java 4.12 now succeeds with Go's rows, annotation removed →
  plain equivalence: `left_outer_join_basic` (4.12 added LEFT OUTER JOIN) and
  `where_case_returns_bool_probe` (4.12 accepts a CASE-bool WHERE predicate).
- **1 reclassified** `BothErrorMessagesDrift` → `JavaSucceedsGoRejects`: `bare_bool_where_rejected` —
  Java 4.12 now plans a bare-boolean WHERE predicate; **Go still rejects** it ("Cascades planner could
  not plan query"), so it is now a tracked Go capability gap, not a both-error case. (Full boolean-WHERE
  / LEFT-RIGHT-join *plan* parity remains §4 R3/R4/R7 — this only re-pins the row-level cross-engine
  contract.)

Stability: the Java conformance server is proven deterministic (`java_planner_warmth_proof_test.go`,
RFC-082 — no cold-plan nondeterminism). The reclassified gate ran **green 3× consecutively**
(`--nocache_test_results`). An initial run showed `bare_bool_where_rejected`'s Java side erroring once
under that run's heavier failing-assertion load (the documented server state-leak/latency-wedge), not a
planner flake — it is deterministically Java-succeeds once the server is healthy. The broader R8
(full corpus re-sweep, `SQL_CONFORMANCE.md` / `CASCADES_DIVERGENCE.md` flips) stays deferred.

## 4. Parity roadmap (each = its own follow-up RFC)

Out of scope here; landed sequentially after the bump, one at a time. Query-engine items carry a
**Graefe ACK gate** (`query-engine` skill); record-layer items use the generic Torvalds + codex + @claude
gate. **Each is verified against real Java 4.12 first** — a "parity" item that Java 4.12 doesn't actually
support becomes an allowed Go-extension (deep tests, wire compat held), not a divergence, per CLAUDE.md.

| # | Item | Java ref (4.12.11.0) | Gate |
|---|---|---|---|
| R1 | Metadata-evolution field renames (`allow{Field,DeprecatedFieldRenames,Undeprecating}` + `RenameFieldsVisitor`) | `MetaDataEvolutionValidator`, `RenameFieldsVisitor` | Torvalds + codex + @claude |
| R2 | Indexer 4.12 changes (clear-metadata-after-readable, typed-record range preset, sliding-window fixes) | `IndexingBase/Common/Subspaces`, `OnlineIndexOperationConfig`, `SlidingWindowIndexMaintainer` | Torvalds + codex + @claude |
| R3 | Parser grammar: `AT ordinality` table source, `functionNameKeyword` in `scalarFunctionName` | `RelationalParser.g4` | **Graefe** + Torvalds |
| R4 | EXISTS in projection list (`PExistsValue.value`) | exists-value planning | **Graefe** + Torvalds |
| R5 | `AT ordinality` array unnest (`PRecordQueryExplodePlan.with_ordinality`) | explode-plan ordinality | **Graefe** + Torvalds |
| R6 | `CARDINALITY()` function + index support | cardinality value/index | **Graefe** + Torvalds |
| R7 | LEFT/RIGHT OUTER JOIN reclassification (verify Go-extension vs Java-now-supported) + boolean/null/outer-join fixes | relational join planning | **Graefe** + Torvalds |
| R8 | Conformance rebaseline: reclassify cross-engine specs/comments encoding lifted 4.11 limits; flip `SQL_CONFORMANCE.md` / `CASCADES_DIVERGENCE.md` from real 4.12 behaviour; clear the `DIVERGENCES.md` banner. **Partial in this PR** — the 7 RFC-082 annotations 4.12 lifted (§3.5) were forced green by the conformance gate; the full corpus re-sweep + doc flips remain | cross-engine harness | Torvalds + codex + @claude |

## 5. Wire / behaviour impact

**The bump:** none on persisted bytes, options, or SQL semantics. The plan-proto *descriptors* now match
Java 4.12 (schema alignment only; Go marshals nothing through them). The §4 ports each carry their own
impact analysis in their own RFC.

## 6. Test plan

- `go build ./...` clean except the pre-existing `cmd/fdb-diff-oracle` case (shown identical on master).
- **Conformance canary** — `//conformance:conformance_test`. This is *the* signal that proto sync is
  correct: a stale `proto/apple/*.proto` vs the 4.12 Maven jar surfaces as `NoSuchFieldError:
  …P*_descriptor` from an fdb-relational-core static initializer, then cascades into HTTP timeouts.
  Green ⇒ our compiled `libapple_proto-speed.jar` matches the 4.12 jar. **Verified green** (no
  `NoSuchFieldError`); the only failure it surfaced was the 7 stale RFC-082 annotations (§3.5),
  now reclassified — **green 3× consecutively** under `--nocache_test_results`.
- `just test` full suite green (regen touched only proto output; no behavioural code changed).
- **Plan-proto schema sentinel** (Graefe's nit) — `pkg/docscheck.TestPlanProtoSchemaMatches412`
  reflects the regenerated `gen` descriptors and asserts: `PValue` tag 38 reserved (no
  `version_value`); `PQueryPredicate` tag 4 = `existential_value_predicate` → `PExistentialValuePredicate`
  (no `exists_predicate`); `PExistsValue.value` = tag 3; `PRecordQueryExplodePlan.with_ordinality` =
  tag 2. Lives in `pkg/docscheck` (not `gen/`, which `just generate` wipes); a regen against an older
  proto or any schema drift goes red. Passing.
- RFC-131 doc-consistency guard (`pkg/docscheck`) green — every 4-part version in a living doc now
  equals the 4.12.11.0 pin.

## 7. Scope

**One PR:** pins + `mod tidy` + the two synced protos + regenerated `gen/*.pb.go` + version-target prose
+ this RFC + TODO roadmap rows. The §4 behavioural ports are **separate, sequential RFCs** — this RFC
establishes the aligned base and the gated plan; it does not undertake the feature work.
