//go:build bazelrunfiles

package conformance_test

// Measures which fault each engine reports for a query faulting in both its
// select list and its WHERE. Java's QueryVisitor resolves FROM, then WHERE,
// then the select list (QueryVisitor.java:272-274 before :283-322), so the
// WHERE's fault wins; Go's catalog SELECT builder resolves the select list
// first. TODO.md "A query faulting in both its select list and its WHERE
// reports the select list's fault; Java reports the WHERE's" is the work; this
// pins what both engines answer today, so either moving reddens it. When Go
// resolves the WHERE first, the first two shapes' Go answers become Java's.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/google/uuid"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/conformance/plandiff"
)

var _ = Describe("SelectWherePrecedenceJavaProbe", func() {
	It("pins which fault each engine reports when the select list and the WHERE both fault", func() {
		ctx := context.Background()
		env, err := SetupTenantEnvironment(ctx, sharedContainer, fmt.Sprintf("swprec_%s", uuid.New().String()))
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = env.Cleanup(ctx) }()
		srv, err := NewIsolatedJavaInvoker()
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = srv.Close() }()
		javaRunner := plandiff.NewJavaRunnerHTTP(javaBaseURL(srv), env.ClusterFile).(plandiff.SetupRunner)
		clusterFilePath := writeClusterFileToTemp(env.ClusterFile)
		defer os.Remove(clusterFilePath)
		goRunner := plandiff.NewGoSQLSetupRunner(clusterFilePath)

		// `f` is a FLOAT: `f & 1` has no bit lane, a planning fault of its own.
		const schema = `CREATE TABLE t (id BIGINT, f FLOAT, PRIMARY KEY (id))`
		setup := []string{"INSERT INTO t VALUES (1, CAST(1.5 AS FLOAT))"}
		outcome := func(r plandiff.RunResult) string {
			if r.Err == nil {
				return "ACCEPT"
			}
			var je *plandiff.JavaError
			if errors.As(r.Err, &je) {
				return je.SQLState + " " + je.Message
			}
			var ge *api.Error
			if errors.As(r.Err, &ge) {
				return string(ge.Code) + " " + ge.Message
			}
			return "? " + r.Err.Error()
		}
		// Each engine's SQLSTATE and a fragment its message must hold: the
		// column a 42703 names, or the fault an XX000 reports.
		type want struct{ state, holds string }
		for _, c := range []struct {
			sql        string
			java, goes want
		}{
			{`SELECT nosucha FROM t WHERE nosuchb = 1`, want{"42703", "NOSUCHB"}, want{"42703", "NOSUCHA"}},
			{`SELECT f & 1 FROM t WHERE nosuchb = 1`, want{"42703", "NOSUCHB"}, want{"XX000", "unable to encapsulate arithmetic operation"}},
			{`SELECT id FROM t WHERE nosuchb = 1 AND f & 1 = 1`, want{"42703", "NOSUCHB"}, want{"42703", "NOSUCHB"}},
		} {
			j := outcome(javaRunner.RunWithSetup(ctx, schema, setup, c.sql))
			g := outcome(goRunner.RunWithSetup(ctx, schema, setup, c.sql))
			GinkgoWriter.Printf("SWPREC %s\n  java=%s\n  go  =%s\n", c.sql, j, g)
			for _, side := range []struct {
				engine, got string
				want        want
			}{{"java", j, c.java}, {"go", g, c.goes}} {
				Expect(strings.HasPrefix(side.got, side.want.state+" ")).To(BeTrue(),
					"%s: %s answers %q, pinned %s", c.sql, side.engine, side.got, side.want.state)
				Expect(strings.ToUpper(side.got)).To(ContainSubstring(strings.ToUpper(side.want.holds)),
					"%s: %s answers %q, pinned to name %q", c.sql, side.engine, side.got, side.want.holds)
			}
		}
	})
})
