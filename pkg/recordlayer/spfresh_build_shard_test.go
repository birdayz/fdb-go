package recordlayer

import (
	"testing"

	"github.com/birdayz/fdb-record-layer-go/gen"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/tuple"
)

// TestSPFreshShardGate pins the prefix-safety gate predicate (RFC-103): a
// bare-PK multi-type store (collision_test.go pathology) is UNSAFE ⇒ S=1; a
// record-type-prefixed (or single-type) store is SAFE ⇒ may shard.
func TestSPFreshShardGate(t *testing.T) {
	t.Parallel()
	shardSafe := func(md *RecordMetaData) bool {
		return len(md.RecordTypes()) == 1 || md.PrimaryKeyHasRecordTypePrefix()
	}

	ub := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
	ub.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
	ub.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
	ub.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
	umd, err := ub.Build()
	if err != nil {
		t.Fatalf("build unsafe metadata: %v", err)
	}
	if shardSafe(umd) {
		t.Fatal("bare-PK multi-type store must be shard-UNSAFE (falls back to S=1)")
	}

	sb := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
	sb.GetRecordType("Order").SetPrimaryKey(Concat(RecordTypeKey(), Field("order_id")))
	sb.GetRecordType("Customer").SetPrimaryKey(Concat(RecordTypeKey(), Field("customer_id")))
	sb.GetRecordType("TypedRecord").SetPrimaryKey(Concat(RecordTypeKey(), Field("id")))
	smd, err := sb.Build()
	if err != nil {
		t.Fatalf("build safe metadata: %v", err)
	}
	if !shardSafe(smd) {
		t.Fatal("record-type-prefixed store must be shard-SAFE")
	}
}

// TestSPFreshShardRanges pins the RFC-103 tiling contract: half-open PK ranges,
// gapless (adjacent shards share a boundary, no gap, no overlap), with ±∞ ends.
func TestSPFreshShardRanges(t *testing.T) {
	t.Parallel()

	// No boundaries ⇒ a single full-range shard (the serial scan / safe floor).
	full := spfreshShardRanges(nil)
	if len(full) != 1 {
		t.Fatalf("nil boundaries: want 1 shard, got %d", len(full))
	}
	if full[0].lowEP != EndpointTypeTreeStart || full[0].highEP != EndpointTypeTreeEnd {
		t.Fatalf("nil boundaries: want [TreeStart,TreeEnd], got lowEP=%v highEP=%v", full[0].lowEP, full[0].highEP)
	}
	if full[0].low != nil || full[0].high != nil {
		t.Fatalf("nil boundaries: want nil low/high, got %v/%v", full[0].low, full[0].high)
	}

	b := []tuple.Tuple{{int64(10)}, {int64(20)}, {int64(30)}}
	rs := spfreshShardRanges(b)
	if len(rs) != len(b)+1 {
		t.Fatalf("%d boundaries: want %d shards, got %d", len(b), len(b)+1, len(rs))
	}

	// Shard 0: [TreeStart, b0).
	if rs[0].lowEP != EndpointTypeTreeStart || rs[0].highEP != EndpointTypeRangeExclusive || !sameTuple(rs[0].high, b[0]) {
		t.Fatalf("shard 0 must be [TreeStart, %v): got %+v", b[0], rs[0])
	}
	// Last shard: [b_{n-1}, TreeEnd).
	last := rs[len(rs)-1]
	if last.lowEP != EndpointTypeRangeInclusive || last.highEP != EndpointTypeTreeEnd || !sameTuple(last.low, b[len(b)-1]) {
		t.Fatalf("last shard must be [%v, TreeEnd): got %+v", b[len(b)-1], last)
	}
	// Interior shards are [b_i, b_{i+1}) inclusive/exclusive.
	for i := 1; i < len(rs)-1; i++ {
		if rs[i].lowEP != EndpointTypeRangeInclusive || rs[i].highEP != EndpointTypeRangeExclusive {
			t.Fatalf("interior shard %d endpoints: got lowEP=%v highEP=%v", i, rs[i].lowEP, rs[i].highEP)
		}
	}
	// Gapless + no overlap: each shard's high is the next shard's low.
	for i := 0; i+1 < len(rs); i++ {
		if !sameTuple(rs[i].high, rs[i+1].low) {
			t.Fatalf("gap/overlap between shard %d (high=%v) and %d (low=%v)", i, rs[i].high, i+1, rs[i+1].low)
		}
	}
}

// TestSPFreshPKSampler pins the bounded-memory decimation + quantile boundaries:
// strictly ascending interior boundaries, bounded buffer, and the degrade cases.
func TestSPFreshPKSampler(t *testing.T) {
	t.Parallel()

	// Small cap forces repeated decimation over many observations.
	s := newSPFreshPKSampler(8)
	const n = 1000
	for i := 0; i < n; i++ {
		s.observe(tuple.Tuple{int64(i)})
	}
	if len(s.pks) > 2*s.cap {
		t.Fatalf("decimation must keep the buffer ≤ 2·cap=%d, got %d", 2*s.cap, len(s.pks))
	}
	// Retained PKs stay strictly ascending (scan order + uniqueness).
	for i := 0; i+1 < len(s.pks); i++ {
		if s.pks[i][0].(int64) >= s.pks[i+1][0].(int64) {
			t.Fatalf("reservoir not strictly ascending at %d: %v then %v", i, s.pks[i], s.pks[i+1])
		}
	}

	b := s.boundaries(8)
	if len(b) != 7 {
		t.Fatalf("boundaries(8) over %d records: want 7, got %d", n, len(b))
	}
	for i := 0; i+1 < len(b); i++ {
		if b[i][0].(int64) >= b[i+1][0].(int64) {
			t.Fatalf("boundaries not strictly ascending at %d: %v then %v", i, b[i], b[i+1])
		}
	}

	// Degrade cases ⇒ nil (single serial shard).
	if got := s.boundaries(1); got != nil {
		t.Fatalf("shards=1 must yield nil boundaries, got %v", got)
	}
	few := newSPFreshPKSampler(8)
	for i := 0; i < 3; i++ {
		few.observe(tuple.Tuple{int64(i)})
	}
	if got := few.boundaries(8); got != nil {
		t.Fatalf("fewer candidates than shards must yield nil, got %v", got)
	}
}
