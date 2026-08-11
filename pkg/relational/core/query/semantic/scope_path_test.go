package semantic

import (
	"errors"
	"testing"
)

// ResolvePathNested is the arbitrary-depth form of the two-segment rule its
// siblings in scope_nested_test.go pin. Java resolves a reference from a
// SEGMENT LIST — Identifier carries `name` plus a `List<String> qualifier`
// built one segment at a time (IdentifierVisitor.java:56-64), and
// lookupNestedField consumes a matched PREFIX and walks whatever remains
// through an unbounded accessor loop (SemanticAnalyzer.java:559-601). Go used
// to carry a (qualifier, column) PAIR, so `a.n.sk` arrived as a lookup for a
// source or struct column literally named "A.N" and resolved to nothing.
//
// These are unit-level because the FDB tests can only reach the arms SQL
// happens to produce today. Depth beyond three and the empty/one-segment edges
// are reachable by construction here and would otherwise ship unexercised.

// deepScope carries a struct nested INSIDE a struct, so a descent has more than
// one step to get wrong: OUTER(INNER(LEAF, OTHER), FLAT).
func deepScope(t *testing.T, extra ...ScopeSource) *Scope {
	t.Helper()
	tbl := &StaticTable{
		TableName: ParseQualifiedName("t", false),
		TableColumns: []Column{
			{Id: NewUnquoted("id"), Type: "BIGINT"},
			{Id: NewUnquoted("outer"), Type: "RECORD", StructFields: []Column{
				{Id: NewUnquoted("inner"), Type: "RECORD", StructFields: []Column{
					{Id: NewUnquoted("leaf"), Type: "STRING"},
					{Id: NewUnquoted("other"), Type: "BIGINT"},
				}},
				{Id: NewUnquoted("flat"), Type: "BIGINT"},
			}},
		},
	}
	s := NewScope(nil)
	if err := s.AddSource(ScopeSource{Table: tbl, Alias: NewUnquoted("a"), CorrelationName: "a"}); err != nil {
		t.Fatalf("AddSource: %v", err)
	}
	for _, e := range extra {
		if err := s.AddSource(e); err != nil {
			t.Fatalf("AddSource extra: %v", err)
		}
	}
	return s
}

func segs(names ...string) []Identifier {
	ids := make([]Identifier, len(names))
	for i, n := range names {
		ids[i] = NewUnquoted(n)
	}
	return ids
}

// Every arity resolves, and each lands on the column the LAST segment names.
// The wantAcc list is asserted in full rather than by length: a descent that
// walked the right number of steps in the wrong order, or that re-used one
// step's ordinal, produces the right count and the wrong column.
func TestResolvePathNested_ResolvesAtEveryDepth(t *testing.T) {
	t.Parallel()
	s := deepScope(t)

	for _, tc := range []struct {
		name     string
		path     []Identifier
		wantRoot string
		wantAcc  []string
		wantOrds []int
		wantType string
	}{
		{
			name: "one segment is the bare arm and never descends",
			path: segs("id"), wantRoot: "ID", wantAcc: nil, wantOrds: nil, wantType: "BIGINT",
		},
		{
			name: "two segments, source-qualified: no descent",
			path: segs("a", "id"), wantRoot: "ID", wantAcc: nil, wantOrds: nil, wantType: "BIGINT",
		},
		{
			name: "two segments, struct-relative: one step",
			path: segs("outer", "flat"), wantRoot: "OUTER",
			wantAcc: []string{"FLAT"}, wantOrds: []int{1}, wantType: "BIGINT",
		},
		{
			name: "three segments, source + struct + member",
			path: segs("a", "outer", "flat"), wantRoot: "OUTER",
			wantAcc: []string{"FLAT"}, wantOrds: []int{1}, wantType: "BIGINT",
		},
		{
			name: "three segments, struct-relative two-step descent",
			path: segs("outer", "inner", "other"), wantRoot: "OUTER",
			wantAcc: []string{"INNER", "OTHER"}, wantOrds: []int{0, 1}, wantType: "BIGINT",
		},
		{
			name: "four segments, source + two-step descent",
			path: segs("a", "outer", "inner", "leaf"), wantRoot: "OUTER",
			wantAcc: []string{"INNER", "LEAF"}, wantOrds: []int{0, 0}, wantType: "STRING",
		},
		{
			name: "four segments landing on the OTHER leaf",
			path: segs("a", "outer", "inner", "other"), wantRoot: "OUTER",
			wantAcc: []string{"INNER", "OTHER"}, wantOrds: []int{0, 1}, wantType: "BIGINT",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			col, _, acc, err := s.ResolvePathNested(tc.path)
			if err != nil {
				t.Fatalf("ResolvePathNested(%v): %v", tc.path, err)
			}
			if col.Id.Name() != tc.wantRoot {
				t.Errorf("root column = %q, want %q — the descent must hang off the column the "+
					"reference enters through, not off the leaf", col.Id.Name(), tc.wantRoot)
			}
			if len(acc) != len(tc.wantAcc) {
				t.Fatalf("accessors = %d, want %d (%v)", len(acc), len(tc.wantAcc), tc.wantAcc)
			}
			for i := range acc {
				if acc[i].Name != tc.wantAcc[i] {
					t.Errorf("accessor %d name = %q, want %q", i, acc[i].Name, tc.wantAcc[i])
				}
				if acc[i].Ordinal != tc.wantOrds[i] {
					t.Errorf("accessor %d ordinal = %d, want %d — the ordinal is the field's "+
						"position in its OWN enclosing struct, not in the path",
						i, acc[i].Ordinal, tc.wantOrds[i])
				}
			}
			leafType := col.Type
			if len(acc) > 0 {
				leafType = acc[len(acc)-1].Col.Type
			}
			if leafType != tc.wantType {
				t.Errorf("leaf type = %q, want %q", leafType, tc.wantType)
			}
		})
	}
}

// The leading segment is a QUALIFIER and must select among sources. Without
// this, a fix that parsed the segment and dropped it would satisfy every
// single-source case above.
func TestResolvePathNested_LeadingSegmentSelectsTheSource(t *testing.T) {
	t.Parallel()
	other := &StaticTable{
		TableName: ParseQualifiedName("t2", false),
		TableColumns: []Column{
			{Id: NewUnquoted("id"), Type: "BIGINT"},
			// SAME struct column name, DIFFERENT member types — so a resolution
			// that reached the wrong source is visible in the type alone.
			{Id: NewUnquoted("outer"), Type: "RECORD", StructFields: []Column{
				{Id: NewUnquoted("flat"), Type: "STRING"},
			}},
		},
	}
	s := deepScope(t, ScopeSource{Table: other, Alias: NewUnquoted("b"), CorrelationName: "b"})

	// a.outer.flat is the BIGINT; b.outer.flat is the STRING. One reference
	// each, distinguished only by the leading segment.
	for _, tc := range []struct{ alias, wantType string }{
		{"a", "BIGINT"},
		{"b", "STRING"},
	} {
		_, src, acc, err := s.ResolvePathNested(segs(tc.alias, "outer", "flat"))
		if err != nil {
			t.Fatalf("%s.outer.flat: %v", tc.alias, err)
		}
		if src.Alias.Name() != NewUnquoted(tc.alias).Name() {
			t.Errorf("%s.outer.flat resolved against source %q — the leading segment was discarded",
				tc.alias, src.Alias.Name())
		}
		if len(acc) != 1 || acc[0].Col.Type != tc.wantType {
			t.Errorf("%s.outer.flat leaf type = %v, want %q", tc.alias, acc, tc.wantType)
		}
	}

	// The bare spelling names no source, so BOTH sources answer it and the
	// reference is ambiguous. This is the control that makes the two
	// resolutions above a statement about the qualifier rather than about
	// which source happens to be first.
	_, _, _, err := s.ResolvePathNested(segs("outer", "flat"))
	var ambig *AmbiguousColumnError
	if !errors.As(err, &ambig) {
		t.Fatalf("bare outer.flat over two sources both declaring OUTER: got %v, want ambiguous", err)
	}
	// And it renders AS WRITTEN. A two-part rendering cannot spell a deeper
	// reference, so the message would name something the user never typed.
	if got := ambig.Reference(); got != "OUTER.FLAT" {
		t.Errorf("ambiguous reference rendered %q, want %q", got, "OUTER.FLAT")
	}
	_, _, _, err3 := s.ResolvePathNested(segs("zz", "outer", "inner", "leaf"))
	var notSrc *SourceNotFoundError
	if !errors.As(err3, &notSrc) {
		t.Fatalf("zz.outer.inner.leaf: got %v, want SourceNotFound — a leading segment naming "+
			"nothing must fail as a missing SOURCE, which is what the caller maps to 42703", err3)
	}
}

// A descent that cannot complete fails ENTIRELY. Java returns Optional.empty()
// from lookupNestedField for a missing field and for a non-struct step alike
// (SemanticAnalyzer.java:576-593) — never a partial path, which would resolve
// the reference to a column it does not name.
func TestResolvePathNested_IncompleteDescentDoesNotPartiallyResolve(t *testing.T) {
	t.Parallel()
	s := deepScope(t)

	for _, tc := range []struct {
		name string
		path []Identifier
	}{
		{"a member that does not exist", segs("a", "outer", "nope")},
		{"a member of a member that does not exist", segs("a", "outer", "inner", "nope")},
		{"a step through a SCALAR field", segs("a", "outer", "flat", "leaf")},
		{"a step through a scalar COLUMN", segs("a", "id", "leaf")},
		{"one segment too many past a valid leaf", segs("a", "outer", "inner", "leaf", "more")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			col, _, acc, err := s.ResolvePathNested(tc.path)
			if err == nil {
				t.Fatalf("%v resolved to %q with accessors %v — an incomplete descent must fail, "+
					"never truncate to the deepest step that happened to match",
					tc.path, col.Id.Name(), acc)
			}
		})
	}
}
