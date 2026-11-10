package recordlayer

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"math"
	"slices"
	"strings"
	"time"

	"fdb.dev/gen/demopb"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"
)

type partitionNativeReadCounter struct {
	fdb.WritableTransaction
	reads int
}

func (c *partitionNativeReadCounter) Get(k fdb.KeyConvertible) fdb.FutureByteSlice {
	c.reads++
	return c.WritableTransaction.Get(k)
}

func (c *partitionNativeReadCounter) GetRange(r fdb.Range, o fdb.RangeOptions) fdb.RangeResult {
	c.reads++
	return c.WritableTransaction.GetRange(r, o)
}

func (c *partitionNativeReadCounter) Snapshot() fdb.ReadTransaction {
	return partitionNativeSnapshot{ReadTransaction: c.WritableTransaction.Snapshot(), counter: c}
}

type partitionNativeSnapshot struct {
	fdb.ReadTransaction
	counter *partitionNativeReadCounter
}

func (s partitionNativeSnapshot) Get(k fdb.KeyConvertible) fdb.FutureByteSlice {
	s.counter.reads++
	return s.ReadTransaction.Get(k)
}

func (s partitionNativeSnapshot) GetRange(r fdb.Range, o fdb.RangeOptions) fdb.RangeResult {
	s.counter.reads++
	return s.ReadTransaction.GetRange(r, o)
}

func partitionIntegrationPage[T any](c RecordCursor[T]) ([]T, []byte, bool) {
	GinkgoHelper()
	var items []T
	for range 1000 {
		r, err := c.OnNext(context.Background())
		Expect(err).NotTo(HaveOccurred())
		if r.HasNext() {
			items = append(items, r.GetValue())
			continue
		}
		if r.GetNoNextReason() == SourceExhausted {
			Expect(r.GetContinuation().IsEnd()).To(BeTrue())
			return items, nil, true
		}
		Expect(r.GetNoNextReason()).To(Equal(ReturnLimitReached))
		token, err := r.GetContinuation().ToBytes()
		Expect(err).NotTo(HaveOccurred())
		Expect(token).NotTo(BeEmpty())
		return items, token, false
	}
	Fail("partition integration cursor did not progress")
	return nil, nil, false
}

func partitionIntegrationToken[T any](c RecordCursor[T]) []byte {
	GinkgoHelper()
	r, err := c.OnNext(context.Background())
	Expect(err).NotTo(HaveOccurred())
	Expect(r.HasNext()).To(BeTrue())
	token, err := r.GetContinuation().ToBytes()
	Expect(err).NotTo(HaveOccurred())
	return token
}

func partitionExpectInvalid[T any](c RecordCursor[T]) {
	GinkgoHelper()
	_, err := c.OnNext(context.Background())
	var invalid *PartitionContinuationInvalidatedError
	Expect(errors.As(err, &invalid)).To(BeTrue(), "error: %v", err)
}

var _ = Describe("Sparse partition directory and consistency", func() {
	ctx := context.Background()
	for _, format := range []int32{4, 14} {
		for _, reverse := range []bool{false, true} {
			for _, pageSize := range []int{1, 2} {
				It(fmt.Sprintf("pages native directory and every body without effects at format %d reverse=%v rows=%d", format, reverse, pageSize), func() {
					b := partitionTestBuilder().SetStoreRecordVersions(true).SetSplitLongRecords(format == 14)
					md, err := b.Build()
					Expect(err).NotTo(HaveOccurred())
					root := specSubspace()
					_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
						store := partitionOpenFormat(rc, md, root, format)
						for i, x := range []int64{-30, 0, 40, 10000000000000} {
							record := partitionOrder(int64(i+1), x, 7)
							if format == 14 && i == 2 {
								record.VectorData = []byte(strings.Repeat("s", 220000))
							}
							_, err := store.SaveRecord(record)
							Expect(err).NotTo(HaveOccurred())
						}
						_, err := store.DeleteRecord(tuple.Tuple{md.GetRecordType("Order").GetRecordTypeKey(), int64(0), int64(0), int64(2)})
						return nil, err
					})
					Expect(err).NotTo(HaveOccurred())
					var token []byte
					var got []int64
					for pages := 0; ; pages++ {
						Expect(pages).To(BeNumerically("<", 10))
						var done bool
						_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
							store := partitionOpenFormat(rc, md, root, format)
							before, effects, counter := partitionWholeStore(rc, store), partitionVersionEffectsOf(rc), rc.localVersion.Load()
							items, next, end := partitionIntegrationPage(store.ScanPartitions("Order", PartitionCoordinateRange{}, token, NewScanProperties(ExecuteProperties{ReturnedRowLimit: pageSize}).WithReverse(reverse)))
							for _, item := range items {
								got = append(got, item.Coordinate)
							}
							token, done = next, end
							Expect(partitionWholeStore(rc, store)).To(Equal(before))
							Expect(partitionVersionEffectsOf(rc)).To(Equal(effects))
							Expect(rc.localVersion.Load()).To(Equal(counter))
							return nil, nil
						})
						Expect(err).NotTo(HaveOccurred())
						if done {
							break
						}
					}
					want := []int64{-3, 0, 4, 1000000000000}
					if reverse {
						slices.Reverse(want)
					}
					Expect(got).To(Equal(want))
					var bodyCount, directoryCount, versionCount int
					token = nil
					for pages := 0; ; pages++ {
						Expect(pages).To(BeNumerically("<", 50))
						var done bool
						_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
							store := partitionOpenFormat(rc, md, root, format)
							before, effects, counter := partitionWholeStore(rc, store), partitionVersionEffectsOf(rc), rc.localVersion.Load()
							items, next, end := partitionIntegrationPage(store.CheckPartitionConsistency("Order", PartitionCoordinateRange{}, token, NewScanProperties(ExecuteProperties{ReturnedRowLimit: pageSize}).WithReverse(reverse)))
							for _, item := range items {
								switch item.Kind {
								case PartitionConsistencyBody:
									bodyCount++
								case PartitionConsistencyDirectory:
									directoryCount++
								case PartitionConsistencyVersion:
									versionCount++
								}
							}
							token, done = next, end
							Expect(partitionWholeStore(rc, store)).To(Equal(before))
							Expect(partitionVersionEffectsOf(rc)).To(Equal(effects))
							Expect(rc.localVersion.Load()).To(Equal(counter))
							return nil, nil
						})
						Expect(err).NotTo(HaveOccurred())
						if done {
							break
						}
					}
					Expect(bodyCount).To(Equal(3))
					Expect(directoryCount).To(Equal(4))
					if format == 4 {
						Expect(versionCount).To(Equal(3))
					} else {
						Expect(versionCount).To(BeZero())
					}
				})
			}
		}
	}

	for _, format := range []int32{4, 14} {
		It(fmt.Sprintf("checks pending readable versions but does not census unordered orphan maps at format %d", format), func() {
			md, err := partitionTestBuilder().SetStoreRecordVersions(true).Build()
			Expect(err).NotTo(HaveOccurred())
			root := specSubspace()
			_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
				store := partitionOpenFormat(rc, md, root, format)
				_, err := store.SaveRecord(partitionOrder(1, -11, 7))
				Expect(err).NotTo(HaveOccurred())
				Expect(partitionVersionEffectsOf(rc).mutations).NotTo(BeEmpty())
				before, effects, counter := partitionWholeStore(rc, store), partitionVersionEffectsOf(rc), rc.localVersion.Load()
				items, _, done := partitionIntegrationPage(store.CheckPartitionConsistency("Order", PartitionCoordinateRange{}, nil, ForwardScan()))
				Expect(done).To(BeTrue())
				var bodies int
				for _, item := range items {
					if item.Kind == PartitionConsistencyBody {
						bodies++
					}
				}
				Expect(bodies).To(Equal(1))
				Expect(partitionWholeStore(rc, store)).To(Equal(before))
				Expect(partitionVersionEffectsOf(rc)).To(Equal(effects))
				Expect(rc.localVersion.Load()).To(Equal(counter))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	}

	It("refuses unchecked Build without native work but reads an opened fresh empty store", func() {
		md, err := partitionTestBuilder().SetStoreRecordVersions(true).Build()
		Expect(err).NotTo(HaveOccurred())
		root := specSubspace()
		_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
			store := partitionProtocolStore(rc, md, root)
			before, effects, counter := partitionWholeStore(rc, store), partitionVersionEffectsOf(rc), rc.localVersion.Load()
			count := &partitionNativeReadCounter{WritableTransaction: rc.tx}
			rc.tx = count
			unchecked, err := NewStoreBuilder().SetContext(rc).SetMetaDataProvider(md).SetSubspace(root).Build()
			Expect(err).NotTo(HaveOccurred())
			_, err = unchecked.ScanPartitions("Order", PartitionCoordinateRange{}, nil, ForwardScan()).OnNext(ctx)
			var metadata *PartitionMetadataError
			Expect(errors.As(err, &metadata)).To(BeTrue())
			_, err = unchecked.CheckPartitionConsistency("Order", PartitionCoordinateRange{}, nil, ForwardScan()).OnNext(ctx)
			Expect(errors.As(err, &metadata)).To(BeTrue())
			Expect(count.reads).To(BeZero())
			rows, _, done := partitionIntegrationPage(store.ScanPartitions("Order", PartitionCoordinateRange{}, nil, ForwardScan()))
			Expect(rows).To(BeEmpty())
			Expect(done).To(BeTrue())
			work, _, done := partitionIntegrationPage(store.CheckPartitionConsistency("Order", PartitionCoordinateRange{}, nil, ForwardScan()))
			Expect(work).To(BeEmpty())
			Expect(done).To(BeTrue())
			Expect(count.reads).To(Equal(8))
			Expect(partitionWholeStore(rc, store)).To(Equal(before))
			Expect(partitionVersionEffectsOf(rc)).To(Equal(effects))
			Expect(rc.localVersion.Load()).To(Equal(counter))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("reads an opened current-format store that retains the legacy omission flag", func() {
		md, err := partitionTestBuilder().SetStoreRecordVersions(true).Build()
		Expect(err).NotTo(HaveOccurred())
		root := specSubspace()
		_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
			_, err := partitionOpenFormat(rc, md, root, 4).SaveRecord(partitionOrder(1, 0, 7))
			return nil, err
		})
		Expect(err).NotTo(HaveOccurred())
		_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
			store := partitionOpenFormat(rc, md, root, 14)
			Expect(store.GetStoreHeader().GetOmitUnsplitRecordSuffix()).To(BeTrue())
			before, effects, counter := partitionWholeStore(rc, store), partitionVersionEffectsOf(rc), rc.localVersion.Load()
			for _, reverse := range []bool{false, true} {
				directory, _, done := partitionIntegrationPage(store.ScanPartitions("Order", PartitionCoordinateRange{}, nil, ForwardScan().WithReverse(reverse)))
				Expect(done).To(BeTrue())
				Expect(directory).To(HaveLen(1))
				items, _, done := partitionIntegrationPage(store.CheckPartitionConsistency("Order", PartitionCoordinateRange{}, nil, ForwardScan().WithReverse(reverse)))
				Expect(done).To(BeTrue())
				var bodies, versions int
				for _, item := range items {
					if item.Kind == PartitionConsistencyBody {
						bodies++
					}
					if item.Kind == PartitionConsistencyVersion {
						versions++
					}
				}
				Expect(bodies).To(Equal(1))
				Expect(versions).To(Equal(1))
			}
			Expect(partitionWholeStore(rc, store)).To(Equal(before))
			Expect(partitionVersionEffectsOf(rc)).To(Equal(effects))
			Expect(rc.localVersion.Load()).To(Equal(counter))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("audits native legacy versions even when metadata disables version storage", func() {
		b := partitionTestBuilder().SetStoreRecordVersions(true)
		md, err := b.Build()
		Expect(err).NotTo(HaveOccurred())
		root := specSubspace()
		_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
			_, err := partitionOpenFormat(rc, md, root, 4).SaveRecord(partitionOrder(1, 0, 7))
			return nil, err
		})
		Expect(err).NotTo(HaveOccurred())
		disabled, err := b.SetStoreRecordVersions(false).Build()
		Expect(err).NotTo(HaveOccurred())
		_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
			store := partitionOpenFormat(rc, disabled, root, 4)
			before, effects := partitionWholeStore(rc, store), partitionVersionEffectsOf(rc)
			items, _, done := partitionIntegrationPage(store.CheckPartitionConsistency("Order", PartitionCoordinateRange{}, nil, ForwardScan()))
			Expect(done).To(BeTrue())
			versions := 0
			for _, item := range items {
				if item.Kind == PartitionConsistencyVersion {
					versions++
				}
			}
			Expect(versions).To(Equal(1))
			Expect(partitionWholeStore(rc, store)).To(Equal(before))
			Expect(partitionVersionEffectsOf(rc)).To(Equal(effects))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	for _, corruption := range []string{"second-body", "wrong-id", "null-source", "unknown-protocol", "live-fence-only", "inactive-control", "legacy-only", "split-gap", "unsplit-tail"} {
		It("reads real native corruption without repair: "+corruption, func() {
			md, err := partitionTestBuilder().SetSplitLongRecords(true).SetStoreRecordVersions(true).Build()
			Expect(err).NotTo(HaveOccurred())
			root := specSubspace()
			_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
				store := partitionProtocolStore(rc, md, root)
				for _, id := range []int64{1, 2} {
					_, err := store.SaveRecord(partitionOrder(id, 0, 7))
					Expect(err).NotTo(HaveOccurred())
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
			_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
				store := partitionProtocolStore(rc, md, root)
				rt := md.GetRecordType("Order")
				ss := partitionSubspace(root, rt)
				pk := tuple.Tuple{rt.GetRecordTypeKey(), int64(0), int64(0), int64(2)}
				key := store.recordsSubspace.Pack(appendToTuple(pk, int64(0)))
				switch corruption {
				case "second-body":
					rc.Transaction().Set(key, []byte{0xff})
				case "wrong-id", "null-source":
					record := partitionOrder(3, 0, 7)
					if corruption == "null-source" {
						record = &demopb.Order{OrderId: proto.Int64(2)}
					}
					raw, err := serializeUnionOver(record, rt, nil)
					Expect(err).NotTo(HaveOccurred())
					rc.Transaction().Set(key, raw)
				case "unknown-protocol":
					rc.Transaction().Set(ss.Pack(tuple.Tuple{"unexpected"}), []byte{1})
				case "live-fence-only":
					rc.Transaction().Set(ss.Pack(tuple.Tuple{"fence", int64(2)}), (tuple.Tuple{int64(1), true}).Pack())
				case "inactive-control":
					rc.Transaction().Set(ss.Pack(tuple.Tuple{"fence", int64(2)}), (tuple.Tuple{int64(1), false}).Pack())
				case "legacy-only":
					version, err := CompleteVersionFromBytes(make([]byte, VersionBytes))
					Expect(err).NotTo(HaveOccurred())
					rc.Transaction().Set(root.Sub(RecordVersionKey).Pack(tuple.Tuple{rt.GetRecordTypeKey(), int64(2), int64(20), int64(3)}), version.ToBytes())
				case "split-gap":
					rc.Transaction().Clear(key)
					rc.Transaction().Set(store.recordsSubspace.Pack(appendToTuple(pk, int64(1))), []byte{1})
					rc.Transaction().Set(store.recordsSubspace.Pack(appendToTuple(pk, int64(3))), []byte{2})
				case "unsplit-tail":
					rc.Transaction().Set(store.recordsSubspace.Pack(appendToTuple(pk, int64(2))), []byte{1})
				}
				before, effects, counter := partitionWholeStore(rc, store), partitionVersionEffectsOf(rc), rc.localVersion.Load()
				for _, reverse := range []bool{false, true} {
					c := store.CheckPartitionConsistency("Order", PartitionCoordinateRange{}, nil, ForwardScan().WithReverse(reverse))
					var readErr error
					var count int
					for range 100 {
						r, err := c.OnNext(ctx)
						if err != nil {
							readErr = err
							break
						}
						if !r.HasNext() {
							break
						}
						count++
					}
					if corruption != "legacy-only" {
						Expect(count).To(BeNumerically(">", 0))
					}
					if corruption == "inactive-control" {
						Expect(readErr).NotTo(HaveOccurred())
					} else {
						var state *PartitionStateError
						Expect(errors.As(readErr, &state)).To(BeTrue(), "%v", readErr)
					}
				}
				Expect(partitionWholeStore(rc, store)).To(Equal(before))
				Expect(partitionVersionEffectsOf(rc)).To(Equal(effects))
				Expect(rc.localVersion.Load()).To(Equal(counter))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	}

	It("shares local revisions across handles and notices activation two at the same persisted epoch", func() {
		md, err := partitionTestBuilder().SetStoreRecordVersions(true).Build()
		Expect(err).NotTo(HaveOccurred())
		root := specSubspace()
		_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
			_, err := partitionProtocolStore(rc, md, root).SaveRecord(partitionOrder(1, 0, 7))
			return nil, err
		})
		Expect(err).NotTo(HaveOccurred())
		_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
			a, b := partitionProtocolStore(rc, md, root), partitionProtocolStore(rc, md, root)
			directory := a.ScanPartitions("Order", PartitionCoordinateRange{}, nil, ForwardScan())
			checker := a.CheckPartitionConsistency("Order", PartitionCoordinateRange{}, nil, ForwardScan())
			token := partitionIntegrationToken(directory)
			checkerToken := partitionIntegrationToken(checker)
			_, err := b.SaveRecord(partitionOrder(2, 10, 7))
			Expect(err).NotTo(HaveOccurred())
			partitionExpectInvalid(directory)
			partitionExpectInvalid(checker)
			partitionExpectInvalid(a.ScanPartitions("Order", PartitionCoordinateRange{}, token, ForwardScan()))
			partitionExpectInvalid(a.CheckPartitionConsistency("Order", PartitionCoordinateRange{}, checkerToken, ForwardScan()))
			token = partitionIntegrationToken(a.ScanPartitions("Order", PartitionCoordinateRange{}, nil, ForwardScan()))
			ss := partitionSubspace(root, md.GetRecordType("Order"))
			epoch := rc.Transaction().Get(ss.Pack(tuple.Tuple{"epoch"})).MustGet()
			_, err = b.SaveRecord(partitionOrder(3, 20, 7))
			Expect(err).NotTo(HaveOccurred())
			Expect(rc.Transaction().Get(ss.Pack(tuple.Tuple{"epoch"})).MustGet()).To(Equal(epoch))
			partitionExpectInvalid(a.ScanPartitions("Order", PartitionCoordinateRange{}, token, ForwardScan()))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	for _, ending := range []string{"commit", "rollback", "cancel"} {
		It("never makes a dirty token portable after "+ending+" while clean rollback tokens remain portable", func() {
			md, err := partitionTestBuilder().SetStoreRecordVersions(true).Build()
			Expect(err).NotTo(HaveOccurred())
			root := specSubspace()
			_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
				_, err := partitionProtocolStore(rc, md, root).SaveRecord(partitionOrder(1, 0, 7))
				return nil, err
			})
			Expect(err).NotTo(HaveOccurred())
			var clean, dirty []byte
			var baseline, committed []fdb.KeyValue
			var origin *FDBRecordContext
			var originCount *partitionNativeReadCounter
			var directory RecordCursor[PartitionEntry]
			var checker RecordCursor[PartitionConsistencyResult]
			refusal := errors.New("deliberate rollback")
			_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
				store := partitionProtocolStore(rc, md, root)
				origin, originCount = rc, &partitionNativeReadCounter{WritableTransaction: rc.tx}
				rc.tx = originCount
				baseline = partitionWholeStore(rc, store)
				effects, counter := partitionVersionEffectsOf(rc), rc.localVersion.Load()
				clean = partitionIntegrationToken(store.ScanPartitions("Order", PartitionCoordinateRange{}, nil, ForwardScan()))
				Expect(partitionWholeStore(rc, store)).To(Equal(baseline))
				Expect(partitionVersionEffectsOf(rc)).To(Equal(effects))
				Expect(rc.localVersion.Load()).To(Equal(counter))
				_, err := store.SaveRecord(partitionOrder(2, 10, 7))
				Expect(err).NotTo(HaveOccurred())
				staged, pending := partitionWholeStore(rc, store), partitionVersionEffectsOf(rc)
				Expect(staged).NotTo(Equal(baseline))
				Expect(pending.mutations).To(HaveLen(1))
				Expect(pending.local).To(HaveLen(1))
				Expect(rc.localVersion.Load()).To(Equal(counter + 1))
				directory = store.ScanPartitions("Order", PartitionCoordinateRange{}, nil, ForwardScan())
				checker = store.CheckPartitionConsistency("Order", PartitionCoordinateRange{}, nil, ForwardScan())
				dirty = partitionIntegrationToken(directory)
				partitionIntegrationToken(checker)
				Expect(partitionWholeStore(rc, store)).To(Equal(staged))
				Expect(partitionVersionEffectsOf(rc)).To(Equal(pending))
				Expect(rc.localVersion.Load()).To(Equal(counter + 1))
				if ending == "commit" {
					return nil, nil
				}
				if ending == "cancel" {
					rc.Cancel()
				}
				return nil, refusal
			})
			switch ending {
			case "commit":
				Expect(err).NotTo(HaveOccurred())
			case "rollback":
				Expect(errors.Is(err, refusal)).To(BeTrue(), "rollback returned %T: %v", err, err)
			case "cancel":
				var cancelled fdb.Error
				Expect(errors.As(err, &cancelled)).To(BeTrue(), "cancel returned %T: %v", err, err)
				Expect(cancelled.Code).To(Equal(1025))
			}
			Expect(clean).NotTo(BeEmpty())
			Expect(dirty).NotTo(BeEmpty())
			Expect(origin).NotTo(BeNil())
			Expect(origin.partitionReadEnded.Load()).To(BeTrue())
			endedReads, endedEffects, endedCounter := originCount.reads, partitionVersionEffectsOf(origin), origin.localVersion.Load()
			for range 2 {
				_, err := directory.OnNext(ctx)
				Expect(err).To(HaveOccurred())
				_, err = checker.OnNext(ctx)
				Expect(err).To(HaveOccurred())
			}
			Expect(originCount.reads).To(Equal(endedReads))
			Expect(partitionVersionEffectsOf(origin)).To(Equal(endedEffects))
			Expect(origin.localVersion.Load()).To(Equal(endedCounter))
			_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
				store := partitionProtocolStore(rc, md, root)
				committed = partitionWholeStore(rc, store)
				if ending == "commit" {
					Expect(committed).NotTo(Equal(baseline))
				} else {
					Expect(committed).To(Equal(baseline))
				}
				effects, counter := partitionVersionEffectsOf(rc), rc.localVersion.Load()
				count := &partitionNativeReadCounter{WritableTransaction: rc.tx}
				rc.tx = count
				partitionExpectInvalid(store.ScanPartitions("Order", PartitionCoordinateRange{}, dirty, ForwardScan()))
				Expect(count.reads).To(BeZero())
				if ending == "commit" {
					partitionExpectInvalid(store.ScanPartitions("Order", PartitionCoordinateRange{}, clean, ForwardScan()))
				} else {
					r, err := store.ScanPartitions("Order", PartitionCoordinateRange{}, clean, ForwardScan()).OnNext(ctx)
					Expect(err).NotTo(HaveOccurred())
					Expect(r.HasNext()).To(BeFalse())
					Expect(r.GetNoNextReason()).To(Equal(SourceExhausted))
				}
				row, err := store.LoadRecord(tuple.Tuple{md.GetRecordType("Order").GetRecordTypeKey(), int64(1), int64(10), int64(2)})
				Expect(err).NotTo(HaveOccurred())
				if ending == "commit" {
					Expect(row).NotTo(BeNil())
					Expect(row.Version).NotTo(BeNil())
					Expect(row.Version.IsComplete()).To(BeTrue())
					Expect(row.Version.GetLocalVersion()).To(Equal(0))
				} else {
					Expect(row).To(BeNil())
				}
				Expect(partitionWholeStore(rc, store)).To(Equal(committed))
				Expect(partitionVersionEffectsOf(rc)).To(Equal(effects))
				Expect(rc.localVersion.Load()).To(Equal(counter))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
			_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
				Expect(partitionWholeStore(rc, partitionProtocolStore(rc, md, root))).To(Equal(committed))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	}

	It("keeps tokens valid for active-row changes, previews, clears, and another table", func() {
		md, err := partitionTestBuilder().SetStoreRecordVersions(true).Build()
		Expect(err).NotTo(HaveOccurred())
		root := specSubspace()
		_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
			_, err := partitionProtocolStore(rc, md, root).SaveRecord(partitionOrder(1, 0, 7))
			return nil, err
		})
		Expect(err).NotTo(HaveOccurred())
		_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
			store := partitionProtocolStore(rc, md, root)
			token := partitionIntegrationToken(store.ScanPartitions("Order", PartitionCoordinateRange{}, nil, ForwardScan()))
			_, err := store.SaveRecord(partitionOrder(1, 0, 9))
			Expect(err).NotTo(HaveOccurred())
			_, err = store.SaveRecord(&demopb.TypedRecord{Id: proto.Int64(1), ValInt64: proto.Int64(20)})
			Expect(err).NotTo(HaveOccurred())
			_, err = store.DryRunSaveRecord(partitionOrder(9, 90, 7), RecordExistenceCheckNone)
			Expect(err).NotTo(HaveOccurred())
			_, err = store.DryRunDeleteRecord(tuple.Tuple{md.GetRecordType("Order").GetRecordTypeKey(), int64(0), int64(0), int64(1)})
			Expect(err).NotTo(HaveOccurred())
			_, err = store.DryRunSaveRecord(partitionOrder(9, 90, 7), RecordExistenceCheckErrorIfNotExists)
			Expect(err).To(HaveOccurred())
			_, err = store.DeleteRecord(tuple.Tuple{md.GetRecordType("Order").GetRecordTypeKey(), int64(0), int64(0), int64(1)})
			Expect(err).NotTo(HaveOccurred())
			Expect(store.ClearPartitionData("Order", 0)).To(Succeed())
			_, err = store.ScanPartitions("Order", PartitionCoordinateRange{}, token, ForwardScan()).OnNext(ctx)
			Expect(err).NotTo(HaveOccurred())
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	for _, reset := range []string{"delete-all", "delete-store"} {
		It("invalidates local and portable identities across "+reset+" and recreate", func() {
			md, err := partitionTestBuilder().Build()
			Expect(err).NotTo(HaveOccurred())
			root := specSubspace()
			_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
				_, err := partitionProtocolStore(rc, md, root).SaveRecord(partitionOrder(1, 0, 7))
				return nil, err
			})
			Expect(err).NotTo(HaveOccurred())
			var token []byte
			_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
				store := partitionProtocolStore(rc, md, root)
				token = partitionIntegrationToken(store.ScanPartitions("Order", PartitionCoordinateRange{}, nil, ForwardScan()))
				if reset == "delete-all" {
					Expect(store.DeleteAllRecords()).To(Succeed())
				} else {
					Expect(DeleteStore(rc, root)).To(Succeed())
					store = partitionProtocolStore(rc, md, root)
				}
				_, err := store.SaveRecord(partitionOrder(1, 0, 7))
				Expect(err).NotTo(HaveOccurred())
				partitionExpectInvalid(store.ScanPartitions("Order", PartitionCoordinateRange{}, token, ForwardScan()))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
			_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
				store := partitionProtocolStore(rc, md, root)
				partitionExpectInvalid(store.ScanPartitions("Order", PartitionCoordinateRange{}, token, ForwardScan()))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	}

	for _, operation := range []string{"activate", "delete-all", "delete-store"} {
		It("refuses local revision overflow before effects for "+operation, func() {
			md, err := partitionTestBuilder().SetStoreRecordVersions(true).Build()
			Expect(err).NotTo(HaveOccurred())
			root := specSubspace()
			_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
				_, err := partitionProtocolStore(rc, md, root).SaveRecord(partitionOrder(1, 0, 7))
				return nil, err
			})
			Expect(err).NotTo(HaveOccurred())
			var before []fdb.KeyValue
			commitCheckRan := false
			_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
				store := partitionProtocolStore(rc, md, root)
				state := store.partitionContextState()
				state.mu.Lock()
				state.revisions = map[string]int64{string(partitionSubspace(root, md.GetRecordType("Order")).Bytes()): math.MaxInt64}
				revisions, epochs, failed := maps.Clone(state.revisions), maps.Clone(state.epochChanged), state.failed
				state.mu.Unlock()
				Expect(failed).To(BeNil())
				before = partitionWholeStore(rc, store)
				effects, counter := partitionVersionEffectsOf(rc), rc.localVersion.Load()
				var refused error
				switch operation {
				case "activate":
					_, refused = store.SaveRecord(partitionOrder(2, 10, 7))
				case "delete-all":
					refused = store.DeleteAllRecords()
				case "delete-store":
					refused = DeleteStore(rc, root)
				}
				var typed *PartitionStateError
				Expect(errors.As(refused, &typed)).To(BeTrue(), "%v", refused)
				Expect(typed.Reason).To(Equal("local topology revision exhausted"))
				Expect(partitionWholeStore(rc, store)).To(Equal(before))
				Expect(partitionVersionEffectsOf(rc)).To(Equal(effects))
				// Java chooses the ordinary save version before PK/load/routing.
				var allocated int32
				if operation == "activate" {
					allocated = 1
				}
				Expect(rc.localVersion.Load()).To(Equal(counter + allocated))
				state.mu.Lock()
				afterRevisions, afterEpochs, afterFailed := maps.Clone(state.revisions), maps.Clone(state.epochChanged), state.failed
				state.mu.Unlock()
				Expect(maps.Equal(afterRevisions, revisions)).To(BeTrue())
				Expect(maps.Equal(afterEpochs, epochs)).To(BeTrue())
				Expect(afterFailed).To(BeNil())
				rc.AddCommitCheck(func() error { commitCheckRan = true; return nil })
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(commitCheckRan).To(BeTrue())
			_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
				store := partitionProtocolStore(rc, md, root)
				effects, counter := partitionVersionEffectsOf(rc), rc.localVersion.Load()
				Expect(partitionWholeStore(rc, store)).To(Equal(before))
				Expect(partitionVersionEffectsOf(rc)).To(Equal(effects))
				Expect(rc.localVersion.Load()).To(Equal(counter))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	}

	It("keeps active writes legal at maximum revision and preserves a refused save's allocated version gap", func() {
		md, err := partitionTestBuilder().SetStoreRecordVersions(true).Build()
		Expect(err).NotTo(HaveOccurred())
		Expect(md.GetAllIndexes()).To(BeEmpty())
		root := specSubspace()
		_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
			_, err := partitionProtocolStore(rc, md, root).SaveRecord(partitionOrder(1, 0, 7))
			return nil, err
		})
		Expect(err).NotTo(HaveOccurred())
		var protocol []fdb.KeyValue
		var originalKeys []string
		_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
			store := partitionProtocolStore(rc, md, root)
			rt := md.GetRecordType("Order")
			state := store.partitionContextState()
			state.mu.Lock()
			state.revisions = map[string]int64{string(partitionSubspace(root, rt).Bytes()): math.MaxInt64}
			revisions, epochs := maps.Clone(state.revisions), maps.Clone(state.epochChanged)
			state.mu.Unlock()
			before, effects, counter := partitionWholeStore(rc, store), partitionVersionEffectsOf(rc), rc.localVersion.Load()
			Expect(counter).To(Equal(int32(0)))
			protocol = partitionProtocolRows(rc, root)
			originalKeys = nil
			for _, kv := range before {
				originalKeys = append(originalKeys, string(kv.Key))
			}
			_, refused := store.SaveRecord(partitionOrder(2, 10, 7))
			var typed *PartitionStateError
			Expect(errors.As(refused, &typed)).To(BeTrue(), "%v", refused)
			Expect(typed.Reason).To(Equal("local topology revision exhausted"))
			Expect(rc.localVersion.Load()).To(Equal(counter + 1))
			Expect(partitionWholeStore(rc, store)).To(Equal(before))
			Expect(partitionVersionEffectsOf(rc)).To(Equal(effects))
			updated, err := store.SaveRecord(partitionOrder(1, 0, 8))
			Expect(err).NotTo(HaveOccurred())
			Expect(updated.Version).NotTo(BeNil())
			Expect(updated.Version.IsComplete()).To(BeFalse())
			Expect(updated.Version.GetLocalVersion()).To(Equal(1))
			Expect(rc.localVersion.Load()).To(Equal(counter + 2))
			Expect(partitionWholeStore(rc, store)).NotTo(Equal(before))
			Expect(partitionProtocolRows(rc, root)).To(Equal(protocol))
			pending := partitionVersionEffectsOf(rc)
			Expect(pending.mutations).To(HaveLen(1))
			Expect(pending.local).To(HaveLen(1))
			Expect(pending.local).To(HaveKey(string(store.versionKey(updated.PrimaryKey))))
			Expect(pending.local).NotTo(HaveKey(string(store.versionKey(tuple.Tuple{rt.GetRecordTypeKey(), int64(1), int64(10), int64(2)}))))
			state.mu.Lock()
			afterRevisions, afterEpochs, afterFailed := maps.Clone(state.revisions), maps.Clone(state.epochChanged), state.failed
			state.mu.Unlock()
			Expect(maps.Equal(afterRevisions, revisions)).To(BeTrue())
			Expect(maps.Equal(afterEpochs, epochs)).To(BeTrue())
			Expect(afterFailed).To(BeNil())
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
		_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
			store := partitionProtocolStore(rc, md, root)
			rt := md.GetRecordType("Order")
			effects, counter := partitionVersionEffectsOf(rc), rc.localVersion.Load()
			row, err := store.LoadRecord(tuple.Tuple{rt.GetRecordTypeKey(), int64(0), int64(0), int64(1)})
			Expect(err).NotTo(HaveOccurred())
			Expect(row).NotTo(BeNil())
			Expect(row.Record.(*demopb.Order).GetPrice()).To(Equal(int32(8)))
			Expect(row.Version).NotTo(BeNil())
			Expect(row.Version.IsComplete()).To(BeTrue())
			Expect(row.Version.GetLocalVersion()).To(Equal(1))
			missing, err := store.LoadRecord(tuple.Tuple{rt.GetRecordTypeKey(), int64(1), int64(10), int64(2)})
			Expect(err).NotTo(HaveOccurred())
			Expect(missing).To(BeNil())
			Expect(partitionProtocolRows(rc, root)).To(Equal(protocol))
			var keys []string
			for _, kv := range partitionWholeStore(rc, store) {
				keys = append(keys, string(kv.Key))
			}
			Expect(keys).To(Equal(originalKeys))
			Expect(partitionVersionEffectsOf(rc)).To(Equal(effects))
			Expect(rc.localVersion.Load()).To(Equal(counter))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	for _, owner := range []string{"database", "runner"} {
		for _, ending := range []string{"callback-error", "precommit-error", "panic", "retry", "success"} {
			It("ends only the exited managed partition read attempt: "+owner+"/"+ending, func() {
				md, err := partitionTestBuilder().Build()
				Expect(err).NotTo(HaveOccurred())
				root := specSubspace()
				_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
					store := partitionProtocolStore(rc, md, root)
					_, err := store.SaveRecord(partitionOrder(1, 0, 7))
					Expect(err).NotTo(HaveOccurred())
					_, err = store.SaveRecord(partitionOrder(2, 40, 7))
					Expect(err).NotTo(HaveOccurred())
					_, err = store.DeleteRecord(tuple.Tuple{md.GetRecordType("Order").GetRecordTypeKey(), int64(0), int64(0), int64(1)})
					return nil, err
				})
				Expect(err).NotTo(HaveOccurred())
				type retained struct {
					rc        *FDBRecordContext
					count     *partitionNativeReadCounter
					directory RecordCursor[PartitionEntry]
					checker   RecordCursor[PartitionConsistencyResult]
				}
				var previous []retained
				attempt := 0
				refusal := errors.New("managed attempt failure")
				body := func(rc *FDBRecordContext) (any, error) {
					attempt++
					for _, prior := range previous {
						reads := prior.count.reads
						_, err := prior.directory.OnNext(ctx)
						Expect(err).To(HaveOccurred())
						_, err = prior.checker.OnNext(ctx)
						Expect(err).To(HaveOccurred())
						Expect(prior.count.reads).To(Equal(reads))
						Expect(prior.rc.partitionReadEnded.Load()).To(BeTrue())
					}
					store := partitionProtocolStore(rc, md, root)
					count := &partitionNativeReadCounter{WritableTransaction: rc.tx}
					rc.tx = count
					var current []retained
					for _, cached := range []string{"head", "stop", "eof"} {
						props := ForwardScan()
						if cached == "stop" {
							props = NewScanProperties(ExecuteProperties{ReturnedRowLimit: 1})
						}
						directory := store.ScanPartitions("Order", PartitionCoordinateRange{}, nil, props)
						checker := store.CheckPartitionConsistency("Order", PartitionCoordinateRange{}, nil, props)
						partitionIntegrationToken(directory)
						partitionIntegrationToken(checker)
						if cached == "stop" {
							r, err := directory.OnNext(ctx)
							Expect(err).NotTo(HaveOccurred())
							Expect(r.GetNoNextReason()).To(Equal(ReturnLimitReached))
							r2, err := checker.OnNext(ctx)
							Expect(err).NotTo(HaveOccurred())
							Expect(r2.GetNoNextReason()).To(Equal(ReturnLimitReached))
						} else if cached == "eof" {
							partitionIntegrationPage(directory)
							partitionIntegrationPage(checker)
						}
						current = append(current, retained{rc, count, directory, checker})
					}
					previous = append(previous, current...)
					Expect(rc.partitionReadEnded.Load()).To(BeFalse())
					switch ending {
					case "callback-error":
						return nil, refusal
					case "panic":
						panic("managed partition lifecycle panic")
					case "retry":
						if attempt == 1 {
							return nil, fdb.Error{Code: 1020}
						}
					}
					rc.AddCommitCheck(func() error {
						Expect(rc.partitionReadEnded.Load()).To(BeFalse())
						for _, live := range current {
							_, err := live.directory.OnNext(ctx)
							Expect(err).NotTo(HaveOccurred())
							_, err = live.checker.OnNext(ctx)
							Expect(err).NotTo(HaveOccurred())
						}
						if ending == "precommit-error" {
							return refusal
						}
						return nil
					})
					return nil, nil
				}
				run := func() {
					if owner == "runner" {
						_, err = NewFDBDatabaseRunner(sharedDB).SetMaxAttempts(3).SetInitialDelay(time.Millisecond).SetMaxDelay(time.Millisecond).RunWithRetry(ctx, body)
					} else {
						_, err = sharedDB.Run(ctx, body)
					}
				}
				if ending == "panic" {
					Expect(run).To(Panic())
				} else {
					run()
					if ending == "callback-error" || ending == "precommit-error" {
						Expect(errors.Is(err, refusal)).To(BeTrue())
					} else {
						Expect(err).NotTo(HaveOccurred())
					}
				}
				if ending == "retry" {
					Expect(attempt).To(Equal(2))
				} else {
					Expect(attempt).To(Equal(1))
				}
				Expect(previous).NotTo(BeEmpty())
				for _, prior := range previous {
					Expect(prior.rc.partitionReadEnded.Load()).To(BeTrue())
					reads := prior.count.reads
					for range 2 {
						_, err := prior.directory.OnNext(ctx)
						Expect(err).To(HaveOccurred())
						_, err = prior.checker.OnNext(ctx)
						Expect(err).To(HaveOccurred())
					}
					Expect(prior.count.reads).To(Equal(reads))
				}
			})
		}
	}

	It("rejects explicitly cancelled cached native heads and EOF without extra native reads", func() {
		md, err := partitionTestBuilder().Build()
		Expect(err).NotTo(HaveOccurred())
		root := specSubspace()
		_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
			store := partitionProtocolStore(rc, md, root)
			_, err := store.SaveRecord(partitionOrder(1, 0, 7))
			return nil, err
		})
		Expect(err).NotTo(HaveOccurred())
		tx, err := sharedDB.CreateWritableTransaction()
		Expect(err).NotTo(HaveOccurred())
		rc := sharedDB.NewRecordContext(tx)
		store := partitionProtocolStore(rc, md, root)
		count := &partitionNativeReadCounter{WritableTransaction: rc.tx}
		rc.tx = count
		directory := store.ScanPartitions("Order", PartitionCoordinateRange{}, nil, ForwardScan())
		checker := store.CheckPartitionConsistency("Order", PartitionCoordinateRange{}, nil, ForwardScan())
		partitionIntegrationPage(directory)
		partitionIntegrationPage(checker)
		before := count.reads
		rc.Cancel()
		for range 2 {
			_, err := directory.OnNext(ctx)
			Expect(err).To(HaveOccurred())
			_, err = checker.OnNext(ctx)
			Expect(err).To(HaveOccurred())
		}
		Expect(count.reads).To(Equal(before))
	})

	It("shares limiter counters between independent directory and checker roots without mutating data", func() {
		md, err := partitionTestBuilder().Build()
		Expect(err).NotTo(HaveOccurred())
		root := specSubspace()
		_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
			store := partitionProtocolStore(rc, md, root)
			_, err := store.SaveRecord(partitionOrder(1, 0, 7))
			Expect(err).NotTo(HaveOccurred())
			state := NewScanLimiterStateIn(rc.Env())
			props := NewScanProperties(ExecuteProperties{ScannedRecordsLimit: 1, ScanState: state})
			before, effects, counter := partitionWholeStore(rc, store), partitionVersionEffectsOf(rc), rc.localVersion.Load()
			directory := store.ScanPartitions("Order", PartitionCoordinateRange{}, nil, props)
			partitionIntegrationToken(directory)
			r, err := directory.OnNext(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(r.GetNoNextReason()).To(Equal(ScanLimitReached))
			checker := store.CheckPartitionConsistency("Order", PartitionCoordinateRange{}, nil, props)
			partitionIntegrationToken(checker)
			r2, err := checker.OnNext(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(r2.GetNoNextReason()).To(Equal(ScanLimitReached))
			Expect(state.RecordsScanned()).To(Equal(14))
			Expect(partitionWholeStore(rc, store)).To(Equal(before))
			Expect(partitionVersionEffectsOf(rc)).To(Equal(effects))
			Expect(rc.localVersion.Load()).To(Equal(counter))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})
})
