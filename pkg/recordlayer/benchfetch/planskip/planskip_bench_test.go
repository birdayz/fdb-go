// Package planskip benchmarks a query index plan with a positive Skip under
// each fetch method. SCAN_AND_FETCH passes the Skip to its single-range index
// cursor, so skipped entries are never loaded; remote fetch keeps one Skip
// above its mapped reads. Requires the RFC-258 plan-level fetch method.
package planskip

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/crc32"
	"os"
	"slices"
	"sync"
	"testing"

	"google.golang.org/protobuf/proto"

	"fdb.dev/gen/demopb"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/indexfetch"
	"fdb.dev/pkg/recordlayer/query/executor"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
	"fdb.dev/pkg/testutil/benchcluster"
)

func TestMain(m *testing.M) { os.Exit(benchcluster.Main(m)) }

const (
	rowCount = 1000
	skip     = 3
)

type fixture struct {
	db *recordlayer.FDBDatabase
	md *recordlayer.RecordMetaData
	ss subspace.Subspace
}

var (
	fixtureOnce sync.Once
	shared      *fixture
)

// loadFixture seeds rowCount small Orders, price = order_id, once per
// process, clearing the subspace first and checking every record after.
func loadFixture(b *testing.B) *fixture {
	b.Helper()
	clusterFile := benchcluster.ClusterFile(b)
	fixtureOnce.Do(func() {
		ctx := context.Background()
		fdb.MustAPIVersion(730)
		conn, err := fdb.OpenDatabase(clusterFile)
		if err != nil {
			b.Fatalf("open FDB: %v", err)
		}
		builder := recordlayer.NewRecordMetaDataBuilder().SetRecords(demopb.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
		builder.AddIndex("Order", recordlayer.NewIndex("price_idx", recordlayer.Field("price")))
		md, err := builder.Build()
		if err != nil {
			b.Fatalf("build metadata: %v", err)
		}
		f := &fixture{db: recordlayer.NewFDBDatabase(conn), md: md, ss: subspace.FromBytes(tuple.Tuple{"benchfetch", "planskip"}.Pack())}
		_, err = f.db.Run(ctx, func(rc *recordlayer.FDBRecordContext) (any, error) {
			rc.Transaction().ClearRange(f.ss)
			store, err := recordlayer.NewStoreBuilder().SetContext(rc).SetMetaDataProvider(md).SetSubspace(f.ss).CreateOrOpen()
			if err != nil {
				return nil, err
			}
			for id := range int64(rowCount) {
				if _, err := store.SaveRecord(&demopb.Order{OrderId: proto.Int64(id), Price: proto.Int32(int32(id)), Quantity: proto.Int32(1)}); err != nil {
					return nil, err
				}
			}
			return nil, nil
		})
		if err != nil {
			b.Fatalf("seed orders: %v", err)
		}
		_, err = f.db.Run(ctx, func(rc *recordlayer.FDBRecordContext) (any, error) {
			store, err := recordlayer.NewStoreBuilder().SetContext(rc).SetMetaDataProvider(md).SetSubspace(f.ss).Open()
			if err != nil {
				return nil, err
			}
			for id := range int64(rowCount) {
				record, err := store.LoadRecord(tuple.Tuple{id})
				if err != nil {
					return nil, err
				}
				if err := checkOrder(id, record); err != nil {
					return nil, err
				}
			}
			return nil, nil
		})
		if err != nil {
			b.Fatalf("audit fixture: %v", err)
		}
		auditLog(b, map[string]any{"phase": "fixture", "rows": rowCount, "prefix_bytes": len(f.ss.Bytes())})
		shared = f
	})
	if shared == nil {
		b.Fatal("fixture initialization failed")
	}
	return shared
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

// rowsChecksum is the CRC-32 of the rows' IDs, in order.
func rowsChecksum(rows []executor.QueryResult) uint32 {
	h := crc32.NewIEEE()
	for _, row := range rows {
		fmt.Fprintf(h, "%d\n", row.Record.Record.(*demopb.Order).GetOrderId())
	}
	return h.Sum32()
}

func checkOrder(id int64, record *recordlayer.FDBStoredRecord[proto.Message]) error {
	if record == nil {
		return fmt.Errorf("order %d: no record", id)
	}
	order, ok := record.Record.(*demopb.Order)
	if !ok || order.GetOrderId() != id || order.GetPrice() != int32(id) || order.GetQuantity() != 1 {
		return fmt.Errorf("order %d: got %v", id, record.Record)
	}
	return nil
}

func priceBound(kind predicates.ComparisonType, price int64) *predicates.Comparison {
	return &predicates.Comparison{Type: kind, Operand: &values.ConstantValue{Value: price, Typ: values.NotNullLong}}
}

// cell is a single-range plan; first..first+count-1 are its forward entries.
type cell struct {
	name         string
	comparisons  []*predicates.Comparison
	first, count int
}

var cells = []cell{
	// Thirteen entries, ten returned after the Skip.
	{name: "rows10", comparisons: []*predicates.Comparison{
		priceBound(predicates.ComparisonGreaterThanEq, 100), priceBound(predicates.ComparisonLessThan, 113),
	}, first: 100, count: 10 + skip},
	// The whole index, 997 returned after the Skip.
	{name: "rows1000", first: 0, count: rowCount},
}

func (c cell) plan(b *testing.B, reverse bool, method indexfetch.Method) *plans.RecordQueryIndexPlan {
	b.Helper()
	scanRange := predicates.EmptyComparisonRange()
	for _, comparison := range c.comparisons {
		merged := scanRange.Merge(comparison)
		if !merged.Complete() {
			b.Fatalf("%s: comparison %v left a residual", c.name, comparison)
		}
		scanRange = merged.Range
	}
	orderType := executor.PositionalTypeForRecordLayout((&demopb.Order{}).ProtoReflect().Descriptor(), false)
	plan, err := plans.NewRecordQueryIndexPlan("price_idx", []*predicates.ComparisonRange{scanRange}, []string{"Order"}, orderType, reverse)
	if err != nil {
		b.Fatalf("%s: build plan: %v", c.name, err)
	}
	return plan.WithKeyComponentTypes([]values.Type{values.NullableInt}).WithIndexFetchMethod(method)
}

func (c cell) want(reverse bool) []int64 {
	ids := make([]int64, c.count)
	for i := range ids {
		ids[i] = int64(c.first + i)
	}
	if reverse {
		slices.Reverse(ids)
	}
	return ids[skip:]
}

// BenchmarkPlanSkip measures one query index plan execution per operation
// with ExecuteProperties.Skip = 3: a fresh transaction, store open,
// ExecutePlan and a full drain. Rows are checked outside the timer each
// operation; an untimed counter audit before and after proves the path.
//
// Sub-benchmarks: cell={rows10,rows1000}/dir={forward,reverse}/
// method={SCAN_AND_FETCH,USE_REMOTE_FETCH}.
func BenchmarkPlanSkip(b *testing.B) {
	f := loadFixture(b)
	for _, c := range cells {
		for _, reverse := range []bool{false, true} {
			dir := "forward"
			if reverse {
				dir = "reverse"
			}
			for _, method := range []indexfetch.Method{indexfetch.ScanAndFetch, indexfetch.UseRemoteFetch} {
				b.Run(fmt.Sprintf("cell=%s/dir=%s/method=%s", c.name, dir, method), func(b *testing.B) {
					benchPlan(b, f, c, reverse, method)
				})
			}
		}
	}
}

func benchPlan(b *testing.B, f *fixture, c cell, reverse bool, method indexfetch.Method) {
	ctx := context.Background()
	plan := c.plan(b, reverse, method)
	wantIDs := c.want(reverse)
	props := recordlayer.DefaultExecuteProperties()
	props.Skip = skip

	run := func(timer *recordlayer.StoreTimer, rows []executor.QueryResult) ([]executor.QueryResult, error) {
		tx, err := f.db.CreateWritableTransaction()
		if err != nil {
			return rows, err
		}
		rc := f.db.NewRecordContext(tx)
		defer rc.Cancel()
		if timer != nil {
			rc.SetTimer(timer)
		}
		store, err := recordlayer.NewStoreBuilder().SetContext(rc).SetSubspace(f.ss).SetMetaDataProvider(f.md).Open()
		if err != nil {
			return rows, err
		}
		cursor, err := executor.ExecutePlan(ctx, plan, store, executor.EmptyEvaluationContext(), nil, props)
		if err != nil {
			return rows, err
		}
		defer cursor.Close()
		for {
			next, err := cursor.OnNext(ctx)
			if err != nil {
				return rows, err
			}
			if !next.HasNext() {
				if reason := next.GetNoNextReason(); reason != recordlayer.SourceExhausted {
					return rows, fmt.Errorf("plan stopped with %v", reason)
				}
				return rows, nil
			}
			rows = append(rows, next.GetValue())
		}
	}
	verify := func(rows []executor.QueryResult) error {
		if len(rows) != len(wantIDs) {
			return fmt.Errorf("%d rows, want %d", len(rows), len(wantIDs))
		}
		for i, row := range rows {
			if err := checkOrder(wantIDs[i], row.Record); err != nil {
				return fmt.Errorf("row %d: %w", i, err)
			}
		}
		return nil
	}
	audit := func(phase string) {
		timer := recordlayer.NewStoreTimer()
		rows, err := run(timer, nil)
		if err == nil {
			err = verify(rows)
		}
		if err != nil {
			b.Fatalf("%s audit: %v", phase, err)
		}
		remote := timer.GetCount(recordlayer.CountRemoteFetch)
		entries := timer.GetCount(recordlayer.EventScanRemoteFetchEntry)
		indexScans := timer.GetCount(recordlayer.EventScanIndex)
		loads := timer.GetCount(recordlayer.EventLoadRecord)
		auditLog(b, map[string]any{
			"phase": phase, "method": method.String(), "payload": "small", "reverse": reverse, "skip": skip,
			"limit": 0, "rows": len(rows), "remote_openings": remote, "remote_entries": entries,
			"index_scans": indexScans, "record_loads": loads, "checksum": rowsChecksum(rows),
			"prefix_bytes": len(f.ss.Bytes()),
		})
		switch method {
		case indexfetch.UseRemoteFetch:
			// The mapped read returns every entry, the skipped ones included,
			// plus the exhausted result.
			if remote != 1 || entries != int64(len(rows)+skip+1) || indexScans != 0 || loads != 0 {
				b.Fatalf("%s audit: USE_REMOTE_FETCH did not keep Skip above one mapped read", phase)
			}
		default:
			// Skipped entries' records are never loaded.
			if remote != 0 || entries != 0 || indexScans < 1 || loads != int64(len(rows)) {
				b.Fatalf("%s audit: SCAN_AND_FETCH did not skip inside its index cursor", phase)
			}
		}
	}

	audit("before")
	rows := make([]executor.QueryResult, 0, len(wantIDs))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var err error
		rows, err = run(nil, rows[:0])
		b.StopTimer()
		if err == nil {
			err = verify(rows)
		}
		if err != nil {
			b.Fatalf("operation %d: %v", i, err)
		}
		clear(rows)
		b.StartTimer()
	}
	b.StopTimer()
	audit("after")
	b.ReportMetric(float64(len(wantIDs)), "rows/op")
	b.ReportMetric(float64(len(f.ss.Bytes())), "prefix-B")
}
