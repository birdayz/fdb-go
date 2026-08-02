//go:build bazelrunfiles

package conformance_test

// Records Java's live behaviour (tag 4.12.11.0 conformance server) for the
// record constructor as a VALUE, and for how a record literal binds to a
// TARGET struct type.
//
// The binding question is the one that cannot be settled by reading alone.
// Java's ExpressionVisitor.parseRecordFieldsUnderReorderings
// (ExpressionVisitor.java:1040-1075) consults a target type when the plan
// fragment carries one, and its "reorderings" come from an INSERT's explicit
// COLUMN LIST — not from `AS` names written inside the constructor. Without a
// reordering it falls to parseRecordFields(contexts, elementFields), which
// pairs by POSITION and takes the TARGET field's name. So the reading says an
// `AS` name inside a targeted literal is ignored; whether that surfaces as a
// silent positional override or an error is what this measures.
//
// It makes NO assertions; it prints both engines' outcomes so the pins in
// pkg/relational/sqldriver/record_constructor_expression_fdb_test.go can be
// re-checked against the live JVM.

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

var _ = Describe("RecordConstructorJavaProbe", func() {
	It("records Java's live behaviour for record constructors and target binding", func() {
		ctx := context.Background()
		tenantName := fmt.Sprintf("rcexpr_%s", uuid.New().String())
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

		schema := "CREATE TYPE AS STRUCT S4 (a BIGINT, b DOUBLE, c STRING, d BOOLEAN)" +
			" CREATE TABLE T_C (id BIGINT, s S4, PRIMARY KEY (id))"
		setup := []string{
			"INSERT INTO T_C VALUES (1, (7, 2.5, 'x', true))",
			"INSERT INTO T_C VALUES (2, null)",
		}

		probes := []struct{ name, sql string }{
			// The producer, in expression position.
			{"bare_record_literal", "SELECT (1, 1.0, 'a', true) FROM T_C WHERE id = 1"},
			{"coalesce_null_then_record", "SELECT COALESCE(null, (1, 1.0, 'a', true)) FROM T_C WHERE id = 1"},
			{"coalesce_record_then_null", "SELECT COALESCE((1, 2), null) FROM T_C WHERE id = 1"},
			{"named_elements", "SELECT (1 AS x, 'q' AS y) FROM T_C WHERE id = 1"},
			// The single-element unwrap: a parenthesised scalar must stay a
			// scalar, not become a one-field record.
			{"single_element_paren", "SELECT (1 + 2) FROM T_C WHERE id = 1"},
			// TARGET binding. The anonymous literal must reach the struct
			// column positionally.
			{"update_anonymous_through_coalesce", "UPDATE T_C SET s = COALESCE(s, (9, 8.5, 'z', false)) WHERE id = 2"},
			// The named literal IN DECLARATION ORDER — names agree with
			// positions, so positional and by-name binding cannot be told
			// apart here. It is the control for the next one.
			{"update_named_in_order", "UPDATE T_C SET s = (11 AS a, 3.5 AS b, 'w' AS c, true AS d) WHERE id = 2"},
			// The named literal OUT OF declaration order — this is the shape
			// that separates the two rules. Positional binding puts 'w' into
			// the DOUBLE field b; by-name binding puts 3.5 there.
			{"update_named_out_of_order", "UPDATE T_C SET s = (11 AS a, 'w' AS c, 3.5 AS b, true AS d) WHERE id = 2"},
			{"read_back", "SELECT s.a, s.b, s.c, s.d FROM T_C WHERE id = 2"},
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
})
