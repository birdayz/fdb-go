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

// Two independent walks decide which field of a MIXED seed's record constructor
// is the whole-object scalar element: the planner's window derivation
// (values.OrdinalSeedLegWindows) and the executor's span derivation
// (executor.unnestMixedSeedSpans / ordinalJoinSpans / resolveSpanLeaf). They must
// agree BIT FOR BIT — the element occupies ONE slot while a leg occupies
// width-many, so disagreeing about which field it is shifts the offset of every
// field after it, and a shifted offset is a wrong-column read, not a lost
// optimization.
//
// Both files' comments already CLAIM that agreement, and the claim was false
// twice. The predicate started as two copies of `_, isRecord :=
// qov.Type().(*RecordType); !isRecord`, one per walk; folding them into
// values.IsMixedSeedElementType left a THIRD copy in resolveSpanLeaf that the
// fold missed, still bit-identical and still a copy. Two copies of a rule agree
// until one of them is edited, and prose asserting they agree does not notice
// when one is.
//
// So the claim is made structural. Inside the two layout-derivation files, a type
// assertion to RecordType whose value is DISCARDED is not asking for a type — it
// is asking "is this a record", which in these files is the element question and
// has exactly one authority. Assertions that BIND the type
// (`legType, isRT := …`) are untouched: those need the record and are a different
// question.
//
// # What is deliberately allowed
//
//   - IsMixedSeedElementType's own body. Something has to ask; the point is that
//     exactly one place does.
//   - Every other file. This gate is the scope of the bit-for-bit claim, and a
//     tree-wide ban would flag unrelated is-it-typed tests — positional_merge's
//     scavenger gate asks whether the flowed value is typed AT ALL, which is a
//     genuinely different question and was measured wrong to fold into this one.
var mixedSeedElementAuthorityFiles = []string{
	"pkg/recordlayer/query/plan/cascades/values/ordinal_seed_layout.go",
	"pkg/recordlayer/query/executor/ordinal_join.go",
}

// mixedSeedElementAuthorityFunc is the one function permitted to hand-roll the
// test, because it IS the test.
const mixedSeedElementAuthorityFunc = "IsMixedSeedElementType"

// scanDiscardedRecordTypeAssertions reports every type assertion to RecordType in
// f whose asserted value is discarded, outside the named authority function.
func scanDiscardedRecordTypeAssertions(f *ast.File, authority string, report func(pos token.Pos, form string)) {
	isRecordType := func(e ast.Expr) bool {
		// A pointer to the record type, bare inside package values and qualified
		// elsewhere. The qualifier is not pinned to "values": an import alias names
		// the same type, and a gate an alias bypasses is decoration.
		star, isStar := e.(*ast.StarExpr)
		if !isStar {
			return false
		}
		switch t := star.X.(type) {
		case *ast.Ident:
			return t.Name == "RecordType"
		case *ast.SelectorExpr:
			return t.Sel != nil && t.Sel.Name == "RecordType"
		}
		return false
	}
	// discardedAssert reports the RecordType assertion in an assignment whose FIRST
	// result is the blank identifier — `_, ok := x.(*RecordType)`. The comma-ok
	// form is what makes it a boolean test rather than a conversion.
	discardedAssert := func(lhs, rhs []ast.Expr) (token.Pos, bool) {
		if len(lhs) != 2 || len(rhs) != 1 {
			return 0, false
		}
		blank, isIdent := lhs[0].(*ast.Ident)
		if !isIdent || blank.Name != "_" {
			return 0, false
		}
		ta, isTA := rhs[0].(*ast.TypeAssertExpr)
		if !isTA || ta.Type == nil || !isRecordType(ta.Type) {
			return 0, false
		}
		return ta.Lparen, true
	}

	for _, decl := range f.Decls {
		fn, isFn := decl.(*ast.FuncDecl)
		if !isFn || fn.Body == nil {
			continue
		}
		if fn.Name != nil && fn.Name.Name == authority {
			continue
		}
		owner := "<unknown>"
		if fn.Name != nil {
			owner = fn.Name.Name
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			switch s := n.(type) {
			case *ast.AssignStmt:
				if pos, found := discardedAssert(s.Lhs, s.Rhs); found {
					report(pos, fmt.Sprintf("%s hand-rolls the is-a-record test", owner))
				}
			}
			return true
		})
	}
}

func TestMixedSeedElementTestHasOneAuthority(t *testing.T) {
	t.Parallel()

	root := sourceTreeRoot(t)
	var findings []string
	for _, rel := range mixedSeedElementAuthorityFiles {
		path := filepath.Join(root, filepath.FromSlash(rel))
		src, err := os.ReadFile(path)
		if err != nil {
			// A moved or renamed file silently empties this gate, which is the
			// failure mode every structural check has. Fail instead.
			t.Fatalf("read %s: %v\n"+
				"  This gate names the two files the bit-for-bit claim is about. If one\n"+
				"  moved, point the gate at its new home — do not drop it.", rel, err)
		}
		fset := token.NewFileSet()
		f, parseErr := parser.ParseFile(fset, rel, src, parser.ParseComments)
		if parseErr != nil {
			t.Fatalf("parse %s: %v", rel, parseErr)
		}
		scanDiscardedRecordTypeAssertions(f, mixedSeedElementAuthorityFunc,
			func(pos token.Pos, form string) {
				findings = append(findings, fmt.Sprintf("%s:%d: %s",
					rel, fset.Position(pos).Line, form))
			})
	}
	sort.Strings(findings)
	if len(findings) > 0 {
		t.Errorf("%d hand-rolled copy/copies of the mixed-seed element test.\n"+
			"The planner's window derivation and the executor's span derivation must agree\n"+
			"BIT FOR BIT about which field is the seed's whole-object element: it occupies\n"+
			"ONE slot where a leg occupies width-many, so a disagreement shifts the offset of\n"+
			"every field after it. Both files' comments already claim that agreement, and the\n"+
			"claim was false twice — the second time because folding two copies into\n"+
			"values.IsMixedSeedElementType left a third one behind, bit-identical.\n"+
			"Call values.IsMixedSeedElementType. An assertion that BINDS the record type is\n"+
			"a different question and is not reported.\n\n%s",
			len(findings), strings.Join(findings, "\n"))
	}
}

// The gate is a claim about what cannot reach two files, so the detector is
// pinned against synthetic source in both directions. One that matched nothing
// would be green while the copies regrew; one that matched the BINDING form would
// flag a dozen legitimate reads and get deleted as noise.
func TestMixedSeedElementDetectorPrecisionAndRecall(t *testing.T) {
	t.Parallel()

	scan := func(body string) []string {
		src := "package p\n\n" + body
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, "snippet.go", src, parser.ParseComments)
		if err != nil {
			t.Fatalf("parse snippet: %v\n---\n%s", err, src)
		}
		var got []string
		scanDiscardedRecordTypeAssertions(f, mixedSeedElementAuthorityFunc,
			func(_ token.Pos, form string) { got = append(got, form) })
		return got
	}

	for _, tc := range []struct {
		name     string
		body     string
		wantFind bool
	}{
		{
			// The exact third copy this gate was written for.
			name:     "qualified discarded assertion, negated",
			body:     `func f(q any) bool { if _, isRecord := q.(*values.RecordType); !isRecord { return true }; return false }`,
			wantFind: true,
		},
		{
			// Same question asked positively. A gate keyed on the `!` would bless
			// the inversion, which is the same copy.
			name:     "qualified discarded assertion, positive",
			body:     `func f(q any) bool { _, isRecord := q.(*values.RecordType); return isRecord }`,
			wantFind: true,
		},
		{
			// Inside package values the type is a bare identifier.
			name:     "bare discarded assertion inside the values package",
			body:     `func f(q any) bool { _, ok := q.(*RecordType); return ok }`,
			wantFind: true,
		},
		{
			// An import alias names the same type.
			name:     "aliased import qualifier",
			body:     `func f(q any) bool { _, ok := q.(*v2.RecordType); return ok }`,
			wantFind: true,
		},
		{
			// PRECISION: the authority itself must be legal, or the gate has no exit.
			name:     "the authority's own body",
			body:     `func IsMixedSeedElementType(t any) bool { _, isRecord := t.(*RecordType); return !isRecord }`,
			wantFind: false,
		},
		{
			// PRECISION: the prescribed fix must be legal.
			name:     "calling the authority",
			body:     `func f(q any) bool { return values.IsMixedSeedElementType(q) }`,
			wantFind: false,
		},
		{
			// PRECISION: binding the record is a different question — these files do
			// it a dozen times to READ the row, and flagging them makes the gate noise.
			name:     "assertion that BINDS the record type",
			body:     `func f(q any) any { legType, isRT := q.(*values.RecordType); if !isRT { return nil }; return legType }`,
			wantFind: false,
		},
		{
			// PRECISION: an unrelated discarded assertion is ordinary work.
			name:     "discarded assertion to another type",
			body:     `func f(q any) bool { _, ok := q.(*values.ArrayType); return ok }`,
			wantFind: false,
		},
		{
			// PRECISION: a non-pointer RecordType assertion is not the flowed-type
			// question these walks ask; the element test is always on *RecordType.
			name:     "value (non-pointer) assertion",
			body:     `func f(q any) bool { _, ok := q.(values.RecordType); return ok }`,
			wantFind: false,
		},
	} {
		got := scan(tc.body)
		if tc.wantFind && len(got) == 0 {
			t.Errorf("%s: detector reported NOTHING — the gate would bless this copy:\n%s",
				tc.name, tc.body)
		}
		if !tc.wantFind && len(got) != 0 {
			t.Errorf("%s: detector reported %v on a legal shape — a gate that flags its own "+
				"prescribed fix leaves no way to comply:\n%s", tc.name, got, tc.body)
		}
	}
}
