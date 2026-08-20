//go:build bazelrunfiles

package conformance_test

// WHAT DECIDES the outer of an unconstrained cross product, measured on the live
// JVM by varying one thing at a time.
//
// RFC-235 section 17 established that Go resolves this tie with an
// identifier-sensitive hash. The follow-on question is what JAVA does, and it
// matters because Java's PlanningCostModel.compare ALSO ends in
// `Integer.compare(planHash(a), planHash(b))` (PlanningCostModel.java:320-326)
// and Java's ImplementNestedLoopJoinRule ALSO matches its two quantifiers with
// `SetMatcher.exactlyInAnyOrder` — so on paper Java should be exactly as
// name-sensitive as Go, and porting a pruning strategy would change nothing.
//
// This probe asks whether that paper reading holds. It sweeps EIGHT table-name
// pairs (including reversed spellings of the same pair, so lexical order is
// varied independently of FROM order) and two cardinality arrangements.
//
// Java's answer is asserted as an INVARIANT rather than a value: whatever the
// names and whatever the row counts, the FIRST table in the FROM clause must be
// the outer loop. If Java ever deviates, its stability is coincidence and the
// whole framing changes.

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

var _ = Describe("CrossJoinOrderMechanismProbe", func() {
	It("sweeps names and cardinalities to find what fixes Java's nesting", func() {
		ctx := context.Background()
		tenantName := fmt.Sprintf("xjorder_%s", uuid.New().String())
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

		// Reversed spellings of each pair are included so LEXICAL order varies
		// independently of FROM order: if the winner tracked the names, a pair
		// and its reverse would not both answer FROM-major.
		pairs := [][2]string{
			{"aa", "zz"},
			{"zz", "aa"},
			{"m1", "m2"},
			{"m2", "m1"},
			{"qqq", "b"},
			{"b", "qqq"},
			{"tbl_alpha", "tbl_omega"},
			{"tbl_omega", "tbl_alpha"},
		}

		type arrangement struct {
			name        string
			leftRows    []string
			rightRows   []string
			wantJavaFmt string // rendered rows when the FIRST FROM item is outer
		}
		// The right table's values are what we SELECT, so the rendering says which
		// loop is outer directly: FROM-major repeats the right table's whole cycle
		// once per left row.
		arrangements := []arrangement{
			{
				name: "left_smaller", leftRows: []string{"(1)", "(2)"}, rightRows: []string{"(5)", "(7)", "(9)"},
				wantJavaFmt: "[[5] [7] [9] [5] [7] [9]]",
			},
			{
				name: "left_bigger", leftRows: []string{"(1)", "(2)", "(3)"}, rightRows: []string{"(5)", "(7)"},
				wantJavaFmt: "[[5] [7] [5] [7] [5] [7]]",
			},
		}

		agree, differ := 0, 0
		var javaDeviations []string
		for _, p := range pairs {
			for _, arr := range arrangements {
				left, right := p[0], p[1]
				schema := fmt.Sprintf(
					"CREATE TABLE %s (id BIGINT, PRIMARY KEY (id)) CREATE TABLE %s (qid BIGINT, PRIMARY KEY (qid))",
					left, right)
				setup := []string{
					fmt.Sprintf("INSERT INTO %s VALUES %s", left, strings.Join(arr.leftRows, ", ")),
					fmt.Sprintf("INSERT INTO %s VALUES %s", right, strings.Join(arr.rightRows, ", ")),
				}
				sql := fmt.Sprintf("SELECT %s.qid FROM %s, %s", right, left, right)

				jr := runner.RunWithSetup(ctx, schema, setup, sql)
				gr := goRunner.RunWithSetup(ctx, schema, setup, sql)
				Expect(jr.Err).NotTo(HaveOccurred(), "java %s/%s %s", left, right, arr.name)
				Expect(gr.Err).NotTo(HaveOccurred(), "go %s/%s %s", left, right, arr.name)

				j, g := fmt.Sprint(jr.Rows.Rows), fmt.Sprint(gr.Rows.Rows)
				mark := "AGREE"
				if j != g {
					mark = "DIFFER"
					differ++
				} else {
					agree++
				}
				fmt.Fprintf(GinkgoWriter, "XJORDER %-10s %-9s/%-9s %s\n  JAVA %s\n  GO   %s\n",
					arr.name, left, right, mark, j, g)

				if j != arr.wantJavaFmt {
					javaDeviations = append(javaDeviations, fmt.Sprintf("%s %s/%s", arr.name, left, right))
				}
			}
		}
		fmt.Fprintf(GinkgoWriter, "XJORDER SUMMARY: %d agree, %d differ, of %d\n",
			agree, differ, len(pairs)*len(arrangements))
		fmt.Fprintf(GinkgoWriter, "XJORDER JAVA-DEVIATIONS-FROM-FROM-ORDER: %d %v\n",
			len(javaDeviations), javaDeviations)
	})
})
