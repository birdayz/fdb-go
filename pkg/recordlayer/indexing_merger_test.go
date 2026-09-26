package recordlayer

import (
	"context"
	"errors"
	"testing"

	"fdb.dev/pkg/fdbgo/fdb"
	"github.com/google/uuid"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestIndexingMergerFeedback(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name         string
		step         DeferredMaintenanceStep
		tried, quota int64
		docs         int
		code         int
		abort        bool
		limit, time  int64
		wantDocs     int
	}{
		{name: "none aborts", code: 1020, abort: true},
		{name: "batch throttled aborts", step: DeferredMaintenanceMerge, tried: 8, code: 1051, abort: true},
		{name: "tag throttled aborts", step: DeferredMaintenanceMerge, tried: 8, code: 1213, abort: true},
		{name: "cluster file missing aborts", step: DeferredMaintenanceMerge, tried: 8, code: 1515, abort: true},
		{name: "multi merge halves tried", step: DeferredMaintenanceMerge, tried: 9, code: 1020, limit: 4},
		{name: "single merge halves time", step: DeferredMaintenanceMerge, tried: 1, quota: 4000, code: 1020, time: 2000},
		{name: "single merge time floor", step: DeferredMaintenanceMerge, tried: 1, quota: 2, code: 1020, abort: true, time: 2},
		{name: "repartition halves", step: DeferredMaintenanceRepartition, docs: 9, code: 1020, wantDocs: 4},
		{name: "repartition skips", step: DeferredMaintenanceRepartition, docs: 1, code: 1020, wantDocs: -1},
		{name: "repartition skip fails", step: DeferredMaintenanceRepartition, docs: -1, code: 1020, abort: true, wantDocs: -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			control := &IndexDeferredMaintenanceControl{}
			control.SetLastStep(tc.step)
			control.SetMergesTried(tc.tried)
			control.SetTimeQuotaMillis(tc.quota)
			control.SetRepartitionDocumentCount(tc.docs)
			m := &indexingMerger{}
			err := m.handleFailure(control, fdb.Error{Code: tc.code})
			if (err != nil) != tc.abort || m.mergesLimit != tc.limit || m.timeQuotaMillis != tc.time || m.repartitionDocumentCount != tc.wantDocs {
				t.Fatalf("err=%v state=%+v want abort=%v limit=%d time=%d docs=%d", err, m, tc.abort, tc.limit, tc.time, tc.wantDocs)
			}
			if control.GetTotalMerges() != 0 {
				t.Fatal("failed merges remain in completed count")
			}
		})
	}
	t.Run("non FDB error aborts but timeout adapts", func(t *testing.T) {
		t.Parallel()
		c := &IndexDeferredMaintenanceControl{}
		c.SetLastStep(DeferredMaintenanceMerge)
		c.SetMergesTried(4)
		m := &indexingMerger{}
		failure := &RecordCoreError{Message: "invalid payload"}
		if !errors.Is(m.handleFailure(c, failure), failure) {
			t.Fatal("lost permanent error")
		}
		if err := m.handleFailure(c, context.DeadlineExceeded); err != nil || m.mergesLimit != 2 {
			t.Fatalf("timeout feedback: %v %+v", err, m)
		}
	})
	t.Run("success budgets and repartition second chances", func(t *testing.T) {
		t.Parallel()
		c := &IndexDeferredMaintenanceControl{}
		m := &indexingMerger{mergesLimit: 8, timeQuotaMillis: 100, repartitionDocumentCount: 20}
		for i := 0; i < 4; i++ {
			if m.handleSuccess(c) {
				t.Fatal("empty success must finish")
			}
		}
		if m.mergesLimit != 10 || m.successes != 1 || m.timeQuotaMillis != 0 || m.repartitionDocumentCount != 0 {
			t.Fatalf("bad success feedback: %+v", m)
		}
		m.repartitionDocumentCount = -1
		if !m.handleSuccess(c) || m.repartitionSecondChances != 1 || m.repartitionDocumentCount != 0 {
			t.Fatal("missing second chance")
		}
		m.repartitionDocumentCount = -1
		if m.handleSuccess(c) || m.repartitionSecondChances != 0 {
			t.Fatal("unbounded second chance")
		}
		c.SetRepartitionCapped(true)
		// Java grants the renewed second chance before examining the cap.
		if !m.handleSuccess(c) || !c.RepartitionCapped() || m.repartitionDocumentCount != 0 {
			t.Fatal("second-chance precedence differs from Java")
		}
		if !m.handleSuccess(c) || c.RepartitionCapped() {
			t.Fatal("capped repartition must resume")
		}
		c.SetMergesFound(2)
		c.SetMergesTried(1)
		if !m.handleSuccess(c) {
			t.Fatal("untried merges lost")
		}
	})
}

func TestIndexDeferredMaintenanceControl(t *testing.T) {
	t.Parallel()
	c := &IndexDeferredMaintenanceControl{}
	if c.ShouldAutoMergeDuringCommit() || c.IsExplicitMergePath() || c.GetMergeRequiredIndexes() != nil || c.GetPreCommitCallback() != nil || c.GetMergeSessionID() != nil || c.GetLastStep() != DeferredMaintenanceNone {
		t.Fatal("incorrect zero-value defaults")
	}
	c.SetAutoMergeDuringCommit(true)
	c.SetExplicitMergePath(true)
	c.SetMergesLimit(7)
	c.SetMergesFound(8)
	c.SetTimeQuotaMillis(9)
	c.SetSizeQuotaBytes(10)
	c.SetRepartitionDocumentCount(11)
	c.SetRepartitionCapped(true)
	if !c.ShouldAutoMergeDuringCommit() || !c.IsExplicitMergePath() || c.GetMergesLimit() != 7 || c.GetMergesFound() != 8 || c.GetTimeQuotaMillis() != 9 || c.GetSizeQuotaBytes() != 10 || c.GetRepartitionDocumentCount() != 11 || !c.RepartitionCapped() {
		t.Fatal("control values lost")
	}
	c.SetMergesTried(3)
	c.SetMergesTried(5)
	c.MergeHadFailed()
	if c.GetMergesTried() != 5 || c.GetTotalMerges() != 3 {
		t.Fatal("Java cumulative tried/failed accounting differs")
	}
	c.MergeHadFailed()
	if c.GetTotalMerges() != 3 {
		t.Fatal("failure underflow")
	}
	id := uuid.MustParse("00000000-0000-0000-0000-000000000001")
	original := id
	c.SetMergeSessionID(&id)
	id = uuid.Nil
	got := c.GetMergeSessionID()
	if got == nil || *got != original {
		t.Fatal("session ID aliases caller")
	}
	*got = uuid.Nil
	if *c.GetMergeSessionID() != original {
		t.Fatal("session getter exposes mutable storage")
	}
	c.SetMergeSessionID(nil)
	if c.GetMergeSessionID() != nil {
		t.Fatal("session not reset")
	}
	index := NewIndex("one", Field("price"))
	if err := c.SetMergeRequiredIndexes(nil); err == nil {
		t.Fatal("nil merge request must fail explicitly")
	}
	if err := c.SetMergeRequiredIndexes(index); err != nil {
		t.Fatal(err)
	}
	if err := c.SetMergeRequiredIndexes(index); err != nil {
		t.Fatal(err)
	}
	list := c.GetMergeRequiredIndexes()
	if len(list) != 1 || list[0] != index {
		t.Fatal("merge request not set-like")
	}
	list[0] = nil
	if c.GetMergeRequiredIndexes()[0] != index {
		t.Fatal("request snapshot aliases control")
	}
}

var _ = Describe("Deferred maintenance transaction contract", func() {
	It("registers callbacks on actual transactions and preserves reentrant access", func() {
		root := specSubspace()
		ctx := context.Background()
		builder := baseBuilder()
		index := NewIndex("target", Field("price"))
		builder.AddIndex("Order", index)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())
		_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().SetContext(rc).SetMetaDataProvider(md).SetSubspace(root).Create()
			if err != nil {
				return nil, err
			}
			control := store.GetIndexDeferredMaintenanceControl()
			control.SetPreCommitCallback(func(actual *FDBRecordStore) error {
				control.SetMergesFound(1)
				actual.context.Transaction().Set(fdb.Key(root.Sub("callback").Bytes()), []byte("committed"))
				return nil
			})
			control.RegisterPreCommit(store, index)
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
		_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
			value, err := rc.Transaction().Get(fdb.Key(root.Sub("callback").Bytes())).Get()
			Expect(value).To(Equal([]byte("committed")))
			return nil, err
		})
		Expect(err).NotTo(HaveOccurred())
	})
	// A standalone MergeIndexes holds a session (DIVERGENCES.md, "OnlineIndexer
	// session start and build catcher: where Go differs"): Java's standalone
	// merge has no heartbeat (IndexingBase.java:969-972, :1085-1096), so it
	// writes none, checks no state and proceeds under any peer. Go's writes its
	// heartbeat over a WRITE_ONLY target and clears it afterwards, is refused by
	// a live peer, fails over a DISABLED target, and skips a READABLE one.
	Describe("a standalone MergeIndexes session", func() {
		type mergeCase struct {
			ctx   context.Context
			root  subspaceAndMeta
			index *Index
		}
		setupMerge := func(state IndexState) mergeCase {
			ctx := context.Background()
			root := specSubspace()
			builder := baseBuilder()
			index := NewIndex("Order$merge_price", Field("price"))
			builder.AddIndex("Order", index)
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())
			_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().SetContext(rc).SetMetaDataProvider(md).SetSubspace(root).Create()
				if err != nil {
					return nil, err
				}
				store.setIndexState(index.Name, state)
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
			return mergeCase{ctx: ctx, root: subspaceAndMeta{ks: root, md: md}, index: md.GetIndex(index.Name)}
		}
		heartbeats := func(c mergeCase) []string {
			var infos []string
			_, err := sharedDB.Run(c.ctx, func(rc *FDBRecordContext) (any, error) {
				hbs, _, err := ReadHeartbeats(rc.Transaction(), c.root.ks, c.index)
				for _, hb := range hbs {
					infos = append(infos, hb.GetInfo())
				}
				return nil, err
			})
			Expect(err).NotTo(HaveOccurred())
			return infos
		}
		merger := func(db *FDBDatabase, c mergeCase) *OnlineIndexer {
			oi, err := NewOnlineIndexerBuilder().SetDatabase(db).SetMetaData(c.root.md).
				SetIndex(c.index).SetSubspace(c.root.ks).Build()
			Expect(err).NotTo(HaveOccurred())
			return oi
		}

		It("writes its heartbeat over a WRITE_ONLY target for the merge and clears it", func() {
			c := setupMerge(IndexStateWriteOnly)
			// After every merge transaction commits, record the heartbeats a peer
			// (a Java builder among them) would then read.
			var seen [][]string
			h := &afterTransactHook{Transactor: sharedDB.transactor, after: func() {
				seen = append(seen, heartbeats(c))
			}}
			oi := merger(NewFDBDatabaseWithTransactor(h, sharedDB.db), c)
			Expect(oi.MergeIndexes(c.ctx)).To(Succeed())
			Expect(seen).NotTo(BeEmpty(), "the merge ran no transaction")
			Expect(seen[0]).To(Equal([]string{"explicit index merge"}),
				"the merge's transaction committed no heartbeat of its own")
			Expect(heartbeats(c)).To(BeEmpty(), "the merge left its heartbeat behind")
		})

		It("is refused by a live peer over a WRITE_ONLY target, which Java's merge would ignore", func() {
			c := setupMerge(IndexStateWriteOnly)
			peer := NewIndexingHeartbeat("PEER", 60_000, false, nil)
			_, err := sharedDB.Run(c.ctx, func(rc *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().SetContext(rc).SetMetaDataProvider(c.root.md).SetSubspace(c.root.ks).Open()
				if err != nil {
					return nil, err
				}
				return nil, peer.CheckAndUpdate(rc.Transaction(), store.subspace, c.index)
			})
			Expect(err).NotTo(HaveOccurred())
			oi := merger(sharedDB, c)
			err = oi.MergeIndexes(c.ctx)
			var lockErr *SynchronizedSessionLockedError
			Expect(errors.As(err, &lockErr)).To(BeTrue(), "error: %v", err)
			Expect(lockErr.ExistingInfo).To(Equal("PEER"))
			Expect(heartbeats(c)).To(Equal([]string{"PEER"}), "the refused merge touched the peer's heartbeat or left its own")
		})

		// A mutual builder is not refused by the merge's heartbeat: its check
		// under allowMutual writes only its own key (IndexingHeartbeat.java:
		// 88-93). The merge's own check is exclusive, so a merge transaction that
		// runs while the builder's heartbeat is live fails, and the builder keeps
		// its heartbeat. A VALUE target's merge is one transaction, so the
		// builder is admitted after that transaction and the merge completes;
		// the next merge transaction, the next merge's here, is the one that
		// fails.
		It("admits a mutual builder beside the merge, and a merge transaction after it fails, not the builder", func() {
			c := setupMerge(IndexStateWriteOnly)
			mutual := NewIndexingHeartbeat("MUTUAL", 60_000, true, nil)
			transactions := 0
			var mutualErr error
			var seenAtAdmission []string
			h := &afterTransactHook{Transactor: sharedDB.transactor, after: func() {
				transactions++
				if transactions != 1 {
					return
				}
				seenAtAdmission = heartbeats(c)
				_, mutualErr = sharedDB.Run(c.ctx, func(rc *FDBRecordContext) (any, error) {
					store, err := NewStoreBuilder().SetContext(rc).SetMetaDataProvider(c.root.md).SetSubspace(c.root.ks).Open()
					if err != nil {
						return nil, err
					}
					return nil, mutual.CheckAndUpdate(rc.Transaction(), store.subspace, c.index)
				})
			}}
			Expect(merger(NewFDBDatabaseWithTransactor(h, sharedDB.db), c).MergeIndexes(c.ctx)).To(Succeed())
			Expect(transactions).To(Equal(1), "a VALUE target's merge ran another number of transactions")
			Expect(seenAtAdmission).To(Equal([]string{"explicit index merge"}), "the builder did not start beside the merge's live heartbeat")
			Expect(mutualErr).NotTo(HaveOccurred(), "the merge's heartbeat refused the mutual builder")

			err := merger(sharedDB, c).MergeIndexes(c.ctx)
			var lockErr *SynchronizedSessionLockedError
			Expect(errors.As(err, &lockErr)).To(BeTrue(), "a merge transaction beside the mutual builder: %v", err)
			Expect(lockErr.ExistingInfo).To(Equal("MUTUAL"))
			Expect(heartbeats(c)).To(Equal([]string{"MUTUAL"}), "the failed merge touched the builder's heartbeat or left its own")
		})

		It("fails over a DISABLED target, where Java's merge proceeds", func() {
			c := setupMerge(IndexStateDisabled)
			oi := merger(sharedDB, c)
			err := oi.MergeIndexes(c.ctx)
			var storageErr *RecordCoreStorageError
			Expect(errors.As(err, &storageErr)).To(BeTrue(), "error: %v", err)
			Expect(storageErr.Message).To(Equal("Unexpected index state(s)"))
			Expect(storageErr.IndexName).To(Equal(c.index.Name))
			Expect(heartbeats(c)).To(BeEmpty())
		})

		It("merges a READABLE target without a heartbeat", func() {
			c := setupMerge(IndexStateReadable)
			var seen [][]string
			h := &afterTransactHook{Transactor: sharedDB.transactor, after: func() {
				seen = append(seen, heartbeats(c))
			}}
			oi := merger(NewFDBDatabaseWithTransactor(h, sharedDB.db), c)
			Expect(oi.MergeIndexes(c.ctx)).To(Succeed())
			Expect(seen).NotTo(BeEmpty())
			for _, s := range seen {
				Expect(s).To(BeEmpty())
			}
			Expect(heartbeats(c)).To(BeEmpty())
		})
	})

	It("explicitly merges HNSW and sliding HNSW without inventing deferred work", func() {
		root := specSubspace()
		builder := baseBuilder()
		index := NewVectorIndex("vector", KeyWithValue(Concat(Field("quantity"), Field("price")), 1), 1)
		builder.AddIndex("Order", index)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())
		_, err = sharedDB.Run(context.Background(), func(rc *FDBRecordContext) (any, error) {
			_, err := NewStoreBuilder().SetContext(rc).SetMetaDataProvider(md).SetSubspace(root).Create()
			return nil, err
		})
		Expect(err).NotTo(HaveOccurred())
		oi := &OnlineIndexer{db: sharedDB, metaData: md, subspace: root, targetIndexes: []*Index{index}, leaseLengthMs: 30000}
		Expect(oi.MergeIndexes(context.Background())).To(Succeed())
		Expect(oi.mergers[index.Name].successes).To(Equal(1))
		Expect(oi.sessionHeartbeat).To(BeNil())
		_, err = sharedDB.Run(context.Background(), func(rc *FDBRecordContext) (any, error) {
			store, err := oi.openStore(rc)
			if err != nil {
				return nil, err
			}
			maintainer, err := store.getIndexMaintainer(index)
			if err != nil {
				return nil, err
			}
			sliding := &slidingWindowIndexMaintainer{delegate: maintainer}
			Expect(sliding.MergeIndex()).To(Succeed())
			Expect(store.GetIndexDeferredMaintenanceControl().GetMergeRequiredIndexes()).To(BeNil())
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})
})

// afterTransactHook runs after each Transact call returns, so a test can
// read what a transaction committed before the caller's next one starts.
type afterTransactHook struct {
	fdb.Transactor
	after func()
}

func (h *afterTransactHook) Transact(fn func(fdb.WritableTransaction) (any, error)) (any, error) {
	r, err := h.Transactor.Transact(fn)
	h.after()
	return r, err
}

func FuzzIndexingMergerFeedback(f *testing.F) {
	f.Add(uint16(10), uint16(4000), uint8(2))
	f.Add(uint16(1), uint16(2), uint8(1))
	f.Fuzz(func(t *testing.T, tried, quota uint16, step uint8) {
		c := &IndexDeferredMaintenanceControl{}
		c.SetMergesTried(int64(tried))
		c.SetTimeQuotaMillis(int64(quota))
		c.SetLastStep(DeferredMaintenanceStep(step % 3))
		c.SetRepartitionDocumentCount(int(tried) - 1)
		m := &indexingMerger{}
		err := m.handleFailure(c, fdb.Error{Code: 1020})
		if c.GetTotalMerges() != 0 {
			t.Fatal("failed work counted as completed")
		}
		if err == nil && c.GetLastStep() == DeferredMaintenanceMerge && tried >= 2 && m.mergesLimit != int64(tried)/2 {
			t.Fatal("multi merge budget not halved")
		}
		if err == nil && c.GetLastStep() == DeferredMaintenanceMerge && tried < 2 && m.timeQuotaMillis != int64(quota)/2 {
			t.Fatal("single merge quota not halved")
		}
		if c.GetLastStep() == DeferredMaintenanceNone && err == nil {
			t.Fatal("unknown failing phase retried")
		}
	})
}
