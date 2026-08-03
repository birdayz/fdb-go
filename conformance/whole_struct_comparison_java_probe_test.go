//go:build bazelrunfiles

package conformance_test

// Records Java's live behaviour (tag 4.12.11.0 conformance server) for
// comparisons and IS NULL applied to a WHOLE struct column, rather than to one
// of its fields.
//
// The measurement matters because the surface is NOT a clean SQLSTATE.
// RelOpValue asserts its comparands are of primitive type
// (RelOpValue.java:333,:345,:350 →
// SemanticException.ErrorCode.COMPARAND_TO_COMPARISON_IS_OF_COMPLEX_TYPE,
// declared at SemanticException.java:45), and
// ExceptionUtil.translateErrorCode's switch has NO case for that code
// (ExceptionUtil.java:88-103), so it falls to `default: INTERNAL_ERROR` —
// ErrorCode.java:177, XX000. A raw internal error is what a client sees.
//
// The IS NULL half is a Java LIMITATION, not an error class: upstream issue
// 3700 has `select a from tWithStruct where embedding is null` and
// `select embeddingGrouped is null from tWithGroupedStruct where a = 200`
// COMMENTED OUT in vector.yamsql, so whole-struct IS NULL is not a shape Java
// answers at this tag.
//
// It makes NO assertions; it prints both engines' outcomes so
// pkg/relational/sqldriver/whole_struct_comparison_fdb_test.go's pins can be
// re-checked against the live JVM whenever either engine changes.

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

var _ = Describe("WholeStructComparisonJavaProbe", func() {
	It("records Java's live behaviour for whole-struct comparison and IS NULL", func() {
		ctx := context.Background()
		tenantName := fmt.Sprintf("wholestruct_%s", uuid.New().String())
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

		schema := "CREATE TYPE AS STRUCT ADDR (city STRING, zip BIGINT)" +
			" CREATE TABLE T_S (id BIGINT, home ADDR, PRIMARY KEY (id))"
		setup := []string{
			"INSERT INTO T_S VALUES (1, ('sf', 94100))",
			"INSERT INTO T_S VALUES (2, NULL)",
		}

		probes := []struct{ name, sql string }{
			// The control: a FIELD of the struct compares and IS NULLs fine.
			{"field_equality", "SELECT id FROM T_S WHERE home.city = 'sf'"},
			{"field_is_null", "SELECT id FROM T_S WHERE home.city IS NULL"},
			// The whole struct as a comparand — the RelOpValue assert.
			{"whole_struct_equality_literal", "SELECT id FROM T_S WHERE home = ('sf', 94100)"},
			{"whole_struct_inequality_literal", "SELECT id FROM T_S WHERE home <> ('sf', 94100)"},
			{"whole_struct_ordering_literal", "SELECT id FROM T_S WHERE home > ('sf', 94100)"},
			{"whole_struct_equality_self", "SELECT id FROM T_S WHERE home = home"},
			// The IS NULL half — upstream issue 3700's commented-out shapes.
			{"whole_struct_is_null_predicate", "SELECT id FROM T_S WHERE home IS NULL"},
			{"whole_struct_is_not_null_predicate", "SELECT id FROM T_S WHERE home IS NOT NULL"},
			{"whole_struct_is_null_projection", "SELECT home IS NULL FROM T_S WHERE id = 2"},
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
