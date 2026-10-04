package catalog

import (
	"testing"

	"google.golang.org/protobuf/proto"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/relational/core/metadata"
)

// Java's builder numbers indexes in registration order (RecordMetaDataBuilder
// addIndexCommon, ++version per index), which the built proto keeps in each
// index's last-modified version while listing indexes by name. A carry numbers
// the new indexes in that registration order, not in list order.
func TestCarryNumbering_NewIndexesInRegistrationOrder(t *testing.T) {
	t.Parallel()
	stored, err := buildVersionedTemplate(t, "order", 1).(*metadata.RecordLayerSchemaTemplate).Underlying().ToProto()
	if err != nil {
		t.Fatal(err)
	}
	stored.Version = proto.Int32(5)
	built := proto.Clone(stored).(*gen.MetaData)
	index := func(name, recordType string, registered int32) *gen.Index {
		return &gen.Index{
			Name:                proto.String(name),
			RecordType:          []string{recordType},
			RootExpression:      recordlayer.Field("order_id").ToKeyExpression(),
			Type:                proto.String(recordlayer.IndexTypeValue),
			SubspaceKey:         tuple.Tuple{name}.Pack(),
			AddedVersion:        proto.Int32(registered),
			LastModifiedVersion: proto.Int32(registered),
		}
	}
	// Listed by name; Z_IDX was registered first.
	built.Indexes = []*gen.Index{index("A_IDX", "Order", 2), index("Z_IDX", "Order", 1)}

	carried, _, err := carryNumbering(stored, built)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]int32{}
	for _, idx := range carried.GetIndexes() {
		got[idx.GetName()] = idx.GetAddedVersion()
		if idx.GetLastModifiedVersion() != idx.GetAddedVersion() {
			t.Errorf("%s: last-modified %d, added %d", idx.GetName(), idx.GetLastModifiedVersion(), idx.GetAddedVersion())
		}
	}
	if got["Z_IDX"] != 6 || got["A_IDX"] != 7 {
		t.Fatalf("added versions %v, want Z_IDX=6 A_IDX=7 (registration order)", got)
	}
	if carried.GetVersion() != 7 {
		t.Fatalf("metadata version %d, want 7", carried.GetVersion())
	}
	if names := []string{carried.GetIndexes()[0].GetName(), carried.GetIndexes()[1].GetName()}; names[0] != "A_IDX" || names[1] != "Z_IDX" {
		t.Fatalf("index list order %v changed, want the built order", names)
	}
}
