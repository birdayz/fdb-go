package semantic

import (
	"errors"
	"testing"
)

// Java's lookup through an operator with preserved expression names and no name
// (ScopeSource.Unnamed): an aggregate block's select-where operator and an outer
// join's. Every expectation is the answer measured against the target in
// conformance/ws_f_table_qualifier_conformance_test.go, named per case.

func unnamedTestScope(t *testing.T, unnamed bool, sources ...ScopeSource) *Scope {
	t.Helper()
	scope := NewScope(nil)
	for _, src := range sources {
		src.Unnamed = unnamed
		if err := scope.AddSource(src); err != nil {
			t.Fatalf("AddSource %s: %v", src.Alias.Name(), err)
		}
	}
	return scope
}

func structS() []Column {
	return []Column{{Id: NewUnquoted("f"), Type: "BIGINT"}, {Id: NewUnquoted("g"), Type: "STRING"}}
}

// x(id, f, x s(f, g)): the table x with a struct column x.
func sourceX() ScopeSource {
	return ScopeSource{Table: &StaticTable{
		TableName: ParseQualifiedName("x", false),
		TableColumns: []Column{
			{Id: NewUnquoted("id"), Type: "BIGINT"},
			{Id: NewUnquoted("f"), Type: "BIGINT"},
			{Id: NewUnquoted("x"), Type: "RECORD", StructFields: structS()},
		},
	}, Alias: NewUnquoted("x"), CorrelationName: "X"}
}

// y(id, h s(f, g)) and h(id, f): a struct column named like another table.
func sourceY() ScopeSource {
	return ScopeSource{Table: &StaticTable{
		TableName: ParseQualifiedName("y", false),
		TableColumns: []Column{
			{Id: NewUnquoted("id"), Type: "BIGINT"},
			{Id: NewUnquoted("h"), Type: "RECORD", StructFields: structS()},
		},
	}, Alias: NewUnquoted("y"), CorrelationName: "Y"}
}

func sourceH() ScopeSource {
	return ScopeSource{Table: &StaticTable{
		TableName: ParseQualifiedName("h", false),
		TableColumns: []Column{
			{Id: NewUnquoted("id"), Type: "BIGINT"},
			{Id: NewUnquoted("f"), Type: "BIGINT"},
		},
	}, Alias: NewUnquoted("h"), CorrelationName: "H"}
}

type pathWant int

const (
	wantResolves pathWant = iota
	wantAmbiguous
	wantNotFound
)

func checkPath(t *testing.T, scope *Scope, path []Identifier, want pathWant, wantCol string, wantDepth int, why string) {
	t.Helper()
	col, _, accessors, err := scope.ResolvePathNested(path)
	switch want {
	case wantResolves:
		if err != nil {
			t.Fatalf("%v: %v; want %s at depth %d (%s)", path, err, wantCol, wantDepth, why)
		}
		name := col.Id.Name()
		if len(accessors) > 0 {
			name = accessors[len(accessors)-1].Col.Id.Name()
		}
		if name != wantCol || len(accessors) != wantDepth {
			t.Fatalf("%v = %s at depth %d; want %s at depth %d (%s)", path, name, len(accessors), wantCol, wantDepth, why)
		}
	case wantAmbiguous:
		var ambig *AmbiguousColumnError
		if !errors.As(err, &ambig) {
			t.Fatalf("%v: %v, %+v; want ambiguous (%s)", path, err, col, why)
		}
	case wantNotFound:
		var notFound *ColumnNotFoundError
		if !errors.As(err, &notFound) {
			t.Fatalf("%v: %v, %+v; want not found (%s)", path, err, col, why)
		}
	}
}

func TestUnnamedSourceCountsTheStructRelativeReadingBesideTheQualified(t *testing.T) {
	t.Parallel()
	named := unnamedTestScope(t, false, sourceX())
	unnamed := unnamedTestScope(t, true, sourceX())
	// SELECT x.f FROM x is x's f; SELECT MAX(x.f) FROM x and
	// SELECT x.f FROM x LEFT JOIN h ON … are 42702.
	checkPath(t, named, segs("x", "f"), wantResolves, "F", 0, "named: the qualified reading first")
	checkPath(t, unnamed, segs("x", "f"), wantAmbiguous, "", 0, "unnamed: x's f and the struct column x's f")
	// x.id has no struct-relative reading (struct x has no id): one column.
	checkPath(t, unnamed, segs("x", "id"), wantResolves, "ID", 0, "unnamed: no struct field id")
	// x.x is the struct column through either operator.
	checkPath(t, named, segs("x", "x"), wantResolves, "X", 0, "named: the struct column")
	checkPath(t, unnamed, segs("x", "x"), wantResolves, "X", 0, "unnamed: the struct column")
}

func TestUnnamedSourceTakesNoDoubledReading(t *testing.T) {
	t.Parallel()
	named := unnamedTestScope(t, false, sourceH())
	unnamed := unnamedTestScope(t, true, sourceH())
	// SELECT w.w.f FROM w resolves; SELECT MAX(w.w.f) FROM w and a WHERE
	// w.w.f after an outer join are 42703.
	checkPath(t, named, segs("h", "h", "f"), wantResolves, "F", 0, "named: the doubled qualifier")
	checkPath(t, unnamed, segs("h", "h", "f"), wantNotFound, "", 0, "unnamed: no operator name to prepend")
}

func TestUnnamedSourceReadsAPathBeginningWithAColumnsBareNameFromTheColumn(t *testing.T) {
	t.Parallel()
	named := unnamedTestScope(t, false, sourceX())
	unnamed := unnamedTestScope(t, true, sourceX())
	// SELECT MAX(x.x.f) FROM x GROUP BY x.id and x.x.f after an outer join are
	// 42703: lookupNestedField reads X.X.F from the column x (cleared
	// qualifier), where struct s has no field x, and never as the qualified
	// path x → x → f. Named, x.x.f is ambiguous (the doubled f, and the path).
	checkPath(t, named, segs("x", "x", "f"), wantAmbiguous, "", 0, "named: the doubled f and x.x's f")
	checkPath(t, unnamed, segs("x", "x", "f"), wantNotFound, "", 0, "unnamed: read from the column x")
	checkPath(t, named, segs("x", "x", "g"), wantResolves, "G", 1, "named: the qualified path")
	checkPath(t, unnamed, segs("x", "x", "g"), wantNotFound, "", 0, "unnamed: read from the column x")
	// SELECT y.h.f FROM y LEFT JOIN h ON … is y's h's f: the path does not
	// begin with the column's bare name, so the qualified path stands.
	checkPath(t, unnamedTestScope(t, true, sourceY(), sourceH()), segs("y", "h", "f"), wantResolves, "F", 1, "unnamed: y → h → f")
}

func TestUnnamedStructRelativeReadingCompetesAcrossSources(t *testing.T) {
	t.Parallel()
	// SELECT h.f FROM y, h is table h's f; SELECT h.f FROM y LEFT JOIN h ON …
	// is 42702 (y's struct column h has an f).
	checkPath(t, unnamedTestScope(t, false, sourceY(), sourceH()), segs("h", "f"), wantResolves, "F", 0, "named: the qualified reading first")
	checkPath(t, unnamedTestScope(t, true, sourceY(), sourceH()), segs("h", "f"), wantAmbiguous, "", 0, "unnamed: both")
	// A level holding both: FROM x LEFT JOIN y ON … JOIN h ON h.f = …, where the
	// outer join's operator is unnamed and h, joined after it, is named: 42702.
	mixed := NewScope(nil)
	y := sourceY()
	y.Unnamed = true
	for _, src := range []ScopeSource{y, sourceH()} {
		if err := mixed.AddSource(src); err != nil {
			t.Fatal(err)
		}
	}
	checkPath(t, mixed, segs("h", "f"), wantAmbiguous, "", 0, "unnamed y's struct-relative reading beside named h's column")
}

func TestDoubledReadingDoesNotCoverAnEphemeralPath(t *testing.T) {
	t.Parallel()
	// A lateral unnest of records aliased item: the element's member b, and
	// the ephemeral whole element item.
	record := ScopeSource{Table: &StaticTable{
		TableName: ParseQualifiedName("item", false),
		TableColumns: []Column{
			{Id: NewUnquoted("b"), Type: "BIGINT"},
			{Id: NewUnquoted("item"), Type: "RECORD", Ephemeral: true, StructFields: []Column{{Id: NewUnquoted("b"), Type: "BIGINT"}}},
		},
	}, Alias: NewUnquoted("item"), CorrelationName: "ITEM", Shadowing: true}
	scope := unnamedTestScope(t, false, record)
	// SELECT item.b resolves (the direct member covers the path through the
	// whole element); SELECT item.item.b is 42702 (the doubled member is named
	// ITEM.B, the path ITEM.ITEM.B, so neither covers the other); SELECT
	// item.item is the element.
	checkPath(t, scope, segs("item", "b"), wantResolves, "B", 0, "the member")
	checkPath(t, scope, segs("item", "item", "b"), wantAmbiguous, "", 0, "the doubled member and the ephemeral path")
	checkPath(t, scope, segs("item", "item"), wantResolves, "ITEM", 0, "the whole element")
	// Source-qualified, as a FROM item's expansion of item.b is resolved:
	// the path through the whole element only.
	if col, _, accessors, err := scope.ResolveSourceQualifiedPath(segs("item", "item", "b")); err != nil || len(accessors) != 1 || accessors[0].Col.Id.Name() != "B" {
		t.Fatalf("source-qualified item.item.b = %+v, %v, %v; want the path through the element", col, accessors, err)
	}
	// A scalar element aliased x: its one attribute is x, so x.x.x is the
	// doubled reading (measured rows).
	scalar := ScopeSource{Table: &StaticTable{
		TableName:    ParseQualifiedName("x", false),
		TableColumns: []Column{{Id: NewUnquoted("x"), Type: "BIGINT"}},
	}, Alias: NewUnquoted("x"), CorrelationName: "X", Shadowing: true}
	checkPath(t, unnamedTestScope(t, false, scalar), segs("x", "x", "x"), wantResolves, "X", 0, "the doubled element")
	checkPath(t, unnamedTestScope(t, false, scalar), segs("x", "x"), wantResolves, "X", 0, "the element")
}

func TestWithUnnamedSourcesCopiesTheLevel(t *testing.T) {
	t.Parallel()
	parent := NewScope(nil)
	scope := NewScope(parent)
	if err := scope.AddSource(sourceX()); err != nil {
		t.Fatal(err)
	}
	view := scope.WithUnnamedSources()
	if view.Parent() != parent {
		t.Fatalf("view parent = %p, want %p", view.Parent(), parent)
	}
	if got := view.Sources(); len(got) != 1 || !got[0].Unnamed {
		t.Fatalf("view sources = %+v, want one Unnamed", got)
	}
	if got := scope.Sources(); got[0].Unnamed {
		t.Fatal("the receiver's source became Unnamed")
	}
}

// ResolvePathAcrossLevels reads a FROM item's path as Java's
// resolveCorrelatedIdentifier does: one lookup over this level and every
// enclosing one. Measured: `EXISTS (SELECT 1 FROM w, w.arr AS v)` under an
// outer w is 42702, and renaming the inner w leaves one reading.
func TestResolvePathAcrossLevels(t *testing.T) {
	t.Parallel()
	withArr := func(alias, corr string) ScopeSource {
		return ScopeSource{Table: &StaticTable{
			TableName: ParseQualifiedName("w", false),
			TableColumns: []Column{
				{Id: NewUnquoted("id"), Type: "BIGINT"},
				{Id: NewUnquoted("arr"), Type: "BIGINT", IsArray: true},
			},
		}, Alias: NewUnquoted(alias), CorrelationName: corr}
	}
	outer := NewScope(nil)
	if err := outer.AddSource(withArr("w", "W")); err != nil {
		t.Fatal(err)
	}
	both := NewScope(outer)
	if err := both.AddSource(withArr("w", "W2")); err != nil {
		t.Fatal(err)
	}
	var ambiguous *AmbiguousColumnError
	if _, _, _, err := both.ResolvePathAcrossLevels(segs("w", "arr")); !errors.As(err, &ambiguous) || ambiguous.Reference() != "W.ARR" {
		t.Fatalf("inner w and outer w: %v, want Ambiguous reference W.ARR", err)
	}
	// ResolvePathNested alone takes the inner level: the check is what makes
	// the FROM item Java's.
	if _, src, _, err := both.ResolvePathNested(segs("w", "arr")); err != nil || src.CorrelationName != "W2" {
		t.Fatalf("ResolvePathNested = %v, %v; want the inner source", src.CorrelationName, err)
	}
	renamed := NewScope(outer)
	if err := renamed.AddSource(withArr("i", "I")); err != nil {
		t.Fatal(err)
	}
	if col, src, _, err := renamed.ResolvePathAcrossLevels(segs("w", "arr")); err != nil || src.CorrelationName != "W" || col.Id.Name() != "ARR" {
		t.Fatalf("outer w only: %v, %v, %v; want the outer w's ARR", src.CorrelationName, col.Id.Name(), err)
	}
	var notFound *SourceNotFoundError
	if _, _, _, err := renamed.ResolvePathAcrossLevels(segs("nosuch", "arr")); !errors.As(err, &notFound) {
		t.Fatalf("no reading: %v, want ResolvePathNested's source-not-found", err)
	}
	// A level that is itself ambiguous reports that.
	twice := NewScope(nil)
	for _, corr := range []string{"W", "Q$DUP1"} {
		if err := twice.AddSource(withArr("w", corr)); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, _, err := NewScope(twice).ResolvePathAcrossLevels(segs("w", "arr")); !errors.As(err, &ambiguous) {
		t.Fatalf("one ambiguous level: %v, want ambiguous", err)
	}

	// Java looks the whole list up twice: the qualified readings of EVERY
	// level first, the struct-relative ones only if none answers. So a
	// qualified reading at one level is not ambiguous with a struct-relative
	// reading at another, in either direction. A source t2 with a struct
	// column n holding arr reads `n.arr` struct-relatively.
	withStructN := func(alias, corr string) ScopeSource {
		return ScopeSource{Table: &StaticTable{
			TableName: ParseQualifiedName("t2", false),
			TableColumns: []Column{
				{Id: NewUnquoted("id"), Type: "BIGINT"},
				{Id: NewUnquoted("n"), Type: "RECORD", StructFields: []Column{
					{Id: NewUnquoted("arr"), Type: "BIGINT", IsArray: true},
				}},
			},
		}, Alias: NewUnquoted(alias), CorrelationName: corr}
	}
	structOuter := NewScope(nil)
	if err := structOuter.AddSource(withStructN("t2", "T2")); err != nil {
		t.Fatal(err)
	}
	qualifiedInner := NewScope(structOuter)
	if err := qualifiedInner.AddSource(withArr("n", "N")); err != nil {
		t.Fatal(err)
	}
	if _, src, acc, err := qualifiedInner.ResolvePathAcrossLevels(segs("n", "arr")); err != nil || src.CorrelationName != "N" || len(acc) != 0 {
		t.Fatalf("qualified inner n.arr beside struct-relative outer: %v, %v, %v; want the inner N's ARR", src.CorrelationName, acc, err)
	}
	qualifiedOuter := NewScope(nil)
	if err := qualifiedOuter.AddSource(withArr("n", "N")); err != nil {
		t.Fatal(err)
	}
	structInner := NewScope(qualifiedOuter)
	if err := structInner.AddSource(withStructN("t2", "T2")); err != nil {
		t.Fatal(err)
	}
	if _, src, acc, err := structInner.ResolvePathAcrossLevels(segs("n", "arr")); err != nil || src.CorrelationName != "N" || len(acc) != 0 {
		t.Fatalf("qualified outer n.arr beside struct-relative inner: %v, %v, %v; want the outer N's ARR", src.CorrelationName, acc, err)
	}
	// With no qualified reading anywhere, the struct-relative one answers.
	alone := NewScope(structOuter)
	if _, src, acc, err := alone.ResolvePathAcrossLevels(segs("n", "arr")); err != nil || src.CorrelationName != "T2" || len(acc) != 1 {
		t.Fatalf("struct-relative only: %v, %v, %v; want T2's n.arr", src.CorrelationName, acc, err)
	}
}

// Through an unnamed operator Java stops at an attribute's direct match
// (lookup's `continue`) and names an unnest's whole element by its bare
// alias. Measured: `SELECT ss.ss FROM ss LEFT JOIN h …` is the struct column,
// `COUNT(item.item) … GROUP BY` 42703, `COUNT(item)` the element.
func TestUnnamedDirectMatchAndBareElementName(t *testing.T) {
	t.Parallel()
	ss := ScopeSource{Table: &StaticTable{
		TableName: ParseQualifiedName("ss", false),
		TableColumns: []Column{
			{Id: NewUnquoted("id"), Type: "BIGINT"},
			{Id: NewUnquoted("ss"), Type: "RECORD", StructFields: []Column{{Id: NewUnquoted("ss"), Type: "BIGINT"}}},
		},
	}, Alias: NewUnquoted("ss"), CorrelationName: "SS"}
	checkPath(t, unnamedTestScope(t, true, ss), segs("ss", "ss"), wantResolves, "SS", 0, "unnamed: the attribute's direct match, not its field")
	checkPath(t, unnamedTestScope(t, false, ss), segs("ss", "ss"), wantResolves, "SS", 0, "named: the qualified reading first")

	element := Column{Id: NewUnquoted("item"), Type: "RECORD", Ephemeral: true, StructFields: []Column{{Id: NewUnquoted("b"), Type: "BIGINT"}}}
	record := ScopeSource{Table: &StaticTable{
		TableName:    ParseQualifiedName("item", false),
		TableColumns: []Column{element, {Id: NewUnquoted("b"), Type: "BIGINT"}},
	}, Alias: NewUnquoted("item"), CorrelationName: "ITEM", Shadowing: true, FlowedObject: &element}
	unnamed := unnamedTestScope(t, true, record)
	checkPath(t, unnamed, segs("item", "item"), wantNotFound, "", 0, "unnamed: the whole element is named ITEM, nothing qualifies it")
	checkPath(t, unnamed, segs("item", "b"), wantResolves, "B", 0, "unnamed: the member keeps its qualified name")
	if col, _, err := unnamed.ResolveColumn(NewUnquoted("item")); err != nil || col.Id.Name() != "ITEM" {
		t.Fatalf("unnamed bare item = %+v, %v; want the whole element", col, err)
	}
	checkPath(t, unnamedTestScope(t, false, record), segs("item", "item"), wantResolves, "ITEM", 0, "named: the operator's name qualifies it")
}
