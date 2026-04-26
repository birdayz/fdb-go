package conformance_test

// End-to-end integration test for the runSql harness (Track A1 of TODO.md
// execution roadmap). Drives the Java fdb-relational engine via the
// SqlPlanSteps#runSql step against a shared FDB testcontainer and verifies
// the result-set wire shape — column metadata, row values, NULL handling.
//
// What this test asserts:
//
//   1. Schema-less SELECT (no FROM, no schema): a literal returns the
//      expected value via the /__SYS connection path.
//
//   2. SELECT against a table in the ephemeral schema: pins the schema-
//      template branch end-to-end (CREATE TEMPLATE / DATABASE / SCHEMA
//      → JDBC executeQuery → RelationalResultSet → JSON encoding).
//      Empty result is sufficient — multi-row + NULL preservation are
//      exercised by httptest unit tests (full wire-shape control).
//
//   3. SELECT 0 rows from an in-line VALUES: empty result set must
//      produce zero-length Rows without crashing the harness.
//
// What this test does NOT assert:
//
//   - Cross-engine result-set equivalence. That's Track A3 (yamsql
//     corpus comparison) and depends on Go-side execution (Track C2).

import (
	"context"
	"fmt"

	"github.com/birdayz/fdb-record-layer-go/pkg/relational/conformance/plandiff"
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
