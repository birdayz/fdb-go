package explaindiff_test

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/antlr4-go/antlr/v4"

	"fdb.dev/pkg/relational/conformance/explaindiff"
	"fdb.dev/pkg/relational/conformance/yamsql"
	"fdb.dev/pkg/relational/core/embedded"
	"fdb.dev/pkg/relational/core/parser"
	antlrgen "fdb.dev/pkg/relational/core/parser/gen"
)

// identifierAgreementFloor is the minimum number of corpus statements this gate
// must actually PERTURB before its verdict means anything.
//
// A green from an empty set is the failure this floor exists to stop, and it is
// reachable three separate ways here: the parser could stop producing UidContext
// nodes, the walk could stop finding them, or every baseline plan could start
// erroring. Each renders as "0 disagreements" — indistinguishable from success.
//
// Measured at 2443 of 2447 plannable statements when written. The floor sits
// well below that so ordinary corpus churn does not trip it; a drop to 2000 is
// not churn, it is the instrument dying.
const identifierAgreementFloor = 2000

// identifierAgreementBaseFailCeiling caps the statements excluded because their
// UNPERTURBED form does not plan.
//
// This is not a tidiness metric, it is the gate's blind spot made observable. A
// difference test cannot see a defect that breaks BOTH spellings equally: when
// the operand mint was reverted to raw parse text, every compound aggregate
// stopped planning on both sides, each landed in this bucket, and the gate
// reported green over the remaining statements. The count went 4 -> 31 while
// the verdict did not move. So the count is the assertion.
//
// Measured at 4. The alarm direction is GROWTH.
const identifierAgreementBaseFailCeiling = 10

// identifierAgreementVerdict is the gate's decision, split out from the corpus
// walk so every arm takes explicit state and can be driven by a unit test.
// Returns "" when the run is a genuine pass. The corpus reading exercises only
// the arms the corpus happens to reach — today that is the pass arm and nothing
// else, which is exactly how a gate ships with a broken alarm.
func identifierAgreementVerdict(perturbed, baseFailed int, disagree []string) string {
	if perturbed < identifierAgreementFloor {
		return fmt.Sprintf("only %d statements were perturbed, floor is %d — the gate is "+
			"not measuring the corpus any more, and a clean verdict from it means nothing",
			perturbed, identifierAgreementFloor)
	}
	if baseFailed > identifierAgreementBaseFailCeiling {
		return fmt.Sprintf("%d statements did not plan even UNPERTURBED, ceiling is %d. "+
			"That bucket is this gate's blind spot: a defect that breaks both spellings "+
			"equally lands here and is compared against nothing, so growth in it is a "+
			"loss of coverage that reads as a pass",
			baseFailed, identifierAgreementBaseFailCeiling)
	}
	if len(disagree) > 0 {
		return fmt.Sprintf("%d of %d statements plan differently under an equivalent "+
			"identifier spelling. A quoted upper-case identifier and its unquoted twin are "+
			"the SAME NAME; a difference is a site that normalizes twice, or reads raw "+
			"parse text where it should read a name:\n\n%s",
			len(disagree), perturbed, strings.Join(disagree, "\n\n"))
	}
	return ""
}

// TestIdentifierAgreementOverCorpus asserts that an UNQUOTED identifier and its
// upper-folded, double-quoted twin are indistinguishable to the whole engine.
//
// Under the rule this port follows (Java's SemanticAnalyzer.normalizeString), a
// name is normalized ONCE at the parse boundary: `qty` folds to QTY, `"QTY"`
// keeps its case, and both are then the name QTY. So rewriting every unquoted
// identifier in a query to `"UPPER"` is semantics-preserving BY CONSTRUCTION,
// and any behavioural difference is a Go defect — no oracle, no golden, no
// second engine required.
//
// WHY THIS RATHER THAN A CENSUS OF FOLDS. The defect class it replaces was
// hunted by grepping for strings.ToUpper, and that instrument missed three live
// sites in one change: one had no fold to find (a relaxed lookup dropped into a
// cross-source adjudicator), one was outside the files being edited, and one had
// no ToUpper at the call site at all — it was a double-application of a function
// whose name promised a no-op. No sweep for a fold can find a fold spelled as a
// no-op. "Did I find every fold?" is unanswerable; "does any spelling disagree?"
// is checkable, and this checks it over 2000+ real statements.
//
// WHAT IT DOES NOT COVER — written from shapes actually run, before the list of
// what it does, because the positive claim is the half that rots:
//
//   - RESULT-SET LABELS. This compares planned PLAN TEXT. A naming authority
//     that only reaches executor.ColumnDef is invisible here. That axis belongs
//     to the yamsql `columns:` assertions (quoted_identifier_labels.yaml).
//   - ROWS. Nothing executes; no FDB, no data.
//   - ALREADY-QUOTED identifiers. `"KeepCase"` has no equivalent second
//     spelling, so nothing perturbs it. The corpus's own quoted scenarios are
//     what pin those.
//   - DDL. The schema_template is compiled once, unperturbed. A descriptor-side
//     fold is out of reach.
//   - Anything that is not a `uid` in the grammar: built-in function names
//     (scalarFunctionName), user-defined function names, and type names are
//     separate rules and are never rewritten.
//   - Statements whose BASELINE does not plan. Their count is ASSERTED rather
//     than logged, because it is where this gate goes blind: a defect that
//     breaks both spellings equally lands there and is compared against nothing.
//     See identifierAgreementBaseFailCeiling.
//   - A fold applied to a name that is genuinely lower-case. The perturbation
//     runs unquoted -> quoted-UPPER, and those two are the pair a fold cannot
//     tell apart, so a stray strings.ToUpper downstream is INVISIBLE here.
//     Verified by mutation: re-folding the AVG arm of the naming authority left
//     this gate green. That axis is the yamsql `columns:` arms and the unit pins
//     in expressions/group_by_naming_verbatim_test.go — this gate finds the
//     sites that treat the two SPELLINGS differently, which is a different
//     defect from folding a name, and the two instruments are not substitutes.
func TestIdentifierAgreementOverCorpus(t *testing.T) {
	t.Parallel()

	matches, err := filepath.Glob(filepath.Join(corpusDir, "*.yaml"))
	if err != nil {
		t.Fatalf("glob %s: %v", corpusDir, err)
	}
	if len(matches) == 0 {
		t.Fatalf("no *.yaml scenarios under %s", corpusDir)
	}
	sort.Strings(matches)

	var (
		plannable  int
		perturbed  int
		noIdent    int
		parseFail  int
		baseFailed int
		disagree   []string
	)
	for _, path := range matches {
		base := filepath.Base(path)
		s, loadErr := yamsql.Load(path)
		if loadErr != nil {
			t.Fatalf("load %s: %v", base, loadErr)
		}
		for i, tc := range s.Tests {
			plan, isPlannable := agreementHarness(tc.Query)
			if !isPlannable || tc.EffectiveErrorCode() != "" {
				continue
			}
			plannable++
			twin, n, perr := quoteEveryUnquotedIdentifier(tc.Query)
			switch {
			case perr != nil:
				parseFail++
				continue
			case n == 0:
				noIdent++
				continue
			}
			want := agreementPlanText(plan, tc.Query, s.SchemaTemplate)
			if strings.HasPrefix(want, agreementErrMarker) {
				baseFailed++
				continue
			}
			perturbed++
			if got := agreementPlanText(plan, twin, s.SchemaTemplate); got != want {
				disagree = append(disagree, fmt.Sprintf(
					"%s#%d\n    sql:      %s\n    twin:     %s\n    baseline: %s\n    twin got: %s",
					base, i, collapseSQL(tc.Query), collapseSQL(twin), want, got))
			}
		}
	}

	t.Logf("identifier agreement: %d perturbed of %d plannable "+
		"(%d had no unquoted identifier, %d did not reparse, %d did not plan at baseline)",
		perturbed, plannable, noIdent, parseFail, baseFailed)

	if v := identifierAgreementVerdict(perturbed, baseFailed, disagree); v != "" {
		t.Fatal(v)
	}
}

const agreementErrMarker = "<ERR:"

type agreementPlanner int

const (
	agreementSelect agreementPlanner = iota
	agreementDML
)

// agreementHarness picks the planner for a corpus statement, mirroring
// explaindiff's own classification so the two harnesses cover the same set.
func agreementHarness(stmt string) (agreementPlanner, bool) {
	if yamsql.IsQuery(stmt) {
		return agreementSelect, true
	}
	switch strings.ToUpper(strings.Fields(strings.TrimLeft(stmt, " \t\r\n("))[0]) {
	case "DELETE", "UPDATE":
		return agreementDML, true
	}
	return agreementSelect, false
}

// agreementPlanText plans one statement and renders it to a single comparable
// line, or to an `<ERR: …>` marker. A panic is caught and rendered too: the
// perturbation must not be able to crash the planner either, and a crash that
// vanished into a failed test binary would take the whole gate's verdict with
// it.
func agreementPlanText(which agreementPlanner, sql, schemaTemplate string) (out string) {
	defer func() {
		if r := recover(); r != nil {
			out = fmt.Sprintf("%s PANIC %v>", agreementErrMarker, r)
		}
	}()
	var (
		p   interface{ Explain() string }
		err error
	)
	switch which {
	case agreementDML:
		p, err = embedded.PlanPhysicalDMLForTest(sql, schemaTemplate, nil)
	default:
		p, err = embedded.PlanPhysicalForTest(sql, schemaTemplate, nil)
	}
	if err != nil {
		return agreementErrMarker + " " + collapseSQL(err.Error()) + ">"
	}
	return explaindiff.NormalizeAliases(collapseSQL(p.Explain()))
}

// quoteEveryUnquotedIdentifier rewrites every UNQUOTED identifier in sql to its
// upper-folded, double-quoted twin, and reports how many it rewrote.
//
// It works off UidContext nodes, never off the SQL text: `uid : simpleId |
// DOUBLE_QUOTE_ID`, so `SimpleId() != nil` IS the unquoted case, and the
// grammar's function-name and type-name rules are separate productions that
// never reach here. A text scan would have to decide what a keyword is.
func quoteEveryUnquotedIdentifier(sql string) (string, int, error) {
	root, err := parser.Parse(sql)
	if err != nil {
		return "", 0, err
	}
	// ANTLR token offsets index the CharStream in RUNES, not bytes. Slicing a
	// Go string by them corrupts every query holding a multi-byte rune —
	// `LENGTH('héllo')` rewrote to `FROM" "t` and reported four confident
	// "engine disagreements" that were entirely this function's.
	runes := []rune(sql)
	type span struct{ lo, hi int }
	var spans []span
	var walk func(t antlr.Tree)
	walk = func(t antlr.Tree) {
		if u, ok := t.(*antlrgen.UidContext); ok && u.SimpleId() != nil {
			lo, hi := u.GetStart().GetStart(), u.GetStop().GetStop()
			if lo >= 0 && hi >= lo && hi < len(runes) {
				spans = append(spans, span{lo, hi})
			}
		}
		for i := 0; i < t.GetChildCount(); i++ {
			walk(t.GetChild(i))
		}
	}
	walk(root)
	if len(spans) == 0 {
		return sql, 0, nil
	}
	// Back to front, so an earlier rewrite cannot move a later span.
	sort.Slice(spans, func(i, j int) bool { return spans[i].lo > spans[j].lo })
	out := runes
	for _, s := range spans {
		rep := []rune(`"` + strings.ToUpper(string(out[s.lo:s.hi+1])) + `"`)
		next := make([]rune, 0, len(out)+2)
		next = append(next, out[:s.lo]...)
		next = append(next, rep...)
		next = append(next, out[s.hi+1:]...)
		out = next
	}
	return string(out), len(spans), nil
}

func collapseSQL(s string) string { return strings.Join(strings.Fields(s), " ") }
