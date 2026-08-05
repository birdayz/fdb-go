//go:build bazelrunfiles

package conformance_test

// Measures BOTH engines on a MIXED-CASE quoted DDL column (`"KeepCase"`) and
// pins what measurement actually found — which is the OPPOSITE of the claim
// this probe was written to verify.
//
// The claim on record was "a quoted DDL column is created but unreferenceable
// by name". Its quoted-LOWERCASE half had already been refuted; the surviving
// residue was the mixed-case spelling, unmeasured for over a month and alive
// only as a parenthetical in a Go test comment that said it was "not exercised
// here". Measurement inverts the residue too: `SELECT "KeepCase"` returns the
// value on Go exactly as on Java, in projection AND in a predicate. The column
// is referenceable by name.
//
// What measurement DID find is a divergence in the other direction: Go resolves
// a quoted identifier CASE-INSENSITIVELY, so `KeepCase`, `"KEEPCASE"` and
// `"keepcase"` all reach the column, while Java treats quoting as
// case-PRESERVING and raises 42703 for every spelling but the exact one. Go is
// MORE permissive, not less — it accepts references Java rejects, rather than
// rejecting references Java accepts. It also REPORTS the column folded
// (`KEEPCASE` against Java's `KeepCase`), which is the same fact seen from the
// result-metadata side.
//
// This is a READ-SIDE name divergence, not a wire one. The stored proto
// descriptor keeps the quoted spelling verbatim on both engines
// (parseColumnDefinitions takes StripIdentifierQuotes' verbatim quoted
// segment); the fold happens when the row layout is derived for execution, at
// executor.PositionalTypeForDescriptor, which upper-cases every field name.
// That split — descriptor case-preserving, row layout folded — is pinned
// directly by executor.TestPositionalTypeFoldsDescriptorFieldCase, so the
// "wire is unaffected" half of this entry is measured too and not asserted here
// by inspection.
//
// Per the section contract these assertions state CURRENT behaviour: RED means
// the divergence moved. If Go starts rejecting the folded spellings (Java
// parity) the shapes marked goOnly must be re-read as CLOSED, not relaxed.

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

// quotedCaseOutcome is one engine's answer reduced to what the probe compares:
// accepted-or-not, the scalar rendering when accepted, and the engine's
// SQLSTATE plus message otherwise. The message is kept because one Java
// rejection in this battery carries NO SQLSTATE, and a stateless rejection must
// still be distinguishable from a 42703.
type quotedCaseOutcome struct {
	accepted bool
	value    string
	detail   string
}

func (o quotedCaseOutcome) String() string {
	if o.accepted {
		return "ACCEPT(" + o.value + ")"
	}
	return "REJECT(" + o.detail + ")"
}

var _ = Describe("QuotedIdentifierCaseJavaProbe", func() {
	It("measures both engines on a mixed-case quoted DDL column", func() {
		ctx := context.Background()
		tenantName := fmt.Sprintf("qcase_%s", uuid.New().String())
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

		// `plain` is declared UNQUOTED alongside the quoted column: it is the
		// control that separates "Go ignores quoting when resolving" from "Go
		// lost the quoted spelling at CREATE time". An unquoted DDL name folds
		// to upper on BOTH engines, so `"PLAIN"` must resolve on both while
		// `"plain"` must not — on Java.
		//
		// The INSERT is POSITIONAL on purpose: naming the column in the setup
		// would abort the setup on a resolution failure rather than surface it
		// per-shape, and the original claim was precisely that the column
		// exists but cannot be named.
		schema := `CREATE TABLE QCASE (id BIGINT, "KeepCase" BIGINT, plain BIGINT, PRIMARY KEY (id))`
		setup := []string{"INSERT INTO QCASE VALUES (1, 42, 7)"}

		classify := func(r plandiff.RunResult) quotedCaseOutcome {
			if r.Err != nil {
				var je *plandiff.JavaError
				if errors.As(r.Err, &je) {
					return quotedCaseOutcome{detail: je.SQLState + " " + je.Message}
				}
				var ge *api.Error
				if errors.As(r.Err, &ge) {
					return quotedCaseOutcome{detail: string(ge.Code) + " " + ge.Message}
				}
				return quotedCaseOutcome{detail: "?:" + r.Err.Error()}
			}
			names := make([]string, 0, len(r.Rows.Columns))
			for _, c := range r.Rows.Columns {
				names = append(names, c.Name)
			}
			return quotedCaseOutcome{
				accepted: true,
				value:    fmt.Sprint(names) + fmt.Sprint(r.Rows.Rows),
			}
		}

		const (
			// both engines answer, and must answer identically.
			agreeAccept = "agree-accept"
			// both engines reject.
			agreeReject = "agree-reject"
			// the measured divergence: Go answers where Java raises 42703
			// because quoting is case-preserving there.
			goOnly = "go-only"
		)

		type probe struct {
			name, sql string
			mode      string
			// wantJava/wantGo are SEPARATE because the engines agree on every
			// VALUE while differing on the reported column NAME — collapsing
			// them to one expectation would have to drop the names, and the
			// names are half the finding. wantJava applies to agreeAccept
			// shapes; wantGo applies to agreeAccept AND goOnly, where it names
			// the single column+value the spelling must bind to.
			wantJava, wantGo string
			why              string
		}
		probes := []probe{
			{
				name: "exact_quoted_projection", mode: agreeAccept,
				sql:      `SELECT "KeepCase" FROM QCASE WHERE id = 1`,
				wantJava: `[KeepCase][[42]]`,
				wantGo:   `[KEEPCASE][[42]]`,
				why:      "REFUTES the old claim: the quoted mixed-case column IS referenceable and returns the SAME value on both engines — only the reported column NAME is folded on Go",
			},
			{
				name: "exact_quoted_predicate", mode: agreeAccept,
				sql:      `SELECT id FROM QCASE WHERE "KeepCase" = 42`,
				wantJava: `[ID][[1]]`,
				wantGo:   `[ID][[1]]`,
				why:      "refutation holds in predicate position too, not only in projection — and here the engines agree COMPLETELY, name included, because the quoted column is not the projected one",
			},
			{
				name: "star_sees_the_column", mode: agreeAccept,
				sql:      `SELECT * FROM QCASE WHERE id = 1`,
				wantJava: `[ID KeepCase PLAIN][[1 42 7]]`,
				wantGo:   `[ID KEEPCASE PLAIN][[1 42 7]]`,
				why:      "every column is present with the same VALUE on both engines; the unquoted `plain` folds to upper on both, isolating the fold to the QUOTED name on Go",
			},
			{
				name: "unquoted_same_spelling", mode: goOnly,
				sql:    `SELECT KeepCase FROM QCASE WHERE id = 1`,
				wantGo: `[KEEPCASE][[42]]`,
				why:    "an unquoted reference folds to KEEPCASE; Java's quoted column is case-preserving so it does not match",
			},
			{
				name: "quoted_upper", mode: goOnly,
				sql:    `SELECT "KEEPCASE" FROM QCASE WHERE id = 1`,
				wantGo: `[KEEPCASE][[42]]`,
				why:    "a quoted reference is exact on Java; Go folds it",
			},
			{
				name: "quoted_lower", mode: goOnly,
				sql:    `SELECT "keepcase" FROM QCASE WHERE id = 1`,
				wantGo: `[KEEPCASE][[42]]`,
				why:    "the same in the lower direction — this is the spelling the retired half of the claim was about",
			},
			{
				name: "quoted_upper_of_unquoted_ddl_control", mode: agreeAccept,
				sql:      `SELECT "PLAIN" FROM QCASE WHERE id = 1`,
				wantJava: `[PLAIN][[7]]`,
				wantGo:   `[PLAIN][[7]]`,
				why:      "control: an UNQUOTED DDL name folds to upper, so its quoted-upper reference is exact and both engines resolve it — proves the goOnly shapes are about CASE, not about quoting per se",
			},
			{
				name: "quoted_lower_of_unquoted_ddl_control", mode: goOnly,
				sql:    `SELECT "plain" FROM QCASE WHERE id = 1`,
				wantGo: `[PLAIN][[7]]`,
				why:    "control in the other direction: Go's folding is a property of its resolver, not of the quoted-DDL path — it over-resolves an unquoted-DDL column too",
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
			switch p.mode {
			case agreeAccept:
				switch {
				case !java.accepted || !goSide.accepted:
					fail("expected BOTH engines to answer: java=%s go=%s", java, goSide)
				case java.value != p.wantJava:
					fail("Java answer changed: got %s want ACCEPT(%s)", java, p.wantJava)
				case goSide.value != p.wantGo:
					fail("Go answer changed: got %s want ACCEPT(%s)", goSide, p.wantGo)
				}
			case agreeReject:
				if java.accepted || goSide.accepted {
					fail("expected BOTH engines to reject: java=%s go=%s", java, goSide)
				}
			case goOnly:
				switch {
				case java.accepted:
					fail("Java now RESOLVES the folded spelling — the divergence is gone from Java's side: java=%s", java)
				case !strings.Contains(java.detail, "42703"):
					// A rejection for some OTHER reason (a plan failure, a
					// syntax error) would wear this shape's expected outcome
					// while measuring nothing about case resolution.
					fail("Java rejected, but not as undefined-column 42703: java=%s", java)
				case !goSide.accepted:
					fail("Go now REJECTS the folded spelling — Java parity may have been reached; re-read the watch-list entry rather than relaxing this: go=%s", goSide)
				case goSide.value != p.wantGo:
					// Each spelling names the ONE column+value it must bind to.
					// Accepting any of the table's columns here would let a
					// resolver that bound "plain" to KeepCase — a WRONG-COLUMN
					// answer, far worse than the over-permissiveness under
					// test — pass as the expected divergence.
					fail("Go resolved the folded spelling to the WRONG column or value: got %s want ACCEPT(%s)",
						goSide, p.wantGo)
				}
			}
			fmt.Fprintf(GinkgoWriter, "%s %-38s %-11s java=%-34s go=%-34s %s\n",
				mark, p.name, p.mode, java, goSide, p.sql)
		}

		Expect(problems).To(BeEmpty(),
			"mixed-case quoted-identifier resolution is no longer what the watch-list records.\n"+
				"The entry states that the quoted column IS referenceable on both engines and that Go\n"+
				"additionally over-resolves folded spellings Java rejects with 42703. Re-read the entry\n"+
				"against this measurement before touching an assertion.\n"+
				strings.Join(problems, "\n"))
	})
})
