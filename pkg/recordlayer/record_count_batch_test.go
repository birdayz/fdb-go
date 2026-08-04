package recordlayer

import (
	"context"
	"testing"

	"fdb.dev/gen"
	"fdb.dev/pkg/dst"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/simfdb"
)

// This pins the BATCH dimension of the grouped record-count roll-up — the one
// dimension record_count_streaming_test.go explicitly says it cannot see ("Nothing
// here would notice if the FDB client's own per-batch buffering grew").
//
// The fold streams, so it retains nothing itself. But the range iterator retains
// whatever the current fetch returned, so peak memory is the size of the largest
// FETCH, and that is chosen by the streaming mode. The pure-Go client's ITERATOR mode
// — which is what fdb.RangeOptions{} selects — doubles the row budget every iteration
// with no saturation (fdb/range_result.go batchSize), so over the count subspace the
// largest fetch is still proportional to the number of groups. A streaming fold on top
// of an unbounded fetch is Θ(groups) at peak all the same, just one level down.
//
// WHAT THIS PINS: the per-fetch row budget the roll-up's range read actually asks the
// client for, observed end to end — the assertions are on numbers the client reported
// through its own trace hook while GetSnapshotRecordCount ran, not on a mode enum read
// back out of a struct. And it pins the property rather than the constant: the peak
// fetch must be the SAME at two group counts an order of magnitude apart. A mode whose
// budget grows with the range fails that without any number here having to encode which
// mode is correct.
//
// WHAT THIS DOES NOT PIN: bytes, and therefore not memory. It measures the requested
// ROW budget; a fetch of bounded row count whose VALUES were huge would still be large,
// and this test would not notice. Record-count slots are 8-byte little-endian
// counters, so rows and bytes are proportional here — but that is a property of this
// subspace, not something asserted. It also says nothing about any other range read in
// the store: the recorder deliberately ignores every range outside the count subspace.
func TestRecordCountRollupFetchesInBoundedBatches(t *testing.T) {
	t.Parallel()

	// Two group counts an order of magnitude apart. Under the client's ITERATOR
	// progression (2, 4, 8, ... rows) 600 groups peaks at a 512-row fetch and 5000
	// groups peaks at a 4096-row fetch, so a mode that grows with the range cannot
	// report the same peak for both.
	const smallGroups = 600
	const largeGroups = 5000

	peakFor := func(t *testing.T, groups int) (peak int64, fetches int, total int64) {
		t.Helper()
		ctx := context.Background()
		env := dst.NewSim(209)
		env.Buggify = dst.DisabledBuggifier()

		rec := &countRangeRecorder{}
		sub := subspace.FromBytes(tuple.Tuple{"countbatch", int64(groups)}.Pack())
		rec.prefix = sub.Sub(RecordCountKey).Bytes()

		backend := &countRangeRecordingBackend{BackendDatabase: simfdb.New(env), rec: rec}
		db := NewFDBDatabaseWithBackend(backend).SetEnv(env)

		// A GROUPED record-count key is what routes GetSnapshotRecordCount(empty)
		// into the roll-up branch: an ungrouped one is answered by a single Get and
		// never opens a range at all.
		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(Concat(RecordTypeKey(), Field("order_id")))
		builder.GetRecordType("Customer").SetPrimaryKey(Concat(RecordTypeKey(), Field("customer_id")))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(Concat(RecordTypeKey(), Field("id")))
		builder.SetRecordCountKey(RecordTypeKey())
		md, err := builder.Build()
		if err != nil {
			t.Fatalf("build metadata: %v", err)
		}

		// The count slots are written directly rather than via SaveRecord: the roll-up
		// reads the count subspace and nothing else, and seeding thousands of records
		// to produce thousands of GROUPS would need thousands of record types.
		if _, err := db.Run(ctx, func(rctx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rctx).SetMetaDataProvider(md).SetSubspace(sub).CreateOrOpen()
			if err != nil {
				return nil, err
			}
			countSubspace := store.subspace.Sub(RecordCountKey)
			for i := 1; i <= groups; i++ {
				rctx.Transaction().Set(countSubspace.Pack(tuple.Tuple{int64(i)}),
					encodeRecordCount(1))
			}
			return nil, nil
		}); err != nil {
			t.Fatalf("seed %d groups: %v", groups, err)
		}

		// Recording starts only now, so the seeding transaction's own reads (store
		// header, index state) cannot contribute a fetch.
		rec.arm()

		readerTx, err := db.CreateWritableTransaction()
		if err != nil {
			t.Fatalf("reader transaction: %v", err)
		}
		readerCtx := NewFDBRecordContext(readerTx, db.Env())
		store, err := NewStoreBuilder().
			SetContext(readerCtx).SetMetaDataProvider(md).SetSubspace(sub).Open()
		if err != nil {
			t.Fatalf("open store: %v", err)
		}
		total, err = store.GetSnapshotRecordCount(tuple.Tuple{})
		if err != nil {
			t.Fatalf("GetSnapshotRecordCount: %v", err)
		}
		return rec.peak(), rec.fetchCount(), total
	}

	smallPeak, smallFetches, smallTotal := peakFor(t, smallGroups)
	largePeak, largeFetches, largeTotal := peakFor(t, largeGroups)

	// A roll-up that read nothing would report a peak of 0 at both sizes and pass the
	// comparison below for the wrong reason.
	if smallTotal != smallGroups || largeTotal != largeGroups {
		t.Fatalf("roll-up totals = %d and %d, want %d and %d: the read under measurement "+
			"did not sum the seeded groups, so the batch numbers describe some other read",
			smallTotal, largeTotal, smallGroups, largeGroups)
	}
	if smallFetches == 0 || largeFetches == 0 {
		t.Fatalf("no batched fetch was observed on the count subspace (%d and %d): the "+
			"roll-up either materialized the range in one call or read it somewhere this "+
			"recorder cannot see, and the bound below is then vacuous",
			smallFetches, largeFetches)
	}

	if smallPeak != largePeak {
		t.Fatalf("largest fetch on the record-count roll-up was %d rows at %d groups but "+
			"%d rows at %d groups: the per-fetch row budget GROWS with the size of the "+
			"count subspace, so the range iterator retains a batch proportional to the "+
			"cardinality of the record-count key. The streaming fold does not bound peak "+
			"memory on its own — it bounds what the FOLD retains, and the largest fetch is "+
			"what the ITERATOR retains. The range read must carry a streaming mode this "+
			"client actually bounds",
			smallPeak, smallGroups, largePeak, largeGroups)
	}

	// A second, independent direction: equal peaks would also be satisfied by a mode
	// that fetched the entire range in one go at both sizes. The bound has to be an
	// absolute one as well, and it has to be smaller than the smaller group count.
	if smallPeak <= 0 || smallPeak >= smallGroups {
		t.Fatalf("largest fetch on the record-count roll-up was %d rows for %d groups: a "+
			"single fetch spanning the whole count subspace is the unbounded shape under "+
			"a different name", smallPeak, smallGroups)
	}
}

// countRangeRecorder records the per-fetch row budget of every range read issued
// against one key prefix. It observes the client's OWN trace hook, so the numbers are
// what the client asked its backend for, not a re-derivation of the batching rule.
type countRangeRecorder struct {
	prefix  []byte
	armed   bool
	fetches []int64
}

func (r *countRangeRecorder) arm() { r.armed = true }

func (r *countRangeRecorder) peak() int64 {
	var peak int64
	for _, n := range r.fetches {
		if n > peak {
			peak = n
		}
	}
	return peak
}

func (r *countRangeRecorder) fetchCount() int { return len(r.fetches) }

// observe wraps a RangeResult when it covers the watched prefix. Everything else is
// returned untouched, which is what keeps this measurement scoped to the roll-up and
// blind to (for example) index scans that share the same transaction.
func (r *countRangeRecorder) observe(rr fdb.RangeResult, rng fdb.Range) fdb.RangeResult {
	if !r.armed {
		return rr
	}
	kr, ok := rng.(fdb.KeyRange)
	if !ok || len(r.prefix) == 0 || !hasPrefix(kr.Begin.FDBKey(), r.prefix) {
		return rr
	}
	return observedRangeResult{RangeResult: rr, rec: r}
}

func hasPrefix(k fdb.Key, prefix []byte) bool {
	if len(k) < len(prefix) {
		return false
	}
	for i := range prefix {
		if k[i] != prefix[i] {
			return false
		}
	}
	return true
}

type observedRangeResult struct {
	fdb.RangeResult
	rec *countRangeRecorder
}

func (o observedRangeResult) Iterator() fdb.RangeIterator {
	it := o.RangeResult.Iterator()
	it.SetTraceLog(func(_, requested, returned int, _ bool, _ error) {
		if returned == 0 {
			return // the trailing empty fetch retains nothing
		}
		o.rec.fetches = append(o.rec.fetches, int64(requested))
	})
	return it
}

// The three wrappers below exist only to reach the RangeResult the production path
// builds internally. Each embeds the interface it decorates, so every method except
// the ones that hand out a range or a nested transaction view is the real thing.

type countRangeRecordingBackend struct {
	fdb.BackendDatabase
	rec *countRangeRecorder
}

func (b *countRangeRecordingBackend) Transact(f func(fdb.WritableTransaction) (any, error)) (any, error) {
	return b.BackendDatabase.Transact(func(tx fdb.WritableTransaction) (any, error) {
		return f(&countRangeRecordingTxn{WritableTransaction: tx, rec: b.rec})
	})
}

func (b *countRangeRecordingBackend) ReadTransact(f func(fdb.ReadTransaction) (any, error)) (any, error) {
	return b.BackendDatabase.ReadTransact(func(tx fdb.ReadTransaction) (any, error) {
		return f(&countRangeRecordingRead{ReadTransaction: tx, rec: b.rec})
	})
}

func (b *countRangeRecordingBackend) CreateWritableTransaction() (fdb.WritableTransaction, error) {
	tx, err := b.BackendDatabase.CreateWritableTransaction()
	if err != nil {
		return nil, err
	}
	return &countRangeRecordingTxn{WritableTransaction: tx, rec: b.rec}, nil
}

type countRangeRecordingTxn struct {
	fdb.WritableTransaction
	rec *countRangeRecorder
}

func (t *countRangeRecordingTxn) GetRange(r fdb.Range, options fdb.RangeOptions) fdb.RangeResult {
	return t.rec.observe(t.WritableTransaction.GetRange(r, options), r)
}

// Snapshot is the one that matters: the roll-up reads through
// store.context.Transaction().Snapshot().
func (t *countRangeRecordingTxn) Snapshot() fdb.ReadTransaction {
	return &countRangeRecordingRead{ReadTransaction: t.WritableTransaction.Snapshot(), rec: t.rec}
}

type countRangeRecordingRead struct {
	fdb.ReadTransaction
	rec *countRangeRecorder
}

func (t *countRangeRecordingRead) GetRange(r fdb.Range, options fdb.RangeOptions) fdb.RangeResult {
	return t.rec.observe(t.ReadTransaction.GetRange(r, options), r)
}

func (t *countRangeRecordingRead) Snapshot() fdb.ReadTransaction {
	return &countRangeRecordingRead{ReadTransaction: t.ReadTransaction.Snapshot(), rec: t.rec}
}
