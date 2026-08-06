package docscheck

// A MULTI-SLOT row may not be asserted through its name-keyed map.
//
// `executor.RowValue` projects a row to `map[string]any`, and CQ-64's whole
// finding is that the projection is blind in the dimension the sqldriver row
// tests exist to police. Permute a row's (Fields, Slots) TOGETHER — which is
// what a mis-bound leg window IS — and every name still maps to its own value,
// so the projection is byte-identical while every slot moved. On top of that,
// `positionalToMap` collapses duplicate output names LAST-WINS before anything
// downstream runs, so a row of width n becomes fewer than n entries whenever two
// output columns share a name, which is legal and routine on a merged or
// unnested row. The order loss makes a permutation invisible; the collapse
// discards a value outright.
//
// CQ-64 converted 573 expectations across 18 files off that projection and onto
// positional renderers. WHY THIS GATE AND NOT THE CONVERSION ALONE: the
// conversion's own closing evidence was a grep. A grep cannot tell a row being
// ASSERTED through the map from a single named column being read out of it, so
// it either reports the legal population as debt or is written loosely enough to
// miss a new offender. Either way the class regrows quietly, exactly as the
// domain-less pinned-ordinal constructor did — migrated to zero callers, then
// left with nothing objecting to a new one.
//
// WHAT STAYS LEGAL, and it is the more important half. A SINGLE named column
// read out of the map has no order to lose and cannot collide with itself:
// `row["ID"].(int64)` is exactly as correct as a positional read and far more
// readable at a site that only wants the id. MEASURED at the time this gate
// landed: 13 files hold such reads and every one of them is legal here. A gate
// that also condemned those would leave no reasonable way to read one column,
// and a gate with no legal alternative is one someone deletes.
//
// So the gate distinguishes by CONSUMPTION, not by the call:
//
//   - one string-literal key indexed out of the map — LEGAL
//   - two or more distinct keys — a multi-slot read, FLAGGED
//   - the map consumed WHOLE (ranged over, printed, compared) — FLAGGED,
//     unless the same scope proves it holds exactly one entry (`len(m) != 1`),
//     which is a single-column read wearing a loop
//
// The scope is `pkg/relational/sqldriver`. That is where the converted
// population lives and where a regrowth would land; other packages have their
// own boundaries and are not this gate's business.

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// rowValueMapScope is the package whose row assertions CQ-64 converted.
const rowValueMapScope = "pkg/relational/sqldriver/"

// isRowValueMapAssert reports whether e is `executor.RowValue(...).(map[string]any)`
// — the projection of a row to its name-keyed form.
//
// `any` and `interface{}` are both matched: they are the same type, and a gate
// that saw only one spelling would be defeated by `gofmt -r` or by habit.
func isRowValueMapAssert(e ast.Expr) bool {
	ta, ok := e.(*ast.TypeAssertExpr)
	if !ok || ta.Type == nil {
		return false
	}
	call, isCall := ta.X.(*ast.CallExpr)
	if !isCall || callName(call) != "RowValue" {
		return false
	}
	m, isMap := ta.Type.(*ast.MapType)
	if !isMap {
		return false
	}
	if k, isIdent := m.Key.(*ast.Ident); !isIdent || k.Name != "string" {
		return false
	}
	switch v := m.Value.(type) {
	case *ast.Ident:
		return v.Name == "any"
	case *ast.InterfaceType:
		return v.Methods == nil || len(v.Methods.List) == 0
	}
	return false
}

// stringLitValue returns the value of a plain string literal, and whether e was one.
func stringLitValue(e ast.Expr) (string, bool) {
	lit, ok := e.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	return lit.Value, true
}

// parentMap indexes every node in f to its parent, and returns the type
// assertions of interest. One walk, so the classification below can ask about a
// node's context without re-walking per site.
func parentMap(f *ast.File) (map[ast.Node]ast.Node, []*ast.TypeAssertExpr) {
	parent := map[ast.Node]ast.Node{}
	var stack []ast.Node
	var found []*ast.TypeAssertExpr
	ast.Inspect(f, func(n ast.Node) bool {
		if n == nil {
			stack = stack[:len(stack)-1]
			return true
		}
		if len(stack) > 0 {
			parent[n] = stack[len(stack)-1]
		}
		stack = append(stack, n)
		if ta, ok := n.(*ast.TypeAssertExpr); ok && isRowValueMapAssert(ta) {
			found = append(found, ta)
		}
		return true
	})
	return parent, found
}

// enclosingFunc returns the INNERMOST function body containing n.
//
// Innermost matters: these test files reuse `row` and `mapped` across many
// subtests, and scoping the use-analysis to the file would merge unrelated
// functions' keys and condemn two correct single-column reads as one multi-slot
// read. A gate that manufactures its own findings is worse than none.
func enclosingFunc(n ast.Node, parent map[ast.Node]ast.Node) ast.Node {
	for cur := parent[n]; cur != nil; cur = parent[cur] {
		switch cur.(type) {
		case *ast.FuncDecl, *ast.FuncLit:
			return cur
		}
	}
	return nil
}

// boundName returns the identifier a type assertion is assigned to, if any —
// `row, ok := ...` / `mapped := ...` / `var m = ...`.
func boundName(ta *ast.TypeAssertExpr, parent map[ast.Node]ast.Node) string {
	switch p := parent[ta].(type) {
	case *ast.AssignStmt:
		for i, rhs := range p.Rhs {
			if rhs == ast.Expr(ta) && i < len(p.Lhs) {
				if id, ok := p.Lhs[i].(*ast.Ident); ok {
					return id.Name
				}
			}
		}
	case *ast.ValueSpec:
		for i, v := range p.Values {
			if v == ast.Expr(ta) && i < len(p.Names) {
				return p.Names[i].Name
			}
		}
	}
	return ""
}

// mapUses classifies every mention of `name` inside scope: the distinct
// string-literal keys indexed out of it, whether it is consumed WHOLE, and
// whether the scope proves it holds exactly one entry.
func mapUses(name string, scope ast.Node, parent map[ast.Node]ast.Node) (keys []string, whole, singleEntryProven bool) {
	seen := map[string]bool{}
	ast.Inspect(scope, func(n ast.Node) bool {
		id, ok := n.(*ast.Ident)
		if !ok || id.Name != name {
			return true
		}
		switch p := parent[id].(type) {
		case *ast.AssignStmt:
			for _, l := range p.Lhs {
				if l == ast.Expr(id) {
					return true // the binding occurrence, not a use
				}
			}
			whole = true
		case *ast.ValueSpec:
			for _, nm := range p.Names {
				if nm == id {
					return true
				}
			}
			whole = true
		case *ast.IndexExpr:
			if p.X != ast.Expr(id) {
				return true // the map is the INDEX of something else
			}
			if k, isStr := stringLitValue(p.Index); isStr {
				if !seen[k] {
					seen[k] = true
					keys = append(keys, k)
				}
				return true
			}
			// A computed key is a name-keyed read whose name the gate cannot
			// see, so it cannot be shown to be single-column.
			whole = true
		case *ast.CallExpr:
			if fn, isIdent := p.Fun.(*ast.Ident); isIdent && fn.Name == "len" {
				return true // a width check, not a read
			}
			whole = true
		case *ast.RangeStmt:
			if p.X == ast.Expr(id) {
				whole = true
			}
		default:
			whole = true
		}
		return true
	})
	// `len(m) != 1` / `1 == len(m)` anywhere in scope: the map is proven to hold
	// a single entry, so ranging or printing it is a single-column read.
	ast.Inspect(scope, func(n ast.Node) bool {
		be, ok := n.(*ast.BinaryExpr)
		if !ok {
			return true
		}
		isLenOfName := func(e ast.Expr) bool {
			c, isCall := e.(*ast.CallExpr)
			if !isCall || len(c.Args) != 1 {
				return false
			}
			fn, isIdent := c.Fun.(*ast.Ident)
			arg, isArgIdent := c.Args[0].(*ast.Ident)
			return isIdent && fn.Name == "len" && isArgIdent && arg.Name == name
		}
		isOne := func(e ast.Expr) bool {
			lit, isLit := e.(*ast.BasicLit)
			return isLit && lit.Kind == token.INT && lit.Value == "1"
		}
		if (isLenOfName(be.X) && isOne(be.Y)) || (isLenOfName(be.Y) && isOne(be.X)) {
			singleEntryProven = true
		}
		return true
	})
	sort.Strings(keys)
	return keys, whole, singleEntryProven
}

// scanRowValueMapExpectations reports every multi-slot consumption of a row's
// name-keyed projection. Split out from the test so both halves can be pinned
// against synthetic source.
func scanRowValueMapExpectations(f *ast.File, report func(token.Pos, string)) {
	parent, asserts := parentMap(f)
	for _, ta := range asserts {
		// `executor.RowValue(r).(map[string]any)["ID"]` — an inline read of ONE
		// named column, the most common legal shape in this package.
		if ix, ok := parent[ta].(*ast.IndexExpr); ok && ix.X == ast.Expr(ta) {
			if _, isStr := stringLitValue(ix.Index); isStr {
				continue
			}
			report(ta.Lparen, "executor.RowValue(...).(map[string]any) indexed by a NON-LITERAL key — "+
				"the gate cannot show this reads a single column")
			continue
		}
		name := boundName(ta, parent)
		if name == "" {
			report(ta.Lparen, "executor.RowValue(...).(map[string]any) consumed whole — a row's "+
				"name-keyed projection has no slot order and collapses duplicate names")
			continue
		}
		scope := enclosingFunc(ta, parent)
		if scope == nil {
			scope = f
		}
		keys, whole, singleEntry := mapUses(name, scope, parent)
		if len(keys) > 1 {
			report(ta.Lparen, fmt.Sprintf("%q reads %d distinct columns out of a row's name-keyed "+
				"projection (%s) — a multi-slot read through a map that has no slot order",
				name, len(keys), strings.Join(keys, ", ")))
			continue
		}
		if whole && !singleEntry {
			report(ta.Lparen, fmt.Sprintf("%q consumes a row's name-keyed projection WHOLE (ranged, "+
				"printed or passed on) without proving it holds one entry", name))
		}
	}
}

func TestSqlDriverRowsAreNotAssertedThroughTheNameKeyedMap(t *testing.T) {
	t.Parallel()

	root := sourceTreeRoot(t)
	var findings []string
	scanned := 0
	for _, rel := range trackedGoFiles(t, root) {
		if !strings.HasPrefix(rel, rowValueMapScope) {
			continue
		}
		src, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		if !strings.Contains(string(src), "RowValue") {
			continue
		}
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, rel, src, parser.ParseComments)
		if err != nil {
			continue // not this gate's job to police syntax
		}
		scanned++
		scanRowValueMapExpectations(f, func(pos token.Pos, form string) {
			findings = append(findings, fmt.Sprintf("%s:%d: %s", rel, fset.Position(pos).Line, form))
		})
	}
	// A scan that reaches no files is green forever while the class regrows. The
	// population is large and stable, so a floor is a real check rather than a
	// ceremony; if it trips, the scope prefix or the tracked-file walk broke.
	if scanned < 10 {
		t.Fatalf("the scan reached only %d file(s) under %s that mention RowValue — this gate is "+
			"vacuous below the measured population (13 files hold legal single-column reads). "+
			"Check rowValueMapScope and the tracked-file walk before trusting a green result.",
			scanned, rowValueMapScope)
	}
	sort.Strings(findings)
	if len(findings) > 0 {
		t.Errorf("%d site(s) assert a MULTI-SLOT row through its name-keyed projection.\n\n"+
			"executor.RowValue drops slot ORDER and collapses duplicate output names LAST-WINS.\n"+
			"Permute a row's (Fields, Slots) together — which is what a mis-bound leg window IS —\n"+
			"and the projection is byte-identical while every slot moved, so an assertion built on\n"+
			"it passes with the row entirely rebound. CQ-64 converted 573 such expectations.\n\n"+
			"Render the row POSITIONALLY instead: positionalPipeSprint / positionalNamedPipeSprint\n"+
			"/ positionalSprint for a string comparison, positionalSlots for a typed read. All\n"+
			"live in pkg/relational/sqldriver/positional_row_render_test.go.\n\n"+
			"Reading ONE named column out of the map stays legal and needs no change — there is no\n"+
			"order to lose and nothing to collide with.\n\n%s",
			len(findings), strings.Join(findings, "\n"))
	}
}

// The gate is a claim about what may reach the package, so it is pinned against
// synthetic source in BOTH directions.
//
// The LEGAL half is the one that matters most here. The converted population's
// remainder is 13 files of single-column reads that are perfectly correct, and a
// detector that condemned them would either be suppressed with an allowlist or
// deleted outright — at which point the multi-slot class has nothing watching it.
func TestRowValueMapDetectorPrecisionAndRecall(t *testing.T) {
	t.Parallel()

	scan := func(body string) []string {
		src := "package p\n\n" + body
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, "snippet.go", src, parser.ParseComments)
		if err != nil {
			t.Fatalf("parse snippet: %v\n---\n%s", err, src)
		}
		var got []string
		scanRowValueMapExpectations(f, func(_ token.Pos, form string) { got = append(got, form) })
		return got
	}

	caught := []struct{ name, body string }{
		{
			name: "two distinct columns read out of the map",
			body: `func f(r executor.QueryResult) {
	row, _ := executor.RowValue(r).(map[string]any)
	got := row["ID"].(int64) + row["Y"].(int64)
	_ = got
}`,
		},
		{
			name: "the sorted-map-key rendering — the exact form CQ-64 removed",
			body: `func f(r executor.QueryResult) string {
	m, _ := executor.RowValue(r).(map[string]any)
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+fmt.Sprint(m[k]))
	}
	return strings.Join(parts, "|")
}`,
		},
		{
			name: "a hand-written column list indexing the map",
			body: `func f(r executor.QueryResult) []any {
	row, _ := executor.RowValue(r).(map[string]any)
	return []any{row["Y"], row["ID"]}
}`,
		},
		{
			name: "the whole map printed",
			body: `func f(r executor.QueryResult) string {
	row, _ := executor.RowValue(r).(map[string]any)
	return fmt.Sprint(row)
}`,
		},
		{
			name: "the whole map compared against a literal expectation",
			body: `func f(t *testing.T, r executor.QueryResult) {
	row, _ := executor.RowValue(r).(map[string]any)
	if !reflect.DeepEqual(row, map[string]any{"ID": int64(1), "Y": int64(2)}) {
		t.Fatal("mismatch")
	}
}`,
		},
		{
			name: "ranged over with no width proof",
			body: `func f(r executor.QueryResult) {
	row, _ := executor.RowValue(r).(map[string]any)
	for _, v := range row {
		_ = v
	}
}`,
		},
		{
			name: "the projection consumed whole without ever being bound",
			body: `func f(r executor.QueryResult) string {
	return fmt.Sprint(executor.RowValue(r).(map[string]any))
}`,
		},
		{
			name: "indexed by a computed key the gate cannot resolve",
			body: `func f(r executor.QueryResult, col string) any {
	row, _ := executor.RowValue(r).(map[string]any)
	return row[col]
}`,
		},
		{
			name: "interface{} spelled out rather than any",
			body: `func f(r executor.QueryResult) {
	row, _ := executor.RowValue(r).(map[string]interface{})
	_, _ = row["ID"], row["Y"]
}`,
		},
		{
			name: "a len guard against a width OTHER than one does not license the whole read",
			body: `func f(r executor.QueryResult) {
	row, _ := executor.RowValue(r).(map[string]any)
	if len(row) != 3 {
		panic("x")
	}
	for _, v := range row {
		_ = v
	}
}`,
		},
	}
	for _, tc := range caught {
		t.Run("caught/"+tc.name, func(t *testing.T) {
			t.Parallel()
			if got := scan(tc.body); len(got) == 0 {
				t.Errorf("detector missed a multi-slot read through the name-keyed projection.\n"+
					"source:\n%s\n"+
					"A gate that does not match this shape is green forever while the class regrows.",
					tc.body)
			}
		})
	}

	legal := []struct{ name, body, why string }{
		{
			name: "ONE named column read out of a bound map",
			body: `func f(r executor.QueryResult) int64 {
	row, ok := executor.RowValue(r).(map[string]any)
	if !ok {
		return 0
	}
	return row["ID"].(int64)
}`,
			why: "a single column has no order to lose and cannot collide with itself; this is " +
				"the shape 13 files in the package legitimately use",
		},
		{
			name: "ONE named column read INLINE, the vector-suite shape",
			body: `func f(r executor.QueryResult) int64 {
	return executor.RowValue(r).(map[string]any)["ID"].(int64)
}`,
			why: "same read without the intermediate binding",
		},
		{
			name: "the same key read several times",
			body: `func f(r executor.QueryResult) (int64, string) {
	row, _ := executor.RowValue(r).(map[string]any)
	id, _ := row["ID"].(int64)
	return id, fmt.Sprintf("%T", row["ID"])
}`,
			why: "repetition of ONE key is still one column — the gate counts DISTINCT keys, " +
				"because an error message re-reading the value it just failed on is routine",
		},
		{
			name: "ranged over with the width PROVEN to be one",
			body: `func f(t *testing.T, r executor.QueryResult) int64 {
	row, ok := executor.RowValue(r).(map[string]any)
	if !ok || len(row) != 1 {
		t.Fatal("want one projected value")
	}
	var out int64
	for _, v := range row {
		out = v.(int64)
	}
	return out
}`,
			why: "fanout_exists_index_fdb_test.go's COUNT read — a single-column read wearing a " +
				"loop, because the column's NAME is what the site does not want to hard-code",
		},
		{
			name: "two DIFFERENT rows each reading their own single column",
			body: `func f(a, b executor.QueryResult) (int64, int64) {
	x, _ := executor.RowValue(a).(map[string]any)
	y, _ := executor.RowValue(b).(map[string]any)
	return x["ID"].(int64), y["ID"].(int64)
}`,
			why: "two single-column reads are not one multi-slot read; scoping is per variable",
		},
		{
			name: "the same variable NAME in two different functions",
			body: `func f(r executor.QueryResult) int64 {
	row, _ := executor.RowValue(r).(map[string]any)
	return row["ID"].(int64)
}

func g(r executor.QueryResult) int64 {
	row, _ := executor.RowValue(r).(map[string]any)
	return row["Y"].(int64)
}`,
			why: "use-analysis is scoped to the INNERMOST enclosing function; merging them would " +
				"manufacture a two-key finding out of two correct one-key reads",
		},
		{
			name: "the same variable name in two sibling closures",
			body: `func f() {
	t.Run("a", func(t *testing.T) {
		row, _ := executor.RowValue(r).(map[string]any)
		_ = row["ID"]
	})
	t.Run("b", func(t *testing.T) {
		row, _ := executor.RowValue(r).(map[string]any)
		_ = row["Y"]
	})
}`,
			why: "subtests are FuncLits and this package is full of them; the innermost scope is " +
				"the closure, not the outer test function",
		},
		{
			name: "RowValue used for a %T diagnostic without the map assertion",
			body: `func f(r executor.QueryResult) string {
	return fmt.Sprintf("%T", executor.RowValue(r))
}`,
			why: "no assertion to map[string]any, so no name-keyed row is being read; the error " +
				"paths of every legal site do exactly this",
		},
		{
			name: "the deliberate probe asserting the map's OWN collapse",
			body: `func f(t *testing.T, dup executor.QueryResult) {
	if m, ok := executor.RowValue(dup).(map[string]any); !ok || len(m) != 2 {
		t.Errorf("last-wins collapse changed")
	}
}`,
			why: "TestPositionalRenderersSeeAPermutation asserts ABOUT the projection's blindness " +
				"rather than asserting a row through it — the measurement that justifies the " +
				"whole conversion must stay writable",
		},
		{
			name: "a map that is not a row at all",
			body: `func f(m map[string]any) any {
	return m["ID"]
}`,
			why: "the gate keys on executor.RowValue, not on map indexing in general",
		},
	}
	for _, tc := range legal {
		t.Run("legal/"+tc.name, func(t *testing.T) {
			t.Parallel()
			if got := scan(tc.body); len(got) != 0 {
				t.Errorf("detector flagged a LEGAL read: %v\nsource:\n%s\nwhy this must stay legal: %s\n"+
					"A gate that condemns the correct shape as well as the wrong one leaves no legal "+
					"alternative, and a gate with no legal alternative gets deleted.",
					got, tc.body, tc.why)
			}
		})
	}
}
