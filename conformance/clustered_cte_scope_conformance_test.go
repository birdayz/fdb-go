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

var _ = Describe("ClusteredCTEScopeConformance", func() {
	It("preserves a shared definition's outer reference under either consumer order", func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		env, err := SetupTenantEnvironment(ctx, sharedContainer, "clusteredcte_"+uuid.NewString())
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = env.Cleanup(ctx) }()
		srv, err := NewIsolatedJavaInvoker()
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = srv.Close() }()
		javaRunner := plandiff.NewJavaRunnerHTTP(javaBaseURL(srv), env.ClusterFile).(plandiff.SetupRunner)
		file := writeClusterFileToTemp(env.ClusterFile)
		defer os.Remove(file)
		goRunner := plandiff.NewGoSQLSetupRunner(file)
		const schema = `CREATE TABLE t (id BIGINT, PRIMARY KEY (id)) CREATE TABLE u (id BIGINT, PRIMARY KEY (id)) CREATE TABLE v (id BIGINT, PRIMARY KEY (id))`
		setup := []string{`INSERT INTO t VALUES (1), (2)`, `INSERT INTO u VALUES (1)`, `INSERT INTO v VALUES (1), (2)`}
		want := [][]any{{float64(1), float64(1)}, {float64(2), nil}}
		var failures []string
		for _, alias := range []string{"A", `"a"`} {
			for _, reverse := range []bool{false, true} {
				from := "c " + alias + " JOIN c x ON " + alias + ".id = x.id"
				if reverse {
					from = "c x JOIN c " + alias + " ON " + alias + ".id = x.id"
				}
				query := "SELECT " + alias + ".id, (WITH c AS (SELECT i.id AS id, " + alias + ".id AS outer_id FROM u i WHERE i.id = " + alias + ".id) SELECT x.outer_id FROM " + from + ") FROM t " + alias + " JOIN v b ON b.id = " + alias + ".id"
				for _, runner := range []plandiff.SetupRunner{javaRunner, goRunner} {
					result := runner.RunWithSetup(ctx, schema, setup, query)
					fmt.Fprintf(GinkgoWriter, "CLUSTERED-CTE-SCOPE alias=%s reverse=%t %s rows=%v err=%v\n", alias, reverse, result.Engine, result.Rows.Rows, result.Err)
					// The pinned Java grammar rejects WITH inside a scalar
					// expression. Go already supports this read-side shape; its
					// rows are independently fixed by the seeded data, not Java.
					if result.Engine == "java" {
						var typed *plandiff.JavaError
						if !errors.As(result.Err, &typed) || typed.SQLState != "42601" || typed.ExceptionClass != "RelationalException" {
							failures = append(failures, fmt.Sprintf("Java scalar WITH grammar boundary changed: %v", result.Err))
						}
						continue
					}
					matches, matchErr := ConsistOf(want).Match(result.Rows.Rows)
					if result.Err != nil || matchErr != nil || !matches {
						failures = append(failures, fmt.Sprintf("alias=%s reverse=%t/%s rows=%v err=%v matcher=%v", alias, reverse, result.Engine, result.Rows.Rows, result.Err, matchErr))
					}
				}
			}
		}
		Expect(failures).To(BeEmpty())
	})
})
