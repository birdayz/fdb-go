//go:build bazelrunfiles

package conformance_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/conformance/plandiff"
	"github.com/google/uuid"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// Java QueryVisitor visits the WHERE expression for SELECT and DML alike.
// ExpressionVisitor.visitExistsExpressionAtom builds its child through that
// same visitor; SemanticAnalyzer.getTable classifies an absent source as
// UNDEFINED_TABLE before there can be an existential producer.
var _ = Describe("BoundExistsSourceConformance", func() {
	It("preserves missing-table classification through SELECT and DML EXISTS construction", func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		env, err := SetupTenantEnvironment(ctx, sharedContainer, "existssource_"+uuid.NewString())
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = env.Cleanup(ctx) }()
		srv, err := NewIsolatedJavaInvoker()
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = srv.Close() }()
		javaRunner := plandiff.NewJavaRunnerHTTP(javaBaseURL(srv), env.ClusterFile).(plandiff.SetupRunner)
		file := writeClusterFileToTemp(env.ClusterFile)
		defer os.Remove(file)
		goRunner := plandiff.NewGoSQLSetupRunner(file)
		const schema = `CREATE TABLE "Customer" (id BIGINT, name STRING, PRIMARY KEY (id))`
		setup := []string{`INSERT INTO "Customer" VALUES (1, 'original')`}
		for _, test := range []struct {
			statement, afterDML string
			want                [][]any
		}{
			{`SELECT id FROM "Customer"`, "", [][]any{{float64(1)}}},
			{`DELETE FROM "Customer"`, `SELECT COUNT(*) FROM "Customer"`, [][]any{{float64(0)}}},
			{`UPDATE "Customer" SET name = 'changed'`, `SELECT name FROM "Customer"`, [][]any{{"changed"}}},
		} {
			for _, missing := range []bool{true, false} {
				source := `"Customer"`
				if missing {
					source = "nosuchtable"
				}
				sql := test.statement + " WHERE EXISTS (SELECT 1 FROM " + source + ")"
				for _, runner := range []plandiff.SetupRunner{javaRunner, goRunner} {
					querySQL, fixture := sql, setup
					if !missing && test.afterDML != "" {
						// The Java query endpoint uses executeQuery. Successful DML
						// must run via setup's executeUpdate, then inspect its effects.
						fixture = append(append([]string(nil), setup...), sql)
						querySQL = test.afterDML
					}
					result := runner.RunWithSetup(ctx, schema, fixture, querySQL)
					fmt.Fprintf(GinkgoWriter, "BOUND-EXISTS-SOURCE %s %s rows=%v err=%v\n", result.Engine, sql, result.Rows.Rows, result.Err)
					if missing {
						var javaErr *plandiff.JavaError
						var goErr *api.Error
						if result.Engine == "java" {
							Expect(errors.As(result.Err, &javaErr)).To(BeTrue(), sql)
							Expect(javaErr.SQLState).To(Equal("42F01"), sql)
						} else {
							Expect(errors.As(result.Err, &goErr)).To(BeTrue(), sql)
							Expect(goErr.Code).To(Equal(api.ErrCodeUndefinedTable), sql)
						}
						continue
					}
					Expect(result.Err).NotTo(HaveOccurred(), sql)
					Expect(result.Rows.Rows).To(Equal(test.want), sql)
				}
			}
		}
	})
})
