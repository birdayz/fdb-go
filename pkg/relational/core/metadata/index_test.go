package metadata

import (
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades"
)

// Compile-time check that RecordLayerIndex satisfies cascades.IndexDef.
var _ cascades.IndexDef = (*RecordLayerIndex)(nil)

func TestRecordLayerIndex_IndexDef(t *testing.T) {
	t.Parallel()
	idx := recordlayer.NewIndex("Order$status", recordlayer.Concat(
		recordlayer.Field("status"),
		recordlayer.Field("date"),
	))
	rli := newIndex(idx, "Order")

	if rli.IndexName() != "Order$status" {
		t.Fatalf("IndexName()=%q", rli.IndexName())
	}
	cols := rli.IndexColumnNames()
	if len(cols) != 2 || cols[0] != "status" || cols[1] != "date" {
		t.Fatalf("IndexColumnNames()=%v", cols)
	}
	types := rli.IndexRecordTypes()
	if len(types) != 1 || types[0] != "Order" {
		t.Fatalf("IndexRecordTypes()=%v", types)
	}
	if rli.IndexIsUnique() {
		t.Fatal("should not be unique")
	}
}
