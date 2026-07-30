package docscheck

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

// A values.RecordTypeLeg records which quantifier owns which slots of a merged
// row. Its Alias is the leg's IDENTITY — the CorrelationIdentifier every reader
// compares through values.SameLeg to answer "does this correlation name that leg
// window?" — and a wrong answer there is a wrong-rows plan, not a lost
// optimization.
//
// Under a composite literal, stating Name and omitting Alias is not a compile
// error. It produces a leg whose identity is the zero CorrelationIdentifier,
// which every reader then fails to bind. That failure is loud for an unpinned
// leg-relative baked reference and silent for a frontier-pinned one, which falls
// through to a whole-row positional read (see executor.buriedLegWindow's comment
// for the disposition per reference kind).
//
// The omission is not hypothetical and it is not caught by the suite: deleting
// `Alias:` from the translator's select-leg producer (3227 legs) and from the
// executor's buried-leg rebase left the ENTIRE test suite green. That is the
// whole reason this gate exists — the defect is invisible at every layer above
// the literal, so it has to be forbidden at the literal.
//
// So the rule is structural: production code constructs a RecordTypeLeg through
// values.NewRecordTypeLeg, which takes the identifier as its FIRST POSITIONAL
// parameter. Forgetting the identity there is a compile error.
//
// # What is deliberately allowed
//
//   - The constructor's own literal, in values/type.go. Something has to build
//     the struct; the point is that exactly one place does.
//   - Test files. A test that pins the unstated-identity behaviour must be able
//     to CONSTRUCT an unstated identity — forbidding the literal in tests would
//     forbid testing the hazard. Production producers are the population that
//     matters, because they are the ones whose omission ships.
//
// A slice or map literal of legs (`[]values.RecordTypeLeg{...}`) is fine as a
// container; what is reported is a STRUCT literal of the leg type, including the
// elided form (`{Alias: a, Name: n}`) that appears inside such a container,
// since that is the same omission written more briefly.
const legConstructorAllowedFile = "pkg/recordlayer/query/plan/cascades/values/type.go"

// scanRecordTypeLegLiterals reports every struct composite literal of
// RecordTypeLeg in f, including elided element literals inside a slice, array or
// map of legs.
func scanRecordTypeLegLiterals(f *ast.File, report func(pos token.Pos, form string)) {
	// isLegType reports whether an expression names the leg type, as a bare
	// identifier (inside package values) or qualified (everywhere else). The
	// qualifier is not pinned to "values": an import alias is still the same type,
	// and a gate that missed an aliased import would be trivially bypassed.
	isLegType := func(e ast.Expr) bool {
		switch t := e.(type) {
		case *ast.Ident:
			return t.Name == "RecordTypeLeg"
		case *ast.SelectorExpr:
			return t.Sel != nil && t.Sel.Name == "RecordTypeLeg"
		}
		return false
	}
	// elementIsLegType reports whether a container literal's element type is the
	// leg type, so its elided element literals can be attributed.
	elementIsLegType := func(e ast.Expr) bool {
		switch t := e.(type) {
		case *ast.ArrayType:
			return isLegType(t.Elt)
		case *ast.MapType:
			return isLegType(t.Value)
		}
		return false
	}

	ast.Inspect(f, func(n ast.Node) bool {
		cl, ok := n.(*ast.CompositeLit)
		if !ok {
			return true
		}
		if cl.Type != nil && isLegType(cl.Type) {
			report(cl.Lbrace, "RecordTypeLeg{...} struct literal")
			return true
		}
		if cl.Type != nil && elementIsLegType(cl.Type) {
			// Report the ELIDED element literals; the container itself is fine. A
			// MAP element arrives wrapped in a KeyValueExpr, so unwrap before
			// looking — the detector's own recall test caught that omission, which
			// would have blessed every leg literal written inside a map.
			for _, el := range cl.Elts {
				if kv, isKV := el.(*ast.KeyValueExpr); isKV {
					el = kv.Value
				}
				if inner, isLit := el.(*ast.CompositeLit); isLit && inner.Type == nil {
					report(inner.Lbrace, "elided {...} leg literal inside a leg container")
				}
			}
		}
		return true
	})
}

func TestRecordTypeLegIsConstructed(t *testing.T) {
	t.Parallel()

	root := sourceTreeRoot(t)
	var findings []string
	for _, rel := range trackedGoFiles(t, root) {
		if strings.HasSuffix(rel, "_test.go") || strings.HasPrefix(rel, "gen/") {
			continue
		}
		if filepath.ToSlash(rel) == legConstructorAllowedFile {
			continue
		}
		src, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		if !strings.Contains(string(src), "RecordTypeLeg") {
			continue // cheap pre-filter; the parse below is the authority
		}
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, rel, src, parser.ParseComments)
		if err != nil {
			continue // not this gate's job to police syntax
		}
		scanRecordTypeLegLiterals(f, func(pos token.Pos, form string) {
			findings = append(findings, fmt.Sprintf("%s:%d: %s",
				rel, fset.Position(pos).Line, form))
		})
	}
	sort.Strings(findings)
	if len(findings) > 0 {
		t.Errorf("%d production RecordTypeLeg literal(s) bypass values.NewRecordTypeLeg.\n"+
			"A literal lets a producer state Name and omit Alias, and Alias is the leg's\n"+
			"IDENTITY: omitting it yields the zero CorrelationIdentifier, which every reader\n"+
			"fails to bind — silently, for a frontier-pinned reference. Deleting `Alias:` from\n"+
			"two producers left the whole suite green, which is why this is a compile-time\n"+
			"obligation rather than a test.\n"+
			"Use values.NewRecordTypeLeg(alias, name, start, width); the identifier is the\n"+
			"first positional parameter, so it cannot be forgotten.\n\n%s",
			len(findings), strings.Join(findings, "\n"))
	}
}

// The gate is a claim about what cannot reach the tree, so it is pinned against
// synthetic source in BOTH directions. A detector that matched nothing would be
// green forever while the class regrew underneath it; one that matched the
// CONTAINER literal or the constructor call would leave no legal way to build a
// leg, and a gate with no legal alternative gets deleted.
func TestRecordTypeLegDetectorPrecisionAndRecall(t *testing.T) {
	t.Parallel()

	scan := func(body string) []string {
		src := "package p\n\n" + body
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, "snippet.go", src, parser.ParseComments)
		if err != nil {
			t.Fatalf("parse snippet: %v\n---\n%s", err, src)
		}
		var got []string
		scanRecordTypeLegLiterals(f, func(pos token.Pos, form string) {
			got = append(got, form)
		})
		return got
	}

	for _, tc := range []struct {
		name     string
		body     string
		wantFind bool
	}{
		{
			// The shape the whole gate is about: Alias omitted, Name stated.
			name:     "qualified struct literal omitting Alias",
			body:     `func f() any { return values.RecordTypeLeg{Name: "A", Start: 0, Width: 1} }`,
			wantFind: true,
		},
		{
			// Stating Alias today is no defence: the next edit drops it, and the
			// point of the constructor is that the next edit cannot.
			name:     "qualified struct literal stating Alias",
			body:     `func f() any { return values.RecordTypeLeg{Alias: a, Name: "A"} }`,
			wantFind: true,
		},
		{
			// Inside the values package the type is a bare identifier.
			name:     "bare struct literal inside the values package",
			body:     `func f() any { return RecordTypeLeg{Name: "A"} }`,
			wantFind: true,
		},
		{
			// The elided form is the same omission written shorter, and it is how
			// the shipped sites actually looked.
			name:     "elided element literal inside a slice of legs",
			body:     `func f() any { return []values.RecordTypeLeg{{Name: "A", Start: 0, Width: 1}} }`,
			wantFind: true,
		},
		{
			name:     "elided element literal inside a map of legs",
			body:     `func f() any { return map[string]values.RecordTypeLeg{"A": {Name: "A"}} }`,
			wantFind: true,
		},
		{
			// An import alias is the same type. A gate keyed on the literal text
			// "values.RecordTypeLeg" would be bypassed by one import line.
			name:     "aliased import qualifier",
			body:     `func f() any { return v2.RecordTypeLeg{Name: "A"} }`,
			wantFind: true,
		},
		{
			// PRECISION: the fix must be legal, or the gate has no exit.
			name:     "constructor call",
			body:     `func f() any { return values.NewRecordTypeLeg(a, "A", 0, 1) }`,
			wantFind: false,
		},
		{
			name:     "slice literal of constructor calls",
			body:     `func f() any { return []values.RecordTypeLeg{values.NewRecordTypeLeg(a, "A", 0, 1)} }`,
			wantFind: false,
		},
		{
			// An empty container declares no leg, so there is nothing to forget.
			name:     "empty slice literal",
			body:     `func f() any { return []values.RecordTypeLeg{} }`,
			wantFind: false,
		},
		{
			// A literal of a DIFFERENT type is ordinary work. If these reported,
			// the gate becomes noise, and noise is read past.
			name:     "unrelated struct literal",
			body:     `func f() any { return values.RecordType{Fields: nil} }`,
			wantFind: false,
		},
		{
			// The container's own type still names the leg type; only its ELEMENTS
			// can omit an identity.
			name:     "slice of legs appended from a variable",
			body:     `func f(l values.RecordTypeLeg) any { return append([]values.RecordTypeLeg(nil), l) }`,
			wantFind: false,
		},
	} {
		got := scan(tc.body)
		if tc.wantFind && len(got) == 0 {
			t.Errorf("%s: detector reported NOTHING — the gate would bless this shape:\n%s",
				tc.name, tc.body)
		}
		if !tc.wantFind && len(got) != 0 {
			t.Errorf("%s: detector reported %v on a legal shape — a gate that flags its own "+
				"prescribed fix leaves no way to comply:\n%s", tc.name, got, tc.body)
		}
	}
}
