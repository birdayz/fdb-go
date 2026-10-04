//go:build bazelrunfiles

package conformance_test

// Pins target-Java ARRAY comparisons and the preserved Go nullable-array read
// extension. Non-null arrays share outcomes. Java rejects NULL elements with
// 0A000 before comparison (AbstractArrayConstructorValue.LightArrayConstructorValue
// eval); Go retains equal-shaped nullable-array comparison and rejects unequal
// element nullability. These extension cases assert each engine independently,
// not cross-engine agreement or persisted NULL-element support.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/conformance/plandiff"
	"github.com/google/uuid"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("ArrayComparisonJavaProbe", func() {
	It("pins shared array comparisons and the Go nullable-array read extension", func() {
		ctx := context.Background()
		tenantName := fmt.Sprintf("arraycmp_%s", uuid.New().String())
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

		schema := "CREATE TABLE T_AC (id BIGINT, arr INTEGER ARRAY, PRIMARY KEY (id))"
		setup := []string{
			"INSERT INTO T_AC VALUES (1, [1]), (2, [1, 2])",
		}

		// The NULL-element cases intentionally preserve the approved Go read
		// extension. Java's explicit rejection replaces its earlier raw NPE;
		// it does not authorize removing Go's successful read semantics.
		probes := []struct{ name, sql, expect string }{
			{"col_eq_literal", "SELECT id FROM T_AC WHERE arr = [1]", "match"},
			{"col_eq_two_elem", "SELECT id FROM T_AC WHERE arr = [1, 2]", "match"},
			{"lit_nullable_vs_notnull", "SELECT [1, NULL] = [1, 2] FROM T_AC WHERE id = 1", "nullable_type_mismatch"},
			{"lit_nullable_vs_nullable", "SELECT [1, NULL] = [1, NULL] FROM T_AC WHERE id = 1", "nullable_read"},
			{"lit_null_elem_eq", "SELECT [NULL] = [NULL] FROM T_AC WHERE id = 1", "nullable_read"},
			{"lit_eq_same", "SELECT [1] = [1] FROM T_AC WHERE id = 1", "match"},
			{"lit_size_mismatch", "SELECT [1] = [1, 2] FROM T_AC WHERE id = 1", "match"},
			{"col_eq_bigint_literal_cast", "SELECT id FROM T_AC WHERE arr = CAST([1] AS BIGINT ARRAY)", "match"},
			{"lit_ordering", "SELECT [1] < [2] FROM T_AC WHERE id = 1", "match"},
			{"col_distinct_literal", "SELECT id FROM T_AC WHERE arr IS NOT DISTINCT FROM [1]", "match"},
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
			return fmt.Sprintf("%s OK rows=%v", engine, r.Rows.Rows)
		}
		// errMsg extracts the engine's error message ("" when no error).
		errMsg := func(r plandiff.RunResult) string {
			if r.Err == nil {
				return ""
			}
			var je *plandiff.JavaError
			if errors.As(r.Err, &je) {
				return je.Message
			}
			var ge *api.Error
			if errors.As(r.Err, &ge) {
				return ge.Message
			}
			return r.Err.Error()
		}
		var divergences []string
		diverge := func(p string, jr, gr plandiff.RunResult, why string) {
			divergences = append(divergences, fmt.Sprintf(
				"probe %s: %s\n  java: %s\n  go:   %s",
				p, why, render("JAVA", jr), render("GO  ", gr)))
		}
		for _, p := range probes {
			jr := runner.RunWithSetup(ctx, schema, setup, p.sql)
			gr := goRunner.RunWithSetup(ctx, schema, setup, p.sql)
			fmt.Fprintf(GinkgoWriter, "PROBE %s\n  %s\n  %s\n  sql: %s\n",
				p.name, render("JAVA", jr), render("GO  ", gr), p.sql)
			switch p.expect {
			case "nullable_read", "nullable_type_mismatch":
				var je *plandiff.JavaError
				if !errors.As(jr.Err, &je) || je.ExceptionClass != "RelationalException" || je.SQLState != "0A000" || je.Message != "An ARRAY value cannot have NULL elements" {
					diverge(p.name, jr, gr, "Java NULL-element rejection changed")
				}
				if p.expect == "nullable_read" {
					if gr.Err != nil || fmt.Sprintf("%v", gr.Rows.Rows) != "[[true]]" {
						diverge(p.name, jr, gr, "Go no longer answers TRUE for the NULL-element equality")
					}
				} else {
					var ge *api.Error
					if !errors.As(gr.Err, &ge) || string(ge.Code) != "42804" || ge.Message != "The operands of a comparison operator are not compatible." {
						diverge(p.name, jr, gr, "Go element-nullability admission changed")
					}
				}
			case "match":
				switch {
				case jr.Err == nil && gr.Err == nil:
					if fmt.Sprintf("%v", jr.Rows.Rows) != fmt.Sprintf("%v", gr.Rows.Rows) {
						diverge(p.name, jr, gr, "row mismatch")
					}
				case jr.Err != nil && gr.Err != nil:
					var je *plandiff.JavaError
					var ge *api.Error
					if !errors.As(jr.Err, &je) || !errors.As(gr.Err, &ge) || je.SQLState == "" || je.SQLState != string(ge.Code) || !strings.Contains(errMsg(gr), errMsg(jr)) {
						diverge(p.name, jr, gr, "error wording mismatch")
					}
				default:
					diverge(p.name, jr, gr, "one engine errors, the other succeeds")
				}
			}
		}
		Expect(divergences).To(BeEmpty())
	})
})
