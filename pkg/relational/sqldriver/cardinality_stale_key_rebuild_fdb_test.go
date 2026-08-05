package sqldriver_test

// CQ-90 — the on-disk migration for the CQ-89 cardinality key change, proven
// end to end.
//
// CQ-89 changed the CARDINALITY() index key for an EMPTY non-nullable (flat
// repeated) array from a NULL key to the integer key 0, because Java has no
// zero special-case on either arm (CardinalityFunctionKeyExpression.java:115-117
// returns Key.Evaluated.scalar(getRepeatedFieldCount(...)) unconditionally).
// Measured, the entry key moved from `indexSubspace ‖ 0x00 ‖ pk` to
// `indexSubspace ‖ 0x14 ‖ pk`.
//
// NOTHING REWRITES THE OLD ENTRIES. A record written by a Go binary predating
// that commit keeps its 0x00 entry forever, so an index-backed
// `CARDINALITY(a) = 0` MISSES rows a full scan returns. The tests below
// reproduce exactly that state at the wire level and then prove the migration
// that clears it.
//
// THE MIGRATION, AND WHY IT IS THE SCHEMA OWNER'S TO PERFORM. Java has NO
// core-side mechanism that force-rebuilds an index because the library's own
// key derivation changed between releases. The rebuild trigger is purely
// meta-data-driven: checkVersion selects candidates from
// getIndexesToBuildSince(oldMetaDataVersion), which compares each index's
// lastModifiedVersion against the version in the store header. FormatVersion
// says so in as many words while contrasting the record-count key, which IS
// store-header-detected (FormatVersion.java:65-66):
//
//	"Unlike indexes, the RecordCountKey does not have a lastModifiedVersion,
//	 and thus the store detects that the counts need to be rebuilt by checking
//	 the key in the StoreHeader."
//
// The contract that IS documented is aimed at schema authors, on
// Index.getLastModifiedVersion (Index.java:614-619): "Any record store older
// than this will need to have the index rebuilt." So the engine-side
// deliverable is not an automatic bump — inventing one would make Go emit
// metadata bytes Java's builder never produces, which is a wire divergence in
// the direction CQ-89 set out to remove. It is this: the rebuild path proven
// correct for exactly this migration, on both of its arms.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/metadata"
)

// The two key encodings the migration moves between. Spelled through the repo's
// own tuple codec and then asserted against the literal FDB tuple codes in
// TestFDB_CardinalityStaleKeyBytes, because those codes are the wire contract.
var (
	cq90NullKey = tuple.Tuple{nil}.Pack()      // 0x00 — what pre-CQ-89 Go wrote
	cq90ZeroKey = tuple.Tuple{int64(0)}.Pack() // 0x14 — what Java always wrote
)

// cq90MetaData builds the affected shape: one table with a NOT NULL array
// column and a CARDINALITY() index over it.
//
// withCountIndex adds a COUNT index grouped by nothing — NewCountIndex(
// GroupAll(EmptyKey())), which is precisely the function
// snapshotTotalRecordCount looks for. It is load-bearing, not decoration: see
// the comment on TestFDB_CardinalityStaleNullKeyRebuiltInline.
func cq90MetaData(t *testing.T, name string, withCountIndex bool) *recordlayer.RecordMetaData {
	t.Helper()
	b := metadata.NewSchemaTemplateBuilder().SetName(name)
	b.AddTable("T_STALE", []metadata.ColumnSpec{
		metadata.NewColumnSpec("ID", api.NewIntegerType(false), 1),
		metadata.NewColumnSpec("INT_ARR", api.NewArrayType(api.NewIntegerType(false), false), 2),
	}, []string{"ID"})
	b.AddCardinalityIndex("T_STALE", "T_STALE_CARD", "INT_ARR")
	if withCountIndex {
		b.AddAggregateIndex("T_STALE", "T_STALE_CNT", []string{}, "COUNT", "")
	}
	tmpl, err := b.Build()
	if err != nil {
		t.Fatalf("build schema %s: %v", name, err)
	}
	return tmpl.Underlying()
}

// cq90BumpIndexVersion re-authors md with ONE index's LastModifiedVersion
// raised past the metadata version, and the metadata version raised to match.
// This is THE production recipe, and it is the schema-owner action Java
// documents on Index.getLastModifiedVersion — the only trigger the core
// consults.
//
// Two details are deliberate and each is load-bearing:
//
//   - AddedVersion is left ALONE. MetaDataEvolutionValidator requires it to be
//     unchanged (metadata_evolution_validator.go:482-487; Java
//     MetaDataEvolutionValidator.java:633-637, the `oldIndex.getAddedVersion()
//     != newIndex.getAddedVersion()` check in validateIndex — NOT :543-546,
//     which is the separate new-index "version not newer than the old meta-data
//     version" branch), and only LastModifiedVersion drives the rebuild.
//     Bumping both would make the evolution illegal for no gain.
//   - Exactly ONE index is bumped. Every index named in indexesToBuild is
//     EXCLUDED from the record-count sources that decide the rebuild policy
//     (store_builder.go's `excluded` map; Java's IndexQueryabilityFilter at
//     FDBRecordStore.java:4841), because an index being built holds no entries
//     and would report 0 for a full store. Bump the whole schema and the COUNT
//     index goes with it, the count degrades to MaxInt64, and every index lands
//     DISABLED regardless of how small the store is.
func cq90BumpIndexVersion(t *testing.T, md *recordlayer.RecordMetaData, indexName string) *recordlayer.RecordMetaData {
	t.Helper()
	p, err := md.ToProto()
	if err != nil {
		t.Fatalf("metadata to proto: %v", err)
	}
	next := int32(md.Version() + 1)
	found := false
	for _, idx := range p.GetIndexes() {
		if idx.GetName() == indexName {
			idx.LastModifiedVersion = proto.Int32(next)
			found = true
		}
	}
	if !found {
		t.Fatalf("index %q not in metadata proto — the bump would be a no-op", indexName)
	}
	p.Version = proto.Int32(next)
	out, err := recordlayer.RecordMetaDataFromProto(p)
	if err != nil {
		t.Fatalf("metadata from proto: %v", err)
	}
	bumped := out.GetIndex(indexName)
	if bumped == nil || bumped.LastModifiedVersion != int(next) {
		t.Fatalf("bump did not survive the proto round-trip: index %q LastModifiedVersion is %v, want %d",
			indexName, bumped, next)
	}
	return out
}

// cq90Rec builds a T_STALE record. A nil arr is an EMPTY array, not a NULL one:
// the column is NOT NULL and a flat repeated field with no elements is [].
func cq90Rec(desc protoreflect.MessageDescriptor, id int32, arr []int32) proto.Message {
	m := dynamicpb.NewMessage(desc)
	m.Set(desc.Fields().ByName("ID"), protoreflect.ValueOfInt32(id))
	fd := desc.Fields().ByName("INT_ARR")
	pvals := make([]protoreflect.Value, len(arr))
	for i, v := range arr {
		pvals[i] = protoreflect.ValueOfInt32(v)
	}
	setArrayField(m, fd, pvals...)
	return m
}

// cq90Run opens a store over ks with md and runs fn against it.
func cq90Run(t *testing.T, db *recordlayer.FDBDatabase, md *recordlayer.RecordMetaData,
	ks subspace.Subspace, create bool, fn func(store *recordlayer.FDBRecordStore) error,
) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	_, err := db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		sb := recordlayer.NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks)
		var store *recordlayer.FDBRecordStore
		var oErr error
		if create {
			store, oErr = sb.CreateOrOpen()
		} else {
			store, oErr = sb.Open()
		}
		if oErr != nil {
			return nil, oErr
		}
		return nil, fn(store)
	})
	return err
}

// cq90IndexKeySuffixes returns every key in indexName's subspace with the
// subspace prefix stripped, in FDB byte order.
func cq90IndexKeySuffixes(t *testing.T, db *recordlayer.FDBDatabase, md *recordlayer.RecordMetaData,
	ks subspace.Subspace, indexName string,
) [][]byte {
	t.Helper()
	var out [][]byte
	err := cq90Run(t, db, md, ks, false, func(store *recordlayer.FDBRecordStore) error {
		idx := md.GetIndex(indexName)
		if idx == nil {
			return fmt.Errorf("index %q not in metadata", indexName)
		}
		prefix := store.IndexSubspace(idx).Bytes()
		kvs, rErr := store.Context().Transaction().GetRange(fdb.KeyRange{
			Begin: fdb.Key(prefix),
			End:   fdb.Key(append(append([]byte{}, prefix...), 0xFF)),
		}, fdb.RangeOptions{}).GetSliceWithError()
		if rErr != nil {
			return fmt.Errorf("scan %s: %w", indexName, rErr)
		}
		for _, kv := range kvs {
			out = append(out, append([]byte{}, kv.Key[len(prefix):]...))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("read index %s: %v", indexName, err)
	}
	return out
}

// cq90Staleize rewrites every cardinality-0 entry of indexName into the
// PRE-CQ-89 on-disk shape: the same entry keyed NULL (0x00) instead of the
// integer 0 (0x14), value untouched.
//
// This is not an approximation of an old store — it is byte-for-byte what a Go
// binary predating CQ-89 wrote, because the only thing that commit changed on
// the write path was cardinalityCountRepeated returning nilKeyResult for a zero
// count. Everything else about the entry (subspace, trimmed primary key,
// empty-tuple value) is unchanged, so the state left behind here is
// indistinguishable from one that binary produced.
//
// Returns the number of entries rewritten; 0 means the test is asserting
// nothing and callers must treat it as a failure.
func cq90Staleize(t *testing.T, db *recordlayer.FDBDatabase, md *recordlayer.RecordMetaData,
	ks subspace.Subspace, indexName string,
) int {
	t.Helper()
	n := 0
	err := cq90Run(t, db, md, ks, false, func(store *recordlayer.FDBRecordStore) error {
		idx := md.GetIndex(indexName)
		if idx == nil {
			return fmt.Errorf("index %q not in metadata", indexName)
		}
		tx := store.Context().Transaction()
		prefix := store.IndexSubspace(idx).Bytes()
		kvs, rErr := tx.GetRange(fdb.KeyRange{
			Begin: fdb.Key(prefix),
			End:   fdb.Key(append(append([]byte{}, prefix...), 0xFF)),
		}, fdb.RangeOptions{}).GetSliceWithError()
		if rErr != nil {
			return fmt.Errorf("scan %s: %w", indexName, rErr)
		}
		for _, kv := range kvs {
			suffix := kv.Key[len(prefix):]
			if !bytes.HasPrefix(suffix, cq90ZeroKey) {
				continue
			}
			stale := append(append([]byte{}, prefix...), cq90NullKey...)
			stale = append(stale, suffix[len(cq90ZeroKey):]...)
			tx.Clear(fdb.Key(kv.Key))
			tx.Set(fdb.Key(stale), kv.Value)
			n++
		}
		return nil
	})
	if err != nil {
		t.Fatalf("staleize %s: %v", indexName, err)
	}
	return n
}

// cq90IndexedIDs runs an index-backed scan of indexName restricted to key
// `card` and returns the IDs it yields, in index order. This is the record
// layer's half of an index-backed `CARDINALITY(a) = card`.
func cq90IndexedIDs(t *testing.T, db *recordlayer.FDBDatabase, md *recordlayer.RecordMetaData,
	ks subspace.Subspace, indexName string, card int64,
) []int32 {
	t.Helper()
	var out []int32
	err := cq90Run(t, db, md, ks, false, func(store *recordlayer.FDBRecordStore) error {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		recs, lErr := recordlayer.AsList(ctx, store.ScanIndexRecords(
			indexName, recordlayer.TupleRangeAllOf(tuple.Tuple{card}), nil, recordlayer.ForwardScan()))
		if lErr != nil {
			return lErr
		}
		for _, r := range recs {
			out = append(out, cq90IDOf(r.Record.Record))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("index scan %s = %d: %v", indexName, card, err)
	}
	return out
}

// cq90ScannedIDs returns the IDs a FULL RECORD SCAN yields, ordered by primary
// key. This is the answer the index is supposed to agree with.
func cq90ScannedIDs(t *testing.T, db *recordlayer.FDBDatabase, md *recordlayer.RecordMetaData,
	ks subspace.Subspace, wantCard int,
) []int32 {
	t.Helper()
	var out []int32
	err := cq90Run(t, db, md, ks, false, func(store *recordlayer.FDBRecordStore) error {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		recs, lErr := recordlayer.AsList(ctx, store.ScanRecords(nil, recordlayer.ForwardScan()))
		if lErr != nil {
			return lErr
		}
		for _, r := range recs {
			m := r.Record.ProtoReflect()
			fd := m.Descriptor().Fields().ByName("INT_ARR")
			if m.Get(fd).List().Len() == wantCard {
				out = append(out, cq90IDOf(r.Record))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("full scan: %v", err)
	}
	return out
}

func cq90IDOf(m proto.Message) int32 {
	r := m.ProtoReflect()
	return int32(r.Get(r.Descriptor().Fields().ByName("ID")).Int())
}

func cq90IDsEqual(a []int32, b ...int32) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// countStaleNullEntries counts entries keyed NULL — the stale form. After a
// rebuild there must be none: the column is NOT NULL, so no record can
// legitimately produce a NULL cardinality key.
func cq90CountStaleNullEntries(suffixes [][]byte) int {
	n := 0
	for _, s := range suffixes {
		if bytes.HasPrefix(s, cq90NullKey) {
			n++
		}
	}
	return n
}

// TestFDB_CardinalityStaleKeyBytes pins the two tuple codes the whole migration
// turns on, against the literal FDB wire bytes rather than against the codec's
// own opinion of itself.
func TestFDB_CardinalityStaleKeyBytes(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		what string
		got  []byte
		want []byte
	}{
		{"the pre-CQ-89 NULL key", cq90NullKey, []byte{0x00}},
		{"the post-CQ-89 integer 0 key", cq90ZeroKey, []byte{0x14}},
	} {
		if !bytes.Equal(tc.got, tc.want) {
			t.Fatalf("%s encodes to %x, want %x — the FDB tuple codes are the wire contract "+
				"and the migration is defined in terms of them", tc.what, tc.got, tc.want)
		}
	}
}

// TestFDB_CardinalityStaleNullKeyRebuiltInline is the migration on a store
// small enough for checkVersion to rebuild the index INLINE, inside the
// store-open transaction.
//
// The COUNT index is what makes that arm reachable, and the reason is worth
// stating because it is the single most surprising thing about this recipe.
// getRecordCountForRebuildPolicy has no cheap count unless the metadata
// declares one: with no COUNT index it falls through to an emptiness probe and
// reports MaxInt64 for ANY non-empty store (store_builder.go's
// `return math.MaxInt64`; Java's getRecordCountForRebuildIndexes does the
// same). MaxInt64 > 200, so DefaultIndexRebuildPolicy answers DISABLED and the
// inline arm never runs — no matter how few records there are. A store with
// three records only rebuilds inline because the COUNT index can be ASKED.
func TestFDB_CardinalityStaleNullKeyRebuiltInline(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	fdb.MustAPIVersion(730)
	rawDB, err := fdb.OpenDatabase(clusterFilePath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	db := recordlayer.NewFDBDatabase(rawDB)
	ks := subspace.FromBytes(tuple.Tuple{t.Name()}.Pack())

	md := cq90MetaData(t, "cq90inline", true)
	desc := md.GetRecordType("T_STALE").Descriptor

	// id1 holds the EMPTY NOT NULL array — the only affected shape. id2 and id3
	// are the contrast: their entries are untouched by the migration, and a
	// rebuild that lost them would be as wrong as one that kept the stale key.
	if rErr := cq90Run(t, db, md, ks, true, func(store *recordlayer.FDBRecordStore) error {
		for _, r := range []struct {
			id  int32
			arr []int32
		}{{1, nil}, {2, []int32{7}}, {3, []int32{7, 8}}} {
			if _, sErr := store.SaveRecord(cq90Rec(desc, r.id, r.arr)); sErr != nil {
				return sErr
			}
		}
		return nil
	}); rErr != nil {
		t.Fatalf("seed records: %v", rErr)
	}

	// Sanity: with the FIXED binary the empty array keys 0, so an index-backed
	// CARDINALITY = 0 finds id1. Without this the "misses rows" assertion below
	// could pass on an index that never held the row at all.
	if got := cq90IndexedIDs(t, db, md, ks, "T_STALE_CARD", 0); !cq90IDsEqual(got, 1) {
		t.Fatalf("before staleizing, index-backed CARDINALITY = 0 returned %v, want [1] — "+
			"the post-CQ-89 write path must key the empty NOT NULL array as 0", got)
	}

	// Rewind the store to the pre-CQ-89 on-disk state.
	if n := cq90Staleize(t, db, md, ks, "T_STALE_CARD"); n != 1 {
		t.Fatalf("staleized %d entries, want 1 — nothing was rewound, so the rest of this test asserts nothing", n)
	}

	// THE DEFECT, reproduced. The index misses the row; the full scan returns it.
	staleIdx := cq90IndexedIDs(t, db, md, ks, "T_STALE_CARD", 0)
	if len(staleIdx) != 0 {
		t.Fatalf("with the stale NULL entry in place, index-backed CARDINALITY = 0 returned %v, want none — "+
			"the pre-fix entry is keyed 0x00, so a scan of key 0 cannot see it", staleIdx)
	}
	if got := cq90ScannedIDs(t, db, md, ks, 0); !cq90IDsEqual(got, 1) {
		t.Fatalf("full scan for an empty array returned %v, want [1]", got)
	}
	if n := cq90CountStaleNullEntries(cq90IndexKeySuffixes(t, db, md, ks, "T_STALE_CARD")); n != 1 {
		t.Fatalf("expected exactly 1 NULL-keyed entry on disk before the migration, found %d", n)
	}

	// THE MIGRATION. Bump only the cardinality index, reopen, and let
	// checkPossiblyRebuild do the rest.
	md2 := cq90BumpIndexVersion(t, md, "T_STALE_CARD")
	if got := md2.GetIndexesToBuildSince(md.Version()); len(got) != 1 || got[0].Name != "T_STALE_CARD" {
		names := make([]string, len(got))
		for i, g := range got {
			names[i] = g.Name
		}
		t.Fatalf("GetIndexesToBuildSince(%d) selected %v, want exactly [T_STALE_CARD] — "+
			"bumping more than the affected index would exclude the COUNT index from the "+
			"record-count sources and force every index DISABLED", md.Version(), names)
	}

	var stateAfter recordlayer.IndexState
	if rErr := cq90Run(t, db, md2, ks, false, func(store *recordlayer.FDBRecordStore) error {
		stateAfter = store.GetIndexState("T_STALE_CARD")
		return nil
	}); rErr != nil {
		t.Fatalf("reopen with bumped metadata: %v", rErr)
	}
	if stateAfter != recordlayer.IndexStateReadable {
		t.Fatalf("after the bump the index is %v, want READABLE — with a real record count of 3 "+
			"(<= the 200 MAX_RECORDS_FOR_REBUILD threshold) DefaultIndexRebuildPolicy must rebuild inline",
			stateAfter)
	}

	// THE MIGRATION WORKED. The index and the full scan agree again, the
	// untouched cardinalities survived, and no NULL-keyed entry remains.
	if got := cq90IndexedIDs(t, db, md2, ks, "T_STALE_CARD", 0); !cq90IDsEqual(got, 1) {
		t.Fatalf("after the rebuild, index-backed CARDINALITY = 0 returned %v, want [1]", got)
	}
	if got := cq90IndexedIDs(t, db, md2, ks, "T_STALE_CARD", 1); !cq90IDsEqual(got, 2) {
		t.Fatalf("after the rebuild, index-backed CARDINALITY = 1 returned %v, want [2] — "+
			"the rebuild must not disturb the unaffected entries", got)
	}
	if got := cq90IndexedIDs(t, db, md2, ks, "T_STALE_CARD", 2); !cq90IDsEqual(got, 3) {
		t.Fatalf("after the rebuild, index-backed CARDINALITY = 2 returned %v, want [3]", got)
	}
	suffixes := cq90IndexKeySuffixes(t, db, md2, ks, "T_STALE_CARD")
	if n := cq90CountStaleNullEntries(suffixes); n != 0 {
		t.Fatalf("after the rebuild %d NULL-keyed entries remain (%x) — the column is NOT NULL, "+
			"so no record can produce a NULL cardinality key and every survivor is stale", n, suffixes)
	}
	if len(suffixes) != 3 {
		t.Fatalf("after the rebuild the index holds %d entries (%x), want 3", len(suffixes), suffixes)
	}
	if !bytes.HasPrefix(suffixes[0], cq90ZeroKey) {
		t.Fatalf("after the rebuild the lowest entry is %x, want it to start with the integer-0 code %x",
			suffixes[0], cq90ZeroKey)
	}
}

// cq90BumpEveryIndexVersion is the WRONG way to author the migration, kept
// executable so the recipe's central warning is a measurement rather than a
// claim. It raises every index's LastModifiedVersion, as re-saving a whole
// relational schema template at a higher version does.
func cq90BumpEveryIndexVersion(t *testing.T, md *recordlayer.RecordMetaData) *recordlayer.RecordMetaData {
	t.Helper()
	p, err := md.ToProto()
	if err != nil {
		t.Fatalf("metadata to proto: %v", err)
	}
	next := int32(md.Version() + 1)
	for _, idx := range p.GetIndexes() {
		idx.LastModifiedVersion = proto.Int32(next)
	}
	p.Version = proto.Int32(next)
	out, err := recordlayer.RecordMetaDataFromProto(p)
	if err != nil {
		t.Fatalf("metadata from proto: %v", err)
	}
	return out
}

// TestFDB_CardinalityWholeSchemaBumpDisablesEvenTinyStore pins the sharpest
// warning in the CQ-90 recipe, which would otherwise be prose nobody had run.
//
// Bumping ONLY the affected index leaves the COUNT index readable and usable, so
// a three-record store reports a real count of 3 and rebuilds inline
// (TestFDB_CardinalityStaleNullKeyRebuiltInline). Bump the WHOLE schema — which
// is what re-saving a relational template at a higher version does, since
// addIndexCommon re-derives every index's version from the builder counter — and
// the COUNT index joins indexesToBuild. Every index being built is excluded from
// the record-count sources (Java's IndexQueryabilityFilter,
// FDBRecordStore.java:4841), because an index holding no entries yet would report
// 0 for a full store. With its only count source excluded,
// getRecordCountForRebuildPolicy falls through to the emptiness probe and reports
// MaxInt64.
//
// The consequence is counter-intuitive enough to deserve a test: a THREE-record
// store lands DISABLED, needing an OnlineIndexer run, purely because the operator
// bumped more than they had to.
func TestFDB_CardinalityWholeSchemaBumpDisablesEvenTinyStore(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	fdb.MustAPIVersion(730)
	rawDB, err := fdb.OpenDatabase(clusterFilePath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	db := recordlayer.NewFDBDatabase(rawDB)
	ks := subspace.FromBytes(tuple.Tuple{t.Name()}.Pack())

	md := cq90MetaData(t, "cq90wholebump", true)
	desc := md.GetRecordType("T_STALE").Descriptor
	if rErr := cq90Run(t, db, md, ks, true, func(store *recordlayer.FDBRecordStore) error {
		for _, r := range []struct {
			id  int32
			arr []int32
		}{{1, nil}, {2, []int32{7}}, {3, []int32{7, 8}}} {
			if _, sErr := store.SaveRecord(cq90Rec(desc, r.id, r.arr)); sErr != nil {
				return sErr
			}
		}
		return nil
	}); rErr != nil {
		t.Fatalf("seed records: %v", rErr)
	}

	// Both indexes bumped — the operator error the recipe warns against.
	mdAll := cq90BumpEveryIndexVersion(t, md)
	built := mdAll.GetIndexesToBuildSince(md.Version())
	if len(built) != 2 {
		names := make([]string, len(built))
		for i, b := range built {
			names[i] = b.Name
		}
		t.Fatalf("GetIndexesToBuildSince(%d) selected %v, want BOTH indexes — this test is about what "+
			"happens when the COUNT index is dragged along, so it must actually be dragged along",
			md.Version(), names)
	}

	var cardState recordlayer.IndexState
	if rErr := cq90Run(t, db, mdAll, ks, false, func(store *recordlayer.FDBRecordStore) error {
		cardState = store.GetIndexState("T_STALE_CARD")
		return nil
	}); rErr != nil {
		t.Fatalf("reopen with whole-schema bump: %v", rErr)
	}
	if cardState != recordlayer.IndexStateDisabled {
		t.Fatalf("after a WHOLE-SCHEMA bump on a 3-record store the cardinality index is %v, want DISABLED. "+
			"Three records is far below MAX_RECORDS_FOR_REBUILD (200), so this can only be DISABLED because "+
			"the COUNT index was excluded as a count source while it too was being rebuilt, degrading the "+
			"count to MaxInt64. If this now reports READABLE, the recipe's 'bump only the affected index' "+
			"warning is obsolete and must be rewritten, not quietly left in place", cardState)
	}
}

// TestFDB_CardinalityUniquePendingBlockedByFormatVersion pins the SECOND
// conjunct of Java's policy gate, the one the opt-in cannot override.
//
// shouldAllowUniquePendingState is an AND (OnlineIndexer.java:1117-1120):
//
//	return allowUniquePendingState
//	        && store.getFormatVersionEnum().isAtLeast(FormatVersion.READABLE_UNIQUE_PENDING);
//
// READABLE_UNIQUE_PENDING is format version 9 (FormatVersion.java:145). Writing
// that state into a store an older binary can still open would hand it an index
// state it cannot interpret, so the opt-in alone must never be sufficient — and
// a gate whose second condition is never exercised is a gate on paper only.
//
// The store is OPENED at format 8 throughout, via StoreBuilder.SetFormatVersion —
// the port of Java's FDBRecordStoreBase.BaseBuilder.setFormatVersion
// (FDBRecordStoreBase.java:2245). Writing the header to 8 after the fact does not
// work and is worth recording: maybeUpgradeFormatVersion upgrades any store below
// the target back to it on the very next open, so a store can only BE at format 8
// if every instance opening it is pinned there. That is exactly the rolling-upgrade
// situation Java's javadoc describes, and the only shape in which this gate matters.
func TestFDB_CardinalityUniquePendingBlockedByFormatVersion(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	fdb.MustAPIVersion(730)
	rawDB, err := fdb.OpenDatabase(clusterFilePath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	db := recordlayer.NewFDBDatabase(rawDB)
	ks := subspace.FromBytes(tuple.Tuple{t.Name()}.Pack())

	const belowPending = int32(8)
	// Every open in this test is pinned at format 8, including the CREATE.
	pinnedRun := func(md *recordlayer.RecordMetaData, create bool,
		fn func(store *recordlayer.FDBRecordStore) error,
	) error {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		_, err := db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
			sb := recordlayer.NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).
				SetSubspace(ks).SetFormatVersion(belowPending)
			var store *recordlayer.FDBRecordStore
			var oErr error
			if create {
				store, oErr = sb.CreateOrOpen()
			} else {
				store, oErr = sb.Open()
			}
			if oErr != nil {
				return nil, oErr
			}
			return nil, fn(store)
		})
		return err
	}

	md := cq90MetaData(t, "cq90fmtgate", false) // no COUNT index → DISABLED, indexer reachable
	desc := md.GetRecordType("T_STALE").Descriptor
	if rErr := pinnedRun(md, true, func(store *recordlayer.FDBRecordStore) error {
		for _, id := range []int32{1, 2} {
			if _, sErr := store.SaveRecord(cq90Rec(desc, id, nil)); sErr != nil {
				return sErr
			}
		}
		return nil
	}); rErr != nil {
		t.Fatalf("seed the colliding pair: %v", rErr)
	}

	md2 := cq90BumpIndexVersion(t, md, "T_STALE_CARD")
	md2.GetIndex("T_STALE_CARD").SetUnique()

	if rErr := pinnedRun(md2, false, func(store *recordlayer.FDBRecordStore) error {
		if got := store.GetFormatVersion(); got != belowPending {
			return fmt.Errorf("store format version is %d, want %d — the pin did not hold, so this test "+
				"would prove nothing about the format conjunct", got, belowPending)
		}
		if st := store.GetIndexState("T_STALE_CARD"); st != recordlayer.IndexStateDisabled {
			return fmt.Errorf("index is %v, want DISABLED so the OnlineIndexer is reachable", st)
		}
		return nil
	}); rErr != nil {
		t.Fatal(rErr)
	}

	// Policy explicitly ON, yet the format conjunct must still block the pending
	// state and make the build fail.
	indexer, bErr := recordlayer.NewOnlineIndexerBuilder().
		SetDatabase(db).SetMetaData(md2).SetIndex(md2.GetIndex("T_STALE_CARD")).
		SetSubspace(ks).SetLimit(50).SetAllowUniquePendingState(true).
		SetFormatVersion(belowPending).Build()
	if bErr != nil {
		t.Fatalf("build online indexer: %v", bErr)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	_, iErr := indexer.BuildIndex(ctx)
	if iErr == nil {
		t.Fatalf("the build completed with allowUniquePendingState=true on a store at format version %d. "+
			"READABLE_UNIQUE_PENDING is format version 9 (FormatVersion.java:145) and the policy is an AND "+
			"(OnlineIndexer.java:1117-1120), so the format conjunct must veto the opt-in — otherwise Go "+
			"writes an index state an older binary opening this store cannot interpret", belowPending)
	}
	var uvErr *recordlayer.RecordIndexUniquenessViolationError
	if !errors.As(iErr, &uvErr) {
		t.Fatalf("format-gated build failed with %v, want a RecordIndexUniquenessViolationError — the veto "+
			"must fall through to the ordinary throwing path, not fail some other way", iErr)
	}

	var st recordlayer.IndexState
	if rErr := pinnedRun(md2, false, func(store *recordlayer.FDBRecordStore) error {
		st = store.GetIndexState("T_STALE_CARD")
		return nil
	}); rErr != nil {
		t.Fatal(rErr)
	}
	if st == recordlayer.IndexStateReadableUniquePending {
		t.Fatalf("the index reached READABLE_UNIQUE_PENDING on a store at format version %d — exactly the "+
			"state the format conjunct exists to prevent", belowPending)
	}
}

// TestFDB_CardinalityStaleNullKeyDisabledThenOnlineIndexer is the same
// migration on a store ABOVE MAX_RECORDS_FOR_REBUILD — the production shape.
// checkVersion must refuse to rebuild inline (a full index build inside the
// store-open transaction would run into the 5s / 10MB limits), leave the index
// DISABLED, and an explicit OnlineIndexer run must finish the job.
//
// The 201 records are load-bearing in both directions: 200 would take the
// inline arm, and the COUNT index is what makes the count REAL rather than
// MaxInt64, so the threshold is genuinely what decides here.
func TestFDB_CardinalityStaleNullKeyDisabledThenOnlineIndexer(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	fdb.MustAPIVersion(730)
	rawDB, err := fdb.OpenDatabase(clusterFilePath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	db := recordlayer.NewFDBDatabase(rawDB)
	ks := subspace.FromBytes(tuple.Tuple{t.Name()}.Pack())

	md := cq90MetaData(t, "cq90online", true)
	desc := md.GetRecordType("T_STALE").Descriptor

	// 201 records: ids 1..3 hold EMPTY arrays (the affected shape), the rest
	// hold one element each.
	const total = 201
	const empties = 3
	for base := int32(1); base <= total; base += 50 {
		lo, hi := base, base+49
		if hi > total {
			hi = total
		}
		if rErr := cq90Run(t, db, md, ks, lo == 1, func(store *recordlayer.FDBRecordStore) error {
			for id := lo; id <= hi; id++ {
				var arr []int32
				if id > empties {
					arr = []int32{id}
				}
				if _, sErr := store.SaveRecord(cq90Rec(desc, id, arr)); sErr != nil {
					return sErr
				}
			}
			return nil
		}); rErr != nil {
			t.Fatalf("seed records %d..%d: %v", lo, hi, rErr)
		}
	}

	if got := cq90IndexedIDs(t, db, md, ks, "T_STALE_CARD", 0); !cq90IDsEqual(got, 1, 2, 3) {
		t.Fatalf("before staleizing, index-backed CARDINALITY = 0 returned %v, want [1 2 3]", got)
	}
	if n := cq90Staleize(t, db, md, ks, "T_STALE_CARD"); n != empties {
		t.Fatalf("staleized %d entries, want %d", n, empties)
	}
	if got := cq90IndexedIDs(t, db, md, ks, "T_STALE_CARD", 0); len(got) != 0 {
		t.Fatalf("with the stale NULL entries in place, index-backed CARDINALITY = 0 returned %v, want none", got)
	}

	// THE MIGRATION, arm two: above the threshold the index must go DISABLED.
	md2 := cq90BumpIndexVersion(t, md, "T_STALE_CARD")
	var stateAfter recordlayer.IndexState
	if rErr := cq90Run(t, db, md2, ks, false, func(store *recordlayer.FDBRecordStore) error {
		stateAfter = store.GetIndexState("T_STALE_CARD")
		return nil
	}); rErr != nil {
		t.Fatalf("reopen with bumped metadata: %v", rErr)
	}
	if stateAfter != recordlayer.IndexStateDisabled {
		t.Fatalf("after the bump on a %d-record store the index is %v, want DISABLED — above the 200 "+
			"MAX_RECORDS_FOR_REBUILD threshold DefaultIndexRebuildPolicy must NOT rebuild inside the "+
			"store-open transaction", total, stateAfter)
	}
	// WHERE THE STALE ENTRIES ACTUALLY DIE, which is not where it looks. Going
	// DISABLED is not a bookkeeping flag flip: MarkIndexDisabled calls
	// clearIndexData (Java's markIndexDisabled does the same), so the stale
	// NULL-keyed entries are gone the moment the bump is reconciled — BEFORE
	// the OnlineIndexer has run at all. Asserting their absence only at the end
	// would credit the OnlineIndexer with a clear it never had to perform, and
	// would stay green if that clear were removed. Pin it here, where it
	// happens.
	if suffixes := cq90IndexKeySuffixes(t, db, md2, ks, "T_STALE_CARD"); len(suffixes) != 0 {
		t.Fatalf("after the bump marked the index DISABLED, %d entries remain in its subspace (%x), want 0 — "+
			"MarkIndexDisabled must clear the index data, otherwise the stale pre-CQ-89 entries survive the "+
			"whole migration window and the OnlineIndexer rebuilds on top of them", len(suffixes), suffixes)
	}

	// A DISABLED index is not scannable, so nothing can read stale answers out
	// of it in the window before the OnlineIndexer runs. That is the property
	// that makes leaving it DISABLED safe rather than merely cheap.
	if rErr := cq90Run(t, db, md2, ks, false, func(store *recordlayer.FDBRecordStore) error {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		_, lErr := recordlayer.AsList(ctx, store.ScanIndexRecords(
			"T_STALE_CARD", recordlayer.TupleRangeAllOf(tuple.Tuple{int64(0)}), nil, recordlayer.ForwardScan()))
		var notReadable *recordlayer.IndexNotReadableError
		if !errors.As(lErr, &notReadable) {
			return fmt.Errorf("scanning the DISABLED index returned %v, want IndexNotReadableError", lErr)
		}
		return nil
	}); rErr != nil {
		t.Fatal(rErr)
	}

	// THE OPERATOR STEP. An explicit OnlineIndexer run completes the rebuild.
	indexer, bErr := recordlayer.NewOnlineIndexerBuilder().
		SetDatabase(db).SetMetaData(md2).SetIndex(md2.GetIndex("T_STALE_CARD")).
		SetSubspace(ks).SetLimit(50).Build()
	if bErr != nil {
		t.Fatalf("build online indexer: %v", bErr)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	built, iErr := indexer.BuildIndex(ctx)
	if iErr != nil {
		t.Fatalf("online index build: %v", iErr)
	}
	if built != total {
		t.Fatalf("online indexer scanned %d records, want %d", built, total)
	}

	if rErr := cq90Run(t, db, md2, ks, false, func(store *recordlayer.FDBRecordStore) error {
		if st := store.GetIndexState("T_STALE_CARD"); st != recordlayer.IndexStateReadable {
			return fmt.Errorf("after the online build the index is %v, want READABLE", st)
		}
		return nil
	}); rErr != nil {
		t.Fatal(rErr)
	}
	if got := cq90IndexedIDs(t, db, md2, ks, "T_STALE_CARD", 0); !cq90IDsEqual(got, 1, 2, 3) {
		t.Fatalf("after the online rebuild, index-backed CARDINALITY = 0 returned %v, want [1 2 3]", got)
	}
	if got := cq90ScannedIDs(t, db, md2, ks, 0); !cq90IDsEqual(got, 1, 2, 3) {
		t.Fatalf("full scan for an empty array returned %v, want [1 2 3] — index and scan must agree", got)
	}
	suffixes := cq90IndexKeySuffixes(t, db, md2, ks, "T_STALE_CARD")
	if n := cq90CountStaleNullEntries(suffixes); n != 0 {
		t.Fatalf("after the online rebuild %d NULL-keyed entries remain — the rebuild reintroduced a NULL "+
			"key, which only the pre-CQ-89 derivation can produce (the DISABLED transition above already "+
			"proved the subspace was empty when the build started)", n)
	}
	if len(suffixes) != total {
		t.Fatalf("after the online rebuild the index holds %d entries, want %d", len(suffixes), total)
	}
}

// TestFDB_CardinalityStaleNullKeyRebuildSurfacesUniquenessViolation is the
// sharp sub-hazard, and it is the one case where the migration is SUPPOSED to
// fail.
//
// StandardIndexMaintainer skips the uniqueness check for an entry whose key
// contains a NULL (Java: StandardIndexMaintainer.java:471; Go:
// index_maintainer.go's `!indexKeyContainsNull` guard). Under the OLD key two
// records with empty NOT NULL arrays BOTH keyed NULL and were therefore never
// checked, so a pre-CQ-89 store can be carrying a pair the new key considers
// impossible. TestFDB_ArrayCardinalityUniqueIndex pins that both records are
// rejected going FORWARD; this pins what happens to the pair that already got
// in.
//
// THE LOUD OUTCOME IS A THROW, and the two-stage mechanism behind it is worth
// stating because the halves pull in opposite directions.
//
// During the build the index is WRITE_ONLY, and there checkUniqueness RECORDS a
// violation rather than throwing (index_maintainer.go:493-504; Java's
// standardIndexMaintainer.checkUniqueness calls addUniquenessViolation for both
// conflicting primary keys). That is why the duplicates do not abort mid-scan.
// The reckoning comes at the end: Java's inline rebuild calls the one-argument
// markIndexReadable(index) (FDBRecordStore.java:4602 → :3767-3768,
// allowUniquePending=FALSE), and checkAndUpdateBuiltIndexState (:3821) then
// THROWS RecordIndexUniquenessViolation ("Uniqueness violation when making index
// readable", :3856-3861).
//
// READABLE_UNIQUE_PENDING is NOT the default landing spot. Java core reaches it
// from exactly one caller, IndexingBase.java:324, gated on
// IndexingPolicy.shouldAllowUniquePendingState (OnlineIndexer.java:1117) — an
// opt-in defaulting FALSE (:1220) that also demands format version >=
// READABLE_UNIQUE_PENDING (FormatVersion.java:145). Both arms are pinned below,
// because a policy flag that is never tested in its OFF position is not a policy.
//
// The failure this excludes is the quiet one: a rebuild that "succeeds" by
// discarding one of the two entries would convert a detectable inconsistency
// into a silent one, and would sail past an assertion that only checked rows.
func TestFDB_CardinalityStaleNullKeyRebuildSurfacesUniquenessViolation(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	fdb.MustAPIVersion(730)
	rawDB, err := fdb.OpenDatabase(clusterFilePath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	db := recordlayer.NewFDBDatabase(rawDB)
	ks := subspace.FromBytes(tuple.Tuple{t.Name()}.Pack())

	// Built NON-unique so the two empty-array records can be written at all —
	// which is exactly the pre-CQ-89 situation, where the NULL key made the
	// unique index behave as if it were not unique for this shape.
	md := cq90MetaData(t, "cq90uniq", true)
	desc := md.GetRecordType("T_STALE").Descriptor
	if rErr := cq90Run(t, db, md, ks, true, func(store *recordlayer.FDBRecordStore) error {
		for _, id := range []int32{1, 2} {
			if _, sErr := store.SaveRecord(cq90Rec(desc, id, nil)); sErr != nil {
				return sErr
			}
		}
		return nil
	}); rErr != nil {
		t.Fatalf("seed the colliding pair: %v", rErr)
	}
	if n := cq90Staleize(t, db, md, ks, "T_STALE_CARD"); n != 2 {
		t.Fatalf("staleized %d entries, want 2 — both empty-array records must carry the pre-fix NULL key", n)
	}

	// Now the index is declared UNIQUE, as a schema owner would when the DDL
	// route for a UNIQUE cardinality index lands. The two NULL-keyed entries
	// coexist on disk; under the new key they cannot.
	md2 := cq90BumpIndexVersion(t, md, "T_STALE_CARD")
	uniq := md2.GetIndex("T_STALE_CARD")
	uniq.SetUnique()
	if !uniq.IsUnique() {
		t.Fatal("T_STALE_CARD is not unique after SetUnique — the rest of this test would assert nothing")
	}

	// ARM 1 — THE DEFAULT. The inline rebuild at store open must FAIL, and fail
	// with the structured error that names the index.
	rebuildErr := cq90Run(t, db, md2, ks, false, func(store *recordlayer.FDBRecordStore) error {
		return nil
	})
	if rebuildErr == nil {
		t.Fatal("the store opened cleanly on a store holding TWO records with an empty NOT NULL array on a " +
			"UNIQUE cardinality index. Both key the integer 0, so exactly one entry can exist. Java's inline " +
			"rebuild marks readable with allowUniquePending=FALSE (FDBRecordStore.java:4602 → :3767-3768) and " +
			"THROWS RecordIndexUniquenessViolation (:3856-3861); a silent success means Go downgraded to " +
			"READABLE_UNIQUE_PENDING behind the operator's back, or dropped an entry")
	}
	var uv *recordlayer.RecordIndexUniquenessViolationError
	if !errors.As(rebuildErr, &uv) {
		t.Fatalf("the rebuild failed with %v, want a RecordIndexUniquenessViolationError — the migration has "+
			"to NAME the invariant it found broken, not merely refuse", rebuildErr)
	}
	if uv.IndexName != "T_STALE_CARD" {
		t.Fatalf("uniqueness violation names index %q, want T_STALE_CARD", uv.IndexName)
	}
	if len(uv.IndexKey) != 1 || uv.IndexKey[0] != int64(0) {
		t.Fatalf("uniqueness violation is on key %v, want the single column [0] — that is the key the two "+
			"empty NOT NULL arrays now share, and the whole point of the migration", uv.IndexKey)
	}
	// The error must NAME a colliding record, not just the key. Java's javadoc is
	// explicit that with multiple duplicates "the exception will only refer to an
	// arbitrary one", so the assertion is that it names ONE OF the two — that both
	// are recorded is pinned in the opt-in test, where the violations survive.
	// A relational primary key carries the record-type key ahead of the declared
	// columns, so ID is the LAST element.
	if len(uv.PrimaryKey) == 0 {
		t.Fatalf("uniqueness violation carries an empty primary key (%+v) — an operator cannot act on a "+
			"violation that names no record", uv)
	}
	namedID, ok := uv.PrimaryKey[len(uv.PrimaryKey)-1].(int64)
	if !ok || (namedID != 1 && namedID != 2) {
		t.Fatalf("uniqueness violation names primary key %v, want one ending in 1 or 2 — the two records "+
			"whose empty NOT NULL arrays collide on key 0", uv.PrimaryKey)
	}

	// ATOMICITY, the header half stated outright rather than inferred from a later
	// error: the store header must still carry the OLD metadata version, so the
	// migration is a clean re-attempt rather than a resume of something partial.
	var headerVersion int32
	if rErr := cq90Run(t, db, md, ks, false, func(store *recordlayer.FDBRecordStore) error {
		headerVersion = store.GetMetaDataVersion()
		return nil
	}); rErr != nil {
		t.Fatalf("reopen at the OLD metadata version to inspect the header: %v", rErr)
	}
	if headerVersion != int32(md.Version()) {
		t.Fatalf("after the FAILED rebuild the store header carries metadata version %d, want the ORIGINAL "+
			"%d — checkVersion advances the header in the same transaction as the rebuild, so a header that "+
			"moved means the throw did not roll everything back and the migration is half-applied",
			headerVersion, md.Version())
	}

	// THE FAILED MIGRATION IS ATOMIC, which is the half a bare "it threw"
	// assertion misses. checkVersion runs inside the store-open transaction, so
	// the throw rolls the whole thing back: no index state is written, no
	// violation records survive, the store header keeps the OLD metadata version,
	// and the stale NULL-keyed entries are still exactly where they were. The
	// operator is left with a store that is unchanged and a retry that is a clean
	// re-attempt rather than a resume of something half-applied.
	//
	// Reopening at the OLD metadata does not itself trigger a rebuild, so this
	// observes the store rather than disturbing it.
	suffixesAfterFail := cq90IndexKeySuffixes(t, db, md, ks, "T_STALE_CARD")
	if n := cq90CountStaleNullEntries(suffixesAfterFail); n != 2 {
		t.Fatalf("after the FAILED rebuild the index holds %d NULL-keyed entries (%x), want the original 2 — "+
			"the throw must roll the store-open transaction back untouched, leaving nothing half-migrated",
			n, suffixesAfterFail)
	}
	if len(suffixesAfterFail) != 2 {
		t.Fatalf("after the FAILED rebuild the index holds %d entries (%x), want 2", len(suffixesAfterFail),
			suffixesAfterFail)
	}

	// Nothing was dropped: both records are still reachable by a full scan. The
	// conflict is REPORTED, never resolved by discarding an entry.
	if got := cq90ScannedIDs(t, db, md, ks, 0); !cq90IDsEqual(got, 1, 2) {
		t.Fatalf("full scan for an empty array returned %v, want [1 2] — a uniqueness conflict must never "+
			"cost a RECORD", got)
	}

	// The OPT-IN side of the policy — the only route to READABLE_UNIQUE_PENDING —
	// lives in TestFDB_CardinalityUniquePendingPolicyBothSides. It cannot be
	// exercised here: on a store small enough for the inline arm, store open
	// rebuilds and throws before an OnlineIndexer could ever be reached.
}

// TestFDB_CardinalityUniquePendingPolicyBothSides pins BOTH positions of Java's
// allowUniquePendingState policy on the CQ-90 duplicate pair. A policy flag
// tested only in its ON position is not a policy — it is a feature that happens
// to work, with nothing pinning the default that almost every operator gets.
//
// The store deliberately declares NO COUNT index. That is what makes this
// reachable at all: without a count source the rebuild policy sees MaxInt64 and
// leaves the index DISABLED instead of rebuilding inline, so the OnlineIndexer —
// which is where Java consults the policy (IndexingBase.java:324) — actually gets
// to run. With a COUNT index the store-open rebuild would throw first and the
// OnlineIndexer would never be reached.
func TestFDB_CardinalityUniquePendingPolicyBothSides(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	fdb.MustAPIVersion(730)
	rawDB, err := fdb.OpenDatabase(clusterFilePath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	db := recordlayer.NewFDBDatabase(rawDB)
	ks := subspace.FromBytes(tuple.Tuple{t.Name()}.Pack())

	md := cq90MetaData(t, "cq90pending", false) // no COUNT index — see above
	desc := md.GetRecordType("T_STALE").Descriptor
	if rErr := cq90Run(t, db, md, ks, true, func(store *recordlayer.FDBRecordStore) error {
		for _, id := range []int32{1, 2} {
			if _, sErr := store.SaveRecord(cq90Rec(desc, id, nil)); sErr != nil {
				return sErr
			}
		}
		return nil
	}); rErr != nil {
		t.Fatalf("seed the colliding pair: %v", rErr)
	}
	if n := cq90Staleize(t, db, md, ks, "T_STALE_CARD"); n != 2 {
		t.Fatalf("staleized %d entries, want 2", n)
	}

	md2 := cq90BumpIndexVersion(t, md, "T_STALE_CARD")
	uniq := md2.GetIndex("T_STALE_CARD")
	uniq.SetUnique()
	if !uniq.IsUnique() {
		t.Fatal("T_STALE_CARD is not unique after SetUnique — the rest of this test would assert nothing")
	}

	// Reconciliation leaves it DISABLED (no count source), so no inline rebuild
	// throws and the OnlineIndexer is reachable.
	var st recordlayer.IndexState
	var formatVersion int32
	if rErr := cq90Run(t, db, md2, ks, false, func(store *recordlayer.FDBRecordStore) error {
		st = store.GetIndexState("T_STALE_CARD")
		formatVersion = store.GetFormatVersion()
		return nil
	}); rErr != nil {
		t.Fatalf("reopen with bumped metadata: %v", rErr)
	}
	if st != recordlayer.IndexStateDisabled {
		t.Fatalf("after the bump the index is %v, want DISABLED — without a COUNT index the count degrades "+
			"to MaxInt64 and reconciliation must defer to a background build", st)
	}
	if formatVersion < int32(9) {
		t.Fatalf("store format version is %d, below READABLE_UNIQUE_PENDING (9) — the ON arm below would be "+
			"gated off by the FORMAT VERSION rather than by the policy, proving nothing about the opt-in",
			formatVersion)
	}

	newIndexer := func(allow bool) *recordlayer.OnlineIndexer {
		t.Helper()
		b := recordlayer.NewOnlineIndexerBuilder().
			SetDatabase(db).SetMetaData(md2).SetIndex(md2.GetIndex("T_STALE_CARD")).
			SetSubspace(ks).SetLimit(50)
		if allow {
			b = b.SetAllowUniquePendingState(true)
		}
		oi, bErr := b.Build()
		if bErr != nil {
			t.Fatalf("build online indexer (allow=%v): %v", allow, bErr)
		}
		return oi
	}

	// SIDE 1 — THE DEFAULT (allowUniquePendingState unset). Java's builder field
	// defaults false (OnlineIndexer.java:1220) and its javadoc spells out the
	// consequence: "allow=false (default, backward compatible): throw an
	// exception." The build must FAIL.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	_, defaultErr := newIndexer(false).BuildIndex(ctx)
	if defaultErr == nil {
		t.Fatal("an OnlineIndexer build with the DEFAULT policy completed on an index holding duplicates. " +
			"Java defaults allowUniquePendingState to FALSE and throws; a Go default that silently lands in " +
			"READABLE_UNIQUE_PENDING hands operators a state older binaries cannot interpret, without asking")
	}
	var uvErr *recordlayer.RecordIndexUniquenessViolationError
	if !errors.As(defaultErr, &uvErr) {
		t.Fatalf("default-policy build failed with %v, want a RecordIndexUniquenessViolationError", defaultErr)
	}

	// SIDE 2 — THE OPT-IN. Same store, same duplicates, policy explicitly on.
	if _, iErr := newIndexer(true).BuildIndex(ctx); iErr != nil {
		t.Fatalf("online build with allowUniquePendingState=true failed with %v — the opt-in exists precisely "+
			"so this build completes instead of throwing", iErr)
	}
	var pendingState recordlayer.IndexState
	var violations []recordlayer.UniquenessViolation
	if rErr := cq90Run(t, db, md2, ks, false, func(store *recordlayer.FDBRecordStore) error {
		pendingState = store.GetIndexState("T_STALE_CARD")
		vs, vErr := store.ScanUniquenessViolations(md2.GetIndex("T_STALE_CARD"))
		violations = vs
		return vErr
	}); rErr != nil {
		t.Fatal(rErr)
	}
	if pendingState != recordlayer.IndexStateReadableUniquePending {
		t.Fatalf("with allowUniquePendingState=true the index is %v, want READABLE_UNIQUE_PENDING — the "+
			"policy opt-in is not reaching MarkIndexReadableOrUniquePending", pendingState)
	}

	// THE VIOLATIONS ARE DURABLE, AND BOTH RECORDS ARE NAMED. This is the state
	// whose entire justification is that the conflict remains enumerable, so the
	// violations subspace is the thing to check — a pending index whose violations
	// were not recorded is unexplainable and unresolvable.
	//
	// Java writes the pair in BOTH directions, one call per conflicting primary
	// key (StandardIndexMaintainer.java:497-498):
	//     addUniquenessViolation(valueKey, primaryKey, existingKey);
	//     addUniquenessViolation(valueKey, existingKey, primaryKey);
	// so an operator can look up either record and find the other. Recording only
	// one direction would leave half the pair invisible to a lookup keyed on it.
	if len(violations) == 0 {
		t.Fatal("the build landed READABLE_UNIQUE_PENDING but recorded NO uniqueness violation — that state " +
			"exists so the conflict stays enumerable, and without the records it is a dead end")
	}
	for _, v := range violations {
		if len(v.IndexKey) != 1 || v.IndexKey[0] != int64(0) {
			t.Fatalf("recorded violation is on key %v, want the single column [0]", v.IndexKey)
		}
	}
	named := map[int64]bool{}
	for _, v := range violations {
		if len(v.PrimaryKey) == 0 {
			t.Fatalf("recorded violation has an empty primary key: %+v", v)
		}
		id, ok := v.PrimaryKey[len(v.PrimaryKey)-1].(int64)
		if !ok {
			t.Fatalf("recorded violation primary key %v does not end in an integer ID", v.PrimaryKey)
		}
		named[id] = true
	}
	if !named[1] || !named[2] {
		t.Fatalf("the recorded violations name primary keys %v, want BOTH 1 and 2 — Java calls "+
			"addUniquenessViolation once per conflicting primary key (StandardIndexMaintainer.java:497-498), "+
			"so a lookup keyed on either record finds the other; one direction leaves half the pair invisible",
			named)
	}

	// The pending index is scannable (Java IndexState.java:95) and agrees with the
	// base table: both entries survived, keyed 0, with no stale NULL key left.
	if got := cq90IndexedIDs(t, db, md2, ks, "T_STALE_CARD", 0); !cq90IDsEqual(got, 1, 2) {
		t.Fatalf("after the pending-state build, index-backed CARDINALITY = 0 returned %v, want [1 2]", got)
	}
	suffixes := cq90IndexKeySuffixes(t, db, md2, ks, "T_STALE_CARD")
	if n := cq90CountStaleNullEntries(suffixes); n != 0 {
		t.Fatalf("after the pending-state build %d NULL-keyed entries remain — the migration did not run", n)
	}
	if len(suffixes) != 2 {
		t.Fatalf("after the pending-state build the index holds %d entries (%x), want 2 — a uniqueness "+
			"conflict must be REPORTED, never resolved by discarding an entry", len(suffixes), suffixes)
	}
}
