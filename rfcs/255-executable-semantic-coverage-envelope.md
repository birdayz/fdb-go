# RFC-255: Executable semantic coverage envelope

Status: implemented first QSC numeric vertical slice; design and implementation ACKed.

## Decision and measured starting point

Extend the existing yamsql runner, not the production planner or a new SQL runner.
`factory.FeatureVector` intentionally omits literal values; its shape census therefore
cannot establish boundary-value coverage. `yamsql.diffRows` deliberately allows numeric
cross-carrier equality and equates the two zero signs. `TestSubstituteParams` previously
asserted a formatted fractional literal without reparsing its type (RFC-254). Those are
different instruments; this change adds an explicit semantic denominator and the exact
assertions required to exercise its first cells without weakening existing coverage.

This milestone covers the registered scalar catalog inventory and ONE finite envelope:
FLOOR, CEIL, CEILING, ROUND(x) (no precision argument), POWER/POW (exponent three), with DOUBLE
inputs in five classes (negative zero, positive zero, negative fraction -0.25, positive
fraction +0.25, whole positive 3.0), through literal, driver-text-transport, and stored-column producers,
consumed by projection. The required Cartesian product is 6 x 5 x 3 = 90 cells.
Other registrations remain explicitly unclassified; other types, functions, consumers,
precision arguments, nesting, lifecycle modes and non-finite inputs remain outside this
envelope. This is not completion of all QSC-01/QSC-02 obligations or universal SQL coverage.

Read Java 4.12.11.0 SqlFunctionCatalogImpl.java and yamltests QueryExecutor.java in full.
The former does not register these math spellings: they are Go extensions. The latter
binds actual parameter objects through prepared statements and can check metadata before
consuming rows. Go's harness passes typed arguments through database/sql to the existing
driver text transport; it does not reproduce Java binding or implement interpolation itself.
The manifest contains hand-derived IEEE hex answers for four operators x five input
classes, shared by CEIL/CEILING and POWER/POW aliases. These constants are the oracle,
not calls to the same math functions used by production. Basis: exact arithmetic for
these finite values, IEEE zero signs, and nearest-integer rounding at non-ties.
The four operator answer vectors must be pairwise distinct (aliases excluded): on +0.25,
FLOOR and ROUND return +0, CEIL returns 1, and POWER returns 0.015625.
Alias spellings count separately. The driver-text-transport axis tests actual API
arguments being formatted/reparsed by substituteParams, NOT executor WithParams binding;
that executor-binding input route remains missing from this SQL-driver envelope.
Omitted values include +/-0.5 ties, fractions above 0.5 in magnitude, negative whole numbers and
subnormals; no claim about those domains follows from this envelope.

## Existing-runner additions

Add optional `args`, `exact_rows`, and `column_types` to yamsql.Test. Existing `rows`
keeps its existing numerical comparison policy. `exact_rows` is mutually exclusive with
`rows` (including explicitly empty rows) and checks concrete driver carriers and exact
IEEE bits; it does not normalize the actual result. Use a pointer presence type: an explicit empty exact row set is
an assertion, distinct from an absent field. `column_types` checks database/sql's declared
SQL type names independently of carriers. Ordered comparison is the default; unordered
exact rows compare typed multisets, preserving duplicate counts.

Use one strict tagged scalar codec for arguments and exact expectations: null, int64
(decimal), float64 (16 hexadecimal IEEE bits), string, bool and bytes (hex). Unknown kinds,
malformed encodings, null with a payload, ignored assertion combinations and unknown YAML
fields are errors. FLOAT's common float64 driver carrier is distinct from SQL FLOAT metadata;
this first envelope requires DOUBLE metadata. The codec adds no new SQL admission policy:
the driver retains its canonical NaN/Infinity CAST transport and its rejection of
unrepresentable NaN payloads; the harness does not override either.

Pass decoded args to QueryContext, ExecContext, error-path execution and EXPLAIN. Validate
programmatically constructed tests too, so direct Run callers cannot bypass loader checks.
Reject result/metadata/plan assertions on error tests and query-only assertions on DML,
rather than silently ignoring them. Pin these paths through real FDB, not mock drivers.

## Executable manifest and observations

Keep a versioned JSON manifest at
`pkg/relational/conformance/yamsql/testdata/semantic/numeric-v1.json`. It records the
scalar catalog spelling inventory, envelope identity, required functions/producers/value
classes, oracle policy and explicitly outside-envelope scope. Read names from a small read-only accessor returning a fresh sorted copy of the live
scalar catalog, pinned against the private runtime map by its existing internal test.
No Go source parsing. Fail on empty inventory, duplicate manifest names, catalog drift,
and removal of a required axis member. A test pins all
90 IDs as a literal list independent of generator axis constants and the manifest;
changing JSON or shared constants cannot shrink it. This additive
metadata accessor is the only production API change, with no evaluator behavior change.

Expand the finite manifest deterministically into ordinary yamsql scenarios with typed
arguments, exact expected rows and DOUBLE metadata. Commit the generated YAML under a
subdirectory (not the root corpus glob), with an up-to-date guard that asserts the expected non-empty file count under Bazel, and execute it exactly
once from a dedicated existing-target test. The docscheck star-body census also walks
this directory recursively; these explicit projections add no SELECT-star cases. Add 15 producer-only prerequisites (3 x 5),
and project input plus function output in each math cell so coincident function answers
cannot hide a swapped argument or wrong source row. Both columns require DOUBLE metadata.
Thus 105 statements validate 90 math cells plus their 15 producer prerequisites. Round-trip the generated scenarios
through the normal strict loader, then execute through the existing runner over real FDB.
In test code (not the yamsql library), validate producer witnesses against typed ANTLR nodes of the actual submitted statement
and the decoded argument vector, in addition to the exact projected input. A placeholder
node witnesses driver-text-transport, never a runtime-bound engine value. Reject producer
labels inconsistent with the statement. This is not SQL substring detection. Stable cell IDs retain function,
producer and value class; signed/whole/fractional cases cannot deduplicate together.

Coverage starts at not-exercised. Only actual successful exact-row AND metadata checks
for the expected generated cell AND its producer prerequisite yield validated evidence. The runner records one explicit success/failure outcome per attempted statement and
binds the live result to a digest of the executed scenario (setup, queries, typed args,
assertions). Completeness is checked in the same real-FDB run, never from a committed
"validated" report. The private live outcome records prevent a caller from manufacturing
a passing run using only the existing exported aggregate counters; absent failure entries
alone are not pass evidence. No external evidence store or replay of stale success records. Failed setup, partial/missing results, duplicate/unknown cell IDs, edited assertions
and a successful ordinary numerical-row test cannot manufacture validation. Reports keep
required/exercised/validated counts and per-cell statuses separate, name the envelope and
catalog population, and expose registrations not covered by this envelope. A completeness
check fails on any required cell without valid execution evidence. No claims about plan
alternatives, runtime-vs-folding dispatch, caches or continuations are made in this slice.

## Verification and delivery

- Unit tests independently drive inventory drift, required-axis removal, duplicates,
  malformed manifests, codec boundaries, ignored assertions, empty exact sets, typed
  multiset comparison, missing/wrong/stale execution evidence and incomplete reports.
  Pin short outcome slices, mismatched scenario digests, and absent failures without
  matching success records. Pin pairwise-distinct operator answer vectors.
- Real-FDB generated 90-cell / 105-statement execution and YAML round-trip through the existing runner;
  positive and failing assertions, bound Query/Exec/error/EXPLAIN routes are tested.
- Mutate zero-sign comparison, argument transport, ROUND dispatch redirected to CEIL, and the manifest denominator; verify
  applied/compiled mutants fail the intended assertions, then restore and run green.
- Fuzz the tagged codec and exact comparator against independent typed expectations.
- Gazelle, module tidy, uncached affected Bazel targets, just test and milestone reviews.
  This changes conformance instrumentation only, not planner/executor cost or behavior;
  a million-row timing comparison does not price this test-harness change.
- Record the measured envelope and remaining unclassified scope in TODO.md, leaving the
  broad QSC items open. LLM batches will consume this manifest/report in QSC-06; no LLM
  service is necessary to run or validate this first deterministic slice.

## Measured mutation evidence (2026-09-14)

Parent revision: `6a1404528`. The tested working-tree population is the catalog
implementation/test and all yamsql files (tracked and untracked), excluding this RFC,
TODO and the pre-existing skill edits. Its SHA-256 inventory digest is
`f3d2c7210bde708e4692dcb38b647ea2b941fb1fc0eec3885cfbfc56ab71d65e`, produced by:

```sh
git ls-files -co --exclude-standard -- \
  pkg/recordlayer/query/plan/cascades/values/scalar_function_catalog.go \
  pkg/recordlayer/query/plan/cascades/values/scalar_function_catalog_test.go \
  pkg/relational/conformance/yamsql | LC_ALL=C sort -u | xargs sha256sum | sha256sum
```

For each edit below, the old text matched exactly once, the new text was verified
present before building, the named Bazel test executed and failed (not a compile
failure), then the original file was restored. The entire inventory was checked
against its pre-mutation hashes afterwards. Restored uncached yamsql and values
Bazel targets both passed.

| Mutant (exact change) | Test and observed failure |
|---|---|
| `exactValueEqual` float64 arm: `math.Float64bits(w) == math.Float64bits(g)` → `w == g` | `TestExactRowsRepresentationAndMultiplicity`: zero-sign, non-final-zero, duplicate-loss and ordered controls failed. |
| `runTest`: `db.QueryContext(ctx, t.Query, args...)` → `db.QueryContext(ctx, t.Query)` | `TestExactRunnerFDB`: test index 1 failed DOUBLE metadata, actual INTEGER. |
| `checkPlanAssertions`: `db.QueryContext(ctx, "EXPLAIN "+query, args...)` → `db.QueryContext(ctx, "EXPLAIN "+query)` | `TestExactRunnerFDB`: index 1 failed `Project([(3 / 2)]`; observed `Project([(?1 / 2)], Scan(T))`. |
| `scalarFunctionCatalog["ROUND"]`: `scalarFunctionRound` → `scalarFunctionCeil` | `TestNumericEnvelopeFDB`: exactly ROUND/{literal,driver-text-transport,stored-column}/positive-quarter failed at the 90-math-cell population. |
| Remove `"CEILING"` from the manifest `functions` array | `TestNumericEnvelopeArtifact`: `required axes changed`. |

The test-only producer witness additionally has negative pins for wrong spellings,
duplicated projections, exponent type/value/arity, integer-vs-DOUBLE literals, extra
projections, parameter shape/count/value, arguments on a literal, wrong column names
and predicated/computed producers. A shared report calculation supplies both the
credit verdict and printed per-cell statuses; appended/removed/duplicate/unknown
cells and invalid witnesses cannot produce validated output or panic the reporter.

Fuzzing passed 1,250,386 all-kind round-trip/multiset executions and 14,094,619
float-bit executions, 15 seconds each, without coverage guidance. It discovered
and pinned yaml.v3's newline-only block-scalar loss: quoted Scalar payloads preserve
the exact string. Arbitrary bytes retain their own tag; invalid UTF-8 string payloads
are rejected. Exact error-message checks also replaced two previously ignored
assertions in the existing NOT NULL corpus. Explicit empty ordinary rows on DML
are now rejected, and the existing DML corpus omits those ignored fields.

Final verification: both affected Bazel targets executed uncached and passed; the
final `just test` reported 92 targets passing (3 executed, 89 cached). The tested
file hashes remained unchanged across that run. Graefe and Torvalds ACKed the
implementation; Codex's implementation findings are fixed and regression-pinned.
The inventory command pins byte-order sorting explicitly: an en_US.UTF-8 sort
produces a different aggregate from C sorting despite identical per-file hashes.
