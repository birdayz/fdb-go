//go:build bazelrunfiles

package conformance_test

// RFC-173 QP-REF-BIND item 1 — TEMPORARY live-Java design-premise probe.
//
// Records Java's live behaviour (tag 4.12.11.0 conformance server) for the
// duplicate-FROM-alias shapes the item-1 design substrate hinges on: per-
// attribute reference resolution, SELECT-* duplicate-column layout, the
// qualified-star/table-row findFirst quirks, and the exact 42702 message
// text. The F3 lesson (RFC-173) demands every Java-behaviour premise be
// live-verified before banking — this probe is that verification. It makes
// NO assertions; it prints each shape's outcome for the RFC record and is
// DELETED once the findings are pinned as corpus entries.

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

var _ = Describe("RFC173Item1Probe", func() {
	It("records Java's live behaviour for duplicate FROM-alias shapes", func() {
		ctx := context.Background()
		tenantName := fmt.Sprintf("rfc173i1_%s", uuid.New().String())
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
		schema := "CREATE TABLE T_P1 (id BIGINT, v BIGINT, PRIMARY KEY (id))" +
			" CREATE TABLE T_Q1 (qid BIGINT, PRIMARY KEY (qid))" +
			" CREATE TABLE T_R1 (id BIGINT, rv BIGINT, PRIMARY KEY (id))"
		setup := []string{
			"INSERT INTO T_P1 VALUES (1, 10), (2, 20)",
			"INSERT INTO T_Q1 VALUES (7)",
			"INSERT INTO T_R1 VALUES (5, 50)",
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
