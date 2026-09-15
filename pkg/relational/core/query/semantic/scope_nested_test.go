package semantic

import (
	"errors"
	"testing"
)

// Java's fifth matching rule, lookupNestedField (SemanticAnalyzer.java:481-488
// dispatching to :548-602), is what lets `home_address.city` mean "the CITY
// field of the HOME_ADDRESS struct column" rather than "the CITY column of a
// FROM source named HOME_ADDRESS". These tests pin the rule's three
// load-bearing properties separately, because the descent can be wrong in
// three independent ways: it can fail to descend at all, it can descend to the
// wrong slot, and it can turn a losing candidate into a hard error.

// structScope builds a scope with one source carrying a STRUCT column
// HOME_ADDRESS(CITY, ZIP) beside a scalar NAME.
func structScope(t *testing.T, extra ...ScopeSource) *Scope {
	t.Helper()
	users := &StaticTable{
		TableName: ParseQualifiedName("users", false),
		TableColumns: []Column{
			{Id: NewUnquoted("id"), Type: "BIGINT"},
			{Id: NewUnquoted("name"), Type: "STRING"},
			{Id: NewUnquoted("home_address"), Type: "RECORD", StructFields: []Column{
				{Id: NewUnquoted("city"), Type: "STRING"},
				{Id: NewUnquoted("zip"), Type: "BIGINT"},
			}},
		},
	}
	s := NewScope(nil)
	if err := s.AddSource(ScopeSource{Table: users, Alias: NewUnquoted("u"), CorrelationName: "u"}); err != nil {
		t.Fatalf("AddSource users: %v", err)
	}
	for _, e := range extra {
		if err := s.AddSource(e); err != nil {
			t.Fatalf("AddSource extra: %v", err)
		}
	}
	return s
}

// The descent happens at all, and lands on the field the name selects — NOT
// merely on some field of the struct. ZIP is asserted alongside CITY because a
// descent hard-wired to ordinal 0 passes a CITY-only test with the bug fully
// present.
func TestScope_NestedDescent_ResolvesFieldByName(t *testing.T) {
	t.Parallel()
	s := structScope(t)

	for _, tc := range []struct {
		field    string
		wantOrd  int
		wantType string
	}{
		{"city", 0, "STRING"},
		{"zip", 1, "BIGINT"},
	} {
		col, _, accessors, err := s.ResolveQualifiedColumnNested(NewUnquoted("home_address"), NewUnquoted(tc.field))
		if err != nil {
			t.Fatalf("resolve home_address.%s: %v", tc.field, err)
		}
		if col.Id.Name() != "HOME_ADDRESS" {
			t.Errorf("home_address.%s: root column = %q, want HOME_ADDRESS — the root of a descent is the STRUCT, not the leaf", tc.field, col.Id.Name())
		}
		if len(accessors) != 1 {
			t.Fatalf("home_address.%s: got %d accessors, want 1", tc.field, len(accessors))
		}
		if got := accessors[0].Ordinal; got != tc.wantOrd {
			t.Errorf("home_address.%s: ordinal = %d, want %d — the ordinal is the field's position in the struct's declared field list (SemanticAnalyzer.java:586-588)", tc.field, got, tc.wantOrd)
		}
		if got := accessors[0].Col.Type; got != tc.wantType {
			t.Errorf("home_address.%s: leaf type = %q, want %q", tc.field, got, tc.wantType)
		}
	}
}

// A field the struct does not have is an ordinary MISS, not a bespoke error.
// Java returns Optional.empty() from both lookupNestedField failure arms
// (SemanticAnalyzer.java:581-583 non-struct, :594-596 no such field) and lets
// UNDEFINED_COLUMN surface generically once every attribute has declined. A
// "no such field FOO in struct BAR" error raised here would kill a reference
// that a LATER source in the scope resolves.
func TestScope_NestedDescent_MissIsNotAnError(t *testing.T) {
	t.Parallel()
	s := structScope(t)

	_, _, _, err := s.ResolveQualifiedColumnNested(NewUnquoted("home_address"), NewUnquoted("nosuchfield"))
	var srcNotFound *SourceNotFoundError
	if !errors.As(err, &srcNotFound) {
		t.Fatalf("home_address.nosuchfield: got %v (%T), want the ordinary SourceNotFoundError — a nested miss must not mint its own error class", err, err)
	}

	// Descending into a NON-struct column is the same ordinary miss.
	_, _, _, err = s.ResolveQualifiedColumnNested(NewUnquoted("name"), NewUnquoted("city"))
	if !errors.As(err, &srcNotFound) {
		t.Fatalf("name.city: got %v (%T), want SourceNotFoundError — a scalar column has no fields to descend into", err, err)
	}
}

// A direct match and a nested match that answer the SAME reference are an
// AMBIGUITY, not a preference. Java appends both into one list and errors when
// it holds more than one (SemanticAnalyzer.java:433-437, "Ambiguous reference
// %s", ErrorCode.AMBIGUOUS_COLUMN). Resolving the collision by which candidate
// was computed first would make order of attempt into a semantics — and a
// nested descent evaluated only as a FALLBACK after direct resolution failed
// is exactly that, which is why this test exists and not a fallback.
func TestScope_NestedDescent_CollidesWithSourceAliasAsAmbiguity(t *testing.T) {
	t.Parallel()
	// A second source ALIASED `home_address` that also carries a CITY column:
	// now `home_address.city` is answerable both as a source-qualified column
	// and as a descent into the struct column of the same name.
	addrTable := &StaticTable{
		TableName: ParseQualifiedName("addresses", false),
		TableColumns: []Column{
			{Id: NewUnquoted("city"), Type: "STRING"},
		},
	}
	s := structScope(t, ScopeSource{Table: addrTable, Alias: NewUnquoted("home_address"), CorrelationName: "home_address"})

	_, _, _, err := s.ResolveQualifiedColumnNested(NewUnquoted("home_address"), NewUnquoted("city"))
	var ambig *AmbiguousColumnError
	if !errors.As(err, &ambig) {
		t.Fatalf("home_address.city with both a struct column and a same-named source: got %v (%T), want AmbiguousColumnError", err, err)
	}
	if got := ambig.Reference(); got != "HOME_ADDRESS.CITY" {
		t.Errorf("ambiguous reference rendered %q, want HOME_ADDRESS.CITY", got)
	}
}

// LookupStructField is name-driven and first-match-wins, and it reports the
// ordinal as the field's POSITION. Pinned separately from the scope walk
// because the position is what the emitted FieldValue accessor carries into
// the plan, where a wrong number reads a different column and never errors.
func TestColumn_LookupStructField_OrdinalIsDeclaredPosition(t *testing.T) {
	t.Parallel()
	c := Column{Id: NewUnquoted("s"), Type: "RECORD", StructFields: []Column{
		{Id: NewUnquoted("a"), Type: "BIGINT"},
		{Id: NewUnquoted("b"), Type: "STRING"},
		{Id: NewUnquoted("c"), Type: "DOUBLE"},
	}}
	for want, name := range []string{"a", "b", "c"} {
		f, ord, ok := c.LookupStructField(NewUnquoted(name))
		if !ok {
			t.Fatalf("field %s not found", name)
		}
		if ord != want {
			t.Errorf("field %s: ordinal = %d, want %d", name, ord, want)
		}
		if f.Id.Name() != NewUnquoted(name).Name() {
			t.Errorf("field %s: returned %q", name, f.Id.Name())
		}
	}
	if _, _, ok := c.LookupStructField(NewUnquoted("zz")); ok {
		t.Error("LookupStructField found a field that does not exist")
	}
	// A non-struct column has no fields at all.
	if _, _, ok := (Column{Id: NewUnquoted("n"), Type: "STRING"}).LookupStructField(NewUnquoted("a")); ok {
		t.Error("LookupStructField descended into a scalar column")
	}
}

func TestNestedLookupRequiresStructValue(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, typ   string
		array, want bool
	}{
		{"struct", "RECORD", false, true},
		{"array_of_struct", "RECORD", true, false},
		{"scalar_with_element_metadata", "STRING", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			column := Column{Type: tc.typ, IsArray: tc.array, StructFields: []Column{{Id: FromNormalized("leaf"), Type: "BIGINT"}}}
			_, _, direct := column.LookupStructField(FromNormalized("leaf"))
			_, _, strict := strictPass.lookupStructField(column, FromNormalized("leaf"))
			_, _, relaxed := relaxedPass.lookupStructField(column, NewUnquoted("LEAF"))
			if direct != tc.want || strict != tc.want || relaxed != tc.want {
				t.Fatalf("direct/strict/relaxed = %v/%v/%v, want %v; element metadata must not authorize container descent", direct, strict, relaxed, tc.want)
			}
		})
	}
}

func TestArrayMetadataDoesNotCompeteWithUnnestedStruct(t *testing.T) {
	t.Parallel()
	element := Column{Id: NewUnquoted("items"), Type: "RECORD", StructFields: []Column{
		{Id: NewUnquoted("n"), Type: "RECORD", StructFields: []Column{{Id: NewUnquoted("vals"), Type: "DOUBLE", IsArray: true}}},
	}}
	array := element
	array.IsArray = true
	scope := NewScope(nil)
	for _, src := range []ScopeSource{
		{Alias: NewUnquoted("items"), CorrelationName: "ITEMS", Table: &StaticTable{TableColumns: []Column{array}}},
		{Alias: NewUnquoted("items"), CorrelationName: "Q$DUP1", Shadowing: true, Table: &StaticTable{TableColumns: []Column{element}}},
	} {
		if err := scope.AddSource(src); err != nil {
			t.Fatal(err)
		}
	}
	_, owner, path, err := scope.ResolveSourceQualifiedPath(segs("items", "items", "n", "vals"))
	if err != nil || owner.CorrelationName != "Q$DUP1" || len(path) != 2 || path[0].Ordinal != 0 || path[1].Ordinal != 0 || !path[1].Col.IsArray {
		t.Fatalf("source-qualified array: owner %s, path %+v, error %v", owner.CorrelationName, path, err)
	}
	_, owner, path, err = scope.ResolvePathNested(segs("items", "n", "vals"))
	if err != nil || owner.CorrelationName != "Q$DUP1" || len(path) != 2 || !path[1].Col.IsArray {
		t.Fatalf("struct-relative array: owner %s, path %+v, error %v", owner.CorrelationName, path, err)
	}
}

func TestNestedArrayStopsDescentButRemainsAddressable(t *testing.T) {
	t.Parallel()
	array := Column{Id: NewUnquoted("a"), Type: "RECORD", IsArray: true, StructFields: []Column{{Id: NewUnquoted("leaf"), Type: "BIGINT"}}}
	scope := NewScope(nil)
	if err := scope.AddSource(ScopeSource{Alias: NewUnquoted("t"), CorrelationName: "T", Table: &StaticTable{TableColumns: []Column{
		array, {Id: NewUnquoted("s"), Type: "RECORD", StructFields: []Column{array}},
	}}}); err != nil {
		t.Fatal(err)
	}
	for _, path := range [][]Identifier{segs("t", "a"), segs("t", "s", "a")} {
		col, _, accessors, err := scope.ResolveSourceQualifiedPath(path)
		if err != nil {
			t.Fatal(err)
		}
		if len(accessors) > 0 {
			col = accessors[len(accessors)-1].Col
		}
		if !col.IsArray {
			t.Fatalf("%v lost its ARRAY type", path)
		}
	}
	for _, path := range [][]Identifier{segs("t", "a", "leaf"), segs("t", "s", "a", "leaf")} {
		if _, _, accessors, err := scope.ResolveSourceQualifiedPath(path); err == nil {
			t.Fatalf("%v descended through ARRAY using element metadata: %+v", path, accessors)
		}
	}
}
