package recordlayer

import (
	"context"
	"errors"
	"fmt"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"github.com/google/uuid"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"
)

var _ = Describe("Indexing heartbeat administration", func() {
	var (
		ctx context.Context
		md  *RecordMetaData
		idx *Index
	)

	BeforeEach(func() {
		ctx = context.Background()
		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		builder.AddIndex("Order", NewIndex("hb_admin_price", Field("price")))
		var err error
		md, err = builder.Build()
		Expect(err).NotTo(HaveOccurred())
		idx = md.GetIndex("hb_admin_price")
	})

	// put writes one heartbeat row with the given value under a UUID key.
	put := func(ss subspace.Subspace, id uuid.UUID, value []byte) {
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			rtx.Transaction().Set(heartbeatSubspace(ss, idx).Pack(tuple.Tuple{tuple.UUID(id)}), value)
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	}
	beat := func(info string, atMs int64) []byte {
		data, err := proto.Marshal(&gen.IndexBuildHeartbeat{
			Info: proto.String(info), CreateTimeMilliseconds: proto.Int64(atMs - 5), HeartbeatTimeMilliseconds: proto.Int64(atMs),
		})
		Expect(err).NotTo(HaveOccurred())
		return data
	}
	ids := func(n int) []uuid.UUID {
		out := make([]uuid.UUID, n)
		for i := range out {
			out[i] = uuid.UUID{0x40, byte(i + 1)} // ascending key order
		}
		return out
	}

	It("reads every heartbeat, honours maxCount and reports invalid values", func() {
		ss := specSubspace()
		id := ids(3)
		put(ss, id[0], beat("a", 1_000))
		put(ss, id[1], []byte{0xff})
		put(ss, id[2], beat("c", 3_000))
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			all, err := GetIndexingHeartbeats(rtx.Transaction(), ss, idx, 0)
			Expect(err).NotTo(HaveOccurred())
			Expect(all).To(HaveLen(3))
			Expect(all[id[0]].GetInfo()).To(Equal("a"))
			Expect(all[id[0]].GetHeartbeatTimeMilliseconds()).To(Equal(int64(1_000)))
			Expect(all[id[1]].GetInfo()).To(Equal(InvalidHeartbeatInfo))
			Expect(all[id[1]].GetCreateTimeMilliseconds()).To(BeZero())
			Expect(all[id[1]].GetHeartbeatTimeMilliseconds()).To(BeZero())
			// Java's valve: maxCount < ++iterationCount stops, so at most maxCount rows
			// in key order are read.
			two, err := GetIndexingHeartbeats(rtx.Transaction(), ss, idx, 2)
			Expect(err).NotTo(HaveOccurred())
			Expect(two).To(HaveLen(2))
			Expect(two).To(HaveKey(id[0]))
			Expect(two).To(HaveKey(id[1]))
			negative, err := GetIndexingHeartbeats(rtx.Transaction(), ss, idx, -1)
			Expect(err).NotTo(HaveOccurred())
			Expect(negative).To(HaveLen(3), "a non-positive maxCount reads everything, as in Java")
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("refuses a non-UUID heartbeat key when reading, as Java's getUUID does", func() {
		ss := specSubspace()
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			rtx.Transaction().Set(heartbeatSubspace(ss, idx).Pack(tuple.Tuple{"legacy-id"}), beat("legacy", 1))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			return GetIndexingHeartbeats(rtx.Transaction(), ss, idx, 0)
		})
		var keyErr *IndexingHeartbeatKeyError
		Expect(errors.As(err, &keyErr)).To(BeTrue())
	})

	It("clears old and invalid heartbeats at the exact age boundary and honours maxIteration", func() {
		ss := specSubspace()
		id := ids(4)
		const now, minAge = int64(100_000), int64(10_000)
		put(ss, id[0], beat("exactly-min-age", now-minAge))  // now >= hb + minAge: cleared
		put(ss, id[1], beat("one-ms-younger", now-minAge+1)) // kept
		put(ss, id[2], []byte{0xff})                         // invalid: cleared
		put(ss, id[3], beat("ancient", 0))                   // cleared, unless the valve stops first
		// Valve first: only the first two rows in key order are examined.
		n, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			return ClearIndexingHeartbeats(rtx.Transaction(), ss, idx, minAge, 2, now)
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(n).To(Equal(1))
		n, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			return ClearIndexingHeartbeats(rtx.Transaction(), ss, idx, minAge, 0, now)
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(n).To(Equal(2), "the invalid and the ancient heartbeat")
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			left, err := GetIndexingHeartbeats(rtx.Transaction(), ss, idx, 0)
			Expect(err).NotTo(HaveOccurred())
			Expect(left).To(HaveLen(1))
			Expect(left).To(HaveKey(id[1]))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("clears a legacy string-keyed heartbeat without parsing its key, as Java does", func() {
		ss := specSubspace()
		legacy := heartbeatSubspace(ss, idx).Pack(tuple.Tuple{"legacy-id"})
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			rtx.Transaction().Set(legacy, beat("legacy", 0))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
		n, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			return ClearIndexingHeartbeats(rtx.Transaction(), ss, idx, 1, 0, 10)
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(n).To(Equal(1))
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			rows, err := rtx.Transaction().GetRange(heartbeatSubspace(ss, idx), fdb.RangeOptions{}).GetSliceWithError()
			Expect(rows).To(BeEmpty())
			return nil, err
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("reports an ongoing build by admission's predicate over each indexer's surviving heartbeat", func() {
		const now, lease = int64(100_000_000), int64(10_000)
		for _, tc := range []struct {
			ageMs int64
			want  bool
		}{
			{0, true},
			{lease - 1, true},
			{lease, false},            // age == lease: not younger than the lease
			{3_600_000, false},        // a crashed session's heartbeat; Java's arithmetic says true
			{-5_000, true},            // dated slightly in the future (skew)
			{-3_600_000, true},        // an hour ahead: admission refuses; Java's arithmetic says false
			{-86_399_999, true},       // just inside the one-day skew tolerance
			{-86_400_000, false},      // a day ahead: bad data to admission and to this check
			{-10 * 86_400_000, false}, // far future: bad data
		} {
			ss := specSubspace().Sub(tc.ageMs)
			put(ss, uuid.New(), beat("peer", now-tc.ageMs))
			got, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				return CheckAnyOngoingOnlineIndexBuilds(rtx.Transaction(), ss, idx, lease, now)
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(got).To(Equal(tc.want), "age=%d", tc.ageMs)
		}
		// An invalid-only heartbeat is no session: admission ignores it (Java logs "Bad
		// indexing heartbeat item", IndexingHeartbeat.java:118-123). Java's ongoing check
		// reads its time as 0 and says true.
		invalid := specSubspace().Sub("invalid")
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			rtx.Transaction().Set(heartbeatSubspace(invalid, idx).Pack(tuple.Tuple{tuple.UUID(uuid.New())}), []byte{0xff})
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
		gotInvalid, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			return CheckAnyOngoingOnlineIndexBuilds(rtx.Transaction(), invalid, idx, lease, now)
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(gotInvalid).To(BeFalse(), "an unparseable heartbeat is no session")
		empty := specSubspace().Sub("empty")
		got, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			return CheckAnyOngoingOnlineIndexBuilds(rtx.Transaction(), empty, idx, lease, now)
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(got).To(BeFalse(), "no heartbeat, no build")

		// A heartbeat key whose first element is not a UUID is an
		// IndexingHeartbeatKeyError, as in Java (getUUID(0) outside the try,
		// IndexingHeartbeat.java:148) and as in admission, which refuses every session
		// over it: even beside a LIVE UUID heartbeat, and whether the bad key sorts
		// before or after it. Tuple type codes order the range: a byte string (0x01)
		// and a string (0x02) sort BEFORE a UUID (0x30), a versionstamp (0x33) AFTER
		// it, so the last key is the one a parse that stopped at the first live
		// heartbeat would never reach.
		for _, legacy := range []tuple.TupleElement{
			"legacy-builder",
			[]byte{0xff, 0xff},
			tuple.Versionstamp{TransactionVersion: [10]byte{0xff}, UserVersion: 7},
		} {
			mixed := specSubspace().Sub("legacy", fmt.Sprint(legacy))
			put(mixed, uuid.New(), beat("live", now))
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				rtx.Transaction().Set(heartbeatSubspace(mixed, idx).Pack(tuple.Tuple{legacy}), beat("old", now))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				return CheckAnyOngoingOnlineIndexBuilds(rtx.Transaction(), mixed, idx, lease, now)
			})
			var keyErr *IndexingHeartbeatKeyError
			Expect(errors.As(err, &keyErr)).To(BeTrue(), "a non-UUID heartbeat key (%v) is an error, got %v", legacy, err)
			Expect(keyErr.IndexName).To(Equal(idx.Name))
		}

		// Java reads only element 0 (getUUID(0)), so a (UUID, x) key IS that UUID's
		// heartbeat: a live one reports an ongoing build, and nothing errors.
		trailing := specSubspace().Sub("trailing")
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			rtx.Transaction().Set(heartbeatSubspace(trailing, idx).Pack(tuple.Tuple{tuple.UUID(uuid.New()), "extra"}), beat("live", now))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
		gotTrailing, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			return CheckAnyOngoingOnlineIndexBuilds(rtx.Transaction(), trailing, idx, lease, now)
		})
		Expect(err).NotTo(HaveOccurred(), "a (UUID, x) heartbeat key parses as its UUID, as Java's getUUID(0) does")
		Expect(gotTrailing).To(BeTrue(), "a live (UUID, x)-keyed heartbeat is an ongoing build")

		// (U) and (U, x) name ONE indexer, and Java's map keeps the later key's value:
		// the (U, x) heartbeat, which sorts after (U). The check judges that value only,
		// so a live (U) beside a (U, x) a day ahead is not ongoing, and a (U) a day
		// ahead beside a live (U, x) is.
		collapse := func(name string, bare, suffixed []byte) bool {
			sub := specSubspace().Sub("collapse", name)
			id := tuple.UUID(uuid.New())
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				rtx.Transaction().Set(heartbeatSubspace(sub, idx).Pack(tuple.Tuple{id}), bare)
				rtx.Transaction().Set(heartbeatSubspace(sub, idx).Pack(tuple.Tuple{id, "extra"}), suffixed)
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
			got, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				return CheckAnyOngoingOnlineIndexBuilds(rtx.Transaction(), sub, idx, lease, now)
			})
			Expect(err).NotTo(HaveOccurred())
			return got.(bool)
		}
		dayAhead := now + 2*86_400_000
		Expect(collapse("live-then-future", beat("live", now), beat("future", dayAhead))).To(BeFalse(),
			"the (U, x) value replaces the (U) value, as Java's HashMap.put does")
		Expect(collapse("future-then-live", beat("future", dayAhead), beat("live", now))).To(BeTrue(),
			"the surviving (U, x) value is live")
		// A live (U) beside an unparseable (U, x): the surviving value is the invalid
		// placeholder, which is no session (the invalid-only population; Java reads its
		// time as 0 and says ongoing). Admission still refuses a new session over the
		// live (U), which is where "ongoing" and "would be refused" part.
		Expect(collapse("live-then-invalid", beat("live", now), []byte{0xff})).To(BeFalse(),
			"the surviving (U, x) value does not parse")
		Expect(collapse("invalid-then-live", []byte{0xff}, beat("live", now))).To(BeTrue(),
			"the surviving (U, x) value is live")
	})

	It("drives the OnlineIndexer wrappers through a real store on the primary target index", func() {
		ss := specSubspace()
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			_, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).CreateOrOpen()
			return nil, err
		})
		Expect(err).NotTo(HaveOccurred())
		oi, err := NewOnlineIndexerBuilder().SetDatabase(sharedDB).SetMetaData(md).SetIndex(idx).SetSubspace(ss).Build()
		Expect(err).NotTo(HaveOccurred())

		ongoing, err := oi.CheckAnyOngoingOnlineIndexBuilds(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(ongoing).To(BeFalse())

		live := NewIndexingHeartbeat("live session", defaultLeaseLengthMs, false, nil)
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			return nil, live.CheckAndUpdate(rtx.Transaction(), ss, idx)
		})
		Expect(err).NotTo(HaveOccurred())
		stale := uuid.UUID{0x40, 0xee}
		put(ss, stale, beat("crashed session", 1))

		all, err := oi.GetIndexingHeartbeats(ctx, 0)
		Expect(err).NotTo(HaveOccurred())
		Expect(all).To(HaveLen(2))
		Expect(all[live.indexerID].GetInfo()).To(Equal("live session"))
		ongoing, err = oi.CheckAnyOngoingOnlineIndexBuilds(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(ongoing).To(BeTrue())
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).Open()
			if err != nil {
				return nil, err
			}
			ok, err := CheckAnyOngoingOnlineIndexBuildsForStore(store, idx)
			Expect(ok).To(BeTrue())
			return nil, err
		})
		Expect(err).NotTo(HaveOccurred())

		cleared, err := oi.ClearIndexingHeartbeats(ctx, defaultLeaseLengthMs, 0)
		Expect(err).NotTo(HaveOccurred())
		Expect(cleared).To(Equal(1), "only the crashed session's heartbeat is older than the lease")
		all, err = oi.GetIndexingHeartbeats(ctx, 0)
		Expect(err).NotTo(HaveOccurred())
		Expect(all).To(HaveLen(1))
		Expect(all).To(HaveKey(live.indexerID))
	})
})
