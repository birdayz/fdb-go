//go:build bazelrunfiles

package conformance_test

// Measures Java's live behaviour (tag 4.12.11.0 conformance server) for
// ARRAY comparison shapes the upstream yaml corpus does NOT cover, and
// asserts Go matches it outcome-for-outcome. The corpus
// (arrays-operators.yamsql) pins the constant matrix; the shapes here are
// the ones a Go reader could plausibly get wrong ANOTHER way:
//
//   - a stored ARRAY column compared against an array literal (element
//     nullability differs from the corpus's CAST shapes: Java types both
//     the column element and the literal element NOT NULL —
//     Type.fromObject's `primitiveType(typeCode, false)` and the DDL's
//     `elementType.withNullable(false)` — so the types match and the
//     comparison is accepted);
//   - literals whose element nullability DIFFERS ([1, NULL] vs [1, 2]:
//     the NULL element folds the left element type nullable while the
//     right stays NOT NULL — does Java's modulo-nullability check strip
//     only the OUTER nullability and reject, or accept?);
//   - element NULLs inside equal-shaped arrays (compareListEquals treats
//     both-NULL elements as EQUAL — two-valued, no UNKNOWN propagation).
//
// Go must produce the same outcome class per probe: same rows on success,
// same SQLSTATE on error. A divergence fails this test.

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
	It("Go matches Java's live outcome for array comparison shapes", func() {
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

		// expect encodes the probe's cross-engine relation:
		//   "match"      — Go's outcome class must equal Java's (rows on
		//                  success; both-error probes additionally require
		//                  Java's message to be the shared 42804 wording,
		//                  since the server channel carries no SQLSTATE).
		//   "java_npe"   — MEASURED upstream Java bug: a NULL element
		//                  inside a compared array literal throws a raw
		//                  NullPointerException. Go answers TRUE — what
		//                  Java's own compareListEquals (both-NULL
		//                  elements EQUAL) specifies. The pin holds while
		//                  Java still NPEs and Go still answers TRUE; if
		//                  Java is fixed upstream this probe fails so the
		//                  divergence gets re-measured and re-pinned.
		probes := []struct{ name, sql, expect string }{
			{"col_eq_literal", "SELECT id FROM T_AC WHERE arr = [1]", "match"},
			{"col_eq_two_elem", "SELECT id FROM T_AC WHERE arr = [1, 2]", "match"},
			{"lit_nullable_vs_notnull", "SELECT [1, NULL] = [1, 2] FROM T_AC WHERE id = 1", "match"},
			{"lit_nullable_vs_nullable", "SELECT [1, NULL] = [1, NULL] FROM T_AC WHERE id = 1", "java_npe"},
			{"lit_null_elem_eq", "SELECT [NULL] = [NULL] FROM T_AC WHERE id = 1", "java_npe"},
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
			case "java_npe":
				if jr.Err == nil || !strings.Contains(errMsg(jr), "NullPointerException") {
					diverge(p.name, jr, gr, "pinned Java NPE no longer reproduces — re-measure and re-pin")
				}
				if gr.Err != nil || fmt.Sprintf("%v", gr.Rows.Rows) != "[[true]]" {
					diverge(p.name, jr, gr, "Go no longer answers TRUE for the NULL-element equality")
				}
			case "match":
				switch {
				case jr.Err == nil && gr.Err == nil:
					if fmt.Sprintf("%v", jr.Rows.Rows) != fmt.Sprintf("%v", gr.Rows.Rows) {
						diverge(p.name, jr, gr, "row mismatch")
					}
				case jr.Err != nil && gr.Err != nil:
					// The server channel carries no SQLSTATE; the shared
					// wording is the strongest cross-engine assertion left.
					if !strings.Contains(errMsg(gr), errMsg(jr)) {
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
