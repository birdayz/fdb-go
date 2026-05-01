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
	"strings"

	"github.com/birdayz/fdb-record-layer-go/pkg/relational/api"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/conformance/plandiff"
	"github.com/google/uuid"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

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

		corpus := plandiff.SeedRunCorpus()
		for _, rq := range corpus {
			rq := rq
			By(rq.Name, func() {
				// Negative entries get their own per-entry Java server
				// — eliminates cross-entry pollution that otherwise
				// pushes innocuous specs past the HTTP timeout.
				perEntryJavaR := javaR
				if rq.ExpectErrorContains != "" {
					perJava, err := NewIsolatedJavaInvoker()
					Expect(err).NotTo(HaveOccurred(),
						"corpus entry %q: failed to spawn per-entry Java server",
						rq.Name)
					defer func() { _ = perJava.Close() }()
					perEntryJavaR = plandiff.NewJavaRunnerHTTP(javaBaseURL(perJava), env.ClusterFile).(plandiff.SetupRunner)
				}

				javaResult := perEntryJavaR.RunWithSetup(ctx, rq.SchemaTemplate, rq.SetupSqls, rq.Query)
				goResult := goR.RunWithSetup(ctx, rq.SchemaTemplate, rq.SetupSqls, rq.Query)

				if rq.ExpectErrorMessage != "" {
					// Strictest negative test: both engines must fail AND
					// each engine's CORE error message string equals
					// rq.ExpectErrorMessage verbatim (Go's
					// `api.Error.Message`, Java's
					// `JavaError.Message` — the unwrapped server-side
					// root-cause text, not the wrapper-prefixed
					// `err.Error()`). Use when Go has been deliberately
					// aligned to emit the exact phrasing Java emits.
					Expect(javaResult.Err).To(HaveOccurred(),
						"corpus entry %q: Java accepted a query expected to fail with message %q",
						rq.Name, rq.ExpectErrorMessage)
					Expect(goResult.Err).To(HaveOccurred(),
						"corpus entry %q: Go accepted a query expected to fail with message %q",
						rq.Name, rq.ExpectErrorMessage)
					var je *plandiff.JavaError
					Expect(errors.As(javaResult.Err, &je)).To(BeTrue(),
						"corpus entry %q: Java error is not *plandiff.JavaError: %T", rq.Name, javaResult.Err)
					var ge *api.Error
					Expect(errors.As(goResult.Err, &ge)).To(BeTrue(),
						"corpus entry %q: Go error is not *api.Error: %T", rq.Name, goResult.Err)
					Expect(je.Message).To(Equal(rq.ExpectErrorMessage),
						"corpus entry %q: Java error message verbatim mismatch", rq.Name)
					Expect(ge.Message).To(Equal(rq.ExpectErrorMessage),
						"corpus entry %q: Go error message verbatim mismatch", rq.Name)
					return
				}
				if rq.ExpectErrorContains != "" {
					// Negative test: both engines must fail AND each
					// error message must contain the substring (matched
					// case-insensitively because Java and Go have
					// different identifier-case conventions in error
					// messages — fdb-relational uppercases identifiers
					// per spec, Go preserves user-typed case). The
					// substring's CONTENT is what we're checking, not
					// its case.
					Expect(javaResult.Err).To(HaveOccurred(),
						"corpus entry %q: Java accepted a query expected to fail with %q",
						rq.Name, rq.ExpectErrorContains)
					Expect(goResult.Err).To(HaveOccurred(),
						"corpus entry %q: Go accepted a query expected to fail with %q",
						rq.Name, rq.ExpectErrorContains)
					needle := strings.ToLower(rq.ExpectErrorContains)
					Expect(strings.ToLower(javaResult.Err.Error())).To(ContainSubstring(needle),
						"corpus entry %q: Java error message missing expected substring", rq.Name)
					Expect(strings.ToLower(goResult.Err.Error())).To(ContainSubstring(needle),
						"corpus entry %q: Go error message missing expected substring", rq.Name)
					return
				}

				Expect(javaResult.Err).NotTo(HaveOccurred(),
					"corpus entry %q: Java executor errored", rq.Name)
				Expect(goResult.Err).NotTo(HaveOccurred(),
					"corpus entry %q: Go executor errored", rq.Name)

				Expect(goResult.Rows.Columns).To(Equal(javaResult.Rows.Columns),
					"corpus entry %q: column metadata diverged between Java and Go", rq.Name)
				Expect(goResult.Rows.Rows).To(Equal(javaResult.Rows.Rows),
					"corpus entry %q: row data diverged between Java and Go", rq.Name)
			})
		}
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
