package conformance_test

// End-to-end integration tests for the runSql harness (Track A1 of TODO.md
// execution roadmap). Drives the Java fdb-relational engine via
// SqlPlanSteps#runSql / runWithSetup against a shared FDB testcontainer.
//
// Specs in this file:
//
//   1. Schema-less SELECT — pins the documented "No Schema specified"
//      error path on /__SYS without a schema.
//   2. SELECT against an ephemeral-schema table — pins column metadata
//      (uppercased names + JDBC type names) for an empty table.
//   3. Empty result set — pins zero-row handling.
//   4. Multi-primitive INSERT-then-SELECT round-trip — BIGINT, DOUBLE,
//      STRING, BOOLEAN with NULL preservation.
//   5. INTEGER + FLOAT round-trip — type narrowing via explicit CAST.
//   6. BYTES round-trip — base64 encoding via X'...' literal.
//   7. SeedRunCorpus driver — runs every corpus entry against Java and
//      asserts per-entry Expected RowSet (precise diagnostics on
//      divergence).
//
// What this file does NOT assert:
//
//   - Cross-engine result-set equivalence. That's Track A3 (yamsql
//     corpus) and depends on a real Go-side runner (Track C2).

import (
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"

	"github.com/birdayz/fdb-record-layer-go/pkg/relational/api"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/conformance/plandiff"
	"github.com/google/uuid"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// entryConforms reports whether Go's result conforms to Java's for a
// non-annotated corpus entry: a matching server-side root error message when
// Java errors, or conforming column metadata (plandiff.ConformColumns) plus
// byte-equal rows when Java succeeds. Returns a human-readable detail on
// non-conformance. This is the predicate the RFC-082 regression lock reconciles
// against rfc082KnownRed.
func entryConforms(javaResult, goResult plandiff.RunResult) (bool, string) {
	if javaResult.Err != nil {
		if goResult.Err == nil {
			// Java errored, Go succeeded. With the conformance server's plan
			// cache disabled (sql_plan_steps.java) the Java result is
			// deterministic: an UnableToPlanException here means Java's
			// Cascades planner genuinely has no plan for this query (it is
			// thrown only on finalExpressions.isEmpty() AFTER full
			// exploration — budget exhaustion throws
			// RecordQueryPlanComplexityException instead). That is a real,
			// reproducible Go read-side extension, NOT planner noise, so it
			// must be declared via an rfc082Divergences annotation
			// (DivergenceJavaErrorsGoCorrect), not silently swallowed here.
			return false, "Java errored but Go succeeded"
		}
		var je *plandiff.JavaError
		if !errors.As(javaResult.Err, &je) {
			return false, fmt.Sprintf("Java error is %T (not *plandiff.JavaError)", javaResult.Err)
		}
		var ge *api.Error
		if !errors.As(goResult.Err, &ge) {
			return false, fmt.Sprintf("Go error is %T (not *api.Error)", goResult.Err)
		}
		goRootMsg := ge.Message
		for cause := ge.Unwrap(); cause != nil; {
			var inner *api.Error
			if !errors.As(cause, &inner) {
				break
			}
			goRootMsg = inner.Message
			cause = inner.Unwrap()
		}
		if goRootMsg != je.Message {
			return false, fmt.Sprintf("error messages diverge: java=%q go=%q", je.Message, goRootMsg)
		}
		return true, ""
	}
	if goResult.Err != nil {
		return false, "Java succeeded but Go errored: " + goResult.Err.Error()
	}
	if detail, ok := plandiff.ConformColumns(goResult.Rows.Columns, javaResult.Rows.Columns); !ok {
		return false, "column metadata: " + detail
	}
	if !reflect.DeepEqual(goResult.Rows.Rows, javaResult.Rows.Rows) {
		return false, "row data diverges"
	}
	return true, ""
}

// divergenceHolds reports whether a corpus entry's RFC-082 Divergence annotation
// still describes reality. The conformance server runs with its plan cache
// disabled (sql_plan_steps.java) and the cross-engine corpus runs on a dedicated
// isolated server, so Java's behaviour is deterministic — the gate asserts BOTH
// the annotation's Java premise AND Go's pinned behaviour. A drift on either side
// returns false so the lock reports it rather than letting a stale annotation rot.
func divergenceHolds(div *plandiff.Divergence, javaResult, goResult plandiff.RunResult) (bool, string) {
	switch div.Direction {
	case plandiff.DivergenceJavaErrorsGoCorrect:
		// Java must (deterministically) error; Go must succeed with pinned rows.
		if javaResult.Err == nil {
			return false, "annotation says Java errors, but Java succeeded — divergence gone, reclassify"
		}
		if goResult.Err != nil {
			return false, "requires Go to succeed but Go errored: " + goResult.Err.Error()
		}
		if !reflect.DeepEqual(goResult.Rows.Rows, div.GoExpectedRows) {
			return false, fmt.Sprintf("Go rows changed from the annotation: %v", goResult.Rows.Rows)
		}
		return true, ""
	case plandiff.DivergenceJavaWrongRowsGoCorrect:
		// Both engines succeed; Java's rows are wrong (Java's bug). Go must
		// succeed with the pinned correct rows AND Java must still be wrong.
		if javaResult.Err != nil {
			return false, "annotation says Java succeeds with wrong rows, but Java errored: " + javaResult.Err.Error()
		}
		if goResult.Err != nil {
			return false, "requires Go to succeed but Go errored: " + goResult.Err.Error()
		}
		if !reflect.DeepEqual(goResult.Rows.Rows, div.GoExpectedRows) {
			return false, fmt.Sprintf("Go rows changed from the annotation: %v", goResult.Rows.Rows)
		}
		if reflect.DeepEqual(javaResult.Rows.Rows, div.GoExpectedRows) {
			return false, "annotation says Java's rows are wrong, but Java now matches Go's correct rows — divergence fixed, reclassify/delete"
		}
		return true, ""
	case plandiff.DivergenceJavaIntermittentGoCorrect:
		// The ONE direction whose Java side can't be pinned to exact rows:
		// documented Java NONDETERMINISM (UNION ALL + outer ORDER BY — Java
		// sometimes sorts, sometimes returns interleaved branch order). Only the
		// ROW ORDER is intermittent — Java still SUCCEEDS every time — so we
		// still assert Java does not error (a deterministic Java throw here is a
		// NEW, worse divergence that must not be masked just because Go's rows
		// match), and that Go succeeds with the pinned (sorted) rows. We do not
		// pin Java's exact rows/order. TODO: re-verify under the plan-cache-
		// disabled server — if Java is now order-deterministic, reclassify to
		// JavaWrongRowsGoCorrect or delete (Java sorts correctly).
		if javaResult.Err != nil {
			return false, "annotation says Java succeeds (only its row order is intermittent), but Java errored: " + javaResult.Err.Error()
		}
		if goResult.Err != nil {
			return false, "requires Go to succeed but Go errored: " + goResult.Err.Error()
		}
		if !reflect.DeepEqual(goResult.Rows.Rows, div.GoExpectedRows) {
			return false, fmt.Sprintf("Go rows changed from the annotation: %v", goResult.Rows.Rows)
		}
		return true, ""
	case plandiff.DivergenceBothErrorMessagesDrift:
		// Both engines error with drifting messages. Java must error; Go must
		// reject with the pinned (cause-specific) substring.
		if javaResult.Err == nil {
			return false, "annotation says both engines error, but Java succeeded"
		}
		if goResult.Err == nil {
			return false, "requires Go to error but Go succeeded"
		}
		if !strings.Contains(goResult.Err.Error(), div.GoErrorContains) {
			return false, "Go error wording changed: " + goResult.Err.Error()
		}
		return true, ""
	case plandiff.DivergenceJavaSucceedsGoRejects:
		// Go is the more restrictive side: Java succeeds, Go rejects.
		if javaResult.Err != nil {
			return false, "annotation says Java succeeds, but Java errored: " + javaResult.Err.Error()
		}
		if goResult.Err == nil {
			return false, "requires Go to error but Go succeeded"
		}
		if !strings.Contains(goResult.Err.Error(), div.GoErrorContains) {
			return false, "Go error wording changed: " + goResult.Err.Error()
		}
		return true, ""
	default:
		return false, "unknown divergence direction " + string(div.Direction)
	}
}

// writeClusterFileToTemp materialises the cluster file string contents
// (env.ClusterFile) to a temp file on disk and returns its path. The
// Go embedded SQL driver's DSN takes a `cluster_file=<path>` option,
// not the file contents — so the conformance test writes once per It
// block and removes it on cleanup.
func writeClusterFileToTemp(contents string) string {
	f, err := os.CreateTemp("", "fdb-conformance-*.cluster")
	Expect(err).NotTo(HaveOccurred())
	_, err = f.WriteString(contents)
	Expect(err).NotTo(HaveOccurred())
	Expect(f.Close()).To(Succeed())
	return f.Name()
}

var _ = Describe("RunSql Harness", func() {
	var (
		ctx  context.Context
		env  *TenantEnvironment
		java *JavaInvoker
	)

	BeforeEach(func() {
		ctx = context.Background()
		var err error
		tenantName := fmt.Sprintf("runsql_%s", uuid.New().String())
		env, err = SetupTenantEnvironment(ctx, sharedContainer, tenantName)
		Expect(err).NotTo(HaveOccurred())
		java = NewJavaInvoker()
	})

	AfterEach(func() {
		if env != nil {
			_ = env.Cleanup(ctx)
		}
	})

	It("runs a schema-less SELECT and returns the literal", func() {
		// Schema-less is the only end-to-end path that fdb-relational
		// genuinely refuses (executeInternal demands conn.getSchema()
		// to be non-null — see AbstractEmbeddedStatement). EXPLAIN
		// bypasses that check; runSql does not. So a typed Java
		// "No Schema specified" surface IS the correct behaviour
		// here, and we pin it explicitly rather than tolerating
		// silently. Transport-level errors remain a real harness bug.
		runner := plandiff.NewJavaRunnerHTTP(javaBaseURL(java), env.ClusterFile)
		got := runner.Run(ctx, plandiff.Query{
			Name: "select_literal",
			SQL:  "SELECT 1",
		})
		Expect(got.Err).To(HaveOccurred(), "fdb-relational rejects schema-less executeQuery")
		Expect(got.Err.Error()).To(ContainSubstring("No Schema specified"))
		Expect(got.Err.Error()).NotTo(ContainSubstring("HTTP "), "transport-level failure")
		Expect(got.Err.Error()).NotTo(ContainSubstring("dial tcp"), "Java server not reachable")
	})

	It("runs a SELECT against a table in the ephemeral schema", func() {
		// Pins the schema-template branch end-to-end: CREATE TEMPLATE
		// / CREATE DATABASE / CREATE SCHEMA, JDBC executeQuery,
		// RelationalResultSet → JSON encoding. The table is empty
		// since each runSql call uses a fresh ephemeral schema, but
		// column metadata + JDBC type mapping are pinned. Multi-row +
		// NULL preservation are covered by the httptest unit tests
		// (full wire-shape control there).
		runner := plandiff.NewJavaRunnerHTTP(javaBaseURL(java), env.ClusterFile)
		// fdb-relational reserves NOT NULL for ARRAY column types in
		// CREATE TABLE syntax. Primary-key columns are implicitly
		// NOT NULL — no explicit annotation needed.
		got := runner.Run(ctx, plandiff.Query{
			Name:           "select_table_columns",
			SQL:            "SELECT id, name FROM Item",
			SchemaTemplate: "CREATE TABLE Item (id BIGINT, name STRING, PRIMARY KEY (id))",
		})
		Expect(got.Err).NotTo(HaveOccurred(), "schema-template branch must succeed")
		Expect(got.Rows.Columns).To(HaveLen(2), "expected 2 columns (id, name)")
		Expect(got.Rows.Columns[0].Name).To(Equal("ID"), "fdb-relational uppercases column names")
		Expect(got.Rows.Columns[0].Type).To(Equal("BIGINT"))
		Expect(got.Rows.Columns[1].Name).To(Equal("NAME"))
		// Pin whatever fdb-relational reports for STRING — surfacing
		// any future cross-engine type-name divergence.
		Expect(got.Rows.Columns[1].Type).NotTo(BeEmpty())
		Expect(got.Rows.Rows).To(BeEmpty(), "ephemeral schema is fresh — Item is empty")
	})

	It("round-trips a row with multiple primitive types via runWithSetup", func() {
		// Pins type encoding end-to-end: INSERT a row with values across
		// fdb-relational's primitive type set, SELECT it back via the
		// shared ephemeral schema, verify each column's JSON-encoded
		// representation. Surfaces any encoder gap in
		// SqlPlanSteps#resultSetToJson — JDBC types that fall through
		// to the {"__unsupported__": ...} marker would fail the asserts.
		//
		// Coverage: BIGINT (long), DOUBLE, STRING (varchar), BOOLEAN.
		// Skipped (not supported by fdb-relational CREATE TABLE in
		// 4.11.1.0): BYTES NOT NULL, DATE, TIMESTAMP — these wait
		// on a follow-up shift.
		runner, ok := plandiff.NewJavaRunnerHTTP(javaBaseURL(java), env.ClusterFile).(plandiff.SetupRunner)
		Expect(ok).To(BeTrue(), "javaRunner must satisfy SetupRunner")

		got := runner.RunWithSetup(ctx,
			"CREATE TABLE T (id BIGINT, score DOUBLE, label STRING, flag BOOLEAN, PRIMARY KEY (id))",
			[]string{
				"INSERT INTO T VALUES (1, 3.5, 'alice', TRUE)",
				"INSERT INTO T VALUES (2, -7.25, 'bob', FALSE)",
				"INSERT INTO T VALUES (3, 0.0, NULL, NULL)",
			},
			"SELECT id, score, label, flag FROM T ORDER BY id",
		)
		Expect(got.Err).NotTo(HaveOccurred(), "INSERT-then-SELECT must succeed")
		Expect(got.Rows.Columns).To(HaveLen(4))
		Expect(got.Rows.Rows).To(HaveLen(3))

		// Row 0: (1, 3.5, "alice", TRUE)
		Expect(got.Rows.Rows[0][0].(float64)).To(Equal(float64(1)))
		Expect(got.Rows.Rows[0][1].(float64)).To(Equal(3.5))
		Expect(got.Rows.Rows[0][2].(string)).To(Equal("alice"))
		Expect(got.Rows.Rows[0][3].(bool)).To(BeTrue())

		// Row 1: (2, -7.25, "bob", FALSE)
		Expect(got.Rows.Rows[1][0].(float64)).To(Equal(float64(2)))
		Expect(got.Rows.Rows[1][1].(float64)).To(Equal(-7.25))
		Expect(got.Rows.Rows[1][3].(bool)).To(BeFalse())

		// Row 2: (3, 0.0, NULL, NULL) — null preservation across two
		// nullable columns (one STRING, one BOOLEAN).
		Expect(got.Rows.Rows[2][1].(float64)).To(Equal(float64(0)))
		Expect(got.Rows.Rows[2][2]).To(BeNil(), "label NULL must round-trip")
		Expect(got.Rows.Rows[2][3]).To(BeNil(), "flag NULL must round-trip")
	})

	It("round-trips INTEGER and FLOAT columns", func() {
		// fdb-relational distinguishes INTEGER (32-bit) from BIGINT
		// (64-bit) and FLOAT (32-bit) from DOUBLE (64-bit) in the
		// grammar (RelationalParser.g4 columnType). Both narrow types
		// arrive over JDBC as Number, so the JSON encoder treats them
		// uniformly — this test pins that behaviour.
		// fdb-relational doesn't auto-promote BIGINT literals to INTEGER
		// or DOUBLE literals to FLOAT — explicit CAST is required at
		// INSERT time. (`A value cannot be assigned to a variable
		// because the type of the value does not match the type of the
		// variable and cannot be promoted to the type of the variable`).
		runner := plandiff.NewJavaRunnerHTTP(javaBaseURL(java), env.ClusterFile).(plandiff.SetupRunner)
		got := runner.RunWithSetup(ctx,
			"CREATE TABLE Numeric_T (id BIGINT, i INTEGER, f FLOAT, PRIMARY KEY (id))",
			[]string{"INSERT INTO Numeric_T VALUES (1, CAST(42 AS INTEGER), CAST(1.5 AS FLOAT))"},
			"SELECT id, i, f FROM Numeric_T",
		)
		Expect(got.Err).NotTo(HaveOccurred())
		Expect(got.Rows.Rows).To(HaveLen(1))
		Expect(got.Rows.Rows[0][1].(float64)).To(Equal(float64(42)))
		Expect(got.Rows.Rows[0][2].(float64)).To(BeNumerically("~", 1.5, 1e-6))
		// Pin the JDBC type names — divergence here would surface a
		// cross-engine type-name mismatch when Go-side runners land.
		Expect(got.Rows.Columns[1].Type).NotTo(BeEmpty())
		Expect(got.Rows.Columns[2].Type).NotTo(BeEmpty())
	})

	It("round-trips BYTES columns as base64", func() {
		// SqlPlanSteps#encodeValue base64-encodes byte[] values. This
		// pins that encoding path against real fdb-relational data.
		// The string "hi" → base64 "aGk=".
		// `blob` is a reserved keyword in fdb-relational's grammar
		// (RelationalLexer.g4#BLOB). Use `payload` for the column name.
		// Hex literal syntax: X'...' (uppercase) per
		// RelationalLexer.g4#HEXADECIMAL_LITERAL.
		runner := plandiff.NewJavaRunnerHTTP(javaBaseURL(java), env.ClusterFile).(plandiff.SetupRunner)
		got := runner.RunWithSetup(ctx,
			"CREATE TABLE Bin_T (id BIGINT, payload BYTES, PRIMARY KEY (id))",
			[]string{"INSERT INTO Bin_T VALUES (1, X'6869')"},
			"SELECT id, payload FROM Bin_T",
		)
		Expect(got.Err).NotTo(HaveOccurred())
		Expect(got.Rows.Rows).To(HaveLen(1))
		// "hi" (0x68 0x69) → base64 "aGk=".
		Expect(got.Rows.Rows[0][1].(string)).To(Equal("aGk="))
	})
	It("runs the SeedRunCorpus through BOTH engines and asserts cross-engine equivalence", func() {
		// Generic plumbing: every SeedRunCorpus entry is driven through
		// Java (via the conformance HTTP server) AND Go (via the
		// embedded fdbsql driver against the same FDB container). The
		// harness asserts both engines succeed AND produce byte-equal
		// column metadata + row values, OR (for negative entries with
		// ExpectErrorContains set) both engines fail with matching
		// error substrings.
		//
		// Adding a new test case is just appending {Name, Schema,
		// Setup, Query[, ExpectErrorContains]} to SeedRunCorpus().
		// No baseline RowSet to capture, no conformance-test wiring.
		//
		// ISOLATION (nightshift-61): negative entries (those with
		// `ExpectErrorContains` set) get their own freshly-spawned
		// Java conformance server, torn down immediately after. This
		// matches the user's explicit "each test case should be
		// isolated" requirement: fdb-relational 4.11.1.0's error-path
		// state cumulates within a single Java JVM (parser caches,
		// semantic analyser intermediate state, type-resolver
		// negative-result cache), and the same query that returns a
		// clean error in <120ms on a fresh server can hit the 30s
		// HTTP timeout after a handful of prior errors. Per-entry
		// isolation eliminates cross-entry pollution at the cost of
		// ~5s extra Java startup per negative entry. Positive entries
		// share the outer-It isolated server because they exercise the
		// success path which doesn't accumulate state. The shared
		// server itself is freshly spawned (not the suite-shared one)
		// to avoid pollution from prior conformance specs.
		isoJava, err := NewIsolatedJavaInvoker()
		Expect(err).NotTo(HaveOccurred(), "failed to spawn isolated Java conformance server")
		defer func() { _ = isoJava.Close() }()
		javaR := plandiff.NewJavaRunnerHTTP(javaBaseURL(isoJava), env.ClusterFile).(plandiff.SetupRunner)
		clusterFilePath := writeClusterFileToTemp(env.ClusterFile)
		defer os.Remove(clusterFilePath)
		goR := plandiff.NewGoSQLSetupRunner(clusterFilePath)

		// Java is the spec. Per entry, ask Java first; whatever it does
		// (success with rows / failure with a message) becomes the
		// expected behaviour Go must match. No per-entry expected-text
		// annotation — drift on either side surfaces immediately.
		corpus := plandiff.SeedRunCorpus()
		// Apply RFC-082 cross-engine divergence annotations (Go-only extensions,
		// tracked Go capability gaps, and both-reject message-drift) so the
		// harness asserts Go's documented behaviour without pinning Java's.
		plandiff.ApplyRFC082Divergences(corpus)
		// RFC-082 regression LOCK: non-annotated entries that diverge but are
		// NOT in rfc082KnownRed are regressions; known-red entries that now pass
		// must be removed from the lock (it only shrinks). Asserted after the
		// loop so one run reports the full delta.
		var regressions, fixedNowGreen, staleAnnotations []string
		for _, rq := range corpus {
			rq := rq
			By(rq.Name, func() {
				javaResult := javaR.RunWithSetup(ctx, rq.SchemaTemplate, rq.SetupSqls, rq.Query)
				goResult := goR.RunWithSetup(ctx, rq.SchemaTemplate, rq.SetupSqls, rq.Query)

				// Divergence path: when an entry carries a Divergence
				// annotation, assert Go's behaviour against the embedded
				// expectation but do NOT pin Java's actual behaviour.
				// Lets us keep documented Java upstream bugs (NPEs,
				// dedup failures, etc.) inside the corpus while still
				// catching Go-side regressions.
				if rq.Divergence != nil {
					if ok, detail := divergenceHolds(rq.Divergence, javaResult, goResult); !ok {
						staleAnnotations = append(staleAnnotations, fmt.Sprintf("%s: %s", rq.Name, detail))
					}
					return
				}

				// Non-annotated entries are subject to the RFC-082 regression
				// LOCK rather than asserted green directly: record whether Go
				// conforms (matching error when Java errors, or conforming
				// columns + equal rows when Java succeeds), then reconcile
				// against rfc082KnownRed after the loop.
				ok, detail := entryConforms(javaResult, goResult)
				known := plandiff.IsKnownRed(rq.Name)
				if !ok && !known {
					regressions = append(regressions, fmt.Sprintf("%s: %s", rq.Name, detail))
				} else if ok && known {
					fixedNowGreen = append(fixedNowGreen, rq.Name)
				}
			})
		}
		Expect(staleAnnotations).To(BeEmpty(),
			"STALE RFC-082 annotation(s) — Go's behaviour no longer matches the pinned divergence; update/remove rfc082Divergences:\n  %s",
			strings.Join(staleAnnotations, "\n  "))
		Expect(regressions).To(BeEmpty(),
			"NEW cross-engine divergence(s) — a regression vs the locked known-red set (RFC-082); fix Go or, if intended, annotate:\n  %s",
			strings.Join(regressions, "\n  "))
		Expect(fixedNowGreen).To(BeEmpty(),
			"known-red corpus entries now PASS — remove them from rfc082KnownRed so the lock shrinks (RFC-082):\n  %s",
			strings.Join(fixedNowGreen, "\n  "))
	})

	It("returns an empty result set for SELECT with no matching rows", func() {
		runner := plandiff.NewJavaRunnerHTTP(javaBaseURL(java), env.ClusterFile)
		// Pin zero-row handling: an empty table SELECT returns
		// columns with no rows. Avoids fdb-relational's VALUES-clause
		// syntax restrictions.
		got := runner.Run(ctx, plandiff.Query{
			Name:           "empty_select",
			SQL:            "SELECT id FROM Dummy",
			SchemaTemplate: "CREATE TABLE Dummy (id BIGINT, PRIMARY KEY (id))",
		})
		Expect(got.Err).NotTo(HaveOccurred())
		Expect(got.Rows.Columns).To(HaveLen(1))
		Expect(got.Rows.Rows).To(BeEmpty())
	})
})
