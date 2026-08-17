package embedded

import (
	"testing"

	"fdb.dev/pkg/relational/core/query/semantic"
)

// unnestElementStructFields is a test-side view over the live typed-column
// resolver. Production consumes the full element Column; only this assertion
// needs its nested-field slice.
func unnestElementStructFields(scope *semantic.Scope, j joinClause) []semantic.Column {
	column, ok := unnestElementColumn(scope, j)
	if !ok {
		return nil
	}
	return column.StructFields
}

// TestUnnestElementStructFieldsTakesTheLeaf drives unnestElementStructFields
// over both shapes its two segments can name, because the function cannot tell
// them apart from a chain-free lookup's result:
//
//	`t.arr`  — segment 0 is a SOURCE ALIAS, segment 1 its array column. Direct.
//	`n.arr`  — segment 0 is a STRUCT COLUMN, segment 1 an array field of it.
//	           A DESCENT, whose chain-free result is the struct ROOT `N`.
//
// The root's IsArray is false, so the discarding form makes the second shape
// return no element fields at all — the unnest binding then carries an untyped
// element and `x.co` stops resolving. A decline rather than a wrong read, but
// the same discard, and the reason it is worth closing is that the two shapes
// are indistinguishable at the call site.
//
// This is a UNIT test on purpose. The nested shape is not reachable from SQL
// today — the translator classifies a comma source as a lateral unnest by
// resolving segment 0 against the in-scope source ALIASES (select_parser.go
// segments doc), so `FROM t, n.arr AS x` is read as a database qualifier and
// dies with 42F00 before this function runs. That decline is pinned end to end
// in the driver suite. Driving the branch here is what keeps it from shipping
// untested: its first firing would otherwise read as a finding rather than as a
// branch nobody exercised.
func TestUnnestElementStructFieldsTakesTheLeaf(t *testing.T) {
	t.Parallel()

	leafFields := []semantic.Column{
		{Id: semantic.NewUnquoted("sk"), Type: "BIGINT"},
		{Id: semantic.NewUnquoted("co"), Type: "BIGINT"},
	}
	tbl := &semantic.StaticTable{
		TableName: semantic.ParseQualifiedName("T", false),
		TableColumns: []semantic.Column{
			{Id: semantic.NewUnquoted("id"), Type: "BIGINT"},
			// A top-level array-of-struct column: the DIRECT shape.
			{Id: semantic.NewUnquoted("arr"), Type: "RECORD", IsArray: true, StructFields: leafFields},
			// A struct column whose own field is an array-of-struct: the NESTED
			// shape. Its root is a RECORD and is NOT an array, which is what the
			// discarding lookup used to hand back.
			{Id: semantic.NewUnquoted("n"), Type: "RECORD", StructFields: []semantic.Column{
				{Id: semantic.NewUnquoted("tag"), Type: "BIGINT"},
				{Id: semantic.NewUnquoted("arr"), Type: "RECORD", IsArray: true, StructFields: leafFields},
			}},
		},
	}
	scope := semantic.NewScope(nil)
	if err := scope.AddSource(semantic.ScopeSource{
		Table: tbl, Alias: semantic.NewUnquoted("t"), CorrelationName: "T",
	}); err != nil {
		t.Fatalf("AddSource: %v", err)
	}

	names := func(cols []semantic.Column) []string {
		out := make([]string, len(cols))
		for i, c := range cols {
			out[i] = c.Id.Name()
		}
		return out
	}
	eq := func(got []string, want ...string) bool {
		if len(got) != len(want) {
			return false
		}
		for i := range got {
			if got[i] != want[i] {
				return false
			}
		}
		return true
	}

	// CONTROL — the direct shape, which worked before and must keep working.
	// Without it the nested assertion is satisfied by a function that returns
	// the same fields for everything.
	direct := names(unnestElementStructFields(scope, joinClause{segments: []string{"T", "ARR"}}))
	if !eq(direct, "SK", "CO") {
		t.Fatalf("direct `t.arr` yielded element fields %v, want [SK CO] — the "+
			"nested assertion below is only interpretable against this control",
			direct)
	}

	// THE NESTED SHAPE.
	nested := names(unnestElementStructFields(scope, joinClause{segments: []string{"N", "ARR"}}))
	if !eq(nested, "SK", "CO") {
		t.Fatalf("nested `n.arr` yielded element fields %v, want [SK CO].\n"+
			"  An empty result is what the chain-discarding lookup produced: it "+
			"returns the struct ROOT N, whose IsArray is false, so the array field "+
			"the reference actually named is never seen. Take the accessor chain's "+
			"LEAF — a descent denotes its leaf, never the struct it was reached "+
			"through.", nested)
	}

	// A prior record-valued unnest is represented by a shadowing one-column
	// virtual source. When its alias and an element member have the same name,
	// SUB.SUB must mean the SUB source's whole element followed by its SUB
	// member; the synthetic wrapper column must not compete with that member.
	selfElement := semantic.Column{Id: semantic.NewUnquoted("sub"), Type: "RECORD", StructFields: []semantic.Column{
		{Id: semantic.NewUnquoted("sub"), Type: "RECORD", IsArray: true, StructFields: leafFields},
	}}
	selfScope := semantic.NewScope(nil)
	if err := selfScope.AddSource(semantic.ScopeSource{
		Table: &semantic.StaticTable{
			TableName:    semantic.ParseQualifiedName("sub", false),
			TableColumns: []semantic.Column{selfElement},
		},
		Alias: semantic.NewUnquoted("sub"), CorrelationName: "SUB", Shadowing: true,
	}); err != nil {
		t.Fatalf("AddSource self-named unnest: %v", err)
	}
	selfNamed := names(unnestElementStructFields(selfScope, joinClause{segments: []string{"SUB", "SUB"}}))
	if !eq(selfNamed, "SK", "CO") {
		t.Fatalf("self-named unnest `SUB.SUB` yielded element fields %v, want [SK CO]", selfNamed)
	}

	// A descent that lands on a NON-array field must still yield nothing: the
	// leaf is what decides, so taking the leaf must not become "always accept".
	notAnArray := unnestElementStructFields(scope, joinClause{segments: []string{"N", "TAG"}})
	if len(notAnArray) != 0 {
		t.Fatalf("`n.tag` (a scalar field) yielded element fields %v, want none — "+
			"the IsArray test must apply to the LEAF, not be skipped for descents",
			names(notAnArray))
	}

	// A miss still returns nil rather than panicking on an empty chain.
	if got := unnestElementStructFields(scope, joinClause{segments: []string{"N", "NOPE"}}); got != nil {
		t.Fatalf("`n.nope` yielded %v, want nil", names(got))
	}
}
