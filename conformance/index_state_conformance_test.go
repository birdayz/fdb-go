//go:build bazelrunfiles

package conformance_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
	"github.com/google/uuid"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"
)

var _ = Describe("Index State Persistence Conformance", func() {
	var (
		ctx   context.Context
		env   *TenantEnvironment
		store *IndexStateConformanceStore
	)

	const indexName = "Order$price"

	BeforeEach(func() {
		ctx = context.Background()

		tenantName := fmt.Sprintf("idxstate_%s", uuid.New().String())

		var err error
		env, err = SetupTenantEnvironment(ctx, sharedContainer, tenantName)
		Expect(err).NotTo(HaveOccurred())

		store, err = NewIndexStateConformanceStore(env.RecordDB, env.Keyspace, env.ClusterFile, env.TenantName)
		Expect(err).NotTo(HaveOccurred())

		// Create the store first so both sides can open it
		err = store.CreateStoreGo(ctx)
		Expect(err).NotTo(HaveOccurred())
	})

	AfterEach(func() {
		if env != nil {
			_ = env.Cleanup(ctx)
		}
	})

	It("pins Java rejection of mixed resumed queue states and mutual queue takeover", func() {
		for _, tc := range []struct {
			mutual, queuedPrimary bool
			message               string
		}{
			{false, false, "A target index state doesn't match the primary index state"},
			{false, true, "A target index state doesn't match the primary index state"},
			{true, true, "Mutual indexing cannot continue a pending write queue index build"},
		} {
			var result struct {
				Error     string `json:"error"`
				Exception string `json:"exception"`
			}
			Expect(store.java.InvokeAs(ctx, "probeQueuedResumeValidation", map[string]any{"clusterFile": env.ClusterFile, "mutual": tc.mutual, "queuedPrimary": tc.queuedPrimary}, &result)).To(Succeed())
			Expect(result.Error).To(Equal(tc.message))
			if tc.mutual {
				Expect(result.Exception).To(Equal("RecordCoreException"))
			} else {
				Expect(result.Exception).To(Equal("ValidationException"))
			}
			fmt.Fprintf(GinkgoWriter, "QUEUED-RESUME mutual=%t queued-primary=%t exception=%s error=%s\n", tc.mutual, tc.queuedPrimary, result.Exception, result.Error)
		}
	})

	It("pins the deliberate checked queued setter safety difference from Java", func() {
		_, err := env.RecordDB.Run(ctx, func(rc *recordlayer.FDBRecordContext) (any, error) {
			handle, err := recordlayer.NewStoreBuilder().SetContext(rc).SetMetaDataProvider(store.MetaData).SetSubspace(store.Keyspace).Open()
			Expect(err).NotTo(HaveOccurred())
			Expect(handle.GetFormatVersion()).To(Equal(int32(14)))
			changed, err := handle.MarkIndexWriteOnlyWithQueue(indexName)
			Expect(changed).To(BeFalse())
			var unsupported *recordlayer.UnsupportedFeatureForFormatVersionError
			Expect(errors.As(err, &unsupported)).To(BeTrue())
			Expect(handle.GetIndexState(indexName)).To(Equal(recordlayer.IndexStateReadable))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
		var result struct {
			Changed bool   `json:"changed"`
			Format  int    `json:"format"`
			Capable bool   `json:"capable"`
			State   string `json:"state"`
		}
		Expect(store.java.InvokeAs(ctx, "uncheckedJavaQueuedSetter", store.buildJavaParams(), &result)).To(Succeed())
		Expect(result.Changed).To(BeTrue())
		Expect(result.Format).To(Equal(14))
		Expect(result.Capable).To(BeFalse())
		Expect(result.State).To(Equal("WRITE_ONLY_WITH_QUEUE"))
		fmt.Fprintf(GinkgoWriter, "QUEUED-SETTER java-format=%d capable=%t state=%s go-refused-without-mutation=true\n", result.Format, result.Capable, result.State)
	})

	It("preserves readable build coverage across write-only transitions in both engines", func() {
		for _, javaWriter := range []bool{true, false} {
			if !javaWriter {
				_, err := env.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
					handle, err := recordlayer.NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(store.MetaData).SetSubspace(store.Keyspace).Open()
					Expect(err).NotTo(HaveOccurred())
					changed, err := handle.MarkIndexWriteOnly(indexName)
					Expect(changed).To(BeTrue())
					return nil, err
				})
				Expect(err).NotTo(HaveOccurred())
			}
			params := map[string]any{"clusterFile": env.ClusterFile, "subspace": BytesToIntArray(store.Keyspace.Bytes()), "tenantName": env.TenantName, "transition": javaWriter}
			var result struct {
				Changed  bool   `json:"changed"`
				Complete bool   `json:"complete"`
				State    string `json:"state"`
			}
			Expect(store.java.InvokeAs(ctx, "writeOnlyBuiltCoverage", params, &result)).To(Succeed())
			Expect(result.Changed).To(Equal(javaWriter))
			Expect(result.Complete).To(BeTrue())
			Expect(result.State).To(Equal("WRITE_ONLY"))
			_, err := env.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
				handle, err := recordlayer.NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(store.MetaData).SetSubspace(store.Keyspace).Open()
				Expect(err).NotTo(HaveOccurred())
				complete, err := recordlayer.NewIndexingRangeSet(store.Keyspace, store.MetaData.GetIndex(indexName)).IsComplete(rtx.Transaction())
				Expect(err).NotTo(HaveOccurred())
				Expect(complete).To(BeTrue())
				changed, err := handle.MarkIndexReadable(indexName)
				Expect(changed).To(BeTrue())
				return nil, err
			})
			Expect(err).NotTo(HaveOccurred())
		}
	})

	It("enforces heartbeat exclusion in both Java and Go directions", func() {
		idx := store.MetaData.GetIndex(indexName)
		goHeartbeat := recordlayer.NewIndexingHeartbeat("GO_PEER", 60_000, false, nil)
		_, err := env.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
			return nil, goHeartbeat.CheckAndUpdate(rtx.Transaction(), store.Keyspace, idx)
		})
		Expect(err).NotTo(HaveOccurred())
		javaID := uuid.New().String()
		params := map[string]any{
			"clusterFile": env.ClusterFile, "subspace": BytesToIntArray(store.Keyspace.Bytes()),
			"tenantName": env.TenantName, "id": javaID, "action": "check",
		}
		_, err = store.java.Invoke(ctx, "indexingHeartbeatInterop", params)
		var javaErr *JavaError
		Expect(errors.As(err, &javaErr)).To(BeTrue())
		Expect(javaErr.ExceptionClass).To(Equal("SynchronizedSessionLockedException"))
		_, err = env.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
			goHeartbeat.Cleanup(rtx.Transaction(), store.Keyspace, idx)
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
		params["action"] = "seed"
		raw, err := store.java.Invoke(ctx, "indexingHeartbeatInterop", params)
		Expect(err).NotTo(HaveOccurred())
		var result struct {
			IDs []string `json:"ids"`
		}
		Expect(json.Unmarshal(raw, &result)).To(Succeed())
		Expect(result.IDs).To(Equal([]string{javaID}))
		_, err = env.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
			values, ids, err := recordlayer.ReadHeartbeats(rtx.Transaction(), store.Keyspace, idx)
			if err != nil {
				return nil, err
			}
			Expect(ids).To(Equal([]string{javaID}))
			Expect(values).To(HaveLen(1))
			Expect(values[0].GetInfo()).To(Equal("JAVA_PEER"))
			return nil, goHeartbeat.CheckAndUpdate(rtx.Transaction(), store.Keyspace, idx)
		})
		var locked *recordlayer.SynchronizedSessionLockedError
		Expect(errors.As(err, &locked)).To(BeTrue())
		Expect(locked.ExistingIndexerID).To(Equal(javaID))
		params["action"] = "clear"
		_, err = store.java.Invoke(ctx, "indexingHeartbeatInterop", params)
		Expect(err).NotTo(HaveOccurred())
		_, err = env.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
			return nil, goHeartbeat.CheckAndUpdate(rtx.Transaction(), store.Keyspace, idx)
		})
		Expect(err).NotTo(HaveOccurred())
		fmt.Fprintln(GinkgoWriter, "HEARTBEAT_INTEROP Go blocks Java; Java blocks Go; both cleanup paths release exclusion")
	})

	It("matches Java's heartbeat administration, and pins Java's ongoing answer on every declared population", func() {
		idx := store.MetaData.GetIndex(indexName)
		hbSub := store.Keyspace.Sub(int64(recordlayer.IndexBuildSpaceKey), idx.SubspaceTupleKey(), int64(7))
		now := time.Now().UnixMilli()
		live, stale, broken := uuid.UUID{0x41, 1}, uuid.UUID{0x41, 2}, uuid.UUID{0x41, 3}
		seed := func(rows map[uuid.UUID][]byte) {
			_, err := env.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
				begin, end := hbSub.FDBRangeKeys()
				rtx.Transaction().ClearRange(fdb.KeyRange{Begin: begin, End: end})
				for id, value := range rows {
					rtx.Transaction().Set(hbSub.Pack(tuple.Tuple{tuple.UUID(id)}), value)
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		}
		beat := func(info string, at int64) []byte {
			data, err := proto.Marshal(&gen.IndexBuildHeartbeat{
				Info:                      proto.String(info),
				CreateTimeMilliseconds:    proto.Int64(at),
				HeartbeatTimeMilliseconds: proto.Int64(at),
			})
			Expect(err).NotTo(HaveOccurred())
			return data
		}
		type javaAdmin struct {
			Cleared    int `json:"cleared"`
			Heartbeats map[string]struct {
				Info          string `json:"info"`
				HeartbeatTime int64  `json:"heartbeatTime"`
			} `json:"heartbeats"`
			Ongoing bool `json:"ongoing"`
		}
		javaTry := func(action string, minAgeMs int64, maxCount int) (javaAdmin, error) {
			var r javaAdmin
			err := store.java.InvokeAs(ctx, "indexingHeartbeatAdmin", map[string]any{
				"clusterFile": env.ClusterFile, "subspace": BytesToIntArray(store.Keyspace.Bytes()),
				"tenantName": env.TenantName, "action": action, "minAgeMs": minAgeMs, "maxCount": maxCount,
			}, &r)
			return r, err
		}
		java := func(action string, minAgeMs int64, maxCount int) javaAdmin {
			r, err := javaTry(action, minAgeMs, maxCount)
			Expect(err).To(Succeed())
			return r
		}
		goRead := func(maxCount int) (map[uuid.UUID]*gen.IndexBuildHeartbeat, bool) {
			var heartbeats map[uuid.UUID]*gen.IndexBuildHeartbeat
			var ongoing bool
			_, err := env.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
				handle, err := recordlayer.NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(store.MetaData).SetSubspace(store.Keyspace).Open()
				if err != nil {
					return nil, err
				}
				if heartbeats, err = recordlayer.GetIndexingHeartbeats(rtx.Transaction(), store.Keyspace, idx, maxCount); err != nil {
					return nil, err
				}
				ongoing, err = recordlayer.CheckAnyOngoingOnlineIndexBuildsForStore(handle, idx)
				return nil, err
			})
			Expect(err).NotTo(HaveOccurred())
			return heartbeats, ongoing
		}
		all := map[uuid.UUID][]byte{live: beat("live", now), stale: beat("crashed", now-3_600_000), broken: {0xff}}

		// Reads agree: ids, infos (the invalid one included) and the maxCount valve.
		seed(all)
		j := java("read", 0, 0)
		g, goOngoing := goRead(0)
		Expect(j.Heartbeats).To(HaveLen(3))
		Expect(g).To(HaveLen(3))
		for id, hb := range g {
			Expect(j.Heartbeats).To(HaveKey(id.String()))
			Expect(j.Heartbeats[id.String()].Info).To(Equal(hb.GetInfo()))
			Expect(j.Heartbeats[id.String()].HeartbeatTime).To(Equal(hb.GetHeartbeatTimeMilliseconds()))
		}
		Expect(j.Heartbeats[broken.String()].Info).To(Equal(recordlayer.InvalidHeartbeatInfo))
		Expect(j.Ongoing).To(BeTrue())
		Expect(goOngoing).To(BeTrue(), "a live heartbeat is an ongoing build in both engines")
		j = java("read", 0, 2)
		g, _ = goRead(2)
		Expect(j.Heartbeats).To(HaveLen(2))
		Expect(g).To(HaveLen(2))
		for id := range g {
			Expect(j.Heartbeats).To(HaveKey(id.String()))
		}

		// The declared divergence: only a crashed session's heartbeat is left. Java's
		// heartbeatTime < now + lease still says "ongoing"; Go follows Java's documented
		// contract (younger than the lease) and says no. If Java fixes its arithmetic this
		// expectation reddens and the DIVERGENCES.md entry must be retired.
		seed(map[uuid.UUID][]byte{stale: beat("crashed", now-3_600_000)})
		j = java("read", 0, 0)
		_, goOngoing = goRead(0)
		Expect(j.Ongoing).To(BeTrue(), "Java reports a one-hour-old heartbeat as an ongoing build")
		Expect(goOngoing).To(BeFalse())

		// The other two declared populations. Go's "ongoing" is admission's own predicate
		// (a parsed heartbeat with -1 day < age < lease): an invalid-only heartbeat is no
		// session to admission (Java reads its time as 0 and says ongoing), and a heartbeat
		// an hour ahead refuses a new session (Java's heartbeatTime < now + lease says no).
		seed(map[uuid.UUID][]byte{broken: {0xff}})
		j = java("read", 0, 0)
		_, goOngoing = goRead(0)
		Expect(j.Ongoing).To(BeTrue(), "Java reports an invalid-only heartbeat as an ongoing build")
		Expect(goOngoing).To(BeFalse())
		seed(map[uuid.UUID][]byte{live: beat("skewed", now+3_600_000)})
		j = java("read", 0, 0)
		_, goOngoing = goRead(0)
		Expect(j.Ongoing).To(BeFalse(), "Java does not report a heartbeat an hour ahead as ongoing")
		Expect(goOngoing).To(BeTrue(), "admission refuses a session against a heartbeat an hour ahead")
		// A day ahead is bad data to both engines.
		seed(map[uuid.UUID][]byte{live: beat("bad-clock", now+2*86_400_000)})
		j = java("read", 0, 0)
		_, goOngoing = goRead(0)
		Expect(j.Ongoing).To(BeFalse())
		Expect(goOngoing).To(BeFalse())

		// A heartbeat whose KEY is not a UUID (a legacy string-keyed builder's): Java's
		// ongoing check throws in getIndexingHeartbeats' getUUID(0) (IndexingHeartbeat.java:
		// 148) even beside a live heartbeat, and Go returns IndexingHeartbeatKeyError, the
		// error its admission refuses every session with.
		seed(map[uuid.UUID][]byte{live: beat("live", now)})
		_, err := env.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
			rtx.Transaction().Set(hbSub.Pack(tuple.Tuple{"legacy-builder"}), beat("legacy", now))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
		_, javaErr := javaTry("ongoing", 0, 0)
		var javaKeyErr *JavaError
		Expect(errors.As(javaErr, &javaKeyErr)).To(BeTrue(), "Java's ongoing check throws on a non-UUID heartbeat key: %v", javaErr)
		Expect(javaKeyErr.ExceptionClass).To(Equal("ClassCastException"), "measured: getUUID(0) casts the String key")
		fmt.Fprintf(GinkgoWriter, "HEARTBEAT_ADMIN legacy key: java=%v\n", javaErr)
		_, err = env.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
			handle, err := recordlayer.NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(store.MetaData).SetSubspace(store.Keyspace).Open()
			if err != nil {
				return nil, err
			}
			return recordlayer.CheckAnyOngoingOnlineIndexBuildsForStore(handle, idx)
		})
		var keyErr *recordlayer.IndexingHeartbeatKeyError
		Expect(errors.As(err, &keyErr)).To(BeTrue(), "Go's ongoing check on a non-UUID heartbeat key: %v", err)

		// The same beside a live heartbeat when the bad key sorts AFTER it: a
		// versionstamp element (tuple code 0x33) follows a UUID (0x30), so an engine
		// that stopped at the first live heartbeat would answer "ongoing" here.
		seed(map[uuid.UUID][]byte{live: beat("live", now)})
		_, err = env.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
			rtx.Transaction().Set(hbSub.Pack(tuple.Tuple{tuple.Versionstamp{TransactionVersion: [10]byte{0xff}, UserVersion: 7}}), beat("after", now))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
		_, javaErr = javaTry("ongoing", 0, 0)
		Expect(errors.As(javaErr, &javaKeyErr)).To(BeTrue(), "Java's ongoing check throws on a versionstamp heartbeat key after a live one: %v", javaErr)
		Expect(javaKeyErr.ExceptionClass).To(Equal("ClassCastException"), "measured: getUUID(0) casts the Versionstamp key")
		Expect(javaKeyErr.Message).To(ContainSubstring("Versionstamp cannot be cast"),
			"measured: the key that threw is the versionstamp one, not the live UUID heartbeat")
		fmt.Fprintf(GinkgoWriter, "HEARTBEAT_ADMIN versionstamp key after a live one: java=%v\n", javaErr)
		_, err = env.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
			handle, err := recordlayer.NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(store.MetaData).SetSubspace(store.Keyspace).Open()
			if err != nil {
				return nil, err
			}
			return recordlayer.CheckAnyOngoingOnlineIndexBuildsForStore(handle, idx)
		})
		Expect(errors.As(err, &keyErr)).To(BeTrue(), "Go's ongoing check on a versionstamp heartbeat key after a live one: %v", err)

		// A (UUID, x) key: Java's getUUID(0) reads element 0 and ignores the rest, so
		// both engines read it as that UUID's heartbeat and report the build ongoing.
		trailing := uuid.UUID{0x41, 4}
		seed(map[uuid.UUID][]byte{})
		_, err = env.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
			rtx.Transaction().Set(hbSub.Pack(tuple.Tuple{tuple.UUID(trailing), "extra"}), beat("trailing", now))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
		j = java("read", 0, 0)
		g, goOngoing = goRead(0)
		Expect(j.Heartbeats).To(HaveLen(1))
		Expect(j.Heartbeats).To(HaveKey(trailing.String()), "measured: Java names a (UUID, x) heartbeat by its UUID")
		Expect(j.Heartbeats[trailing.String()].Info).To(Equal("trailing"))
		Expect(j.Ongoing).To(BeTrue())
		Expect(g).To(HaveLen(1))
		Expect(g).To(HaveKey(trailing))
		Expect(g[trailing].GetInfo()).To(Equal("trailing"))
		Expect(goOngoing).To(BeTrue(), "a live (UUID, x)-keyed heartbeat is an ongoing build in both engines")

		// (U) and (U, x) name ONE indexer. Java's getIndexingHeartbeats keeps the later
		// key's value (HashMap.put, IndexingHeartbeat.java:151) and its ongoing check reads
		// only the map's values, so a live (U) beside a (U, x) a day ahead is not ongoing,
		// and Go judges the same surviving value.
		pair := func(bare, suffixed []byte) (javaAdmin, map[uuid.UUID]*gen.IndexBuildHeartbeat, bool) {
			seed(map[uuid.UUID][]byte{})
			_, err := env.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
				rtx.Transaction().Set(hbSub.Pack(tuple.Tuple{tuple.UUID(trailing)}), bare)
				rtx.Transaction().Set(hbSub.Pack(tuple.Tuple{tuple.UUID(trailing), "extra"}), suffixed)
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
			j := java("read", 0, 0)
			g, ongoing := goRead(0)
			return j, g, ongoing
		}
		j, g, goOngoing = pair(beat("live", now), beat("future", now+2*86_400_000))
		Expect(j.Heartbeats).To(HaveLen(1))
		Expect(j.Heartbeats[trailing.String()].Info).To(Equal("future"), "measured: the (U, x) value replaces the (U) value")
		Expect(j.Ongoing).To(BeFalse(), "measured: Java judges only the surviving (U, x) value")
		Expect(g).To(HaveLen(1))
		Expect(g[trailing].GetInfo()).To(Equal("future"))
		Expect(goOngoing).To(BeFalse(), "Go judges the same surviving value")
		j, g, goOngoing = pair(beat("future", now+2*86_400_000), beat("live", now))
		Expect(j.Heartbeats[trailing.String()].Info).To(Equal("live"))
		Expect(j.Ongoing).To(BeTrue())
		Expect(g[trailing].GetInfo()).To(Equal("live"))
		Expect(goOngoing).To(BeTrue())
		// A live (U) beside an UNPARSEABLE (U, x): the survivor is the invalid-heartbeat
		// placeholder in both engines, so this is the declared invalid-only population
		// (Java reads its time as 0 and reports ongoing; Go reports no session).
		j, g, goOngoing = pair(beat("live", now), []byte{0xff})
		Expect(j.Heartbeats).To(HaveLen(1))
		Expect(g).To(HaveLen(1))
		Expect(g[trailing].GetInfo()).To(Equal(j.Heartbeats[trailing.String()].Info), "both engines keep the invalid placeholder")
		Expect(j.Ongoing).To(BeTrue(), "measured: Java reports the invalid survivor as an ongoing build")
		Expect(goOngoing).To(BeFalse(), "Go: an invalid heartbeat is no session")
		j, g, goOngoing = pair([]byte{0xff}, beat("live", now))
		Expect(j.Heartbeats[trailing.String()].Info).To(Equal("live"))
		Expect(j.Ongoing).To(BeTrue())
		Expect(g[trailing].GetInfo()).To(Equal("live"))
		Expect(goOngoing).To(BeTrue())

		// Clears agree on the same state: the stale and the invalid heartbeat go.
		seed(all)
		j = java("clear", 10_000, 0)
		Expect(j.Cleared).To(Equal(2))
		Expect(j.Heartbeats).To(HaveLen(1))
		Expect(j.Heartbeats).To(HaveKey(live.String()))
		seed(all)
		cleared, err := env.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
			return recordlayer.ClearIndexingHeartbeats(rtx.Transaction(), store.Keyspace, idx, 10_000, 0, time.Now().UnixMilli())
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(cleared).To(Equal(2))
		g, _ = goRead(0)
		Expect(g).To(HaveLen(1))
		Expect(g).To(HaveKey(live))
		fmt.Fprintln(GinkgoWriter, "HEARTBEAT_ADMIN reads, valve and clears agree; ongoing: stale java=true go=false, invalid-only java=true go=false, hour-ahead java=false go=true, day-ahead both false (declared), non-UUID key both error")
	})

	Describe("Go marks WRITE_ONLY, Java reads raw state", func() {
		It("should persist WRITE_ONLY state readable by Java", func() {
			err := store.MarkIndexWriteOnlyGo(ctx, indexName)
			Expect(err).NotTo(HaveOccurred())

			// Java reads raw state
			javaState, err := store.GetIndexStateRawJava(ctx, indexName)
			Expect(err).NotTo(HaveOccurred())
			Expect(javaState).To(Equal("WRITE_ONLY"))

			// Go reads raw state for comparison
			goState, err := store.GetIndexStateRawGo(ctx, indexName)
			Expect(err).NotTo(HaveOccurred())
			Expect(goState).To(Equal("WRITE_ONLY"))
		})
	})

	Describe("Java marks WRITE_ONLY, Go reads state", func() {
		It("should persist WRITE_ONLY state readable by Go", func() {
			err := store.MarkIndexWriteOnlyJava(ctx, indexName)
			Expect(err).NotTo(HaveOccurred())

			// Go reads raw state
			goState, err := store.GetIndexStateRawGo(ctx, indexName)
			Expect(err).NotTo(HaveOccurred())
			Expect(goState).To(Equal("WRITE_ONLY"))

			// Go opens store and reads state through API
			openState, err := store.GetIndexStateViaOpenGo(ctx, indexName)
			Expect(err).NotTo(HaveOccurred())
			Expect(openState).To(Equal("WRITE_ONLY"))
		})
	})

	Describe("Go marks DISABLED, Java reads raw state", func() {
		It("should persist DISABLED state readable by Java", func() {
			err := store.MarkIndexDisabledGo(ctx, indexName)
			Expect(err).NotTo(HaveOccurred())

			javaState, err := store.GetIndexStateRawJava(ctx, indexName)
			Expect(err).NotTo(HaveOccurred())
			Expect(javaState).To(Equal("DISABLED"))
		})
	})

	Describe("Go marks WRITE_ONLY then READABLE, Java reads default", func() {
		It("should clear state entry when returning to READABLE", func() {
			// Mark WRITE_ONLY first
			err := store.MarkIndexWriteOnlyGo(ctx, indexName)
			Expect(err).NotTo(HaveOccurred())

			// Verify it's WRITE_ONLY
			javaState, err := store.GetIndexStateRawJava(ctx, indexName)
			Expect(err).NotTo(HaveOccurred())
			Expect(javaState).To(Equal("WRITE_ONLY"))

			// Mark READABLE (clears the key)
			err = store.MarkIndexReadableGo(ctx, indexName)
			Expect(err).NotTo(HaveOccurred())

			// Should be READABLE (no key in FDB)
			javaState, err = store.GetIndexStateRawJava(ctx, indexName)
			Expect(err).NotTo(HaveOccurred())
			Expect(javaState).To(Equal("READABLE"))
		})
	})
})

// IndexStateConformanceStore wraps index state operations for cross-platform testing.
// Uses the indexed metadata (Order$price VALUE index) matching Java's createIndexedMetaData().
type IndexStateConformanceStore struct {
	RecordDB    *recordlayer.FDBDatabase
	MetaData    *recordlayer.RecordMetaData
	Keyspace    subspace.Subspace
	java        *JavaInvoker
	clusterFile string
	tenantName  string
}

// NewIndexStateConformanceStore creates a conformance store for index state persistence tests.
func NewIndexStateConformanceStore(recordDB *recordlayer.FDBDatabase, keyspace subspace.Subspace, clusterFile string, tenantName string) (*IndexStateConformanceStore, error) {
	idx := recordlayer.NewIndex("Order$price", recordlayer.Field("price"))
	builder := recordlayer.NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
	builder.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
	builder.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
	builder.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
	builder.AddIndex("Order", idx)
	md, err := builder.Build()
	if err != nil {
		return nil, err
	}

	ks := keyspace
	if tenantName != "" {
		ks = subspace.Sub(tuple.Tuple{})
	}

	return &IndexStateConformanceStore{
		RecordDB:    recordDB,
		MetaData:    md,
		Keyspace:    ks,
		java:        NewJavaInvoker(),
		clusterFile: clusterFile,
		tenantName:  tenantName,
	}, nil
}

func (s *IndexStateConformanceStore) buildJavaParams() map[string]any {
	params := map[string]any{
		"clusterFile": s.clusterFile,
		"subspace":    BytesToIntArray(s.Keyspace.Bytes()),
	}
	if s.tenantName != "" {
		params["tenantName"] = s.tenantName
	}
	return params
}

// CreateStoreGo creates the indexed store using Go's CreateOrOpen.
func (s *IndexStateConformanceStore) CreateStoreGo(ctx context.Context) error {
	_, err := s.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		_, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(s.MetaData).SetSubspace(s.Keyspace).CreateOrOpen()
		return nil, err
	})
	return err
}

// MarkIndexWriteOnlyGo marks an index as WRITE_ONLY using Go.
func (s *IndexStateConformanceStore) MarkIndexWriteOnlyGo(ctx context.Context, indexName string) error {
	_, err := s.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(s.MetaData).SetSubspace(s.Keyspace).
			SetIndexRebuildPolicy(recordlayer.AlwaysRebuildPolicy).CreateOrOpen()
		if err != nil {
			return nil, err
		}
		_, err = store.MarkIndexWriteOnly(indexName)
		return nil, err
	})
	return err
}

// MarkIndexDisabledGo marks an index as DISABLED using Go.
func (s *IndexStateConformanceStore) MarkIndexDisabledGo(ctx context.Context, indexName string) error {
	_, err := s.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(s.MetaData).SetSubspace(s.Keyspace).
			SetIndexRebuildPolicy(recordlayer.AlwaysRebuildPolicy).CreateOrOpen()
		if err != nil {
			return nil, err
		}
		_, err = store.MarkIndexDisabled(indexName)
		return nil, err
	})
	return err
}

// MarkIndexReadableGo marks an index as READABLE using Go.
// Inserts the full range set first so that checkIndexBuilt passes.
func (s *IndexStateConformanceStore) MarkIndexReadableGo(ctx context.Context, indexName string) error {
	_, err := s.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(s.MetaData).SetSubspace(s.Keyspace).
			SetIndexRebuildPolicy(recordlayer.AlwaysRebuildPolicy).CreateOrOpen()
		if err != nil {
			return nil, err
		}
		// Mark the range set as complete so checkIndexBuilt passes.
		idx := s.MetaData.GetIndex(indexName)
		if idx != nil {
			rangeSet := recordlayer.NewIndexingRangeSet(s.Keyspace, idx)
			if _, err := rangeSet.InsertRange(rtx.Transaction(), nil, nil, false); err != nil {
				return nil, err
			}
		}
		_, err = store.MarkIndexReadable(indexName)
		return nil, err
	})
	return err
}

// MarkIndexWriteOnlyJava marks an index as WRITE_ONLY using Java.
func (s *IndexStateConformanceStore) MarkIndexWriteOnlyJava(ctx context.Context, indexName string) error {
	params := s.buildJavaParams()
	params["indexName"] = indexName
	return s.java.InvokeAs(ctx, "markIndexWriteOnly", params, nil)
}

// MarkIndexDisabledJava marks an index as DISABLED using Java.
func (s *IndexStateConformanceStore) MarkIndexDisabledJava(ctx context.Context, indexName string) error {
	params := s.buildJavaParams()
	params["indexName"] = indexName
	return s.java.InvokeAs(ctx, "markIndexDisabled", params, nil)
}

// MarkIndexReadableJava marks an index as READABLE using Java.
func (s *IndexStateConformanceStore) MarkIndexReadableJava(ctx context.Context, indexName string) error {
	params := s.buildJavaParams()
	params["indexName"] = indexName
	return s.java.InvokeAs(ctx, "markIndexReadable", params, nil)
}

// GetIndexStateRawGo reads the raw index state from FDB using Go (no store open).
func (s *IndexStateConformanceStore) GetIndexStateRawGo(ctx context.Context, indexName string) (string, error) {
	var state string
	_, err := s.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		isSubspace := s.Keyspace.Sub(int64(5)) // IndexStateSpaceKey = 5
		stateKey := fdb.Key(isSubspace.Pack(tuple.Tuple{indexName}))
		stateBytes, err := rtx.Transaction().Get(stateKey).Get()
		if err != nil {
			return nil, fmt.Errorf("failed to read index state: %w", err)
		}
		if stateBytes == nil {
			state = "READABLE" // Default
			return nil, nil
		}
		valueTuple, err := tuple.Unpack(stateBytes)
		if err != nil {
			return nil, fmt.Errorf("failed to unpack index state value: %w", err)
		}
		code := valueTuple[0].(int64)
		switch recordlayer.IndexState(code) {
		case recordlayer.IndexStateReadable:
			state = "READABLE"
		case recordlayer.IndexStateWriteOnly:
			state = "WRITE_ONLY"
		case recordlayer.IndexStateDisabled:
			state = "DISABLED"
		case recordlayer.IndexStateReadableUniquePending:
			state = "READABLE_UNIQUE_PENDING"
		default:
			state = fmt.Sprintf("UNKNOWN(%d)", code)
		}
		return nil, nil
	})
	return state, err
}

// GetIndexStateRawJava reads the raw index state from FDB using Java (no store open).
func (s *IndexStateConformanceStore) GetIndexStateRawJava(ctx context.Context, indexName string) (string, error) {
	params := s.buildJavaParams()
	params["indexName"] = indexName

	var state string
	if err := s.java.InvokeAs(ctx, "getIndexStateRaw", params, &state); err != nil {
		return "", fmt.Errorf("java getIndexStateRaw failed: %w", err)
	}
	return state, nil
}

// GetIndexStateViaOpenGo opens the store with Go and reads the index state through the store API.
func (s *IndexStateConformanceStore) GetIndexStateViaOpenGo(ctx context.Context, indexName string) (string, error) {
	var state string
	_, err := s.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(s.MetaData).SetSubspace(s.Keyspace).
			SetIndexRebuildPolicy(recordlayer.AlwaysRebuildPolicy).Open()
		if err != nil {
			return nil, err
		}
		state = store.GetIndexState(indexName).String()
		return nil, nil
	})
	return state, err
}
