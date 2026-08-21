//go:build bazelrunfiles

package conformance_test

// MEASURES FOUR CLAIMS ABOUT JAVA THAT ARRIVED AS ASSERTIONS, not as readings.
// Each one decides whether a corpus row is a Java-authoritative pin or a Go
// divergence blessed as expected — the difference between a regression net and
// a green that means nothing — so each is measured against a live JVM here
// rather than reasoned about:
//
//  1. `SELECT "a.b" FROM dott` reported `a.b` on Java and `b` on Go —
//     CONFIRMED, and narrower than reported: `SELECT *` over the same table
//     reported `a.b` on BOTH. The truncation was in the explicit-projection
//     label path alone, with star expansion in the same engine as the working
//     reference. FIXED, and both arms now assert agreement; what they watch is
//     a return to the split.
//  2. A recursive CTE whose seed is four columns wide and whose recursive term
//     is one column wide is rejected `42F10` on Java, where Go says `0AF00`.
//  3. `WITH RECURSIVE s AS (…), d AS (… UNION ALL … d …)` — a recursive CTE
//     with a SIBLING — is accepted by Java, where Go rejects it `0A000`. If
//     true, that is a missing capability rather than a shared rejection.
//  4. A table declaring BOTH `"TOTAL"` and `"X.TOTAL"` makes two different
//     queries render one string at the label boundary. This is the half that
//     still diverges, and it is here rather than in the yamsql corpus because
//     that corpus is Java-authoritative: pinning Go's answer there would credit
//     a known divergence as supported. RFC-238 is the fix.
//
// Per the section contract these assertions state CURRENT MEASURED behaviour on
// both engines. RED means one of the engines moved, which is the signal; it does
// not by itself say which one is wrong.

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

// seedProbeOutcome is one engine's answer reduced to what this probe compares:
// the reported column names and rows when accepted, the SQLSTATE and message
// otherwise. Both halves matter — claim 1 is about NAMES and claims 2 and 3 are
// about SQLSTATEs, so an outcome type that kept only one would be blind to half
// the battery.
type seedProbeOutcome struct {
	accepted bool
	value    string
	detail   string
}

func (o seedProbeOutcome) String() string {
	if o.accepted {
		return "ACCEPT(" + o.value + ")"
	}
	return "REJECT(" + o.detail + ")"
}

var _ = Describe("DottedAndRecursiveSeedJavaProbe", func() {
	It("measures both engines on a delimited dotted column and on two recursive-CTE rejections", func() {
		ctx := context.Background()
		tenantName := fmt.Sprintf("dotseed_%s", uuid.New().String())
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

		// Two tables: one carrying a DELIMITED column whose own name contains a
		// dot, and one ordinary two-column table to join against so a recursive
		// seed can be made four columns wide.
		schema := `CREATE TABLE DOTT ("a.b" BIGINT, plain BIGINT, PRIMARY KEY ("a.b"))
CREATE TABLE Q1 ("id" BIGINT, PRIMARY KEY ("id"))
CREATE TABLE QCASE (id BIGINT, "KeepCase" BIGINT, plain BIGINT, PRIMARY KEY (id))
CREATE TABLE XPROBE (id BIGINT, "TOTAL" BIGINT, "X.TOTAL" BIGINT, PRIMARY KEY (id))`
		setup := []string{
			`INSERT INTO DOTT VALUES (1, 9)`,
			`INSERT INTO Q1 VALUES (1), (2)`,
			`INSERT INTO QCASE VALUES (1, 42, 7)`,
			`INSERT INTO XPROBE VALUES (1, 10, 99)`,
		}

		classify := func(r plandiff.RunResult) seedProbeOutcome {
			if r.Err != nil {
				var je *plandiff.JavaError
				if errors.As(r.Err, &je) {
					return seedProbeOutcome{detail: je.SQLState + " " + je.Message}
				}
				var ge *api.Error
				if errors.As(r.Err, &ge) {
					return seedProbeOutcome{detail: string(ge.Code) + " " + ge.Message}
				}
				return seedProbeOutcome{detail: "?:" + r.Err.Error()}
			}
			names := make([]string, 0, len(r.Rows.Columns))
			for _, c := range r.Rows.Columns {
				names = append(names, c.Name)
			}
			return seedProbeOutcome{
				accepted: true,
				value:    fmt.Sprint(names) + fmt.Sprint(r.Rows.Rows),
			}
		}

		type probe struct {
			name, sql string
			// wantJava and wantGo are the FULL renderings — ACCEPT(...) or a
			// SQLSTATE substring for a REJECT. They are separate fields because
			// the whole point of the battery is that the two engines may differ,
			// and a single expectation could not express that.
			wantJava, wantGo string
			// javaRejects/goRejects say which side is expected to reject, so a
			// side that flips from accepting to rejecting fails loudly instead
			// of quietly matching a substring against an error it never saw.
			javaRejects, goRejects bool
			why                    string
		}
		probes := []probe{
			{
				name:     "delimited_dotted_column_label",
				sql:      `SELECT "a.b" FROM DOTT`,
				wantJava: `[a.b][[1]]`,
				wantGo:   `[a.b][[1]]`,
				why: "claim 1, NOW CLOSED. A column DECLARED with a dot inside its delimiters " +
					"is a single name, not a qualified reference. Go split it at the last " +
					"depth-0 dot and reported the fragment `b`; the schema settles which dots " +
					"are qualifiers now, so both engines report the whole declared name",
			},
			{
				name:     "delimited_dotted_column_star",
				sql:      `SELECT * FROM DOTT`,
				wantJava: `[a.b PLAIN][[1 9]]`,
				wantGo:   `[a.b PLAIN][[1 9]]`,
				why: "THE SAME CLAIM THROUGH STAR EXPANSION, AND IT AGREES. Star reaches its " +
					"labels by a different authority than an explicit projection, and that " +
					"authority keeps the delimited name whole. So the truncation above is not " +
					"a property of dotted columns — it is one path getting it wrong while its " +
					"sibling, in the same engine, gets it right",
			},
			{
				name:     "dotted_column_whose_tail_is_its_sibling",
				sql:      `SELECT "X.TOTAL" FROM XPROBE`,
				wantJava: `[X.TOTAL][[99]]`,
				wantGo:   `[TOTAL][[99]]`,
				why: "THE HALF OF RFC-238's PAIR THAT DIVERGES, and it lives here rather " +
					"than in the yamsql corpus because that corpus is Java-authoritative — " +
					"an arm asserting `TOTAL` there would convert a known divergence into " +
					"a passing conformance case and credit it in the generated ledgers. " +
					"XPROBE declares both `\"TOTAL\"` and `\"X.TOTAL\"`, so this read renders " +
					"IDENTICALLY to the correlated `x.\"TOTAL\"` read (pinned in the corpus, " +
					"correct at `TOTAL`) while wanting the opposite label. Note the VALUE: " +
					"99 proves the right column was read, so only the label is wrong. Not a " +
					"regression — this branch's base split the same way",
			},
			{
				name:        "recursive_seed_arity_mismatch",
				sql:         `WITH RECURSIVE d AS (SELECT * FROM Q1, QCASE WHERE Q1."id" = 1 UNION ALL SELECT d."id" FROM d) SELECT * FROM d`,
				javaRejects: true, goRejects: true,
				wantJava: `42F10`,
				wantGo:   `0AF00`,
				why: "claim 2. Four-column seed against a one-column recursive term is invalid " +
					"UNION arity on any engine; what is measured here is the SQLSTATE each one " +
					"reports for it",
			},
			{
				name:        "recursive_cte_with_sibling",
				sql:         `WITH RECURSIVE s AS (SELECT "id" FROM Q1), d AS (SELECT * FROM s UNION ALL SELECT d."id" + 1 FROM d WHERE d."id" < 3) SELECT * FROM d`,
				javaRejects: false, goRejects: true,
				wantJava: `[id][[1] [2] [2] [3] [3]]`,
				wantGo:   `0A000`,
				why: "claim 3. A recursive CTE alongside a plain one. If Java plans it, Go's " +
					"0A000 is a missing capability and not a shared rejection",
			},
		}

		var problems []string
		for _, p := range probes {
			java := classify(javaRunner.RunWithSetup(ctx, schema, setup, p.sql))
			goSide := classify(goRunner.RunWithSetup(ctx, schema, setup, p.sql))
			mark := "  "
			fail := func(f string, a ...any) {
				mark = "!!"
				problems = append(problems, fmt.Sprintf("%s: %s (%s)\n    %s",
					p.name, fmt.Sprintf(f, a...), p.why, p.sql))
			}
			check := func(side string, got seedProbeOutcome, want string, rejects bool) {
				switch {
				case rejects && got.accepted:
					fail("%s now ACCEPTS a shape it rejected: %s", side, got)
				case rejects && !strings.Contains(got.detail, want):
					fail("%s rejection no longer carries %s: %s", side, want, got)
				case !rejects && !got.accepted:
					fail("%s now REJECTS a shape it answered: %s", side, got)
				case !rejects && got.value != want:
					fail("%s answer changed: got %s want ACCEPT(%s)", side, got, want)
				}
			}
			check("Java", java, p.wantJava, p.javaRejects)
			check("Go", goSide, p.wantGo, p.goRejects)
			fmt.Fprintf(GinkgoWriter, "%s %-34s java=%-46s go=%-46s %s\n",
				mark, p.name, java, goSide, p.sql)
		}

		Expect(problems).To(BeEmpty(),
			"one of the three measured claims about Java has moved. Each decides whether a\n"+
				"corpus row is a Java-authoritative pin or a Go divergence blessed as expected,\n"+
				"so re-read the corresponding row before touching an assertion here.\n"+
				strings.Join(problems, "\n"))
	})
})

// A DECODED-NAME COLLISION HAS A DIFFERENT BLAST RADIUS ON EACH ENGINE, and
// that difference is the finding. It took two wrong readings to get to it.
//
// `"___"` and `"___0"` are distinct, legal, non-duplicate SQL columns. Both
// begin `__`, so both pass through ToProtoBufCompliantName UNCHANGED — and
// `___0` then decodes to `___`, because the decode scan finds `__0` at index 1.
// One decoded spelling for two columns; no buildable row type.
//
// FIRST WRONG READING: "DDL cannot produce this." It can — the shape above is
// ordinary DDL, and the preserved-`__`-prefix arm is why neither name is
// escaped at all.
//
// SECOND WRONG READING: "both engines fail, so this is upstream-faithful." That
// came from putting COLL beside the probes above, where five unrelated arms went
// red on BOTH engines. The shared setup INSERTed into COLL, so Java was failing
// on the INSERT and every later query inherited it. Isolated, with no setup, the
// engines separate:
//
//	SELECT id FROM INNOCENT     Java ANSWERS      Go XX000 (recovered panic)
//	SELECT id FROM COLL         Java fails        Go XX000
//
// So Java's failure is TABLE-LOCAL and Go's is SCHEMA-WIDE. A collision in one
// table takes down queries against every other table in the schema, which Java
// does not do. That is a real divergence, not a faithfully reproduced upstream
// defect, and it is what RFC-238 §7b has to fix — reproducing Java means
// failing on COLL and answering on INNOCENT.
//
// The INNOCENT arm is the one that carries this. A COLL-only probe proves that
// building the offending table's row type visits its own columns, which is true
// and much weaker; it would also stay green if the blast radius later shrank to
// Java's, and go on asserting a scope that no longer exists.
var _ = Describe("DecodedNameCollisionJavaProbe", func() {
	It("measures each engine's blast radius for a decoded-name collision", func() {
		ctx := context.Background()
		tenantName := fmt.Sprintf("collide_%s", uuid.New().String())
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

		// No setup rows: an INSERT into COLL is what made the first version of
		// this probe read both engines as failing everywhere.
		schema := `CREATE TABLE COLL (id BIGINT, "___" BIGINT, "___0" BIGINT, PRIMARY KEY (id))
CREATE TABLE INNOCENT (id BIGINT, v BIGINT, PRIMARY KEY (id))`

		// THE DIVERGENCE. A query that never names COLL.
		javaInnocent := javaRunner.RunWithSetup(ctx, schema, nil, `SELECT id FROM INNOCENT`).Err
		goInnocent := goRunner.RunWithSetup(ctx, schema, nil, `SELECT id FROM INNOCENT`).Err

		Expect(javaInnocent).NotTo(HaveOccurred(),
			"Java stopped answering a query against an UNRELATED table. If Java's blast\n"+
				"radius grew to the whole schema, Go's behaviour is upstream-faithful after\n"+
				"all and RFC-238 §7b's framing must change.")

		Expect(goInnocent).To(HaveOccurred(),
			"Go now ANSWERS the unrelated table — the divergence is CLOSED, which is what\n"+
				"RFC-238 §7b's criterion asks for. FLIP this to NotTo(HaveOccurred()) and\n"+
				"KEEP it. Do not delete it: it is the only Go-side pin that an unrelated\n"+
				"table stays queryable, so deleting it lets a later return to schema-wide\n"+
				"failure through unnoticed.")
		var ge *api.Error
		Expect(errors.As(goInnocent, &ge)).To(BeTrue(),
			"Go's failure stopped being an api.Error. A panic escaping the driver boundary\n"+
				"looks exactly like this, and removing that panic is §7b's other half.")
		Expect(string(ge.Code)).To(Equal("XX000"),
			"Go's SQLSTATE moved. If it became a real diagnostic, say so here.")

		// THE SHARED HALF, on BOTH engines. Java's alone would leave the arm
		// above satisfiable by the wrong fix: an implementation that simply
		// ignored the duplicate decoded fields makes every table queryable,
		// including COLL, and a probe that never asks Go about COLL would call
		// that a success. Failing on COLL is half the requirement.
		javaColl := javaRunner.RunWithSetup(ctx, schema, nil, `SELECT id FROM COLL`).Err
		goColl := goRunner.RunWithSetup(ctx, schema, nil, `SELECT id FROM COLL`).Err

		Expect(javaColl).To(HaveOccurred(),
			"Java ANSWERED a read of the colliding table itself. Then the collision is not\n"+
				"a defect on either engine and this whole section needs re-reading.")
		Expect(javaColl.Error()).To(ContainSubstring("Multiple entries with same key: ___="),
			"Java still fails on COLL but for a DIFFERENT reason; re-read before assuming\n"+
				"the causes still match.")

		Expect(goColl).To(HaveOccurred(),
			"Go ANSWERED the COLLIDING table. Two columns share one decoded spelling, so\n"+
				"a row type naming both cannot be built and any answer here is reading one\n"+
				"of them under the other's name. This assertion holds BEFORE and AFTER the\n"+
				"§7b fix — table-local means failing HERE and answering on INNOCENT, not\n"+
				"answering everywhere.")
	})
})
