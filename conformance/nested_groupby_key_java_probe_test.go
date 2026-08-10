//go:build bazelrunfiles

package conformance_test

// Measures Java's live behaviour (tag 4.12.11.0 conformance server) for a GROUP
// BY key that DESCENDS INTO a struct column, and for the three-segment
// `alias.struct.member` spelling of the same descent.
//
// Why this exists: the pins in pkg/relational/sqldriver refuse a nested
// grouping key with 0AF00 rather than 42703, on the grounds that Java gets such
// a key PAST semantic analysis — so "undefined column" would misreport a
// reference Java resolves. That was read out of the Java source. This probe
// observes it instead, and the comments it backs cite these rows.
//
// HOW TO READ A JAVA OUTCOME HERE. Live Java's Cascades declines a plain GROUP
// BY with no matching aggregate index ("Cascades planner could not plan query",
// no SQLSTATE), so at this tag a PLANNER DECLINE is what a semantically
// accepted grouping key looks like — including the flat one. Two controls make
// that reading sound rather than convenient, and both are asserted:
//
//   - flat_key_control groups by an ordinary column: the signature of "semantic
//     analysis ACCEPTED this key" (planner decline, no SQLSTATE);
//   - bogus_qualifier_control groups by a qualifier resolving to nothing: the
//     signature of "semantic analysis REFUSED this key" (42703).
//
// Every nested-key row lands on the first signature and never the second. That
// is the measurement behind the 0AF00 choice. Note what it does NOT say: Java
// does not ANSWER a nested grouping key at this tag either — nothing does,
// because the planner declines the flat key too. The claim is about which layer
// refuses, which is exactly the claim the error-code choice rests on.
//
// THE THREE-SEGMENT ROWS ARE A MEASURED GO LIMITATION. Java ANSWERS
// `SELECT a.n.sk FROM t AS a` (three_segment_select_control), so the
// alias-qualified descent resolves in Java. Go refuses that spelling with 42703
// wherever it appears. That divergence predates the nested-key gate and is
// pinned here so it is a known, measured gap rather than a rule anyone can
// mistake for SQL semantics.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/conformance/plandiff"
	"github.com/google/uuid"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("NestedGroupByKeyJavaProbe", func() {
	It("records Java's live outcome for a struct-descending GROUP BY key", func() {
		ctx := context.Background()
		tenantName := fmt.Sprintf("nestedgroup_%s", uuid.New().String())
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

		// T_NG2 differs from T_NG1 in ONE way: it also declares a FLAT column
		// whose name equals the struct member's LEAF. That single difference is
		// what armed the Go escape these probes accompany, so the two tables
		// are a controlled comparison rather than two fixtures.
		schema := "CREATE TYPE AS STRUCT GST (sk BIGINT, co BIGINT)" +
			" CREATE TABLE T_NG1 (id BIGINT, n GST, PRIMARY KEY (id))" +
			" CREATE TABLE T_NG2 (id BIGINT, sk BIGINT, n GST, PRIMARY KEY (id))"
		setup := []string{
			"INSERT INTO T_NG1 VALUES (1, (1, 1)), (2, (1, 2)), (3, (2, 1)), (4, (2, 2)), (5, (1, 1))",
			"INSERT INTO T_NG2 VALUES (1, 90, (1, 1)), (2, 91, (1, 2)), (3, 92, (2, 1))",
		}

		// The expectation classes, and what each one exists to catch:
		//
		//   java_plans_none_go_serves — Java's planner decline with Go serving:
		//       the flat-key signature. If Java ever starts ANSWERING these, the
		//       "planner decline == semantically accepted" reading below is
		//       stale and every nested row must be re-measured against the new
		//       signature.
		//   both_42703 — the semantic-refusal signature. Its presence is what
		//       makes "the nested key is not 42703 in Java" non-vacuous.
		//   both_answer — the reference resolves outside GROUP BY in both.
		//   java_semantic_ok_go_0af00 — the shape under test.
		//   java_semantic_ok_go_42703 — the three-segment GROUP BY spelling:
		//       Java gets it past semantics, Go cannot resolve it at all.
		//   java_answers_go_42703 — the three-segment SELECT: Java returns rows,
		//       so Go's refusal is a measured Go gap, not a semantic rule.
		probes := []struct{ name, sql, expect string }{
			{"flat_key_control", "SELECT sk, COUNT(*) FROM T_NG2 GROUP BY sk", "java_plans_none_go_serves"},
			{"bogus_qualifier_control", "SELECT COUNT(*) FROM T_NG2 GROUP BY zzz.sk", "both_42703"},
			// The reference outside GROUP BY: nested paths answer at all.
			{"nested_select_control", "SELECT n.sk FROM T_NG1", "both_answer"},
			{"nested_orderby_control", "SELECT id FROM T_NG1 ORDER BY n.sk", "java_plans_none_go_serves"},
			// The shape itself, across the three dimensions the Go defect got
			// wrong INDEPENDENTLY: the table (flat leaf twin present or not),
			// the projection (key projected, or only COUNT(*)), the member.
			{"nested_key_projected", "SELECT n.sk, COUNT(*) FROM T_NG1 GROUP BY n.sk", "java_semantic_ok_go_0af00"},
			{"nested_key_unprojected", "SELECT COUNT(*) FROM T_NG1 GROUP BY n.sk", "java_semantic_ok_go_0af00"},
			{"nested_key_no_agg", "SELECT n.sk FROM T_NG1 GROUP BY n.sk", "java_semantic_ok_go_0af00"},
			{"nested_key_two_members", "SELECT n.sk, n.co, COUNT(*) FROM T_NG1 GROUP BY n.sk, n.co", "java_semantic_ok_go_0af00"},
			{"nested_key_flat_twin", "SELECT n.sk, COUNT(*) FROM T_NG2 GROUP BY n.sk", "java_semantic_ok_go_0af00"},
			{"nested_key_no_twin_member", "SELECT COUNT(*) FROM T_NG2 GROUP BY n.co", "java_semantic_ok_go_0af00"},
			// THREE SEGMENTS: the same descent with the source name in front.
			{"nested_key_alias_qualified", "SELECT a.n.sk, COUNT(*) FROM T_NG2 AS a GROUP BY a.n.sk", "java_semantic_ok_go_42703"},
			{"nested_key_table_qualified", "SELECT T_NG2.n.sk, COUNT(*) FROM T_NG2 GROUP BY T_NG2.n.sk", "java_semantic_ok_go_42703"},
			// Three segments OUTSIDE GROUP BY. This separates "GROUP BY refuses
			// this" from "Go cannot resolve this spelling anywhere", and it is
			// the row that proves the latter.
			{"three_segment_select_control", "SELECT a.n.sk FROM T_NG2 AS a", "java_answers_go_42703"},
			// Grouping by the struct COLUMN itself is not a descent, and Go
			// keeps serving it.
			{"whole_struct_key", "SELECT n, COUNT(*) FROM T_NG1 GROUP BY n", "java_plans_none_go_serves"},
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
			return fmt.Sprintf("%s OK rows=%v", engine, r.Rows.Rows)
		}
		javaState := func(r plandiff.RunResult) string {
			var je *plandiff.JavaError
			if r.Err != nil && errors.As(r.Err, &je) {
				return je.SQLState
			}
			return ""
		}
		errMsg := func(r plandiff.RunResult) string {
			if r.Err == nil {
				return ""
			}
			var je *plandiff.JavaError
			if errors.As(r.Err, &je) {
				return je.Message
			}
			var ge *api.Error
			if errors.As(r.Err, &ge) {
				return ge.Message
			}
			return r.Err.Error()
		}
		goState := func(r plandiff.RunResult) string {
			var ge *api.Error
			if r.Err != nil && errors.As(r.Err, &ge) {
				return string(ge.Code)
			}
			return ""
		}
		// Java got the key past semantic analysis: it reached the planner and
		// the planner is what turned it away, with no SQLSTATE — the flat key's
		// own outcome. Anything carrying a SQLSTATE was refused EARLIER, which
		// is the case this whole probe exists to rule out.
		javaSemanticallyAccepted := func(r plandiff.RunResult) bool {
			return r.Err != nil && javaState(r) == "" && strings.Contains(errMsg(r), "could not plan")
		}

		var divergences []string
		seen := map[string]int{}
		for _, p := range probes {
			jr := runner.RunWithSetup(ctx, schema, setup, p.sql)
			gr := goRunner.RunWithSetup(ctx, schema, setup, p.sql)
			fmt.Fprintf(GinkgoWriter, "PROBE %s [%s]\n  %s\n  %s\n  sql: %s\n",
				p.name, p.expect, render("JAVA", jr), render("GO  ", gr), p.sql)
			seen[p.expect]++
			bad := func(format string, args ...any) {
				divergences = append(divergences, fmt.Sprintf("probe %s (%s): %s\n  %s\n  %s\n  sql: %s",
					p.name, p.expect, fmt.Sprintf(format, args...),
					render("JAVA", jr), render("GO  ", gr), p.sql))
			}
			switch p.expect {
			case "java_plans_none_go_serves":
				if !javaSemanticallyAccepted(jr) {
					bad("Java's planner-decline signature no longer reproduces; the " +
						"'decline == semantically accepted' reading this file rests on is stale")
				}
				if gr.Err != nil {
					bad("Go stopped serving a shape it served")
				}
			case "both_42703":
				if javaState(jr) != "42703" {
					bad("Java's SEMANTIC-refusal signature is gone; without it, " +
						"'the nested key is not 42703 in Java' says nothing")
				}
				if goState(gr) != "42703" {
					bad("Go no longer refuses an unresolvable qualifier with 42703")
				}
			case "both_answer":
				if jr.Err != nil {
					bad("Java stopped answering a nested reference outside GROUP BY")
				}
				if gr.Err != nil {
					bad("Go stopped answering a nested reference outside GROUP BY")
				}
			case "java_semantic_ok_go_0af00":
				if !javaSemanticallyAccepted(jr) {
					bad("Java no longer gets the nested grouping key past semantic " +
						"analysis. If it now carries 42703, Go's 0AF00 is the wrong " +
						"code and the sqldriver pins must move with it")
				}
				if goState(gr) != "0AF00" {
					bad("Go's nested-key refusal moved off 0AF00")
				}
			case "java_semantic_ok_go_42703":
				if !javaSemanticallyAccepted(jr) {
					bad("Java no longer gets the three-segment grouping key past " +
						"semantic analysis")
				}
				if goState(gr) != "42703" {
					bad("Go's three-segment refusal moved; if it now resolves, the " +
						"nested-key gate must cover this spelling too, and the " +
						"sqldriver pin calling it a Go limitation is stale")
				}
			case "java_answers_go_42703":
				if jr.Err != nil {
					bad("Java stopped ANSWERING the three-segment spelling; the " +
						"sqldriver pin calls Go's refusal a Go limitation on the " +
						"strength of this row")
				}
				if goState(gr) != "42703" {
					bad("Go's three-segment refusal moved; re-measure the sqldriver pin")
				}
			default:
				bad("unknown expectation class %q", p.expect)
			}
		}
		// No class may go empty: a verdict computed over zero probes reads as a
		// pass, and the two control classes are what the rest is interpreted
		// against.
		for _, class := range []string{
			"java_plans_none_go_serves", "both_42703", "both_answer",
			"java_semantic_ok_go_0af00", "java_semantic_ok_go_42703", "java_answers_go_42703",
		} {
			Expect(seen[class]).To(BeNumerically(">", 0),
				"expectation class %q exercised no probe; its verdict is vacuous", class)
		}
		Expect(divergences).To(BeEmpty())
	})
})
