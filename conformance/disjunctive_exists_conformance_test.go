//go:build bazelrunfiles

package conformance_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"fdb.dev/pkg/relational/conformance/plandiff"
	"github.com/google/uuid"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("DisjunctiveExistsConformance", func() {
	It("evaluates complete boolean consumers after the existential witness", func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		env, err := SetupTenantEnvironment(ctx, sharedContainer, "disjexists_"+uuid.NewString())
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = env.Cleanup(ctx) }()
		srv, err := NewIsolatedJavaInvoker()
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = srv.Close() }()
		javaRunner := plandiff.NewJavaRunnerHTTP(javaBaseURL(srv), env.ClusterFile).(plandiff.SetupRunner)
		file := writeClusterFileToTemp(env.ClusterFile)
		defer os.Remove(file)
		goRunner := plandiff.NewGoSQLSetupRunner(file)
		const schema = `CREATE TABLE t (id BIGINT, a BIGINT, arr BIGINT ARRAY, PRIMARY KEY(id))
			CREATE TABLE u (id BIGINT, k BIGINT, PRIMARY KEY(id)) CREATE TABLE v (id BIGINT, PRIMARY KEY(id))`
		setup := []string{
			`INSERT INTO t VALUES (1,0,[1,9,9]),(2,9,[3,9]),(3,NULL,[]),(4,9,NULL),(5,9,[]),(6,0,[])`,
			`INSERT INTO u VALUES (1,99),(2,1),(3,1),(4,3),(5,4)`, `INSERT INTO v VALUES (2),(4)`,
		}
		var failures []string
		for _, tc := range []struct {
			name, where string
			want        []int
		}{
			{"correlated", `a=9 OR EXISTS (SELECT 1 FROM u WHERE u.k=t.id)`, []int{1, 2, 3, 4, 5}},
			{"negative", `a=9 OR NOT EXISTS (SELECT 1 FROM u WHERE u.k=t.id)`, []int{2, 4, 5, 6}},
			{"independent_true", `a=9 OR EXISTS (SELECT 1 FROM u WHERE k=1)`, []int{1, 2, 3, 4, 5, 6}},
			{"independent_false", `a=9 OR EXISTS (SELECT 1 FROM u WHERE k=100)`, []int{2, 4, 5}},
			{"null_or_false", `a=99 OR EXISTS (SELECT 1 FROM u WHERE u.k=t.id)`, []int{1, 3, 4}},
			{"two_exists", `EXISTS (SELECT 1 FROM u WHERE u.k=t.id) OR EXISTS (SELECT 1 FROM v WHERE v.id=t.id)`, []int{1, 2, 3, 4}},
			{"three_exists", `EXISTS (SELECT 1 FROM u WHERE u.k=t.id) OR EXISTS (SELECT 1 FROM v WHERE v.id=t.id) OR EXISTS (SELECT 1 FROM u WHERE u.id=t.id)`, []int{1, 2, 3, 4, 5}},
			{"independent_pair_false", `EXISTS (SELECT 1 FROM u WHERE k=100) OR EXISTS (SELECT 1 FROM v WHERE id=100)`, nil},
			{"independent_pair_true", `EXISTS (SELECT 1 FROM u WHERE k=100) OR EXISTS (SELECT 1 FROM v)`, []int{1, 2, 3, 4, 5, 6}},
			{"two_negative", `NOT EXISTS (SELECT 1 FROM u WHERE u.k=t.id) OR NOT EXISTS (SELECT 1 FROM v WHERE v.id=t.id)`, []int{1, 2, 3, 5, 6}},
			{"nested_boolean", `(a=9 AND EXISTS (SELECT 1 FROM u WHERE u.k=t.id)) OR (id=6 AND NOT EXISTS (SELECT 1 FROM v WHERE v.id=t.id))`, []int{4, 6}},
			{"not_or", `NOT (a=9 OR EXISTS (SELECT 1 FROM u WHERE u.k=t.id))`, []int{6}},
			{"nested_child", `EXISTS (SELECT 1 FROM u WHERE u.k=t.id OR EXISTS (SELECT 1 FROM v WHERE v.id=t.id))`, []int{1, 2, 3, 4}},
			{"negative_nested_child", `NOT EXISTS (SELECT 1 FROM u WHERE u.k=t.id OR EXISTS (SELECT 1 FROM v WHERE v.id=t.id))`, []int{5, 6}},
			{"known_true", `a=99 OR EXISTS (SELECT COUNT(*) FROM u WHERE u.k=t.id)`, []int{1, 2, 3, 4, 5, 6}},
			{"known_false", `a=9 OR EXISTS (SELECT id FROM u WHERE u.k=t.id LIMIT 0)`, []int{2, 4, 5}},
		} {
			want := make([][]any, len(tc.want))
			for i, id := range tc.want {
				want[i] = []any{float64(id)}
			}
			for _, runner := range []plandiff.SetupRunner{javaRunner, goRunner} {
				result := runner.RunWithSetup(ctx, schema, setup, `SELECT id FROM t WHERE `+tc.where)
				fmt.Fprintf(GinkgoWriter, "DISJ-EXISTS %s %s rows=%v err=%v\n", tc.name, result.Engine, result.Rows.Rows, result.Err)
				// Subquery LIMIT is a Go extension; Java rejects it before planning.
				if tc.name == "known_false" && result.Engine == "java" {
					var javaErr *plandiff.JavaError
					if !errors.As(result.Err, &javaErr) || javaErr.Message != "LIMIT clause is not supported." {
						failures = append(failures, fmt.Sprintf("subquery LIMIT admission changed: %v", result.Err))
					}
					continue
				}
				matches, matchErr := ConsistOf(want).Match(result.Rows.Rows)
				if result.Err != nil || matchErr != nil || !matches {
					failures = append(failures, fmt.Sprintf("%s/%s rows=%v err=%v want=%v", tc.name, result.Engine, result.Rows.Rows, result.Err, want))
				}
			}
		}
		Expect(failures).To(BeEmpty())
	})
})
