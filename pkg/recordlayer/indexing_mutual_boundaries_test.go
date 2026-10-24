package recordlayer

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"fdb.dev/gen/demopb"
	"fdb.dev/pkg/dst"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/simfdb"
	"google.golang.org/protobuf/proto"
)

// shardMapBackend is simfdb with an injected shard map: LocalityGetBoundaryKeys returns
// the injected keys inside the requested range, or shardErr, and records each request.
type shardMapBackend struct {
	fdb.BackendDatabase
	shards       []fdb.Key
	shardErr     error
	requests     []fdb.KeyRange
	readVersions []int64
	// transactions counts Transact calls; call failAt fails with failErr after its body.
	transactions int
	failAt       int
	failErr      error
}

func (b *shardMapBackend) Transact(fn func(fdb.WritableTransaction) (any, error)) (any, error) {
	b.transactions++
	call := b.transactions
	return b.BackendDatabase.Transact(func(tx fdb.WritableTransaction) (any, error) {
		value, err := fn(tx)
		if err == nil && call == b.failAt {
			return nil, b.failErr
		}
		return value, err
	})
}

func (b *shardMapBackend) LocalityGetBoundaryKeys(r fdb.ExactRange, _ int, readVersion int64) ([]fdb.Key, error) {
	begin, end := r.FDBRangeKeys()
	b.requests = append(b.requests, fdb.KeyRange{Begin: begin, End: end})
	b.readVersions = append(b.readVersions, readVersion)
	if b.shardErr != nil {
		return nil, b.shardErr
	}
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
		format int32   // 0: the current format
		ids    []int64 // nil: 10, 50 and 90
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
	}, {
		// More boundaries than lookups in flight, with a repeat across the window's edge.
		name: "pipelined lookups", ids: []int64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15},
		shards: func(records subspace.Subspace) []fdb.Key {
			var keys []fdb.Key
			for i := int64(1); i <= 14; i++ {
				keys = append(keys, records.Pack(tuple.Tuple{i}))
				if i == 10 {
					keys = append(keys, records.Pack(tuple.Tuple{i, int64(0)}))
				}
			}
			return keys
		},
		want: []tuple.Tuple{
			{int64(1)},
			{int64(2)},
			{int64(3)},
			{int64(4)},
			{int64(5)},
			{int64(6)},
			{int64(7)},
			{int64(8)},
			{int64(9)},
			{int64(10)},
			{int64(11)},
			{int64(12)},
			{int64(13)},
			{int64(14)},
		},
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
			ids := tc.ids
			if ids == nil {
				ids = []int64{10, 50, 90}
			}
			seedOrders(t, db, sb, ids, tc.large...)
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
	chunks := 0
	for {
		n, more, err := m.buildMutual(ctx)
		if err != nil {
			t.Fatalf("buildMutual: %v", err)
		}
		total += n
		if !more {
			break
		}
		if chunks++; n > 10 {
			t.Fatalf("chunk of %d records, limit 10", n)
		}
		if m.iterType != fragmentFull {
			t.Fatalf("left FULL after %d records", total)
		}
		if len(visited) == 0 || visited[len(visited)-1] != m.fragmentCur {
			visited = append(visited, m.fragmentCur)
		}
	}
	// 49 and 51 records in chunks of up to 10: 5 + 6 chunks.
	if total != 100 || chunks != 11 || len(visited) != 2 {
		t.Fatalf("built %d records in %d chunks over fragments %v, want 100 in 11 over each fragment once", total, chunks, visited)
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

// A backend without a shard map gives one fragment; any other shard-map error fails the claim.
func TestMutualBoundaryReadErrors(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	t.Run("no shard map", func(t *testing.T) {
		t.Parallel()
		db, backend := newShardMapDB(53)
		oi := newSimMutualIndexer(t, db, "noshardmap", priceIndexMetaData(t, nil), 100, []int64{10, 50})
		backend.shardErr = &BackendCapabilityError{Op: "LocalityGetBoundaryKeys"}
		m := newMutualIndexBuilder(oi)
		var total int64
		for more := true; more; {
			n, nextMore, err := m.buildMutual(ctx)
			if err != nil {
				t.Fatalf("buildMutual: %v", err)
			}
			total, more = total+n, nextMore
		}
		if want := [][]byte{rangeSetFirstKey, rangeSetFinalKey}; total != 2 || !slices.EqualFunc(m.boundaries, want, bytes.Equal) {
			t.Fatalf("built %d records over edges %x, want 2 over one fragment", total, m.boundaries)
		}
	})
	t.Run("shard map failure", func(t *testing.T) {
		t.Parallel()
		db, backend := newShardMapDB(59)
		oi := newSimMutualIndexer(t, db, "shardmapfails", priceIndexMetaData(t, nil), 100, []int64{10})
		backend.shardErr = errors.New("shard map unavailable")
		m := newMutualIndexBuilder(oi)
		if _, _, err := m.buildMutual(ctx); !errors.Is(err, backend.shardErr) || m.boundaries != nil {
			t.Fatalf("buildMutual = %v with edges %x, want the shard-map error and no fragments", err, m.boundaries)
		}
	})
}

// A claimed chunk too large for one transaction is retried at a lower limit, where Java's
// anyJumper fails the build (DIVERGENCES.md, "Mutual indexing has no anyJumper").
func TestMutualTooLargeChunkRetriesAtALowerLimit(t *testing.T) {
	t.Parallel()
	db, backend := newShardMapDB(61)
	ids := make([]int64, 20)
	for i := range ids {
		ids[i] = int64(i + 1)
	}
	oi := newSimMutualIndexer(t, db, "toolarge", priceIndexMetaData(t, nil), 10, ids)
	m := newMutualIndexBuilder(oi)
	// The claim commits; its first chunk fails.
	backend.failAt, backend.failErr = backend.transactions+2, fdb.Error{Code: 2101}
	n, more, err := oi.buildRangeWithRetries(context.Background(), m.buildMutual)
	if err != nil || !more || n < 1 || n >= 10 {
		t.Fatalf("buildRangeWithRetries = (%d, %t, %v), want a chunk below the limit of 10", n, more, err)
	}
}

// A builder keeps a claim a peer built part of, and moves on from one a peer finished, in FULL.
func TestMutualClaimBuiltByAPeer(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	db, _ := newShardMapDB(67)
	ids := make([]int64, 100)
	for i := range ids {
		ids[i] = int64(i + 1)
	}
	md := priceIndexMetaData(t, nil)
	oi := newSimMutualIndexer(t, db, "peer", md, 10, ids)
	oi.mutualBoundaries = [][]byte{tuple.Tuple{int64(50)}.Pack()}
	pk := func(id int64) []byte { return tuple.Tuple{id}.Pack() }
	rangeSet := func(rtx *FDBRecordContext) *IndexingRangeSet {
		store, err := oi.openStore(rtx)
		if err != nil {
			t.Fatalf("open store: %v", err)
		}
		return NewIndexingRangeSet(store.subspace, md.GetIndex("Order$price"))
	}
	peerBuilds := func(begin, end []byte) {
		t.Helper()
		if _, err := db.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			return rangeSet(rtx).InsertRange(rtx.Transaction(), begin, end, true)
		}); err != nil {
			t.Fatalf("peer insert: %v", err)
		}
	}
	m := newMutualIndexBuilder(oi)
	m.fragmentCur, m.fragmentFirst = 0, 0
	if more, err := m.claimNextRange(ctx); err != nil || !more {
		t.Fatalf("claimNextRange = %t, %v", more, err)
	}
	// The peer finishes the claimed fragment: the builder claims the next one.
	peerBuilds(rangeSetFirstKey, pk(50))
	second := RangeSetRange{Begin: pk(50), End: rangeSetFinalKey}
	n, more, err := m.buildMutual(ctx)
	if err != nil || n != 10 || !more || m.claimed == nil || !bytes.Equal(m.claimed.Begin, second.Begin) {
		t.Fatalf("buildMutual = (%d, %t, %v) with claim %x, want 10 records of the next fragment", n, more, err, m.claimed)
	}
	// The peer builds the middle of the claim: the builder keeps it and builds around it.
	peerBuilds(pk(70), pk(80))
	if _, err := db.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
		missing, err := rangeSet(rtx).ListMissingRangesInBytes(rtx.Transaction(), second.Begin, second.End)
		want := []RangeSetRange{{Begin: pk(60), End: pk(70)}, {Begin: pk(80), End: rangeSetFinalKey}}
		if err == nil && !slices.EqualFunc(missing, want, func(a, b RangeSetRange) bool {
			return bytes.Equal(a.Begin, b.Begin) && bytes.Equal(a.End, b.End)
		}) {
			t.Fatalf("missing in the claim = %x, want %x", missing, want)
		}
		return nil, err
	}); err != nil {
		t.Fatalf("list missing: %v", err)
	}
	total := n
	for m.claimed != nil {
		if !bytes.Equal(m.claimed.Begin, second.Begin) || !bytes.Equal(m.claimed.End, second.End) || m.iterType != fragmentFull {
			t.Fatalf("claim %x in phase %v, want %x in FULL", m.claimed, m.iterType, second)
		}
		if n, _, err = m.buildMutual(ctx); err != nil {
			t.Fatalf("buildMutual: %v", err)
		}
		total += n
	}
	if _, more, err = m.buildMutual(ctx); err != nil || more || total != 41 {
		t.Fatalf("built %d records, then more = %t, %v; want the 41 the peer left and no more", total, more, err)
	}
}

// The lookups are snapshot reads: a concurrent save between a boundary and the record it maps
// to does not conflict the caller. A context without a database has no shard map.
func TestGetPrimaryKeyBoundariesTakesNoReadConflict(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db, backend := newShardMapDB(71)
	md, err := baseBuilder().Build()
	if err != nil {
		t.Fatalf("metadata: %v", err)
	}
	sb := NewStoreBuilder().SetMetaDataProvider(md).SetSubspace(subspace.FromBytes(tuple.Tuple{"pkbounds", "snapshot"}.Pack()))
	seedOrders(t, db, sb, []int64{10, 50})
	attempts := 0
	if _, err := db.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
		attempts++
		store, err := sb.copyBuilder().SetContext(rtx).Open()
		if err != nil {
			return nil, err
		}
		backend.shards = []fdb.Key{store.recordsSubspace.Pack(tuple.Tuple{int64(5)})}
		got, err := store.GetPrimaryKeyBoundaries(TupleRangeAll)
		if err != nil || len(got) != 1 || !bytes.Equal(got[0].Pack(), tuple.Tuple{int64(10)}.Pack()) {
			t.Fatalf("GetPrimaryKeyBoundaries = %v, %v; want [(10)]", got, err)
		}
		rtx.Transaction().Set(fdb.Key("pkbounds-snapshot-marker"), []byte{1})
		if attempts == 1 {
			seedOrders(t, db, sb, []int64{7})
		}
		return nil, nil
	}); err != nil || attempts != 1 {
		t.Fatalf("Run = %v after %d attempts, want one attempt despite the concurrent save", err, attempts)
	}

	tx, err := db.CreateWritableTransaction()
	if err != nil {
		t.Fatalf("transaction: %v", err)
	}
	store, err := sb.copyBuilder().SetContext(NewFDBRecordContext(tx, db.Env())).Open()
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	var unsupported *BackendCapabilityError
	if _, err := store.GetPrimaryKeyBoundaries(TupleRangeAll); !errors.As(err, &unsupported) {
		t.Fatalf("GetPrimaryKeyBoundaries without a database = %v, want BackendCapabilityError", err)
	}
}

// A resumed build whose fragments are all partly built finishes them in ANY.
func TestMutualAnyBuildsPartlyBuiltFragments(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db, _ := newShardMapDB(73)
	ids := make([]int64, 100)
	for i := range ids {
		ids[i] = int64(i + 1)
	}
	md := priceIndexMetaData(t, nil)
	oi := newSimMutualIndexer(t, db, "any", md, 10, ids)
	oi.mutualBoundaries = [][]byte{tuple.Tuple{int64(50)}.Pack()}
	// An earlier build left one chunk in each fragment.
	for fragment := range 2 {
		earlier := newMutualIndexBuilder(oi)
		earlier.fragmentCur, earlier.fragmentFirst = fragment, fragment
		if n, _, err := earlier.buildMutual(ctx); err != nil || n != 10 {
			t.Fatalf("earlier chunk in fragment %d = %d, %v", fragment, n, err)
		}
	}
	m := newMutualIndexBuilder(oi)
	var total int64
	for more := true; more; {
		n, nextMore, err := m.buildMutual(ctx)
		if err != nil {
			t.Fatalf("buildMutual: %v", err)
		}
		total, more = total+n, nextMore
	}
	if total != 80 || m.iterType != fragmentAny {
		t.Fatalf("built %d records ending in phase %v, want the 80 left, in ANY", total, m.iterType)
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
		if err == nil && len(rows) != 100 {
			t.Fatalf("index has %d entries, want 100", len(rows))
		}
		return nil, err
	}); err != nil {
		t.Fatalf("scan index: %v", err)
	}
}
