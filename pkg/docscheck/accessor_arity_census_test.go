package docscheck

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// The Accessors arity census is a classified, exact population. RFC-230
// originally introduced the behavioral sweep after repeated hand inventories
// missed nested-path decisions. RFC-232 then sealed FieldValue/FieldPath behind
// exact read views and removed most of that public-struct surface; this file now
// records the smaller post-migration population.
//
// Classes:
//   (a) correct decline: this consumer genuinely requires one top-level slot;
//   (b) blocker: rejects a legitimate nested grouping key;
//   (c) nesting-safe: preserves, traverses, or compares the complete path;
//   (d) measured live defect;
//   (?) not yet established.
//
// Sites are keyed by file#symbol rather than line. The raw grep-line population
// is pinned separately from the AST expression population because generated
// wire fields and explanatory comments also spell Accessors. This remains a
// behavioral sweep, not a proof that no caller can discard a path without
// checking its length; the field-name decision gate covers that complementary
// risk.

// accessorArityClass is the classification of one arity site.
type accessorArityClass string

const (
	arityCorrectDecline accessorArityClass = "a" // legitimately refuses a multi-accessor value
	arityBlocker        accessorArityClass = "b" // must change for nested grouping keys to work
	arityNestingOK      accessorArityClass = "c" // already handles multi-accessor paths
	arityLiveDefect     accessorArityClass = "d" // mis-handles one today
	arityUncertain      accessorArityClass = "?" // honestly unresolved — reason recorded
)

type accessorAritySite struct {
	class accessorArityClass
	// exprs is how many `len(...Accessors)` expressions this symbol contains.
	// Pinned so an arity test ADDED to an already-classified function still
	// fails the census rather than hiding behind its neighbour's verdict.
	exprs int
	why   string
}

// accessorAritySites is the post-RFC-232 classified population, keyed
// `file#symbol`. The earlier public-struct inventory is intentionally gone:
// private exact views moved the arity decisions to this smaller set.
var accessorAritySites = map[string]accessorAritySite{
	// (a) Correct declines: these consumers require one top-level source slot.
	"pkg/recordlayer/query/plan/cascades/values/values.go#fieldPath.OrdinalIn": {
		class: arityCorrectDecline, exprs: 1,
		why: "OrdinalIn answers which slot of one frontier a value reads; nested suffix ordinals index nested records, not that frontier.",
	},
	"pkg/recordlayer/query/plan/cascades/values/values.go#fieldPath.Single": {
		class: arityCorrectDecline, exprs: 1,
		why: "Single is the explicit one-accessor recognizer; callers needing a path use the arity-tolerant path view.",
	},
	"pkg/recordlayer/query/plan/cascades/values/values.go#fieldValue.SourceRelativeBaked": {
		class: arityCorrectDecline, exprs: 1,
		why: "the retired compatibility predicate deliberately recognizes only an unpinned direct source field; no production caller remains.",
	},
	"pkg/recordlayer/query/plan/cascades/values/values.go#CanBridgeOrderingFieldValues": {
		class: arityCorrectDecline, exprs: 2,
		why: "the legacy baked/lazy ordering bridge can justify one direct source slot only; declining a nested path preserves ordering soundness.",
	},
	"pkg/recordlayer/query/plan/cascades/values/column_identity.go#OrderingIdentityOf": {
		class: arityCorrectDecline, exprs: 1,
		why: "one ordering identity is one correlation/domain/root ordinal; a nested chain cannot be collapsed to that triple.",
	},
	"pkg/recordlayer/query/plan/cascades/values/ordinal_join_seed.go#AssertOrdinalJoinSeed": {
		class: arityCorrectDecline, exprs: 1,
		why: "the physical join seed consists of direct, frontier-pinned source slots; a fused path is a malformed seed and fails loudly.",
	},
	"pkg/recordlayer/query/plan/cascades/values/ordinal_output_layout.go#NewFlatOrdinalLayoutForRetainedResultWithSources": {
		class: arityCorrectDecline, exprs: 1,
		why: "direct retained-source discovery requires each top-level source field exactly once; nested members cannot retain their root, while separately producer-proven ObjectPaths are admitted and revalidated through the additional-source channel.",
	},
	"pkg/recordlayer/query/plan/cascades/values/ordinal_output_layout.go#NewFlatOrdinalLayoutForResult": {
		class: arityCorrectDecline, exprs: 1,
		why: "a declared flat source window maps direct top-level source fields; nested members cannot stand in for a complete retained source.",
	},

	// (c) Arity-tolerant exact machinery: these sites preserve, traverse, copy,
	// compare, or expose the complete accessor vector.
	"pkg/recordlayer/query/plan/cascades/values/field_value.go#fieldPath.Len": {
		class: arityNestingOK, exprs: 1,
		why: "the read-only view reports the complete path length, including zero for nil.",
	},
	"pkg/recordlayer/query/plan/cascades/values/field_value.go#fieldPath.Ordinals": {
		class: arityNestingOK, exprs: 1,
		why: "copies every accessor ordinal into a defensive slice.",
	},
	"pkg/recordlayer/query/plan/cascades/values/field_value.go#fieldPath.Accessor": {
		class: arityNestingOK, exprs: 1,
		why: "the length check is an index bound; any accessor in a nested path remains addressable.",
	},
	"pkg/recordlayer/query/plan/cascades/values/field_value.go#isAdmittedFieldValue": {
		class: arityNestingOK, exprs: 1,
		why: "admission requires a non-empty complete exact path, then validates every accessor without imposing a maximum arity.",
	},
	"pkg/recordlayer/query/plan/cascades/values/field_value.go#resolveFieldAccess": {
		class: arityNestingOK, exprs: 1,
		why: "fusing onto an admitted FieldValue copies its complete existing prefix before resolving the requested suffix.",
	},
	"pkg/recordlayer/query/plan/cascades/values/field_value.go#RebuildFieldValue": {
		class: arityNestingOK, exprs: 1,
		why: "rebuild converts every original accessor to an ordinal request and resolves the complete vector atomically.",
	},
	"pkg/recordlayer/query/plan/cascades/values/field_value.go#rebuildFieldValueOnChangedChild": {
		class: arityNestingOK, exprs: 1,
		why: "translation-map rebuild copies every outer accessor and preserves the changed child's exact fused path.",
	},
	"pkg/recordlayer/query/plan/cascades/values/field_value_reanchor.go#reanchorValueThroughProducer": {
		class: arityNestingOK, exprs: 1,
		why: "the shared ordinary/owned implementation compares candidate and requested prefix lengths, verifies the complete prefix, and appends the arbitrary remaining suffix into the full mapped path; the two-level and four-leg producer-lineage mutation tests pin both depths.",
	},
	"pkg/recordlayer/query/plan/cascades/values/values.go#fieldPath.RootOrdinalIn": {
		class: arityNestingOK, exprs: 1,
		why: "guards only emptiness and deliberately answers the root ordinal of paths at any depth.",
	},
	"pkg/recordlayer/query/plan/cascades/values/values.go#fieldPath.WithSuffix": {
		class: arityNestingOK, exprs: 3,
		why: "handles an empty suffix defensively and otherwise concatenates both complete accessor lists.",
	},
	"pkg/recordlayer/query/plan/cascades/values/values.go#fieldPath.Last": {
		class: arityNestingOK, exprs: 1,
		why: "indexes the final accessor; nested paths are the reason this accessor exists.",
	},
	"pkg/recordlayer/query/plan/cascades/values/values.go#fieldPath.Equals": {
		class: arityNestingOK, exprs: 2,
		why: "checks equal lengths and then compares the complete accessor vectors element by element.",
	},
	"pkg/recordlayer/query/plan/cascades/values/values.go#fieldValue.descendResolvedPath": {
		class: arityNestingOK, exprs: 1,
		why: "the one-step check is the fast path; deeper paths are traversed from the second accessor onward.",
	},
	"pkg/recordlayer/query/plan/cascades/values/values.go#NestedResolvedPath": {
		class: arityNestingOK, exprs: 1,
		why: "this is the nesting predicate itself and selects paths with more than one accessor.",
	},
	"pkg/recordlayer/query/plan/cascades/values/values.go#ProjectionOutputIdentityKey": {
		class: arityNestingOK, exprs: 1,
		why: "renders every path component into the compatibility identity key rather than dropping a suffix.",
	},
	"pkg/recordlayer/query/plan/cascades/values/values.go#explainValueOrdinalsWithAliases": {
		class: arityNestingOK, exprs: 2,
		why: "renders all accessor steps; the arity checks choose the single-step presentation and allocate the full multi-step presentation.",
	},
	"pkg/recordlayer/query/plan/cascades/values/column_identity.go#statesColumnPath": {
		class: arityNestingOK, exprs: 1,
		why: "rejects only an empty path and validates every ordinal in a path of arbitrary depth.",
	},
	"pkg/recordlayer/query/plan/cascades/values/column_identity.go#SameColumnPath": {
		class: arityNestingOK, exprs: 3,
		why: "requires equal non-empty lengths and compares every accessor ordinal.",
	},
	"pkg/recordlayer/query/plan/cascades/values/replace.go#fieldValueLegOrdinal": {
		class: arityNestingOK, exprs: 1,
		why: "reads the root ordinal deliberately; its caller rebases that root and retains the complete suffix.",
	},
	"pkg/recordlayer/query/plan/cascades/values/values.go#DisplayColumnName": {
		class: arityNestingOK, exprs: 2,
		why: "takes the LAST accessor of a path of any depth as the display leaf; it refuses only an EMPTY path, and the leaf is read from the resolved accessors rather than by splitting a rendered name, so a column legally named with a dot in it is not torn in half.",
	},
}

// Population facts measured after RFC-232 made the public FieldValue/FieldPath
// representation private and introduced sealed exact views. These are exact
// census pins, not budgets; a move requires reclassification and an RFC-230
// current-state note.
//
// THE +2 OVER RFC-230 rev 6 §7.3's PUBLISHED 45 IS DisplayColumnName, and it is
// classified rather than absorbed. The projection label authority stopped
// splitting a rendered name at its last dot and now reads the LEAF out of the
// resolved accessors — which needs the path's length twice, once to reject an
// empty path and once to index its last step. Both expressions are class (c):
// the site traverses a path of any depth. A name-splitting label authority is
// what the ordinal model retires, so the population growing here is the
// conversion arriving, not new debt.
const (
	rfcPublishedPopulation = 47
	arityGeneratedLines    = 8  // protobuf loops over PFieldPath.FieldAccessors
	arityCommentLines      = 7  // historical SourceRelativeBaked legibility prose
	arityCodeLines         = 32 // the live production population
	arityExpressions       = 36 // four code lines contain multiple expressions
)

// accessorArityLine is the RFC's own sweep, as a regexp.
var accessorArityLine = regexp.MustCompile(`len\(.*Accessors\)`)

// TestAccessorArityCensusIsClassified pins the classified population. It fails
// when a new arity site appears, when one disappears, or when an already
// classified symbol gains or loses an arity expression.
func TestAccessorArityCensusIsClassified(t *testing.T) {
	t.Parallel()

	root := sourceTreeRoot(t)

	var (
		rawLines, genLines, commentLines, codeLines int
		scanned                                     int
		liveExprs                                   int
	)
	live := map[string]int{}

	for _, rel := range trackedGoFiles(t, root) {
		rel = filepath.ToSlash(rel)
		if strings.HasSuffix(rel, "_test.go") {
			continue
		}
		src, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		scanned++
		if !bytes.Contains(src, []byte("Accessors")) {
			continue
		}

		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, rel, src, parser.ParseComments)
		if err != nil {
			t.Fatalf("parse %s: %v", rel, err)
		}
		generated := ast.IsGenerated(f)

		// Line arm: reproduce the RFC's grep exactly, then classify each hit as
		// generated / comment-only / code so the published 52 stays checkable
		// AND its decomposition stays honest.
		commentLine := map[int]bool{}
		for _, group := range f.Comments {
			for _, c := range group.List {
				lo := fset.Position(c.Pos()).Line
				hi := fset.Position(c.End()).Line
				for ln := lo; ln <= hi; ln++ {
					commentLine[ln] = true
				}
			}
		}
		exprLine := map[int]bool{}
		symbols := funcSpansOf(fset, f)
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			ident, ok := call.Fun.(*ast.Ident)
			if !ok || ident.Name != "len" || len(call.Args) != 1 {
				return true
			}
			var buf bytes.Buffer
			if err := printer.Fprint(&buf, fset, call.Args[0]); err != nil {
				t.Fatalf("render len() argument in %s: %v", rel, err)
			}
			if !strings.HasSuffix(buf.String(), "Accessors") {
				return true
			}
			exprLine[fset.Position(call.Pos()).Line] = true
			if generated {
				return true
			}
			liveExprs++
			live[rel+"#"+enclosingSymbol(symbols, call.Pos())]++
			return true
		})

		for i, line := range strings.Split(string(src), "\n") {
			if !accessorArityLine.MatchString(line) {
				continue
			}
			rawLines++
			switch {
			case generated:
				genLines++
			case exprLine[i+1]:
				codeLines++
			case commentLine[i+1]:
				commentLines++
			default:
				t.Errorf("%s:%d matches the arity sweep but is neither generated, an "+
					"expression, nor a comment — the census decomposition does not cover "+
					"it:\n\t%s", rel, i+1, strings.TrimSpace(line))
			}
		}
	}

	// ---- Anti-vacuity. A green from an empty set is the dominant false
	// positive here: an empty scan classifies nothing and reports success.
	if scanned < 100 {
		t.Fatalf("the census scanned only %d production Go files under %s — that is not the "+
			"real source tree. Fix sourceTreeRoot/runfiles staging; do NOT trust this run",
			scanned, root)
	}
	if liveExprs == 0 || len(live) == 0 {
		t.Fatalf("the census found ZERO arity expressions across %d files. The sweep is "+
			"broken, not the tree — an empty population cannot classify anything", scanned)
	}

	// ---- The RFC's published number, and its decomposition.
	if rawLines != rfcPublishedPopulation {
		t.Errorf("the arity sweep now matches %d non-test lines, RFC-230 rev 6 §7.3 published "+
			"%d.\n\tRe-run: git grep -n \"len(.*Accessors)\" -- '*.go' | grep -v _test.go\n"+
			"\tA changed population is a finding, not a nit: classify what moved and update "+
			"this census and the RFC together.", rawLines, rfcPublishedPopulation)
	}
	for _, c := range []struct {
		name        string
		got, pinned int
	}{
		{"generated (protobuf marshal loops, not gates)", genLines, arityGeneratedLines},
		{"comment-only (prose, not a gate)", commentLines, arityCommentLines},
		{"code lines (the real population)", codeLines, arityCodeLines},
		{"arity expressions across those code lines", liveExprs, arityExpressions},
	} {
		if c.got != c.pinned {
			t.Errorf("arity population decomposition drifted — %s: got %d, pinned %d",
				c.name, c.got, c.pinned)
		}
	}

	// ---- The classification itself.
	var unclassified, stale []string
	for key, n := range live {
		site, known := accessorAritySites[key]
		if !known {
			unclassified = append(unclassified, fmt.Sprintf("%s (%d expression(s))", key, n))
			continue
		}
		if site.exprs != n {
			t.Errorf("%s now holds %d arity expression(s), the census pinned %d.\n"+
				"\tRe-read the symbol: an added arity test in an already-classified function "+
				"does NOT inherit its neighbour's verdict. Classify the new expression, then "+
				"update `exprs`.", key, n, site.exprs)
		}
	}
	for key := range accessorAritySites {
		if _, still := live[key]; !still {
			stale = append(stale, key)
		}
	}
	sort.Strings(unclassified)
	sort.Strings(stale)

	if len(unclassified) > 0 {
		t.Errorf("%d arity site(s) are NOT classified:\n\t%s\n\n"+
			"WHAT TO DO: read the site AND its callers — not the shape of its condition; two "+
			"sites with identical `len(Accessors) != 1` checks classify differently. Decide "+
			"which it is:\n"+
			"\t(a) a correct decline  — the value genuinely cannot be what the site needs\n"+
			"\t(b) a blocker          — it would refuse a LEGITIMATE nested grouping key\n"+
			"\t(c) already correct    — it handles multi-accessor paths\n"+
			"\t(d) a live defect      — it mis-handles one TODAY; stop and fix that first\n"+
			"\t(?) uncertain          — record WHY; an honest unknown beats a wrong verdict\n"+
			"Then add it to accessorAritySites with its reason. Leaving it out is how this "+
			"census was wrong five times.",
			len(unclassified), strings.Join(unclassified, "\n\t"))
	}
	if len(stale) > 0 {
		t.Errorf("%d classified arity site(s) no longer exist:\n\t%s\n\n"+
			"A site that vanished was either fixed or renamed. If it was a (b) blocker that "+
			"RFC-230 resolved, delete the entry and say so in the RFC; if it merely moved, "+
			"re-key it. Do not leave a verdict pointing at nothing.",
			len(stale), strings.Join(stale, "\n\t"))
	}
}

// TestAccessorArityClassCounts pins the per-class totals. The site table above
// could drift class-by-class while staying the same size; these counts are the
// summary RFC-230 quotes, so they are asserted rather than recomputed by a
// reader.
func TestAccessorArityClassCounts(t *testing.T) {
	t.Parallel()

	// RFC-232's exact views leave some legitimate single-slot declines and some
	// arity-tolerant path operations; the `pinned` map below is where those
	// counts are stated and asserted. Nested-path GROUP BY is shipped, so
	// blocker/live-defect/uncertain are zero-ratchet classes.
	//
	// values.DisplayColumnName is one of the arity-tolerant sites, added when
	// the projection label authority stopped splitting a rendered name at its
	// last dot and started reading the leaf out of the resolved accessors. The
	// DECLINE count did not move with it, which is the reading that matters:
	// the conversion added an arity-TOLERANT site, not another single-slot
	// requirement.
	pinned := map[accessorArityClass]int{
		arityCorrectDecline: 8,
		arityBlocker:        0,
		arityNestingOK:      20,
		arityLiveDefect:     0,
		arityUncertain:      0,
	}
	got := map[accessorArityClass]int{}
	for key, site := range accessorAritySites {
		switch site.class {
		case arityCorrectDecline, arityBlocker, arityNestingOK, arityLiveDefect, arityUncertain:
		default:
			t.Errorf("%s carries unknown class %q", key, site.class)
		}
		if strings.TrimSpace(site.why) == "" {
			t.Errorf("%s is classified %q with no reason", key, site.class)
		}
		got[site.class]++
	}
	total := 0
	for class, want := range pinned {
		if got[class] != want {
			t.Errorf("class (%s): %d sites, pinned %d", class, got[class], want)
		}
		total += want
	}
	if total != len(accessorAritySites) {
		t.Errorf("class counts sum to %d but the site table holds %d entries", total, len(accessorAritySites))
	}
	if got[arityBlocker]+got[arityLiveDefect]+got[arityUncertain] != 0 {
		t.Errorf("post-RFC-232 arity census contains blocker/live-defect/uncertain sites: b=%d d=%d ?=%d",
			got[arityBlocker], got[arityLiveDefect], got[arityUncertain])
	}
}

// funcSpan is one top-level function's name and source extent.
type funcSpan struct {
	name   string
	lo, hi token.Pos
}

// funcSpansOf lists every top-level func in f, methods rendered `Recv.Name`.
func funcSpansOf(fset *token.FileSet, f *ast.File) []funcSpan {
	var spans []funcSpan
	for _, decl := range f.Decls {
		fd, ok := decl.(*ast.FuncDecl)
		if !ok {
			continue
		}
		name := fd.Name.Name
		if fd.Recv != nil && len(fd.Recv.List) > 0 {
			var buf bytes.Buffer
			if printer.Fprint(&buf, fset, fd.Recv.List[0].Type) == nil {
				name = strings.TrimPrefix(buf.String(), "*") + "." + name
			}
		}
		spans = append(spans, funcSpan{name: name, lo: fd.Pos(), hi: fd.End()})
	}
	return spans
}

// enclosingSymbol names the innermost listed func containing pos. A site at
// file scope (a package-level var initializer) is keyed "<file scope>" rather
// than dropped — an unnamed site still has to be classified.
func enclosingSymbol(spans []funcSpan, pos token.Pos) string {
	name := "<file scope>"
	for _, s := range spans {
		if pos >= s.lo && pos < s.hi {
			name = s.name
		}
	}
	return name
}
