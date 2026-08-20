//go:build bazelrunfiles

package conformance_test

// Does a CHAINED `USING` resolve, or is its column ambiguous?
//
// `a JOIN b USING (id) JOIN c USING (k)` asks a question the first join has
// already complicated. Java's resolveJoinUsingClause marks the RIGHT copy of a
// USING column hidden, so after `USING (id)` only `b.id` is hidden — `a.k` and
// `b.k` both remain visible on the left. The second `USING (k)` then resolves
// `k` against every left operator and can find two of them.
//
// Go instead qualifies the second USING against the PRIOR JOIN'S RIGHT alias,
// picks `b.k`, and answers. If Java raises AMBIGUOUS_COLUMN there, then a Go
// fixture asserting rows for that query pins a divergence as though it were the
// specification — the failure mode is not a wrong answer but a test that
// enshrines one.
//
// This is measured rather than reasoned about because the reasoning above is
// exactly the kind that has been wrong twice on this branch: the alias chain
// was assumed correct until it was mutated, and the mutation showed it was.
//
// THE FIXTURE MAKES THE TWO READINGS DISAGREE ON ROWS, so an engine that
// resolves rather than rejects still reveals WHICH column it chose: a.k and b.k
// differ at id=2, so `k` meaning a.k yields two rows and `k` meaning b.k yields
// one. A test that could not tell them apart would report agreement whichever
// column either engine picked.

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

var _ = Describe("JoinUsingChainJavaProbe", func() {
	It("measures both engines on a chained USING whose column exists on both left sources", func() {
		ctx := context.Background()
		tenantName := fmt.Sprintf("usingchain_%s", uuid.New().String())
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

		const schema = "CREATE TABLE a (id BIGINT, k BIGINT, j BIGINT, PRIMARY KEY (id)) " +
			"CREATE TABLE b (id BIGINT, k BIGINT, PRIMARY KEY (id)) " +
			"CREATE TABLE c (id BIGINT, k BIGINT, j BIGINT, PRIMARY KEY (id))"
		setup := []string{
			"INSERT INTO a (id,k,j) VALUES (1,10,5),(2,20,6),(3,30,7)",
			"INSERT INTO b (id,k) VALUES (1,10),(2,99),(3,30)",
			"INSERT INTO c (id,k,j) VALUES (1,10,5),(2,20,9),(3,77,9)",
		}

		render := func(r plandiff.RunResult) string {
			if r.Err != nil {
				return "ERR(" + r.Err.Error() + ")"
			}
			return fmt.Sprint(r.Rows.Rows)
		}
		rejects := func(s string) bool { return strings.HasPrefix(s, "ERR(") }

		cases := []struct {
			name string
			sql  string
			// control marks arms both engines must accept and agree on.
			control bool
			// chained marks the arms this file exists to measure.
			chained bool
			// javaSays and goSays, when set, require the arm to be REFUSED and the
			// refusal to NAME the fault. Agreement on "rejected" is too weak
			// wherever the point of the arm is WHICH fault is reported first.
			javaSays, goSays string
		}{
			{
				name: "single USING", control: true,
				sql: "SELECT COUNT(*) FROM a JOIN b USING (id)",
			},
			{
				name: "multi-column USING", control: true,
				sql: "SELECT COUNT(*) FROM a JOIN b USING (id, k)",
			},
			{
				// The chain whose second USING column is present on BOTH left
				// sources — the shape where Java's hidden-column rule and Go's
				// prior-right-alias rule can disagree.
				chained: true, name: "chained USING, second column on both left sources",
				sql: "SELECT a.id FROM a JOIN b USING (id) JOIN c USING (id, k) ORDER BY a.id",
			},
			{
				// The same chain where the FIRST join already equates k, so both
				// readings coincide. If the engines agree here and differ above,
				// the difference is about ambiguity and not about USING chains.
				// It also explains the rule: the first `USING (id, k)` HIDES the
				// right copy b.k, leaving only a.k visible, so the second USING
				// has exactly one candidate. Hiding is what makes one chain legal
				// and the other ambiguous.
				chained: true, name: "chained USING, first join already equates the column",
				sql: "SELECT a.id FROM a JOIN b USING (id, k) JOIN c USING (id, k) ORDER BY a.id",
			},
			{
				// THE FAR-LEFT CASE, measured now so the repair is designed once
				// rather than discovered twice. `j` exists ONLY on `a`, so the
				// second USING's unique candidate is the FIRST source, not the
				// prior join's right side. A rule that qualifies by the prior
				// right alias looks for b.j and cannot find it; Java resolves
				// against all visible left operators and finds a.j.
				chained: true, name: "chained USING whose column is only on the FIRST source",
				sql: "SELECT a.id FROM a JOIN b USING (id) JOIN c USING (j) ORDER BY a.id",
			},
			{
				// A DERIVED source to the LEFT of a second USING. Go cannot read
				// a subquery's output columns from the record metadata, so it
				// declines and leaves the positional predicate — which ANSWERS.
				// If Java instead finds the derived table's `k` alongside `a.k`
				// and calls the column ambiguous, declining is a divergence and
				// the decline has to become a scope lookup.
				//
				// Measured rather than argued, because both readings are
				// plausible and the answer decides whether a yamsql arm asserting
				// rows is a pin or a mistake.
				chained: true, name: "chained USING with a DERIVED left source",
				sql: "SELECT a.id FROM a JOIN (SELECT id, k FROM c) d USING (id) " +
					"JOIN c USING (k) ORDER BY a.id",
			},
			{
				// The same shape with a CTE rather than a subquery, since they
				// reach the resolver by different routes.
				chained: true, name: "chained USING with a CTE left source",
				sql: "WITH cte AS (SELECT id, k FROM c) SELECT a.id FROM a JOIN cte USING (id) " +
					"JOIN c USING (k) ORDER BY a.id",
			},
			{
				// ERROR ORDER WITH A DERIVED RIGHT LEG. The FIRST join names a
				// column the subquery does not export (`k`); the SECOND is
				// ambiguous. Java resolves left to right, so the missing column
				// should win. If the derived leg's schema is admitted to later
				// ownership WITHOUT being checked at its own join, the later
				// ambiguity is reported first and the earlier fault is masked.
				chained: true, name: "derived right leg missing the USING column",
				sql: "SELECT a.id FROM a JOIN (SELECT id FROM c) d USING (k) " +
					"JOIN b USING (id)",
				javaSays: "Unknown reference K",
				goSays:   "42703",
			},
			{
				// A DERIVED BODY THAT IS ITSELF INVALID — A KNOWN DIVERGENCE,
				// pinned as it stands rather than asserted away.
				//
				// `nope` is not a source inside the subquery, so the body is
				// wrong before any USING question arises, and Java reports
				// `Unknown reference NOPE`. Go reports the OUTER ambiguity
				// instead, because the schema derivation advertises a simple
				// star body's columns without validating it: a lone qualified
				// star is read as the source's full schema whatever the
				// qualifier says.
				//
				// Both engines refuse the query — it is invalid twice over — so
				// this is an error-ORDER divergence, not a wrong answer. The fix
				// is to route a derived source's schema through the validating
				// builder (buildExactScopeSourceOrBodyError, which today only
				// serves join/derived-legged bodies), which is a change to
				// shared CTE derivation rather than to this resolver. Booked in
				// TODO.md under "A derived source's schema is advertised before
				// its body is validated".
				//
				// Pinned by naming BOTH sides so it fails when it changes in
				// either direction — repaired, or moved to a third answer.
				chained: true, name: "derived body invalid, outer USING ambiguous",
				sql: "SELECT a.id FROM a JOIN (SELECT nope.* FROM c) d USING (id) " +
					"JOIN c USING (k)",
				javaSays: "Unknown reference NOPE",
				goSays:   "42702",
			},
			{
				// A RIGHT LEG THAT EXPORTS THE SAME NAME TWICE. Within one
				// USING, `id` is ambiguous ON THE RIGHT while `k` is missing
				// from it — two faults, and which one is reported says whether
				// resolution is per-attribute and left-to-right.
				//
				// A column lookup that returns its FIRST match cannot see the
				// duplicate at all, so it passes `id` and reports `k`. Measured
				// to find out whether that is what Java does.
				chained: true, name: "right leg exports the USING name twice",
				sql:      "SELECT a.id FROM a JOIN (SELECT id, id FROM c) d USING (id, k)",
				javaSays: "Ambiguous reference ID",
				goSays:   "42702",
			},
			{
				// THE LEFT-HAND MIRROR, which needed its own fix. Ownership
				// answered a BOOLEAN, so a left source exporting the name twice
				// counted as one owner: the walk advanced past the ambiguity and
				// reported the NEXT column's absence instead. Java stops on the
				// first attribute.
				//
				// It gets its own arm because the right-hand fix did not carry
				// over — two structures, two lookups, and only one of them was
				// looked at the first time. That has now happened twice on this
				// branch with the hidden map, so the mirror is assumed to be
				// broken until measured, not assumed to be fixed.
				// The duplicate source is the PRIMARY leg, reached through an ON
				// join, and `j` exists on no other source in scope — so neither
				// the right-hand duplicate check nor a second owner can be what
				// reports it.
				//
				// AND IT IS STILL NOT THIS RESOLVER THAT REPORTS IT. Collapsing
				// the ownership count back to a boolean leaves this arm GREEN:
				// Go answers `Ambiguous reference J` either way, from a later
				// layer that catches the duplicate projection on its own. So
				// the left-side count is symmetry with the right-hand check and
				// defence against a future caller, NOT a repair for a defect
				// reachable today — two earlier drafts of this comment claimed
				// otherwise, and the mutation is what corrected them.
				//
				// The arm earns its place as the measurement behind that
				// sentence, and as coverage of a shape nothing else exercises.
				chained: true, name: "left source exports the USING name twice",
				sql: "SELECT d.id FROM (SELECT j, j, id FROM c) d " +
					"JOIN b ON d.id = b.id JOIN a USING (j)",
			},
		}

		var disagreed []string
		var chainedArms, controls int
		for _, c := range cases {
			javaOut := render(javaRunner.RunWithSetup(ctx, schema, setup, c.sql))
			goOut := render(goRunner.RunWithSetup(ctx, schema, setup, c.sql))
			// The marker uses the SAME rule the assertions do — same decision,
			// and same rows when both answer. Comparing raw text here instead
			// would flag every both-reject arm as a disagreement purely because
			// the two engines word their errors differently, which reads as a
			// failure in a passing run.
			mark := "  "
			if rejects(javaOut) != rejects(goOut) || (!rejects(javaOut) && javaOut != goOut) {
				mark = "!!"
			}
			fmt.Fprintf(GinkgoWriter, "%s %-52s java=%-40s go=%s\n", mark, c.name, javaOut, goOut)

			if c.control {
				controls++
				Expect(rejects(goOut)).To(BeFalse(),
					"%s is a CONTROL and Go rejected it\n  go: %s\n  sql: %s", c.name, goOut, c.sql)
				Expect(rejects(javaOut)).To(BeFalse(),
					"%s is a CONTROL and Java rejected it\n  java: %s\n  sql: %s", c.name, javaOut, c.sql)
				Expect(goOut).To(Equal(javaOut),
					"%s is a CONTROL and the engines disagree, so nothing below is readable\n"+
						"  java: %s\n  go: %s", c.name, javaOut, goOut)
			}
			if c.chained {
				chainedArms++
				// Both engines must make the SAME decision, and where they both
				// answer the ROWS must match — the fixture is built so the two
				// candidate columns give different row counts, so agreeing on
				// "no error" would not be agreeing on which column was chosen.
				//
				// Wording is compared only where an arm SETS it. Where both
				// merely refuse the same query, Java says "Ambiguous reference
				// K" and Go raises 42702 in its own words, and aligning the text
				// would say nothing the shared decision does not.
				//
				// But for an arm about which fault is reported FIRST, the shared
				// decision is exactly what is NOT enough — both engines refusing
				// is compatible with them refusing for different reasons, which
				// is the divergence.
				switch {
				case rejects(javaOut) != rejects(goOut):
					disagreed = append(disagreed, fmt.Sprintf(
						"%s: one engine answered and the other refused\n"+
							"    java: %s\n    go  : %s\n    sql : %s",
						c.name, javaOut, goOut, c.sql))
				case !rejects(javaOut) && javaOut != goOut:
					disagreed = append(disagreed, fmt.Sprintf(
						"%s: both answered but chose DIFFERENT columns\n"+
							"    java: %s\n    go  : %s\n    sql : %s",
						c.name, javaOut, goOut, c.sql))
				}
				for _, e := range []struct{ engine, out, want string }{
					{"java", javaOut, c.javaSays}, {"go", goOut, c.goSays},
				} {
					if e.want == "" {
						continue
					}
					if !strings.Contains(e.out, e.want) {
						disagreed = append(disagreed, fmt.Sprintf(
							"%s: %s did not report %q — this arm is about WHICH fault is "+
								"reported first, so agreement on refusing is not enough\n"+
								"    %s: %s\n    sql : %s",
							c.name, e.engine, e.want, e.engine, e.out, c.sql))
					}
				}
			}
		}

		// THE ENGINES NOW AGREE ON ALL THREE CHAINS. They did not when this file
		// was written: Go qualified a chained USING by the PRIOR JOIN'S RIGHT
		// alias, while Java resolves the column against every visible left
		// operator with the earlier USING's right copy hidden. One positional
		// rule standing in for ownership was wrong in both directions at once —
		//
		//	USING (id) … USING (id, k)   java AMBIGUOUS_COLUMN   go picked b.k
		//	USING (id) … USING (j)       java answered           go 42703
		//
		// — the second being a legitimate query Go refused. `retargetUsingJoins`
		// replaced the positional rule with ownership, and both closed.
		//
		// The middle arm is what makes the pair one bug rather than two: it
		// agreed all along, because there the first USING hides the column and
		// the two rules coincide. Any repair had to keep it agreeing while
		// moving the other two, which a blanket change of alias would not.
		Expect(disagreed).To(BeEmpty(),
			"%d of %d chained arms disagree. Go resolves a USING column to its unique "+
				"visible left OWNER (42702 on two candidates, 42703 on none); a regression to "+
				"qualifying by the prior join's right alias reopens both directions at once — "+
				"silently answering an ambiguous chain, and refusing a column that lives on an "+
				"earlier source.\n\n%s",
			len(disagreed), chainedArms, strings.Join(disagreed, "\n"))
		Expect(chainedArms).To(Equal(9),
			"%d chained arms ran, not 9 — a green from a shrunken set says nothing about "+
				"the shape this file is named for", chainedArms)
		Expect(controls).To(Equal(2),
			"%d controls ran, not 2 — without an agreed USING baseline, a chained "+
				"disagreement cannot be attributed to the chaining", controls)
	})
})

// The `__ROW_VERSION` pseudo-column in a USING clause.
//
// It is not a descriptor field — the catalog appends it as an ephemeral column
// when the store keeps row versions — so it is the one USING column whose
// visibility depends on which surface the resolver reads. Once ownership moved
// from the record descriptor to the catalog, the pseudo-column became visible
// on BOTH sides of a join, and a review raised the concern that a chain would
// then find two owners and raise 42702 on a query the Java corpus requires to
// work.
//
// Reasoning says the hiding rule prevents that: the first USING hides the right
// copy, leaving one candidate. But the hiding rule is exactly what was got
// wrong twice on this branch, so it is measured instead — including the shape
// where nothing hides anything, an ordinary ON join placing two row-versioned
// sources in scope before a USING names the pseudo-column.
var _ = Describe("JoinUsingRowVersionJavaProbe", func() {
	It("measures both engines on __ROW_VERSION as a USING column", func() {
		ctx := context.Background()
		tenantName := fmt.Sprintf("usingrv_%s", uuid.New().String())
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

		// Mirrors the corpus fixture (join-tests-row-version.yamsql): row
		// versions are a SCHEMA option, so every table here carries the
		// pseudo-column.
		const schema = "create table jua(c1 bigint, c2 bigint, c5 bigint, primary key(c1)) " +
			"create table jub(c1 bigint, c3 bigint, c5 bigint, primary key(c1)) " +
			"create table juc(c1 bigint, c4 bigint, c5 bigint, primary key(c1)) " +
			"with options (store_row_versions=true)"
		setup := []string{
			"INSERT INTO jua VALUES(1, 2, 5)",
			"INSERT INTO jub VALUES(1, 3, 5)",
			"INSERT INTO juc VALUES(1, 4, 5)",
		}

		render := func(r plandiff.RunResult) string {
			if r.Err != nil {
				return "ERR(" + r.Err.Error() + ")"
			}
			return fmt.Sprint(r.Rows.Rows)
		}
		rejects := func(s string) bool { return strings.HasPrefix(s, "ERR(") }

		cases := []struct {
			name, sql string
			// javaSays and goSays, when set, require the arm to be REFUSED and
			// the refusal to name the fault. Agreement on "rejected" is too
			// weak for an arm whose whole point is WHICH fault it is.
			javaSays, goSays string
		}{
			{
				// The corpus shape: one join, one left source, one owner.
				name: "single join on the pseudo-column",
				sql:  `select jua.c1 from jua join jub using("__ROW_VERSION")`,
			},
			{
				// A CHAIN. The first USING hides jub's copy, so the second
				// should still see exactly one candidate.
				name: "chained USING on the pseudo-column",
				sql: `select jua.c1 from jua join jub using("__ROW_VERSION") ` +
					`join juc using("__ROW_VERSION")`,
			},
			{
				// NOTHING HIDES ANYTHING HERE. An ordinary ON join puts two
				// row-versioned sources in scope, and only then does a USING
				// name the pseudo-column — so both copies are visible. This is
				// the shape the hiding rule does NOT rescue, and the one worth
				// measuring rather than predicting.
				name: "ON join first, then USING on the pseudo-column",
				sql: `select jua.c1 from jua join jub on jua.c1 = jub.c1 ` +
					`join juc using("__ROW_VERSION")`,
				// AMBIGUITY is asserted, not merely rejection. The file's
				// claim is that two owners really is two owners here, and
				// "both engines refused" is weaker than that — both drifting
				// to 42703 would keep an agreement check green while making
				// the claim false.
				javaSays: "Ambiguous reference __ROW_VERSION",
				goSays:   "42702",
			},
		}

		var disagreed []string
		for _, c := range cases {
			javaOut := render(javaRunner.RunWithSetup(ctx, schema, setup, c.sql))
			goOut := render(goRunner.RunWithSetup(ctx, schema, setup, c.sql))
			mark := "  "
			if rejects(javaOut) != rejects(goOut) || (!rejects(javaOut) && javaOut != goOut) {
				mark = "!!"
				disagreed = append(disagreed, fmt.Sprintf(
					"%s\n    java: %s\n    go  : %s\n    sql : %s",
					c.name, javaOut, goOut, c.sql))
			}
			fmt.Fprintf(GinkgoWriter, "%s %-44s java=%-34s go=%s\n", mark, c.name, javaOut, goOut)

			// An arm that names its fault must SHOW that fault. Agreement on
			// "refused" is a weaker statement than this file makes, and both
			// engines drifting to a different refusal would keep the agreement
			// check green while the header's word became false.
			for _, e := range []struct{ engine, out, want string }{
				{"java", javaOut, c.javaSays}, {"go", goOut, c.goSays},
			} {
				if e.want == "" {
					continue
				}
				if !strings.Contains(e.out, e.want) {
					disagreed = append(disagreed, fmt.Sprintf(
						"%s: %s did not report %q — this arm asserts WHICH fault, not merely "+
							"that there was one\n    %s: %s\n    sql : %s",
						c.name, e.engine, e.want, e.engine, e.out, c.sql))
				}
			}
		}

		Expect(disagreed).To(BeEmpty(),
			"the engines disagree on __ROW_VERSION as a USING column, for %d of %d shapes. "+
				"This column is not a descriptor field, so it is the one that exposes a "+
				"resolver reading two different column surfaces — owner from one, right-side "+
				"check from the other.\n\n%s",
			len(disagreed), len(cases), strings.Join(disagreed, "\n"))
		Expect(cases).To(HaveLen(3),
			"the shape population changed; the no-hiding arm is the one that carries this file")
	})
})

// A QUOTED, lower-case USING column.
//
// Quoted identifiers keep their case; unquoted ones fold to upper. A resolver
// that normalises a USING column by upper-casing it unconditionally therefore
// asks the catalog for `K` when the column is `k`, and refuses a valid query —
// while the same query planned before ownership consulted a column surface at
// all, because nothing looked the name up.
//
// This is the direction that matters most: Go refusing what Java answers. It is
// measured rather than reasoned about, because "the normalisation is obviously
// fine" is the shape of every other thing on this branch that was not.
var _ = Describe("JoinUsingQuotedIdentifierJavaProbe", func() {
	It("measures both engines on a quoted lower-case USING column", func() {
		ctx := context.Background()
		tenantName := fmt.Sprintf("usingquoted_%s", uuid.New().String())
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

		// q3 exposes an UNQUOTED K, which folds to `K` — a different column
		// from q1/q2's quoted `"k"`. That distinction is what the hidden-map
		// arm below turns on.
		const schema = `CREATE TABLE q1 ("id" BIGINT, "k" BIGINT, PRIMARY KEY ("id")) ` +
			`CREATE TABLE q2 ("id" BIGINT, "k" BIGINT, PRIMARY KEY ("id")) ` +
			`CREATE TABLE q3 ("id" BIGINT, K BIGINT, PRIMARY KEY ("id")) ` +
			`CREATE TABLE q4 ("id" BIGINT, "z" BIGINT, PRIMARY KEY ("id"))`
		setup := []string{
			`INSERT INTO q1 VALUES (1, 10), (2, 20)`,
			`INSERT INTO q2 VALUES (1, 10), (2, 99)`,
			`INSERT INTO q3 VALUES (1, 10), (2, 20)`,
			`INSERT INTO q4 VALUES (1, 7), (2, 8)`,
		}

		render := func(r plandiff.RunResult) string {
			if r.Err != nil {
				return "ERR(" + r.Err.Error() + ")"
			}
			return fmt.Sprint(r.Rows.Rows)
		}

		cases := []struct {
			name, sql string
			// javaSays and goSays mark a PINNED divergence: both sides asserted
			// verbatim, so the arm reddens on a repair as well as on a drift.
			javaSays, goSays string
		}{
			{
				name: "quoted USING against a base table",
				sql:  `SELECT q1."id" FROM q1 JOIN q2 USING ("k") ORDER BY q1."id"`,
			},
			{
				// THE HIDDEN MAP MUST BE AS QUOTE-AWARE AS OWNERSHIP IS.
				// `q1` and `q2` expose a quoted lower-case `"k"`; `q3` exposes
				// a plain `K`. The first USING hides the quoted `"k"` on its
				// right, and the second names `K` — a DIFFERENT column. A
				// hidden map keyed on an upper-folded name collapses the two
				// and hides a column that was never hidden, changing which
				// source owns it.
				//
				// This is the same case-folding defect the ownership lookup was
				// repaired for, and it survived in the sibling map — which is
				// why it gets its own arm rather than being assumed to travel
				// with the first fix. The hidden map now derives its key
				// EXACTLY as ownership does, so the two cannot disagree.
				//
				// WHAT THIS ARM THEN FOUND, one layer down and pre-existing:
				// Java treats `"K"` and `"k"` as different columns and reports
				// `Unknown reference K`, while Go reaches one from the other
				// and finds TWO owners. That is not this resolver — the key
				// derivation above is quote-aware and identical on both sides
				// of the comparison.
				//
				// It is the scope's case-insensitive SECOND PASS, which is a
				// deliberate read-side extension (RFC-236 §3.4): Go does not
				// plumb Java's CASE_SENSITIVE_IDENTIFIERS option, and wrapping a
				// hand-written .proto as a SQL catalog — where field names never
				// went through DDL normalization — is a first-class entry point
				// here. Pinned with both engines' text. Closing it means
				// plumbing the option, not deleting the pass.
				// A CHAINED quoted USING where the owner is NOT the prior right
				// leg. `q3` has no `"k"`, so the second USING's owner must be
				// `q1` — which only works if the column multiset carries the
				// quoted spelling exactly as the catalog exports it. If
				// `Columns()` exposed a folded key while the parsed name stayed
				// quoted, the lookup would miss, the join would decline, and
				// the positional predicate would ask q3 for a column it does
				// not have.
				// PINNED: the scope's case-insensitive second pass reaches q3's
				// unquoted `K` from a lookup for `"k"`, so q3 becomes a second
				// owner. Java keeps them distinct and finds one. Same cause as
				// the arm above — the read-side extension of RFC-236 §3.4, not
				// a fold in any name.
				name: "chained quoted USING resolving to the far-left source",
				sql: `SELECT q1."id" FROM q1 JOIN q3 USING ("id") ` +
					`JOIN q2 USING ("k") ORDER BY q1."id"`,
				javaSays: "[[1]]",
				goSays:   "42702",
			},
			{
				// THE SAME CHAIN WITH AN INTERVENING SOURCE THAT HAS NO `k` AT
				// ALL. q4 exposes only "id" and "z", so the second USING's owner
				// can only be q1 — and unlike the arm above, no second source
				// can be masking the answer by coincidence.
				//
				// It is the arm that decides whether the column multiset carries
				// a quoted lower-case name as written. If `Columns()` exposed a
				// FOLDED identifier while only `LookupColumn` knew the exact
				// spelling, the multiset would miss `"k"` entirely, the join
				// would decline, and the positional predicate would ask q4 for a
				// column it does not have.
				name: "chained quoted USING over an intervening source without the column",
				sql: `SELECT q1."id" FROM q1 JOIN q4 USING ("id") ` +
					`JOIN q2 USING ("k") ORDER BY q1."id"`,
			},
			{
				name: "a quoted USING must not hide the unquoted column",
				sql: `SELECT q1."id" FROM q1 JOIN q2 USING ("k") ` +
					`JOIN q3 USING ("K") ORDER BY q1."id"`,
				javaSays: "Unknown reference K",
				goSays:   "[[1]]",
			},
			{
				// The USING spelling of the arm above — same cause, kept because
				// it is the shape a user is most likely to write, and because
				// it is what a review first reported as a USING bug.
				//
				// It is not one: the ON form fails identically, and bypassing
				// `retargetUsingJoins` entirely changes nothing. The report was
				// half right — the retarget DID fold the column name, which is
				// fixed — but that was never what refuses the query.
				// THE SAME QUERY WITH NO `USING` AT ALL, which is what said the
				// fault was never in USING resolution: an explicit ON naming
				// the same quoted column failed identically, so the subject is
				// a quoted reference into a DERIVED source's row.
				//
				// BOTH OF THESE WERE PINNED DIVERGENCES AND BOTH ARE REPAIRED.
				// Go answered `executor.layout` here because three identifier
				// models coexisted and disagreed: the record catalog PRESENTED
				// folded names, a derived table's StaticTable presented names
				// as built and matched exactly, and reference resolution
				// preserved a quoted name while folding an unquoted one — so
				// D's scope said RECORD(id,k) while the row that flowed said
				// RECORD(ID,K).
				//
				// There is one model now (RFC-236): a name is normalized once,
				// at the parse boundary, and carried verbatim after it. These
				// two shapes are therefore asserted as plain AGREEMENT rather
				// than as pinned disagreement, which is a strictly stronger
				// check — a pin only fails when the named substring moves,
				// while agreement fails on any difference at all.
				name: "quoted column into a derived source, plain ON join",
				sql: `SELECT q1."id" FROM q1 JOIN (SELECT "id", "k" FROM q2) d ` +
					`ON q1."k" = d."k" ORDER BY q1."id"`,
			},
			{
				name: "quoted USING against a derived right leg",
				sql: `SELECT q1."id" FROM q1 JOIN (SELECT "id", "k" FROM q2) d USING ("k") ` +
					`ORDER BY q1."id"`,
			},
		}

		var disagreed []string
		for _, c := range cases {
			javaOut := render(javaRunner.RunWithSetup(ctx, schema, setup, c.sql))
			goOut := render(goRunner.RunWithSetup(ctx, schema, setup, c.sql))
			mark := "  "
			switch {
			case c.javaSays != "" || c.goSays != "":
				// A pinned divergence: both sides are asserted verbatim so the
				// pin fails if either engine moves, in either direction.
				for _, e := range []struct{ engine, out, want string }{
					{"java", javaOut, c.javaSays}, {"go", goOut, c.goSays},
				} {
					if e.want != "" && !strings.Contains(e.out, e.want) {
						mark = "!!"
						disagreed = append(disagreed, fmt.Sprintf(
							"%s: %s no longer reports %q — a PINNED divergence changed, which "+
								"is either the repair or a new fault\n    %s: %s\n    sql : %s",
							c.name, e.engine, e.want, e.engine, e.out, c.sql))
					}
				}
			case javaOut != goOut:
				mark = "!!"
				disagreed = append(disagreed, fmt.Sprintf(
					"%s\n    java: %s\n    go  : %s\n    sql : %s",
					c.name, javaOut, goOut, c.sql))
			}
			fmt.Fprintf(GinkgoWriter, "%s %-44s java=%-24s go=%s\n", mark, c.name, javaOut, goOut)
		}

		Expect(disagreed).To(BeEmpty(),
			"the engines disagree on a QUOTED USING column, for %d of %d shapes. A quoted "+
				"identifier keeps its case, so anything that folds a USING column to upper "+
				"asks for a column that does not exist.\n\n%s",
			len(disagreed), len(cases), strings.Join(disagreed, "\n"))
	})
})
