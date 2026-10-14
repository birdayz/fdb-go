package benchfetch

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/crc32"
	"os"
	"strings"
	"sync"
	"testing"

	"google.golang.org/protobuf/proto"

	"fdb.dev/gen/demopb"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/testutil/benchcluster"
)

func TestMain(m *testing.M) { os.Exit(benchcluster.Main(m)) }

// rowCount Orders are seeded per payload, with price = order_id, so a price
// range selects the same IDs and the index order is the primary-key order.
const rowCount = 1000

// splitPadding makes a split Order's serialized size just over the 100,000-byte
// split threshold, so each record is stored as exactly two chunks.
const splitPadding = 100_000

type payload struct {
	name    string
	split   bool
	padding int
	// chunks is the key-value count every seeded record must have.
	chunks int
}

var payloads = []payload{
	{name: "small", chunks: 1},
	{name: "split", split: true, padding: splitPadding, chunks: 2},
}

type fixture struct {
	payload payload
	db      *recordlayer.FDBDatabase
	md      *recordlayer.RecordMetaData
	ss      subspace.Subspace
	index   *recordlayer.Index
	// commonPrimaryKeyLength is the store's, as Java's remote fetch takes it.
	commonPrimaryKeyLength int
	// checksums[id] is the CRC-32 of the seeded Order's padding.
	checksums []uint32
}

var (
	dbOnce   sync.Once
	sharedDB *recordlayer.FDBDatabase
	fixtures sync.Map // payload name -> *fixture
	seedMu   sync.Mutex
)

func database(b *testing.B) *recordlayer.FDBDatabase {
	b.Helper()
	clusterFile := benchcluster.ClusterFile(b)
	dbOnce.Do(func() {
		fdb.MustAPIVersion(730)
		conn, err := fdb.OpenDatabase(clusterFile)
		if err != nil {
			b.Fatalf("open FDB: %v", err)
		}
		sharedDB = recordlayer.NewFDBDatabase(conn)
	})
	if sharedDB == nil {
		b.Fatal("FDB database initialization failed")
	}
	return sharedDB
}

func metadata(b *testing.B, split bool) *recordlayer.RecordMetaData {
	b.Helper()
	builder := recordlayer.NewRecordMetaDataBuilder().SetRecords(demopb.File_record_layer_demo_proto).
		SetSplitLongRecords(split)
	builder.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
	builder.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
	builder.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
	builder.AddIndex("Order", recordlayer.NewIndex("price_idx", recordlayer.Field("price")))
	md, err := builder.Build()
	if err != nil {
		b.Fatalf("build metadata: %v", err)
	}
	return md
}

// paddingFor is order id's deterministic, distinct padding.
func paddingFor(id int64, n int) string {
	if n == 0 {
		return ""
	}
	head := fmt.Sprintf("%06d:", id)
	return head + strings.Repeat(string(rune('a'+id%26)), n-len(head))
}

func orderFor(p payload, id int64) *demopb.Order {
	order := &demopb.Order{
		OrderId:  proto.Int64(id),
		Price:    proto.Int32(int32(id)),
		Quantity: proto.Int32(int32(id%7 + 1)),
	}
	if p.padding > 0 {
		order.Flower = &demopb.Flower{Type: proto.String(paddingFor(id, p.padding))}
	}
	return order
}

// loadFixture seeds payload p's store once per process: it clears the
// store's subspace, saves rowCount Orders and then loads every record back,
// checking its contents and chunk count. All of it is outside any timer.
func loadFixture(b *testing.B, p payload) *fixture {
	b.Helper()
	if f, ok := fixtures.Load(p.name); ok {
		return f.(*fixture)
	}
	seedMu.Lock()
	defer seedMu.Unlock()
	if f, ok := fixtures.Load(p.name); ok {
		return f.(*fixture)
	}
	ctx := context.Background()
	db := database(b)
	f := &fixture{
		payload:   p,
		db:        db,
		md:        metadata(b, p.split),
		ss:        subspace.FromBytes(tuple.Tuple{"benchfetch", p.name}.Pack()),
		checksums: make([]uint32, rowCount),
	}
	f.index = f.md.GetIndex("price_idx")
	for id := range f.checksums {
		f.checksums[id] = crc32.ChecksumIEEE([]byte(paddingFor(int64(id), p.padding)))
	}
	if _, err := db.Run(ctx, func(rc *recordlayer.FDBRecordContext) (any, error) {
		rc.Transaction().ClearRange(f.ss)
		_, err := recordlayer.NewStoreBuilder().SetContext(rc).SetMetaDataProvider(f.md).SetSubspace(f.ss).CreateOrOpen()
		return nil, err
	}); err != nil {
		b.Fatalf("create %s store: %v", p.name, err)
	}
	batch := 500
	if p.split {
		batch = 40 // about 4 MB per transaction
	}
	for first := 0; first < rowCount; first += batch {
		if _, err := db.Run(ctx, func(rc *recordlayer.FDBRecordContext) (any, error) {
			store, err := recordlayer.NewStoreBuilder().SetContext(rc).SetMetaDataProvider(f.md).SetSubspace(f.ss).Open()
			if err != nil {
				return nil, err
			}
			for id := first; id < min(rowCount, first+batch); id++ {
				if _, err := store.SaveRecord(orderFor(p, int64(id))); err != nil {
					return nil, err
				}
			}
			return nil, nil
		}); err != nil {
			b.Fatalf("seed %s orders from %d: %v", p.name, first, err)
		}
	}
	if _, err := db.Run(ctx, func(rc *recordlayer.FDBRecordContext) (any, error) {
		store, err := recordlayer.NewStoreBuilder().SetContext(rc).SetMetaDataProvider(f.md).SetSubspace(f.ss).Open()
		if err != nil {
			return nil, err
		}
		f.commonPrimaryKeyLength = store.GetCommonPrimaryKeyLength(f.index)
		for id := range rowCount {
			record, err := store.LoadRecord(tuple.Tuple{int64(id)})
			if err != nil {
				return nil, err
			}
			if record == nil {
				return nil, fmt.Errorf("order %d missing", id)
			}
			if record.KeyCount != p.chunks {
				return nil, fmt.Errorf("order %d stored as %d key-values, want %d", id, record.KeyCount, p.chunks)
			}
			if err := f.check(int64(id), record); err != nil {
				return nil, err
			}
		}
		return nil, nil
	}); err != nil {
		b.Fatalf("audit %s fixture: %v", p.name, err)
	}
	auditLog(b, map[string]any{
		"phase": "fixture", "payload": p.name, "payload_bytes": p.padding,
		"chunks": p.chunks, "rows": rowCount, "split": p.split, "prefix_bytes": len(f.ss.Bytes()),
	})
	fixtures.Store(p.name, f)
	return f
}

// auditLog logs one machine-readable, untimed receipt: FDB_BENCH_AUDIT and a
// JSON object carrying the benchmark name.
func auditLog(b *testing.B, fields map[string]any) {
	b.Helper()
	fields["case"] = b.Name()
	line, err := json.Marshal(fields)
	if err != nil {
		b.Fatalf("audit: %v", err)
	}
	b.Logf("FDB_BENCH_AUDIT %s", line)
}

// rowsChecksum is the CRC-32 of each row's ID and payload checksum, in order.
func (f *fixture) rowsChecksum(rows []*recordlayer.FDBIndexedRecord) uint32 {
	h := crc32.NewIEEE()
	for _, row := range rows {
		id := row.Record.Record.(*demopb.Order).GetOrderId()
		fmt.Fprintf(h, "%d|%d\n", id, f.checksums[id])
	}
	return h.Sum32()
}

// check reports whether record is order id exactly as seeded.
func (f *fixture) check(id int64, record *recordlayer.FDBStoredRecord[proto.Message]) error {
	if record == nil {
		return fmt.Errorf("order %d: no record", id)
	}
	order, ok := record.Record.(*demopb.Order)
	if !ok {
		return fmt.Errorf("order %d: record is %T", id, record.Record)
	}
	if order.GetOrderId() != id || order.GetPrice() != int32(id) || order.GetQuantity() != int32(id%7+1) {
		return fmt.Errorf("order %d: got id %d price %d quantity %d", id, order.GetOrderId(), order.GetPrice(), order.GetQuantity())
	}
	pad := order.GetFlower().GetType()
	if len(pad) != f.payload.padding || crc32.ChecksumIEEE([]byte(pad)) != f.checksums[id] {
		return fmt.Errorf("order %d: padding of %d bytes does not match the seeded %d", id, len(pad), f.payload.padding)
	}
	return nil
}
