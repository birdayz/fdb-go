package docscheck

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// Every index maintainer must have its DeleteRecordsWhere bound driven by a row
// in pkg/recordlayer's CanDeleteWhere table, and THIS is what establishes that
// the table covers the population — not a count inside the table itself.
//
// The distinction is the whole point of this file. The table originally guarded
// itself with `len(cases) != 14`, which cannot detect the direction that
// matters: adding a maintainer without adding a row leaves len(cases) at 14 and
// the guard green, while its failure message claimed to catch exactly that. A
// set derived from the table can only ever describe the table. So the
// population is derived HERE, independently, from the source tree.
//
// The Go compiler already forces every maintainer to HAVE a CanDeleteWhere (it
// is part of IndexMaintainer). What it cannot force is that the answer was
// thought about rather than inherited by accident, which is what a row
// asserting an explicit bound, at and one past it, is for.
const (
	// maintainerDir is where the maintainers live.
	maintainerDir = "pkg/recordlayer"
	// boundTableRel is the table under audit.
	boundTableRel = "pkg/recordlayer/index_maintainer_delete_where_bound_test.go"
	// deleteWhereMethod is the method whose presence — declared OR promoted —
	// puts a type on the DeleteRecordsWhere path.
	deleteWhereMethod = "DeleteWhere"
	// maintainerFloor guards the INSTRUMENT rather than the census: if the walk
	// or the parse silently stops finding maintainers, the set difference goes
	// empty and the gate passes while checking nothing. 13 implementations exist
	// at the time of writing; the floor sits below that with room for churn.
	maintainerFloor = 8
)

// boundShapeSentinels are index NAMES that must still appear in the table.
//
// Type coverage is not row coverage. `covered` below is a set of receiver
// names, so a type stays covered while a load-bearing ROW for it is deleted:
// dropping the wrapped-MULTIDIMENSIONAL row leaves MULTIDIMENSIONAL covered by
// the unwrapped one, and dropping either VECTOR shape leaves VECTOR covered by
// the other. Those are precisely the rows that fail when a maintainer's own
// override is lost, because they are the shapes where the override and the
// inherited bound DISAGREE — the sibling rows cannot detect it, since there the
// two bounds coincide.
//
// So each shape whose disagreement is the point is named here, independently of
// the table, and each entry says which override it holds down.
var boundShapeSentinels = map[string]string{
	"md_wrapped": "MULTIDIMENSIONAL wrapped in KeyWithValue: the generic bound answers with " +
		"the split point while the R-trees are rooted at PrefixSize",
	"vec_plain": "VECTOR on a non-KeyWithValue root: the generic bound answers with the " +
		"root's width while no non-empty prefix names a graph",
	"kwv": "a KeyWithValue root: the bound is the split point, not the inner key's width",
	"perm": "PERMUTED: the bound is the grouping count MINUS the permuted columns, which " +
		"no other row exercises",
	"spf": "SPFRESH: the only maintainer whose bound is zero, so the only row proving a " +
		"whole-index-clear-only answer is expressible",
}

// missingShapeSentinels returns the sentinels absent from the audited file,
// each with the reason it exists.
//
// Factored out so the MISSING branch can be driven by a test. Left inline it
// was reachable only through the corpus, where every sentinel is present by
// construction — so the branch that reports, and the message it reports, ran in
// exactly one state: the passing one.
func missingShapeSentinels(literals map[string]bool) []string {
	var lost []string
	for name, why := range boundShapeSentinels {
		if !literals[name] {
			lost = append(lost, name+" — "+why)
		}
	}
	sort.Strings(lost)
	return lost
}

// maintainerSource is one parsed non-test source file under maintainerDir.
type maintainerSource struct {
	rel string
	ast *ast.File
}

// collectDeleteWhereMaintainers returns every type reachable by a DeleteWhere
// method, mapped to the file it is declared in.
//
// Membership follows EMBEDDING (and type ALIASES) as well as declaration, and
// that is not defensive generality. DeleteWhere is promoted from
// standardIndexMaintainer, so a new maintainer can satisfy IndexMaintainer —
// and reach the DeleteRecordsWhere path — without declaring the method at all.
// It would then carry an ACCIDENTALLY INHERITED bound, which is the exact
// regression this census exists to catch, while a declaration-only scan omitted
// it from the population and reported green.
//
// What this is NOT is a Go method set, and the difference is worth stating
// because the two agree on every shape in the tree today:
//
//   - A named field that SHADOWS the promoted method (a field literally called
//     DeleteWhere) leaves the type in this population though its method set has
//     no such method. That direction over-includes: the census demands a bound
//     row for a type that needs none, which fails LOUDLY and is corrected by
//     whoever reads the failure. The opposite direction is the dangerous one,
//     and it is the one closed here.
//   - Embedding across PACKAGES is not modelled. Every maintainer lives in
//     pkg/recordlayer and the base is unexported, so a cross-package embedder
//     cannot promote it; if that ever changes, this becomes a real gap.
//
// Resolving genuine method sets means type-checking the package, which needs an
// importer able to resolve every dependency — not available to a sandboxed
// source scan. The AST approximation is chosen deliberately, with the
// over-inclusive direction preferred over the silent one.
func collectDeleteWhereMaintainers(sources []maintainerSource) map[string]string {
	population := map[string]string{}
	embeds := map[string][]string{}
	declaredIn := map[string]string{}

	for _, src := range sources {
		for _, decl := range src.ast.Decls {
			switch d := decl.(type) {
			case *ast.FuncDecl:
				if d.Recv == nil || d.Name.Name != deleteWhereMethod || len(d.Recv.List) == 0 {
					continue
				}
				if name := receiverTypeName(d.Recv.List[0].Type); name != "" {
					population[name] = src.rel
				}
			case *ast.GenDecl:
				if d.Tok != token.TYPE {
					continue
				}
				for _, spec := range d.Specs {
					ts, ok := spec.(*ast.TypeSpec)
					if !ok {
						continue
					}
					declaredIn[ts.Name.Name] = src.rel
					// A type ALIAS carries the target's whole method set, so
					// embedding the alias promotes the target's DeleteWhere just
					// as embedding the target does. Recorded as an edge because
					// nothing else would connect the two: the alias declares no
					// methods of its own, so a scan looking only for
					// declarations sees an inert name and drops every type that
					// reaches the method through it.
					if ts.Assign.IsValid() {
						if target := baseTypeName(ts.Type); target != "" {
							embeds[ts.Name.Name] = append(embeds[ts.Name.Name], target)
						}
					}
				}
			}
		}
		scanEmbeddedFields(src.ast, func(outer, embedded string) {
			embeds[outer] = append(embeds[outer], embedded)
		})
	}

	// Fixpoint over embedding: promotion is transitive, so a type embedding a
	// type that embeds the base has the method just as much as the base does.
	for changed := true; changed; {
		changed = false
		for outer, inner := range embeds {
			if _, already := population[outer]; already {
				continue
			}
			for _, e := range inner {
				if _, ok := population[e]; ok {
					population[outer] = declaredIn[outer]
					changed = true
					break
				}
			}
		}
	}
	return population
}

func TestEveryIndexMaintainerHasADeleteWhereBoundRow(t *testing.T) {
	t.Parallel()

	root := sourceTreeRoot(t)
	files := trackedGoFiles(t, root)

	var sources []maintainerSource
	var tableSrc []byte
	for _, rel := range files {
		if !strings.HasPrefix(rel, maintainerDir+"/") || !strings.HasSuffix(rel, ".go") {
			continue
		}
		src, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		if rel == boundTableRel {
			tableSrc = src
			continue
		}
		if strings.HasSuffix(rel, "_test.go") {
			continue
		}
		fset := token.NewFileSet()
		f, perr := parser.ParseFile(fset, rel, src, 0)
		if perr != nil {
			t.Fatalf("parse %s: %v", rel, perr)
		}
		sources = append(sources, maintainerSource{rel: rel, ast: f})
	}

	if tableSrc == nil {
		t.Fatalf("%s not found in the source tree — the table this gate audits is gone, "+
			"which would otherwise read as every maintainer being covered", boundTableRel)
	}
	population := collectDeleteWhereMaintainers(sources)
	if len(population) < maintainerFloor {
		t.Fatalf("found only %d types with a %s method in %s (floor %d) — the scan is not "+
			"seeing the maintainers, so the coverage check below is vacuous",
			len(population), deleteWhereMethod, maintainerDir, maintainerFloor)
	}

	fset := token.NewFileSet()
	tableAST, err := parser.ParseFile(fset, boundTableRel, tableSrc, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", boundTableRel, err)
	}

	// The covered set: every population type the table CONSTRUCTS. A composite
	// literal naming the type is the evidence, because a row cannot drive a
	// maintainer's bound without building one. The string literals come along
	// in the same walk — that is where the shape sentinels live.
	covered := map[string]bool{}
	literals := map[string]bool{}
	ast.Inspect(tableAST, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.CompositeLit:
			if id, ok := node.Type.(*ast.Ident); ok {
				if _, inPopulation := population[id.Name]; inPopulation {
					covered[id.Name] = true
				}
			}
		case *ast.BasicLit:
			if node.Kind == token.STRING {
				if s, uerr := strconv.Unquote(node.Value); uerr == nil {
					literals[s] = true
				}
			}
		}
		return true
	})

	var missing []string
	for name, rel := range population {
		if !covered[name] {
			missing = append(missing, name+" ("+rel+")")
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("%d index maintainer(s) reach %s but are never constructed in %s, so no test "+
			"states what their DeleteRecordsWhere prefix bound is:\n  %s\n"+
			"A maintainer inherits standardIndexMaintainer's bound silently when it does not "+
			"override — which is how RANK, the aggregates and TEXT each shipped clearable past "+
			"their secondary structures. Add a row driving the bound AT its limit and ONE PAST it.",
			len(missing), deleteWhereMethod, boundTableRel, strings.Join(missing, "\n  "))
	}

	if lostShapes := missingShapeSentinels(literals); len(lostShapes) > 0 {
		t.Errorf("%d load-bearing bound shape(s) no longer appear in %s:\n  %s\n"+
			"Type coverage is not row coverage: a sibling row keeps the TYPE covered while the "+
			"shape that actually discriminates the override from the inherited bound is gone. "+
			"Restore the row, or remove the sentinel in the same change that removes the "+
			"override it holds down.",
			len(lostShapes), boundTableRel, strings.Join(lostShapes, "\n  "))
	}
}

// TestDeleteWhereCensusDetectorArms drives the population collector directly.
// The corpus run above exercises only the shapes pkg/recordlayer happens to
// contain today — every maintainer there DECLARES DeleteWhere — so the promoted
// and transitively-promoted arms, which are the ones a future maintainer is
// most likely to take, would never run.
func TestDeleteWhereCensusDetectorArms(t *testing.T) {
	t.Parallel()

	const src = `package recordlayer

type standardIndexMaintainer struct{ index int }

func (m *standardIndexMaintainer) DeleteWhere(prefix int) error { return nil }

// Declares its own.
type textIndexMaintainer struct{ index int }

func (m *textIndexMaintainer) DeleteWhere(prefix int) error { return nil }

// PROMOTED: embeds the base and declares nothing.
type inheritingIndexMaintainer struct {
	standardIndexMaintainer
}

// PROMOTED TRANSITIVELY: embeds a type that embeds the base, by pointer.
type nestedIndexMaintainer struct {
	*inheritingIndexMaintainer
}

// PROMOTED THROUGH AN ALIAS: the alias declares no methods of its own, so
// nothing but an alias edge connects the embedder to the method.
type aliasedBase = standardIndexMaintainer

type aliasEmbeddingIndexMaintainer struct {
	aliasedBase
}

// Not a maintainer: holds one in a NAMED field, which promotes nothing.
type notAMaintainer struct {
	delegate standardIndexMaintainer
}
`
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "synthetic.go", src, 0)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	got := collectDeleteWhereMaintainers([]maintainerSource{{rel: "synthetic.go", ast: f}})

	for _, want := range []string{
		"standardIndexMaintainer",
		"textIndexMaintainer",
		"inheritingIndexMaintainer",
		"nestedIndexMaintainer",
		"aliasEmbeddingIndexMaintainer",
	} {
		if _, ok := got[want]; !ok {
			t.Errorf("%s reaches DeleteWhere but is missing from the population — a maintainer "+
				"with an accidentally inherited bound would pass the census unseen", want)
		}
	}
	if _, ok := got["notAMaintainer"]; ok {
		t.Error("a NAMED field promotes nothing; including its holder would demand a bound row " +
			"for a type that has no DeleteWhere")
	}
	// The alias itself carries the method set, so it is in the population too;
	// 6 = the 5 asserted above plus aliasedBase.
	if len(got) != 6 {
		t.Errorf("expected exactly the 6 reaching names, got %d: %v", len(got), sortedMaintainerNames(got))
	}
}

// TestShapeSentinelDetectorArms drives the sentinel decision in both states.
// The corpus can only ever supply the all-present one, so without this the
// reporting branch — and the guidance in its message — never runs until the day
// it fires for real, which is the most expensive moment to discover it is
// wrong.
func TestShapeSentinelDetectorArms(t *testing.T) {
	t.Parallel()

	if len(boundShapeSentinels) == 0 {
		t.Fatal("no shape sentinels are declared, so both arms below are vacuous")
	}

	all := map[string]bool{}
	for name := range boundShapeSentinels {
		all[name] = true
	}
	if lost := missingShapeSentinels(all); len(lost) != 0 {
		t.Errorf("every sentinel present must report none missing, got: %v", lost)
	}

	// One removed: reported, and reported WITH its reason, since the message is
	// the only place a reader learns which override the row holds down.
	for name, why := range boundShapeSentinels {
		partial := map[string]bool{}
		for n := range all {
			if n != name {
				partial[n] = true
			}
		}
		lost := missingShapeSentinels(partial)
		if len(lost) != 1 {
			t.Errorf("removing sentinel %q must report exactly it, got %d: %v", name, len(lost), lost)
			continue
		}
		if !strings.Contains(lost[0], name) || !strings.Contains(lost[0], why) {
			t.Errorf("the report for %q must name the shape AND why it exists, got: %q", name, lost[0])
		}
	}

	// And none present at all — the state a wholesale rewrite of the table
	// produces, where every sentinel must be named rather than the first.
	if lost := missingShapeSentinels(map[string]bool{}); len(lost) != len(boundShapeSentinels) {
		t.Errorf("an empty table must report all %d sentinels, got %d: %v",
			len(boundShapeSentinels), len(lost), lost)
	}
}

func sortedMaintainerNames(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
