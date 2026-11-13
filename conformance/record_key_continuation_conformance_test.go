//go:build bazelrunfiles

package conformance_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
	"github.com/google/uuid"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

type recordKeyInteropRequest struct {
	Layout       string  `json:"layout"`
	Reverse      bool    `json:"reverse"`
	RowLimit     int     `json:"rowLimit"`
	Skip         int     `json:"skip"`
	ScannedLimit int     `json:"scannedLimit"`
	ByteLimit    int64   `json:"byteLimit"`
	Continuation *string `json:"continuation"` // nil=start; pointer to ""=present empty token
}

type recordKeyInteropRow struct {
	PrimaryKey   string `json:"primaryKey"`
	Continuation string `json:"continuation"`
}

type recordKeyInteropTerminal struct {
	Reason         string  `json:"reason"`
	Continuation   *string `json:"continuation"`
	End            bool    `json:"end"`
	ScannedRecords int     `json:"scannedRecords"`
	ScannedBytes   int64   `json:"scannedBytes"`
}

type recordKeyInteropPage struct {
	recordKeyInteropTerminal
	Rows           []recordKeyInteropRow      `json:"rows"`
	Repeats        []recordKeyInteropTerminal `json:"repeats"`
	ExceptionClass string                     `json:"exceptionClass"`
}

func recordKeyInteropBytes(raw []byte) *string {
	if raw == nil {
		return nil
	}
	encoded := base64.StdEncoding.EncodeToString(raw)
	return &encoded
}

func recordKeyInteropDecode(encoded *string) []byte {
	if encoded == nil {
		return nil
	}
	raw, err := base64.StdEncoding.DecodeString(*encoded)
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	// Preserve presence even if a decoder represents empty output as nil.
	if raw == nil {
		return []byte{}
	}
	return raw
}

// Fixed tuple bytes for ids 0..5; no derivation from either engine's result.
func recordKeyInteropPK(id byte) []byte {
	if id == 0 {
		return []byte{0x14}
	}
	return []byte{0x15, id}
}

func recordKeyInteropIDs(page recordKeyInteropPage) []int64 {
	ids := []int64{}
	for _, row := range page.Rows {
		raw, err := base64.StdEncoding.DecodeString(row.PrimaryKey)
		ExpectWithOffset(1, err).NotTo(HaveOccurred())
		pk, err := tuple.Unpack(raw)
		ExpectWithOffset(1, err).NotTo(HaveOccurred())
		ExpectWithOffset(1, pk).To(HaveLen(1))
		ids = append(ids, pk[0].(int64))
	}
	return ids
}

func recordKeyInteropAssertFraming(page recordKeyInteropPage, bare bool) {
	for _, row := range page.Rows {
		raw, err := base64.StdEncoding.DecodeString(row.Continuation)
		ExpectWithOffset(1, err).NotTo(HaveOccurred())
		pk, err := base64.StdEncoding.DecodeString(row.PrimaryKey)
		ExpectWithOffset(1, err).NotTo(HaveOccurred())
		inner := raw
		if !bare {
			var dedup gen.DedupContinuation
			ExpectWithOffset(1, dedup.UnmarshalVT(raw)).To(Succeed())
			ExpectWithOffset(1, dedup.LastValue).To(Equal(pk))
			inner = dedup.InnerContinuation
		}
		var leaf gen.KeyValueCursorContinuation
		ExpectWithOffset(1, leaf.UnmarshalVT(inner)).To(Succeed())
		ExpectWithOffset(1, leaf.MagicNumber).NotTo(BeNil())
		ExpectWithOffset(1, leaf.GetMagicNumber()).To(Equal(int64(6773487359078157740)))
		if bare {
			ExpectWithOffset(1, leaf.InnerContinuation).To(Equal(pk))
		} else {
			ExpectWithOffset(1, bytes.HasPrefix(leaf.InnerContinuation, pk)).To(BeTrue())
			ExpectWithOffset(1, len(leaf.InnerContinuation)).To(BeNumerically(">", len(pk)))
		}
	}
	if page.ExceptionClass != "" {
		return
	}
	ExpectWithOffset(1, page.Repeats).To(HaveLen(2))
	for _, repeated := range page.Repeats {
		ExpectWithOffset(1, repeated).To(Equal(page.recordKeyInteropTerminal))
	}
	if page.End {
		ExpectWithOffset(1, page.Reason).To(Equal("SOURCE_EXHAUSTED"))
		ExpectWithOffset(1, page.Continuation).To(BeNil())
	} else {
		ExpectWithOffset(1, page.Continuation).NotTo(BeNil())
		ExpectWithOffset(1, *page.Continuation).NotTo(BeEmpty())
	}
}

var _ = Describe("Record-key opaque continuation Java interchange", func() {
	var (
		ctx  context.Context
		env  *TenantEnvironment
		java *JavaInvoker
		ks   subspace.Subspace
	)
	BeforeEach(func() {
		ctx = context.Background()
		var err error
		env, err = SetupTenantEnvironment(ctx, sharedContainer, "record_key_cont_"+uuid.NewString())
		Expect(err).NotTo(HaveOccurred())
		java = NewJavaInvoker()
		ks = env.Keyspace.Sub("record_key_continuations")
	})
	AfterEach(func() {
		if env != nil {
			Expect(env.Cleanup(ctx)).To(Succeed())
		}
	})

	javaPage := func(req recordKeyInteropRequest) recordKeyInteropPage {
		var page recordKeyInteropPage
		Expect(java.InvokeAs(ctx, "scanRecordKeyContinuations", map[string]any{
			"clusterFile": env.ClusterFile, "tenantName": env.TenantName,
			"subspace": BytesToIntArray(ks.Bytes()), "request": req,
		}, &page)).To(Succeed())
		return page
	}
	goPage := func(req recordKeyInteropRequest) recordKeyInteropPage {
		layout := recordScanLayouts[req.Layout]
		tx, err := env.RecordDB.CreateTransaction()
		Expect(err).NotTo(HaveOccurred())
		defer tx.Cancel()
		store, err := recordlayer.NewStoreBuilder().SetContext(env.RecordDB.NewRecordContext(tx)).
			SetSubspace(ks.Sub(req.Layout)).SetFormatVersion(layout.format).
			SetMetaDataProvider(recordScanMetadata(layout.split, layout.scanVersions, layout.seedVersions != layout.scanVersions)).CreateOrOpen()
		Expect(err).NotTo(HaveOccurred())
		execution := recordlayer.DefaultExecuteProperties()
		execution.ReturnedRowLimit, execution.Skip = req.RowLimit, req.Skip
		execution.ScannedRecordsLimit, execution.ScannedBytesLimit = req.ScannedLimit, req.ByteLimit
		cursor := store.ScanRecordKeys(recordKeyInteropDecode(req.Continuation), recordlayer.NewScanProperties(execution).WithReverse(req.Reverse))
		defer func() { Expect(cursor.Close()).To(Succeed()) }()
		page := recordKeyInteropPage{Rows: []recordKeyInteropRow{}, Repeats: []recordKeyInteropTerminal{}}
		terminal := func(next recordlayer.RecordCursorResult[tuple.Tuple]) recordKeyInteropTerminal {
			raw, err := next.GetContinuation().ToBytes()
			Expect(err).NotTo(HaveOccurred())
			return recordKeyInteropTerminal{
				Reason: scanLimitReason(next.GetNoNextReason()), Continuation: recordKeyInteropBytes(raw), End: next.GetContinuation().IsEnd(),
				ScannedRecords: execution.ScanState.RecordsScanned(), ScannedBytes: execution.ScanState.BytesScanned(),
			}
		}
		for {
			next, err := cursor.OnNext(ctx)
			if err != nil {
				var parse *recordlayer.ContinuationParseError
				if errors.As(err, &parse) {
					page.ExceptionClass = "RecordCoreException"
				} else {
					page.ExceptionClass = fmt.Sprintf("%T", err)
				}
				break
			}
			if !next.HasNext() {
				page.recordKeyInteropTerminal = terminal(next)
				for range 2 {
					repeated, err := cursor.OnNext(ctx)
					Expect(err).NotTo(HaveOccurred())
					Expect(repeated.HasNext()).To(BeFalse())
					page.Repeats = append(page.Repeats, terminal(repeated))
				}
				break
			}
			raw, err := next.GetContinuation().ToBytes()
			Expect(err).NotTo(HaveOccurred())
			page.Rows = append(page.Rows, recordKeyInteropRow{
				PrimaryKey: base64.StdEncoding.EncodeToString(next.GetValue().Pack()), Continuation: *recordKeyInteropBytes(raw),
			})
		}
		page.ScannedRecords, page.ScannedBytes = execution.ScanState.RecordsScanned(), execution.ScanState.BytesScanned()
		return page
	}

	It("distinguishes a nil start from a present empty token on an empty store", func() {
		for _, reverse := range []bool{false, true} {
			req := recordKeyInteropRequest{Layout: "split_versions", Reverse: reverse}
			for _, page := range []recordKeyInteropPage{javaPage(req), goPage(req)} {
				Expect(page.ExceptionClass).To(BeEmpty())
				Expect(page.Rows).To(BeEmpty())
				Expect(page.End).To(BeTrue())
				Expect(page.Reason).To(Equal("SOURCE_EXHAUSTED"))
				Expect(page.Continuation).To(BeNil())
				recordKeyInteropAssertFraming(page, false)
			}
			empty := ""
			req.Continuation = &empty
			Expect(javaPage(req).ExceptionClass).To(Equal("RecordCoreException"))
			Expect(goPage(req).ExceptionClass).To(Equal("RecordCoreException"))
		}
	})

	for _, name := range []string{"unsplit", "split", "unsplit_versions", "split_versions", "omit_suffix_versions", "split_old_versions"} {
		It("exchanges successful and stopped tokens for "+name, func() {
			layout := recordScanLayouts[name]
			bare := name == "omit_suffix_versions"
			var written int
			Expect(java.InvokeAs(ctx, "seedRecordScanLimits", map[string]any{
				"clusterFile": env.ClusterFile, "tenantName": env.TenantName,
				"subspace": BytesToIntArray(ks.Bytes()), "layout": name,
			}, &written)).To(Succeed())
			Expect(written).To(Equal(6))
			// Assert the committed physical fixture before exchanging any tokens.
			tx, err := env.RecordDB.CreateTransaction()
			Expect(err).NotTo(HaveOccurred())
			physical, err := tx.GetRange(ks.Sub(name).Sub(recordlayer.RecordKey), fdb.RangeOptions{}).GetSliceWithError()
			tx.Cancel()
			Expect(err).NotTo(HaveOccurred())
			prefix := ks.Sub(name).Sub(recordlayer.RecordKey).Bytes()
			actual := [][]byte{}
			for _, kv := range physical {
				actual = append(actual, []byte(kv.Key[len(prefix):]))
			}
			wantPhysical := [][]byte{}
			for id := byte(0); id < 6; id++ {
				pk := recordKeyInteropPK(id)
				if bare {
					wantPhysical = append(wantPhysical, pk)
					continue
				}
				if layout.seedVersions && layout.format >= 6 {
					wantPhysical = append(wantPhysical, append(append([]byte{}, pk...), 0x13, 0xfe))
				}
				chunks := 0
				if layout.split && id == 2 {
					chunks = 2
				} else if layout.split && id == 4 {
					chunks = 3
				}
				if chunks == 0 {
					wantPhysical = append(wantPhysical, append(append([]byte{}, pk...), 0x14))
				} else {
					for chunk := byte(1); chunk <= byte(chunks); chunk++ {
						wantPhysical = append(wantPhysical, append(append([]byte{}, pk...), 0x15, chunk))
					}
				}
			}
			Expect(actual).To(Equal(wantPhysical))

			for _, reverse := range []bool{false, true} {
				want := []int64{0, 1, 2, 3, 4, 5}
				if reverse {
					want = []int64{5, 4, 3, 2, 1, 0}
				}
				base := recordKeyInteropRequest{Layout: name, Reverse: reverse}
				// Both engine-local controls and both alternating-engine starts must
				// enumerate a fixed sequence, including first-chunk/version positions.
				for _, mode := range []string{"java", "go", "java-go", "go-java"} {
					req := base
					req.RowLimit = 1
					ids := []int64{}
					ended := false
					for pageIndex := 0; pageIndex < 12; pageIndex++ {
						useJava := mode == "java" || (mode == "java-go" && pageIndex%2 == 0) || (mode == "go-java" && pageIndex%2 == 1)
						var page recordKeyInteropPage
						if useJava {
							page = javaPage(req)
						} else {
							page = goPage(req)
						}
						Expect(page.ExceptionClass).To(BeEmpty(), "%s reverse=%t mode=%s", name, reverse, mode)
						recordKeyInteropAssertFraming(page, bare)
						ids = append(ids, recordKeyInteropIDs(page)...)
						if page.End {
							ended = true
							break
						}
						Expect(page.Reason).To(Equal("RETURN_LIMIT_REACHED"))
						Expect(page.Rows).To(HaveLen(1))
						Expect(page.Continuation).To(Equal(&page.Rows[0].Continuation))
						req.Continuation = page.Continuation
						if pageIndex%2 == 0 {
							req.Continuation = &page.Rows[0].Continuation
						}
					}
					Expect(ended).To(BeTrue(), "bounded walk never reached EOF")
					Expect(ids).To(Equal(want), "%s reverse=%t mode=%s", name, reverse, mode)
				}
				for _, limit := range []int{0, 2, 6} {
					req := base
					req.RowLimit = limit
					jp, gp := javaPage(req), goPage(req)
					Expect(gp).To(Equal(jp))
					recordKeyInteropAssertFraming(jp, bare)
					count := limit
					if count == 0 {
						count = 6
					}
					Expect(recordKeyInteropIDs(jp)).To(Equal(want[:count]))
					if limit == 0 {
						Expect(jp.End).To(BeTrue())
					} else {
						Expect(jp.Reason).To(Equal("RETURN_LIMIT_REACHED"))
						Expect(jp.End).To(BeFalse())
						// Opaque terminal tokens are exchanged in both directions.
						javaResume, goResume := base, base
						javaResume.Continuation, goResume.Continuation = gp.Continuation, jp.Continuation
						for _, resumed := range []recordKeyInteropPage{javaPage(javaResume), goPage(goResume)} {
							Expect(recordKeyInteropIDs(resumed)).To(Equal(want[count:]))
							Expect(resumed.ExceptionClass).To(BeEmpty())
							Expect(resumed.End).To(BeTrue())
							Expect(resumed.Reason).To(Equal("SOURCE_EXHAUSTED"))
							Expect(resumed.Continuation).To(BeNil())
						}
					}
				}

				// A one-physical-KV budget creates zero-row pages inside chunk and
				// inline-version runs. Alternate engines at EVERY such stop.
				if layout.split || (layout.seedVersions && layout.format >= 6) {
					for _, budget := range []string{"records", "bytes"} {
						for _, javaFirst := range []bool{false, true} {
							req := base
							if budget == "records" {
								req.ScannedLimit = 1
							} else {
								req.ByteLimit = 1
							}
							ids := []int64{}
							duplicatePages, ended := 0, false
							for pageIndex := 0; pageIndex < 40; pageIndex++ {
								var page recordKeyInteropPage
								if (pageIndex%2 == 0) == javaFirst {
									page = javaPage(req)
								} else {
									page = goPage(req)
								}
								recordKeyInteropAssertFraming(page, false)
								Expect(page.ExceptionClass).To(BeEmpty())
								ids = append(ids, recordKeyInteropIDs(page)...)
								if page.End {
									ended = true
									break
								}
								wantReason := "SCAN_LIMIT_REACHED"
								if budget == "bytes" {
									wantReason = "BYTE_LIMIT_REACHED"
								}
								Expect(page.Reason).To(Equal(wantReason))
								Expect(page.ScannedRecords).To(Equal(2))
								Expect(page.ScannedBytes).To(BeNumerically(">", 0))
								if len(page.Rows) == 0 {
									duplicatePages++
								}
								Expect(ids).NotTo(BeEmpty())
								var stopped gen.DedupContinuation
								Expect(stopped.UnmarshalVT(recordKeyInteropDecode(page.Continuation))).To(Succeed())
								Expect(stopped.LastValue).To(Equal(recordKeyInteropPK(byte(ids[len(ids)-1]))))
								Expect(page.Continuation).NotTo(Equal(req.Continuation))
								req.Continuation = page.Continuation
							}
							Expect(ended).To(BeTrue())
							Expect(duplicatePages).To(BeNumerically(">", 0))
							Expect(ids).To(Equal(want))
						}
					}
				}
			}
			if !bare {
				for _, token := range [][]byte{{}, {0xff}, {0x12, 0x01, 0x14}, {0x0a, 0x00, 0x12, 0x01, 0xff}} {
					req := recordKeyInteropRequest{Layout: name, Continuation: recordKeyInteropBytes(token)}
					jp, gp := javaPage(req), goPage(req)
					Expect(jp.ExceptionClass).NotTo(BeEmpty())
					Expect(gp.ExceptionClass).NotTo(BeEmpty())
					Expect(jp.Rows).To(BeEmpty())
					Expect(gp.Rows).To(BeEmpty())
				}
			}
		})
	}
})
