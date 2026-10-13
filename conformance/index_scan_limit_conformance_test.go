//go:build bazelrunfiles

package conformance_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"time"

	"fdb.dev/gen/demopb"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/recordlayer"
	"github.com/google/uuid"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"
)

type scanLimitRequest struct {
	IndexName       string `json:"indexName"`
	Streaming       string `json:"streaming"`
	Records         bool   `json:"records"`
	Reverse         bool   `json:"reverse"`
	RowLimit        int    `json:"rowLimit"`
	ScannedLimit    int    `json:"scannedLimit"`
	ByteLimit       int64  `json:"byteLimit"`
	TimeLimit       int64  `json:"timeLimit"`
	FailOnScanLimit bool   `json:"failOnScanLimit"`
	Continuation    string `json:"continuation"`
}

type scanLimitRangeOptions struct {
	Mode    int  `json:"mode"`
	Limit   int  `json:"limit"`
	Reverse bool `json:"reverse"`
}

type scanLimitRow struct {
	IndexKey     string `json:"indexKey"`
	IndexValue   string `json:"indexValue"`
	PrimaryKey   string `json:"primaryKey"`
	Data         string `json:"data"`
	Version      string `json:"version"`
	Continuation string `json:"continuation"`
}

type scanLimitTerminal struct {
	Reason         string `json:"reason"`
	Continuation   string `json:"continuation"`
	End            bool   `json:"end"`
	ScannedRecords int    `json:"scannedRecords"`
	ScannedBytes   int64  `json:"scannedBytes"`
}

type scanLimitPage struct {
	scanLimitTerminal
	Rows           []scanLimitRow          `json:"rows"`
	Repeats        []scanLimitTerminal     `json:"repeats"`
	RangeOptions   []scanLimitRangeOptions `json:"rangeOptions"`
	ExceptionClass string                  `json:"exceptionClass"`
}

// Observe the real GetRange options without substituting its result or iterator.
// The filter excludes metadata/state reads and record loads from SCAN_AND_FETCH.
type scanLimitTransaction struct {
	fdb.WritableTransaction
	indexPrefix []byte
	calls       []scanLimitRangeOptions
}

func (t *scanLimitTransaction) observe(r fdb.Range, options fdb.RangeOptions) {
	begin, _ := r.FDBRangeKeySelectors()
	if len(t.indexPrefix) > 0 && bytes.HasPrefix(begin.FDBKeySelector().Key.FDBKey(), t.indexPrefix) {
		t.calls = append(t.calls, scanLimitRangeOptions{int(options.Mode), options.Limit, options.Reverse})
	}
}

func (t *scanLimitTransaction) GetRange(r fdb.Range, options fdb.RangeOptions) fdb.RangeResult {
	t.observe(r, options)
	return t.WritableTransaction.GetRange(r, options)
}

func (t *scanLimitTransaction) Snapshot() fdb.ReadTransaction {
	return &scanLimitSnapshot{ReadTransaction: t.WritableTransaction.Snapshot(), owner: t}
}

type scanLimitSnapshot struct {
	fdb.ReadTransaction
	owner *scanLimitTransaction
}

func (s *scanLimitSnapshot) GetRange(r fdb.Range, options fdb.RangeOptions) fdb.RangeResult {
	s.owner.observe(r, options)
	return s.ReadTransaction.GetRange(r, options)
}

func scanLimitBytes(value []byte) string { return base64.StdEncoding.EncodeToString(value) }

func scanLimitMetadata() *recordlayer.RecordMetaData {
	builder := recordlayer.NewRecordMetaDataBuilder().SetRecords(demopb.File_record_layer_demo_proto)
	builder.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
	builder.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
	builder.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
	builder.SetStoreRecordVersions(true)
	builder.AddIndex("Order", recordlayer.NewIndex("price", recordlayer.Field("price")).SetSubspaceKey("price"))
	builder.AddIndex("Order", recordlayer.NewVersionIndex("version", recordlayer.VersionKey()).SetSubspaceKey("version"))
	metadata, err := builder.Build()
	Expect(err).NotTo(HaveOccurred())
	return metadata
}

func scanLimitReason(reason recordlayer.NoNextReason) string {
	return map[recordlayer.NoNextReason]string{
		recordlayer.SourceExhausted: "SOURCE_EXHAUSTED", recordlayer.ReturnLimitReached: "RETURN_LIMIT_REACHED",
		recordlayer.ScanLimitReached: "SCAN_LIMIT_REACHED", recordlayer.ByteLimitReached: "BYTE_LIMIT_REACHED",
		recordlayer.TimeLimitReached: "TIME_LIMIT_REACHED",
	}[reason]
}

func scanLimitDrain[T any](ctx context.Context, cursor recordlayer.RecordCursor[T], state *recordlayer.ScanLimiterState,
	convert func(T) scanLimitRow,
) scanLimitPage {
	defer func() { Expect(cursor.Close()).To(Succeed()) }()
	page := scanLimitPage{Rows: make([]scanLimitRow, 0), Repeats: make([]scanLimitTerminal, 0)}
	terminal := func(next recordlayer.RecordCursorResult[T]) scanLimitTerminal {
		state.WaitForIssuedRecordLoadCharges()
		continuation, err := next.GetContinuation().ToBytes()
		Expect(err).NotTo(HaveOccurred())
		return scanLimitTerminal{
			Reason: scanLimitReason(next.GetNoNextReason()), Continuation: scanLimitBytes(continuation),
			End: next.GetContinuation().IsEnd(), ScannedRecords: state.RecordsScanned(), ScannedBytes: state.BytesScanned(),
		}
	}
	for {
		next, err := cursor.OnNext(ctx)
		if err != nil {
			var limit *recordlayer.ScanLimitReachedError
			if errors.As(err, &limit) {
				page.ExceptionClass = "ScanLimitReachedException"
			} else {
				page.ExceptionClass = fmt.Sprintf("%T", err)
			}
			break
		}
		if !next.HasNext() {
			page.scanLimitTerminal = terminal(next)
			for repeat := 0; repeat < 2; repeat++ {
				repeated, err := cursor.OnNext(ctx)
				Expect(err).NotTo(HaveOccurred())
				Expect(repeated.HasNext()).To(BeFalse())
				page.Repeats = append(page.Repeats, terminal(repeated))
			}
			break
		}
		row := convert(next.GetValue())
		continuation, err := next.GetContinuation().ToBytes()
		Expect(err).NotTo(HaveOccurred())
		row.Continuation = scanLimitBytes(continuation)
		page.Rows = append(page.Rows, row)
	}
	state.WaitForIssuedRecordLoadCharges()
	page.ScannedRecords, page.ScannedBytes = state.RecordsScanned(), state.BytesScanned()
	return page
}

// Java KeyValueCursorBase.onNext charges tryRecordScan before iterator exhaustion
// or its finite returned-row boundary, and caches the resulting terminal value.
var _ = Describe("Ordinary index cursor Java limit conformance", func() {
	const count = 6
	var (
		ctx  context.Context
		env  *TenantEnvironment
		java *JavaInvoker
		ks   subspace.Subspace
	)
	BeforeEach(func() {
		ctx = context.Background()
		var err error
		env, err = SetupTenantEnvironment(ctx, sharedContainer, "index_limits_"+uuid.NewString())
		Expect(err).NotTo(HaveOccurred())
		java = NewJavaInvoker()
		ks = env.Keyspace.Sub("limits")
		var written int
		Expect(java.InvokeAs(ctx, "seedIndexScanLimits", map[string]any{
			"clusterFile": env.ClusterFile, "tenantName": env.TenantName,
			"subspace": BytesToIntArray(ks.Bytes()), "count": count,
		}, &written)).To(Succeed())
		Expect(written).To(Equal(count))
	})
	AfterEach(func() {
		if env != nil {
			Expect(env.Cleanup(ctx)).To(Succeed())
		}
	})
	scanGo := func(req scanLimitRequest) scanLimitPage {
		transaction, err := env.RecordDB.CreateTransaction()
		Expect(err).NotTo(HaveOccurred())
		defer transaction.Cancel()
		logged := &scanLimitTransaction{WritableTransaction: transaction, calls: make([]scanLimitRangeOptions, 0)}
		rtx := env.RecordDB.NewRecordContext(logged)
		store, err := recordlayer.NewStoreBuilder().SetContext(rtx).SetSubspace(ks).
			SetMetaDataProvider(scanLimitMetadata()).SetFormatVersion(14).CreateOrOpen()
		Expect(err).NotTo(HaveOccurred())
		index := store.GetRecordMetaData().GetIndex(req.IndexName)
		logged.indexPrefix = store.IndexSubspace(index).Bytes()
		// Constructing execution state AFTER this delay must not reset the clock.
		if req.TimeLimit > 0 {
			time.Sleep(time.Duration(req.TimeLimit+25) * time.Millisecond)
		}
		execution := recordlayer.DefaultExecuteProperties()
		execution.ReturnedRowLimit, execution.ScannedRecordsLimit = req.RowLimit, req.ScannedLimit
		execution.ScannedBytesLimit = req.ByteLimit
		execution.TimeLimit = time.Duration(req.TimeLimit) * time.Millisecond
		execution.FailOnScanLimitReached = req.FailOnScanLimit
		streaming := recordlayer.StreamingModeIterator
		if req.Streaming == "WANT_ALL" {
			streaming = recordlayer.StreamingModeWantAll
		}
		properties := recordlayer.NewScanProperties(execution).WithStreamingMode(streaming).WithReverse(req.Reverse)
		continuation, err := base64.StdEncoding.DecodeString(req.Continuation)
		Expect(err).NotTo(HaveOccurred())
		if req.Continuation == "" {
			continuation = nil
		}
		entryRow := func(entry *recordlayer.IndexEntry) scanLimitRow {
			return scanLimitRow{
				IndexKey: scanLimitBytes(entry.Key.Pack()), IndexValue: scanLimitBytes(entry.Value.Pack()),
				PrimaryKey: scanLimitBytes(entry.PrimaryKey().Pack()),
			}
		}
		var page scanLimitPage
		if req.Records {
			cursor := store.ScanIndexRecords(req.IndexName, recordlayer.TupleRangeAll, continuation, properties)
			page = scanLimitDrain(ctx, cursor, execution.ScanState, func(indexed *recordlayer.FDBIndexedRecord) scanLimitRow {
				row := entryRow(indexed.IndexEntry)
				data, err := proto.MarshalOptions{Deterministic: true}.Marshal(indexed.Record.Record)
				Expect(err).NotTo(HaveOccurred())
				row.Data = scanLimitBytes(data)
				if indexed.Record.Version != nil {
					row.Version = scanLimitBytes(indexed.Record.Version.ToBytes())
				}
				return row
			})
		} else {
			page = scanLimitDrain(ctx, store.ScanIndex(index, recordlayer.TupleRangeAll, continuation, properties), execution.ScanState, entryRow)
		}
		page.RangeOptions = logged.calls
		return page
	}
	compare := func(req scanLimitRequest) scanLimitPage {
		var oracle scanLimitPage
		Expect(java.InvokeAs(ctx, "scanIndexScanLimits", map[string]any{
			"clusterFile": env.ClusterFile, "tenantName": env.TenantName, "subspace": BytesToIntArray(ks.Bytes()), "request": req,
		}, &oracle)).To(Succeed())
		Expect(scanGo(req)).To(Equal(oracle), "index=%s records=%v reverse=%v row=%d scan=%d byte=%d time=%d fail=%v",
			req.IndexName, req.Records, req.Reverse, req.RowLimit, req.ScannedLimit, req.ByteLimit, req.TimeLimit, req.FailOnScanLimit)
		for _, repeat := range oracle.Repeats {
			Expect(repeat).To(Equal(oracle.scanLimitTerminal), "terminal reads must not charge more attempts/bytes or change the continuation")
		}
		return oracle
	}
	forEachScan := func(check func(scanLimitRequest)) {
		for _, index := range []string{"price", "version"} {
			for _, records := range []bool{false, true} {
				for _, reverse := range []bool{false, true} {
					check(scanLimitRequest{IndexName: index, Records: records, Reverse: reverse, Streaming: "ITERATOR"})
				}
			}
		}
	}

	It("charges the exhausted scan attempt and repeats the same terminal result", func() {
		forEachScan(func(req scanLimitRequest) {
			page := compare(req)
			Expect(page.Rows).To(HaveLen(count))
			Expect(page.Reason).To(Equal("SOURCE_EXHAUSTED"))
			Expect(page.End).To(BeTrue())
			Expect(page.ScannedRecords).To(Equal(count + 1))
			Expect(page.Repeats).To(HaveLen(2))
		})
	})
	It("returns a resumable row-limit boundary at exactly n, but exhaustion above n", func() {
		forEachScan(func(req scanLimitRequest) {
			for _, limit := range []int{1, count, count + 1} {
				req.RowLimit = limit
				page := compare(req)
				Expect(page.Rows).To(HaveLen(min(limit, count)))
				Expect(page.ScannedRecords).To(Equal(min(limit, count) + 1))
				if limit <= count {
					Expect(page.Reason).To(Equal("RETURN_LIMIT_REACHED"))
					Expect(page.End).To(BeFalse())
					Expect(page.Continuation).To(Equal(page.Rows[len(page.Rows)-1].Continuation))
				} else {
					Expect(page.Reason).To(Equal("SOURCE_EXHAUSTED"))
					Expect(page.End).To(BeTrue())
				}
			}
		})
	})
	It("uses native EXACT and the original finite budget for WANT_ALL", func() {
		forEachScan(func(req scanLimitRequest) {
			req.Streaming = "WANT_ALL"
			for _, limit := range []int{1, count, count + 1} {
				req.RowLimit = limit
				page := compare(req)
				Expect(page.RangeOptions).To(Equal([]scanLimitRangeOptions{{int(fdb.StreamingModeExact), limit, req.Reverse}}))
			}
		})
	})
	It("resumes every VALUE and VERSION token including the final row token", func() {
		forEachScan(func(req scanLimitRequest) {
			baseline := compare(req)
			Expect(baseline.Rows).To(HaveLen(count))
			for position, row := range baseline.Rows {
				req.Continuation = row.Continuation
				page := compare(req)
				Expect(page.Rows).To(Equal(baseline.Rows[position+1:]))
				Expect(page.Reason).To(Equal("SOURCE_EXHAUSTED"))
				Expect(page.ScannedRecords).To(Equal(count - position))
			}
		})
	})
	It("charges the byte-limited attempt after its unconditional initial pass", func() {
		forEachScan(func(req scanLimitRequest) {
			req.ByteLimit = 1
			page := compare(req)
			Expect(page.Rows).To(HaveLen(1))
			Expect(page.Reason).To(Equal("BYTE_LIMIT_REACHED"))
			Expect(page.ScannedRecords).To(Equal(2))
		})
	})
	It("counts failed scanned-record attempts and gives scanned limits priority over bytes", func() {
		forEachScan(func(req scanLimitRequest) {
			for _, limit := range []int{1, 3} {
				req.ScannedLimit = limit
				page := compare(req)
				Expect(page.Rows).To(HaveLen(limit))
				Expect(page.Reason).To(Equal("SCAN_LIMIT_REACHED"))
				Expect(page.ScannedRecords).To(Equal(limit + 1))
			}
			req.ScannedLimit, req.ByteLimit = 1, 1
			page := compare(req)
			Expect(page.Reason).To(Equal("SCAN_LIMIT_REACHED"))
		})
	})
	It("uses the transaction creation clock and prioritizes bytes before time", func() {
		forEachScan(func(req scanLimitRequest) {
			req.TimeLimit = 200
			page := compare(req)
			Expect(page.Rows).To(HaveLen(1))
			Expect(page.Reason).To(Equal("TIME_LIMIT_REACHED"))
			req.ByteLimit = 1
			page = compare(req)
			Expect(page.Reason).To(Equal("BYTE_LIMIT_REACHED"))
			req.ScannedLimit = 1
			page = compare(req)
			Expect(page.Reason).To(Equal("SCAN_LIMIT_REACHED"))
		})
	})
	It("raises Java ScanLimitReachedException for record, byte and time limits", func() {
		forEachScan(func(req scanLimitRequest) {
			req.FailOnScanLimit = true
			req.ScannedLimit = 1
			page := compare(req)
			Expect(page.ExceptionClass).To(Equal("ScanLimitReachedException"))
			req.ScannedLimit, req.ByteLimit = 0, 1
			page = compare(req)
			Expect(page.ExceptionClass).To(Equal("ScanLimitReachedException"))
			req.ByteLimit, req.TimeLimit = 0, 200
			page = compare(req)
			Expect(page.ExceptionClass).To(Equal("ScanLimitReachedException"))
		})
	})
})
