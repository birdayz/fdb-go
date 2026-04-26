# plandiff — cross-engine plan + result-set diff harness

Two parallel Go-vs-Java diff harnesses for fdb-relational SQL:

| Surface | Type | Java step | Go interface | Diff function |
|---------|------|-----------|--------------|---------------|
| Plan tree (EXPLAIN) | `Engine` / `PlanResult` | `planSql` | `goEngine` / `javaEngine` | `Run` → `Report` |
| Result set (executeQuery) | `Runner` / `RunResult` | `runSql` | `goRunner` / `javaRunner` | `RunCorpus` → `RunReport` |
| Result set with setup DML | `SetupRunner.RunWithSetup` | `runWithSetup` | `javaRunner.RunWithSetup` | `RunCorpusWithSetup` → `RunReport` |

The Java side is shared infrastructure (`conformance/sql_plan_steps.java`).
Go side is in this package.

The Go runner returns `ErrGoUnimplemented` on every call until Track C2
(`QueryExecutor`) lands. Today's diff runs are effectively Java-only
baselines; the harness shape is ready to flip to real cross-engine
comparison the moment the Go runner has a real implementation.

## Adding a corpus entry

`SeedRunCorpus` (in `corpus.go`) is the result-set diff corpus. Each entry
is a `RunQuery` with:

```go
{
    Name:           "snake_case_unique_name",
    SchemaTemplate: "CREATE TABLE T (...) PRIMARY KEY (...)",
    SetupSqls:      []string{"INSERT INTO T VALUES (...)", ...},
    Query:          "SELECT ... ORDER BY ...",
    Expected: RowSet{
        Columns: []Column{{Name: "ID", Type: "BIGINT"}, ...},
        Rows:    [][]any{{float64(1), "alice"}, ...},
    },
},
```

### Capturing `Expected` for a new entry

Don't hand-write the expected `RowSet` — let the Java side produce it.
Add the entry without `Expected`, run the conformance test, dump the actual
RowSet, paste it back as `Expected`:

```go
// Temporarily in run_sql_conformance_test.go inside the test loop:
bs, _ := json.Marshal(c.Java.Rows)
GinkgoWriter.Printf("CORPUS[%s] = %s\n", c.Query.Name, string(bs))
```

```sh
bazelisk test //conformance:conformance_test \
  --test_arg="--ginkgo.focus=SeedRunCorpus" \
  --test_output=streamed --test_timeout=300 \
  --nocache_test_results \
  --test_arg="--ginkgo.v" 2>&1 | grep CORPUS
```

Translate the JSON to Go: `42` → `float64(42)` (encoding/json's default
for JSON numbers — `int` literals would fail equality), `null` → `nil`,
`true`/`false` → `true`/`false`, `"text"` → `"text"`.

### When fdb-relational rejects your SQL

Real failures fall into categories — see `CLAUDE.md` "Java↔Go conformance
gotchas (fdb-relational)" first. Common ones surfaced by this harness:

- `RelationalException: NOT NULL is only allowed for ARRAY column type` — drop
  `NOT NULL` from BIGINT/STRING/etc. PK columns are implicitly NOT NULL.
- `RelationalException: No Schema specified` — the runWithEphemeralSchema
  helper handles this internally; if you see it, the bug is in your test
  shape, not the harness.
- `UnableToPlanException` — Cascades planner can't plan that shape yet.
  Examples: `GROUP BY <col>`, `SELECT DISTINCT`. Don't add such corpus
  entries until the planner ports the rule.
- `LIMIT clause is not supported` — `LIMIT N` is JDBC-only
  (`Statement.setMaxRows`), not SQL syntax in fdb-relational.

If your shape DOES work but ordering is non-deterministic, add `ORDER BY
<pk>` so the Expected rows compare deterministically across runs.

## Running the harness locally

```sh
# All RunSql Harness specs:
bazelisk test //conformance:conformance_test \
  --test_arg="--ginkgo.focus=RunSql Harness" \
  --test_output=streamed --test_timeout=300 \
  --nocache_test_results

# Just the corpus driver:
bazelisk test //conformance:conformance_test \
  --test_arg="--ginkgo.focus=SeedRunCorpus" \
  --test_output=streamed --test_timeout=300 \
  --nocache_test_results

# Plan-tree harness:
bazelisk test //conformance:conformance_test \
  --test_arg="--ginkgo.focus=Plan Equivalence" \
  --test_output=streamed --test_timeout=300 \
  --nocache_test_results

# Pure unit tests (no FDB / Java needed):
bazelisk test //pkg/relational/conformance/plandiff:plandiff_test \
  --test_output=streamed
```

## When something breaks

A per-entry failure looks like:

```
[FAILED] corpus entry "single_row_bigint": row data diverged
    <[][]interface {} | len:1, cap:1>: [[<float64>42]]
    <[][]interface {} | len:1, cap:1>: [[<float64>99]]
```

The first line shows WHICH entry, the assertion message says WHAT diverged
(columns vs rows), and the Gomega diff shows expected (top) and actual
(bottom). Update `Expected` if the change is intentional; otherwise track
down what regressed in fdb-relational, our schema/setup DDL, or our
encoder.

The plan-tree harness uses a corpus-wide hash (`HashCorpus` in
`plandiff.go`) because plan trees are large free text — pinning them
verbatim per entry would be unmanageable. The result-set harness uses
per-entry RowSets because they're small and the diff diagnostic is the
whole value-add.

## Adding a new Java step

If you need a new shape that runSql/runWithSetup don't cover:

1. Add `@ConformanceStep("yourStep")` method to `conformance/sql_plan_steps.java`.
   Reuse `runWithEphemeralSchema(clusterFile, schemaTemplate, op)` for the
   DDL lifecycle.
2. In Go, write a method on `javaRunner` (or `javaEngine` for plan-tree
   shapes) that calls `invokeStep(ctx, r.httpClient, r.baseURL, "yourStep",
   params, &out)`. The helper in `httpclient.go` handles all HTTP
   plumbing + JSON parsing + exception-class-aware error wrapping.
3. Add httptest unit tests mirroring the runSql / runWithSetup ones.

## See also

- `rfcs/026-fdb-grpc-trajectory.md` — why we don't worry about gRPC.
- `CLAUDE.md` "Java↔Go conformance gotchas (fdb-relational)" — fdb-
  relational integration quirks.
- `TODO.md` Track A1 — context on what this harness is for.
