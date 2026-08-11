package docscheck

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// RFC-197's `## Order` section carries TWO kinds of claim, and only one of them
// was gated.
//
// The census gates hold its COUNTS to `knownFieldDecisionDebt` in both
// directions. Its DEPENDENCY claims — which producer's output which reader
// consumes, and therefore what retires behind what — went unwatched in the same
// document, in the same section, with the same rot class. That half shipped
// wrong: the section's headline named a mint and asserted that killing it
// "mechanically retires" a list of readers, and re-derivation by dataflow found
// that NOT ONE of them reads that mint. Three refuse a dotted name outright and
// therefore cannot retire when dotted names stop being minted; the fourth
// composes its own key on a different channel. The mint's real consumer lived in
// a third package and was not named in the section at all.
//
// THE FAILURE WAS METHODOLOGICAL, NOT ARITHMETIC, WHICH IS WHY A COUNT GATE
// COULD NEVER HAVE CAUGHT IT. The graph was assembled by CO-OCCURRENCE of the
// `LEG.COL` string shape rather than by dataflow. Every site named touches a
// dotted string; none touches that producer's. Co-occurrence errs in ONE
// direction — it inflates fan-out, never deflates it — so the resulting graph
// reads as leverage and gets planned as leverage. Every number in the document
// was correct throughout.
//
// WHAT THIS GATE CHECKS, and it is deliberately the mechanical part only:
//
//   - every identifier named in either column of the edges table is a LIVE
//     declaration somewhere in the tracked Go tree (so an edge cannot outlive a
//     rename or a deletion — the "names a function that no longer exists" case);
//   - every READER is declared in the file the row cites as evidence (so the
//     citation points at the code that decides the relationship, rather than at
//     a file that merely once contained it);
//   - the class is one of the three the section defines;
//   - a `decliner` or `co-occurring` edge does not claim `retires = yes`.
//
// That last rule is the one that mechanically forbids the exact error that
// shipped. It is a ONE-directional implication and is stated that way on
// purpose:
//
//	decliner     => retires = no
//	co-occurring => retires = no
//	consumer     => retires may be yes OR no
//
// A consumer that reads from more than one producer does not retire behind any
// single one of them — two rows in the live table are exactly that shape — so
// `consumer` cannot imply `yes` and this gate must not pretend otherwise.
//
// WHAT THIS GATE CANNOT CHECK, said plainly because a gate whose limits are
// unstated gets read as covering more than it does: it cannot verify that a
// row's CLASS is the TRUE one. Deciding whether a reader parses the minted
// string, refuses it, or merely touches a same-shaped string from elsewhere is a
// reading of dataflow, and no regex over a markdown table can perform it. What
// the gate buys is that a claim must be well-formed, must name live code, must
// cite the deciding site, and must not draw a retirement conclusion its own
// stated class forbids. A wrong class is still possible; a wrong class that also
// claims leverage is not.
const fieldDebtOrderEdgesMarker = "<!-- FIELD-DEBT-ORDER-EDGES -->"

// The closed vocabulary. Adding a class is a deliberate act — a new relationship
// kind needs its own retirement rule below, so it cannot be introduced by
// writing a new word into the table.
const (
	edgeClassConsumer    = "consumer"
	edgeClassDecliner    = "decliner"
	edgeClassCoOccurring = "co-occurring"
)

// edgeClassMayRetire reports whether a class is permitted to claim that its
// reader retires when its producer is killed.
//
// The two `false` arms are the whole point of the gate. A DECLINER refuses the
// string; the shape stays legal however few instances are minted, because
// `DOUBLE_QUOTE_ID` makes `A.B` and `"A.B"` the same bytes — such a site retires
// on `Resolved` arity, never on a producer going away. A CO-OCCURRING site reads
// a different producer's output entirely.
func edgeClassMayRetire(class string) (known, mayRetire bool) {
	switch class {
	case edgeClassConsumer:
		return true, true
	case edgeClassDecliner, edgeClassCoOccurring:
		return true, false
	}
	return false, false
}

type orderEdge struct {
	producer string
	reader   string
	class    string
	retires  string
	evidence string
}

// goIdent matches a bare Go identifier. Cells arrive from parseFieldDebtTable
// with their backticks already stripped, so this is the post-strip shape. A cell
// that is not one is a malformed row rather than a name to look up — reported as
// such instead of silently scanned for.
var goIdent = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// parseOrderEdges turns the marked table's rows into edges. Pure over explicit
// state so TestFieldDebtOrderEdgeGateArms can drive every arm; problems are
// returned rather than fataled for the same reason.
//
// The header row is dropped by NAME rather than by position, so a table that
// lost its header does not silently forfeit its first data row instead.
func parseOrderEdges(rows [][]string) (edges []orderEdge, problems []string) {
	for i, r := range rows {
		if len(r) > 0 && strings.TrimSpace(r[0]) == "producer" {
			continue
		}
		if len(r) != 5 {
			problems = append(problems, fmt.Sprintf(
				"edges table row %d has %d cells, want 5 (producer | reader | class | retires | evidence)",
				i+1, len(r)))
			continue
		}
		e := orderEdge{
			producer: strings.TrimSpace(r[0]),
			reader:   strings.TrimSpace(r[1]),
			class:    strings.TrimSpace(r[2]),
			retires:  strings.TrimSpace(r[3]),
			evidence: strings.TrimSpace(r[4]),
		}
		for _, c := range []struct{ what, cell string }{{"producer", e.producer}, {"reader", e.reader}} {
			if !goIdent.MatchString(c.cell) {
				problems = append(problems, fmt.Sprintf(
					"edges table row %d: %s cell %q is not a bare Go identifier; "+
						"an edge names DECLARATIONS so the existence check below can be mechanical",
					i+1, c.what, c.cell))
			}
		}
		if e.retires != "yes" && e.retires != "no" {
			problems = append(problems, fmt.Sprintf(
				"edges table row %d: retires = %q, want \"yes\" or \"no\"", i+1, e.retires))
		}
		edges = append(edges, e)
	}
	if len(edges) == 0 {
		problems = append(problems, "edges table parsed to no rows at all — a dependency "+
			"graph that describes nothing passes every per-row check, which is the "+
			"green-from-an-empty-set shape this document's other gates are built against")
	}
	return edges, problems
}

// checkEdgeClasses applies the vocabulary and the retirement implication. Pure
// over explicit state.
func checkEdgeClasses(edges []orderEdge) (problems []string) {
	seenClass := map[string]int{}
	for i, e := range edges {
		known, mayRetire := edgeClassMayRetire(e.class)
		if !known {
			problems = append(problems, fmt.Sprintf(
				"edges table row %d (%s -> %s): class %q is not one of %q, %q, %q.\n"+
					"  The vocabulary is closed because each class carries its own retirement "+
					"rule; a new word would arrive with no rule and be checked by nothing.",
				i+1, e.producer, e.reader, e.class,
				edgeClassConsumer, edgeClassDecliner, edgeClassCoOccurring))
			continue
		}
		seenClass[e.class]++
		if e.retires == "yes" && !mayRetire {
			problems = append(problems, fmt.Sprintf(
				"edges table row %d: %s -> %s is classed %q but claims retires = yes.\n"+
					"  A %s does not retire when its producer stops minting — this is the EXACT "+
					"claim that shipped wrong in this section, where a mint was said to "+
					"mechanically retire readers that refuse its output or read a different "+
					"channel. Either the class is wrong or the retirement claim is; both are "+
					"a dataflow question, not a wording one.",
				i+1, e.producer, e.reader, e.class, e.class))
		}
	}
	// Both directions, as the census gates are. A table that has quietly become
	// all-consumer has lost the distinction it exists to record, and would pass
	// every per-row check above while re-admitting the retracted reading.
	if seenClass[edgeClassConsumer] == 0 {
		problems = append(problems, "edges table records no consumer edge at all — "+
			"the graph then claims nothing retires behind anything, and the section's "+
			"ordering rests on nothing")
	}
	if seenClass[edgeClassDecliner]+seenClass[edgeClassCoOccurring] == 0 {
		problems = append(problems, "edges table records no decliner and no co-occurring "+
			"edge — the distinction this table exists to draw has collapsed, and a "+
			"co-occurrence graph would pass unnoticed exactly as it did before")
	}
	return problems
}

// TestFieldDebtRFCOrderEdgesAreClassified is the dependency half of the RFC-197
// `## Order` gate, the half #738 left unwatched while gating the counts.
func TestFieldDebtRFCOrderEdgesAreClassified(t *testing.T) {
	t.Parallel()
	// sourceTreeRoot, NOT repoRoot: the RFC is not staged into this target's
	// runfiles, so the repoRoot walk-up lands on the staged tree and reads a file
	// that is not there. The census gates on this same document resolve it the
	// same way, and this test failed under Bazel while passing under `go test`
	// until it did — the two are different claims and only the second is CI's.
	tree := sourceTreeRoot(t)
	src := readDoc(t, tree, fieldDebtRFCPath)

	section, ok := orderSectionOf(src)
	if !ok {
		t.Fatalf("%s: no `## Order` section — this gate reads the section, so an "+
			"absent one must fail loudly rather than scan an empty string", fieldDebtRFCPath)
	}
	rows, found := parseFieldDebtTable(section, fieldDebtOrderEdgesMarker)
	if p := tablePresenceProblem(rows, found, fieldDebtOrderEdgesMarker); p != "" {
		t.Fatal(p)
	}

	edges, problems := parseOrderEdges(rows)
	problems = append(problems, checkEdgeClasses(edges)...)

	// Existence, against the real source tree rather than the runfiles staging.
	decls := declaredIdents(t, tree)
	if len(decls) < 1000 {
		t.Fatalf("declared-identifier scan found only %d names over %s — the walk is "+
			"reading the wrong tree, and an empty scan would report every edge as naming "+
			"a dead declaration (or, with the polarity reversed, none)", len(decls), tree)
	}

	for i, e := range edges {
		for _, c := range []struct{ what, cell string }{{"producer", e.producer}, {"reader", e.reader}} {
			if !goIdent.MatchString(c.cell) {
				continue // already reported by parseOrderEdges
			}
			if _, live := decls[c.cell]; !live {
				problems = append(problems, fmt.Sprintf(
					"edges table row %d: %s %q is declared nowhere in the tracked Go tree.\n"+
						"  A dependency claim naming code that no longer exists is the rot this "+
						"gate was added for: it reads as a live constraint and plans as one.",
					i+1, c.what, c.cell))
			}
		}
		if !goIdent.MatchString(e.reader) {
			continue
		}
		if p := readerDeclaredIn(tree, e.evidence, e.reader); p != "" {
			problems = append(problems, fmt.Sprintf("edges table row %d: %s", i+1, p))
		}
	}

	sort.Strings(problems)
	for _, p := range problems {
		t.Error(p)
	}
}

// readerDeclaredIn checks the evidence citation points at the file declaring the
// reader. Returns "" when it does.
//
// The evidence column carries a FILE and deliberately no line number. A cited
// line rots on the next edit above it and rots SILENTLY, which would rebuild the
// stale-citation problem inside the gate meant to prevent it; a file that must
// contain the declaration is checkable and stable.
func readerDeclaredIn(tree, evidence, reader string) string {
	if evidence == "" || !strings.HasSuffix(evidence, ".go") {
		return fmt.Sprintf("evidence %q is not a .go path; the citation must name the file "+
			"whose code decides the relationship", evidence)
	}
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, filepath.Join(tree, evidence), nil, parser.SkipObjectResolution)
	if err != nil {
		return fmt.Sprintf("evidence %q does not parse: %v", evidence, err)
	}
	if findFunc(f, reader) == nil {
		return fmt.Sprintf("reader %q is not declared in its evidence file %q — the citation "+
			"names a file that does not contain the code deciding this edge", reader, evidence)
	}
	return ""
}

// declaredIdents collects every top-level declared name in the tracked Go tree —
// funcs, methods, types, vars, consts. Broad on purpose: an edge may legitimately
// name a carrier type (the `projCol` channel) rather than a function, and the
// question this set answers is "does this name still exist", not "is it a func".
func declaredIdents(t *testing.T, tree string) map[string]struct{} {
	t.Helper()
	out := map[string]struct{}{}
	fset := token.NewFileSet()
	for _, rel := range trackedGoFiles(t, tree) {
		f, err := parser.ParseFile(fset, filepath.Join(tree, rel), nil, parser.SkipObjectResolution)
		if err != nil {
			continue // an unparseable file is not evidence of absence
		}
		for _, d := range f.Decls {
			switch decl := d.(type) {
			case *ast.FuncDecl:
				if decl.Name != nil {
					out[decl.Name.Name] = struct{}{}
				}
			case *ast.GenDecl:
				for _, s := range decl.Specs {
					switch spec := s.(type) {
					case *ast.TypeSpec:
						if spec.Name != nil {
							out[spec.Name.Name] = struct{}{}
						}
					case *ast.ValueSpec:
						for _, n := range spec.Names {
							out[n.Name] = struct{}{}
						}
					}
				}
			}
		}
	}
	return out
}
