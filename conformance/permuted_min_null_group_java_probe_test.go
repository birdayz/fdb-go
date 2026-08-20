//go:build bazelrunfiles

package conformance_test

// Does JAVA answer MIN correctly over a group that holds a NULL beside real
// values?
//
// Go did not. A PERMUTED_MIN index stores one extremum per group; a NULL-valued
// record produces an index entry like any other; and NULL sorts before every
// value, so it won the comparison unconditionally and a mixed group read back as
// NULL. Go now repairs that at READ time — a stored NULL extremum is resolved
// against the index's ordinary subspace — and deliberately does NOT change what
// it writes, because Java's PermutedMinMaxIndexMaintainer has no NULL filter
// either and its getExtremum takes the first entry of the group scan. The stored
// bytes are therefore identical in both engines, which is the property that lets
// them share a cluster.
//
// That reasoning says the stored state matches. It does NOT say what Java
// ANSWERS, and the fix was shipped with the Java side recorded as unverified.
// This probe is that verification, and it decides which of two things the repair
// is:
//
//   - if Java answers 5 for a group holding {5, NULL}, Java resolves the same
//     stored NULL somewhere Go did not, and the repair restores PARITY;
//   - if Java answers NULL, both engines were wrong the same way, the repair
//     puts Go in the DivergenceJavaWrongRowsGoCorrect direction, and that has to
//     be booked and reported upstream rather than left as folklore.
//
// Either way this file holds the answer as a measurement that fails if Java
// moves, which a comment could not.
//
// MAX is measured beside MIN throughout. NULL sorts LOWEST, so a real value
// always beats it for MAX and the same fixture asks a question with a known
// answer — the control that says the harness and the fixture are sound when the
// MIN rows disagree.

import (
	"context"
	"fmt"
	"os"
	"strings"

	"fdb.dev/pkg/relational/conformance/plandiff"
	"github.com/google/uuid"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("PermutedMinNullGroupJavaProbe", func() {
	It("measures both engines on MIN over a group holding NULLs", func() {
		ctx := context.Background()
		tenantName := fmt.Sprintf("minnullgrp_%s", uuid.New().String())
		env, err := SetupTenantEnvironment(ctx, sharedContainer, tenantName)
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = env.Cleanup(ctx) }()

		srv, err := NewIsolatedJavaInvoker()
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = srv.Close() }()
		javaRunner := plandiff.NewJavaRunnerHTTP(javaBaseURL(srv), env.ClusterFile).(plandiff.SetupRunner)
		clusterFilePath := writeClusterFileToTemp(env.ClusterFile)
		defer os.Remove(clusterFilePath)
		goRunner := plandiff.NewGoSQLSetupRunner(clusterFilePath)

		const schema = "CREATE TABLE t (pk BIGINT, g BIGINT, v BIGINT, PRIMARY KEY (pk)) " +
			"CREATE INDEX t_min_v AS SELECT MIN(v) FROM t GROUP BY g " +
			"CREATE INDEX t_max_v AS SELECT MAX(v) FROM t GROUP BY g"
		// g=1 mixed (NULL beside 5 and 9)   -> MIN 5,    MAX 9
		// g=2 all NULL                      -> MIN NULL, MAX NULL
		// g=3 no NULLs                      -> MIN 2,    MAX 8
		// g=4 mixed with negatives          -> MIN -4,   MAX 0
		setup := []string{
			"INSERT INTO t (pk,g,v) VALUES (1,1,5),(2,1,NULL),(3,1,9)," +
				"(4,2,NULL),(5,2,NULL)," +
				"(6,3,2),(7,3,8)," +
				"(8,4,0),(9,4,NULL),(10,4,-4)",
		}

		render := func(r plandiff.RunResult) string {
			if r.Err != nil {
				return "ERR(" + r.Err.Error() + ")"
			}
			return fmt.Sprint(r.Rows.Rows)
		}

		for _, q := range []string{
			"SELECT g, MIN(v) FROM t GROUP BY g",
			"SELECT g, MAX(v) FROM t GROUP BY g",
			"SELECT MIN(v) FROM t WHERE g = 1",
			"SELECT MIN(v) FROM t WHERE g = 2",
		} {
			javaOut := render(javaRunner.RunWithSetup(ctx, schema, setup, q))
			goOut := render(goRunner.RunWithSetup(ctx, schema, setup, q))
			fmt.Fprintf(GinkgoWriter, "\n=== %s\n   java: %s\n   go  : %s\n", q, javaOut, goOut)
		}

		// The claim under test, isolated: the grouped MIN.
		javaMin := javaRunner.RunWithSetup(ctx, schema, setup, "SELECT g, MIN(v) FROM t GROUP BY g")
		goMin := goRunner.RunWithSetup(ctx, schema, setup, "SELECT g, MIN(v) FROM t GROUP BY g")
		Expect(goMin.Err).NotTo(HaveOccurred())

		// Go's answer is the SQL-correct one and is asserted absolutely, so this
		// probe cannot be satisfied by both engines drifting together.
		goRows := fmt.Sprint(goMin.Rows.Rows)
		Expect(goRows).To(ContainSubstring("5"),
			"Go no longer answers MIN=5 for a group holding {5, NULL, 9}; the permuted-MIN NULL "+
				"repair has regressed. Rows: %s", goRows)

		// MAX is the control: NULL never wins it, so both engines must agree
		// here whatever they do about MIN. If this disagrees, the fixture or the
		// harness is at fault and the MIN comparison below means nothing.
		javaMax := render(javaRunner.RunWithSetup(ctx, schema, setup, "SELECT g, MAX(v) FROM t GROUP BY g"))
		goMax := render(goRunner.RunWithSetup(ctx, schema, setup, "SELECT g, MAX(v) FROM t GROUP BY g"))
		Expect(goMax).To(Equal(javaMax),
			"the engines disagree on MAX, which NULL cannot affect — so this fixture is not measuring "+
				"what it claims and the MIN result below is not interpretable.\n  java: %s\n  go  : %s",
			javaMax, goMax)

		// THE MEASUREMENT. Recorded as an assertion so the classification cannot
		// go stale: if Java starts answering 5, the divergence closed and the
		// repair became parity; if it still answers NULL, both engines were
		// wrong together and Go is deliberately ahead.
		javaRows := render(javaMin)
		fmt.Fprintf(GinkgoWriter, "\nPROBE SUMMARY grouped MIN\n   java: %s\n   go  : %s\n", javaRows, goRows)
		if javaRows == goRows {
			Fail("Java now agrees with Go on MIN over a mixed NULL group, so this is no longer a " +
				"divergence: re-arm this probe as an agreement assertion and update the note in " +
				"pkg/recordlayer/permuted_min_null_semantics.go, which records the Java side as the " +
				"reason the repair is read-side only.\n  both: " + goRows)
		}
		fmt.Fprintf(GinkgoWriter,
			"\nMEASURED DIVERGENCE: Java does not answer the SQL-correct MIN for a group holding a "+
				"NULL beside real values; Go does, since the read-side repair. Direction: "+
				"%s.\n", plandiff.DivergenceJavaWrongRowsGoCorrect)
		_ = strings.TrimSpace(javaRows)
	})
})
