//go:build bazelrunfiles

package conformance_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/conformance/plandiff"
	"github.com/google/uuid"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// docClaimSchemas are the schemas the DocClaimsJavaProbe cases run under, with
// their setup; each DDL claim has a schema of its own.
var docClaimSchemas = map[string]struct {
	ddl   string
	setup []string
}{
	"main": {
		ddl: "CREATE TABLE t (id BIGINT, v BIGINT, s STRING, d DOUBLE, b BOOLEAN, i INTEGER, PRIMARY KEY (id)) " +
			"CREATE TABLE u (id BIGINT, t_id BIGINT, w BIGINT, PRIMARY KEY (id)) " +
			"CREATE TABLE arr (id BIGINT, xs BIGINT ARRAY, PRIMARY KEY (id)) " +
			"CREATE TABLE bt (id BIGINT, bs BYTES, PRIMARY KEY (id)) " +
			"CREATE TABLE g (id BIGINT, k BIGINT, PRIMARY KEY (id)) " +
			"CREATE INDEX t_v AS SELECT v FROM t ORDER BY v " +
			"CREATE UNIQUE INDEX u_w AS SELECT w FROM u ORDER BY w",
		setup: []string{
			"INSERT INTO t VALUES (1, 10, 'a', 1.5, true, 1), (2, 20, 'b', -2.5, false, 2), (3, 10, 'c', 0.0, NULL, 3)",
			"INSERT INTO u VALUES (1, 1, 5), (2, 3, 6), (3, 9, 7)",
			"INSERT INTO arr VALUES (1, [1, 2, 3]), (2, [])",
			"INSERT INTO bt VALUES (1, x'cafe'), (2, x'00')",
			"INSERT INTO g VALUES (1, 5), (2, 5), (3, 6)",
		},
	},
	"tree": {
		ddl: "CREATE TABLE t1 (id BIGINT, parent BIGINT, PRIMARY KEY (id)) " +
			"CREATE INDEX parent_idx AS SELECT parent, id FROM t1 ORDER BY parent, id " +
			"CREATE INDEX child_idx AS SELECT id, parent FROM t1 ORDER BY id, parent",
		setup: []string{"INSERT INTO t1 VALUES (1, -1), (10, 1), (20, 1), (40, 10), (50, 10)"},
	},
	"diamond": {
		ddl: "CREATE TABLE e (id BIGINT, parent BIGINT, PRIMARY KEY (id, parent)) " +
			"CREATE INDEX parent_idx AS SELECT parent FROM e ORDER BY parent",
		setup: []string{"INSERT INTO e VALUES (1, -1), (2, 1), (3, 1), (4, 2), (4, 3)"},
	},
	"date_column":      {ddl: "CREATE TABLE dt (id BIGINT, d DATE, PRIMARY KEY (id))"},
	"timestamp_column": {ddl: "CREATE TABLE ts (id BIGINT, d TIMESTAMP, PRIMARY KEY (id))"},
	"scalar_not_null":  {ddl: "CREATE TABLE nn (id BIGINT, v BIGINT NOT NULL, PRIMARY KEY (id))"},
	"array_not_null":   {ddl: "CREATE TABLE na (id BIGINT, xs BIGINT ARRAY NOT NULL, PRIMARY KEY (id))"},
}

var generatedAlias = regexp.MustCompile(`q[0-9a-f]{8}(_[0-9a-f]{4}){3}_[0-9a-f]{12}`)

// renderDocClaim renders one engine's answer. Rows are sorted unless the query
// orders them, so a probe pins what an engine answers, not its plan's order.
func renderDocClaim(result plandiff.RunResult, sql string) string {
	if result.Err != nil {
		var je *plandiff.JavaError
		var ge *api.Error
		if errors.As(result.Err, &je) {
			// Java names a quantifier by a random UUID in some messages.
			msg := generatedAlias.ReplaceAllString(je.Message, "q<uuid>")
			return fmt.Sprintf("ERROR %s %s %q", je.ExceptionClass, je.SQLState, msg)
		}
		if errors.As(result.Err, &ge) {
			return fmt.Sprintf("ERROR %s %q", ge.Code, ge.Message)
		}
		return fmt.Sprintf("ERROR %v", result.Err)
	}
	rows := make([]string, 0, len(result.Rows.Rows))
	for _, r := range result.Rows.Rows {
		encoded, err := json.Marshal(r)
		Expect(err).NotTo(HaveOccurred())
		rows = append(rows, string(encoded))
	}
	if upper := strings.ToUpper(sql); !strings.Contains(upper, "ORDER BY") && !strings.Contains(upper, "TRAVERSAL ORDER") {
		sort.Strings(rows)
	}
	columns, err := json.Marshal(result.Rows.Columns)
	Expect(err).NotTo(HaveOccurred())
	return fmt.Sprintf("%s [%s]", columns, strings.Join(rows, ","))
}

// DocClaimsJavaProbe pins Java 4.14.2.0's answer to every Java claim of
// DIVERGENCES.md's "Go-Only Extensions" table and SQL_CONFORMANCE.md's matrix.
var _ = Describe("DocClaimsJavaProbe", func() {
	It("pins Java's and Go's answer to each documented feature claim", func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
		defer cancel()
		env, err := SetupTenantEnvironment(ctx, sharedContainer, "docclaims_"+uuid.NewString())
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = env.Cleanup(ctx) }()
		srv, err := NewIsolatedJavaInvoker()
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = srv.Close() }()
		javaRunner := plandiff.NewJavaRunnerHTTP(javaBaseURL(srv), env.ClusterFile).(plandiff.SetupRunner)
		clusterFile := writeClusterFileToTemp(env.ClusterFile)
		defer os.Remove(clusterFile)
		goRunner := plandiff.NewGoSQLSetupRunner(clusterFile)

		// Every case runs before the verdict, so one run reports all drifted pins.
		var drift []string
		for _, c := range docClaimCases {
			schema, ok := docClaimSchemas[c.schema]
			Expect(ok).To(BeTrue(), c.name)
			setup := append(append([]string(nil), schema.setup...), c.setup...)
			jr := javaRunner.RunWithSetup(ctx, schema.ddl, setup, c.sql)
			gr := goRunner.RunWithSetup(ctx, schema.ddl, setup, c.sql)
			java, goAnswer := renderDocClaim(jr, c.sql), renderDocClaim(gr, c.sql)
			fmt.Fprintf(GinkgoWriter, "DOC-CLAIM %s\n JAVA %s\n GO %s\n SQL %s\n", c.name, java, goAnswer, c.sql)
			if want, ok := docClaimWantJava[c.name]; !ok || java != want {
				drift = append(drift, fmt.Sprintf("Java %q: %q,", c.name, java))
			}
			wantGo, distinct := docClaimWantGo[c.name]
			if !distinct {
				wantGo = docClaimWantJava[c.name]
				if jr.Err != nil {
					drift = append(drift, fmt.Sprintf("Go (Java errored, so Go needs its own pin) %q: %q,", c.name, goAnswer))
					continue
				}
			}
			if goAnswer != wantGo {
				drift = append(drift, fmt.Sprintf("Go %q: %q,", c.name, goAnswer))
			}
		}
		Expect(drift).To(BeEmpty(), "answers that differ from their pins:\n%s", strings.Join(drift, "\n"))
	})
})
