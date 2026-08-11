//go:build bazelrunfiles

package conformance_test

// Records Java's live behaviour (tag 4.12.11.0 conformance server) for
// duplicate-FROM-alias shapes: per-attribute reference resolution, SELECT-*
// duplicate-column layout, the qualified-star/table-row findFirst quirks, and
// the exact 42702 message text. It makes NO assertions; it prints each
// shape's outcome so it can be diffed against Go's corpus entries (see
// plandiff/corpus.go's dup_from_alias_* entries) whenever either engine's
// handling of duplicate aliases changes.

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

var _ = Describe("DuplicateFromAliasJavaProbe", func() {
	It("records Java's live behaviour for duplicate FROM-alias shapes", func() {
		ctx := context.Background()
		tenantName := fmt.Sprintf("dupalias_%s", uuid.New().String())
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

		// T_P1 and T_R1 SHARE column id (v / rv unique); T_Q1 is DISJOINT
		// from both.
		// T_S1 and T_S2 carry STRUCT columns under DIFFERENT names (n / m), so a
		// three-segment reference through a duplicated alias has exactly one
		// candidate by the per-attribute rule; T_S3 repeats T_S1's `n` so the
		// two-candidate case is reachable too. Struct member values are disjoint
		// across the tables, which is what makes a wrong-leg read visible as a
		// VALUE rather than only as a count.
		schema := "CREATE TABLE T_P1 (id BIGINT, v BIGINT, PRIMARY KEY (id))" +
			" CREATE TABLE T_Q1 (qid BIGINT, PRIMARY KEY (qid))" +
			" CREATE TABLE T_R1 (id BIGINT, rv BIGINT, PRIMARY KEY (id))" +
			" CREATE TYPE AS STRUCT NST (sk BIGINT, co BIGINT)" +
			" CREATE TABLE T_S1 (id BIGINT, n NST, PRIMARY KEY (id))" +
			" CREATE TABLE T_S2 (pid BIGINT, m NST, PRIMARY KEY (pid))" +
			" CREATE TABLE T_S3 (sid BIGINT, n NST, PRIMARY KEY (sid))"
		setup := []string{
			"INSERT INTO T_P1 VALUES (1, 10), (2, 20)",
			"INSERT INTO T_Q1 VALUES (7)",
			"INSERT INTO T_R1 VALUES (5, 50)",
			"INSERT INTO T_S1 VALUES (1, (11, 12)), (2, (21, 22))",
			"INSERT INTO T_S2 VALUES (5, (31, 32)), (7, (41, 42))",
			"INSERT INTO T_S3 VALUES (9, (91, 92))",
		}

		probes := []struct{ name, sql string }{
			{"star_dup_unaliased", "SELECT * FROM T_P1, T_Q1, T_P1"},
			{"star_dup_cte", "WITH w AS (SELECT id FROM T_P1) SELECT * FROM w, w"},
			{"star_dup_derived", "SELECT * FROM (SELECT id FROM T_P1) AS d, (SELECT id FROM T_P1) AS d"},
			{"disjoint_where_outer", "SELECT a.id FROM T_P1 AS a, T_Q1 AS a WHERE a.id = 2"},
			{"disjoint_where_inner", "SELECT a.qid FROM T_P1 AS a, T_Q1 AS a WHERE a.qid = 7"},
			{"shared_partial_unique_ref", "SELECT a.v FROM T_P1 AS a, T_R1 AS a"},
			{"shared_partial_unique_where", "SELECT a.v FROM T_P1 AS a, T_R1 AS a WHERE a.v = 10"},
			{"shared_col_ref_select", "SELECT a.id FROM T_P1 AS a, T_R1 AS a"},
			{"shared_col_ref_where", "SELECT a.v FROM T_P1 AS a, T_R1 AS a WHERE a.id = 1"},
			{"bare_unique_dup", "SELECT v FROM T_P1 AS a, T_Q1 AS a"},
			{"bare_ambig_dup", "SELECT id FROM T_P1 AS a, T_R1 AS a"},
			{"bare_ambig_distinct", "SELECT id FROM T_P1, T_R1"},
			{"qualified_star_dup", "SELECT a.* FROM T_P1 AS a, T_Q1 AS a"},
			{"table_row_dup", "SELECT a FROM T_P1 AS a, T_Q1 AS a"},
			{"order_by_ambig", "SELECT a.v FROM T_P1 AS a, T_R1 AS a ORDER BY a.id"},
			{"group_by_ambig", "SELECT COUNT(*) FROM T_P1 AS a, T_R1 AS a GROUP BY a.id"},
			{"alias_as_from_source", "SELECT * FROM T_P1 AS a, T_P1 AS a, a AS b"},
			{"join_on_dup_unique", "SELECT a.v FROM T_P1 AS a JOIN T_Q1 AS a ON a.v > 0"},
			{"left_join_dup", "SELECT a.v FROM T_P1 AS a LEFT JOIN T_Q1 AS a ON a.qid = 7"},
			// Does a QUALIFIED reference whose alias exists in the INNER scope
			// WITHOUT the column fall through to a same-aliased OUTER scope
			// source (Java resolveAcrossFragments zero-match → parent), or
			// stop at the local alias?
			{"qual_parent_fallthrough", "SELECT p.v FROM T_P1 AS p WHERE EXISTS (SELECT 1 FROM T_Q1 AS p WHERE p.v = 10)"},
			{"qual_parent_fallthrough_ctl", "SELECT p.v FROM T_P1 AS p WHERE EXISTS (SELECT 1 FROM T_Q1 AS q WHERE p.v = 10)"},
			// The inner-scope AMBIGUITY-is-terminal twin: the inner scope has
			// TWO same-aliased sources sharing the column; the outer also has
			// the alias. Java: inner ambiguity must NOT fall through.
			{"qual_parent_inner_ambig", "SELECT p.id FROM T_P1 AS p WHERE EXISTS (SELECT 1 FROM T_P1 AS p, T_R1 AS p WHERE p.id = 1)"},
			// A THREE-SEGMENT reference `alias.struct.member` through a
			// DUPLICATED alias. The question these settle is whether the
			// duplicate is adjudicated per-ALIAS ("`a` names two sources, so
			// refuse") or per-ATTRIBUTE ("only one of them carries `n`, so
			// answer from that one") — Java's rule for the two-segment case is
			// per-attribute, and these ask whether depth changes it.
			//
			// seg3_dup_first / seg3_dup_second are the pair that matters: the
			// carrying leg is the FIRST source in one and the SECOND in the
			// other, so a first-match reader answers seg3_dup_first correctly
			// and seg3_dup_second from the wrong leg. Their values are disjoint,
			// so that shows up as different numbers rather than as an error.
			{"seg3_dup_first", "SELECT a.n.sk FROM T_S1 AS a, T_S2 AS a"},
			{"seg3_dup_second", "SELECT a.m.sk FROM T_S1 AS a, T_S2 AS a"},
			{"seg3_dup_ctl", "SELECT a.n.sk FROM T_S1 AS a, T_S2 AS b"},
			// BOTH same-aliased sources carry `n`, which is the only shape that
			// produces two candidates under the per-attribute rule.
			{"seg3_dup_both_carry", "SELECT a.n.sk FROM T_S1 AS a, T_S3 AS a"},
			{"seg3_dup_self", "SELECT a.n.sk FROM T_S1 AS a, T_S1 AS a"},
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
