package recordlayer

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"fdb.dev/gen"
	"fdb.dev/gen/demopb"
	"fdb.dev/pkg/dst"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"
)

// These bytes are independent of the production continuation encoder.
func recordKeyLiteralLeaf(physical []byte) []byte {
	out := append([]byte{0x0a, byte(len(physical))}, physical...)
	return append(out, 0x11, 0xac, 0xcd, 0x73, 0x98, 0xdd, 0x42, 0x00, 0x5e)
}

func recordKeyLiteralToken(id byte, suffix []byte) []byte {
	leaf := recordKeyLiteralLeaf(append([]byte{0x15, id}, suffix...))
	out := append([]byte{0x0a, byte(len(leaf))}, leaf...)
	return append(out, 0x12, 0x02, 0x15, id)
}

func recordKeyAssertToken(raw []byte, id byte, suffix []byte) {
	ExpectWithOffset(1, raw).To(Equal(recordKeyLiteralToken(id, suffix)))
	var outer gen.DedupContinuation
	ExpectWithOffset(1, outer.UnmarshalVT(raw)).To(Succeed())
	ExpectWithOffset(1, outer.LastValue).To(Equal([]byte{0x15, id}))
	var leaf gen.KeyValueCursorContinuation
	ExpectWithOffset(1, leaf.UnmarshalVT(outer.InnerContinuation)).To(Succeed())
	ExpectWithOffset(1, leaf.MagicNumber).NotTo(BeNil())
	ExpectWithOffset(1, leaf.GetMagicNumber()).To(Equal(int64(6773487359078157740)))
	ExpectWithOffset(1, leaf.InnerContinuation).To(Equal(append([]byte{0x15, id}, suffix...)))
}

type recordKeyTestPage struct {
	keys      []tuple.Tuple
	rowTokens [][]byte
	token     []byte
	reason    NoNextReason
	end       bool
	records   int
	bytes     int64
}

func recordKeyTestMetadata(split, versions bool) *RecordMetaData {
	builder := NewRecordMetaDataBuilder().SetRecords(demopb.File_record_layer_demo_proto)
	builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
	builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
	builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
	builder.SetSplitLongRecords(split)
	builder.SetStoreRecordVersions(versions)
	md, err := builder.Build()
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	return md
}

func recordKeyTestOpen(rtx *FDBRecordContext, ss subspace.Subspace, md *RecordMetaData, format int32) *FDBRecordStore {
	store, err := NewStoreBuilder().SetContext(rtx).SetSubspace(ss).SetMetaDataProvider(md).
		SetFormatVersion(format).SetSkipPossiblyRebuild(true).CreateOrOpen()
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	return store
}

func recordKeyTestRead(ss subspace.Subspace, md *RecordMetaData, format int32, props ScanProperties, token []byte) recordKeyTestPage {
	transaction, err := sharedDB.CreateTransaction()
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	defer transaction.Cancel()
	store := recordKeyTestOpen(sharedDB.NewRecordContext(transaction), ss, md, format)
	state := NewScanLimiterState()
	props.ExecuteProperties.ScanState = state
	cursor := store.ScanRecordKeys(token, props)
	defer func() { Expect(cursor.Close()).To(Succeed()) }()
	page := recordKeyTestPage{keys: []tuple.Tuple{}, rowTokens: [][]byte{}}
	for {
		next, err := cursor.OnNext(context.Background())
		ExpectWithOffset(1, err).NotTo(HaveOccurred())
		raw, err := next.GetContinuation().ToBytes()
		ExpectWithOffset(1, err).NotTo(HaveOccurred())
		if next.HasNext() {
			page.keys = append(page.keys, next.GetValue())
			page.rowTokens = append(page.rowTokens, raw)
			continue
		}
		page.token, page.reason, page.end = raw, next.GetNoNextReason(), next.GetContinuation().IsEnd()
		page.records, page.bytes = state.RecordsScanned(), state.BytesScanned()
		for range 2 {
			repeated, err := cursor.OnNext(context.Background())
			ExpectWithOffset(1, err).NotTo(HaveOccurred())
			repeatedBytes, err := repeated.GetContinuation().ToBytes()
			ExpectWithOffset(1, err).NotTo(HaveOccurred())
			ExpectWithOffset(1, repeated.HasNext()).To(BeFalse())
			ExpectWithOffset(1, repeated.GetNoNextReason()).To(Equal(page.reason))
			ExpectWithOffset(1, repeated.GetContinuation().IsEnd()).To(Equal(page.end))
			ExpectWithOffset(1, repeatedBytes).To(Equal(page.token))
			ExpectWithOffset(1, state.RecordsScanned()).To(Equal(page.records))
			ExpectWithOffset(1, state.BytesScanned()).To(Equal(page.bytes))
		}
		return page
	}
}

// Observe pulls from the real FDB iterator, independently of limiter counters.
// Range prefetch is not counted as a cursor Advance or Get.
type recordKeyObservedTransaction struct {
	fdb.WritableTransaction
	advances int
	gets     int
}

func (tx *recordKeyObservedTransaction) GetRange(r fdb.Range, options fdb.RangeOptions) fdb.RangeResult {
	return recordKeyObservedRange{RangeResult: tx.WritableTransaction.GetRange(r, options), tx: tx}
}

type recordKeyObservedRange struct {
	fdb.RangeResult
	tx *recordKeyObservedTransaction
}

func (r recordKeyObservedRange) Iterator() fdb.RangeIterator {
	return recordKeyObservedIterator{RangeIterator: r.RangeResult.Iterator(), tx: r.tx}
}

type recordKeyObservedIterator struct {
	fdb.RangeIterator
	tx *recordKeyObservedTransaction
}

func (i recordKeyObservedIterator) Advance() bool {
	i.tx.advances++
	return i.RangeIterator.Advance()
}

func (i recordKeyObservedIterator) Get() (fdb.KeyValue, error) {
	i.tx.gets++
	return i.RangeIterator.Get()
}

func recordKeyObservedOpen(ss subspace.Subspace, md *RecordMetaData, env *dst.Env) (*FDBRecordStore, *recordKeyObservedTransaction) {
	tx, err := sharedDB.CreateTransaction()
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	observed := &recordKeyObservedTransaction{WritableTransaction: tx}
	rc := NewFDBRecordContext(observed, env)
	DeferCleanup(rc.Cancel)
	store := recordKeyTestOpen(rc, ss, md, 14)
	observed.advances, observed.gets = 0, 0
	return store, observed
}

func recordKeySeedRaw(ss subspace.Subspace, md *RecordMetaData, keys [][]byte) {
	_, err := sharedDB.Run(context.Background(), func(rc *FDBRecordContext) (any, error) {
		recordKeyTestOpen(rc, ss, md, 14)
		for _, suffix := range keys {
			key := append(append([]byte(nil), ss.Sub(RecordKey).Bytes()...), suffix...)
			rc.Transaction().Set(fdb.Key(key), []byte{0x61})
		}
		return nil, nil
	})
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
}

var _ = Describe("Record-key continuation framing", func() {
	for _, layout := range []struct {
		name            string
		split, versions bool
		format          int32
	}{
		{"modern unsplit", false, false, 14},
		{"modern split", true, false, 14},
		{"modern inline unsplit", false, true, 14},
		{"modern inline split", true, true, 14},
		{"legacy-format split-capable", true, false, 4},
	} {
		It("exchanges physical positions and PK state for "+layout.name, func() {
			ctx := context.Background()
			ss := specSubspace()
			md := recordKeyTestMetadata(layout.split, layout.versions)
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store := recordKeyTestOpen(rtx, ss, md, layout.format)
				for _, id := range []int64{1, 2, 3} {
					payload := []byte{byte(id)}
					if layout.split && id == 2 {
						payload = bytes.Repeat([]byte{0x61}, 250_000)
					}
					_, err := store.SaveRecord(&demopb.Order{OrderId: proto.Int64(id), VectorData: payload})
					Expect(err).NotTo(HaveOccurred())
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
			// A committed physical census prevents split/version cases from becoming vacuous.
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				kvs, err := rtx.Transaction().GetRange(ss.Sub(RecordKey), fdb.RangeOptions{}).GetSliceWithError()
				Expect(err).NotTo(HaveOccurred())
				var physical [][]byte
				for _, kv := range kvs {
					physical = append(physical, []byte(kv.Key[len(ss.Sub(RecordKey).Bytes()):]))
				}
				var want [][]byte
				for _, id := range []byte{1, 2, 3} {
					if layout.versions {
						want = append(want, []byte{0x15, id, 0x13, 0xfe})
					}
					if layout.split && id == 2 {
						want = append(want, []byte{0x15, 2, 0x15, 1}, []byte{0x15, 2, 0x15, 2}, []byte{0x15, 2, 0x15, 3})
					} else {
						want = append(want, []byte{0x15, id, 0x14})
					}
				}
				Expect(physical).To(Equal(want))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			for _, reverse := range []bool{false, true} {
				props := ForwardScan().WithReverse(reverse)
				props.ExecuteProperties.ReturnedRowLimit = 2
				page := recordKeyTestRead(ss, md, layout.format, props, nil)
				wantFirst, wantRest := []tuple.Tuple{{int64(1)}, {int64(2)}}, []tuple.Tuple{{int64(3)}}
				if reverse {
					wantFirst, wantRest = []tuple.Tuple{{int64(3)}, {int64(2)}}, []tuple.Tuple{{int64(1)}}
				}
				Expect(page.keys).To(Equal(wantFirst))
				physicalReads := 2
				if layout.versions {
					physicalReads++ // the first PK's data/version duplicate
				}
				Expect(page.records).To(Equal(physicalReads)) // no limit-boundary physical pull
				Expect(page.reason).To(Equal(ReturnLimitReached))
				Expect(page.end).To(BeFalse())
				Expect(page.token).To(Equal(page.rowTokens[1]))
				suffix := []byte{0x14}
				if reverse && layout.split {
					suffix = []byte{0x15, 3}
				} else if !reverse && layout.versions {
					suffix = []byte{0x13, 0xfe}
				} else if !reverse && layout.split {
					suffix = []byte{0x15, 1}
				}
				unlimited := ForwardScan().WithReverse(reverse)
				for _, token := range [][]byte{page.rowTokens[1], page.token} {
					rest := recordKeyTestRead(ss, md, layout.format, unlimited, token)
					Expect(rest.keys).To(Equal(wantRest))
					Expect(rest.reason).To(Equal(SourceExhausted))
					Expect(rest.end).To(BeTrue())
					Expect(rest.token).To(BeNil())
				}

				recordKeyAssertToken(page.token, 2, suffix)

				// The logical limit cannot infer EOF, even at exactly the population.
				props.ExecuteProperties.ReturnedRowLimit = 3
				exact := recordKeyTestRead(ss, md, layout.format, props, nil)
				Expect(exact.keys).To(HaveLen(3))
				Expect(exact.reason).To(Equal(ReturnLimitReached))
				Expect(exact.end).To(BeFalse())
				end := recordKeyTestRead(ss, md, layout.format, unlimited, exact.token)
				Expect(end.keys).To(BeEmpty())
				Expect(end.reason).To(Equal(SourceExhausted))
				Expect(end.end).To(BeTrue())
				Expect(end.token).To(BeNil())

				if !layout.split && !layout.versions {
					continue
				}
				var duplicateSuffixes [][]byte
				if reverse {
					if layout.split {
						duplicateSuffixes = append(duplicateSuffixes, []byte{0x15, 2}, []byte{0x15, 1})
					}
					if layout.versions {
						duplicateSuffixes = append(duplicateSuffixes, []byte{0x13, 0xfe})
					}
				} else if layout.split {
					if layout.versions {
						duplicateSuffixes = append(duplicateSuffixes, []byte{0x15, 1})
					}
					duplicateSuffixes = append(duplicateSuffixes, []byte{0x15, 2}, []byte{0x15, 3})
				} else {
					duplicateSuffixes = append(duplicateSuffixes, []byte{0x14})
				}
				for _, budget := range []string{"records", "bytes"} {
					token := page.token
					for _, expectedSuffix := range duplicateSuffixes {
						limited := unlimited
						wantReason := ScanLimitReached
						if budget == "records" {
							limited.ExecuteProperties.ScannedRecordsLimit = 1
						} else {
							limited.ExecuteProperties.ScannedBytesLimit = 1
							wantReason = ByteLimitReached
						}
						duplicate := recordKeyTestRead(ss, md, layout.format, limited, token)
						Expect(duplicate.keys).To(BeEmpty())
						Expect(duplicate.reason).To(Equal(wantReason))
						Expect(duplicate.end).To(BeFalse())
						Expect(duplicate.records).To(Equal(2)) // accepted physical KV plus rejected attempt
						Expect(duplicate.bytes).To(BeNumerically(">", 0))
						Expect(duplicate.token).NotTo(Equal(token))
						recordKeyAssertToken(duplicate.token, 2, expectedSuffix)
						token = duplicate.token
					}
					rest := recordKeyTestRead(ss, md, layout.format, unlimited, token)
					Expect(rest.keys).To(Equal(wantRest))
				}
			}
		})
	}

	It("stops at the exact returned-row boundary before probing physical EOF", func() {
		ss := specSubspace()
		md := recordKeyTestMetadata(false, false)
		_, err := sharedDB.Run(context.Background(), func(rtx *FDBRecordContext) (any, error) {
			store := recordKeyTestOpen(rtx, ss, md, 14)
			for _, id := range []int64{1, 2, 3} {
				_, err := store.SaveRecord(&demopb.Order{OrderId: proto.Int64(id)})
				Expect(err).NotTo(HaveOccurred())
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
		for _, reverse := range []bool{false, true} {
			props := ForwardScan().WithReverse(reverse)
			props.ExecuteProperties.ReturnedRowLimit = 3
			page := recordKeyTestRead(ss, md, 14, props, nil)
			want, last := []tuple.Tuple{{int64(1)}, {int64(2)}, {int64(3)}}, byte(3)
			if reverse {
				want, last = []tuple.Tuple{{int64(3)}, {int64(2)}, {int64(1)}}, 1
			}
			Expect(page.keys).To(Equal(want))
			// At exact population, a peek would incorrectly replace this limit with EOF.
			Expect(page.reason).To(Equal(ReturnLimitReached))
			Expect(page.end).To(BeFalse())
			recordKeyAssertToken(page.token, last, []byte{0x14})
			Expect(page.records).To(Equal(3))
			end := recordKeyTestRead(ss, md, 14, ForwardScan().WithReverse(reverse), page.token)
			Expect(end.keys).To(BeEmpty())
			Expect(end.reason).To(Equal(SourceExhausted))
			Expect(end.end).To(BeTrue())
			Expect(end.token).To(BeNil())
		}
	})

	It("keeps physical selector skip and streaming settings on resumed split scans", func() {
		ss := specSubspace()
		md := recordKeyTestMetadata(true, false)
		_, err := sharedDB.Run(context.Background(), func(rtx *FDBRecordContext) (any, error) {
			store := recordKeyTestOpen(rtx, ss, md, 14)
			for _, id := range []int64{1, 2, 3} {
				payload := []byte{0x61}
				if id == 2 {
					payload = bytes.Repeat(payload, 250_000)
				}
				_, err := store.SaveRecord(&demopb.Order{OrderId: proto.Int64(id), VectorData: payload})
				Expect(err).NotTo(HaveOccurred())
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
		for _, mode := range []CursorStreamingMode{StreamingModeSmall, StreamingModeWantAll} {
			for _, reverse := range []bool{false, true} {
				props := ForwardScan().WithReverse(reverse).WithStreamingMode(mode)
				props.ExecuteProperties.ReturnedRowLimit = 1
				props.ExecuteProperties.Skip = 2 // skip one whole record and one chunk, not two PKs
				page := recordKeyTestRead(ss, md, 14, props, nil)
				Expect(page.keys).To(Equal([]tuple.Tuple{{int64(2)}}))
				Expect(page.records).To(Equal(1))
				recordKeyAssertToken(page.token, 2, []byte{0x15, 2})
				props.ExecuteProperties.Skip = 1 // applied again after the resumed bound
				props.ExecuteProperties.ReturnedRowLimit = 0
				rest := recordKeyTestRead(ss, md, 14, props, page.token)
				want := int64(3)
				if reverse {
					want = 1
				}
				Expect(rest.keys).To(Equal([]tuple.Tuple{{want}}))
				Expect(rest.reason).To(Equal(SourceExhausted))
			}
		}
	})

	It("returns stable EOF for an empty suffixed store", func() {
		ss := specSubspace()
		md := recordKeyTestMetadata(true, true)
		_, err := sharedDB.Run(context.Background(), func(rtx *FDBRecordContext) (any, error) {
			recordKeyTestOpen(rtx, ss, md, 14)
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
		for _, reverse := range []bool{false, true} {
			for _, token := range [][]byte{nil, {0x0a, 0x00}} { // required inner field present but empty is legal
				page := recordKeyTestRead(ss, md, 14, ForwardScan().WithReverse(reverse), token)
				Expect(page.keys).To(BeEmpty())
				Expect(page.reason).To(Equal(SourceExhausted))
				Expect(page.end).To(BeTrue())
				Expect(page.token).To(BeNil())
			}
		}
	})
})

var _ = Describe("Record-key public factory limit and corruption contracts", func() {
	for _, budget := range []string{"records", "bytes"} {
		for _, reverse := range []bool{false, true} {
			It(fmt.Sprintf("propagates fail-on-%s during duplicate-only progress, reverse=%t", budget, reverse), func() {
				ss := specSubspace()
				md := recordKeyTestMetadata(true, false)
				recordKeySeedRaw(ss, md, [][]byte{
					{0x15, 1, 0x14}, {0x15, 2, 0x15, 1}, {0x15, 2, 0x15, 2}, {0x15, 2, 0x15, 3}, {0x15, 3, 0x14},
				})
				startSuffix := []byte{0x15, 1}
				if reverse {
					startSuffix = []byte{0x15, 3}
				}
				for _, fail := range []bool{false, true} {
					store, pulls := recordKeyObservedOpen(ss, md, nil)
					props := ForwardScan().WithReverse(reverse)
					props.ExecuteProperties.FailOnScanLimitReached = fail
					wantReason := ScanLimitReached
					if budget == "records" {
						props.ExecuteProperties.ScannedRecordsLimit = 1
					} else {
						props.ExecuteProperties.ScannedBytesLimit = 1
						wantReason = ByteLimitReached
					}
					state := props.ExecuteProperties.ScanState
					cursor := store.ScanRecordKeys(recordKeyLiteralToken(2, startSuffix), props)
					DeferCleanup(func() { Expect(cursor.Close()).To(Succeed()) })
					next, err := cursor.OnNext(context.Background())
					if fail {
						var limit *ScanLimitReachedError
						Expect(errors.As(err, &limit)).To(BeTrue(), "requested fail mode returned %v, result=%+v", err, next)
						Expect(limit.Reason).To(Equal(wantReason))
					} else {
						Expect(err).NotTo(HaveOccurred())
						Expect(next.HasNext()).To(BeFalse())
						Expect(next.GetNoNextReason()).To(Equal(wantReason))
						Expect(next.GetContinuation().IsEnd()).To(BeFalse())
						raw, err := next.GetContinuation().ToBytes()
						Expect(err).NotTo(HaveOccurred())
						recordKeyAssertToken(raw, 2, []byte{0x15, 2})
					}
					Expect(state.RecordsScanned()).To(Equal(2)) // accepted duplicate plus rejected attempt
					Expect(state.BytesScanned()).To(Equal(int64(len(ss.Sub(RecordKey).Bytes()) + 4 + 1)))
					Expect(pulls.advances).To(Equal(1))
					Expect(pulls.gets).To(Equal(1))
				}
			})
		}
	}

	for _, reverse := range []bool{false, true} {
		It(fmt.Sprintf("rejects an already-spent shared record budget on the first pull, reverse=%t", reverse), func() {
			ss := specSubspace()
			md := recordKeyTestMetadata(true, false)
			recordKeySeedRaw(ss, md, [][]byte{
				{0x15, 2, 0x15, 1}, {0x15, 2, 0x15, 2}, {0x15, 2, 0x15, 3},
			})
			startSuffix := []byte{0x15, 1}
			if reverse {
				startSuffix = []byte{0x15, 3}
			}
			for _, spent := range []bool{false, true} {
				store, pulls := recordKeyObservedOpen(ss, md, nil)
				props := ForwardScan().WithReverse(reverse)
				props.ExecuteProperties.ScannedRecordsLimit = 1
				props.ExecuteProperties.FailOnScanLimitReached = true
				state := props.ExecuteProperties.ScanState
				if spent {
					state.AddRecordScanned()
				}
				cursor := store.ScanRecordKeys(recordKeyLiteralToken(2, startSuffix), props)
				DeferCleanup(func() { Expect(cursor.Close()).To(Succeed()) })
				next, err := cursor.OnNext(context.Background())
				var limit *ScanLimitReachedError
				Expect(errors.As(err, &limit)).To(BeTrue(), "spent=%t err=%v result=%+v", spent, err, next)
				Expect(limit.Reason).To(Equal(ScanLimitReached))
				Expect(state.RecordsScanned()).To(Equal(2))
				if spent {
					Expect(pulls.advances).To(BeZero())
					Expect(pulls.gets).To(BeZero())
					Expect(state.BytesScanned()).To(BeZero())
				} else {
					Expect(pulls.advances).To(Equal(1))
					Expect(pulls.gets).To(Equal(1))
					Expect(state.BytesScanned()).To(Equal(int64(len(ss.Sub(RecordKey).Bytes()) + 4 + 1)))
				}
			}
		})
	}

	for _, corruption := range []struct {
		name string
		key  []byte
	}{
		{"malformed tuple", []byte{0x15, 2, 0xff}},
		{"string suffix", []byte{0x15, 2, 0x02, 'b', 'a', 'd', 0x00}},
		{"minus-two suffix", []byte{0x15, 2, 0x13, 0xfd}},
	} {
		It("rejects a committed physical "+corruption.name, func() {
			ss := specSubspace()
			md := recordKeyTestMetadata(true, false)
			recordKeySeedRaw(ss, md, [][]byte{{0x15, 1, 0x14}, corruption.key, {0x15, 3, 0x14}})
			for _, reverse := range []bool{false, true} {
				store, pulls := recordKeyObservedOpen(ss, md, nil)
				persisted := append(append([]byte(nil), ss.Sub(RecordKey).Bytes()...), corruption.key...)
				value, err := store.context.Transaction().Get(fdb.Key(persisted)).Get()
				Expect(err).NotTo(HaveOccurred())
				Expect(value).To(Equal([]byte{0x61})) // fresh transaction observes the raw committed corruption
				props := ForwardScan().WithReverse(reverse)
				state := props.ExecuteProperties.ScanState
				cursor := store.ScanRecordKeys(nil, props)
				DeferCleanup(func() { Expect(cursor.Close()).To(Succeed()) })
				first, err := cursor.OnNext(context.Background())
				Expect(err).NotTo(HaveOccurred())
				Expect(first.HasNext()).To(BeTrue())
				firstID := byte(1)
				if reverse {
					firstID = 3
				}
				Expect(first.GetValue()).To(Equal(tuple.Tuple{int64(firstID)}))
				firstToken, err := first.GetContinuation().ToBytes()
				Expect(err).NotTo(HaveOccurred())
				recordKeyAssertToken(firstToken, firstID, []byte{0x14})
				next, err := cursor.OnNext(context.Background())
				Expect(err).To(HaveOccurred(), "corrupt key was skipped or accepted: result=%+v", next)
				var storage *RecordCoreStorageError
				if corruption.name == "malformed tuple" {
					Expect(errors.As(err, &storage)).To(BeFalse())
					var argument *IllegalArgumentError
					Expect(errors.As(err, &argument)).To(BeTrue())
					Expect(argument.Message).To(Equal("unknown tuple typecode 0xff at 2"))
					var coreArgument *RecordCoreArgumentError
					Expect(errors.As(err, &coreArgument)).To(BeFalse())
				} else {
					Expect(errors.As(err, &storage)).To(BeTrue(), "want suffix storage error, got %T: %v", err, err)
					Expect(storage.Message).To(Equal("Invalid record split number"))
					wantKey := tuple.Tuple{int64(2), "bad"}
					if corruption.name == "minus-two suffix" {
						wantKey = tuple.Tuple{int64(2), int64(-2)}
					}
					Expect(storage.KeyTuple).To(Equal(wantKey))
				}
				Expect(state.RecordsScanned()).To(Equal(2))
				Expect(state.BytesScanned()).To(Equal(int64(2*len(ss.Sub(RecordKey).Bytes()) + 3 + len(corruption.key) + 2)))
				Expect(pulls.advances).To(Equal(2))
				Expect(pulls.gets).To(Equal(2))
			}
		})
	}

	It("accepts the numeric physical suffix controls minus-one, zero and one", func() {
		ss := specSubspace()
		md := recordKeyTestMetadata(true, false)
		recordKeySeedRaw(ss, md, [][]byte{{0x15, 1, 0x13, 0xfe}, {0x15, 2, 0x14}, {0x15, 3, 0x15, 1}})
		for _, reverse := range []bool{false, true} {
			page := recordKeyTestRead(ss, md, 14, ForwardScan().WithReverse(reverse), nil)
			want := []tuple.Tuple{{int64(1)}, {int64(2)}, {int64(3)}}
			ids := []byte{1, 2, 3}
			suffixes := [][]byte{{0x13, 0xfe}, {0x14}, {0x15, 1}}
			if reverse {
				want = []tuple.Tuple{{int64(3)}, {int64(2)}, {int64(1)}}
				ids, suffixes = []byte{3, 2, 1}, [][]byte{{0x15, 1}, {0x14}, {0x13, 0xfe}}
			}
			Expect(page.keys).To(Equal(want))
			Expect(page.reason).To(Equal(SourceExhausted))
			Expect(page.end).To(BeTrue())
			Expect(page.token).To(BeNil())
			Expect(page.records).To(Equal(4)) // three physical keys plus the EOF attempt
			for i, id := range ids {
				recordKeyAssertToken(page.rowTokens[i], id, suffixes[i])
			}
		}
	})

	It("retains bare-key unpack error class, cause and physical context", func() {
		ss := specSubspace()
		md := recordKeyTestMetadata(false, false)
		badKey := append(append([]byte(nil), ss.Sub(RecordKey).Bytes()...), 0x15, 2, 0xff)
		_, err := sharedDB.Run(context.Background(), func(rc *FDBRecordContext) (any, error) {
			recordKeyTestOpen(rc, ss, md, 4)
			rc.Transaction().Set(fdb.Key(badKey), []byte{0x61})
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
		for _, reverse := range []bool{false, true} {
			tx, err := sharedDB.CreateTransaction()
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(tx.Cancel)
			store := recordKeyTestOpen(sharedDB.NewRecordContext(tx), ss, md, 4)
			value, err := tx.Get(fdb.Key(badKey)).Get()
			Expect(err).NotTo(HaveOccurred())
			Expect(value).To(Equal([]byte{0x61}))
			cursor := store.ScanRecordKeys(nil, ForwardScan().WithReverse(reverse))
			DeferCleanup(func() { Expect(cursor.Close()).To(Succeed()) })
			_, err = cursor.OnNext(context.Background())
			var argument *RecordCoreArgumentError
			Expect(errors.As(err, &argument)).To(BeTrue(), "got %T: %v", err, err)
			Expect(argument.Message).To(Equal("unable to unpack key"))
			Expect(argument.Key).To(Equal(badKey))
			Expect(argument.Subspace).To(Equal(ss.Sub(RecordKey).Bytes()))
			var cause *IllegalArgumentError
			Expect(errors.As(errors.Unwrap(argument), &cause)).To(BeTrue())
			Expect(cause.Message).To(Equal("unknown tuple typecode 0xff at 2"))
			Expect(argument.Error()).To(ContainSubstring(fmt.Sprintf("key=%x", badKey)))
			Expect(argument.Error()).To(ContainSubstring(fmt.Sprintf("subspace=%x", ss.Sub(RecordKey).Bytes())))
		}
	})

	// Literal tuple bytes exercise Tuple.getLong's Number.longValue conversion
	// through committed physical keys, independently of Go's tuple encoder.
	for _, numeric := range []struct {
		name   string
		suffix []byte
		valid  bool
	}{
		{"float 1.5", []byte{0x20, 0xbf, 0xc0, 0, 0}, true},
		{"double -1.5", []byte{0x21, 0x40, 0x07, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}, true},
		{"double NaN", []byte{0x21, 0xff, 0xf8, 0, 0, 0, 0, 0, 0}, true},
		{"double positive infinity", []byte{0x21, 0xff, 0xf0, 0, 0, 0, 0, 0, 0}, true},
		{"unsigned 64-bit minus-one", []byte{0x1c, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}, true},
		{"big integer low-bits one", []byte{0x1d, 9, 1, 0, 0, 0, 0, 0, 0, 0, 1}, true},
		{"float -2.5", []byte{0x20, 0x3f, 0xdf, 0xff, 0xff}, false},
		{"double negative infinity", []byte{0x21, 0, 0x0f, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}, false},
		{"unsigned 64-bit minus-two", []byte{0x1c, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xfe}, false},
		{"big integer low-bits minus-two", []byte{0x1d, 9, 1, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xfe}, false},
	} {
		It("validates the converted physical suffix for "+numeric.name, func() {
			ss := specSubspace()
			md := recordKeyTestMetadata(true, false)
			key := append([]byte{0x15, 2}, numeric.suffix...)
			recordKeySeedRaw(ss, md, [][]byte{key})
			for _, reverse := range []bool{false, true} {
				store, pulls := recordKeyObservedOpen(ss, md, nil)
				persisted := append(append([]byte(nil), ss.Sub(RecordKey).Bytes()...), key...)
				value, err := store.context.Transaction().Get(fdb.Key(persisted)).Get()
				Expect(err).NotTo(HaveOccurred())
				Expect(value).To(Equal([]byte{0x61}))
				props := ForwardScan().WithReverse(reverse)
				props.ExecuteProperties.ReturnedRowLimit = 1
				cursor := store.ScanRecordKeys(nil, props)
				DeferCleanup(func() { Expect(cursor.Close()).To(Succeed()) })
				next, err := cursor.OnNext(context.Background())
				if numeric.valid {
					Expect(err).NotTo(HaveOccurred())
					Expect(next.HasNext()).To(BeTrue())
					Expect(next.GetValue()).To(Equal(tuple.Tuple{int64(2)}))
					raw, err := next.GetContinuation().ToBytes()
					Expect(err).NotTo(HaveOccurred())
					recordKeyAssertToken(raw, 2, numeric.suffix)
					last, err := cursor.OnNext(context.Background())
					Expect(err).NotTo(HaveOccurred())
					Expect(last.HasNext()).To(BeFalse())
					Expect(last.GetNoNextReason()).To(Equal(ReturnLimitReached))
					Expect(last.GetContinuation().IsEnd()).To(BeFalse())
					resume := recordKeyTestRead(ss, md, 14, ForwardScan().WithReverse(reverse), raw)
					Expect(resume.keys).To(BeEmpty())
					Expect(resume.reason).To(Equal(SourceExhausted))
					Expect(resume.end).To(BeTrue())
				} else {
					var storage *RecordCoreStorageError
					Expect(errors.As(err, &storage)).To(BeTrue(), "got %T: %v", err, err)
					Expect(storage.Message).To(Equal("Invalid record split number"))
					Expect(storage.KeyTuple[0]).To(Equal(int64(2)))
					Expect(storage.KeyTuple.Pack()).To(Equal(key))
				}
				Expect(pulls.advances).To(Equal(1))
				Expect(pulls.gets).To(Equal(1))
			}
		})
	}

	for _, fail := range []bool{false, true} {
		It(fmt.Sprintf("anchors fresh key-scan properties at the aged transaction clock, fail=%t", fail), func() {
			ss := specSubspace()
			md := recordKeyTestMetadata(false, false)
			recordKeySeedRaw(ss, md, [][]byte{{0x15, 1, 0x14}, {0x15, 2, 0x14}, {0x15, 3, 0x14}})
			for _, reverse := range []bool{false, true} {
				clock := dst.NewSimClock(dst.Epoch)
				env := &dst.Env{Clock: clock, Random: dst.NewSeededRandomness(151)}
				store, pulls := recordKeyObservedOpen(ss, md, env)
				clock.Advance(time.Second)
				props := NewScanProperties(DefaultExecutePropertiesIn(env).WithTimeLimit(100 * time.Millisecond)).WithReverse(reverse)
				props.ExecuteProperties.FailOnScanLimitReached = fail
				state := props.ExecuteProperties.ScanState
				Expect(env.Now().Sub(store.context.createTime)).To(Equal(time.Second))
				Expect(state.Elapsed()).To(BeZero()) // properties were created after the transaction aged
				cursor := store.ScanRecordKeys(nil, props)
				DeferCleanup(func() { Expect(cursor.Close()).To(Succeed()) })
				first, err := cursor.OnNext(context.Background())
				Expect(err).NotTo(HaveOccurred())
				Expect(first.HasNext()).To(BeTrue()) // the initial time-limit free pass is unconditional
				id := byte(1)
				if reverse {
					id = 3
				}
				Expect(first.GetValue()).To(Equal(tuple.Tuple{int64(id)}))
				token, err := first.GetContinuation().ToBytes()
				Expect(err).NotTo(HaveOccurred())
				recordKeyAssertToken(token, id, []byte{0x14})
				last, err := cursor.OnNext(context.Background())
				if fail {
					var limit *ScanLimitReachedError
					Expect(errors.As(err, &limit)).To(BeTrue(), "want transaction-aged time error, got %v result=%+v", err, last)
					Expect(limit.Reason).To(Equal(TimeLimitReached))
				} else {
					Expect(err).NotTo(HaveOccurred())
					Expect(last.HasNext()).To(BeFalse())
					Expect(last.GetNoNextReason()).To(Equal(TimeLimitReached))
					Expect(last.GetContinuation().IsEnd()).To(BeFalse())
					stopped, err := last.GetContinuation().ToBytes()
					Expect(err).NotTo(HaveOccurred())
					Expect(stopped).To(Equal(token))
					for range 2 {
						repeated, err := cursor.OnNext(context.Background())
						Expect(err).NotTo(HaveOccurred())
						Expect(repeated.HasNext()).To(BeFalse())
						Expect(repeated.GetNoNextReason()).To(Equal(TimeLimitReached))
						raw, err := repeated.GetContinuation().ToBytes()
						Expect(err).NotTo(HaveOccurred())
						Expect(raw).To(Equal(token))
					}
				}
				Expect(state.RecordsScanned()).To(Equal(2))
				Expect(state.BytesScanned()).To(Equal(int64(len(ss.Sub(RecordKey).Bytes()) + 3 + 1)))
				Expect(pulls.advances).To(Equal(1))
				Expect(pulls.gets).To(Equal(1))
				Expect(state.Elapsed()).To(BeZero())

				// A fresh context at the same frozen time has an unspent time budget.
				resumedStore, resumedPulls := recordKeyObservedOpen(ss, md, env)
				fresh := NewScanProperties(DefaultExecutePropertiesIn(env).WithTimeLimit(100 * time.Millisecond)).WithReverse(reverse)
				fresh.ExecuteProperties.FailOnScanLimitReached = fail
				resumed := resumedStore.ScanRecordKeys(token, fresh)
				DeferCleanup(func() { Expect(resumed.Close()).To(Succeed()) })
				keys, err := AsList(context.Background(), resumed)
				Expect(err).NotTo(HaveOccurred())
				want := []tuple.Tuple{{int64(2)}, {int64(3)}}
				if reverse {
					want = []tuple.Tuple{{int64(2)}, {int64(1)}}
				}
				Expect(keys).To(Equal(want))
				Expect(fresh.ExecuteProperties.ScanState.RecordsScanned()).To(Equal(3))
				Expect(resumedPulls.advances).To(Equal(3))
				Expect(resumedPulls.gets).To(Equal(3))
			}
		})
	}
})

func TestRecordKeyContinuationInvalid(t *testing.T) {
	t.Parallel()
	builder := NewRecordMetaDataBuilder().SetRecords(demopb.File_record_layer_demo_proto)
	builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
	builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
	builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
	md, err := builder.Build()
	if err != nil {
		t.Fatal(err)
	}
	store := &FDBRecordStore{metaData: md}
	for _, tc := range []struct {
		name  string
		token []byte
		parse bool
	}{
		{"empty", []byte{}, true},
		{"malformed", []byte{0xff}, true},
		{"missing inner", []byte{0x12, 0x02, 0x15, 0x02}, true},
		{"invalid packed lastValue", []byte{0x0a, 0x00, 0x12, 0x01, 0xff}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cursor := store.ScanRecordKeys(tc.token, ForwardScan())
			_, err := cursor.OnNext(context.Background())
			if err == nil {
				t.Fatal("invalid continuation silently restarted")
			}
			var parse *ContinuationParseError
			if tc.parse && !errors.As(err, &parse) {
				t.Fatalf("want ContinuationParseError, got %T: %v", err, err)
			}
			_, repeated := cursor.OnNext(context.Background())
			if repeated != err {
				t.Fatalf("invalid continuation error changed: %v -> %v", err, repeated)
			}
		})
	}
}

// A duplicate-only time stop is deterministic: the iterator advances the clock
// anchor after one physical KV, rather than depending on scheduler timing.
type recordKeyClockIterator struct {
	kv    fdb.KeyValue
	clock *ScanLimiterState
	reads int
}

func (i *recordKeyClockIterator) Advance() bool {
	i.reads++
	i.clock.AnchorAt(time.Now().Add(-time.Hour))
	return true
}
func (i *recordKeyClockIterator) Get() (fdb.KeyValue, error) { return i.kv, nil }

func TestRecordKeyContinuationDuplicateTimeStop(t *testing.T) {
	t.Parallel()
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprintf("fail=%t", fail), func(t *testing.T) {
			t.Parallel()
			state := NewScanLimiterState()
			iterator := &recordKeyClockIterator{kv: fdb.KeyValue{Key: fdb.Key{0x15, 2, 0x15, 2}, Value: []byte{0x61}}, clock: state}
			physical := &recordKeyCursor{
				iterator: iterator, scanState: state, clock: state,
				scanProperties: NewScanProperties(ExecuteProperties{TimeLimit: time.Millisecond, FailOnScanLimitReached: fail}),
			}
			cursor := Dedup(func([]byte) RecordCursor[tuple.Tuple] { return physical }, tuplesEqual,
				func(pk tuple.Tuple) []byte { return pk.Pack() }, func(raw []byte) (tuple.Tuple, bool) {
					pk, err := tuple.Unpack(raw)
					return pk, err == nil
				}, recordKeyLiteralToken(2, []byte{0x15, 1}))
			result, err := cursor.OnNext(context.Background())
			if fail {
				var limit *ScanLimitReachedError
				if !errors.As(err, &limit) || limit.Reason != TimeLimitReached {
					t.Fatalf("want time scan-limit error, got %v", err)
				}
			} else {
				if err != nil || result.HasNext() || result.GetNoNextReason() != TimeLimitReached {
					t.Fatalf("want duplicate-only time stop, result=%v err=%v", result, err)
				}
				raw, err := result.GetContinuation().ToBytes()
				if err != nil || !bytes.Equal(raw, recordKeyLiteralToken(2, []byte{0x15, 2})) {
					t.Fatalf("time stop token=%x err=%v", raw, err)
				}
				_, err = cursor.OnNext(context.Background())
				if err != nil || state.RecordsScanned() != 2 {
					t.Fatalf("terminal re-read changed state: records=%d err=%v", state.RecordsScanned(), err)
				}
			}
			if iterator.reads != 1 {
				t.Fatalf("duplicate time stop pulled %d physical keys, want 1", iterator.reads)
			}
		})
	}
}
