package recordlayer

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sync"

	"fdb.dev/gen/demopb"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/fdbgo/transport"
	"fdb.dev/pkg/fdbgo/wire"
	wiretypes "fdb.dev/pkg/fdbgo/wire/types"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"
)

const batchConflictIndexType = "test_batch_conflict_secondary"

func init() { RegisterIndexMaintainerFactory(batchConflictFactory{}) }

type batchConflictFactory struct{}

func (batchConflictFactory) IndexTypes() []string { return []string{batchConflictIndexType} }

func (batchConflictFactory) IsIdempotent(*Index) bool { return true }

func (batchConflictFactory) ValidateIndexOptions(*Index) error { return nil }

func (batchConflictFactory) ValidateChangedOptions(_, _ *Index, _ map[string]bool) error { return nil }

func (batchConflictFactory) NewIndexMaintainer(state IndexMaintainerState) (IndexMaintainer, error) {
	return &batchConflictMaintainer{StandardIndexMaintainer: *NewStandardIndexMaintainer(state), secondary: state.Store.IndexSecondarySubspace(state.Index)}, nil
}

type batchConflictMaintainer struct {
	StandardIndexMaintainer
	secondary subspace.Subspace
}

func (m *batchConflictMaintainer) Update(old, new *FDBStoredRecord[proto.Message]) error {
	if err := m.StandardIndexMaintainer.Update(old, new); err != nil {
		return err
	}
	if new != nil && new.Record.(*demopb.TypedRecord).GetPrice() < 0 {
		return &RecordCoreArgumentError{Message: "injected index update failure"}
	}
	if old != nil {
		m.Transaction().Clear(m.secondary.Pack(old.PrimaryKey))
	}
	if new != nil {
		m.Transaction().Set(m.secondary.Pack(new.PrimaryKey), []byte("covering secondary"))
	}
	return nil
}

func (m *batchConflictMaintainer) UpdateWhileWriteOnly(old, new *FDBStoredRecord[proto.Message]) error {
	return m.Update(old, new)
}

type batchConflictProbeTransaction struct {
	fdb.WritableTransaction
	added []fdb.KeyRange
}

func (tx *batchConflictProbeTransaction) AddWriteConflictRange(r fdb.ExactRange) error {
	begin, end := r.FDBRangeKeys()
	tx.added = append(tx.added, fdb.KeyRange{Begin: begin, End: end})
	return tx.WritableTransaction.AddWriteConflictRange(r)
}

// The transport capture is process-global. Record-layer specs using it serialize here.
var batchConflictCaptureMu sync.Mutex

type batchConflictCommit struct {
	transaction wiretypes.CommitTransactionRef
	bytes       int
}

func captureBatchConflictCommit(root subspace.Subspace, run func() error) batchConflictCommit {
	batchConflictCaptureMu.Lock()
	defer batchConflictCaptureMu.Unlock()
	var log bytes.Buffer
	stop := transport.CaptureWireLog(&log)
	defer stop()
	err := run()
	stop()
	Expect(err).NotTo(HaveOccurred())
	input := bytes.NewReader(log.Bytes())
	var found []batchConflictCommit
	for {
		var header [29]byte
		if _, err := io.ReadFull(input, header[:]); err == io.EOF {
			break
		} else {
			Expect(err).NotTo(HaveOccurred())
		}
		n := binary.LittleEndian.Uint32(header[25:29])
		Expect(uint64(n)).To(BeNumerically("<=", uint64(input.Len())))
		body := make([]byte, int(n))
		_, err := io.ReadFull(input, body)
		Expect(err).NotTo(HaveOccurred())
		if header[0] != 'S' {
			continue
		}
		r, err := wire.NewReader(body)
		if err != nil || r.FileIdentifier() != wiretypes.CommitTransactionRequestFileID {
			continue
		}
		var request wiretypes.CommitTransactionRequest
		Expect(request.UnmarshalFromReader(r)).To(Succeed())
		for _, mutation := range request.Transaction.Mutations {
			if bytes.HasPrefix(mutation.Param1, root.Bytes()) {
				found = append(found, batchConflictCommit{transaction: request.Transaction, bytes: len(body)})
				break
			}
		}
	}
	Expect(found).To(HaveLen(1), "need one populated source commit, not an empty capture")
	Expect(found[0].transaction.Mutations).NotTo(BeEmpty())
	Expect(found[0].transaction.ReadConflictRanges).NotTo(BeEmpty())
	Expect(found[0].transaction.WriteConflictRanges).NotTo(BeEmpty())
	return found[0]
}

var _ = Describe("Batch index write-conflict coalescing", func() {
	ctx := context.Background()
	options := BatchSaveOptions{CoalesceIndexWriteConflicts: true}
	metadata := func(split bool) *RecordMetaData {
		b := baseBuilder().SetSplitLongRecords(split)
		b.AddIndex("TypedRecord", NewIndex("price", Field("price")))
		b.AddIndex("TypedRecord", NewIndex("bytes", Field("val_bytes")))
		secondary := NewIndex("secondary", Field("price"))
		secondary.Type = batchConflictIndexType
		b.AddIndex("TypedRecord", secondary)
		md, err := b.Build()
		Expect(err).NotTo(HaveOccurred())
		return md
	}
	record := func(id int64, price int32) proto.Message {
		return &demopb.TypedRecord{Id: proto.Int64(id), Price: proto.Int32(price), ValBytes: []byte(fmt.Sprintf("payload-%d", id))}
	}
	open := func(rc *FDBRecordContext, md *RecordMetaData, root subspace.Subspace, format int32) *FDBRecordStore {
		b := NewStoreBuilder().SetContext(rc).SetMetaDataProvider(md).SetSubspace(root)
		if format != 0 {
			b.SetFormatVersion(format)
		}
		s, err := b.CreateOrOpen()
		Expect(err).NotTo(HaveOccurred())
		return s
	}
	transaction := func() *FDBRecordContext {
		tx, err := sharedDB.CreateTransaction()
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(tx.Cancel)
		return sharedDB.NewRecordContext(tx)
	}
	snapshot := func(root subspace.Subspace) []fdb.KeyValue {
		v, err := sharedDB.RunRead(ctx, func(tx fdb.ReadTransaction) (any, error) {
			r, err := fdb.PrefixRange(root.Bytes())
			if err != nil {
				return nil, err
			}
			return tx.GetRange(r, fdb.RangeOptions{Mode: fdb.StreamingModeWantAll}).GetSliceWithError()
		})
		Expect(err).NotTo(HaveOccurred())
		rows := v.([]fdb.KeyValue)
		Expect(rows).NotTo(BeEmpty())
		return rows
	}
	restore := func(root subspace.Subspace, rows []fdb.KeyValue) {
		_, err := sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
			r, err := fdb.PrefixRange(root.Bytes())
			if err != nil {
				return nil, err
			}
			rc.Transaction().ClearRange(r)
			for _, row := range rows {
				rc.Transaction().Set(row.Key, row.Value)
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	}

	for _, layout := range []struct {
		name   string
		split  bool
		format int32
	}{{"modern", true, 0}, {"legacy bare key", false, 4}} {
		It("ships fewer conflict ranges and bytes with identical mutations and reads for "+layout.name, func() {
			md, root := metadata(layout.split), specSubspace()
			_, err := sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
				s := open(rc, md, root, layout.format)
				_, err := s.SaveRecordBatch([]proto.Message{record(1, 99), record(99, 99)})
				return nil, err
			})
			Expect(err).NotTo(HaveOccurred())
			initial := snapshot(root)
			var input []proto.Message
			for i := int64(1); i <= 16; i++ {
				input = append(input, record(i, int32(i)))
			}
			input = append(input, record(1, 40))
			run := func(mode int) (batchConflictCommit, int64) {
				var size int64
				captured := captureBatchConflictCommit(root, func() error {
					_, err := sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
						s := open(rc, md, root, layout.format)
						var err error
						if mode == 0 {
							_, err = s.SaveRecordBatch(input)
						} else {
							opt := BatchSaveOptions{}
							if mode == 2 {
								opt = options
							}
							_, err = s.SaveRecordBatchWithOptions(input, opt)
						}
						if err != nil {
							return nil, err
						}
						size, err = rc.Transaction().GetApproximateSize().Get()
						return nil, err
					})
					return err
				})
				return captured, size
			}
			control, controlSize := run(0)
			wantRows := snapshot(root)
			restore(root, initial)
			zero, zeroSize := run(1)
			Expect(zero.transaction.Mutations).To(Equal(control.transaction.Mutations))
			Expect(zero.transaction.ReadConflictRanges).To(Equal(control.transaction.ReadConflictRanges))
			Expect(zero.transaction.WriteConflictRanges).To(Equal(control.transaction.WriteConflictRanges))
			Expect(zero.bytes).To(Equal(control.bytes))
			Expect(zeroSize).To(Equal(controlSize))
			Expect(snapshot(root)).To(Equal(wantRows))
			restore(root, initial)
			wide, wideSize := run(2)
			Expect(wide.transaction.Mutations).To(Equal(control.transaction.Mutations))
			Expect(wide.transaction.ReadConflictRanges).To(Equal(control.transaction.ReadConflictRanges))
			Expect(snapshot(root)).To(Equal(wantRows))
			Expect(wideSize).To(BeNumerically(">", controlSize), "live accounting retains every implicit conflict")
			Expect(len(wide.transaction.WriteConflictRanges)).To(BeNumerically("<", len(control.transaction.WriteConflictRanges)))
			Expect(wide.bytes).To(BeNumerically("<", control.bytes), "measure the actual captured serialized commit body")
			rangeBytes := func(ranges []wiretypes.KeyRangeRef) int {
				var n int
				for _, r := range ranges {
					n += len(r.Begin) + len(r.End)
				}
				return n
			}
			mutationBytes := 0
			for _, mutation := range control.transaction.Mutations {
				mutationBytes += len(mutation.Param1) + len(mutation.Param2)
			}
			fmt.Printf("batch conflicts layout=%q normal_wcr=%d wide_wcr=%d normal_wcr_endpoint_bytes=%d wide_wcr_endpoint_bytes=%d normal_commit_body_bytes=%d wide_commit_body_bytes=%d unchanged_mutations=%d unchanged_mutation_operand_bytes=%d unchanged_read_conflicts=%d unchanged_read_endpoint_bytes=%d normal_live_approx=%d wide_live_approx=%d\n",
				layout.name, len(control.transaction.WriteConflictRanges), len(wide.transaction.WriteConflictRanges),
				rangeBytes(control.transaction.WriteConflictRanges), rangeBytes(wide.transaction.WriteConflictRanges), control.bytes, wide.bytes,
				len(control.transaction.Mutations), mutationBytes, len(control.transaction.ReadConflictRanges), rangeBytes(control.transaction.ReadConflictRanges), controlSize, wideSize)
			owned, err := fdb.PrefixRange(root.Bytes())
			Expect(err).NotTo(HaveOccurred())
			ownedBegin, ownedEnd := owned.FDBRangeKeys()
			for _, r := range wide.transaction.WriteConflictRanges {
				Expect(bytes.Compare(r.Begin, ownedBegin.FDBKey())).To(BeNumerically(">=", 0))
				Expect(bytes.Compare(r.End, ownedEnd.FDBKey())).To(BeNumerically("<=", 0))
			}
			for _, normal := range control.transaction.WriteConflictRanges {
				covered := false
				for _, r := range wide.transaction.WriteConflictRanges {
					covered = covered || bytes.Compare(r.Begin, normal.Begin) <= 0 && bytes.Compare(r.End, normal.End) >= 0
				}
				Expect(covered).To(BeTrue(), "lost normal conflict [%x,%x)", normal.Begin, normal.End)
			}
			records, err := fdb.PrefixRange(root.Sub(RecordKey).Bytes())
			Expect(err).NotTo(HaveOccurred())
			begin, end := records.FDBRangeKeys()
			primary := func(ranges []wiretypes.KeyRangeRef) []wiretypes.KeyRangeRef {
				var out []wiretypes.KeyRangeRef
				for _, r := range ranges {
					if bytes.Compare(r.Begin, begin.FDBKey()) >= 0 && bytes.Compare(r.End, end.FDBKey()) <= 0 {
						out = append(out, r)
					}
				}
				return out
			}
			Expect(primary(control.transaction.WriteConflictRanges)).NotTo(BeEmpty())
			Expect(primary(wide.transaction.WriteConflictRanges)).To(Equal(primary(control.transaction.WriteConflictRanges)), "primary-record conflicts must not be widened")
		})
	}

	It("adds nothing for empty or failed batches and scopes successful ranges to index data", func() {
		md, root := metadata(true), specSubspace()
		rc := transaction()
		probe := &batchConflictProbeTransaction{WritableTransaction: rc.Transaction()}
		rc.tx = probe
		s := open(rc, md, root, 0)
		probe.added = nil
		_, err := s.SaveRecordBatchWithOptions(nil, options)
		Expect(err).NotTo(HaveOccurred())
		Expect(probe.added).To(BeEmpty())
		_, err = s.SaveRecordBatchWithOptions([]proto.Message{record(1, 1), nil}, options)
		Expect(err).To(HaveOccurred())
		Expect(probe.added).To(BeEmpty())
		_, err = s.SaveRecordBatchWithOptions([]proto.Message{record(3, 3), record(4, -1)}, options)
		Expect(err).To(HaveOccurred(), "a late maintainer failure must not add broad conflicts")
		Expect(probe.added).To(BeEmpty())
		_, err = s.SaveRecordBatchWithOptions([]proto.Message{record(1, 1)}, BatchSaveOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(probe.added).To(BeEmpty())
		_, err = s.SaveRecordBatchWithOptions([]proto.Message{record(2, 2)}, options)
		Expect(err).NotTo(HaveOccurred())
		Expect(probe.added).To(HaveLen(6))
		var expected []fdb.KeyRange
		for _, name := range []string{"bytes", "price", "secondary"} {
			index := md.GetIndex(name)
			for _, ss := range []subspace.Subspace{s.IndexSubspace(index), s.IndexSecondarySubspace(index)} {
				r, err := fdb.PrefixRange(ss.Bytes())
				Expect(err).NotTo(HaveOccurred())
				expected = append(expected, r)
			}
		}
		Expect(probe.added).To(Equal(expected))
	})

	It("lets disjoint bulk writers commit and does not cover another owner's indexes", func() {
		md, root := metadata(true), specSubspace().Sub("owner")
		otherRoot := specSubspace().Sub("other")
		_, err := sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
			open(rc, md, root, 0)
			open(rc, md, otherRoot, 0)
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
		a, b, other := transaction(), transaction(), transaction()
		as, bs := open(a, md, root, 0), open(b, md, root, 0)
		os := open(other, md, otherRoot, 0)
		_, err = other.Transaction().GetRange(os.IndexSubspace(md.GetIndex("price")), fdb.RangeOptions{}).GetSliceWithError()
		Expect(err).NotTo(HaveOccurred())
		other.Transaction().Set(otherRoot.Sub("marker").FDBKey(), []byte("unrelated reader"))
		_, err = as.SaveRecordBatchWithOptions([]proto.Message{record(1, 1)}, options)
		Expect(err).NotTo(HaveOccurred())
		_, err = bs.SaveRecordBatchWithOptions([]proto.Message{record(2, 2)}, options)
		Expect(err).NotTo(HaveOccurred())
		Expect(a.Commit()).To(Succeed())
		Expect(b.Commit()).To(Succeed(), "write/write range overlap alone must not serialize disjoint writers")
		Expect(other.Commit()).To(Succeed(), "another owner's serializable index read was not changed")
	})

	It("preserves conflicts for concurrent index readers and authoritative primary readers", func() {
		md, root := metadata(true), specSubspace()
		_, err := sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
			open(rc, md, root, 0)
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
		indexReader, primaryReader, writer := transaction(), transaction(), transaction()
		is := open(indexReader, md, root, 0)
		ps := open(primaryReader, md, root, 0)
		ws := open(writer, md, root, 0)
		rows, err := indexReader.Transaction().GetRange(is.IndexSubspace(md.GetIndex("price")), fdb.RangeOptions{}).GetSliceWithError()
		Expect(err).NotTo(HaveOccurred())
		Expect(rows).To(BeEmpty())
		old, err := ps.LoadRecord(tuple.Tuple{int64(1)})
		Expect(err).NotTo(HaveOccurred())
		Expect(old).To(BeNil())
		indexReader.Transaction().Set(root.Sub("index-reader").FDBKey(), []byte("must conflict"))
		primaryReader.Transaction().Set(root.Sub("primary-reader").FDBKey(), []byte("must conflict"))
		_, err = ws.SaveRecordBatchWithOptions([]proto.Message{record(1, 1)}, options)
		Expect(err).NotTo(HaveOccurred())
		Expect(writer.Commit()).To(Succeed())
		for _, reader := range []*FDBRecordContext{indexReader, primaryReader} {
			var conflict fdb.Error
			Expect(errors.As(reader.Commit(), &conflict)).To(BeTrue())
			Expect(conflict.Code).To(Equal(1020))
		}
	})
})
