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
// FETCH, and that is chosen by the streaming mode. The roll-up takes the client's
// default mode, ITERATOR, which is the mode Java's argument-less getRange resolves to.
// ITERATOR doubles its row budget each iteration and then SATURATES at the same
// iteration index libfdb_c clamps at (fdb/range_result.go iteratorMaxIteration): the
// progression runs 2, 4, 8, ... 1024 and stays at 1024 rows forever. That ceiling is
// what makes an unbounded fold safe under the default — without it the budget keeps
// doubling and the largest fetch grows with the number of groups, so a streaming fold
// would still be Θ(groups) at peak, one level below the fold but exactly the cost the
// fold was meant to remove.
//
// WHAT THIS PINS: the per-fetch row budget the roll-up's range read actually asks the
// client for, observed end to end — the assertions are on numbers the client reported
// through its own trace hook while GetSnapshotRecordCount ran, not on a mode enum read
// back out of a struct. And it pins the property rather than the constant: the peak
// fetch must be the SAME at two group counts an order of magnitude apart. A budget that
// keeps growing with the range fails that without any number here having to encode
// which mode is correct or where the ceiling sits.
//
// WHY BOTH SIZES ARE LARGE: the ceiling is only observable above it. A subspace smaller
// than the saturation point is bounded by the RANGE rather than by the mode — 600 groups
// peaks at a 512-row fetch simply because it runs out of keys at iteration 9, and that
// number moves with the cardinality even though nothing is wrong. Comparing a
// below-saturation size against an above-saturation one therefore measures the range,
// not the bound, and would report a growing budget on a correctly clamped client. Both
// sizes here sit past the saturation point, where the ceiling is the only thing that can
// decide the peak.
//
// WHAT THIS DOES NOT PIN: bytes, and therefore not memory. It measures the requested
// ROW budget; a fetch of bounded row count whose VALUES were huge would still be large,
// and this test would not notice. Record-count slots are 8-byte little-endian
// counters, so rows and bytes are proportional here — but that is a property of this
// subspace, not something asserted. It also says nothing about any other range read in
// the store: the recorder deliberately ignores every range outside the count subspace.
func TestRecordCountRollupFetchesInBoundedBatches(t *testing.T) {
	t.Parallel()

	// Two group counts an order of magnitude apart, both past the point where the
	// ITERATOR progression saturates (it reaches its ceiling after ~2046 cumulative
	// rows). With the clamp in place both peak at the ceiling. Strip the clamp and the
	// budget keeps doubling — 5000 groups peaks at a 4096-row fetch and 50000 at a
	// 32768-row one — so a progression that grows with the range cannot report the same
	// peak for both.
	const smallGroups = 5000
	const largeGroups = 50000

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
			"what the ITERATOR retains. Both sizes are past saturation, so the per-fetch "+
			"budget is no longer clamped: either the ITERATOR progression stopped "+
			"saturating (fdb/range_result.go iteratorMaxIteration) or this read moved to a "+
			"mode with no ceiling",
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

	// A third direction: equality and an absolute cap are both satisfied by any bounded
	// mode, so neither would notice this read quietly acquiring an explicit mode again
	// (StreamingModeSerial bounds too, at 500 rows). What identifies the mode is the
	// VALUE of the ceiling, and the client will say what its own ITERATOR ceiling is
	// rather than that value being written down here — a constant would just restate
	// fdb's row table in a second place and drift from it. The iteration number is past
	// saturation and the remaining-row budget is effectively unlimited, so this asks for
	// the saturated target and nothing else.
	//
	// This is the assertion that pins Java parity. Java reaches this read through
	// getRange with no mode argument (FDBTransaction.java:174-176, 430-432), so the peak
	// this roll-up produces has to be the peak of the client's DEFAULT mode.
	ceiling := int64(fdb.BatchSize(fdb.StreamingModeIterator, 64, 1<<30))
	if largePeak != ceiling {
		t.Fatalf("largest fetch on the record-count roll-up was %d rows at %d groups, but "+
			"the client's saturated ITERATOR target is %d rows: this read is no longer "+
			"running on the client's default streaming mode. Java takes the default here, "+
			"so an explicit mode on this range read is a divergence even when the mode it "+
			"names is also bounded", largePeak, largeGroups, ceiling)
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
