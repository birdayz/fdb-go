//go:build bazelrunfiles

package conformance_test

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"sync"
	"time"

	"fdb.dev/gen/demopb"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
	"github.com/google/uuid"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"
)

// RFC-258: Record Layer index scans with each IndexFetchMethod against Java on
// one cluster. The case list is Java's RemoteFetch* tests. ExecuteProperties
// skip is not covered here: Go routes a skipped remote-fetch scan to the
// ordinary scan, whose skip handling is covered on its own.

type indexRemoteRequest struct {
	Shape               string `json:"shape"`
	IndexName           string `json:"indexName"`
	Method              string `json:"method"`
	Orphan              string `json:"orphan"`
	Range               string `json:"range"`
	Streaming           string `json:"streaming"`
	ScanType            string `json:"scanType"`
	CommonPKLength      int    `json:"commonPKLength"`
	RowLimit            int    `json:"rowLimit"`
	ScannedLimit        int    `json:"scannedLimit"`
	ByteLimit           int64  `json:"byteLimit"`
	TimeLimit           int64  `json:"timeLimit"`
	Reverse             bool   `json:"reverse"`
	Snapshot            bool   `json:"snapshot"`
	FailOnScanLimit     bool   `json:"failOnScanLimit"`
	Continuation        string `json:"continuation"`
	LocalWriteID        int    `json:"localWriteID"`
	LocalPrimaryWriteID int    `json:"localPrimaryWriteID"`
}

type indexRemoteRow struct {
	IndexKey     string `json:"indexKey"`
	IndexValue   string `json:"indexValue"`
	PrimaryKey   string `json:"primaryKey"`
	Continuation string `json:"continuation"`
	Present      bool   `json:"present"`
	Data         string `json:"data"`
	Version      string `json:"version"`
	KeyCount     int    `json:"keyCount"`
	KeySize      int    `json:"keySize"`
	ValueSize    int    `json:"valueSize"`
	Split        bool   `json:"split"`
}

type indexRemotePage struct {
	Rows           []indexRemoteRow `json:"rows"`
	Continuation   string           `json:"continuation"`
	End            bool             `json:"end"`
	Reason         string           `json:"reason"`
	ExceptionClass string           `json:"exceptionClass"`
	ErrorCode      int              `json:"errorCode"`
	ScannedRecords int              `json:"scannedRecords"`
	ScannedBytes   int64            `json:"scannedBytes"`
	RemoteEntries  int64            `json:"remoteEntries"`
	BadEntries     int64            `json:"badEntries"`
}

func indexRemoteBytes(value []byte) string { return base64.StdEncoding.EncodeToString(value) }

var remoteFetchJava struct {
	once    sync.Once
	invoker *JavaInvoker
	err     error
}

// remoteFetchJavaInvoker is a server whose record layer runs at API version 7.1,
// which Java's remote fetch requires; the shared server keeps Java's 6.3 default.
func remoteFetchJavaInvoker() *JavaInvoker {
	remoteFetchJava.once.Do(func() {
		flags := append(append([]string{}, reusedServerJVMFlags...), "-Dconformance.fdbApiVersion=710")
		remoteFetchJava.invoker, remoteFetchJava.err = startJavaServer(flags...)
	})
	Expect(remoteFetchJava.err).NotTo(HaveOccurred())
	return remoteFetchJava.invoker
}

func indexRemoteSplit(shape string) bool {
	return shape == "basic" || shape == "compound" || shape == "split" || shape == "many"
}

func indexRemoteFormat(shape string) int32 {
	switch shape {
	case "legacy":
		return 4
	case "legacy_suffix":
		return 5
	default:
		return 14
	}
}

func indexRemotePrice(shape string, id int) int64 {
	if shape == "many" {
		return int64(id)
	}
	return int64(1000 - id)
}

func indexRemotePrimaryKey(shape string, id int) tuple.Tuple {
	if shape == "compound" {
		return tuple.Tuple{int64(id % 3), int64(id)}
	}
	return tuple.Tuple{int64(id)}
}

func indexRemoteMetadata(shape string) *recordlayer.RecordMetaData {
	builder := recordlayer.NewRecordMetaDataBuilder().SetRecords(demopb.File_record_layer_demo_proto)
	pk := recordlayer.Field("order_id")
	if shape == "compound" {
		pk = recordlayer.Concat(recordlayer.Field("quantity"), recordlayer.Field("order_id"))
	}
	builder.GetRecordType("Order").SetPrimaryKey(pk)
	builder.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
	builder.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
	builder.SetSplitLongRecords(indexRemoteSplit(shape)).SetStoreRecordVersions(true)
	bitmap := recordlayer.NewBitmapValueIndex("bitmap", recordlayer.GroupBy(recordlayer.Field("order_id"),
		recordlayer.Concat(recordlayer.Field("price"), recordlayer.Field("quantity"))))
	bitmap.SetOption(recordlayer.IndexOptionBitmapValueEntrySize, "16")
	for _, index := range []*recordlayer.Index{
		recordlayer.NewIndex("price", recordlayer.Field("price")),
		recordlayer.NewIndex("pk", recordlayer.Field("order_id")),
		recordlayer.NewIndex("pk_first", recordlayer.Concat(recordlayer.Field("price"), recordlayer.Field("quantity"))),
		recordlayer.NewIndex("pk_second", recordlayer.Concat(recordlayer.Field("price"), recordlayer.Field("order_id"))),
		recordlayer.NewVersionIndex("version", recordlayer.Concat(recordlayer.Field("price"), recordlayer.VersionKey())),
		recordlayer.NewRankIndex("rank", recordlayer.GroupBy(recordlayer.Field("price"))),
		bitmap,
	} {
		builder.AddIndex("Order", index.SetSubspaceKey(index.Name))
	}
	metadata, err := builder.Build()
	Expect(err).NotTo(HaveOccurred())
	return metadata
}

func indexRemoteRange(name string) recordlayer.TupleRange {
	switch name {
	case "price_tail":
		return recordlayer.TupleRangeBetweenInclusive(tuple.Tuple{int64(994)}, tuple.Tuple{int64(999)})
	case "price_prefix":
		return recordlayer.TupleRangeAllOf(tuple.Tuple{int64(997)})
	default:
		return recordlayer.TupleRangeAll
	}
}

func indexRemoteMethod(name string) recordlayer.IndexFetchMethod {
	switch name {
	case "SCAN_AND_FETCH":
		return recordlayer.IndexFetchScanAndFetch
	case "USE_REMOTE_FETCH":
		return recordlayer.IndexFetchUseRemoteFetch
	case "USE_REMOTE_FETCH_WITH_FALLBACK":
		return recordlayer.IndexFetchUseRemoteFetchWithFallback
	}
	Fail("unknown fetch method " + name)
	return 0
}

func indexRemoteOrphan(name string) recordlayer.IndexOrphanBehavior {
	switch name {
	case "ERROR":
		return recordlayer.IndexOrphanError
	case "SKIP":
		return recordlayer.IndexOrphanSkip
	case "RETURN":
		return recordlayer.IndexOrphanReturn
	}
	Fail("unknown orphan behavior " + name)
	return 0
}

func indexRemoteReason(reason recordlayer.NoNextReason) string {
	return map[recordlayer.NoNextReason]string{
		recordlayer.SourceExhausted: "SOURCE_EXHAUSTED", recordlayer.ReturnLimitReached: "RETURN_LIMIT_REACHED",
		recordlayer.ScanLimitReached: "SCAN_LIMIT_REACHED", recordlayer.ByteLimitReached: "BYTE_LIMIT_REACHED",
		recordlayer.TimeLimitReached: "TIME_LIMIT_REACHED",
	}[reason]
}

// indexRemoteException names the Java exception class an error stands for, as
// the Java step reports its root cause.
func indexRemoteException(err error) (string, int) {
	var native fdb.Error
	if errors.As(err, &native) {
		return "FDBException", native.Code
	}
	var storage *recordlayer.RecordCoreStorageError
	if errors.As(err, &storage) {
		return "RecordCoreStorageException", 0
	}
	var argument *recordlayer.RecordCoreArgumentError
	if errors.As(err, &argument) {
		return "RecordCoreArgumentException", 0
	}
	var unsupported *recordlayer.UnsupportedOperationError
	if errors.As(err, &unsupported) {
		return "UnsupportedOperationException", 0
	}
	var scanLimit *recordlayer.ScanLimitReachedError
	if errors.As(err, &scanLimit) {
		return "ScanLimitReachedException", 0
	}
	var remote *recordlayer.UnsupportedRemoteFetchIndexError
	if errors.As(err, &remote) {
		return "UnsupportedRemoteFetchIndexException", 0
	}
	var recordCore interface{ JavaRecordCoreException() }
	if errors.As(err, &recordCore) {
		return "RecordCoreException", 0
	}
	return fmt.Sprintf("%T", err), 0
}

var _ = Describe("Index remote fetch Record Layer Java conformance (RFC-258)", func() {
	var (
		ctx  context.Context
		env  *TenantEnvironment
		java *JavaInvoker
		ks   subspace.Subspace
	)
	methods := []string{"SCAN_AND_FETCH", "USE_REMOTE_FETCH", "USE_REMOTE_FETCH_WITH_FALLBACK"}
	request := func(shape, index string) indexRemoteRequest {
		pkLength := 1
		if shape == "compound" {
			pkLength = 2
		}
		return indexRemoteRequest{
			Shape: shape, IndexName: index, Method: "USE_REMOTE_FETCH", Orphan: "ERROR",
			Range: "all", Streaming: "ITERATOR", ScanType: "BY_VALUE", CommonPKLength: pkLength,
			LocalWriteID: -1, LocalPrimaryWriteID: -1,
		}
	}
	params := func(req indexRemoteRequest) map[string]any {
		return map[string]any{
			"clusterFile": env.ClusterFile, "tenantName": env.TenantName,
			"subspace": BytesToIntArray(ks.Bytes()), "request": req,
		}
	}
	seed := func(shape string, count int, orphan bool) {
		// The tenant isolates the spec. The storage server parses the mapper with
		// a tuple decoder that has no nested tuples, so the prefix has none.
		ks = subspace.FromBytes(tuple.Tuple{"remote", shape}.Pack())
		var written int
		Expect(java.InvokeAs(ctx, "seedIndexRemoteFetch", map[string]any{
			"clusterFile": env.ClusterFile, "tenantName": env.TenantName, "subspace": BytesToIntArray(ks.Bytes()),
			"shape": shape, "count": count, "orphan": orphan,
		}, &written)).To(Succeed())
		Expect(written).To(Equal(count))
	}
	openGo := func(shape string) (*recordlayer.FDBRecordStore, fdb.Transaction) {
		transaction, err := env.RecordDB.CreateTransaction()
		Expect(err).NotTo(HaveOccurred())
		rtx := env.RecordDB.NewRecordContext(transaction)
		rtx.SetTimer(recordlayer.NewStoreTimer())
		store, err := recordlayer.NewStoreBuilder().SetContext(rtx).SetSubspace(ks).
			SetMetaDataProvider(indexRemoteMetadata(shape)).SetFormatVersion(indexRemoteFormat(shape)).CreateOrOpen()
		Expect(err).NotTo(HaveOccurred())
		return store, transaction
	}
	localWrite := func(store *recordlayer.FDBRecordStore, shape string, id int) {
		previous, err := store.LoadRecord(indexRemotePrimaryKey(shape, id))
		Expect(err).NotTo(HaveOccurred())
		Expect(previous).NotTo(BeNil())
		order := proto.Clone(previous.Record).(*demopb.Order)
		order.Tags = []string{"local-write"}
		_, err = store.SaveRecord(order)
		Expect(err).NotTo(HaveOccurred())
	}
	scanGo := func(req indexRemoteRequest) indexRemotePage {
		store, transaction := openGo(req.Shape)
		defer transaction.Cancel()
		if req.LocalWriteID >= 0 {
			localWrite(store, req.Shape, req.LocalWriteID)
		}
		if req.LocalPrimaryWriteID >= 0 {
			id := req.LocalPrimaryWriteID
			index := store.GetRecordMetaData().GetIndex("price")
			transaction.Set(store.IndexSubspace(index).Pack(tuple.Tuple{indexRemotePrice(req.Shape, id), int64(id)}), tuple.Tuple{int64(42)}.Pack())
		}
		execution := recordlayer.DefaultExecuteProperties()
		execution.ReturnedRowLimit, execution.ScannedRecordsLimit = req.RowLimit, req.ScannedLimit
		execution.ScannedBytesLimit = req.ByteLimit
		execution.FailOnScanLimitReached = req.FailOnScanLimit
		if req.Snapshot {
			execution.IsolationLevel = recordlayer.IsolationLevelSnapshot
		}
		if req.TimeLimit > 0 {
			execution.TimeLimit = time.Duration(req.TimeLimit) * time.Millisecond
			time.Sleep(execution.TimeLimit + 25*time.Millisecond)
		}
		execution.ScanState = recordlayer.NewScanLimiterState()
		streaming := map[string]recordlayer.CursorStreamingMode{
			"ITERATOR": recordlayer.StreamingModeIterator,
			"SMALL":    recordlayer.StreamingModeSmall, "MEDIUM": recordlayer.StreamingModeMedium,
			"LARGE": recordlayer.StreamingModeLarge, "WANT_ALL": recordlayer.StreamingModeWantAll,
		}[req.Streaming]
		properties := recordlayer.NewScanProperties(execution).WithReverse(req.Reverse).WithStreamingMode(streaming)
		var continuation []byte
		if req.Continuation != "" {
			var err error
			continuation, err = base64.StdEncoding.DecodeString(req.Continuation)
			Expect(err).NotTo(HaveOccurred())
		}
		scanType := recordlayer.IndexScanByValue
		if req.ScanType == "BY_GROUP" {
			scanType = recordlayer.IndexScanByGroup
		}
		store.Context().Timer().Reset()
		page := indexRemotePage{Rows: make([]indexRemoteRow, 0)}
		cursor := store.ScanIndexRecordsWithFetchMethod(store.GetRecordMetaData().GetIndex(req.IndexName), indexRemoteMethod(req.Method), scanType,
			indexRemoteRange(req.Range), req.CommonPKLength, continuation, indexRemoteOrphan(req.Orphan), properties)
		defer func() { Expect(cursor.Close()).To(Succeed()) }()
		for {
			next, err := cursor.OnNext(ctx)
			if err != nil {
				page.ExceptionClass, page.ErrorCode = indexRemoteException(err)
				break
			}
			token, err := next.GetContinuation().ToBytes()
			Expect(err).NotTo(HaveOccurred())
			if !next.HasNext() {
				page.Continuation, page.End = indexRemoteBytes(token), next.GetContinuation().IsEnd()
				page.Reason = indexRemoteReason(next.GetNoNextReason())
				break
			}
			indexed := next.GetValue()
			row := indexRemoteRow{
				IndexKey: indexRemoteBytes(indexed.IndexEntry.Key.Pack()), IndexValue: indexRemoteBytes(indexed.IndexEntry.Value.Pack()),
				PrimaryKey: indexRemoteBytes(indexed.IndexEntry.PrimaryKey().Pack()), Continuation: indexRemoteBytes(token),
				Present: indexed.Record != nil,
			}
			if record := indexed.Record; record != nil {
				data, err := proto.MarshalOptions{Deterministic: true}.Marshal(record.Record)
				Expect(err).NotTo(HaveOccurred())
				row.Data = indexRemoteBytes(data)
				if record.Version != nil {
					row.Version = indexRemoteBytes(record.Version.ToBytes())
				}
				row.KeyCount, row.KeySize, row.ValueSize, row.Split = record.KeyCount, record.KeySize, record.ValueSize, record.Split
			}
			page.Rows = append(page.Rows, row)
		}
		// Record loads issued ahead by SCAN_AND_FETCH's pipeline charge bytes when they complete.
		execution.ScanState.WaitForIssuedRecordLoadCharges()
		page.ScannedRecords, page.ScannedBytes = execution.ScanState.RecordsScanned(), execution.ScanState.BytesScanned()
		page.RemoteEntries = store.Context().Timer().GetCount(recordlayer.EventScanRemoteFetchEntry)
		page.BadEntries = store.Context().Timer().GetCount(recordlayer.CountBadIndexEntry)
		return page
	}
	compare := func(req indexRemoteRequest) indexRemotePage {
		var oracle indexRemotePage
		Expect(java.InvokeAs(ctx, "scanIndexRemoteFetch", params(req), &oracle)).To(Succeed())
		actual := scanGo(req)
		// Scalars first, then row by row, so a mismatch is readable.
		summary := func(page indexRemotePage) indexRemotePage {
			page.Rows = nil
			return page
		}
		Expect(summary(actual)).To(Equal(summary(oracle)), "%+v", req)
		Expect(actual.Rows).To(HaveLen(len(oracle.Rows)), "%+v", req)
		for i := range oracle.Rows {
			Expect(actual.Rows[i]).To(Equal(oracle.Rows[i]), "row %d of %+v", i, req)
		}
		return oracle
	}
	mapper := func(req indexRemoteRequest) {
		var oracle struct {
			Mapper          string `json:"mapper"`
			IndexSubspace   string `json:"indexSubspace"`
			RecordsSubspace string `json:"recordsSubspace"`
			CommonPKLength  int    `json:"commonPKLength"`
		}
		Expect(java.InvokeAs(ctx, "indexRemoteFetchMapper", params(req), &oracle)).To(Succeed())
		store, transaction := openGo(req.Shape)
		defer transaction.Cancel()
		index := store.GetRecordMetaData().GetIndex(req.IndexName)
		actual, err := store.CreateRemoteFetchMapper(index, req.CommonPKLength)
		Expect(err).NotTo(HaveOccurred())
		Expect(indexRemoteBytes(actual.Pack())).To(Equal(oracle.Mapper), "mapper bytes for %s/%s", req.Shape, req.IndexName)
		Expect(indexRemoteBytes(store.IndexSubspace(index).Bytes())).To(Equal(oracle.IndexSubspace))
		Expect(indexRemoteBytes(store.RecordsSubspace().Bytes())).To(Equal(oracle.RecordsSubspace))
		Expect(store.GetCommonPrimaryKeyLength(index)).To(Equal(oracle.CommonPKLength))
	}

	BeforeEach(func() {
		ctx = context.Background()
		var err error
		env, err = SetupTenantEnvironment(ctx, sharedContainer, "index_remote_"+uuid.NewString())
		Expect(err).NotTo(HaveOccurred())
		java = remoteFetchJavaInvoker()
	})
	AfterEach(func() {
		if env != nil {
			Expect(env.Cleanup(ctx)).To(Succeed())
		}
	})

	// RemoteFetchIndexScanTest (simple, reverse, complex, continuations, mixed
	// continuations), RemoteFetchSplitRecordsTest, RemoteFetchWithoutSplitRecordsTest,
	// RemoteFetchMultiColumnKeyTest and RemoteFetchOldVersionTest's layouts.
	for _, shape := range []string{"basic", "compound", "split", "unsplit", "legacy", "legacy_suffix"} {
		It("matches records, versions, sizes and every continuation across methods for "+shape, func() {
			count := 6
			if shape == "basic" {
				count = 12
			}
			seed(shape, count, false)
			indexes := []string{"price", "pk", "version"}
			if shape == "compound" {
				indexes = []string{"price", "pk_first", "pk_second", "pk", "version"}
			}
			for _, index := range indexes {
				for _, reverse := range []bool{false, true} {
					req := request(shape, index)
					req.Reverse = reverse
					mapper(req)
					var baseline indexRemotePage
					for _, method := range methods {
						req.Method = method
						page := compare(req)
						Expect(page.ExceptionClass).To(BeEmpty())
						Expect(page.Rows).To(HaveLen(count))
						Expect(page.End).To(BeTrue())
						if baseline.Rows == nil {
							baseline = page
						} else {
							Expect(page.Rows).To(Equal(baseline.Rows), "every method yields the same rows and tokens")
						}
						if method == "SCAN_AND_FETCH" {
							Expect(page.RemoteEntries).To(BeZero())
						} else {
							Expect(page.RemoteEntries).To(Equal(int64(count+1)), "one per result, including the exhausted one")
						}
						for _, row := range page.Rows {
							version, err := base64.StdEncoding.DecodeString(row.Version)
							Expect(err).NotTo(HaveOccurred())
							Expect(version).To(HaveLen(12))
						}
					}
					// Every token, resumed by every method.
					for position, row := range baseline.Rows {
						for _, method := range methods {
							req.Method, req.Continuation = method, row.Continuation
							resumed := compare(req)
							Expect(resumed.ExceptionClass).To(BeEmpty())
							Expect(resumed.Rows).To(Equal(baseline.Rows[position+1:]))
						}
					}
					req.Continuation = ""
				}
			}
		})
	}

	It("matches bounds, streaming modes and row, scanned-record, byte and time limits", func() {
		seed("basic", 12, false)
		for _, reverse := range []bool{false, true} {
			for _, bounds := range []string{"all", "price_tail", "price_prefix"} {
				req := request("basic", "price")
				req.Reverse, req.Range = reverse, bounds
				mapper(req)
				for _, method := range methods {
					req.Method = method
					for _, streaming := range []string{"SMALL", "MEDIUM", "LARGE", "ITERATOR", "WANT_ALL"} {
						req.Streaming = streaming
						Expect(compare(req).ExceptionClass).To(BeEmpty())
					}
					req.Streaming = "ITERATOR"
					for _, limit := range []int{1, 3, 12, 13} {
						req.RowLimit = limit
						Expect(len(compare(req).Rows)).To(BeNumerically("<=", limit))
					}
					req.RowLimit = 0
					// testScanLimit
					for _, records := range []int{1, 3} {
						req.ScannedLimit = records
						Expect(len(compare(req).Rows)).To(BeNumerically("<=", records))
					}
					req.ScannedLimit, req.ByteLimit = 0, 1
					Expect(compare(req).Rows).To(HaveLen(1))
					req.ByteLimit, req.TimeLimit = 0, 1
					Expect(compare(req).Reason).To(Equal("TIME_LIMIT_REACHED"))
					req.TimeLimit = 0
				}
			}
		}
	})

	It("matches orphan ERROR, SKIP and RETURN with BAD_INDEX_ENTRY", func() {
		seed("basic", 12, true)
		for _, reverse := range []bool{false, true} {
			for _, method := range methods {
				for _, orphan := range []string{"ERROR", "SKIP", "RETURN"} {
					req := request("basic", "price")
					req.Reverse, req.Method, req.Orphan = reverse, method, orphan
					page := compare(req)
					switch orphan {
					case "ERROR":
						Expect(page.ExceptionClass).To(Equal("RecordCoreStorageException"))
						if method == "USE_REMOTE_FETCH_WITH_FALLBACK" {
							Expect(page.BadEntries).To(Equal(int64(2)), "the fallback meets the orphan again")
						} else {
							Expect(page.BadEntries).To(Equal(int64(1)))
						}
					case "SKIP":
						Expect(page.Rows).To(HaveLen(11))
					case "RETURN":
						Expect(page.Rows).To(HaveLen(12))
					}
				}
			}
		}
	})

	It("matches immediate failures and their fallback", func() {
		seed("basic", 12, false)
		for _, method := range methods {
			for _, arm := range []string{"snapshot", "zero_pk", "negative_pk", "oversized_pk", "unsupported_bitmap", "unsupported_rank", "wrong_scan_type", "primary_ryw", "secondary_ryw"} {
				req := request("basic", "price")
				req.Method = method
				switch arm {
				case "snapshot":
					req.Snapshot = true
				case "zero_pk":
					req.CommonPKLength = 0
				case "negative_pk":
					req.CommonPKLength = -1
				case "oversized_pk":
					req.CommonPKLength = 86
				case "unsupported_bitmap":
					req.IndexName = "bitmap"
				case "unsupported_rank":
					req.IndexName = "rank"
				case "wrong_scan_type":
					req.ScanType = "BY_GROUP"
				case "primary_ryw":
					req.LocalPrimaryWriteID = 1
				case "secondary_ryw":
					req.LocalWriteID = 1
				}
				page := compare(req)
				if method == "USE_REMOTE_FETCH" || arm == "unsupported_bitmap" || arm == "wrong_scan_type" {
					Expect(page.ExceptionClass).NotTo(BeEmpty(), arm)
				} else {
					Expect(page.ExceptionClass).To(BeEmpty(), arm)
					Expect(page.Rows).To(HaveLen(12), arm)
				}
			}
		}
	})

	It("matches read-your-writes failures after returned rows and the gapless fallback", func() {
		seed("many", 500, false)
		// failAfterRecordsReturnedTest: the write precedes the scan. A write made
		// mid-scan would race Java's read-ahead of the next batch.
		for _, reverse := range []bool{false, true} {
			for _, method := range methods {
				req := request("many", "price")
				req.Reverse, req.Method = reverse, method
				req.LocalWriteID = 499
				if reverse {
					req.LocalWriteID = 0
				}
				page := compare(req)
				if method == "USE_REMOTE_FETCH" {
					Expect(page.ErrorCode).To(Equal(2039))
					Expect(page.Rows).NotTo(BeEmpty(), "fails after returning rows")
					continue
				}
				Expect(page.ExceptionClass).To(BeEmpty())
				Expect(page.Rows).To(HaveLen(500))
				for position, row := range page.Rows {
					id := position
					if reverse {
						id = 499 - position
					}
					Expect(row.PrimaryKey).To(Equal(indexRemoteBytes(tuple.Tuple{int64(id)}.Pack())))
				}
			}
		}
	})

	It("matches writes outside the range and fail-on-scan-limit", func() {
		seed("basic", 12, false)
		for _, method := range methods {
			req := request("basic", "price")
			req.Method, req.Range, req.LocalWriteID = method, "price_tail", 11
			page := compare(req)
			Expect(page.ExceptionClass).To(BeEmpty())
			Expect(page.Rows).To(HaveLen(6))
			req.LocalWriteID, req.Range, req.ScannedLimit, req.FailOnScanLimit = -1, "all", 1, true
			Expect(compare(req).ExceptionClass).To(Equal("ScanLimitReachedException"))
		}
	})
})
