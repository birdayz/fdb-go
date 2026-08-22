package recordlayer

import (
	"fmt"
	"testing"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
)

// primaryKeyComponentPositions IS A WIRE FIELD: it decides whether an index
// entry carries the record's primary key whole or with the components that
// already appear in the index key removed (Index.TrimPrimaryKey). Assigning it
// to one index more than Java does changes the bytes in FDB.
//
// Java assigns it at exactly one place -- RecordMetaDataBuilder.java:1465-1467,
// its only setPrimaryKeyComponentPositions call site in MAIN sources (the Java
// tree holds 8 more, all under src/test/, which is why "in the tree" was the
// wrong scope for this claim) -- inside
// `for (Index index : recordTypeBuilder.getIndexes())`. `getIndexes()` and
// `getMultiTypeIndexes()` are separate lists (RecordTypeIndexesBuilder.java:43
// and :45 -- an earlier cite read ":43-44", a range whose second line is an
// @Nonnull annotation and which excluded multiTypeIndexes, the field the whole
// argument rests on). `addMultiTypeIndex` routes by arity: zero names to
// `universalIndexes`, exactly one to `getIndexes()`, two or more to
// `getMultiTypeIndexes()`. So single-type registration is the only route that
// ends in a trimmed entry.
//
// Go assigned positions to all three. Both halves of that reached the wire, and
// the second is the worse one:
//
//   - Against Java: a multi-type index over two types keyed on the same field
//     trimmed the primary key away entirely, so Go wrote `(price)` where Java
//     writes `(price, pk)`.
//   - Against ITSELF: universal indexes took "the first record type's primary
//     key" by `break`ing out of a range over a MAP. Forty builds of one
//     metadata produced positions 33 times and nil 7 times, so two Go stores
//     opened from identical metadata could write index entries that disagree.
//
// Both are pinned below. The single-type arm is the control that keeps the
// other two honest: without it, a Build that stopped computing positions
// altogether would satisfy every negative assertion here.
func TestPositionsAreAssignedOnlyToSingleTypeIndexes(t *testing.T) {
	t.Parallel()

	// The index key overlaps the primary key in every arm, so positions WOULD
	// be computed for any arm Java computed them for. An arm that reported "no
	// positions" because the key simply did not overlap would prove nothing.
	for _, tc := range []struct {
		name string
		// register is the registration spelling under test.
		register func(b *RecordMetaDataBuilder, idx *Index)
		// wantPositions is Java's answer for this spelling.
		wantPositions bool
		// wantTrimmed is what TrimPrimaryKey returns for a primary key of [7].
		wantTrimmed tuple.Tuple
	}{
		{
			name:          "AddIndex, one record type",
			register:      func(b *RecordMetaDataBuilder, idx *Index) { b.AddIndex("Order", idx) },
			wantPositions: true,
			wantTrimmed:   tuple.Tuple{},
		},
		{
			// Java's addMultiTypeIndex sends a single name to getIndexes(), the
			// same list AddIndex feeds, so this arm must agree with the one
			// above. It is driven rather than inferred from the delegation.
			name:          "AddMultiTypeIndex, exactly one record type",
			register:      func(b *RecordMetaDataBuilder, idx *Index) { b.AddMultiTypeIndex([]string{"Order"}, idx) },
			wantPositions: true,
			wantTrimmed:   tuple.Tuple{},
		},
		{
			name: "AddMultiTypeIndex, two record types",
			register: func(b *RecordMetaDataBuilder, idx *Index) {
				b.AddMultiTypeIndex([]string{"Order", "Customer"}, idx)
			},
			wantPositions: false,
			wantTrimmed:   tuple.Tuple{int64(7)},
		},
		{
			name:          "AddUniversalIndex",
			register:      func(b *RecordMetaDataBuilder, idx *Index) { b.AddUniversalIndex(idx) },
			wantPositions: false,
			wantTrimmed:   tuple.Tuple{int64(7)},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			md, err := buildPriceKeyedMetaData(tc.register, "pos_idx")
			if err != nil {
				t.Fatalf("Build: %v", err)
			}
			idx := md.GetIndex("pos_idx")
			if idx == nil {
				t.Fatal("index missing from built metadata; the registration did nothing and every " +
					"assertion below would be about an absent object")
			}

			if got := idx.HasPrimaryKeyComponentPositions(); got != tc.wantPositions {
				t.Fatalf("HasPrimaryKeyComponentPositions() = %v, want %v.\n"+
					"Java assigns positions only through RecordMetaDataBuilder.java:1465-1467's loop over "+
					"getIndexes(); a divergence here changes the index entry key bytes.", got, tc.wantPositions)
			}

			// The consequence, asserted directly rather than through the flag:
			// this is the value that decides what goes into FDB.
			trimmed, err := idx.TrimPrimaryKey(tuple.Tuple{int64(7)})
			if err != nil {
				t.Fatalf("TrimPrimaryKey: %v", err)
			}
			if len(trimmed) != len(tc.wantTrimmed) {
				t.Fatalf("TrimPrimaryKey([7]) = %v (len %d), want %v (len %d)",
					trimmed, len(trimmed), tc.wantTrimmed, len(tc.wantTrimmed))
			}
			for i := range trimmed {
				if trimmed[i] != tc.wantTrimmed[i] {
					t.Fatalf("TrimPrimaryKey([7]) = %v, want %v", trimmed, tc.wantTrimmed)
				}
			}
		})
	}
}

// THE NON-DETERMINISM PIN, kept separate because it fails differently: the bug
// it watches for is not a wrong answer but an UNSTABLE one, and a single build
// samples it rather than detecting it.
//
// The record types are given DIFFERENT primary keys on purpose. That is what
// made the old `for … range b.recordTypes { …; break }` observable: with
// identical primary keys every choice agreed, and the map's iteration order
// stopped mattering. Measured on the code this replaced, 40 builds split 33/7.
func TestUniversalIndexPositionsDoNotDependOnMapIterationOrder(t *testing.T) {
	t.Parallel()

	const builds = 40
	seen := map[string]int{}
	for i := 0; i < builds; i++ {
		b := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		// Only `price` is declared by all three types, so it is the only legal
		// universal index key here -- and Order alone is keyed on it, which is
		// what gives the record types disagreeing answers to "does this index
		// key overlap your primary key?".
		b.GetRecordType("Order").SetPrimaryKey(Field("price"))
		b.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		b.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		b.AddUniversalIndex(NewIndex("uni_idx", Field("price")))

		md, err := b.Build()
		if err != nil {
			t.Fatalf("Build %d: %v", i, err)
		}
		idx := md.GetIndex("uni_idx")
		if idx == nil {
			t.Fatalf("build %d: universal index missing; this loop would then be measuring nothing", i)
		}
		seen[fmt.Sprint(idx.primaryKeyComponentPositions)]++
	}

	if len(seen) != 1 {
		t.Fatalf("%d builds of ONE metadata produced %d different primaryKeyComponentPositions: %v.\n"+
			"Build is choosing a record type by iterating a map, so the index entry key depends on "+
			"map order and two stores opened from identical metadata can disagree with each other.",
			builds, len(seen), seen)
	}
	// Which single value it settles on is Java's answer, asserted separately so
	// that a deterministic-but-wrong result cannot pass as a deterministic one.
	if _, ok := seen["[]"]; !ok {
		t.Fatalf("universal index positions settled on %v, want no positions at all: Java never "+
			"assigns them to a universal index", seen)
	}
}

// buildPriceKeyedMetaData registers one index on a metadata whose record types
// are ALL keyed on `price` -- the only field the demo proto declares on all
// three -- so that a universal index key can overlap every primary key and the
// arms differ in registration alone.
func buildPriceKeyedMetaData(register func(*RecordMetaDataBuilder, *Index), indexName string) (*RecordMetaData, error) {
	b := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
	b.GetRecordType("Order").SetPrimaryKey(Field("price"))
	b.GetRecordType("Customer").SetPrimaryKey(Field("price"))
	b.GetRecordType("TypedRecord").SetPrimaryKey(Field("price"))
	register(b, NewIndex(indexName, Field("price")))
	return b.Build()
}

// THE UPGRADE HAZARD, measured rather than reasoned about.
//
// positions are derived at every Build and never persisted -- `grep -rn
// primary_key_component_positions` over Java's *.proto tree returns nothing
// (positive control: `last_modified_version` returns hits) -- so an index whose
// positions changed has no on-disk discriminator. Entries written by an older Go
// for a multi-type or universal index are TRIMMED; this build writes them whole
// and reads them with nil positions.
//
// What that does to a trimmed entry is the part worth pinning: it does not
// error, it returns an EMPTY primary key. For an index whose key IS the primary
// key, colSize equals the trimmed entry's length, so the `colSize < len` guard
// is false and the empty tuple is returned. A scan over pre-upgrade data
// therefore yields rows with no primary key rather than a failure, and the
// metadata-evolution validator cannot see it either: it compares two BUILT
// metadata objects, and after the upgrade both sides derive nil.
//
// The remedy is operational, not a version gate -- there is nothing on disk to
// gate on, and the old format was itself nondeterministic for universal indexes.
// Bump the affected index's lastModifiedVersion, which IS persisted and does
// trigger a rebuild.
func TestPreUpgradeTrimmedEntryReadsBackWithAnEmptyPrimaryKey(t *testing.T) {
	t.Parallel()

	// A universal index on `price` over record types keyed on `price`: exactly
	// the shape whose entries an older Go trimmed.
	md, err := buildPriceKeyedMetaData(
		func(b *RecordMetaDataBuilder, idx *Index) { b.AddUniversalIndex(idx) }, "uni_idx")
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	idx := md.GetIndex("uni_idx")
	if idx.HasPrimaryKeyComponentPositions() {
		t.Fatal("this build assigned positions to a universal index; the premise of this test is gone " +
			"and the multi-type/universal fix has been reverted")
	}

	// What this build writes: (price, pk).
	current := idx.getEntryPrimaryKey(tuple.Tuple{int64(100), int64(100)})
	if len(current) != 1 || current[0] != int64(100) {
		t.Fatalf("an entry written by THIS build reads back with primary key %v, want [100]", current)
	}

	// What an older Go wrote: (price) alone, the primary key trimmed away.
	legacy := idx.getEntryPrimaryKey(tuple.Tuple{int64(100)})
	if len(legacy) != 0 {
		t.Fatalf("a pre-upgrade trimmed entry now reads back with primary key %v.\n"+
			"That is BETTER than the empty tuple this pins, so the hazard has changed shape: "+
			"re-read the migration paragraph in DIVERGENCES.md before relaxing anything, because "+
			"it tells operators the failure is silent.", legacy)
	}
}
