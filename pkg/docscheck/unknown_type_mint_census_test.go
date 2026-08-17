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

// The UnknownType mint ratchet (RFC-181 WS-N Phase D).
//
// A "mint" is `values.UnknownType` written into a composite literal — a
// FieldValue, Field, ConstantValue, ParameterValue or similar constructed with
// its type slot deliberately left unknown. Each such site is a place where a
// type the catalog or the operand tree could have stated was DISCARDED at
// construction, and every downstream consumer of that value is pushed onto a
// name-keyed re-derivation to recover it. That re-derivation family is exactly
// what produced the correlated-scalar wrong-client-type defect (the seed
// minted its inner leg UnknownType, and the bare-name descriptor search that
// "recovered" the type first-matched the wrong join leg), so the mint
// population is the defect's supply side.
//
// Java has no counterpart population: `Type.any()` never stands in for a
// column whose type the catalog knows — resolution attaches the type once
// (SemanticAnalyzer) and metadata is positional over the flowed result type
// (RelationalStructMetaData.getField is List.get(i)).
//
// Phase D's exit state is that a mint is RARE and each survivor carries a WHY.
// This ratchet makes that a measured claim instead of a commit-message one:
// the census below pins every production mint site per file, and the ONLY
// allowed direction is DOWN. A count can go below its pin (lower the pin in
// the same change — that is the ratchet tightening); it must never go above,
// and a file absent from the census must not gain one.
//
// What is deliberately NOT counted:
//   - `return values.UnknownType` and other non-literal uses: a classified
//     DECLINE ("this shape is underivable, fall back") states an answer about
//     the shape, not a discarded type on a constructed value. The gate is
//     about values that ENTER the tree untyped — and a decline is often the
//     FIX for a mint, so counting it would penalize the cure.
//   - Test files: a test must be able to construct an untyped value to pin
//     untyped-value behavior.
//   - `pkg/relational/api/datatype_primitive.go`: `api.UnknownType` is an
//     unrelated type of the same name (the api DataType lattice); the literal
//     there is that type's own singleton definition, not a values-mint.
const unknownTypeMintAllowedFile = "pkg/relational/api/datatype_primitive.go"

// knownUnknownTypeMints pins the census: file → number of production
// composite-literal UnknownType mints. Measured; on any mismatch
// TestUnknownTypeMintCensus prints the live census to re-pin from.
var knownUnknownTypeMints = map[string]int{
	// An aggregate group may have no stored child row. This width-only filler
	// cannot recover a value type from an absent datum.
	"pkg/recordlayer/query/executor/executor_new_plans.go": 1,

	// Name-only positional metadata has no descriptor/type authority. It is a
	// compatibility boundary and never admits an exact QOV.
	"pkg/recordlayer/query/executor/positional_row.go": 1,

	// DerivedValue is the explicit placeholder node for a derivation whose type
	// has not yet been supplied.
	"pkg/recordlayer/query/plan/cascades/values/value_derived.go": 1,

	// ParameterValue's two construction arms cannot know a runtime binding's
	// type before evaluation.
	"pkg/recordlayer/query/plan/cascades/values/values.go": 2,

	// Translator survivors are best-effort metadata-only fallbacks: projection
	// output without an exact value, unnest syntax before element typing, and
	// fieldsFromColumnNames' name-only compatibility row. The last retires with
	// that dead helper below; this pin is exact for the present corpus.
	"pkg/relational/core/query/cascades_translator.go": 2,
}

// scanUnknownTypeMints reports every reference to UnknownType that appears
// inside a composite literal in f — qualified (`values.UnknownType`, any
// import alias) or bare (inside the values package itself). A wrapped mint
// (`values.WithNullability(values.UnknownType, true)` as a literal's field
// value) counts: the wrapper changes the nullability bit, not the fact that
// the constructed value's type slot is unknown.
func scanUnknownTypeMints(f *ast.File, report func(pos token.Pos)) {
	var walk func(n ast.Node, inLit bool)
	walk = func(n ast.Node, inLit bool) {
		if n == nil {
			return
		}
		switch e := n.(type) {
		case *ast.CompositeLit:
			inLit = true
		case *ast.SelectorExpr:
			if e.Sel != nil && e.Sel.Name == "UnknownType" {
				if inLit {
					report(e.Sel.Pos())
				}
				// Do not descend: the Sel ident would report the same site twice.
				return
			}
		case *ast.Ident:
			if e.Name == "UnknownType" && inLit {
				report(e.Pos())
			}
		}
		ast.Inspect(n, func(c ast.Node) bool {
			if c == n {
				return true
			}
			walk(c, inLit)
			return false
		})
	}
	walk(f, false)
}

// TestUnknownTypeMintCensus is the ratchet: the per-file mint counts above are
// exact pins, red in BOTH directions. Above the pin (or a new file) means a
// new value entered the tree with its type discarded; below means the census
// is stale and the pin must be LOWERED — never left slack, because slack is
// headroom a future mint ships inside silently.
func TestUnknownTypeMintCensus(t *testing.T) {
	t.Parallel()

	root := sourceTreeRoot(t)
	got := map[string]int{}
	var sites []string
	scanned := 0
	for _, rel := range trackedGoFiles(t, root) {
		rel = filepath.ToSlash(rel)
		if strings.HasSuffix(rel, "_test.go") || strings.HasPrefix(rel, "gen/") ||
			!strings.HasPrefix(rel, "pkg/") || rel == unknownTypeMintAllowedFile {
			continue
		}
		src, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		scanned++
		if !strings.Contains(string(src), "UnknownType") {
			continue // cheap pre-filter; the parse below is the authority
		}
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, rel, src, parser.ParseComments)
		if err != nil {
			continue // not this gate's job to police syntax
		}
		scanUnknownTypeMints(f, func(pos token.Pos) {
			got[rel]++
			sites = append(sites, fmt.Sprintf("%s:%d", rel, fset.Position(pos).Line))
		})
	}
	// Anti-vacuity floor: a scan that saw almost nothing is a broken root, not
	// a clean tree.
	if scanned < 100 {
		t.Fatalf("mint census scanned only %d production Go files under %s — that is not "+
			"the real source tree; fix sourceTreeRoot/runfiles staging rather than trusting "+
			"an empty census", scanned, root)
	}

	var problems []string
	for rel, n := range got {
		pinned, ok := knownUnknownTypeMints[rel]
		switch {
		case !ok:
			problems = append(problems, fmt.Sprintf(
				"%s: %d mint(s), file not in the census — a NEW UnknownType mint site", rel, n))
		case n > pinned:
			problems = append(problems, fmt.Sprintf(
				"%s: %d mint(s), census pins %d — a NEW UnknownType mint site", rel, n, pinned))
		case n < pinned:
			problems = append(problems, fmt.Sprintf(
				"%s: %d mint(s), census pins %d — mints were removed; LOWER the pin to %d "+
					"(slack left in a pin is headroom the next mint ships inside silently)",
				rel, n, pinned, n))
		}
	}
	for rel := range knownUnknownTypeMints {
		if _, ok := got[rel]; !ok {
			problems = append(problems, fmt.Sprintf(
				"%s: pinned in the census but has no mints (or no longer exists) — delete its entry", rel))
		}
	}
	if len(problems) > 0 {
		sort.Strings(problems)
		sort.Strings(sites)
		t.Errorf("UnknownType mint census mismatch.\n"+
			"A mint constructs a value whose type slot is UnknownType, discarding a type the\n"+
			"catalog or the operand tree could have stated; every downstream reader is then\n"+
			"pushed onto name-keyed re-derivation — the mechanism behind the correlated-scalar\n"+
			"wrong-client-type defect. The only allowed direction is DOWN: populate the type\n"+
			"(catalog/scope knows it), derive it (operand types), or restructure so it becomes\n"+
			"knowable — and when a mint is removed, lower its pin in the same change. Raising a\n"+
			"pin or adding a file needs the same justification as new debt: a WHY comment at\n"+
			"the site explaining what makes the type genuinely unknowable there.\n\n%s\n\n"+
			"full census as measured:\n%s",
			strings.Join(problems, "\n"), strings.Join(sites, "\n"))
	}
}

// TestUnknownTypeMintDetectorPrecisionAndRecall pins the detector against
// synthetic source in both directions, so the census counts what its prose
// claims: a detector that matched nothing would hold every pin green while the
// mint class regrew, and one that matched declines or comparisons would make
// the census unpayable.
func TestUnknownTypeMintDetectorPrecisionAndRecall(t *testing.T) {
	t.Parallel()

	scan := func(body string) int {
		src := "package p\n\n" + body
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, "snippet.go", src, parser.ParseComments)
		if err != nil {
			t.Fatalf("parse snippet: %v\n---\n%s", err, src)
		}
		n := 0
		scanUnknownTypeMints(f, func(token.Pos) { n++ })
		return n
	}

	for _, tc := range []struct {
		name string
		body string
		want int
	}{
		{
			// The canonical mint: a value constructed with its type slot unknown.
			name: "field value in a struct literal",
			body: `func f() any { return &values.FieldValue{Field: "X", Typ: values.UnknownType} }`,
			want: 1,
		},
		{
			// The wrapped form is the same mint: the wrapper adjusts nullability,
			// the constructed value's type is still unknown. This is the exact
			// shape the scalar-subquery seed used to mint before it learned to
			// carry the inner leg's own column type.
			name: "wrapped mint inside a literal",
			body: `func f() any { return values.Field{Name: "S", FieldType: values.WithNullability(values.UnknownType, true), Ordinal: 0} }`,
			want: 1,
		},
		{
			// Inside the values package the constant is a bare identifier.
			name: "bare ident inside the values package",
			body: `func f() any { return &ParameterValue{Ordinal: 1, Typ: UnknownType} }`,
			want: 1,
		},
		{
			// An import alias is the same constant; a text-pinned detector would
			// be bypassed by one import line.
			name: "aliased import qualifier",
			body: `func f() any { return &v2.FieldValue{Typ: v2.UnknownType} }`,
			want: 1,
		},
		{
			name: "two mints in one literal count twice",
			body: `func f() any { return pair{a: values.UnknownType, b: values.UnknownType} }`,
			want: 2,
		},
		{
			// PRECISION: a classified decline states an answer about a shape; it
			// does not construct an untyped value.
			name: "return of UnknownType is a decline, not a mint",
			body: `func f() values.Type { return values.UnknownType }`,
			want: 0,
		},
		{
			name: "comparison against UnknownType is a read, not a mint",
			body: `func f(t values.Type) bool { return t == values.UnknownType }`,
			want: 0,
		},
		{
			name: "plain assignment outside a literal is not counted",
			body: `func f() { var t values.Type = values.UnknownType; _ = t }`,
			want: 0,
		},
		{
			// The values package's own declaration of the constant: the ident
			// being declared sits outside the PrimitiveType literal that follows.
			name: "the constant's own declaration is not a mint",
			body: `var UnknownType Type = &PrimitiveType{TypeCode: TypeCodeUnknown, Nullable: true}`,
			want: 0,
		},
	} {
		if got := scan(tc.body); got != tc.want {
			t.Errorf("%s: detector reported %d, want %d:\n%s", tc.name, got, tc.want, tc.body)
		}
	}
}
