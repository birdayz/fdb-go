package recordlayer

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"testing"

	"fdb.dev/gen/demopb"
	"fdb.dev/pkg/dst"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/simfdb"
	"google.golang.org/protobuf/proto"
)

// shardMapBackend is simfdb with an injected shard map: LocalityGetBoundaryKeys returns
// the injected keys inside the requested range and records each request.
type shardMapBackend struct {
	fdb.BackendDatabase
	shards       []fdb.Key
	requests     []fdb.KeyRange
	readVersions []int64
}

func (b *shardMapBackend) LocalityGetBoundaryKeys(r fdb.ExactRange, _ int, readVersion int64) ([]fdb.Key, error) {
	begin, end := r.FDBRangeKeys()
	b.requests = append(b.requests, fdb.KeyRange{Begin: begin, End: end})
	b.readVersions = append(b.readVersions, readVersion)
	var keys []fdb.Key
	for _, k := range b.shards {
		if bytes.Compare(k, begin.FDBKey()) >= 0 && bytes.Compare(k, end.FDBKey()) < 0 {
			keys = append(keys, k)
		}
	}
	return keys, nil
}

// newShardMapDB is a simulated database whose shard map the test sets.
func newShardMapDB(seed uint64) (*FDBDatabase, *shardMapBackend) {
	env := dst.NewSim(seed)
	env.Buggify = dst.DisabledBuggifier()
	backend := &shardMapBackend{BackendDatabase: simfdb.New(env)}
	return NewFDBDatabaseWithBackend(backend).SetEnv(env), backend
}

// seedOrders saves orders with the given ids; an order in large holds a record split
// into three chunks.
func seedOrders(t *testing.T, db *FDBDatabase, sb *StoreBuilder, ids []int64, large ...int64) {
	t.Helper()
	if _, err := db.Run(context.Background(), func(rtx *FDBRecordContext) (any, error) {
		store, err := sb.copyBuilder().SetContext(rtx).CreateOrOpen()
		if err != nil {
			return nil, err
		}
		for _, id := range ids {
			order := &demopb.Order{OrderId: proto.Int64(id), Price: proto.Int32(int32(id))}
			if slices.Contains(large, id) {
				order.VectorData = make([]byte, 250_000)
			}
			if _, err := store.SaveRecord(order); err != nil {
				return nil, err
			}
		}
		return nil, nil
	}); err != nil {
		t.Fatalf("seed orders: %v", err)
	}
}

func TestGetPrimaryKeyBoundaries(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		split  bool
		format int32 // 0: the current format
		large  []int64
		shards func(records subspace.Subspace) []fdb.Key
		want   []tuple.Tuple
	}{{
		// A boundary maps to the first record key at or after it, without the split suffix.
		name: "split records", split: true, large: []int64{50},
		shards: func(records subspace.Subspace) []fdb.Key {
			return []fdb.Key{
				fdb.Key(append(bytes.Clone(records.Bytes()), 0x15)), // not a tuple
				records.Pack(tuple.Tuple{int64(5)}),
				records.Pack(tuple.Tuple{int64(50), int64(2)}),
				records.Pack(tuple.Tuple{int64(50), int64(3)}),
				records.Pack(tuple.Tuple{int64(95)}), // past the last record
			}
		},
		want: []tuple.Tuple{{int64(10)}, {int64(50)}},
	}, {
		// Format 4 stores an unsplit record at the bare primary key.
		name: "unsuffixed records", format: 4,
		shards: func(records subspace.Subspace) []fdb.Key {
			return []fdb.Key{records.Pack(tuple.Tuple{int64(50)})}
		},
		want: []tuple.Tuple{{int64(50)}},
	}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			db, backend := newShardMapDB(17)
			md, err := baseBuilder().SetSplitLongRecords(tc.split).Build()
			if err != nil {
				t.Fatalf("metadata: %v", err)
			}
			sb := NewStoreBuilder().SetMetaDataProvider(md).SetSubspace(subspace.FromBytes(tuple.Tuple{"pkbounds", tc.name}.Pack()))
			if tc.format != 0 {
				sb.SetFormatVersion(tc.format)
			}
			seedOrders(t, db, sb, []int64{10, 50, 90}, tc.large...)
			var got []tuple.Tuple
			var readVersion int64
			if _, err := db.Run(context.Background(), func(rtx *FDBRecordContext) (any, error) {
				store, err := sb.copyBuilder().SetContext(rtx).Open()
				if err != nil {
					return nil, err
				}
				backend.shards = tc.shards(store.recordsSubspace)
				if readVersion, err = rtx.GetReadVersion(); err != nil {
					return nil, err
				}
				got, err = store.GetPrimaryKeyBoundaries(TupleRangeAll)
				return nil, err
			}); err != nil {
				t.Fatalf("GetPrimaryKeyBoundaries: %v", err)
			}
			if !slices.EqualFunc(got, tc.want, func(a, b tuple.Tuple) bool { return bytes.Equal(a.Pack(), b.Pack()) }) {
				t.Fatalf("boundaries = %v, want %v", got, tc.want)
			}
			if backend.readVersions[0] != readVersion {
				t.Fatalf("shard map read at version %d, want the transaction's %d", backend.readVersions[0], readVersion)
			}
		})
	}
}

// newSimMutualIndexer seeds orders over a simulated store and returns a mutual indexer
// for a new price index, marked write-only.
func newSimMutualIndexer(t *testing.T, db *FDBDatabase, name string, md *RecordMetaData, limit int, ids []int64, large ...int64) *OnlineIndexer {
	t.Helper()
	sub := subspace.FromBytes(tuple.Tuple{"mutualbounds", name}.Pack())
	sb := NewStoreBuilder().SetMetaDataProvider(md).SetSubspace(sub)
	seedOrders(t, db, sb, ids, large...)
	// A new store's index starts readable; the build needs it disabled.
	if _, err := db.Run(context.Background(), func(rtx *FDBRecordContext) (any, error) {
		store, err := sb.copyBuilder().SetContext(rtx).Open()
		if err != nil {
			return nil, err
		}
		return nil, disableIndexes(store, md.GetIndex("Order$price"))
	}); err != nil {
		t.Fatalf("disable index: %v", err)
	}
	oi, err := NewOnlineIndexerBuilder().SetDatabase(db).SetMetaData(md).SetSubspace(sub).
		SetIndex(md.GetIndex("Order$price")).SetMutualIndexing().SetLimit(limit).Build()
	if err != nil {
		t.Fatalf("build indexer: %v", err)
	}
	oi.sessionHeartbeat = NewIndexingHeartbeat("mutual worker", 30_000, true, nil)
	if _, err := oi.markWriteOnly(context.Background()); err != nil {
		t.Fatalf("mark write-only: %v", err)
	}
	return oi
}

func priceIndexMetaData(t *testing.T, configure func(*RecordMetaDataBuilder)) *RecordMetaData {
	t.Helper()
	builder := baseBuilder().SetSplitLongRecords(true)
	if configure != nil {
		configure(builder)
	}
	builder.AddIndex("Order", NewIndex("Order$price", Field("price")))
	md, err := builder.Build()
	if err != nil {
		t.Fatalf("metadata: %v", err)
	}
	return md
}

// A shard boundary inside a split record becomes the record's primary key, so the
// fragment edge never splits a record.
func TestMutualFragmentsEndAtPrimaryKeys(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db, backend := newShardMapDB(23)
	md := priceIndexMetaData(t, nil)
	oi := newSimMutualIndexer(t, db, "split", md, 100, []int64{10, 50, 90}, 50)
	records := oi.subspace.Sub(RecordKey)
	backend.shards = []fdb.Key{records.Pack(tuple.Tuple{int64(50), int64(2)})}

	m := newMutualIndexBuilder(oi)
	for more := true; more; {
		var err error
		if _, more, err = m.buildMutual(ctx); err != nil {
			t.Fatalf("buildMutual: %v", err)
		}
	}
	want := [][]byte{rangeSetFirstKey, tuple.Tuple{int64(50)}.Pack(), rangeSetFinalKey}
	if !slices.EqualFunc(m.boundaries, want, bytes.Equal) {
		t.Fatalf("fragment edges = %x, want %x", m.boundaries, want)
	}
	if _, err := db.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
		store, err := oi.openStore(rtx)
		if err != nil {
			return nil, err
		}
		if _, err := store.MarkIndexReadable("Order$price"); err != nil {
			return nil, err
		}
		rows, err := AsList(ctx, store.ScanIndex(md.GetIndex("Order$price"), TupleRangeAll, nil, ForwardScan()))
		if err != nil {
			return nil, err
		}
		if len(rows) != 3 {
			t.Fatalf("index has %d entries, want 3", len(rows))
		}
		return nil, nil
	}); err != nil {
		t.Fatalf("scan index: %v", err)
	}
}

// The shard map is read over the indexed record types' records only, Java's
// computeRecordsRange.
func TestMutualBoundariesCoverIndexedRecordTypes(t *testing.T) {
	t.Parallel()
	db, backend := newShardMapDB(29)
	md := priceIndexMetaData(t, func(b *RecordMetaDataBuilder) {
		b.GetRecordType("Order").SetPrimaryKey(Concat(RecordTypeKey(), Field("order_id")))
	})
	oi := newSimMutualIndexer(t, db, "typed", md, 100, []int64{10})
	m := newMutualIndexBuilder(oi)
	if _, err := m.claimNextRange(context.Background()); err != nil {
		t.Fatalf("claimNextRange: %v", err)
	}
	orders := oi.subspace.Sub(RecordKey).Pack(tuple.Tuple{md.GetRecordType("Order").GetRecordTypeKey()})
	want := fdb.KeyRange{Begin: orders, End: fdb.Key(append(bytes.Clone(orders), 0xff))}
	if len(backend.requests) != 1 || !bytes.Equal(backend.requests[0].Begin.FDBKey(), want.Begin.FDBKey()) ||
		!bytes.Equal(backend.requests[0].End.FDBKey(), want.End.FDBKey()) {
		t.Fatalf("shard map requests = %v, want one over %v", backend.requests, want)
	}
}

// FULL builds a claimed fragment to its end before taking the next, so a lone builder
// never leaves FULL.
func TestMutualFullBuildsEachFragmentToItsEnd(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db, _ := newShardMapDB(31)
	ids := make([]int64, 100)
	for i := range ids {
		ids[i] = int64(i + 1)
	}
	oi := newSimMutualIndexer(t, db, "full", priceIndexMetaData(t, nil), 10, ids)
	oi.mutualBoundaries = [][]byte{tuple.Tuple{int64(50)}.Pack()}
	m := newMutualIndexBuilder(oi)
	var visited []int
	var total int64
	for {
		n, more, err := m.buildMutual(ctx)
		if err != nil {
			t.Fatalf("buildMutual: %v", err)
		}
		total += n
		if !more {
			break
		}
		if m.iterType != fragmentFull {
			t.Fatalf("left FULL after %d records", total)
		}
		if len(visited) == 0 || visited[len(visited)-1] != m.fragmentCur {
			visited = append(visited, m.fragmentCur)
		}
	}
	if total != 100 || len(visited) != 2 {
		t.Fatalf("built %d records over fragments %v, want 100 over each fragment once", total, visited)
	}
}

// Java's OnlineIndexerMutualTest.testFullyUnBuiltRange and testPartlyUnBuiltRange.
func TestMutualUnbuiltRanges(t *testing.T) {
	t.Parallel()
	r := func(begin, end byte) RangeSetRange { return RangeSetRange{Begin: []byte{begin}, End: []byte{end}} }
	missing := []RangeSetRange{r(0, 9), r(20, 29), r(40, 49)}
	for _, tc := range []struct {
		fragment RangeSetRange
		full     bool
		partly   *RangeSetRange
	}{
		{r(0, 9), true, &RangeSetRange{Begin: []byte{0}, End: []byte{9}}},
		{r(0, 8), true, &RangeSetRange{Begin: []byte{0}, End: []byte{8}}},
		{r(41, 49), true, &RangeSetRange{Begin: []byte{41}, End: []byte{49}}},
		{r(23, 24), true, &RangeSetRange{Begin: []byte{23}, End: []byte{24}}},
		{r(14, 24), false, &RangeSetRange{Begin: []byte{20}, End: []byte{24}}},
		{r(0, 12), false, &RangeSetRange{Begin: []byte{0}, End: []byte{9}}},
		{r(0, 100), false, &RangeSetRange{Begin: []byte{0}, End: []byte{9}}},
		{r(40, 100), false, &RangeSetRange{Begin: []byte{40}, End: []byte{49}}},
		{r(0, 10), false, &RangeSetRange{Begin: []byte{0}, End: []byte{9}}},
		{r(8, 10), false, &RangeSetRange{Begin: []byte{8}, End: []byte{9}}},
		{r(9, 10), false, nil},
		{r(100, 110), false, nil},
		{r(0, 20), false, &RangeSetRange{Begin: []byte{0}, End: []byte{9}}},
		{r(20, 44), false, &RangeSetRange{Begin: []byte{20}, End: []byte{29}}},
		{r(41, 50), false, &RangeSetRange{Begin: []byte{41}, End: []byte{49}}},
		{r(10, 11), false, nil},
		{r(33, 40), false, nil},
	} {
		full := fullyUnbuiltRange(missing, tc.fragment.Begin, tc.fragment.End)
		if (full != nil) != tc.full || (full != nil && (!bytes.Equal(full.Begin, tc.fragment.Begin) || !bytes.Equal(full.End, tc.fragment.End))) {
			t.Errorf("fullyUnbuiltRange(%x) = %v, want fragment: %t", tc.fragment, full, tc.full)
		}
		partly := partlyUnbuiltRange(missing, tc.fragment.Begin, tc.fragment.End)
		if (partly == nil) != (tc.partly == nil) || (partly != nil && (!bytes.Equal(partly.Begin, tc.partly.Begin) || !bytes.Equal(partly.End, tc.partly.End))) {
			t.Errorf("partlyUnbuiltRange(%x) = %v, want %v", tc.fragment, partly, tc.partly)
		}
	}
}

// A walk that finds nothing in FULL and then in ANY is Java's third-iteration failure.
func TestMutualThirdIterationFails(t *testing.T) {
	t.Parallel()
	db, _ := newShardMapDB(37)
	m := &mutualIndexBuilder{indexer: &OnlineIndexer{db: db}}
	m.setFragments([][]byte{tuple.Tuple{int64(50)}.Pack()})
	claim, err := m.nextClaim([]RangeSetRange{{Begin: []byte{0xff}, End: []byte{0xff, 0x00}}})
	var validation *IndexingValidationError
	if claim != nil || !errors.As(err, &validation) || validation.Message != "Mutual indexing failure - third iteration" {
		t.Fatalf("nextClaim = %v, %v; want the third-iteration failure", claim, err)
	}
	if m.iterType != fragmentRecover {
		t.Fatalf("iteration = %v, want RECOVER", m.iterType)
	}
}
