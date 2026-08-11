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
// WHICH ROWS CARRY THAT CONCLUSION. Two of them, and neither depends on whether
// the planner could find an access path — which matters because index
// availability confounds every DECLINE row in this file (see below):
//
//   - bogus_qualifier_control (`GROUP BY zzz.sk`) gets 42703 out of Java. Java
//     spends UNDEFINED_COLUMN on a reference that does not resolve;
//   - nested_select_control (`SELECT n.sk FROM T_NG1`) is answered by both. The
//     reference is well-formed, so 42703 would name a column that exists.
//
// indexed_nested_key makes the conclusion STRONGER than a layer argument.
// MEASURED live, not inferred: with an index over the path,
// `SELECT COUNT(*) FROM T_NG3 GROUP BY n.sk` returns [[2] [1]] out of Java
// while Go answers 0AF00, and the flat twin `GROUP BY k` over its own index
// returns the same [[2] [1]] from both. Java HAS the capability; Go lacks it.
// That is what UNSUPPORTED_QUERY means and precisely what UNDEFINED_COLUMN
// would misreport.
//
// INDEX AVAILABILITY IS THE OPERATIVE VARIABLE, and it is now a variable rather
// than a constant held at NONE. Java's Cascades has no physical sort operator,
// so a GROUP BY plans only when an index supplies the grouping key's ordering:
// in the vendored corpus EVERY answering GROUP BY is fed by an index scan
// (`ISCAN(I1 <,>) | … | AGG (…)`, groupby-tests.yamsql:136,144,154,158,162,166,
// 185,189) and no plan there puts a full table scan under an AGG. Without a
// matching index the planner declines with "Cascades planner could not plan
// query" and no SQLSTATE.
//
// So a PLANNER DECLINE means "semantic analysis accepted this key and the
// planner had no access path", and it says nothing about nesting on its own.
// Reading the nested-key rows requires holding nesting and index availability
// apart, which is what these four controls do:
//
//   - flat_key_control groups by an ordinary UNINDEXED column: the signature of
//     "semantic analysis ACCEPTED this key, no access path" (decline, no
//     SQLSTATE). The NEGATIVE arm;
//   - indexed_flat_key_control groups by an INDEXED column: the POSITIVE arm,
//     the same query shape with the operative variable flipped. Without it the
//     decline rows are uninterpretable — "nested is unsupported" and "unordered
//     input is unplannable" produce the identical outcome;
//   - indexed_nested_key groups by a NESTED path with an index over that path:
//     nesting AND index both present;
//   - bogus_qualifier_control groups by a qualifier resolving to nothing: the
//     signature of "semantic analysis REFUSED this key" (42703).
//
// WHAT THIS FILE USED TO CLAIM AND WHY IT WAS FALSE. It said "Java does not
// ANSWER a nested grouping key at this tag either — nothing does, because the
// planner declines the flat key too." That was read off a probe set in which
// every one of the 14 queries ran against bare primary-key tables, so index
// availability never varied and the sentence generalised from a single cell of
// the table. The vendored corpus refutes it directly: with `create index i2 as
// select r.v.z from nested order by r.v.z` (groupby-tests.yamsql:29), the query
// `select max(q.s) from nested group by r.v.z having r.v.z > 120` answers
// `[{330}]` (:61-62) — a nested grouping key, answered, at this exact tag. The
// true statement is narrower and is now measured here rather than reasoned
// about: Java does not answer a grouping key whose ordering no index supplies,
// which is equally true of a FLAT key. Live outcomes, all four cells:
//
//	                    UNINDEXED                       INDEXED
//	FLAT     GROUP BY sk    → decline      GROUP BY k    → [[2] [1]]
//	NESTED   GROUP BY n.sk  → decline      GROUP BY n.sk → [[2] [1]]
//
// The rows vary with the INDEX and not with the NESTING, which is the whole
// finding. Every DECLINE row in this file sits in the left column and can
// therefore carry no conclusion about nesting on its own.
//
// THE THREE-SEGMENT ROWS MEASURED A GO LIMITATION THAT IS NOW CLOSED. Java
// ANSWERS `SELECT a.n.sk FROM t AS a` (three_segment_select_control) — it
// returned [[1] [1] [2]] against the live server — and Go refused that spelling
// with 42703 everywhere it appeared, because the resolver carried a reference
// as a (qualifier, name) PAIR and joined the leading segments into "A.N", which
// names neither a source nor a struct column. Go now carries the parse tree's
// segments and answers the same rows, so those rows are ordinary both_answer
// agreement and the two classes that described the gap are retired.
//
// What did NOT move is the GROUP BY spelling. A nested grouping key is refused
// in Go at BOTH arities with 0AF00 — the gate this file documents — so the
// three-segment grouping rows now sit with their two-segment twins in
// java_semantic_ok_go_0af00. Keeping them is the point: the two spellings of
// ONE key must agree about which layer refuses them, and they did not. The
// descent test behind the gate could see only two segments, so for
// `GROUP BY a.n.sk` it was really asking whether a source called "A.N" exists,
// answering no, and letting the key fall through to undefined-column.

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
		//
		// T_NG3 exists ONLY to make index availability a variable. T_NG1 and
		// T_NG2 carry no index at all, so every grouping key over them is
		// unordered and Java's planner declines it whether or not the key is
		// nested — nesting and index availability move together there and no
		// decline row can distinguish them. T_NG3 carries one index over a FLAT
		// column and one over a NESTED path, mirroring the vendored corpus's i1
		// and i2 (groupby-tests.yamsql:23,29), so the same query shapes run with
		// the operative variable flipped.
		schema := "CREATE TYPE AS STRUCT GST (sk BIGINT, co BIGINT)" +
			" CREATE TABLE T_NG1 (id BIGINT, n GST, PRIMARY KEY (id))" +
			" CREATE TABLE T_NG2 (id BIGINT, sk BIGINT, n GST, PRIMARY KEY (id))" +
			" CREATE TABLE T_NG3 (id BIGINT, k BIGINT, n GST, PRIMARY KEY (id))" +
			" CREATE INDEX ng3_k AS SELECT k FROM T_NG3 ORDER BY k" +
			" CREATE INDEX ng3_nsk AS SELECT n.sk FROM T_NG3 ORDER BY n.sk"
		setup := []string{
			"INSERT INTO T_NG1 VALUES (1, (1, 1)), (2, (1, 2)), (3, (2, 1)), (4, (2, 2)), (5, (1, 1))",
			"INSERT INTO T_NG2 VALUES (1, 90, (1, 1)), (2, 91, (1, 2)), (3, 92, (2, 1))",
			// Two groups on BOTH k and n.sk, with different members per group,
			// so "answered" can be asserted as a group COUNT rather than merely
			// as the absence of an error — a query that answered zero rows, or
			// that dropped the grouping and answered three, fails the same check.
			"INSERT INTO T_NG3 VALUES (1, 10, (1, 1)), (2, 10, (1, 2)), (3, 20, (2, 1))",
		}

		// The expectation classes, and what each one exists to catch:
		//
		//   java_plans_none_go_serves — Java's planner decline with Go serving.
		//       This is the NO-ACCESS-PATH signature, not a nesting signature:
		//       the flat unindexed key lands here too, which is exactly why it
		//       decides nothing on its own.
		//   java_answers_grouping / java_answers_grouping_go_0af00 — the same
		//       shapes with an index over the grouping key. These are the
		//       POSITIVE arms that make the decline class readable; without
		//       them "nested is unsupported" and "unordered input is
		//       unplannable" are indistinguishable.
		//   both_42703 — the semantic-refusal signature. Its presence is what
		//       makes "the nested key is not 42703 in Java" non-vacuous.
		//   both_answer — the reference resolves outside GROUP BY in both.
		//   java_semantic_ok_go_0af00 — the shape under test.
		//       java_semantic_ok_go_0af00 also carries the THREE-SEGMENT
		//       grouping spelling, which is the same key with its source named
		//       and must therefore reach the same refusal. It reached 42703
		//       until the resolver stopped joining the leading segments.
		probes := []struct{ name, sql, expect string }{
			{"flat_key_control", "SELECT sk, COUNT(*) FROM T_NG2 GROUP BY sk", "java_plans_none_go_serves"},
			// THE POSITIVE ARM the decline rows are read against: the SAME
			// query shape as flat_key_control with one variable changed, the
			// grouping column indexed. Without this row every decline in this
			// file is uninterpretable, because "Java will not group by a nested
			// path" and "Java will not group by an unordered input" produce the
			// same outcome and the probe held the second one fixed.
			{"indexed_flat_key_control", "SELECT COUNT(*) FROM T_NG3 GROUP BY k", "java_answers_grouping"},
			// NESTING AND INDEX BOTH PRESENT. The vendored corpus already shows
			// Java answering a nested grouping key at this tag
			// (groupby-tests.yamsql:61-62, `group by r.v.z` over index i2), so
			// this row measures live what that file asserts statically — and it
			// is what makes Go's 0AF00 a capability gap rather than a layer
			// disagreement.
			{"indexed_nested_key", "SELECT COUNT(*) FROM T_NG3 GROUP BY n.sk", "java_answers_grouping_go_0af00"},
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
			{"nested_key_alias_qualified", "SELECT a.n.sk, COUNT(*) FROM T_NG2 AS a GROUP BY a.n.sk", "java_semantic_ok_go_0af00"},
			{"nested_key_table_qualified", "SELECT T_NG2.n.sk, COUNT(*) FROM T_NG2 GROUP BY T_NG2.n.sk", "java_semantic_ok_go_0af00"},
			// Three segments OUTSIDE GROUP BY. These separate "GROUP BY refuses
			// this" from "this spelling resolves at all", which is what makes the
			// grouping refusal above a capability statement rather than a
			// resolution failure wearing a different code.
			//
			// One row per CLAUSE, because the refusal was never one gate: each
			// clause reached the flattened qualifier through its own carrier and
			// failed with its own symptom — 42703 in the SELECT list, a bare
			// planner decline in WHERE, an executor "malformed plan" in ORDER BY.
			// A single SELECT row would have left the others unmeasured, and the
			// two that were NOT 42703 are exactly the ones a 42703-shaped
			// expectation would have missed.
			{"three_segment_select_control", "SELECT a.n.sk FROM T_NG2 AS a", "both_answer"},
			{"three_segment_select_table_qualified", "SELECT T_NG2.n.sk FROM T_NG2", "both_answer"},
			{"three_segment_other_member", "SELECT a.n.co FROM T_NG2 AS a", "both_answer"},
			{"three_segment_where", "SELECT a.id FROM T_NG2 AS a WHERE a.n.sk = 1", "both_answer"},
			{"three_segment_order_by", "SELECT a.id FROM T_NG2 AS a ORDER BY a.n.co", "java_plans_none_go_serves"},
			{"three_segment_aggregate_arg", "SELECT COUNT(a.n.sk) FROM T_NG2 AS a", "both_answer"},
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
			case "java_answers_grouping", "java_answers_grouping_go_0af00":
				// JAVA ANSWERS, asserted by the GROUP COUNT and not by the
				// absence of an error. A decline is an error and would be caught
				// either way, but a query that answered zero rows — or that
				// dropped the grouping and answered one row per record — is a
				// silent pass against a bare error check, and both are exactly
				// what this arm exists to distinguish.
				if jr.Err != nil {
					bad("Java DECLINED a grouping key whose ordering an index supplies. " +
						"If this is the INDEXED FLAT arm, the premise that index " +
						"availability is the operative variable is wrong and every " +
						"decline row in this file needs re-reading. If it is the NESTED " +
						"arm, Java's nested-grouping-key support is narrower than " +
						"groupby-tests.yamsql:61-62 shows and the capability-gap claim " +
						"must be re-derived from the corpus rather than from here")
				} else if n := len(jr.Rows.Rows); n != 2 {
					bad("Java answered %d rows, want 2 — T_NG3 has two groups on both k "+
						"and n.sk. One row means the grouping was dropped; zero means the "+
						"query answered nothing and 'answers' is vacuous", n)
				}
				if p.expect == "java_answers_grouping" {
					if gr.Err != nil {
						bad("Go stopped serving an INDEXED flat grouping key")
					}
				} else if goState(gr) != "0AF00" {
					bad("Go's nested-key refusal moved off 0AF00 on the indexed shape. " +
						"If Go now ANSWERS it, nested grouping keys have landed and this " +
						"row becomes a both_answer")
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
			default:
				bad("unknown expectation class %q", p.expect)
			}
		}
		// No class may go empty: a verdict computed over zero probes reads as a
		// pass, and the two control classes are what the rest is interpreted
		// against.
		for _, class := range []string{
			"java_plans_none_go_serves", "both_42703", "both_answer",
			"java_semantic_ok_go_0af00",
			// The two POSITIVE arms are floored beside the rest for the same
			// reason: a decline class read without a matching answering class is
			// a one-cell table, which is the confound this file was rebuilt to
			// remove.
			"java_answers_grouping", "java_answers_grouping_go_0af00",
		} {
			Expect(seen[class]).To(BeNumerically(">", 0),
				"expectation class %q exercised no probe; its verdict is vacuous", class)
		}
		Expect(divergences).To(BeEmpty())
	})
})
