package chaos

import (
	"testing"
	"time"

	"github.com/birdayz/fdb-record-layer-go/gen"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer"
)

// buildConcurrentMetadata creates metadata with VALUE index + COUNT index + record counting.
// Suitable for concurrent chaos testing — all indexes are snapshot-derivable.
func buildConcurrentMetadata() *recordlayer.RecordMetaData {
	builder := recordlayer.NewRecordMetaDataBuilder()
	builder.SetRecords(gen.File_record_layer_demo_proto)
	builder.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
	builder.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
	builder.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
	builder.SetRecordCountKey(recordlayer.EmptyKey())
	builder.AddIndex("Order", recordlayer.NewIndex("conc_price_idx", recordlayer.Field("price")))
	builder.AddIndex("Order", recordlayer.NewCountIndex("conc_count_by_price",
		recordlayer.GroupAll(recordlayer.Field("price"))))
	md, err := builder.Build()
	if err != nil {
		panic("chaos: failed to build concurrent metadata: " + err.Error())
	}
	return md
}

// buildConcurrentSumMetadata creates metadata with VALUE + SUM indexes.
func buildConcurrentSumMetadata() *recordlayer.RecordMetaData {
	builder := recordlayer.NewRecordMetaDataBuilder()
	builder.SetRecords(gen.File_record_layer_demo_proto)
	builder.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
	builder.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
	builder.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
	builder.SetRecordCountKey(recordlayer.EmptyKey())
	builder.AddIndex("Order", recordlayer.NewIndex("conc_sum_price_idx", recordlayer.Field("price")))
	builder.AddIndex("Order", recordlayer.NewSumIndex("conc_sum_total_price",
		recordlayer.Ungrouped(recordlayer.Field("price"))))
	md, err := builder.Build()
	if err != nil {
		panic("chaos: failed to build concurrent sum metadata: " + err.Error())
	}
	return md
}

// TestConcurrentBasic runs 4 workers for 3 seconds with VALUE + COUNT indexes.
func TestConcurrentBasic(t *testing.T) {
	t.Parallel()
	RunConcurrent(t, testRealDB, buildConcurrentMetadata(), ConcurrentConfig{
		Seed:          30030,
		Workers:       4,
		Duration:      3 * time.Second,
		MaxPKs:        20,
		ValidateEvery: 500 * time.Millisecond,
	})
}

// TestConcurrentHighContention uses many workers on a tiny PK space
// to maximize transaction conflicts.
func TestConcurrentHighContention(t *testing.T) {
	t.Parallel()
	RunConcurrent(t, testRealDB, buildConcurrentMetadata(), ConcurrentConfig{
		Seed:          31031,
		Workers:       8,
		Duration:      3 * time.Second,
		MaxPKs:        5,
		ValidateEvery: 500 * time.Millisecond,
	})
}

// TestConcurrentWithSum verifies SUM index consistency under concurrent access.
func TestConcurrentWithSum(t *testing.T) {
	t.Parallel()
	RunConcurrent(t, testRealDB, buildConcurrentSumMetadata(), ConcurrentConfig{
		Seed:          32032,
		Workers:       4,
		Duration:      3 * time.Second,
		MaxPKs:        30,
		ValidateEvery: 500 * time.Millisecond,
	})
}

// TestConcurrentLongRun runs longer with more PKs to exercise steady-state behavior.
func TestConcurrentLongRun(t *testing.T) {
	t.Parallel()
	RunConcurrent(t, testRealDB, buildConcurrentMetadata(), ConcurrentConfig{
		Seed:          33033,
		Workers:       4,
		Duration:      5 * time.Second,
		MaxPKs:        50,
		ValidateEvery: 1 * time.Second,
	})
}
