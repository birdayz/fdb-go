package recordlayer

import (
	"testing"

	"fdb.dev/gen"
	"google.golang.org/protobuf/proto"
)

// Java's Index(proto) constructor has THREE compatibility behaviours for
// metadata somebody else already wrote, and they sit in eleven consecutive lines
// (Index.java:195-215): the deprecated `index_type` enum, the VALUE default on
// `hasType()`, and a grouping wrap for aggregate types whose serialized root
// expression is not a GroupingKeyExpression.
//
// They are tested together because they FAIL TOGETHER, in the same direction.
// Each one is a case where the reader must supply something the bytes do not
// carry, and a reader that skips it does not error — it produces a plausible
// index of the wrong kind. That is the same shape as the maintainer dispatch's
// old default arm, which is why closing that arm is what made these reachable.

// bareTypeRoundTrip serializes real metadata, rewrites one index's proto the way
// an older or Java-side writer would have, and reads it back through the actual
// load path (RecordMetaDataFromProto -> ... -> Build, validators included).
func bareTypeRoundTrip(t *testing.T, mutate func(*gen.Index)) (*RecordMetaData, error) {
	t.Helper()
	b := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
	b.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
	b.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
	b.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
	b.AddIndex("Order", NewIndex("Order$compat", Field("price")))
	md, err := b.Build()
	if err != nil {
		t.Fatalf("baseline build: %v", err)
	}
	p, err := md.ToProto()
	if err != nil {
		t.Fatalf("ToProto: %v", err)
	}
	found := false
	for _, ip := range p.Indexes {
		if ip.GetName() == "Order$compat" {
			mutate(ip)
			found = true
		}
	}
	if !found {
		t.Fatal("Order$compat missing from the serialized metadata — fixture is wrong")
	}
	return RecordMetaDataFromProto(p)
}

// TestIndexFromProto_GroupingFixupForOldMetaData pins Java's grouping wrap.
//
// This is the regression that canonicalizing the grouping validator introduces
// if the wrap is not ported, and it lands on exactly the metadata the bare
// _EVER spellings exist to read. Before canonicalization a bare `min_ever`
// missed every case label in the validator and loaded (then reached the wrong
// maintainer). After it, the validator matches — so without Java's wrap, Go
// REFUSES metadata Java opens by repairing.
func TestIndexFromProto_GroupingFixupForOldMetaData(t *testing.T) {
	t.Parallel()

	// Java wraps for exactly these five, comparing the RAW type string, which is
	// why the list carries the bare _EVER spellings and not the canonical ones.
	for _, tc := range []struct {
		indexType string
		wrapped   bool
		why       string
	}{
		{IndexTypeMinEver, true, "bare min_ever is in Java's list at Index.java:209"},
		{IndexTypeMaxEver, true, "bare max_ever is in Java's list at Index.java:208"},
		{IndexTypeSum, true, "sum is in Java's list"},
		{IndexTypeCount, true, "count is in Java's list, with getColumnSize() as the grouped count"},
		{IndexTypeRank, true, "rank is in Java's list"},

		// The negatives carry the whole design. Java compares the raw string, so
		// the CANONICAL spellings are absent from its list on purpose: the wrap
		// repairs old metadata that used the old names, and must not paper over
		// new metadata that is simply malformed.
		{IndexTypeMinEverLong, false, "min_ever_long is NOT in Java's list — new spelling, no repair"},
		{IndexTypeMaxEverLong, false, "max_ever_long is NOT in Java's list"},
		{IndexTypeMinEverTuple, false, "min_ever_tuple is NOT in Java's list"},
		{IndexTypeValue, false, "a value index has no grouping at all"},
	} {
		t.Run(tc.indexType, func(t *testing.T) {
			t.Parallel()
			md, err := bareTypeRoundTrip(t, func(ip *gen.Index) {
				ip.Type = proto.String(tc.indexType)
			})
			if !tc.wrapped {
				// Unwrapped aggregate types must still be REFUSED, not repaired.
				if tc.indexType != IndexTypeValue {
					if err == nil {
						t.Fatalf("%s: load succeeded, want rejection. %s — repairing it here would "+
							"accept malformed new metadata under a compatibility rule that exists "+
							"only for old metadata", tc.indexType, tc.why)
					}
					return
				}
				if err != nil {
					t.Fatalf("value index load: %v", err)
				}
				if _, isGrouping := md.GetIndex("Order$compat").RootExpression.(*GroupingKeyExpression); isGrouping {
					t.Fatalf("%s: root expression was wrapped. %s", tc.indexType, tc.why)
				}
				return
			}

			if err != nil {
				t.Fatalf("%s: load FAILED for metadata Java opens: %v. %s", tc.indexType, err, tc.why)
			}
			gke, ok := md.GetIndex("Order$compat").RootExpression.(*GroupingKeyExpression)
			if !ok {
				t.Fatalf("%s: root expression is %T, want *GroupingKeyExpression. %s",
					tc.indexType, md.GetIndex("Order$compat").RootExpression, tc.why)
			}
			// Java: `new GroupingKeyExpression(expr, type.equals(COUNT) ?
			// expr.getColumnSize() : 1)`. Field("price") has column size 1, so
			// COUNT groups nothing and the others aggregate one column — the two
			// happen to coincide at width 1, and the count arm is pinned on its
			// own below where the widths differ.
			want := 1
			if tc.indexType == IndexTypeCount {
				want = 1 // getColumnSize() of Field("price")
			}
			if got := gke.GetGroupedCount(); got != want {
				t.Fatalf("%s: grouped count %d, want %d", tc.indexType, got, want)
			}
		})
	}
}

// TestIndexFromProto_GroupingFixupCountUsesColumnSize separates COUNT's arm from
// the others at a width where they DISAGREE.
//
// At column size 1 both arms produce 1, so the whole table above would pass with
// the ternary collapsed to a constant. The defect only becomes expressible on a
// multi-column root: Java gives COUNT `getColumnSize()` (group by nothing, count
// everything) and everything else 1.
func TestIndexFromProto_GroupingFixupCountUsesColumnSize(t *testing.T) {
	t.Parallel()

	twoCols := Concat(Field("price"), Field("quantity"))
	if twoCols.ColumnSize() != 2 {
		t.Fatalf("fixture: column size %d, want 2", twoCols.ColumnSize())
	}

	countWrapped := groupingFixupForOldMetaData(IndexTypeCount, twoCols)
	gke, ok := countWrapped.(*GroupingKeyExpression)
	if !ok {
		t.Fatalf("count: %T, want *GroupingKeyExpression", countWrapped)
	}
	if got := gke.GetGroupedCount(); got != 2 {
		t.Fatalf("count grouped count = %d, want 2 (Java: expr.getColumnSize()) — a constant 1 "+
			"here means COUNT would group by the first column instead of counting every record", got)
	}

	sumWrapped := groupingFixupForOldMetaData(IndexTypeSum, twoCols)
	sgke, ok := sumWrapped.(*GroupingKeyExpression)
	if !ok {
		t.Fatalf("sum: %T, want *GroupingKeyExpression", sumWrapped)
	}
	if got := sgke.GetGroupedCount(); got != 1 {
		t.Fatalf("sum grouped count = %d, want 1 (Java's non-COUNT arm)", got)
	}
}

// TestIndexFromProto_GroupingFixupLeavesGroupedRootsAlone pins Java's guard
// clause: the wrap only fires when the root is NOT already a grouping
// expression. Re-wrapping a correct index would change its key layout.
func TestIndexFromProto_GroupingFixupLeavesGroupedRootsAlone(t *testing.T) {
	t.Parallel()

	already := GroupBy(Field("price"), Field("customer_id"))
	got := groupingFixupForOldMetaData(IndexTypeSum, already)
	if got != KeyExpression(already) {
		t.Fatalf("an already-grouping root was replaced (%T) — Java's guard is "+
			"`!(expr instanceof GroupingKeyExpression)`, and re-wrapping changes the index key layout", got)
	}
}

// TestIndexFromProto_DeprecatedIndexTypeEnum pins Java's FIRST compatibility
// branch, which Go ignored entirely.
//
// `index_type` is where a legacy index's uniqueness lives — it is in the ENUM
// and nowhere else, never in the options list. A reader that only consults
// `type` gets a plain value index: the constraint disappears silently and the
// store accepts duplicates Java rejects. RANK/RANK_UNIQUE are worse, because
// with `type` absent the index defaults to VALUE and is then MAINTAINED as one,
// which is the wrong key format in the index's own subspace.
func TestIndexFromProto_DeprecatedIndexTypeEnum(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name       string
		enum       gen.Index_Type
		wantType   string
		wantUnique bool
	}{
		{"INDEX", gen.Index_INDEX, IndexTypeValue, false},
		{"UNIQUE", gen.Index_UNIQUE, IndexTypeValue, true},
		{"RANK", gen.Index_RANK, IndexTypeRank, false},
		{"RANK_UNIQUE", gen.Index_RANK_UNIQUE, IndexTypeRank, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			idx, err := indexFromProto(&gen.Index{
				Name:      proto.String("Order$legacy"),
				IndexType: tc.enum.Enum(),
			})
			if err != nil {
				t.Fatalf("indexFromProto: %v", err)
			}
			if idx.Type != tc.wantType {
				t.Fatalf("type = %q, want %q (Java Index.indexTypeToType, Index.java:269-279)",
					idx.Type, tc.wantType)
			}
			if got := idx.IsUnique(); got != tc.wantUnique {
				t.Fatalf("IsUnique() = %t, want %t (Java Index.indexTypeToOptions, "+
					"Index.java:281-291). A legacy index carries its uniqueness in the enum "+
					"alone — losing it here permits the duplicates Java rejects",
					got, tc.wantUnique)
			}
		})
	}
}

// TestIndexFromProto_DeprecatedIndexTypeWinsOverTypeAndOptions pins the branch
// STRUCTURE, not just the mapping.
//
// Java's `if (proto.hasIndexType())` takes type AND options from the enum and
// never reads `type` or `getOptionsList()`. A port that merged the two — reading
// the enum but still layering the options list on top, or letting `type` win —
// would pass the mapping test above and still diverge on any metadata carrying
// both, which is exactly what a partially-migrated writer produces.
func TestIndexFromProto_DeprecatedIndexTypeWinsOverTypeAndOptions(t *testing.T) {
	t.Parallel()

	idx, err := indexFromProto(&gen.Index{
		Name:      proto.String("Order$both"),
		IndexType: gen.Index_UNIQUE.Enum(),
		Type:      proto.String(IndexTypeSum), // must be IGNORED
		Options: []*gen.Index_Option{{
			Key:   proto.String("some_option"),
			Value: proto.String("from_the_list"),
		}},
	})
	if err != nil {
		t.Fatalf("indexFromProto: %v", err)
	}
	if idx.Type != IndexTypeValue {
		t.Fatalf("type = %q, want %q — `type` must not win over a present `index_type` "+
			"(Java takes the hasIndexType() branch and never reads it)", idx.Type, IndexTypeValue)
	}
	if !idx.IsUnique() {
		t.Fatal("uniqueness from the enum was lost")
	}
	if v, ok := idx.Options["some_option"]; ok {
		t.Fatalf("options list was read on the legacy branch (some_option=%q). Java uses "+
			"indexTypeToOptions ALONE there and never calls buildOptions(getOptionsList())", v)
	}
}
