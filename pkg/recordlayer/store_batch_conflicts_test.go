package recordlayer

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"slices"
	"sync"
	"time"

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

// Install a conflict after the fixture body; deferred commit checks still run
// before rejection, so both discarded and successful attempts exercise them.
type fixtureCommitRetry struct {
	key                          fdb.Key
	injectedContext, lastContext *FDBRecordContext
}

func newFixtureCommitRetry(root subspace.Subspace) *fixtureCommitRetry {
	return &fixtureCommitRetry{key: subspace.Sub("record-fixture-retry", root.Bytes()).Pack(nil)}
}

func (r *fixtureCommitRetry) begin(rc *FDBRecordContext) { r.lastContext = rc }

func (r *fixtureCommitRetry) conflict(rc *FDBRecordContext) error {
	if r.injectedContext != nil {
		return nil
	}
	if _, err := rc.Transaction().Get(r.key).Get(); err != nil {
		return err
	}
	_, err := sharedDB.db.TransactCtx(rc.Context(), func(tx fdb.WritableTransaction) (any, error) {
		tx.Set(r.key, []byte("force the observed attempt to conflict"))
		return nil, nil
	})
	if err == nil {
		r.injectedContext = rc
	}
	return err
}

func (r *fixtureCommitRetry) assertReplayed() {
	GinkgoHelper()
	Expect(r.injectedContext).NotTo(BeNil())
	Expect(r.lastContext).NotTo(BeIdenticalTo(r.injectedContext), "a fresh attempt must follow the injected commit conflict")
}

type (
	fixtureWireAttempt struct {
		context *FDBRecordContext
		offset  int
		started time.Time
	}
	fixtureWireLog struct {
		mu       sync.Mutex
		data     bytes.Buffer
		attempts []fixtureWireAttempt
	}
)

func (l *fixtureWireLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.data.Write(p)
}

func (l *fixtureWireLog) begin(rc *FDBRecordContext) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.attempts = append(l.attempts, fixtureWireAttempt{context: rc, offset: l.data.Len(), started: time.Now()})
}

type fixtureWireFrame struct {
	direction byte
	token     [16]byte
	body      []byte
	attempt   *FDBRecordContext
}

func (l *fixtureWireLog) frames() []fixtureWireFrame {
	// CaptureWireLog.stop has joined the writer before this is called.
	input := bytes.NewReader(l.data.Bytes())
	var frames []fixtureWireFrame
	for {
		offset := l.data.Len() - input.Len()
		var header [29]byte
		if _, err := io.ReadFull(input, header[:]); err == io.EOF {
			break
		} else {
			Expect(err).NotTo(HaveOccurred())
		}
		n := binary.LittleEndian.Uint32(header[25:29])
		Expect(uint64(n)).To(BeNumerically("<=", uint64(input.Len())))
		frame := fixtureWireFrame{direction: header[0], body: make([]byte, int(n))}
		copy(frame.token[:], header[9:25])
		_, err := io.ReadFull(input, frame.body)
		Expect(err).NotTo(HaveOccurred())
		for _, attempt := range l.attempts {
			if attempt.offset <= offset {
				frame.attempt = attempt.context
			}
		}
		frames = append(frames, frame)
	}
	return frames
}

func (l *fixtureWireLog) finalAttempt() *FDBRecordContext {
	if len(l.attempts) == 0 {
		return nil
	} // Existing direct-context captures cannot retry.
	return l.attempts[len(l.attempts)-1].context
}

type batchConflictCommit struct {
	transaction wiretypes.CommitTransactionRef
	bytes       int
	rejected    map[*FDBRecordContext][]int
}

func successfulFixtureCommit(root subspace.Subspace, frames []fixtureWireFrame, final *FDBRecordContext) batchConflictCommit {
	replies := map[[16]byte][]byte{}
	for _, frame := range frames {
		if frame.direction == 'R' {
			replies[frame.token] = frame.body
		}
	}
	var found []batchConflictCommit
	rejected := map[*FDBRecordContext][]int{}
	for _, frame := range frames {
		if frame.direction != 'S' {
			continue
		}
		r, err := wire.NewReader(frame.body)
		if err != nil || r.FileIdentifier() != wiretypes.CommitTransactionRequestFileID {
			continue
		}
		var request wiretypes.CommitTransactionRequest
		Expect(request.UnmarshalFromReader(r)).To(Succeed())
		owned := false
		for _, mutation := range request.Transaction.Mutations {
			owned = owned || bytes.HasPrefix(mutation.Param1, root.Bytes())
		}
		if !owned {
			continue
		}
		body, answered := replies[request.Reply.Token]
		if !answered {
			if frame.attempt == final {
				Expect(answered).To(BeTrue(), "successful commit must have its matching reply")
			}
			continue // An earlier unknown-result attempt need not have received a reply.
		}
		var replyReader wire.Reader
		if err := wire.ReadErrorOrInto(body, &replyReader); err != nil {
			var native *wire.FDBError
			if !errors.As(err, &native) {
				if frame.attempt == final {
					Expect(err).NotTo(HaveOccurred(), "successful commit reply must decode")
				}
				continue
			}
			rejected[frame.attempt] = append(rejected[frame.attempt], native.Code)
			continue
		}
		var reply wiretypes.CommitID
		if err := reply.UnmarshalFromReader(&replyReader); err != nil {
			if frame.attempt == final {
				Expect(err).NotTo(HaveOccurred(), "successful CommitID must decode")
			}
			continue
		}
		if reply.Version == -1 {
			rejected[frame.attempt] = append(rejected[frame.attempt], 1020)
			continue
		}
		if frame.attempt != final {
			continue
		} // Only the attempt accepted by Run supplies the oracle.
		Expect(reply.Version).To(BeNumerically(">", 0))
		found = append(found, batchConflictCommit{transaction: request.Transaction, bytes: len(frame.body)})
	}
	Expect(found).To(HaveLen(1), "one successful populated source commit with a correlated success reply")
	out := found[0]
	out.rejected = rejected
	Expect(out.transaction.Mutations).NotTo(BeEmpty())
	Expect(out.transaction.ReadConflictRanges).NotTo(BeEmpty())
	Expect(out.transaction.WriteConflictRanges).NotTo(BeEmpty())
	return out
}

func captureBatchConflictCommit(root subspace.Subspace, run func() error, log *fixtureWireLog) batchConflictCommit {
	batchConflictCaptureMu.Lock()
	defer batchConflictCaptureMu.Unlock()
	stop := transport.CaptureWireLog(log)
	defer stop()
	err := run()
	stop()
	Expect(err).NotTo(HaveOccurred())
	return successfulFixtureCommit(root, log.frames(), log.finalAttempt())
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
				log := &fixtureWireLog{}
				retry := newFixtureCommitRetry(root.Sub("capture", mode))
				captured := captureBatchConflictCommit(root, func() error {
					_, err := sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
						log.begin(rc)
						retry.begin(rc)
						size = 0
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
						if err == nil && mode == 2 {
							err = retry.conflict(rc)
						}
						return nil, err
					})
					return err
				}, log)
				if mode == 2 {
					retry.assertReplayed()
					Expect(captured.rejected[retry.injectedContext]).To(Equal([]int{1020}), "the injected attempt must have a sent commit and its matching rejection")
				}
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

var _ = Describe("Batch index write-conflict coalescing scope", func() {
	ctx := context.Background()
	options := BatchSaveOptions{CoalesceIndexWriteConflicts: true}
	// Order and Customer each own indexes, share a multi-type index and every
	// universal index; TypedRecord has only the universal ones.
	metadata := func(legacy bool) *RecordMetaData {
		b := baseBuilder().SetSplitLongRecords(!legacy)
		b.AddIndex("Order", NewIndex("order_price", Field("price")))
		b.AddIndex("Customer", NewIndex("customer_price", Field("price")))
		email := NewIndex("customer_email", Field("email"))
		email.Options[IndexOptionUnique] = "true"
		b.AddIndex("Customer", email)
		b.AddMultiTypeIndex([]string{"Order", "Customer"}, NewIndex("order_customer_price", Field("price")))
		b.AddUniversalIndex(NewCountIndex("record_count", Ungrouped(EmptyKey())))
		if !legacy {
			b.SetStoreRecordVersions(true)
			b.AddUniversalIndex(NewVersionIndex("versions", VersionKey()))
		}
		md, err := b.Build()
		Expect(err).NotTo(HaveOccurred())
		return md
	}
	format := func(legacy bool) int32 {
		if legacy {
			return 4
		}
		return 0
	}
	open := func(rc *FDBRecordContext, md *RecordMetaData, root subspace.Subspace, legacy bool) *FDBRecordStore {
		b := NewStoreBuilder().SetContext(rc).SetMetaDataProvider(md).SetSubspace(root)
		if legacy {
			b.SetFormatVersion(format(legacy))
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
	commit := func(md *RecordMetaData, root subspace.Subspace, legacy bool, body func(*FDBRecordStore) error) {
		_, err := sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
			return nil, body(open(rc, md, root, legacy))
		})
		Expect(err).NotTo(HaveOccurred())
	}
	order := func(id int64, price int32) proto.Message {
		return &demopb.Order{OrderId: proto.Int64(id), Price: proto.Int32(price)}
	}
	customer := func(id int64, price int32) proto.Message {
		return &demopb.Customer{CustomerId: proto.Int64(id), Price: proto.Int32(price), Email: proto.String(fmt.Sprintf("c%d@example.com", id))}
	}
	typed := func(id int64) proto.Message {
		return &demopb.TypedRecord{Id: proto.Int64(id), Price: proto.Int32(int32(id))}
	}
	// expected is the primary and secondary prefix range of each named index,
	// in name order, as the batch adds them.
	expected := func(s *FDBRecordStore, md *RecordMetaData, names ...string) []fdb.KeyRange {
		var out []fdb.KeyRange
		for _, name := range slices.Sorted(slices.Values(names)) {
			index := md.GetIndex(name)
			Expect(index).NotTo(BeNil(), name)
			for _, ss := range []subspace.Subspace{s.IndexSubspace(index), s.IndexSecondarySubspace(index)} {
				r, err := fdb.PrefixRange(ss.Bytes())
				Expect(err).NotTo(HaveOccurred())
				out = append(out, r)
			}
		}
		return out
	}

	for _, legacy := range []bool{false, true} {
		layout := "modern"
		if legacy {
			layout = "legacy bare key"
		}
		It("covers exactly the indexes the batch's saved and overwritten types maintain for "+layout, func() {
			md, root := metadata(legacy), specSubspace()
			universal := []string{"record_count"}
			if !legacy {
				universal = append(universal, "versions")
			}
			orderIndexes := append([]string{"order_price", "order_customer_price"}, universal...)
			customerIndexes := append([]string{"customer_price", "customer_email", "order_customer_price"}, universal...)
			allIndexes := append([]string{"order_price", "customer_price", "customer_email", "order_customer_price"}, universal...)
			for _, c := range []struct {
				name    string
				records []proto.Message
				want    []string
			}{
				{"one type", []proto.Message{order(1, 10), order(2, 20)}, orderIndexes},
				{"other type", []proto.Message{customer(1, 10)}, customerIndexes},
				{"only universal indexes", []proto.Message{typed(1)}, universal},
				{"type with universal-only type", []proto.Message{typed(1), order(2, 20)}, orderIndexes},
				{"two types", []proto.Message{order(1, 10), customer(2, 20)}, allIndexes},
			} {
				caseRoot := root.Sub(c.name)
				commit(md, caseRoot, legacy, func(*FDBRecordStore) error { return nil })
				rc := transaction()
				probe := &batchConflictProbeTransaction{WritableTransaction: rc.Transaction()}
				rc.tx = probe
				s := open(rc, md, caseRoot, legacy)
				probe.added = nil
				_, err := s.SaveRecordBatchWithOptions(c.records, options)
				Expect(err).NotTo(HaveOccurred(), c.name)
				Expect(probe.added).To(Equal(expected(s, md, c.want...)), c.name)
				Expect(rc.Commit()).To(Succeed(), c.name)
			}

			// A primary key reused by another type removes the old record's
			// entries from the old type's indexes, so those are covered too.
			overwriteRoot := root.Sub("cross-type overwrite")
			commit(md, overwriteRoot, legacy, func(s *FDBRecordStore) error {
				_, err := s.SaveRecord(customer(7, 70))
				return err
			})
			rc := transaction()
			probe := &batchConflictProbeTransaction{WritableTransaction: rc.Transaction()}
			rc.tx = probe
			s := open(rc, md, overwriteRoot, legacy)
			probe.added = nil
			_, err := s.SaveRecordBatchWithOptions([]proto.Message{order(7, 71)}, options)
			Expect(err).NotTo(HaveOccurred())
			// Overwrite clears add normal old-record conflicts before coalescing (Java SplitHelper.java:357-359).
			// Bare-key unsplit records with size info skip that clear, so legacy layout adds none.
			var want []fdb.KeyRange
			if !legacy {
				begin, end := overwriteRoot.Sub(RecordKey, int64(7)).FDBRangeKeys()
				want = append(want, fdb.KeyRange{Begin: begin, End: end})
			}
			want = append(want, expected(s, md, allIndexes...)...)
			Expect(probe.added).To(Equal(want))
			rows, err := rc.Transaction().GetRange(s.IndexSubspace(md.GetIndex("customer_price")), fdb.RangeOptions{}).GetSliceWithError()
			Expect(err).NotTo(HaveOccurred())
			Expect(rows).To(BeEmpty(), "the overwrite did write the old type's index")
			Expect(rc.Commit()).To(Succeed())
		})
	}

	It("skips disabled indexes and covers write-only ones", func() {
		md, root := metadata(false), specSubspace()
		commit(md, root, false, func(s *FDBRecordStore) error {
			if _, err := s.MarkIndexDisabled("order_price"); err != nil {
				return err
			}
			_, err := s.MarkIndexWriteOnly("order_customer_price")
			return err
		})
		rc := transaction()
		probe := &batchConflictProbeTransaction{WritableTransaction: rc.Transaction()}
		rc.tx = probe
		s := open(rc, md, root, false)
		probe.added = nil
		_, err := s.SaveRecordBatchWithOptions([]proto.Message{order(1, 10)}, options)
		Expect(err).NotTo(HaveOccurred())
		Expect(probe.added).To(Equal(expected(s, md, "order_customer_price", "record_count", "versions")))
	})

	It("conflicts with concurrent readers of the batch's indexes only", func() {
		md := metadata(false)
		for _, coalesce := range []bool{false, true} {
			root := specSubspace().Sub(coalesce)
			commit(md, root, false, func(*FDBRecordStore) error { return nil })
			type reader struct {
				name      string
				ranges    func(*FDBRecordStore) []fdb.KeyRange
				conflicts bool
			}
			whole := func(names ...string) func(*FDBRecordStore) []fdb.KeyRange {
				return func(s *FDBRecordStore) []fdb.KeyRange { return expected(s, md, names...) }
			}
			readers := []reader{
				// The batch writes price 5; this reader's range holds no
				// entry the batch changes, so only the widened range reaches it.
				{"untouched part of the type's own index", func(s *FDBRecordStore) []fdb.KeyRange {
					ss := s.IndexSubspace(md.GetIndex("order_price"))
					return []fdb.KeyRange{{Begin: ss.Pack(tuple.Tuple{int64(1000)}), End: ss.Pack(tuple.Tuple{int64(2000)})}}
				}, coalesce},
				{"the type's own index", whole("order_price"), true},
				{"multi-type index", whole("order_customer_price"), true},
				{"universal COUNT index", whole("record_count"), true},
				// A versionstamped index key has no client conflict range; the
				// commit proxy adds its final key's (CommitProxyServer.actor.cpp:211-213).
				{"universal VERSION index", whole("versions"), true},
				{"other type's indexes", whole("customer_price", "customer_email"), false},
			}
			contexts := make([]*FDBRecordContext, len(readers))
			for i, r := range readers {
				contexts[i] = transaction()
				s := open(contexts[i], md, root, false)
				for _, kr := range r.ranges(s) {
					_, err := contexts[i].Transaction().GetRange(kr, fdb.RangeOptions{}).GetSliceWithError()
					Expect(err).NotTo(HaveOccurred())
				}
				contexts[i].Transaction().Set(root.Sub("reader", i).FDBKey(), []byte(r.name))
			}
			writer := transaction()
			ws := open(writer, md, root, false)
			opt := BatchSaveOptions{CoalesceIndexWriteConflicts: coalesce}
			_, err := ws.SaveRecordBatchWithOptions([]proto.Message{order(1, 5), order(2, 5)}, opt)
			Expect(err).NotTo(HaveOccurred())
			Expect(writer.Commit()).To(Succeed())
			for i, r := range readers {
				err := contexts[i].Commit()
				if !r.conflicts {
					Expect(err).NotTo(HaveOccurred(), "coalesce=%v reader of %s", coalesce, r.name)
					continue
				}
				var conflict fdb.Error
				Expect(errors.As(err, &conflict)).To(BeTrue(), "coalesce=%v reader of %s: %v", coalesce, r.name, err)
				Expect(conflict.Code).To(Equal(1020))
			}
		}
	})
})
