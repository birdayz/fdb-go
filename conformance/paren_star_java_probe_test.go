//go:build bazelrunfiles

package conformance_test

// Records Java's live behaviour (tag 4.12.11.0 conformance server) for the
// PARENTHESISED STAR — `SELECT (*)` and `SELECT (T.*)` — and for the
// one-element parenthesised scalar that shares the same grammar rule.
//
// Both shapes come out of the SAME production: RelationalParser.g4:953-955's
// `recordConstructor : ofTypeClause? '(' (uid DOT STAR | STAR |
// expressionWithOptionalName (',' ...)* ')'`. So `(*)`, `(t.*)`, `(1+2)` and
// `(1, 2)` are four arms of one rule, and Java's
// ExpressionVisitor.visitRecordConstructor (:889-926) dispatches between
// them. That shared origin is why these are measured together: the
// one-element arm has no unwrap in Java (:918-925 goes straight to
// RecordConstructorValue.ofColumns), so `(1+2)` should be a ONE-FIELD RECORD,
// not a scalar — and the star arms should be a SINGLE struct-typed column,
// not an expansion into many.
//
// The naming rule for the unqualified star is the part that cannot be read
// off one file: visitRecordConstructor (:902-916) asks
// SemanticAnalyzer.expandStar (:321-347) for the expansion, then names the
// column after the SOLE for-each quantifier in scope, falling back to an
// anonymous column when there is more than one — and "more than one" includes
// PartiQL unnest bindings, not just joins. Star.overQuantifiers
// (Star.java:141-148) is where one-vs-many changes the underlying value:
// with one quantifier the value is that quantifier's flowed object, with
// several it is a record constructor over the expansion, which FLATTENS.
//
// It makes NO assertions; it prints both engines' outcomes so the pins added
// for `SELECT (*)` can be diffed against the live JVM whenever either engine
// changes. It depends on encodeValue rendering a RelationalStruct as its
// attributes (conformance/sql_plan_steps.java): a struct that renders as
// `__unsupported__` shows the column TYPE but not its CONTENTS, and contents
// are the whole question for the one-element arm.

import (
	"context"
	"errors"
	"fmt"
	"os"

	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/conformance/plandiff"
	"github.com/google/uuid"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("ParenStarJavaProbe", func() {
	It("records Java's live behaviour for parenthesised-star and one-element record constructors", func() {
		ctx := context.Background()
		tenantName := fmt.Sprintf("parenstar_%s", uuid.New().String())
		env, err := SetupTenantEnvironment(ctx, sharedContainer, tenantName)
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = env.Cleanup(ctx) }()

		srv, err := NewIsolatedJavaInvoker()
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = srv.Close() }()
		runner := plandiff.NewJavaRunnerHTTP(javaBaseURL(srv), env.ClusterFile).(plandiff.SetupRunner)
		clusterFilePath := writeClusterFileToTemp(env.ClusterFile)
		defer os.Remove(clusterFilePath)
		goRunner := plandiff.NewGoSQLSetupRunner(clusterFilePath)

		// Mirrors star-expression-metadata.yamsql's schema so the probe's
		// answers can be read directly against that file's expectations.
		schema := "CREATE TABLE FOO (id BIGINT, val BIGINT, PRIMARY KEY (id))" +
			" CREATE TABLE BAR (bid BIGINT, bval BIGINT, PRIMARY KEY (bid))"
		setup := []string{
			"INSERT INTO FOO VALUES (1, 10)",
			"INSERT INTO BAR VALUES (1, 20)",
		}

		probes := []struct{ name, sql string }{
			// ---- the star arms: ONE for-each quantifier in scope ----
			// The column is named after the sole source, and carries the
			// whole row as one struct.
			{"star_single_table", "SELECT (*) FROM FOO"},
			{"star_single_qualified", "SELECT (FOO.*) FROM FOO"},
			{"star_single_table_alias", "SELECT (*) FROM FOO AS B"},
			{"star_single_qualified_alias", "SELECT (B.*) FROM FOO AS B"},
			// An explicit AS overrides the inferred name.
			{"star_single_explicit_alias", "SELECT (*) AS Z FROM FOO"},
			{"star_qualified_explicit_alias", "SELECT (FOO.*) AS Z FROM FOO"},
			// A derived table's alias is the source name.
			{"star_subquery_alias", "SELECT (*) FROM (SELECT id, val FROM FOO) AS SUB"},

			// ---- the star arm: TWO for-each quantifiers in scope ----
			// The name falls back to anonymous, and the struct FLATTENS both
			// sources' columns rather than nesting one struct per source —
			// Star.overQuantifiers builds ofUnnamed(quantifiers) but
			// ensureValueConsistentWithExpansion replaces it with the flat
			// record constructor over the expansion whenever the types differ,
			// which for two quantifiers they always do.
			{"star_two_tables", "SELECT (*) FROM FOO, BAR WHERE FOO.id = BAR.bid"},
			// A qualified star with two sources in scope still names after the
			// qualifier — the ambiguity only affects the UNqualified arm.
			{"star_qualified_two_tables", "SELECT (FOO.*) FROM FOO, BAR WHERE FOO.id = BAR.bid"},

			// ---- the star arm nested ----
			// select-a-star.yamsql's `select ((*)) from t1` — the outer paren
			// is the one-element arm over the inner star arm, so a confirmed
			// no-unwrap Java should double-nest.
			{"star_double_paren", "SELECT ((*)) FROM FOO"},
			// The star alongside an ordinary column, to show the parenthesised
			// star occupies exactly ONE slot.
			{"star_with_sibling_column", "SELECT (*), id FROM FOO"},

			// ---- the one-element arm (CQ-87) ----
			// The control: no parens at all. Must be a plain scalar column.
			{"scalar_no_paren", "SELECT 1 + 2 FROM FOO"},
			// The question: Java's visitRecordConstructor has no one-element
			// unwrap, so this should be a ONE-FIELD STRUCT containing 3.
			// Go unwraps it to the scalar 3.
			{"scalar_one_paren", "SELECT (1 + 2) FROM FOO"},
			// Double parens — two nested one-field records if there is no
			// unwrap, still a scalar if there is.
			{"scalar_two_parens", "SELECT ((1 + 2)) FROM FOO"},
			// A parenthesised COLUMN reference, the shape that would re-type
			// most broadly if the unwrap were removed.
			{"column_one_paren", "SELECT (val) FROM FOO"},
			// A parenthesised element with an AS name INSIDE the parens —
			// distinguishes the record arm (name lands on the field) from a
			// projection alias (name lands on the column).
			{"named_one_element", "SELECT (val AS Q) FROM FOO"},
			// The two-element control: unambiguously a record in both engines.
			{"two_element", "SELECT (1, 2) FROM FOO"},
			// A parenthesised scalar in a PREDICATE — if `(x)` were a
			// one-field record everywhere, this comparison would be
			// record-vs-scalar. Whether Java accepts it says whether the
			// no-unwrap reading extends past projection position.
			{"paren_scalar_in_predicate", "SELECT id FROM FOO WHERE (val) = 10"},
			{"paren_scalar_arith", "SELECT (val) + 1 FROM FOO"},
		}

		render := func(engine string, r plandiff.RunResult) string {
			if r.Err != nil {
				var je *plandiff.JavaError
				if errors.As(r.Err, &je) {
					return fmt.Sprintf("%s ERROR sqlstate=%q msg=%q", engine, je.SQLState, je.Message)
				}
				var ge *api.Error
				if errors.As(r.Err, &ge) {
					return fmt.Sprintf("%s ERROR sqlstate=%q msg=%q", engine, string(ge.Code), ge.Message)
				}
				return fmt.Sprintf("%s ERROR %v", engine, r.Err)
			}
			cols := make([]string, 0, len(r.Rows.Columns))
			for _, c := range r.Rows.Columns {
				cols = append(cols, fmt.Sprintf("%s(%s)", c.Name, c.Type))
			}
			return fmt.Sprintf("%s OK cols=%v rows=%v", engine, cols, r.Rows.Rows)
		}
		for _, p := range probes {
			jr := runner.RunWithSetup(ctx, schema, setup, p.sql)
			gr := goRunner.RunWithSetup(ctx, schema, setup, p.sql)
			fmt.Fprintf(GinkgoWriter, "PROBE %s\n  %s\n  %s\n  sql: %s\n",
				p.name, render("JAVA", jr), render("GO  ", gr), p.sql)
		}
	})

	// The probe above PRINTS; this pins. Three facts justify decisions that
	// outlive the measurement, so each is asserted rather than left to a
	// reader of GinkgoWriter output:
	//
	//  1. Java's `SELECT (*) FROM FOO` is ONE struct column named after the
	//     sole for-each quantifier, carrying the whole row. That is the
	//     shape Go's parenthesised-star expansion has to produce, and the
	//     naming rule is the part that cannot be read off a single Java file.
	//  2. Java's `SELECT (1 + 2)` is a ONE-FIELD STRUCT, not the scalar 3 —
	//     visitRecordConstructor has no one-element unwrap. Go unwraps, so
	//     this is a live divergence on the SHARED surface, and the assertion
	//     is what keeps it from being quietly re-classified as settled.
	//  3. Java's `SELECT (val) + 1` is nevertheless a plain BIGINT. So the
	//     absence of an unwrap in the CONSTRUCTOR does not mean a one-field
	//     record survives into an arithmetic operand. Anyone removing Go's
	//     unwrap to close (2) must keep this working, and without the pin
	//     that constraint is invisible.
	//
	// All three depend on the harness rendering a RelationalStruct as its
	// attributes (sql_plan_steps.java encodeValue): with the struct arm
	// removed every expected map below reads `__unsupported__` instead.
	It("pins Java's parenthesised-star and one-element record shapes", func() {
		ctx := context.Background()
		tenantName := fmt.Sprintf("parenstarpin_%s", uuid.New().String())
		env, err := SetupTenantEnvironment(ctx, sharedContainer, tenantName)
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = env.Cleanup(ctx) }()

		srv, err := NewIsolatedJavaInvoker()
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = srv.Close() }()
		runner := plandiff.NewJavaRunnerHTTP(javaBaseURL(srv), env.ClusterFile).(plandiff.SetupRunner)

		schema := "CREATE TABLE FOO (id BIGINT, val BIGINT, PRIMARY KEY (id))"
		setup := []string{"INSERT INTO FOO VALUES (1, 10)"}

		// (1) the star arm over a single source.
		star := runner.RunWithSetup(ctx, schema, setup, "SELECT (*) FROM FOO")
		Expect(star.Err).NotTo(HaveOccurred())
		Expect(star.Rows.Columns).To(HaveLen(1), "SELECT (*) is ONE column, never an expansion")
		Expect(star.Rows.Columns[0].Name).To(Equal("FOO"),
			"the column takes the sole for-each quantifier's name")
		Expect(star.Rows.Columns[0].Type).To(Equal("STRUCT"))
		Expect(star.Rows.Rows).To(HaveLen(1))
		Expect(star.Rows.Rows[0][0]).To(Equal(map[string]any{"ID": float64(1), "VAL": float64(10)}),
			"the struct carries the whole row")

		// (2) the one-element arm — a record, not the scalar.
		paren := runner.RunWithSetup(ctx, schema, setup, "SELECT (1 + 2) FROM FOO")
		Expect(paren.Err).NotTo(HaveOccurred())
		Expect(paren.Rows.Columns).To(HaveLen(1))
		Expect(paren.Rows.Columns[0].Type).To(Equal("STRUCT"),
			"Java has no one-element unwrap: (1+2) is a one-field record")
		Expect(paren.Rows.Rows[0][0]).To(Equal(map[string]any{"_0": float64(3)}),
			"the scalar sits INSIDE the record under the ordinal name")

		// (3) the same parenthesis in an arithmetic operand is a scalar.
		arith := runner.RunWithSetup(ctx, schema, setup, "SELECT (val) + 1 FROM FOO")
		Expect(arith.Err).NotTo(HaveOccurred())
		Expect(arith.Rows.Columns).To(HaveLen(1))
		Expect(arith.Rows.Columns[0].Type).To(Equal("BIGINT"),
			"a parenthesised operand does NOT survive as a record into arithmetic")
		Expect(arith.Rows.Rows[0][0]).To(Equal(float64(11)))
	})
})
