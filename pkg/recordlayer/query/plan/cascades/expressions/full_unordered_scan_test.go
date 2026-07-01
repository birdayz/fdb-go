package expressions

import (
	"reflect"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

func TestFullUnorderedScan_Construction(t *testing.T) {
	t.Parallel()
	s := NewFullUnorderedScanExpression([]string{"Order", "Customer", "Order"}, values.UnknownType)
	want := []string{"Customer", "Order"}
	if got := s.GetRecordTypes(); !reflect.DeepEqual(got, want) {
		t.Fatalf("recordTypes=%v, want %v (sorted+deduped)", got, want)
	}
	if s.GetFlowedType() != values.UnknownType {
		t.Fatal("flowed type not preserved")
	}
	if s.CanCorrelate() {
		t.Fatal("scan should not anchor a correlation")
	}
	if got := s.GetQuantifiers(); got != nil {
		t.Fatalf("scan has quantifiers: %v", got)
	}
}

func TestFullUnorderedScan_NilFlowedType(t *testing.T) {
	t.Parallel()
	s := NewFullUnorderedScanExpression([]string{"Order"}, nil)
	if s.GetFlowedType() != values.UnknownType {
		t.Fatal("nil flowedType not normalised to UnknownType")
	}
}

// TestFullUnorderedScan_GetResultValueFlowsType_RFC173Slice1 pins the latent bug
// Graefe caught in RFC-173 Slice 1: GetResultValue hard-coded
// NewQuantifiedObjectValue → UnknownType, silently DISCARDING the scan's own
// flowedType. That forced every single-table scan onto name resolution because
// FieldValue.resolveOrdinal's `f.Child.Type().(*RecordType)` assertion failed on
// the UnknownType QOV → (0,false). Java's scan quantifier always flows the record
// type; ordinal resolution can only fire once the QOV carries it.
func TestFullUnorderedScan_GetResultValueFlowsType_RFC173Slice1(t *testing.T) {
	t.Parallel()
	mkType := func() *values.RecordType {
		return values.NewRecordType("", false, []values.Field{
			{Name: "ID", FieldType: values.UnknownType, Ordinal: 0},
			{Name: "V", FieldType: values.UnknownType, Ordinal: 1},
		})
	}
	scan := NewFullUnorderedScanExpression([]string{"T"}, mkType())

	rv := scan.GetResultValue()
	qov, ok := rv.(*values.QuantifiedObjectValue)
	if !ok {
		t.Fatalf("GetResultValue=%T, want *QuantifiedObjectValue", rv)
	}
	got, ok := qov.Type().(*values.RecordType)
	if !ok {
		t.Fatalf("scan result value Type=%T, want *RecordType (flowedType was discarded — the latent bug)", qov.Type())
	}
	if len(got.Fields) != 2 || got.Fields[0].Name != "ID" || got.Fields[1].Name != "V" {
		t.Fatalf("flowed record type fields=%v, want [ID V] in order", got.Fields)
	}

	// An UnknownType/nil flowedType still degrades to name resolution — untyped
	// seeds (no metadata) must NOT flow a *RecordType.
	untyped := NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	if _, ok := untyped.GetResultValue().Type().(*values.RecordType); ok {
		t.Fatal("UnknownType scan must not flow a *RecordType — untyped seeds stay on the name path")
	}

	// Trap 2 (Graefe): two scans of one table build structurally-equal RecordTypes,
	// so EqualsWithoutChildren (which compares flowedType) still dedups them in the
	// memo — no pointer cache needed because RecordType.Equals is structural.
	scanTwin := NewFullUnorderedScanExpression([]string{"T"}, mkType())
	if !scan.EqualsWithoutChildren(scanTwin, EmptyAliasMap()) {
		t.Fatal("two scans of one table with structurally-equal flowedType must dedup (EqualsWithoutChildren)")
	}
}

// TestFullUnorderedScan_FlowedTypeWildcard_RFC173Slice1 pins Fork B (Graefe's
// ruling): the scan-leaf flowedType is non-discriminating when either side is
// UnknownType, matching Java's AnyRecord-on-both-leaves invariant. A typed query
// scan (RFC-173 Slice 1) must still subsume/dedup against an UnknownType
// candidate scan over the same record types — names discriminate, not the leaf
// type — and they must hash identically so they share a memo bucket. Two
// CONCRETE, structurally-different types over the same names stay distinct
// (query-side dedup preserved).
func TestFullUnorderedScan_FlowedTypeWildcard_RFC173Slice1(t *testing.T) {
	t.Parallel()
	typed := func() values.Type {
		return values.NewRecordType("", false, []values.Field{
			{Name: "ID", FieldType: values.UnknownType, Ordinal: 0},
			{Name: "V", FieldType: values.UnknownType, Ordinal: 1},
		})
	}

	typedScan := NewFullUnorderedScanExpression([]string{"T"}, typed())
	untypedScan := NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)

	// Wildcard match: typed query scan vs UnknownType candidate scan (same names).
	if !typedScan.EqualsWithoutChildren(untypedScan, EmptyAliasMap()) {
		t.Fatal("typed scan must match UnknownType scan over the same record types (Fork B wildcard)")
	}
	if !untypedScan.EqualsWithoutChildren(typedScan, EmptyAliasMap()) {
		t.Fatal("wildcard match must be symmetric")
	}
	// Same bucket: hash must be names-only so the wildcard match can even fire.
	if typedScan.HashCodeWithoutChildren() != untypedScan.HashCodeWithoutChildren() {
		t.Fatal("typed and untyped scans over the same types must hash identically (names-only hash)")
	}

	// Different record-type NAMES never match, regardless of leaf type.
	if typedScan.EqualsWithoutChildren(
		NewFullUnorderedScanExpression([]string{"U"}, values.UnknownType), EmptyAliasMap()) {
		t.Fatal("scans over different record types must not match even when one is UnknownType")
	}

	// Two CONCRETE structurally-different types over the same names stay distinct
	// (both non-UnknownType → structural compare → query-side dedup preserved).
	otherType := values.NewRecordType("", false, []values.Field{
		{Name: "ID", FieldType: values.UnknownType, Ordinal: 0},
	})
	if typedScan.EqualsWithoutChildren(
		NewFullUnorderedScanExpression([]string{"T"}, otherType), EmptyAliasMap()) {
		t.Fatal("two concrete, structurally-different scan types must NOT match (structural dedup)")
	}
}

func TestFullUnorderedScan_EqualsWithoutChildren(t *testing.T) {
	t.Parallel()
	s1 := NewFullUnorderedScanExpression([]string{"Order", "Customer"}, values.UnknownType)
	s1Twin := NewFullUnorderedScanExpression([]string{"Customer", "Order"}, values.UnknownType)
	s2 := NewFullUnorderedScanExpression([]string{"Order"}, values.UnknownType)
	if !s1.EqualsWithoutChildren(s1Twin, EmptyAliasMap()) {
		t.Fatal("permuted-equal scans reported unequal")
	}
	if s1.EqualsWithoutChildren(s2, EmptyAliasMap()) {
		t.Fatal("subset reported equal")
	}
}

func TestFullUnorderedScan_HashCodeStable(t *testing.T) {
	t.Parallel()
	s1 := NewFullUnorderedScanExpression([]string{"Order", "Customer"}, values.UnknownType)
	s1Twin := NewFullUnorderedScanExpression([]string{"Customer", "Order"}, values.UnknownType)
	s2 := NewFullUnorderedScanExpression([]string{"Customer"}, values.UnknownType)
	if s1.HashCodeWithoutChildren() != s1Twin.HashCodeWithoutChildren() {
		t.Fatal("permuted-equal scans produced different hashes")
	}
	if s1.HashCodeWithoutChildren() == s2.HashCodeWithoutChildren() {
		t.Fatal("disjoint scans produced identical hashes (collision unlikely)")
	}
}

// TestRealExpressionTree builds an actual (Scan → Filter → Projection)
// tree using the real FullUnorderedScanExpression as the leaf — proves
// the seed types compose without the test-only leafScan stub.
func TestRealExpressionTree(t *testing.T) {
	t.Parallel()
	scan := NewFullUnorderedScanExpression([]string{"Order"}, values.UnknownType)
	scanRef := InitialOf(scan)
	scanQ := ForEachQuantifier(scanRef)

	filter := NewLogicalFilterExpression(
		[]predicates.QueryPredicate{predicates.NewConstantPredicate(predicates.TriTrue)},
		scanQ,
	)
	filterRef := InitialOf(filter)
	filterQ := ForEachQuantifier(filterRef)

	proj := NewLogicalProjectionExpression(
		[]values.Value{values.NewBooleanValue(true)},
		filterQ,
	)

	// Walk the tree: Projection → Filter → Scan
	if len(proj.GetQuantifiers()) != 1 {
		t.Fatal("projection should have 1 quantifier")
	}
	pInner := proj.GetQuantifiers()[0].GetRangesOver().Get()
	if _, ok := pInner.(*LogicalFilterExpression); !ok {
		t.Fatalf("projection inner=%T, want *LogicalFilterExpression", pInner)
	}
	fInner := pInner.GetQuantifiers()[0].GetRangesOver().Get()
	if _, ok := fInner.(*FullUnorderedScanExpression); !ok {
		t.Fatalf("filter inner=%T, want *FullUnorderedScanExpression", fInner)
	}
	if len(fInner.GetQuantifiers()) != 0 {
		t.Fatalf("scan has %d quantifiers, want 0", len(fInner.GetQuantifiers()))
	}
}

// TestSemanticEquals_FullTree verifies SemanticEquals walks all the
// way to leaves through real RelationalExpressions.
func TestSemanticEquals_FullTree(t *testing.T) {
	t.Parallel()
	build := func() RelationalExpression {
		scan := NewFullUnorderedScanExpression([]string{"Order"}, values.UnknownType)
		scanQ := ForEachQuantifier(InitialOf(scan))
		return NewLogicalFilterExpression(
			[]predicates.QueryPredicate{predicates.NewConstantPredicate(predicates.TriTrue)},
			scanQ,
		)
	}
	a := build()
	b := build()
	if !SemanticEquals(a, b, EmptyAliasMap()) {
		t.Fatal("two identically-built (filter over scan) trees reported semantically unequal")
	}
}

// TestSemanticEquals_DifferentLeaf walks the tree and detects a
// difference at the leaf level even when the operator chain is
// identical.
func TestSemanticEquals_DifferentLeaf(t *testing.T) {
	t.Parallel()
	build := func(recordType string) RelationalExpression {
		scan := NewFullUnorderedScanExpression([]string{recordType}, values.UnknownType)
		scanQ := ForEachQuantifier(InitialOf(scan))
		return NewLogicalFilterExpression(
			[]predicates.QueryPredicate{predicates.NewConstantPredicate(predicates.TriTrue)},
			scanQ,
		)
	}
	a := build("Order")
	b := build("Customer")
	if SemanticEquals(a, b, EmptyAliasMap()) {
		t.Fatal("tree comparison didn't propagate down to leaf — disjoint scans reported semantically equal")
	}
}
