//go:build bazelrunfiles

package conformance_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"math"
	"sync"
	"sync/atomic"
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
	IndexName                       string `json:"indexName"`
	Streaming                       string `json:"streaming"`
	Records                         bool   `json:"records"`
	Reverse                         bool   `json:"reverse"`
	RowLimit                        int    `json:"rowLimit"`
	Skip                            int    `json:"skip"`
	ScannedLimit                    int    `json:"scannedLimit"`
	ByteLimit                       int64  `json:"byteLimit"`
	TimeLimit                       int64  `json:"timeLimit"`
	FailOnScanLimit                 bool   `json:"failOnScanLimit"`
	Continuation                    string `json:"continuation"`
	HoldRecordLoadsUntilSourceError bool   `json:"holdRecordLoadsUntilSourceError"`
	SelectorsOnly                   bool   `json:"selectorsOnly"`
}

type scanLimitRangeOptions struct {
	Mode    int  `json:"mode"`
	Limit   int  `json:"limit"`
	Reverse bool `json:"reverse"`
}

// scanLimitSelectors is the getRange selector pair after continuation and skip.
type scanLimitSelectors struct {
	BeginKey     string `json:"beginKey"`
	BeginOrEqual bool   `json:"beginOrEqual"`
	BeginOffset  int    `json:"beginOffset"`
	EndKey       string `json:"endKey"`
	EndOrEqual   bool   `json:"endOrEqual"`
	EndOffset    int    `json:"endOffset"`
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
	Selectors      []scanLimitSelectors    `json:"selectors"`
	ExceptionClass string                  `json:"exceptionClass"`
}

// Observe the real GetRange options without substituting its result or iterator.
// The filter excludes metadata/state reads and record loads from SCAN_AND_FETCH.
type scanLimitTransaction struct {
	fdb.WritableTransaction
	indexPrefix []byte
	calls       []scanLimitRangeOptions
	selectors   []scanLimitSelectors
	recordGate  *scanLimitRecordGate
}

func (t *scanLimitTransaction) observe(r fdb.Range, options fdb.RangeOptions) {
	b, e := r.FDBRangeKeySelectors()
	begin, end := b.FDBKeySelector(), e.FDBKeySelector()
	if len(t.indexPrefix) > 0 && bytes.HasPrefix(begin.Key.FDBKey(), t.indexPrefix) {
		t.calls = append(t.calls, scanLimitRangeOptions{int(options.Mode), options.Limit, options.Reverse})
		t.selectors = append(t.selectors, scanLimitSelectors{
			BeginKey: scanLimitBytes(begin.Key.FDBKey()), BeginOrEqual: begin.OrEqual, BeginOffset: begin.Offset,
			EndKey: scanLimitBytes(end.Key.FDBKey()), EndOrEqual: end.OrEqual, EndOffset: end.Offset,
		})
	}
}

func (t *scanLimitTransaction) GetRange(r fdb.Range, options fdb.RangeOptions) fdb.RangeResult {
	t.observe(r, options)
	return t.gateRecordRange(r, t.WritableTransaction.GetRange(r, options))
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
	return s.owner.gateRecordRange(r, s.ReadTransaction.GetRange(r, options))
}

func (t *scanLimitTransaction) gateRecordRange(r fdb.Range, result fdb.RangeResult) fdb.RangeResult {
	gate := t.recordGate
	if gate == nil {
		return result
	}
	begin, end := r.FDBRangeKeySelectors()
	if !bytes.HasPrefix(begin.FDBKeySelector().Key.FDBKey(), gate.prefix) || !bytes.HasPrefix(end.FDBKeySelector().Key.FDBKey(), gate.prefix) {
		return result
	}
	gate.issued.Add(1)
	return scanLimitHeldRange{RangeResult: result, gate: gate}
}

type scanLimitRecordGate struct {
	ctx           context.Context
	prefix        []byte
	release       chan struct{}
	once          sync.Once
	issued        atomic.Int32
	nativeRows    atomic.Int32
	sourceError   error
	issuedAtError int32
}

func (g *scanLimitRecordGate) open() { g.once.Do(func() { close(g.release) }) }

type scanLimitHeldRange struct {
	fdb.RangeResult
	gate *scanLimitRecordGate
}

func (r scanLimitHeldRange) Iterator() fdb.RangeIterator {
	return &scanLimitHeldIterator{RangeIterator: r.RangeResult.Iterator(), gate: r.gate}
}

type scanLimitHeldIterator struct {
	fdb.RangeIterator
	gate    *scanLimitRecordGate
	started bool
	err     error
}

func (i *scanLimitHeldIterator) Advance() bool {
	if i.started {
		return i.err == nil && i.RangeIterator.Advance()
	}
	i.started = true
	if !i.RangeIterator.Advance() {
		_, i.err = i.RangeIterator.Get()
		if i.err == nil {
			i.err = fmt.Errorf("held record range positive control was empty")
		}
		return false
	}
	// Get is a reentrant peek at the actual native iterator's current row.
	// Do not advance again, buffer replacement rows, or inject record data.
	row, err := i.RangeIterator.Get()
	if err != nil {
		i.err = err
		return false
	}
	if !bytes.HasPrefix(row.Key, i.gate.prefix) {
		i.err = fmt.Errorf("held record range returned key outside record prefix")
		return false
	}
	i.gate.nativeRows.Add(1)
	select {
	case <-i.gate.release:
		return true
	case <-i.gate.ctx.Done():
		i.err = i.gate.ctx.Err()
		return false
	}
}

func (i *scanLimitHeldIterator) Get() (fdb.KeyValue, error) {
	if i.err != nil {
		return fdb.KeyValue{}, i.err
	}
	return i.RangeIterator.Get()
}

type scanLimitHeldCursor struct {
	recordlayer.RecordCursor[*recordlayer.FDBIndexedRecord]
	gate *scanLimitRecordGate
}

func (c *scanLimitHeldCursor) OnNext(ctx context.Context) (recordlayer.RecordCursorResult[*recordlayer.FDBIndexedRecord], error) {
	next, err := c.RecordCursor.OnNext(ctx)
	if err != nil {
		select {
		case <-c.gate.release:
			Fail("held record gate opened before the actual source error")
		default:
		}
		c.gate.sourceError = err
		c.gate.issuedAtError = c.gate.issued.Load()
		c.gate.open()
	}
	return next, err
}

func (c *scanLimitHeldCursor) Close() error {
	c.gate.open()
	return c.RecordCursor.Close()
}

func scanLimitAssertHeldRequest(req scanLimitRequest) {
	GinkgoHelper()
	Expect(req.Records && req.FailOnScanLimit && req.RowLimit == 0 && req.Continuation == "").To(BeTrue(), "held scheduling requires the current record-bearing exception cases without row limit or continuation")
	valid := (req.ScannedLimit == 1 && req.ByteLimit == 0 && req.TimeLimit == 0) ||
		(req.ScannedLimit == 0 && req.ByteLimit == 1 && req.TimeLimit == 0) ||
		(req.ScannedLimit == 0 && req.ByteLimit == 0 && req.TimeLimit == 200)
	Expect(valid).To(BeTrue(), "holding until error is unsafe for arbitrary limits that could fill the mapping pipeline")
}

func scanLimitBytes(value []byte) string { return base64.StdEncoding.EncodeToString(value) }

func scanLimitMetadata() *recordlayer.RecordMetaData {
	builder := recordlayer.NewRecordMetaDataBuilder().SetRecords(demopb.File_record_layer_demo_proto)
	builder.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
	builder.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
	builder.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
	builder.SetStoreRecordVersions(true)
	bitmap := recordlayer.NewBitmapValueIndex("bitmap",
		recordlayer.GroupBy(recordlayer.Field("order_id"), recordlayer.Field("price"))).SetSubspaceKey("bitmap")
	bitmap.Options[recordlayer.IndexOptionBitmapValueEntrySize] = "4"
	builder.AddIndex("Order", recordlayer.NewIndex("price", recordlayer.Field("price")).SetSubspaceKey("price"))
	builder.AddIndex("Order", recordlayer.NewVersionIndex("version", recordlayer.VersionKey()).SetSubspaceKey("version"))
	builder.AddIndex("Order", recordlayer.NewCountIndex("count", recordlayer.GroupAll(recordlayer.Field("price"))).SetSubspaceKey("count"))
	builder.AddIndex("Order", recordlayer.NewMinEverTupleIndex("min_ever_tuple",
		recordlayer.GroupBy(recordlayer.Field("order_id"), recordlayer.Field("price"))).SetSubspaceKey("min_ever_tuple"))
	builder.AddIndex("Order", recordlayer.NewMaxEverTupleIndex("max_ever_tuple",
		recordlayer.GroupBy(recordlayer.Field("order_id"), recordlayer.Field("price"))).SetSubspaceKey("max_ever_tuple"))
	builder.AddIndex("Order", recordlayer.NewMaxEverVersionIndex("max_ever_version",
		recordlayer.GroupBy(recordlayer.VersionKey(), recordlayer.Field("price"))).SetSubspaceKey("max_ever_version"))
	builder.AddIndex("Order", bitmap)
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
		logged := &scanLimitTransaction{
			WritableTransaction: transaction, calls: make([]scanLimitRangeOptions, 0),
			selectors: make([]scanLimitSelectors, 0),
		}
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
		execution.Skip = req.Skip
		execution.ScannedBytesLimit = req.ByteLimit
		execution.TimeLimit = time.Duration(req.TimeLimit) * time.Millisecond
		execution.FailOnScanLimitReached = req.FailOnScanLimit
		streaming, known := map[string]recordlayer.CursorStreamingMode{
			"ITERATOR": recordlayer.StreamingModeIterator, "WANT_ALL": recordlayer.StreamingModeWantAll,
			"SMALL": recordlayer.StreamingModeSmall, "MEDIUM": recordlayer.StreamingModeMedium, "LARGE": recordlayer.StreamingModeLarge,
		}[req.Streaming]
		Expect(known).To(BeTrue(), "streaming mode %q", req.Streaming)
		properties := recordlayer.NewScanProperties(execution).WithStreamingMode(streaming).WithReverse(req.Reverse)
		continuation, err := base64.StdEncoding.DecodeString(req.Continuation)
		Expect(err).NotTo(HaveOccurred())
		if req.Continuation == "" {
			continuation = nil
		}
		byGroup := index.Type != recordlayer.IndexTypeValue && index.Type != recordlayer.IndexTypeVersion
		entryRow := func(entry *recordlayer.IndexEntry) scanLimitRow {
			primaryKey := ""
			if !byGroup {
				primaryKey = scanLimitBytes(entry.PrimaryKey().Pack())
			}
			return scanLimitRow{
				IndexKey: scanLimitBytes(entry.Key.Pack()), IndexValue: scanLimitBytes(entry.Value.Pack()),
				PrimaryKey: primaryKey,
			}
		}
		var page scanLimitPage
		var gate *scanLimitRecordGate
		if req.HoldRecordLoadsUntilSourceError {
			scanLimitAssertHeldRequest(req)
			gateCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
			defer cancel()
			Expect(transaction.Options().SetTimeout((15 * time.Second).Milliseconds())).To(Succeed())
			gate = &scanLimitRecordGate{ctx: gateCtx, prefix: ks.Sub(recordlayer.RecordKey).Bytes(), release: make(chan struct{})}
			logged.recordGate = gate // Activate only after metadata/store opening.
			defer gate.open()
		}
		if req.Records {
			cursor := store.ScanIndexRecords(req.IndexName, recordlayer.TupleRangeAll, continuation, properties)
			if gate != nil {
				cursor = &scanLimitHeldCursor{RecordCursor: cursor, gate: gate}
			}
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
		} else if byGroup {
			page = scanLimitDrain(ctx, store.ScanIndexByType(index, recordlayer.IndexScanByGroup, recordlayer.TupleRangeAll, continuation, properties), execution.ScanState, entryRow)
		} else {
			page = scanLimitDrain(ctx, store.ScanIndex(index, recordlayer.TupleRangeAll, continuation, properties), execution.ScanState, entryRow)
		}
		if gate != nil {
			var sourceLimit *recordlayer.ScanLimitReachedError
			Expect(errors.As(gate.sourceError, &sourceLimit)).To(BeTrue(), "the actual source scan error must release held native record reads")
			Expect(gate.issuedAtError).To(Equal(int32(1)), "current held exception schedules issue exactly one record load")
			Expect(gate.issued.Load()).To(Equal(gate.issuedAtError), "quiescence must not issue another record range")
			Expect(gate.nativeRows.Load()).To(Equal(gate.issued.Load()), "each held read must consume a positive real native row after release")
		}
		page.RangeOptions, page.Selectors = logged.calls, logged.selectors
		return page
	}
	compare := func(req scanLimitRequest) scanLimitPage {
		var oracle scanLimitPage
		Expect(java.InvokeAs(ctx, "scanIndexScanLimits", map[string]any{
			"clusterFile": env.ClusterFile, "tenantName": env.TenantName, "subspace": BytesToIntArray(ks.Bytes()), "request": req,
		}, &oracle)).To(Succeed())
		Expect(scanGo(req)).To(Equal(oracle), "index=%s records=%v reverse=%v row=%d skip=%d scan=%d byte=%d time=%d fail=%v mode=%s",
			req.IndexName, req.Records, req.Reverse, req.RowLimit, req.Skip, req.ScannedLimit, req.ByteLimit, req.TimeLimit,
			req.FailOnScanLimit, req.Streaming)
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
	forEachGroupScan := func(check func(scanLimitRequest)) {
		for _, index := range []string{"count", "min_ever_tuple", "max_ever_tuple", "max_ever_version", "bitmap"} {
			for _, reverse := range []bool{false, true} {
				check(scanLimitRequest{IndexName: index, Reverse: reverse, Streaming: "ITERATOR"})
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
	// KeyValueCursorBase.Builder.prepare (KeyValueCursorBase.java:327-356) applies
	// the continuation first, then adds Skip to the begin (forward) or end
	// (reverse) selector. The native row limit is not widened by the skip.
	It("applies Skip as selector offsets to exhausted, row-limited and resumed pages", func() {
		forEachScan(func(req scanLimitRequest) {
			baseline := compare(req)
			Expect(baseline.Rows).To(HaveLen(count))
			for _, skip := range []int{1, 3, count, count + 1, math.MaxInt32 - 1} {
				req.Skip = skip
				page := compare(req)
				Expect(page.Rows).To(Equal(baseline.Rows[min(skip, count):]))
				Expect(page.Reason).To(Equal("SOURCE_EXHAUSTED"))
				Expect(page.ScannedRecords).To(Equal(len(page.Rows) + 1))
				Expect(page.Selectors).To(HaveLen(1))
				if req.Reverse {
					Expect(page.Selectors[0].EndOffset).To(Equal(1 - skip))
				} else {
					Expect(page.Selectors[0].BeginOffset).To(Equal(1 + skip))
				}
			}
			for _, skip := range []int{1, 3} {
				req.Skip = skip
				for _, limit := range []int{1, count - skip, count - skip + 1} {
					req.RowLimit = limit
					page := compare(req)
					Expect(page.Rows).To(Equal(baseline.Rows[skip:min(skip+limit, count)]))
					Expect(page.RangeOptions[0].Limit).To(Equal(limit))
					if skip+limit <= count {
						Expect(page.Reason).To(Equal("RETURN_LIMIT_REACHED"))
					} else {
						Expect(page.Reason).To(Equal("SOURCE_EXHAUSTED"))
					}
				}
			}
			// A resumed page re-applies the caller's Skip after its continuation.
			req.Skip, req.RowLimit = 2, 0
			for position, row := range baseline.Rows {
				req.Continuation = row.Continuation
				Expect(compare(req).Rows).To(Equal(baseline.Rows[min(position+3, count):]))
			}
			req.Skip, req.RowLimit, req.Continuation = 1, 2, ""
			first := compare(req)
			Expect(first.Rows).To(Equal(baseline.Rows[1:3]))
			req.Continuation = first.Continuation
			second := compare(req)
			Expect(second.Rows).To(Equal(baseline.Rows[4:6]))
			req.Continuation = second.Continuation
			Expect(compare(req).Rows).To(BeEmpty())
		})
	})
	It("does not charge skipped rows to scanned-record or byte limits", func() {
		forEachScan(func(req scanLimitRequest) {
			baseline := compare(req)
			req.Skip = 2
			for _, limit := range []int{1, 3} {
				req.ScannedLimit = limit
				page := compare(req)
				Expect(page.Rows).To(Equal(baseline.Rows[2 : 2+limit]))
				Expect(page.Reason).To(Equal("SCAN_LIMIT_REACHED"))
				Expect(page.ScannedRecords).To(Equal(limit + 1))
			}
			req.ScannedLimit, req.ByteLimit = 0, 1
			page := compare(req)
			Expect(page.Rows).To(Equal(baseline.Rows[2:3]))
			Expect(page.Reason).To(Equal("BYTE_LIMIT_REACHED"))
			Expect(page.ScannedRecords).To(Equal(2))
		})
	})
	It("keeps Java calcStreamingMode and the unadjusted native limit under Skip", func() {
		forEachScan(func(req scanLimitRequest) {
			req.Skip = 2
			for _, mode := range []string{"ITERATOR", "WANT_ALL", "SMALL", "MEDIUM", "LARGE"} {
				req.Streaming = mode
				for _, limit := range []int{0, 2} {
					req.RowLimit = limit
					page := compare(req)
					Expect(page.RangeOptions).To(HaveLen(1))
					Expect(page.RangeOptions[0].Limit).To(Equal(limit))
					if mode == "WANT_ALL" && limit > 0 {
						Expect(page.RangeOptions[0].Mode).To(Equal(int(fdb.StreamingModeExact)))
					}
				}
			}
		})
	})
	// fdb-java KeySelector.add is int arithmetic (KeySelector.java:144-146): for
	// Skip=MaxInt32 Java passes a forward begin offset of MinInt32 and a reverse end
	// offset of 1-MaxInt32 to getRange. Go saturates the skip at MaxInt32-1
	// (docs/compatibility.md). Java's selectors are built without the read, so
	// neither offset is sent to the shared cluster.
	It("saturates Skip=MaxInt32 selector offsets where Java's differ", func() {
		forEachScan(func(req scanLimitRequest) {
			req.Skip = math.MaxInt32
			probe := req
			probe.SelectorsOnly = true
			var oracle scanLimitPage
			Expect(java.InvokeAs(ctx, "scanIndexScanLimits", map[string]any{
				"clusterFile": env.ClusterFile, "tenantName": env.TenantName, "subspace": BytesToIntArray(ks.Bytes()), "request": probe,
			}, &oracle)).To(Succeed())
			Expect(oracle.Rows).To(BeEmpty())
			Expect(oracle.Selectors).To(HaveLen(1))
			page := scanGo(req)
			Expect(page.ExceptionClass).To(BeEmpty())
			Expect(page.Rows).To(BeEmpty())
			Expect(page.Reason).To(Equal("SOURCE_EXHAUSTED"))
			Expect(page.ScannedRecords).To(Equal(1))
			Expect(page.RangeOptions).To(Equal(oracle.RangeOptions))
			want := oracle.Selectors[0]
			if req.Reverse {
				Expect(want.EndOffset).To(Equal(1 - math.MaxInt32))
				want.EndOffset = 2 - math.MaxInt32
			} else {
				Expect(want.BeginOffset).To(Equal(math.MinInt32))
				want.BeginOffset = math.MaxInt32
			}
			Expect(page.Selectors).To(Equal([]scanLimitSelectors{want}))
		})
	})
	It("applies Skip as selector offsets on COUNT, MIN/MAX_EVER_TUPLE, MAX_EVER_VERSION and BITMAP_VALUE cursors", func() {
		compareGroup := func(req scanLimitRequest) scanLimitPage {
			var oracle scanLimitPage
			Expect(java.InvokeAs(ctx, "scanIndexScanLimits", map[string]any{
				"clusterFile": env.ClusterFile, "tenantName": env.TenantName, "subspace": BytesToIntArray(ks.Bytes()), "request": req,
			}, &oracle)).To(Succeed())
			got := scanGo(req)
			Expect(got.ExceptionClass).To(Equal(oracle.ExceptionClass))
			Expect(got.Rows).To(Equal(oracle.Rows), "index=%s reverse=%v row=%d skip=%d cont=%s",
				req.IndexName, req.Reverse, req.RowLimit, req.Skip, req.Continuation)
			Expect(got.Reason).To(Equal(oracle.Reason))
			Expect(got.Continuation).To(Equal(oracle.Continuation))
			Expect(got.End).To(Equal(oracle.End))
			Expect(got.ScannedBytes).To(Equal(oracle.ScannedBytes))
			Expect(got.Selectors).To(Equal(oracle.Selectors))
			return oracle
		}
		forEachGroupScan(func(req scanLimitRequest) {
			baseline := compareGroup(req)
			Expect(baseline.Rows).To(HaveLen(count))
			for _, skip := range []int{1, 3, count, count + 1, math.MaxInt32 - 1} {
				req.Skip = skip
				page := compareGroup(req)
				Expect(page.Rows).To(Equal(baseline.Rows[min(skip, count):]))
				Expect(page.Reason).To(Equal("SOURCE_EXHAUSTED"))
				Expect(page.Selectors).To(HaveLen(1))
				if req.Reverse {
					Expect(page.Selectors[0].EndOffset).To(Equal(1 - skip))
				} else {
					Expect(page.Selectors[0].BeginOffset).To(Equal(1 + skip))
				}
			}
			// A resumed page re-applies the caller's Skip after its continuation.
			req.Skip, req.RowLimit = 2, 0
			for position, row := range baseline.Rows {
				req.Continuation = row.Continuation
				Expect(compareGroup(req).Rows).To(Equal(baseline.Rows[min(position+3, count):]))
			}
			req.Skip, req.RowLimit, req.Continuation = 1, 2, ""
			first := compareGroup(req)
			Expect(first.Rows).To(Equal(baseline.Rows[1:3]))
			req.Continuation = first.Continuation
			second := scanGo(req)
			Expect(second.Rows).To(Equal(baseline.Rows[4:6]))
			req.Continuation = second.Rows[len(second.Rows)-1].Continuation
			Expect(compareGroup(req).Rows).To(BeEmpty())

			// Saturates Skip=MaxInt32 and 1<<32 selector offsets where Java's MaxInt32 wraps.
			probe := scanLimitRequest{IndexName: req.IndexName, Reverse: req.Reverse, Streaming: "ITERATOR", Skip: math.MaxInt32, SelectorsOnly: true}
			var oracle scanLimitPage
			Expect(java.InvokeAs(ctx, "scanIndexScanLimits", map[string]any{
				"clusterFile": env.ClusterFile, "tenantName": env.TenantName, "subspace": BytesToIntArray(ks.Bytes()), "request": probe,
			}, &oracle)).To(Succeed())
			Expect(oracle.Selectors).To(HaveLen(1))
			want := oracle.Selectors[0]
			if req.Reverse {
				Expect(want.EndOffset).To(Equal(1 - math.MaxInt32))
				want.EndOffset = 2 - math.MaxInt32
			} else {
				Expect(want.BeginOffset).To(Equal(math.MinInt32))
				want.BeginOffset = math.MaxInt32
			}
			for _, hugeSkip := range []int{math.MaxInt32, 1 << 32} {
				hugeReq := scanLimitRequest{IndexName: req.IndexName, Reverse: req.Reverse, Streaming: "ITERATOR", Skip: hugeSkip}
				page := scanGo(hugeReq)
				Expect(page.ExceptionClass).To(BeEmpty())
				Expect(page.Rows).To(BeEmpty())
				Expect(page.Reason).To(Equal("SOURCE_EXHAUSTED"))
				Expect(page.RangeOptions).To(Equal(oracle.RangeOptions))
				Expect(page.Selectors).To(Equal([]scanLimitSelectors{want}))
			}
		})
	})
	It("accepts only the three held schedules and rejects unsafe requests before opening a Java gate", func() {
		base := scanLimitRequest{
			IndexName: "version", Streaming: "ITERATOR", Records: true, Reverse: true,
			FailOnScanLimit: true, HoldRecordLoadsUntilSourceError: true, ScannedLimit: 1,
		}
		accepted := 0
		for _, profile := range []struct {
			scanned int
			bytes   int64
			time    int64
		}{{1, 0, 0}, {0, 1, 0}, {0, 0, 200}} {
			req := base
			req.ScannedLimit, req.ByteLimit, req.TimeLimit = profile.scanned, profile.bytes, profile.time
			failures := InterceptGomegaFailures(func() { scanLimitAssertHeldRequest(req) })
			Expect(failures).To(BeEmpty(), "approved held budget (%d,%d,%d)", profile.scanned, profile.bytes, profile.time)
			accepted++
		}
		rejected := 0
		for _, tc := range []struct {
			name   string
			mutate func(*scanLimitRequest)
		}{
			{"records_false", func(req *scanLimitRequest) { req.Records = false }},
			{"fail_false", func(req *scanLimitRequest) { req.FailOnScanLimit = false }},
			{"row_limit", func(req *scanLimitRequest) { req.RowLimit = 1 }},
			{"continuation", func(req *scanLimitRequest) { req.Continuation = "FQU=" }},
			{"scanned_two", func(req *scanLimitRequest) { req.ScannedLimit = 2 }},
			{"bytes_two", func(req *scanLimitRequest) { req.ScannedLimit, req.ByteLimit = 0, 2 }},
			{"time_199", func(req *scanLimitRequest) { req.ScannedLimit, req.TimeLimit = 0, 199 }},
			{"no_budget", func(req *scanLimitRequest) { req.ScannedLimit = 0 }},
			{"mixed_budget", func(req *scanLimitRequest) { req.ByteLimit = 1 }},
		} {
			req := base
			tc.mutate(&req)
			failures := InterceptGomegaFailures(func() { scanLimitAssertHeldRequest(req) })
			Expect(failures).NotTo(BeEmpty(), "Go must reject unsafe held schedule %s", tc.name)
			var ignored scanLimitPage
			err := java.InvokeAs(ctx, "scanIndexScanLimits", map[string]any{
				"clusterFile": env.ClusterFile, "tenantName": env.TenantName,
				"subspace": BytesToIntArray(ks.Bytes()), "request": req,
			}, &ignored)
			Expect(err).To(HaveOccurred(), "Java must reject unsafe held schedule %s", tc.name)
			Expect(err.Error()).To(ContainSubstring("held matrix schedule requires records, fail-on-limit"), "Java must reject %s at the early request guard, not a downstream gate", tc.name)
			rejected++
		}
		Expect(accepted).To(Equal(3), "every accepted budget arm must be populated")
		Expect(rejected).To(Equal(9), "every unsafe request guard arm must execute")
		GinkgoWriter.Printf("HELD_SCHEDULE_GUARD_POPULATION accepted=%d rejected=%d\n", accepted, rejected)
	})

	// This matrix explicitly holds real record ranges until the source error.
	// READY is separately pinned by 40 Java observations (20 READY/20 HELD)
	// and eight real-FDB Go regressions (4 READY/4 HELD across indexes/directions).
	It("raises Java ScanLimitReachedException for record, byte and time limits", func() {
		forEachScan(func(req scanLimitRequest) {
			req.FailOnScanLimit = true
			req.HoldRecordLoadsUntilSourceError = req.Records
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
