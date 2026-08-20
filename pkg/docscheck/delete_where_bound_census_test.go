package docscheck

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
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
// population is derived HERE, independently, from the source tree — the
// receivers that declare a DeleteWhere method — and compared against the types
// the table actually constructs.
//
// The Go compiler already forces every maintainer to HAVE a CanDeleteWhere
// (it is part of IndexMaintainer). What it cannot force is that the answer was
// thought about rather than inherited by accident, which is what a row asserting
// an explicit bound, at and one past it, is for.
const (
	// maintainerDir is where the maintainers live.
	maintainerDir = "pkg/recordlayer"
	// boundTableRel is the table under audit.
	boundTableRel = "pkg/recordlayer/index_maintainer_delete_where_bound_test.go"
	// maintainerFloor guards the INSTRUMENT rather than the census: if the walk
	// or the parse silently stops finding maintainers, the set difference below
	// goes empty and the gate passes while checking nothing. 13 implementations
	// exist at the time of writing; the floor sits below that with room for
	// ordinary churn.
	maintainerFloor = 8
)

func TestEveryIndexMaintainerHasADeleteWhereBoundRow(t *testing.T) {
	t.Parallel()

	root := sourceTreeRoot(t)
	files := trackedGoFiles(t, root)

	// The population: every type declaring a DeleteWhere method in
	// pkg/recordlayer's non-test sources. That method IS the DeleteRecordsWhere
	// entry point, so declaring it is what puts a type in scope for a bound.
	population := map[string]string{} // type -> file it is declared in
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
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv == nil || fn.Name.Name != "DeleteWhere" {
				continue
			}
			if len(fn.Recv.List) == 0 {
				continue
			}
			if name := receiverTypeName(fn.Recv.List[0].Type); name != "" {
				population[name] = rel
			}
		}
	}

	if tableSrc == nil {
		t.Fatalf("%s not found in the source tree — the table this gate audits is gone, "+
			"which would otherwise read as every maintainer being covered", boundTableRel)
	}
	if len(population) < maintainerFloor {
		t.Fatalf("found only %d types declaring DeleteWhere in %s (floor %d) — the scan is "+
			"not seeing the maintainers, so the coverage check below is vacuous",
			len(population), maintainerDir, maintainerFloor)
	}

	// The covered set: every population type the table CONSTRUCTS. A composite
	// literal naming the type is the evidence, because a row cannot drive a
	// maintainer's bound without building one.
	fset := token.NewFileSet()
	tableAST, err := parser.ParseFile(fset, boundTableRel, tableSrc, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", boundTableRel, err)
	}
	covered := map[string]bool{}
	ast.Inspect(tableAST, func(n ast.Node) bool {
		lit, ok := n.(*ast.CompositeLit)
		if !ok {
			return true
		}
		id, ok := lit.Type.(*ast.Ident)
		if !ok {
			return true
		}
		if _, inPopulation := population[id.Name]; inPopulation {
			covered[id.Name] = true
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
		t.Fatalf("%d index maintainer(s) declare DeleteWhere but are never constructed in %s, "+
			"so no test states what their DeleteRecordsWhere prefix bound is:\n  %s\n"+
			"A maintainer inherits standardIndexMaintainer's bound silently when it does not "+
			"override — which is how RANK, the aggregates and TEXT each shipped clearable past "+
			"their secondary structures. Add a row driving the bound AT its limit and ONE PAST it.",
			len(missing), boundTableRel, strings.Join(missing, "\n  "))
	}
}
