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
	It("keeps quoted lowercase aliases distinct during EXISTS admission", func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		env, err := SetupTenantEnvironment(ctx, sharedContainer, "existsquoted_"+uuid.NewString())
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = env.Cleanup(ctx) }()
		srv, err := NewIsolatedJavaInvoker()
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = srv.Close() }()
		javaRunner := plandiff.NewJavaRunnerHTTP(javaBaseURL(srv), env.ClusterFile).(plandiff.SetupRunner)
		file := writeClusterFileToTemp(env.ClusterFile)
		defer os.Remove(file)
		goRunner := plandiff.NewGoSQLSetupRunner(file)
		const schema = `CREATE TABLE t (id BIGINT, arr BIGINT ARRAY, PRIMARY KEY (id)) CREATE TABLE u (id BIGINT, PRIMARY KEY (id)) CREATE TABLE v (id BIGINT, PRIMARY KEY (id))`
		setup := []string{`INSERT INTO t VALUES (1, [1]), (2, [2])`, `INSERT INTO u VALUES (1)`, `INSERT INTO v VALUES (1)`}
		want := [][]any{{float64(1)}}
		var failures []string
		for _, test := range []struct {
			name, sql   string
			unnest      bool
			independent bool
			ordered     bool
		}{
			{"quoted_outer", `SELECT "a".id FROM t AS "a" WHERE EXISTS (SELECT 1 FROM u AS A JOIN v AS B ON A.id = B.id WHERE A.id = "a".id)`, false, false, false},
			{"quoted_inner", `SELECT A.id FROM t AS A WHERE EXISTS (SELECT 1 FROM u AS "a" JOIN v AS B ON "a".id = B.id WHERE "a".id = A.id)`, false, false, false},
			{"distinct_letter_control", `SELECT O.id FROM t AS O WHERE EXISTS (SELECT 1 FROM u AS A JOIN v AS B ON A.id = B.id WHERE A.id = O.id)`, false, false, false},
			{"later_inner_alias", `SELECT "a".id FROM t AS "a" WHERE EXISTS (SELECT 1 FROM u AS X JOIN v AS B ON B.id = "a".id JOIN u AS A ON A.id = X.id)`, false, false, false},
			{"unnest_frame", `SELECT "a" FROM t, t.arr AS "a" WHERE EXISTS (SELECT 1 FROM u AS A JOIN v AS B ON A.id = B.id WHERE A.id = "a")`, true, false, false},
			{"unnest_distinct_letter_control", `SELECT o FROM t, t.arr AS o WHERE EXISTS (SELECT 1 FROM u AS A JOIN v AS B ON A.id = B.id WHERE A.id = o)`, true, false, false},
			{"independent_unnest_frame", `SELECT "a" FROM t, t.arr AS "a" WHERE EXISTS (SELECT 1 FROM u AS A JOIN v AS B ON A.id = B.id WHERE A.id = 1)`, true, true, false},
			{"independent_unnest_control", `SELECT o FROM t, t.arr AS o WHERE EXISTS (SELECT 1 FROM u AS A JOIN v AS B ON A.id = B.id WHERE A.id = 1)`, true, true, false},
			{"ordered_unnest_frame", `SELECT "a" FROM t, t.arr AS "a" WHERE EXISTS (SELECT 1 FROM u AS A JOIN v AS B ON A.id = B.id WHERE A.id = 1) ORDER BY 1`, true, true, true},
			{"ordered_unnest_control", `SELECT o FROM t, t.arr AS o WHERE EXISTS (SELECT 1 FROM u AS A JOIN v AS B ON A.id = B.id WHERE A.id = 1) ORDER BY 1`, true, true, true},
		} {
			for _, runner := range []plandiff.SetupRunner{javaRunner, goRunner} {
				wantRows := want
				if test.independent {
					wantRows = [][]any{{float64(1)}, {float64(2)}}
				}
				result := runner.RunWithSetup(ctx, schema, setup, test.sql)
				fmt.Fprintf(GinkgoWriter, "BOUND-EXISTS-QUOTED %s %s rows=%v err=%v\n", test.name, result.Engine, result.Rows.Rows, result.Err)
				// Multi-source EXISTS reading an outer UNNEST element has a
				// separate translator restriction, also reached with different
				// letters. Fixing lexical equality must not remove that boundary.
				if result.Engine == "go" && test.unnest && !test.independent {
					var typed *api.Error
					if !errors.As(result.Err, &typed) || typed.Code != api.ErrCodeUnsupportedQuery || typed.Message != "EXISTS with a multi-table FROM referencing the unnest element is not supported" {
						failures = append(failures, fmt.Sprintf("%s: expected retained multi-source UNNEST restriction, got %v", test.name, result.Err))
					}
					continue
				}
				// Java cannot satisfy this ordered lateral shape; Go has an
				// in-memory sort fallback. Keep the ordered probe separate from
				// the unordered SQL contract, which requires an exact multiset.
				if result.Engine == "java" && test.ordered {
					var typed *plandiff.JavaError
					if !errors.As(result.Err, &typed) || typed.ExceptionClass != "UnableToPlanException" {
						failures = append(failures, fmt.Sprintf("%s: expected Java ordering restriction, got %v", test.name, result.Err))
					}
					continue
				}
				matches, matchErr := ConsistOf(wantRows).Match(result.Rows.Rows)
				if test.ordered {
					matches, matchErr = Equal(wantRows).Match(result.Rows.Rows)
				}
				if result.Err != nil || matchErr != nil || !matches {
					failures = append(failures, fmt.Sprintf("%s/%s: rows=%v err=%v matcher=%v, want %v", test.name, result.Engine, result.Rows.Rows, result.Err, matchErr, wantRows))
				}
			}
		}
		Expect(failures).To(BeEmpty())
	})

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
