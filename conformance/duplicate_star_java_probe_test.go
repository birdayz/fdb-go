//go:build bazelrunfiles

package conformance_test

// Records Java's live behaviour (tag 4.12.11.0 conformance server) for the
// DUPLICATE qualified star — `SELECT a.*, a.* FROM a` and the derived-table
// reference over it, which select-a-star.yamsql asserts as AMBIGUOUS_COLUMN.
//
// The question the probe settles is WHERE the error lives. Java's expandStar
// has no uniqueness rule (SemanticAnalyzer.java:332-347 resolves each star
// independently), so producing a row with two same-named attributes is legal;
// AMBIGUOUS_COLUMN comes from resolveIdentifier's lookup returning more than
// one matching attribute (SemanticAnalyzer.java:417,:422). Go used to reject
// the PRODUCER with 22023, which reported an error on a query Java answers
// with rows — and reported it on the inner select, before the outer reference
// that is the actual error.
//
// It makes NO assertions; it prints each shape's outcome for both engines so
// the pinned expectations in
// pkg/relational/sqldriver/ambiguous_column_star_test.go can be diffed
// against the live JVM whenever either engine changes.

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

var _ = Describe("DuplicateStarJavaProbe", func() {
	It("records Java's live behaviour for duplicate qualified-star shapes", func() {
		ctx := context.Background()
		tenantName := fmt.Sprintf("dupstar_%s", uuid.New().String())
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

		schema := "CREATE TABLE T_A (id BIGINT, name STRING, PRIMARY KEY (id))" +
			" CREATE TABLE T_B (bid BIGINT, PRIMARY KEY (bid))"
		setup := []string{
			"INSERT INTO T_A VALUES (1, 'alpha'), (2, 'beta')",
			"INSERT INTO T_B VALUES (7)",
		}

		probes := []struct{ name, sql string }{
			// The PRODUCER, alone. Java: legal, four columns.
			{"dup_star_alone", "SELECT a.* , a.* FROM T_A AS a"},
			// The producer with a DISJOINT second source, so the duplication
			// cannot be confused with a join-widening.
			{"dup_star_with_other", "SELECT a.*, a.* FROM T_A AS a, T_B AS b"},
			// The CONSUMER: an outer bare reference to a name the derived row
			// carries twice. select-a-star.yamsql asserts AMBIGUOUS_COLUMN.
			{"dup_star_derived_bare_ref", "SELECT id FROM (SELECT a.*, a.* FROM T_A AS a) AS nested"},
			// The consumer's qualified spelling.
			{"dup_star_derived_qual_ref", "SELECT nested.id FROM (SELECT a.*, a.* FROM T_A AS a) AS nested"},
			// A reference to a name the derived row carries ONCE, over a body
			// that duplicates a DIFFERENT name — must still answer.
			{"dup_star_derived_unique_ref", "SELECT bid FROM (SELECT a.*, a.*, b.bid FROM T_A AS a, T_B AS b) AS nested"},
			// Star over the duplicating derived table: no reference is made,
			// so no ambiguity — the row just carries the columns twice.
			{"dup_star_derived_star", "SELECT * FROM (SELECT a.*, a.* FROM T_A AS a) AS nested"},
			// Three stars, to show the count is not a two-only special case.
			{"triple_star", "SELECT a.*, a.*, a.* FROM T_A AS a"},
			// The unqualified-star twin of the producer: does Java allow a
			// bare `*` alongside a qualified star at all?
			{"bare_star_plus_qual_star", "SELECT *, a.* FROM T_A AS a"},
			// A qualified star under GROUP BY. Java expands the star FIRST and
			// then requires each expanded output to be composable from the
			// grouping expressions plus the aggregates plus the outer
			// correlations (LogicalOperator.java:435-441, isComposableFrom →
			// GROUPING_ERROR). So a star whose columns are EXACTLY the grouping
			// list is legal, and only a star reaching past it is 42803.
			{"group_by_star_covers", "SELECT a.* FROM T_A AS a GROUP BY id, name"},
			{"group_by_star_exceeds", "SELECT a.* FROM T_A AS a GROUP BY id"},
			// The same, correlated: the star of an OUTER source is composable
			// through outerCorrelations even though it is in no GROUP BY.
			{"group_by_star_correlated", "SELECT b.bid FROM T_B AS b WHERE EXISTS (SELECT a.*, b.bid FROM T_A AS a GROUP BY id, name)"},
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
