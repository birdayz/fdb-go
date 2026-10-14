//go:build bazelrunfiles

package conformance_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"

	"fdb.dev/gen/demopb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/recordlayer"
	"github.com/google/uuid"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"
)

type recordScanRequest struct {
	Layout          string `json:"layout"`
	Reverse         bool   `json:"reverse"`
	RowLimit        int    `json:"rowLimit"`
	Skip            int    `json:"skip"`
	ScannedLimit    int    `json:"scannedLimit"`
	ByteLimit       int64  `json:"byteLimit"`
	TimeLimit       int64  `json:"timeLimit"`
	FailOnScanLimit bool   `json:"failOnScanLimit"`
	LocalSave       bool   `json:"localSave"`
	Walk            bool   `json:"walk"`
	Continuation    string `json:"continuation"`
}

type recordScanRow struct {
	PrimaryKey      string `json:"primaryKey"`
	DataHash        string `json:"dataHash"`
	Version         string `json:"version"`
	KeyCount        int    `json:"keyCount"`
	KeySize         int    `json:"keySize"`
	ValueSize       int    `json:"valueSize"`
	Split           bool   `json:"split"`
	VersionedInline bool   `json:"versionedInline"`
	Continuation    string `json:"continuation"`
}

type recordScanTerminal struct {
	Reason         string `json:"reason"`
	Continuation   string `json:"continuation"`
	End            bool   `json:"end"`
	ScannedRecords int    `json:"scannedRecords"`
	ScannedBytes   int64  `json:"scannedBytes"`
}

type recordScanPage struct {
	recordScanTerminal
	Rows           []recordScanRow      `json:"rows"`
	Repeats        []recordScanTerminal `json:"repeats"`
	ExceptionClass string               `json:"exceptionClass"`
}

type recordCountResult struct {
	Count          int    `json:"count"`
	ExceptionClass string `json:"exceptionClass"`
	ScannedRecords int    `json:"scannedRecords"`
	ScannedBytes   int64  `json:"scannedBytes"`
}

type recordScanLayout struct {
	split, seedVersions, scanVersions bool
	format                            int32
}

// Mirrors RecordScanLimitSteps.LAYOUTS: format 4 omits the unsplit suffix and
// format 5 keeps versions in their own subspace.
var recordScanLayouts = map[string]recordScanLayout{
	"split_versions":       {true, true, true, 14},
	"split":                {true, false, false, 14},
	"split_stale_versions": {true, true, false, 14},
	"unsplit_versions":     {false, true, true, 14},
	"unsplit":              {false, false, false, 14},
	"omit_suffix_versions": {false, true, true, 4},
	"split_old_versions":   {true, true, true, 5},
	"unsplit_old_versions": {false, true, true, 5},
}

func recordScanMetadata(split, versions, evolved bool) *recordlayer.RecordMetaData {
	builder := recordlayer.NewRecordMetaDataBuilder().SetRecords(demopb.File_record_layer_demo_proto)
	builder.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
	builder.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
	builder.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
	builder.SetSplitLongRecords(split)
	builder.SetStoreRecordVersions(versions)
	if evolved {
		builder.SetVersion(100)
	}
	metadata, err := builder.Build()
	Expect(err).NotTo(HaveOccurred())
	return metadata
}

func recordScanOrder(id int64, split bool) *demopb.Order {
	size := 8
	switch {
	case split && id == 2:
		size = 150_000
	case split && id == 4:
		size = 250_000
	case split && id == 7:
		size = 120_000
	}
	return &demopb.Order{OrderId: proto.Int64(id), Price: proto.Int32(int32(100 - id)), VectorData: bytes.Repeat([]byte{byte(id)}, size)}
}

func recordScanDrain(ctx context.Context, cursor recordlayer.RecordCursor[*recordlayer.FDBStoredRecord[proto.Message]], state *recordlayer.ScanLimiterState) recordScanPage {
	defer func() { Expect(cursor.Close()).To(Succeed()) }()
	page := recordScanPage{Rows: make([]recordScanRow, 0), Repeats: make([]recordScanTerminal, 0)}
	terminal := func(next recordlayer.RecordCursorResult[*recordlayer.FDBStoredRecord[proto.Message]]) recordScanTerminal {
		continuation, err := next.GetContinuation().ToBytes()
		Expect(err).NotTo(HaveOccurred())
		return recordScanTerminal{
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
			page.recordScanTerminal = terminal(next)
			for repeat := 0; repeat < 2; repeat++ {
				repeated, err := cursor.OnNext(ctx)
				Expect(err).NotTo(HaveOccurred())
				page.Repeats = append(page.Repeats, terminal(repeated))
			}
			break
		}
		record := next.GetValue()
		data, err := proto.MarshalOptions{Deterministic: true}.Marshal(record.Record)
		Expect(err).NotTo(HaveOccurred())
		hash := sha256.Sum256(data)
		continuation, err := next.GetContinuation().ToBytes()
		Expect(err).NotTo(HaveOccurred())
		row := recordScanRow{
			PrimaryKey: scanLimitBytes(record.PrimaryKey.Pack()), DataHash: scanLimitBytes(hash[:]),
			KeyCount: record.KeyCount, KeySize: record.KeySize, ValueSize: record.ValueSize,
			Split: record.Split, VersionedInline: record.VersionedInline, Continuation: scanLimitBytes(continuation),
		}
		if record.Version != nil {
			row.Version = scanLimitBytes(record.Version.ToBytes())
		}
		page.Rows = append(page.Rows, row)
	}
	page.ScannedRecords, page.ScannedBytes = state.RecordsScanned(), state.BytesScanned()
	return page
}

// Java's scanTypedRecords: KeyValueUnsplitter (skip, limitRowsTo) over every
// suffixed layout, a plain KeyValueCursor over the suffix-less one. Every page
// of every walk is compared: rows with sizes, stop reason, token and counters.
// countRecords is compared on the same layouts.
var _ = Describe("Record scan Java limit conformance", func() {
	var (
		ctx  context.Context
		env  *TenantEnvironment
		java *JavaInvoker
		ks   subspace.Subspace
	)
	BeforeEach(func() {
		ctx = context.Background()
		var err error
		env, err = SetupTenantEnvironment(ctx, sharedContainer, "record_scan_limits_"+uuid.NewString())
		Expect(err).NotTo(HaveOccurred())
		java = NewJavaInvoker()
		ks = env.Keyspace.Sub("record_scan_limits")
	})
	AfterEach(func() {
		if env != nil {
			Expect(env.Cleanup(ctx)).To(Succeed())
		}
	})

	goPage := func(req recordScanRequest, continuation string) recordScanPage {
		layout := recordScanLayouts[req.Layout]
		transaction, err := env.RecordDB.CreateTransaction()
		Expect(err).NotTo(HaveOccurred())
		defer transaction.Cancel()
		rtx := env.RecordDB.NewRecordContext(transaction)
		store, err := recordlayer.NewStoreBuilder().SetContext(rtx).SetSubspace(ks.Sub(req.Layout)).
			SetMetaDataProvider(recordScanMetadata(layout.split, layout.scanVersions, layout.seedVersions != layout.scanVersions)).SetFormatVersion(layout.format).CreateOrOpen()
		Expect(err).NotTo(HaveOccurred())
		if req.LocalSave {
			for _, id := range []int64{6, 7} {
				_, err := store.SaveRecord(recordScanOrder(id, layout.split))
				Expect(err).NotTo(HaveOccurred())
			}
		}
		if req.TimeLimit > 0 {
			time.Sleep(time.Duration(req.TimeLimit+25) * time.Millisecond)
		}
		execution := recordlayer.DefaultExecuteProperties()
		execution.ReturnedRowLimit, execution.Skip, execution.ScannedRecordsLimit = req.RowLimit, req.Skip, req.ScannedLimit
		execution.ScannedBytesLimit = req.ByteLimit
		execution.TimeLimit = time.Duration(req.TimeLimit) * time.Millisecond
		execution.FailOnScanLimitReached = req.FailOnScanLimit
		var resume []byte
		if continuation != "" {
			resume, err = base64.StdEncoding.DecodeString(continuation)
			Expect(err).NotTo(HaveOccurred())
		}
		properties := recordlayer.NewScanProperties(execution).WithReverse(req.Reverse)
		return recordScanDrain(ctx, store.ScanRecords(resume, properties), execution.ScanState)
	}
	goScan := func(req recordScanRequest) []recordScanPage {
		pages := make([]recordScanPage, 0)
		continuation := req.Continuation
		for {
			page := goPage(req, continuation)
			pages = append(pages, page)
			if !req.Walk || page.ExceptionClass != "" || page.Reason == "SOURCE_EXHAUSTED" || page.Reason == "" || len(pages) >= 40 {
				return pages
			}
			continuation = page.Continuation
		}
	}
	goCount := func(req recordScanRequest) recordCountResult {
		layout := recordScanLayouts[req.Layout]
		transaction, err := env.RecordDB.CreateTransaction()
		Expect(err).NotTo(HaveOccurred())
		defer transaction.Cancel()
		store, err := recordlayer.NewStoreBuilder().SetContext(env.RecordDB.NewRecordContext(transaction)).SetSubspace(ks.Sub(req.Layout)).
			SetMetaDataProvider(recordScanMetadata(layout.split, layout.scanVersions, layout.seedVersions != layout.scanVersions)).SetFormatVersion(layout.format).CreateOrOpen()
		Expect(err).NotTo(HaveOccurred())
		if req.TimeLimit > 0 {
			time.Sleep(time.Duration(req.TimeLimit+25) * time.Millisecond)
		}
		execution := recordlayer.DefaultExecuteProperties()
		execution.ReturnedRowLimit, execution.Skip, execution.ScannedRecordsLimit = req.RowLimit, req.Skip, req.ScannedLimit
		execution.ScannedBytesLimit = req.ByteLimit
		execution.TimeLimit = time.Duration(req.TimeLimit) * time.Millisecond
		execution.FailOnScanLimitReached = req.FailOnScanLimit
		count, err := store.CountRecords(ctx, nil, nil, recordlayer.EndpointTypeTreeStart, recordlayer.EndpointTypeTreeEnd, nil,
			recordlayer.NewScanProperties(execution).WithReverse(req.Reverse))
		result := recordCountResult{Count: count, ScannedRecords: execution.ScanState.RecordsScanned(), ScannedBytes: execution.ScanState.BytesScanned()}
		var limit *recordlayer.ScanLimitReachedError
		if errors.As(err, &limit) {
			result.ExceptionClass = "ScanLimitReachedException"
		} else if err != nil {
			result.ExceptionClass = fmt.Sprintf("%T", err)
		}
		return result
	}
	compareCounts := func(requests []recordScanRequest) {
		var javaCounts []recordCountResult
		Expect(java.InvokeAs(ctx, "countRecordScanLimits", map[string]any{
			"clusterFile": env.ClusterFile, "tenantName": env.TenantName,
			"subspace": BytesToIntArray(ks.Bytes()), "requests": requests,
		}, &javaCounts)).To(Succeed())
		Expect(javaCounts).To(HaveLen(len(requests)))
		for i, req := range requests {
			described, err := json.Marshal(req)
			Expect(err).NotTo(HaveOccurred())
			got, want := goCount(req), javaCounts[i]
			if want.ExceptionClass != "" {
				// Java throws without a count.
				got.Count = want.Count
			} else if got.ExceptionClass == "ScanLimitReachedException" && !req.FailOnScanLimit {
				// Go also reports a count cut short by a scan limit (RFC-106a).
				got.ExceptionClass = ""
			}
			Expect(got).To(Equal(want), "count request %s", described)
		}
	}
	compare := func(requests []recordScanRequest) [][]recordScanPage {
		var javaPages [][]recordScanPage
		Expect(java.InvokeAs(ctx, "scanRecordScanLimits", map[string]any{
			"clusterFile": env.ClusterFile, "tenantName": env.TenantName,
			"subspace": BytesToIntArray(ks.Bytes()), "requests": requests,
		}, &javaPages)).To(Succeed())
		Expect(javaPages).To(HaveLen(len(requests)))
		for i, req := range requests {
			described, err := json.Marshal(req)
			Expect(err).NotTo(HaveOccurred())
			Expect(goScan(req)).To(Equal(javaPages[i]), "request %s", described)
		}
		return javaPages
	}

	for _, name := range []string{
		"split_versions", "split", "split_stale_versions", "unsplit_versions",
		"unsplit", "omit_suffix_versions", "split_old_versions", "unsplit_old_versions",
	} {
		It("matches Java for layout "+name, func() {
			var written int
			Expect(java.InvokeAs(ctx, "seedRecordScanLimits", map[string]any{
				"clusterFile": env.ClusterFile, "tenantName": env.TenantName,
				"subspace": BytesToIntArray(ks.Bytes()), "layout": name,
			}, &written)).To(Succeed())
			Expect(written).To(Equal(6))
			for _, reverse := range []bool{false, true} {
				base := recordScanRequest{Layout: name, Reverse: reverse}
				full := compare([]recordScanRequest{base})[0][0]
				Expect(full.Rows).To(HaveLen(6))
				Expect(full.Reason).To(Equal("SOURCE_EXHAUSTED"))

				with := func(f func(*recordScanRequest)) recordScanRequest {
					req := base
					f(&req)
					return req
				}
				var requests []recordScanRequest
				for rows := 1; rows <= 7; rows++ {
					requests = append(requests, with(func(r *recordScanRequest) { r.RowLimit, r.Walk = rows, rows <= 3 }))
				}
				for skip := 1; skip <= 2; skip++ {
					requests = append(requests, with(func(r *recordScanRequest) { r.Skip, r.RowLimit, r.Walk = skip, 2, true }))
					requests = append(requests, with(func(r *recordScanRequest) { r.Skip, r.ScannedLimit = skip, 3 }))
				}
				for scanned := 1; scanned <= full.ScannedRecords+1; scanned++ {
					requests = append(requests, with(func(r *recordScanRequest) { r.ScannedLimit, r.Walk = scanned, scanned == 2 || scanned == 3 }))
				}
				// Byte limits on and beside every record boundary, and inside each record.
				byteLimits := map[int64]bool{1: true}
				var crossed int64
				for _, row := range full.Rows {
					size := int64(row.KeySize + row.ValueSize)
					byteLimits[crossed+size/2] = true
					crossed += size
					byteLimits[crossed-1], byteLimits[crossed], byteLimits[crossed+1] = true, true, true
				}
				for _, limit := range slices.Sorted(maps.Keys(byteLimits)) {
					requests = append(requests, with(func(r *recordScanRequest) { r.ByteLimit, r.Walk = limit, limit == 1 }))
				}
				// Java's mapPipelined loads old-format versions asynchronously; a thrown
				// limit or a time limit can then drop rows still loading, so Java races.
				if layout := recordScanLayouts[name]; layout.format >= 6 {
					for scanned := 1; scanned <= 3; scanned++ {
						requests = append(requests, with(func(r *recordScanRequest) { r.ScannedLimit, r.FailOnScanLimit = scanned, true }))
					}
					requests = append(requests,
						with(func(r *recordScanRequest) { r.ByteLimit, r.FailOnScanLimit = 1, true }),
						with(func(r *recordScanRequest) { r.TimeLimit = 100 }),
						with(func(r *recordScanRequest) { r.TimeLimit, r.FailOnScanLimit = 100, true }),
					)
				}
				requests = append(requests,
					with(func(r *recordScanRequest) { r.LocalSave = true }),
					with(func(r *recordScanRequest) { r.LocalSave, r.ScannedLimit = true, 2 }),
				)
				compare(requests)

				counts := []recordScanRequest{base}
				for skip := 1; skip <= 2; skip++ {
					counts = append(counts, with(func(r *recordScanRequest) { r.Skip = skip }))
				}
				for scanned := 1; scanned <= 4; scanned++ {
					counts = append(counts, with(func(r *recordScanRequest) { r.ScannedLimit = scanned }))
				}
				counts = append(counts,
					with(func(r *recordScanRequest) { r.ByteLimit = 1 }),
					with(func(r *recordScanRequest) { r.RowLimit = 2 }),
					with(func(r *recordScanRequest) { r.TimeLimit = 100 }),
					with(func(r *recordScanRequest) { r.ScannedLimit, r.FailOnScanLimit = 2, true }),
				)
				compareCounts(counts)
			}
		})
	}
})
