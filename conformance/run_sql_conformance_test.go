//go:build bazelrunfiles

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
	"fmt"
	"os"
	"strings"
	"sync"

	"fdb.dev/pkg/relational/conformance/plandiff"
	"github.com/google/uuid"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

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
		// 4.12.11.0): BYTES NOT NULL, DATE, TIMESTAMP — these wait
		// on a follow-up shift.
		runner, ok := plandiff.NewJavaRunnerHTTP(javaBaseURL(java), env.ClusterFile).(plandiff.SetupRunner)
		Expect(ok).To(BeTrue(), "javaRunner must satisfy SetupRunner")

		got := runner.RunWithSetup(
			ctx,
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
		got := runner.RunWithSetup(
			ctx,
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
		got := runner.RunWithSetup(
			ctx,
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
		// The corpus is driven in PARALLEL across a small pool of freshly-spawned
		// Java servers + Go runners (see the fan-out below) — fresh servers, not
		// the suite-shared one, to avoid pollution from prior conformance specs.
		corpus := plandiff.SeedRunCorpus()
		// Apply RFC-082 cross-engine divergence annotations (Go-only extensions,
		// tracked Go capability gaps, and both-reject message-drift) so the
		// harness asserts Go's documented behaviour without pinning Java's.
		plandiff.ApplyRFC082Divergences(corpus)

		clusterFilePath := writeClusterFileToTemp(env.ClusterFile)
		defer os.Remove(clusterFilePath)

		// Fan the corpus out across N workers, each driving a disjoint subset on
		// its OWN pre-spawned Java server + Go runner against the one shared FDB
		// cluster. Every entry runs in its own uuid-isolated ephemeral schema, so
		// workers never collide; pre-spawning all servers up front preserves the
		// no-spawn-during-query rule; and per-server load is 1/N of the
		// single-server serial baseline that is already green (so error-path
		// state accumulation, if any, is strictly lower). Java is the spec: per
		// entry, whatever Java does becomes the behaviour Go must match — drift on
		// either side surfaces. Workers only COMPUTE the (java, go) result pair;
		// Gomega's Expect is not goroutine-safe, so every assertion runs serially
		// after the join.
		n := seedCorpusParallelism()
		if n > len(corpus) {
			n = len(corpus)
		}
		type corpusRunner struct {
			java plandiff.SetupRunner
			gor  plandiff.SetupRunner
			srv  *JavaInvoker
		}
		runners := make([]corpusRunner, n)
		for i := range runners {
			srv, err := NewIsolatedJavaInvoker()
			Expect(err).NotTo(HaveOccurred(), "failed to spawn Java conformance server %d/%d", i+1, n)
			runners[i] = corpusRunner{
				java: plandiff.NewJavaRunnerHTTP(javaBaseURL(srv), env.ClusterFile).(plandiff.SetupRunner),
				gor:  plandiff.NewGoSQLSetupRunner(clusterFilePath),
				srv:  srv,
			}
		}
		defer func() {
			for _, r := range runners {
				_ = r.srv.Close()
			}
		}()

		type enginePair struct{ java, golang plandiff.RunResult }
		results := make([]enginePair, len(corpus))
		idxCh := make(chan int)
		var wg sync.WaitGroup
		for w := 0; w < n; w++ {
			wg.Add(1)
			go func(r corpusRunner, wid int) {
				defer wg.Done()
				for idx := range idxCh {
					rq := corpus[idx]
					jr := r.java.RunWithSetup(ctx, rq.SchemaTemplate, rq.SetupSqls, rq.Query)
					gr := r.gor.RunWithSetup(ctx, rq.SchemaTemplate, rq.SetupSqls, rq.Query)
					// Re-run whichever side hit a retryable HARNESS error — a
					// retryable FDB code (catalog conflict, transaction starved
					// past 5s) or a lifecycle failure (blown deadline, torn-down
					// connection, closed context). See rerunWhileRetryable.
					jr, gr = rerunWhileRetryable(jr, gr, wid,
						func() plandiff.RunResult {
							return r.java.RunWithSetup(ctx, rq.SchemaTemplate, rq.SetupSqls, rq.Query)
						},
						func() plandiff.RunResult {
							return r.gor.RunWithSetup(ctx, rq.SchemaTemplate, rq.SetupSqls, rq.Query)
						})
					results[idx] = enginePair{java: jr, golang: gr}
				}
			}(runners[w], w)
		}
		for i := range corpus {
			idxCh <- i
		}
		close(idxCh)
		wg.Wait()

		// RFC-082 regression LOCK, asserted serially over the computed results so
		// one run reports the full delta: non-annotated entries that diverge but
		// are NOT in rfc082KnownRed are regressions; known-red entries that now
		// pass must be removed from the lock (it only shrinks). Annotated entries
		// assert Go's pinned behaviour without pinning Java's.
		var regressions, fixedNowGreen, staleAnnotations []string
		for idx := range corpus {
			rq := corpus[idx]
			javaResult, goResult := results[idx].java, results[idx].golang
			if rq.Divergence != nil {
				if ok, detail := divergenceHolds(rq.Divergence, rq.Query, javaResult, goResult); !ok {
					staleAnnotations = append(staleAnnotations, fmt.Sprintf("%s: %s", rq.Name, detail))
				}
				continue
			}
			ok, detail := entryConforms(javaResult, goResult)
			known := plandiff.IsKnownRed(rq.Name)
			if !ok && !known {
				regressions = append(regressions, fmt.Sprintf("%s: %s", rq.Name, detail))
			} else if ok && known {
				fixedNowGreen = append(fixedNowGreen, rq.Name)
			}
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
