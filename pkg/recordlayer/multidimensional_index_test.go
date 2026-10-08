package recordlayer

import (
	"context"
	"errors"
	"math"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"
)

var _ = Describe("MultidimensionalIndex", func() {
	ctx := context.Background()

	baseMetaData := func() *RecordMetaDataBuilder {
		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		return builder
	}

	It("basic lifecycle — save records and scan index", func() {
		ks := specSubspace()

		dimExpr := Dimensions(Concat(Field("coord_x"), Field("coord_y")), 0, 2)
		mdIdx := NewMultidimensionalIndex("md_price_qty", dimExpr)
		builder := baseMetaData()
		builder.AddIndex("Order", mdIdx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			// Save 3 orders with different (price, quantity).
			for _, o := range []struct {
				id       int64
				price    int64
				quantity int64
			}{
				{1, 100, 10},
				{2, 200, 20},
				{3, 300, 30},
			} {
				_, err = store.SaveRecord(&gen.Order{
					OrderId: proto.Int64(o.id),
					CoordX:  proto.Int64(o.price),
					CoordY:  proto.Int64(o.quantity),
				})
				Expect(err).NotTo(HaveOccurred())
			}

			// Scan index — should return 3 entries.
			entries, err := AsList(ctx, store.ScanIndex(mdIdx, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(3))

			// Verify that all coordinate pairs are present.
			type coordPair struct{ x, y int64 }
			found := make(map[coordPair]bool)
			for _, e := range entries {
				Expect(len(e.Key)).To(BeNumerically(">=", 2))
				x, ok := e.Key[0].(int64)
				Expect(ok).To(BeTrue())
				y, ok := e.Key[1].(int64)
				Expect(ok).To(BeTrue())
				found[coordPair{x, y}] = true
			}
			Expect(found).To(HaveKey(coordPair{100, 10}))
			Expect(found).To(HaveKey(coordPair{200, 20}))
			Expect(found).To(HaveKey(coordPair{300, 30}))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("prefix skip-scan honors the aggregate scanned-records budget (RFC-106a)", func() {
		ks := specSubspace()

		// PrefixSize=1: price is the partition prefix; coord_x/coord_y are the 2
		// R-tree dims. Distinct prices → distinct R-tree partitions, so an
		// unbounded-prefix scan must skip-scan across them (prefixSkipScanCursor).
		dimExpr := Dimensions(Concat(Field("price"), Field("coord_x"), Field("coord_y")), 1, 2)
		mdIdx := NewMultidimensionalIndex("md_prefix_skip", dimExpr)
		builder := baseMetaData()
		builder.AddIndex("Order", mdIdx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		const partitions = 20 // 20 distinct prices → 20 small (1-point) prefixes
		const scanLimit = 5

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			for i := 0; i < partitions; i++ {
				_, err = store.SaveRecord(&gen.Order{
					OrderId: proto.Int64(int64(i)),
					Price:   proto.Int32(int32(i)), // distinct prefix per record
					CoordX:  proto.Int64(int64(i * 10)),
					CoordY:  proto.Int64(int64(i * 10)),
				})
				Expect(err).NotTo(HaveOccurred())
			}

			// Unbounded-prefix scan with a small aggregate scan budget + fail. Each
			// prefix holds 1 point (< scanLimit), so WITHOUT the shared budget the
			// skip-scan reads all 20 cleanly; WITH it, totalScanned (per-prefix scans
			// + findNextPrefix enumeration reads) trips ScanLimitReached at the cap.
			scan := ForwardScan()
			scan.ExecuteProperties = scan.ExecuteProperties.WithScannedRecordsLimit(scanLimit)
			scan.ExecuteProperties.FailOnScanLimitReached = true

			_, scanErr := AsList(ctx, store.ScanIndex(mdIdx, TupleRangeAll, nil, scan))
			var sle *ScanLimitReachedError
			Expect(errors.As(scanErr, &sle)).To(BeTrue(),
				"the cross-prefix scan budget must trip ScanLimitReachedError, got: %v", scanErr)
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("prefix skip-scan honors the aggregate scanned-BYTES budget (RFC-106a)", func() {
		ks := specSubspace()

		// Same prefix-partitioned shape as the records test, but the shared budget
		// is a BYTE cap. Each prefix is one small point; per-prefix byte counters
		// reset, so only the cross-prefix totalBytesScanned (per-prefix point reads
		// + findNextPrefix enumeration reads) trips the cap.
		dimExpr := Dimensions(Concat(Field("price"), Field("coord_x"), Field("coord_y")), 1, 2)
		mdIdx := NewMultidimensionalIndex("md_prefix_skip_bytes", dimExpr)
		builder := baseMetaData()
		builder.AddIndex("Order", mdIdx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		const partitions = 20

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			for i := 0; i < partitions; i++ {
				_, err = store.SaveRecord(&gen.Order{
					OrderId: proto.Int64(int64(i)),
					Price:   proto.Int32(int32(i)),
					CoordX:  proto.Int64(int64(i * 10)),
					CoordY:  proto.Int64(int64(i * 10)),
				})
				Expect(err).NotTo(HaveOccurred())
			}

			// A small byte cap (one prefix's point + enumeration read is already
			// tens of bytes) trips the cross-prefix byte budget. The skip-scan
			// cannot resume cross-prefix, so an aggregate budget stop is a terminal
			// ScanLimitReachedError regardless of FailOnScanLimitReached.
			scan := ForwardScan()
			scan.ExecuteProperties = scan.ExecuteProperties.WithScannedBytesLimit(40)

			_, scanErr := AsList(ctx, store.ScanIndex(mdIdx, TupleRangeAll, nil, scan))
			var sle *ScanLimitReachedError
			Expect(errors.As(scanErr, &sle)).To(BeTrue(),
				"the cross-prefix byte budget must trip ScanLimitReachedError, got: %v", scanErr)
			Expect(sle.Reason).To(Equal(ByteLimitReached))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("prefix skip-scan aggregates the scanned-records budget across legs of a fan-out (RFC-106a)", func() {
		// The MULTIDIMENSIONAL index has no SQL surface (no CREATE INDEX ...
		// USING RTREE / SPATIAL DDL — grep confirms zero references to
		// MULTIDIMENSIONAL anywhere under pkg/relational), so this cannot be
		// driven through a SQL IN-join/IN-union the way
		// TestFDB_RFC106a_INJoinScanLimitAggregatesAcrossLegs exercises the
		// index_scan.go leaf cursor. Instead, drive store.ScanIndex directly,
		// N times, reusing the SAME ScanProperties (and so the SAME
		// *ScanLimiterState pointer on ExecuteProperties.ScanState) across
		// every call — exactly the mechanism executeInJoin/executeInUnion use
		// for real IN-list legs (props.ClearSkipAndLimit(), never a fresh
		// DefaultExecuteProperties() per leg — executor_new_plans.go:1055).
		// Each "leg" here is a full unbound-prefix scan of a tiny index (one
		// partition, two points in it), so it always hits prefixSkipScanCursor
		// (dimExpr.PrefixSize > 0, scanRange.Low == nil) — the cursor that used
		// to nil out ExecuteProperties.ScanState for its per-prefix inner scans
		// and keep a local total that reset to zero on every new
		// prefixSkipScanCursor, i.e. every leg.
		//
		// Per leg, in Hilbert order: findNextPrefix's 1-row enumeration probe
		// charges the shared state, the FIRST point read is exempt (the
		// leg-wide free-initial-pass — CursorLimitManager.java:134-138, granted
		// once per prefixSkipScanCursor, not once per prefix), and the SECOND
		// point read is fully gated by the shared cumulative count. A leg is
		// "individually small" — 3 units of shared budget — yet with the fix
		// the AGGREGATE across legs still trips a small shared cap; without the
		// fix every leg resets to zero and none ever would, no matter how many
		// legs ran.
		ks := specSubspace()

		dimExpr := Dimensions(Concat(Field("price"), Field("coord_x")), 1, 1)
		mdIdx := NewMultidimensionalIndex("md_fanout_legs", dimExpr)
		builder := baseMetaData()
		builder.AddIndex("Order", mdIdx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		const scanLimit = 10
		const numLegs = 10

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			// One partition (price=1), two points in it (coord_x=10,20) — the
			// per-leg cost every leg re-scans identically.
			for i, x := range []int64{10, 20} {
				_, err = store.SaveRecord(&gen.Order{
					OrderId: proto.Int64(int64(i + 1)),
					Price:   proto.Int32(1),
					CoordX:  proto.Int64(x),
				})
				Expect(err).NotTo(HaveOccurred())
			}

			// ONE ScanProperties (and so one shared *ScanLimiterState) reused
			// across every leg — the fan-out sharing mechanism under test.
			props := ForwardScan()
			props.ExecuteProperties = props.ExecuteProperties.WithScannedRecordsLimit(scanLimit)

			var tripped int
			for leg := 0; leg < numLegs; leg++ {
				legProps := props
				legProps.ExecuteProperties = legProps.ExecuteProperties.ClearSkipAndLimit()
				entries, legErr := AsList(ctx, store.ScanIndex(mdIdx, TupleRangeAll, nil, legProps))
				if legErr != nil {
					var sle *ScanLimitReachedError
					Expect(errors.As(legErr, &sle)).To(BeTrue(),
						"leg %d: want ScanLimitReachedError, got: %v", leg, legErr)
					tripped++
					continue
				}
				Expect(entries).To(HaveLen(2), "leg %d: an untripped leg must see both points", leg)
			}

			Expect(tripped).To(BeNumerically(">", 0),
				"the aggregate scan budget across %d individually-small legs (cap=%d) never tripped — "+
					"the shared ScanLimiterState is not being charged across legs", numLegs, scanLimit)
			Expect(tripped).To(BeNumerically("<", numLegs),
				"every leg tripped — the cap is too tight to also prove early legs succeed on their own merits")

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("prefix skip-scan does not issue another read once the shared record budget is already spent (FailOnScanLimitReached)", func() {
		// Regression test: prefixSkipScanCursor.OnNext used to call
		// findNextPrefix() (a real GetRange) UNCONDITIONALLY, charging
		// AddRecordScanned() only after that read came back — so under
		// FailOnScanLimitReached, a shared budget that was ALREADY at its
		// limit before this cursor ever ran still let one more
		// prefix-enumeration read reach FDB before the limit error
		// surfaced (only the per-prefix rtreeScanCursor built from that
		// escaped read would ever report it). A hard limit checked after
		// the read has already gone out is not a hard limit.
		//
		// Pre-spend the shared *ScanLimiterState directly (no read
		// involved) to the cap, exactly the state a prior leg of an
		// IN-join/IN-union fan-out would have left behind sharing the
		// same pointer (see the fan-out test above), then run a fresh
		// unbounded-prefix scan against a non-empty index. If a read
		// escapes, findNextPrefix's own AddRecordScanned() call (fired
		// only after a successful GetRange) advances the counter past
		// the pre-charged value — that is the observable this test pins.
		ks := specSubspace()

		dimExpr := Dimensions(Concat(Field("price"), Field("coord_x")), 1, 1)
		mdIdx := NewMultidimensionalIndex("md_prefix_prespent", dimExpr)
		builder := baseMetaData()
		builder.AddIndex("Order", mdIdx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		const scanLimit = 3

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			// Real data — without the fix, findNextPrefix would actually
			// find this prefix and the escaped read would succeed.
			for i, x := range []int64{10, 20} {
				_, err = store.SaveRecord(&gen.Order{
					OrderId: proto.Int64(int64(i + 1)),
					Price:   proto.Int32(1),
					CoordX:  proto.Int64(x),
				})
				Expect(err).NotTo(HaveOccurred())
			}

			props := ForwardScan()
			props.ExecuteProperties = props.ExecuteProperties.WithScannedRecordsLimit(scanLimit)
			props.ExecuteProperties.FailOnScanLimitReached = true

			state := props.ExecuteProperties.ScanState
			for state.RecordsScanned() < scanLimit {
				state.AddRecordScanned()
			}
			before := state.RecordsScanned()

			_, scanErr := AsList(ctx, store.ScanIndex(mdIdx, TupleRangeAll, nil, props))
			var sle *ScanLimitReachedError
			Expect(errors.As(scanErr, &sle)).To(BeTrue(),
				"an already-spent shared budget under FailOnScanLimitReached must surface "+
					"ScanLimitReachedError before the skip-scan opens its first prefix, got: %v", scanErr)

			Expect(state.RecordsScanned()).To(Equal(before),
				"prefix skip-scan must not perform another range read once the shared "+
					"record-scan budget is already spent under FailOnScanLimitReached — "+
					"the shared counter advanced past the pre-charged value, meaning a "+
					"findNextPrefix GetRange escaped the hard limit")

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("prefix skip-scan charges the shared budget for a miss that exhausts enumeration, not just for a hit", func() {
		// Regression test for the sibling half of the ask-before-read fix
		// above: findNextPrefix used to charge AddRecordScanned() only on its
		// `found` return path, back in the caller (OnNext). Every MISS return
		// — GetRange came back empty, an unparseable key, a too-short prefix —
		// still performs the exact same GetRange and must cost the same one
		// record, matching CursorLimitManager.tryRecordScan(), which
		// decrements recordScanLimiter on the ATTEMPT, before the outcome is
		// known (CursorLimitManager.java:134-136) — a miss costs identically
		// to a hit in Java. Charging only hits silently undercounts every leg
		// whose prefix enumeration terminates by exhaustion: one uncharged
		// read per leg, against a budget shared across an entire fan-out plan
		// (not a one-off — see the fan-out test above for why that sharing
		// matters).
		//
		// An index with ZERO records isolates the assertion cleanly:
		// PrefixSize > 0 still routes ScanIndex through prefixSkipScanCursor
		// (Scan()'s dispatch is on the index shape, not on data presence), so
		// OnNext makes exactly ONE findNextPrefix() call, which immediately
		// misses (GetRange finds nothing) and exhausts the cursor — no
		// per-prefix rtreeScanCursor is ever built, so there is no other read
		// in the leg to conflate the count with.
		ks := specSubspace()

		dimExpr := Dimensions(Concat(Field("price"), Field("coord_x")), 1, 1)
		mdIdx := NewMultidimensionalIndex("md_prefix_miss_charge", dimExpr)
		builder := baseMetaData()
		builder.AddIndex("Order", mdIdx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		const numLegs = 5

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())
			// Deliberately no SaveRecord calls: the index stays empty, so
			// every leg's single findNextPrefix() call is a miss.

			// ONE ScanProperties (and so one shared *ScanLimiterState) reused
			// across every leg — same fan-out sharing mechanism as the
			// existing aggregation test above.
			props := ForwardScan()
			state := props.ExecuteProperties.ScanState
			before := state.RecordsScanned()

			for leg := 0; leg < numLegs; leg++ {
				legProps := props
				legProps.ExecuteProperties = legProps.ExecuteProperties.ClearSkipAndLimit()
				entries, legErr := AsList(ctx, store.ScanIndex(mdIdx, TupleRangeAll, nil, legProps))
				Expect(legErr).NotTo(HaveOccurred())
				Expect(entries).To(HaveLen(0))
			}

			Expect(state.RecordsScanned()).To(Equal(before+numLegs),
				"each of %d exhausted legs must charge exactly one record for its "+
					"terminating findNextPrefix miss — got %d new charges, want %d",
				numLegs, state.RecordsScanned()-before, numLegs)

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("delete record clears index entry", func() {
		ks := specSubspace()

		dimExpr := Dimensions(Concat(Field("coord_x"), Field("coord_y")), 0, 2)
		mdIdx := NewMultidimensionalIndex("md_price_qty", dimExpr)
		builder := baseMetaData()
		builder.AddIndex("Order", mdIdx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			// Save one order.
			_, err = store.SaveRecord(&gen.Order{
				OrderId: proto.Int64(1),
				CoordX:  proto.Int64(100),
				CoordY:  proto.Int64(10),
			})
			Expect(err).NotTo(HaveOccurred())

			// Verify it is in the index.
			entries, err := AsList(ctx, store.ScanIndex(mdIdx, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(1))

			// Delete the record.
			existed, err := store.DeleteRecord(tuple.Tuple{int64(1)})
			Expect(err).NotTo(HaveOccurred())
			Expect(existed).To(BeTrue())

			// Index should be empty.
			entries, err = AsList(ctx, store.ScanIndex(mdIdx, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(0))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("update record updates index entry", func() {
		ks := specSubspace()

		dimExpr := Dimensions(Concat(Field("coord_x"), Field("coord_y")), 0, 2)
		mdIdx := NewMultidimensionalIndex("md_price_qty", dimExpr)
		builder := baseMetaData()
		builder.AddIndex("Order", mdIdx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			// Save original order.
			_, err = store.SaveRecord(&gen.Order{
				OrderId: proto.Int64(1),
				CoordX:  proto.Int64(100),
				CoordY:  proto.Int64(10),
			})
			Expect(err).NotTo(HaveOccurred())

			// Update same order with new price and quantity.
			_, err = store.SaveRecord(&gen.Order{
				OrderId: proto.Int64(1),
				CoordX:  proto.Int64(500),
				CoordY:  proto.Int64(50),
			})
			Expect(err).NotTo(HaveOccurred())

			// Should have exactly 1 entry with the new coordinates.
			entries, err := AsList(ctx, store.ScanIndex(mdIdx, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(1))
			Expect(entries[0].Key[0]).To(Equal(int64(500)))
			Expect(entries[0].Key[1]).To(Equal(int64(50)))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("multiple records — save 5 and verify all present", func() {
		ks := specSubspace()

		dimExpr := Dimensions(Concat(Field("coord_x"), Field("coord_y")), 0, 2)
		mdIdx := NewMultidimensionalIndex("md_price_qty", dimExpr)
		builder := baseMetaData()
		builder.AddIndex("Order", mdIdx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		type order struct {
			id       int64
			price    int64
			quantity int64
		}
		orders := []order{
			{1, 10, 100},
			{2, 20, 200},
			{3, 30, 300},
			{4, 40, 400},
			{5, 50, 500},
		}

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			for _, o := range orders {
				_, err = store.SaveRecord(&gen.Order{
					OrderId: proto.Int64(o.id),
					CoordX:  proto.Int64(o.price),
					CoordY:  proto.Int64(o.quantity),
				})
				Expect(err).NotTo(HaveOccurred())
			}

			entries, err := AsList(ctx, store.ScanIndex(mdIdx, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(5))

			type coordPair struct{ x, y int64 }
			found := make(map[coordPair]bool)
			for _, e := range entries {
				x, _ := e.Key[0].(int64)
				y, _ := e.Key[1].(int64)
				found[coordPair{x, y}] = true
			}
			for _, o := range orders {
				Expect(found).To(HaveKey(coordPair{int64(o.price), int64(o.quantity)}))
			}

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("mixed save and delete — interleaved operations", func() {
		ks := specSubspace()

		dimExpr := Dimensions(Concat(Field("coord_x"), Field("coord_y")), 0, 2)
		mdIdx := NewMultidimensionalIndex("md_price_qty", dimExpr)
		builder := baseMetaData()
		builder.AddIndex("Order", mdIdx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			// Save 3.
			for _, id := range []int64{1, 2, 3} {
				_, err = store.SaveRecord(&gen.Order{
					OrderId: proto.Int64(id),
					CoordX:  proto.Int64(int64(id * 100)),
					CoordY:  proto.Int64(int64(id * 10)),
				})
				Expect(err).NotTo(HaveOccurred())
			}

			// Delete #2.
			existed, err := store.DeleteRecord(tuple.Tuple{int64(2)})
			Expect(err).NotTo(HaveOccurred())
			Expect(existed).To(BeTrue())

			// Save #4.
			_, err = store.SaveRecord(&gen.Order{
				OrderId: proto.Int64(4),
				CoordX:  proto.Int64(400),
				CoordY:  proto.Int64(40),
			})
			Expect(err).NotTo(HaveOccurred())

			// Should have 3 entries: #1, #3, #4.
			entries, err := AsList(ctx, store.ScanIndex(mdIdx, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(3))

			type coordPair struct{ x, y int64 }
			found := make(map[coordPair]bool)
			for _, e := range entries {
				x, _ := e.Key[0].(int64)
				y, _ := e.Key[1].(int64)
				found[coordPair{x, y}] = true
			}
			Expect(found).To(HaveKey(coordPair{100, 10}))
			Expect(found).NotTo(HaveKey(coordPair{200, 20})) // deleted
			Expect(found).To(HaveKey(coordPair{300, 30}))
			Expect(found).To(HaveKey(coordPair{400, 40}))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("MULTIDIMENSIONAL index with small MaxM forces R-tree splits via index maintainer", func() {
		ks := specSubspace()

		dimExpr := Dimensions(Concat(Field("coord_x"), Field("coord_y")), 0, 2)
		mdIdx := NewMultidimensionalIndex("md_price_qty_small", dimExpr)
		// Configure small MaxM via index options.
		mdIdx.Options[IndexOptionRTreeMaxM] = "4"
		mdIdx.Options[IndexOptionRTreeMinM] = "2"
		mdIdx.Options[IndexOptionRTreeSplitS] = "2"

		builder := baseMetaData()
		builder.AddIndex("Order", mdIdx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		// Generate enough orders to force multiple R-tree splits.
		// With MaxM=4, 25 records should create a multi-level tree.
		const n = 25
		type order struct {
			id       int64
			price    int64
			quantity int64
		}
		orders := make([]order, n)
		for i := 0; i < n; i++ {
			orders[i] = order{
				id:       int64(i + 1),
				price:    int64((i + 1) * 50),
				quantity: int64((i + 1) * 7),
			}
		}

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			for _, o := range orders {
				_, err = store.SaveRecord(&gen.Order{
					OrderId: proto.Int64(o.id),
					CoordX:  proto.Int64(o.price),
					CoordY:  proto.Int64(o.quantity),
				})
				Expect(err).NotTo(HaveOccurred())
			}

			// Scan index and verify all entries present.
			entries, err := AsList(ctx, store.ScanIndex(mdIdx, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(n))

			// Verify all coordinate pairs are present.
			type coordPair struct{ x, y int64 }
			found := make(map[coordPair]bool)
			for _, e := range entries {
				x, ok := e.Key[0].(int64)
				Expect(ok).To(BeTrue())
				y, ok := e.Key[1].(int64)
				Expect(ok).To(BeTrue())
				found[coordPair{x, y}] = true
			}
			for _, o := range orders {
				Expect(found).To(HaveKey(coordPair{int64(o.price), int64(o.quantity)}),
					"missing order %d at (%d, %d)", o.id, o.price, o.quantity)
			}

			// Now delete half the records and verify remaining.
			for i := 0; i < n; i += 2 {
				existed, err := store.DeleteRecord(tuple.Tuple{orders[i].id})
				Expect(err).NotTo(HaveOccurred())
				Expect(existed).To(BeTrue())
			}

			entries, err = AsList(ctx, store.ScanIndex(mdIdx, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			remaining := n / 2 // odd-indexed orders survive
			Expect(entries).To(HaveLen(remaining))

			found = make(map[coordPair]bool)
			for _, e := range entries {
				x := e.Key[0].(int64)
				y := e.Key[1].(int64)
				found[coordPair{x, y}] = true
			}
			for i := 1; i < n; i += 2 {
				o := orders[i]
				Expect(found).To(HaveKey(coordPair{int64(o.price), int64(o.quantity)}),
					"missing surviving order %d", o.id)
			}

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("MULTIDIMENSIONAL index with small MaxM — update records after splits", func() {
		ks := specSubspace()

		dimExpr := Dimensions(Concat(Field("coord_x"), Field("coord_y")), 0, 2)
		mdIdx := NewMultidimensionalIndex("md_price_qty_update", dimExpr)
		mdIdx.Options[IndexOptionRTreeMaxM] = "4"
		mdIdx.Options[IndexOptionRTreeMinM] = "2"
		mdIdx.Options[IndexOptionRTreeSplitS] = "2"

		builder := baseMetaData()
		builder.AddIndex("Order", mdIdx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		const n = 15

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			// Insert n records.
			for i := 1; i <= n; i++ {
				_, err = store.SaveRecord(&gen.Order{
					OrderId: proto.Int64(int64(i)),
					CoordX:  proto.Int64(int64(i * 100)),
					CoordY:  proto.Int64(int64(i * 10)),
				})
				Expect(err).NotTo(HaveOccurred())
			}

			entries, err := AsList(ctx, store.ScanIndex(mdIdx, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(n))

			// Update every record with new coordinates. This exercises
			// delete-old-entry + insert-new-entry through the split tree.
			for i := 1; i <= n; i++ {
				_, err = store.SaveRecord(&gen.Order{
					OrderId: proto.Int64(int64(i)),
					CoordX:  proto.Int64(int64(i*100 + 1)),
					CoordY:  proto.Int64(int64(i*10 + 1)),
				})
				Expect(err).NotTo(HaveOccurred())
			}

			entries, err = AsList(ctx, store.ScanIndex(mdIdx, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(n))

			// Verify updated coordinates.
			type coordPair struct{ x, y int64 }
			found := make(map[coordPair]bool)
			for _, e := range entries {
				x := e.Key[0].(int64)
				y := e.Key[1].(int64)
				found[coordPair{x, y}] = true
			}
			for i := 1; i <= n; i++ {
				Expect(found).To(HaveKey(coordPair{int64(i*100 + 1), int64(i*10 + 1)}))
			}

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("continuation token round-trip through ScanIndex", func() {
		ks := specSubspace()

		dimExpr := Dimensions(Concat(Field("coord_x"), Field("coord_y")), 0, 2)
		mdIdx := NewMultidimensionalIndex("md_cont_roundtrip", dimExpr)
		builder := baseMetaData()
		builder.AddIndex("Order", mdIdx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			// Save 15 records.
			const n = 15
			for i := 1; i <= n; i++ {
				_, err = store.SaveRecord(&gen.Order{
					OrderId: proto.Int64(int64(i)),
					CoordX:  proto.Int64(int64(i * 100)),
					CoordY:  proto.Int64(int64(i * 10)),
				})
				Expect(err).NotTo(HaveOccurred())
			}

			// Paginate with limit=5.
			props := ScanProperties{
				ExecuteProperties: DefaultExecuteProperties().WithReturnedRowLimit(5),
			}

			// Page 1.
			page1, cont1, err := AsListWithContinuation(ctx, store.ScanIndex(
				mdIdx, TupleRangeAll, nil, props))
			Expect(err).NotTo(HaveOccurred())
			Expect(page1).To(HaveLen(5))
			Expect(cont1).NotTo(BeNil(), "continuation should be non-nil after page 1")

			// Page 2.
			page2, cont2, err := AsListWithContinuation(ctx, store.ScanIndex(
				mdIdx, TupleRangeAll, cont1, props))
			Expect(err).NotTo(HaveOccurred())
			Expect(page2).To(HaveLen(5))
			Expect(cont2).NotTo(BeNil(), "continuation should be non-nil after page 2")

			// Page 3.
			page3, _, err := AsListWithContinuation(ctx, store.ScanIndex(
				mdIdx, TupleRangeAll, cont2, props))
			Expect(err).NotTo(HaveOccurred())
			Expect(page3).To(HaveLen(5))

			// Verify all 15 unique entries, no duplicates, no gaps.
			type coordPair struct{ x, y int64 }
			allCoords := make(map[coordPair]bool)
			for _, pages := range [][]*IndexEntry{page1, page2, page3} {
				for _, e := range pages {
					x := e.Key[0].(int64)
					y := e.Key[1].(int64)
					cp := coordPair{x, y}
					Expect(allCoords).NotTo(HaveKey(cp), "duplicate entry at (%d, %d)", x, y)
					allCoords[cp] = true
				}
			}
			Expect(allCoords).To(HaveLen(n))
			for i := 1; i <= n; i++ {
				Expect(allCoords).To(HaveKey(coordPair{int64(i * 100), int64(i * 10)}))
			}

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("row limit enforcement", func() {
		ks := specSubspace()

		dimExpr := Dimensions(Concat(Field("coord_x"), Field("coord_y")), 0, 2)
		mdIdx := NewMultidimensionalIndex("md_row_limit", dimExpr)
		builder := baseMetaData()
		builder.AddIndex("Order", mdIdx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			// Save 10 records.
			for i := 1; i <= 10; i++ {
				_, err = store.SaveRecord(&gen.Order{
					OrderId: proto.Int64(int64(i)),
					CoordX:  proto.Int64(int64(i * 50)),
					CoordY:  proto.Int64(int64(i * 5)),
				})
				Expect(err).NotTo(HaveOccurred())
			}

			// ScanIndex with ReturnedRowLimit=3.
			props := ScanProperties{
				ExecuteProperties: DefaultExecuteProperties().WithReturnedRowLimit(3),
			}
			cursor := store.ScanIndex(mdIdx, TupleRangeAll, nil, props)
			defer func() { _ = cursor.Close() }()

			count := 0
			for {
				result, err := cursor.OnNext(ctx)
				Expect(err).NotTo(HaveOccurred())
				if !result.HasNext() {
					// Must stop due to row limit, not source exhaustion.
					Expect(result.GetNoNextReason()).To(Equal(ReturnLimitReached))
					cont := result.GetContinuation()
					Expect(cont).NotTo(BeNil())
					Expect(cont.IsEnd()).To(BeFalse(), "continuation should not be end when limit reached")
					break
				}
				count++
			}
			Expect(count).To(Equal(3), "exactly 3 entries should be returned")

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("negative and boundary coordinates", func() {
		ks := specSubspace()

		dimExpr := Dimensions(Concat(Field("coord_x"), Field("coord_y")), 0, 2)
		mdIdx := NewMultidimensionalIndex("md_neg_boundary", dimExpr)
		builder := baseMetaData()
		builder.AddIndex("Order", mdIdx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			// Save records with extreme coordinates: the dimensions are INT64,
			// so the extremes are int64's; int32's are ordinary values.
			type testCase struct {
				id       int64
				price    int64
				quantity int64
			}
			cases := []testCase{
				{1, -100, -200},
				{2, 0, 0},
				{3, math.MaxInt64, math.MinInt64},
				{4, 1, -1},
				{5, math.MaxInt32, math.MinInt32},
			}

			for _, tc := range cases {
				_, err = store.SaveRecord(&gen.Order{
					OrderId: proto.Int64(tc.id),
					CoordX:  proto.Int64(tc.price),
					CoordY:  proto.Int64(tc.quantity),
				})
				Expect(err).NotTo(HaveOccurred())
			}

			// Scan — all 5 entries must be present.
			entries, err := AsList(ctx, store.ScanIndex(mdIdx, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(5))

			type coordPair struct{ x, y int64 }
			found := make(map[coordPair]bool)
			for _, e := range entries {
				x := e.Key[0].(int64)
				y := e.Key[1].(int64)
				found[coordPair{x, y}] = true
			}
			for _, tc := range cases {
				Expect(found).To(HaveKey(coordPair{int64(tc.price), int64(tc.quantity)}),
					"missing entry for order %d at (%d, %d)", tc.id, tc.price, tc.quantity)
			}

			// Delete one (the extreme one) and verify rest survive.
			existed, err := store.DeleteRecord(tuple.Tuple{int64(3)})
			Expect(err).NotTo(HaveOccurred())
			Expect(existed).To(BeTrue())

			entries, err = AsList(ctx, store.ScanIndex(mdIdx, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(4))

			found = make(map[coordPair]bool)
			for _, e := range entries {
				x := e.Key[0].(int64)
				y := e.Key[1].(int64)
				found[coordPair{x, y}] = true
			}
			Expect(found).NotTo(HaveKey(coordPair{math.MaxInt64, math.MinInt64}))
			Expect(found).To(HaveKey(coordPair{math.MaxInt32, math.MinInt32}))
			Expect(found).To(HaveKey(coordPair{-100, -200}))
			Expect(found).To(HaveKey(coordPair{0, 0}))
			Expect(found).To(HaveKey(coordPair{1, -1}))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("duplicate coordinate points with different PKs", func() {
		ks := specSubspace()

		dimExpr := Dimensions(Concat(Field("coord_x"), Field("coord_y")), 0, 2)
		mdIdx := NewMultidimensionalIndex("md_dup_coords", dimExpr)
		builder := baseMetaData()
		builder.AddIndex("Order", mdIdx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			// Save two orders with identical (price=100, quantity=10) but different PKs.
			_, err = store.SaveRecord(&gen.Order{
				OrderId: proto.Int64(1),
				CoordX:  proto.Int64(100),
				CoordY:  proto.Int64(10),
			})
			Expect(err).NotTo(HaveOccurred())

			_, err = store.SaveRecord(&gen.Order{
				OrderId: proto.Int64(2),
				CoordX:  proto.Int64(100),
				CoordY:  proto.Int64(10),
			})
			Expect(err).NotTo(HaveOccurred())

			// Scan — both entries must be present.
			entries, err := AsList(ctx, store.ScanIndex(mdIdx, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(2))

			// Both entries have the same coordinates but different PK in the key suffix.
			for _, e := range entries {
				Expect(e.Key[0]).To(Equal(int64(100)))
				Expect(e.Key[1]).To(Equal(int64(10)))
			}

			// Delete one. The other must survive.
			existed, err := store.DeleteRecord(tuple.Tuple{int64(1)})
			Expect(err).NotTo(HaveOccurred())
			Expect(existed).To(BeTrue())

			entries, err = AsList(ctx, store.ScanIndex(mdIdx, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(1))
			Expect(entries[0].Key[0]).To(Equal(int64(100)))
			Expect(entries[0].Key[1]).To(Equal(int64(10)))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("DeleteAllRecords clears R-tree completely", func() {
		ks := specSubspace()

		dimExpr := Dimensions(Concat(Field("coord_x"), Field("coord_y")), 0, 2)
		mdIdx := NewMultidimensionalIndex("md_delete_all", dimExpr)
		mdIdx.Options[IndexOptionRTreeMaxM] = "4"
		mdIdx.Options[IndexOptionRTreeMinM] = "2"
		mdIdx.Options[IndexOptionRTreeSplitS] = "2"

		builder := baseMetaData()
		builder.AddIndex("Order", mdIdx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			// Save 20 records (enough for multi-level R-tree with MaxM=4).
			const n = 20
			for i := 1; i <= n; i++ {
				_, err = store.SaveRecord(&gen.Order{
					OrderId: proto.Int64(int64(i)),
					CoordX:  proto.Int64(int64(i * 50)),
					CoordY:  proto.Int64(int64(i * 7)),
				})
				Expect(err).NotTo(HaveOccurred())
			}

			entries, err := AsList(ctx, store.ScanIndex(mdIdx, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(n))

			// DeleteAllRecords.
			Expect(store.DeleteAllRecords()).To(Succeed())

			// Index should be empty.
			entries, err = AsList(ctx, store.ScanIndex(mdIdx, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(0))

			// Save new records — only new ones should appear.
			for i := 100; i < 105; i++ {
				_, err = store.SaveRecord(&gen.Order{
					OrderId: proto.Int64(int64(i)),
					CoordX:  proto.Int64(int64(i * 10)),
					CoordY:  proto.Int64(int64(i * 3)),
				})
				Expect(err).NotTo(HaveOccurred())
			}

			entries, err = AsList(ctx, store.ScanIndex(mdIdx, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(5))

			type coordPair struct{ x, y int64 }
			found := make(map[coordPair]bool)
			for _, e := range entries {
				x := e.Key[0].(int64)
				y := e.Key[1].(int64)
				found[coordPair{x, y}] = true
			}
			for i := 100; i < 105; i++ {
				Expect(found).To(HaveKey(coordPair{int64(i * 10), int64(i * 3)}))
			}

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("scan with MBR predicate from scanRange prunes subtrees", func() {
		ks := specSubspace()

		dimExpr := Dimensions(Concat(Field("coord_x"), Field("coord_y")), 0, 2)
		mdIdx := NewMultidimensionalIndex("md_mbr_scan", dimExpr)
		mdIdx.Options[IndexOptionRTreeMaxM] = "4"
		mdIdx.Options[IndexOptionRTreeMinM] = "2"
		mdIdx.Options[IndexOptionRTreeSplitS] = "2"

		builder := baseMetaData()
		builder.AddIndex("Order", mdIdx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			// Insert 30 records spread across coordinate space.
			const n = 30
			for i := 1; i <= n; i++ {
				_, err = store.SaveRecord(&gen.Order{
					OrderId: proto.Int64(int64(i)),
					CoordX:  proto.Int64(int64(i * 10)),
					CoordY:  proto.Int64(int64(i * 10)),
				})
				Expect(err).NotTo(HaveOccurred())
			}

			// Scan with spatial bounds [0, 100] x [0, 100].
			// This should return items with (price, quantity) in that range via MBR pruning.
			spatialRange := TupleRange{
				Low:  tuple.Tuple{int64(0), int64(0)},
				High: tuple.Tuple{int64(100), int64(100)},
			}
			entries, err := AsList(ctx, store.ScanIndex(mdIdx, spatialRange, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())

			// Items with (i*10, i*10) where i*10 <= 100 → i = 1..10 → 10 items must be present.
			// MBR predicate only prunes intermediate subtrees, not leaf items,
			// so we may get extra items sharing leaves with qualifying ones.
			expectedCount := 10
			foundExpected := 0
			for _, e := range entries {
				x := e.Key[0].(int64)
				y := e.Key[1].(int64)
				if x >= 0 && x <= 100 && y >= 0 && y <= 100 {
					foundExpected++
				}
			}
			Expect(foundExpected).To(Equal(expectedCount),
				"all 10 qualifying items must be in results")

			// Scan all (no bounds) to compare.
			allEntries, err := AsList(ctx, store.ScanIndex(mdIdx, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(allEntries).To(HaveLen(n))

			// With MBR pruning on a multi-level tree (MaxM=4, 30 items),
			// the bounded scan should return fewer entries than the full scan.
			Expect(len(entries)).To(BeNumerically("<", len(allEntries)),
				"MBR-bounded scan should return fewer entries than full scan")

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("scan with one-sided MBR bounds from scanRange", func() {
		ks := specSubspace()

		dimExpr := Dimensions(Concat(Field("coord_x"), Field("coord_y")), 0, 2)
		mdIdx := NewMultidimensionalIndex("md_mbr_onesided", dimExpr)
		builder := baseMetaData()
		builder.AddIndex("Order", mdIdx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			// Save 5 orders.
			for i := 1; i <= 5; i++ {
				_, err = store.SaveRecord(&gen.Order{
					OrderId: proto.Int64(int64(i)),
					CoordX:  proto.Int64(int64(i * 100)),
					CoordY:  proto.Int64(int64(i * 10)),
				})
				Expect(err).NotTo(HaveOccurred())
			}

			// Scan with only Low bound: [200, 20] to +inf.
			// Items: (100,10), (200,20), (300,30), (400,40), (500,50)
			// Filter: coord >= [200, 20] → items 2,3,4,5 match.
			lowOnly := TupleRange{
				Low: tuple.Tuple{int64(200), int64(20)},
			}
			entries, err := AsList(ctx, store.ScanIndex(mdIdx, lowOnly, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(4))

			// Scan with only High bound: -inf to [300, 30].
			// Filter: coord <= [300, 30] → items 1,2,3 match.
			highOnly := TupleRange{
				High: tuple.Tuple{int64(300), int64(30)},
			}
			entries, err = AsList(ctx, store.ScanIndex(mdIdx, highOnly, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(3))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("scan with MBR predicate and continuation tokens", func() {
		ks := specSubspace()

		dimExpr := Dimensions(Concat(Field("coord_x"), Field("coord_y")), 0, 2)
		mdIdx := NewMultidimensionalIndex("md_mbr_cont", dimExpr)
		mdIdx.Options[IndexOptionRTreeMaxM] = "4"
		mdIdx.Options[IndexOptionRTreeMinM] = "2"
		mdIdx.Options[IndexOptionRTreeSplitS] = "2"

		builder := baseMetaData()
		builder.AddIndex("Order", mdIdx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			// Insert 20 records.
			const n = 20
			for i := 1; i <= n; i++ {
				_, err = store.SaveRecord(&gen.Order{
					OrderId: proto.Int64(int64(i)),
					CoordX:  proto.Int64(int64(i * 10)),
					CoordY:  proto.Int64(int64(i * 10)),
				})
				Expect(err).NotTo(HaveOccurred())
			}

			// Scan with MBR bounds and a row limit, then continue.
			spatialRange := TupleRange{
				Low:  tuple.Tuple{int64(0), int64(0)},
				High: tuple.Tuple{int64(200), int64(200)},
			}
			props := ScanProperties{
				ExecuteProperties: DefaultExecuteProperties().WithReturnedRowLimit(5),
			}

			// Page 1.
			page1, cont1, err := AsListWithContinuation(ctx, store.ScanIndex(
				mdIdx, spatialRange, nil, props))
			Expect(err).NotTo(HaveOccurred())
			Expect(page1).To(HaveLen(5))
			Expect(cont1).NotTo(BeNil())

			// Page 2.
			page2, _, err := AsListWithContinuation(ctx, store.ScanIndex(
				mdIdx, spatialRange, cont1, props))
			Expect(err).NotTo(HaveOccurred())
			Expect(len(page2)).To(BeNumerically(">", 0), "page 2 should have entries")

			// No duplicates between pages.
			type coordPair struct{ x, y int64 }
			seen := make(map[coordPair]bool)
			for _, e := range page1 {
				cp := coordPair{e.Key[0].(int64), e.Key[1].(int64)}
				seen[cp] = true
			}
			for _, e := range page2 {
				cp := coordPair{e.Key[0].(int64), e.Key[1].(int64)}
				Expect(seen).NotTo(HaveKey(cp), "duplicate entry in page 2: (%d, %d)", cp.x, cp.y)
			}

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("prefix skip-scan enumerates all prefixes", func() {
		ks := specSubspace()

		// quantity is prefix (PrefixSize=1), price is 1D spatial dimension.
		dimExpr := Dimensions(Concat(Field("quantity"), Field("coord_x")), 1, 1)
		mdIdx := NewMultidimensionalIndex("md_prefix_skip", dimExpr)
		builder := baseMetaData()
		builder.AddIndex("Order", mdIdx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			// Save orders with 3 distinct quantity values (prefixes).
			// quantity=10: orders 1,2,3; quantity=20: orders 4,5; quantity=30: orders 6,7,8.
			orders := []struct {
				id       int64
				price    int64
				quantity int32
			}{
				{1, 100, 10},
				{2, 200, 10},
				{3, 300, 10},
				{4, 400, 20},
				{5, 500, 20},
				{6, 600, 30},
				{7, 700, 30},
				{8, 800, 30},
			}

			for _, o := range orders {
				_, err = store.SaveRecord(&gen.Order{
					OrderId:  proto.Int64(o.id),
					CoordX:   proto.Int64(o.price),
					Quantity: proto.Int32(o.quantity),
				})
				Expect(err).NotTo(HaveOccurred())
			}

			// Scan with TupleRangeAll (no prefix specified) — triggers prefix skip-scan.
			entries, err := AsList(ctx, store.ScanIndex(mdIdx, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(8))

			// Verify all entries from all prefixes are present.
			type entry struct{ qty, price int64 }
			found := make(map[entry]bool)
			for _, e := range entries {
				qty := e.Key[0].(int64)
				price := e.Key[1].(int64)
				found[entry{qty, price}] = true
			}
			for _, o := range orders {
				Expect(found).To(HaveKey(entry{int64(o.quantity), int64(o.price)}),
					"missing order %d at (qty=%d, price=%d)", o.id, o.quantity, o.price)
			}

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("prefix skip-scan with specific prefix scans only that prefix", func() {
		ks := specSubspace()

		dimExpr := Dimensions(Concat(Field("quantity"), Field("coord_x")), 1, 1)
		mdIdx := NewMultidimensionalIndex("md_prefix_specific", dimExpr)
		builder := baseMetaData()
		builder.AddIndex("Order", mdIdx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			orders := []struct {
				id       int64
				price    int64
				quantity int32
			}{
				{1, 100, 10},
				{2, 200, 10},
				{3, 300, 20},
				{4, 400, 20},
				{5, 500, 20},
				{6, 600, 30},
			}

			for _, o := range orders {
				_, err = store.SaveRecord(&gen.Order{
					OrderId:  proto.Int64(o.id),
					CoordX:   proto.Int64(o.price),
					Quantity: proto.Int32(o.quantity),
				})
				Expect(err).NotTo(HaveOccurred())
			}

			// Scan with a specific prefix (quantity=20) — should return only orders 3,4,5.
			specificPrefix := TupleRange{
				Low:  tuple.Tuple{int64(20)},
				High: tuple.Tuple{int64(20)},
			}
			entries, err := AsList(ctx, store.ScanIndex(mdIdx, specificPrefix, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(3))

			for _, e := range entries {
				Expect(e.Key[0]).To(Equal(int64(20)), "all entries should have quantity=20")
			}

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("prefix skip-scan with row limit", func() {
		ks := specSubspace()

		dimExpr := Dimensions(Concat(Field("quantity"), Field("coord_x")), 1, 1)
		mdIdx := NewMultidimensionalIndex("md_prefix_limit", dimExpr)
		builder := baseMetaData()
		builder.AddIndex("Order", mdIdx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			// Save 12 orders across 3 prefixes.
			for i := 1; i <= 12; i++ {
				qty := int32(((i-1)/4 + 1) * 10) // 10, 10, 10, 10, 20, 20, 20, 20, 30, 30, 30, 30
				_, err = store.SaveRecord(&gen.Order{
					OrderId:  proto.Int64(int64(i)),
					CoordX:   proto.Int64(int64(i * 100)),
					Quantity: proto.Int32(qty),
				})
				Expect(err).NotTo(HaveOccurred())
			}

			// Scan with limit=5 and no prefix (prefix skip-scan).
			props := ScanProperties{
				ExecuteProperties: DefaultExecuteProperties().WithReturnedRowLimit(5),
			}
			entries, err := AsList(ctx, store.ScanIndex(mdIdx, TupleRangeAll, nil, props))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(5), "should return exactly 5 entries with limit=5")

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("prefix skip-scan paginates across prefixes without duplicating or replaying rows", func() {
		// Regression test for the cross-prefix continuation bug: a
		// ReturnLimitReached page boundary used to hand back an empty
		// placeholder continuation the reconstruction gate reads as "no
		// continuation" (multidimensionalIndexMaintainer.Scan parses
		// len(continuation) > 0), silently restarting prefix enumeration from
		// scratch every page — replaying prefix #1 forever instead of
		// advancing. 6 partitions x 2 rows, paginated 2 rows at a time,
		// forces the row limit to land mid-skip-scan on every page.
		ks := specSubspace()

		dimExpr := Dimensions(Concat(Field("quantity"), Field("coord_x")), 1, 1)
		mdIdx := NewMultidimensionalIndex("md_prefix_paginate", dimExpr)
		builder := baseMetaData()
		builder.AddIndex("Order", mdIdx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			// 6 distinct quantity prefixes (10..60), 2 orders each = 12 rows.
			const numPrefixes = 6
			const perPrefix = 2
			type coordKey struct{ qty, price int64 }
			expected := make(map[coordKey]bool)
			id := int64(1)
			for p := 1; p <= numPrefixes; p++ {
				qty := int32(p * 10)
				for j := 1; j <= perPrefix; j++ {
					price := int64(p*1000 + j)
					_, err = store.SaveRecord(&gen.Order{
						OrderId:  proto.Int64(id),
						CoordX:   proto.Int64(price),
						Quantity: proto.Int32(qty),
					})
					Expect(err).NotTo(HaveOccurred())
					expected[coordKey{int64(qty), int64(price)}] = true
					id++
				}
			}
			total := numPrefixes * perPrefix

			props := ScanProperties{
				ExecuteProperties: DefaultExecuteProperties().WithReturnedRowLimit(2),
			}

			seen := make(map[coordKey]int)
			var cont []byte
			pages := 0
			// Bounded loop: a real fix terminates in exactly total/limit pages;
			// a duplicate-replay bug never terminates, so cap generously above
			// that and fail loudly instead of hanging forever.
			const maxPages = 50
			for {
				pages++
				Expect(pages).To(BeNumerically("<=", maxPages),
					"scan did not terminate after %d pages — replaying a prefix instead of advancing", maxPages)

				page, nextCont, err := AsListWithContinuation(ctx, store.ScanIndex(
					mdIdx, TupleRangeAll, cont, props))
				Expect(err).NotTo(HaveOccurred())

				for _, e := range page {
					qty := e.Key[0].(int64)
					price := e.Key[1].(int64)
					k := coordKey{qty, price}
					seen[k]++
					Expect(seen[k]).To(Equal(1),
						"row (qty=%d, price=%d) delivered more than once — cross-prefix continuation replayed a page", qty, price)
				}

				if nextCont == nil {
					break
				}
				cont = nextCont
			}

			Expect(seen).To(HaveLen(total), "every row must be delivered exactly once")
			for k := range expected {
				Expect(seen).To(HaveKey(k), "missing row (qty=%d, price=%d)", k.qty, k.price)
			}
			Expect(pages).To(BeNumerically(">", 1), "the row limit must force real multi-page pagination")

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("prefix skip-scan pagination is byte-identical to the unpaginated scan across row limits 1..7 over uneven prefix sizes", func() {
		// Fold-in from review: the prior pagination regression test used 6
		// equal-size (2-row) prefixes with ReturnedRowLimit=2, so EVERY page
		// boundary was ALSO a prefix boundary — it only proved the `outer`
		// (which prefix) half of the fix advances. The novel, previously-buggy
		// half is the (priorPrefixBytes, inner) PAIRING: a wrong prior-prefix
		// value silently drops or duplicates a prefix's tail the moment a page
		// boundary lands MID-prefix rather than exactly at one.
		//
		// Uneven prefix sizes {1,4,2,5,1,3} swept against row limits 1..7
		// force every kind of boundary at least once: mid-prefix (most
		// combinations), exactly-at-a-prefix-boundary (e.g. limit=1 against
		// the size-1 prefixes), a single-row prefix consumed in one page
		// (limit>=1 on prefixes of size 1), and a limit that spans multiple
		// prefixes in one page (limit=7 crosses at least two). Comparing the
		// full paginated sequence — not just membership — against the
		// unpaginated scan at every limit catches a wrong `outer` producing
		// either a dropped tail (missing rows) or a replayed tail (duplicate
		// rows out of place), not just a wrong total count.
		ks := specSubspace()

		dimExpr := Dimensions(Concat(Field("quantity"), Field("coord_x")), 1, 1)
		mdIdx := NewMultidimensionalIndex("md_prefix_sweep", dimExpr)
		builder := baseMetaData()
		builder.AddIndex("Order", mdIdx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			prefixCounts := []int{1, 4, 2, 5, 1, 3}
			total := 0
			id := int64(1)
			for p, n := range prefixCounts {
				qty := int32((p + 1) * 10)
				for j := 0; j < n; j++ {
					price := int64((p+1)*1000 + j)
					_, err = store.SaveRecord(&gen.Order{
						OrderId:  proto.Int64(id),
						CoordX:   proto.Int64(price),
						Quantity: proto.Int32(qty),
					})
					Expect(err).NotTo(HaveOccurred())
					id++
					total++
				}
			}

			keyOf := func(e *IndexEntry) [2]int64 {
				return [2]int64{e.Key[0].(int64), e.Key[1].(int64)}
			}

			// Unpaginated baseline: the canonical order the skip-scan produces
			// with no row limit at all — the sequence every limited run below
			// must reproduce exactly.
			baseline, err := AsList(ctx, store.ScanIndex(mdIdx, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(baseline).To(HaveLen(total))

			for limit := 1; limit <= 7; limit++ {
				props := ScanProperties{
					ExecuteProperties: DefaultExecuteProperties().WithReturnedRowLimit(limit),
				}
				var got []*IndexEntry
				var cont []byte
				pages := 0
				// Bounded loop: a wrong outer/inner pairing can replay or skip
				// prefixes forever; fail loudly instead of hanging.
				const maxPages = 50
				for {
					pages++
					Expect(pages).To(BeNumerically("<=", maxPages),
						"limit=%d: scan did not terminate after %d pages", limit, maxPages)

					page, nextCont, perr := AsListWithContinuation(ctx, store.ScanIndex(
						mdIdx, TupleRangeAll, cont, props))
					Expect(perr).NotTo(HaveOccurred())
					Expect(len(page)).To(BeNumerically("<=", limit),
						"limit=%d: page delivered %d rows, exceeding the row limit", limit, len(page))

					got = append(got, page...)
					if nextCont == nil {
						break
					}
					cont = nextCont
				}

				Expect(got).To(HaveLen(len(baseline)),
					"limit=%d: total row count diverges from the unpaginated scan", limit)
				for i := range baseline {
					Expect(keyOf(got[i])).To(Equal(keyOf(baseline[i])),
						"limit=%d: row %d diverges from the unpaginated sequence (got %v, want %v) — "+
							"a wrong prior-prefix pairing dropped or replayed a prefix's tail",
						limit, i, keyOf(got[i]), keyOf(baseline[i]))
				}
			}

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("prefix skip-scan with empty index returns empty", func() {
		ks := specSubspace()

		dimExpr := Dimensions(Concat(Field("quantity"), Field("coord_x")), 1, 1)
		mdIdx := NewMultidimensionalIndex("md_prefix_empty", dimExpr)
		builder := baseMetaData()
		builder.AddIndex("Order", mdIdx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			// No records saved — scan should return empty.
			entries, err := AsList(ctx, store.ScanIndex(mdIdx, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(0))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("prefix skip-scan after delete from one prefix", func() {
		ks := specSubspace()

		dimExpr := Dimensions(Concat(Field("quantity"), Field("coord_x")), 1, 1)
		mdIdx := NewMultidimensionalIndex("md_prefix_delete", dimExpr)
		builder := baseMetaData()
		builder.AddIndex("Order", mdIdx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			orders := []struct {
				id       int64
				price    int64
				quantity int32
			}{
				{1, 100, 10},
				{2, 200, 10},
				{3, 300, 20},
				{4, 400, 20},
			}

			for _, o := range orders {
				_, err = store.SaveRecord(&gen.Order{
					OrderId:  proto.Int64(o.id),
					CoordX:   proto.Int64(o.price),
					Quantity: proto.Int32(o.quantity),
				})
				Expect(err).NotTo(HaveOccurred())
			}

			// Delete both orders in prefix quantity=10.
			for _, id := range []int64{1, 2} {
				existed, err := store.DeleteRecord(tuple.Tuple{id})
				Expect(err).NotTo(HaveOccurred())
				Expect(existed).To(BeTrue())
			}

			// Prefix skip-scan should only find the quantity=20 entries.
			entries, err := AsList(ctx, store.ScanIndex(mdIdx, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(2))

			for _, e := range entries {
				Expect(e.Key[0]).To(Equal(int64(20)))
			}

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("RebuildIndex for MULTIDIMENSIONAL", func() {
		ks := specSubspace()

		dimExpr := Dimensions(Concat(Field("coord_x"), Field("coord_y")), 0, 2)
		mdIdx := NewMultidimensionalIndex("md_rebuild", dimExpr)
		builder := baseMetaData()
		builder.AddIndex("Order", mdIdx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			// Save 10 records.
			const n = 10
			for i := 1; i <= n; i++ {
				_, err = store.SaveRecord(&gen.Order{
					OrderId: proto.Int64(int64(i)),
					CoordX:  proto.Int64(int64(i * 100)),
					CoordY:  proto.Int64(int64(i * 10)),
				})
				Expect(err).NotTo(HaveOccurred())
			}

			// Verify all entries present before rebuild.
			entries, err := AsList(ctx, store.ScanIndex(mdIdx, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(n))

			// Rebuild the index (WRITE_ONLY -> re-index -> READABLE).
			Expect(store.RebuildIndex(mdIdx)).To(Succeed())

			// Verify all 10 entries match after rebuild.
			entries, err = AsList(ctx, store.ScanIndex(mdIdx, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(n))

			type coordPair struct{ x, y int64 }
			found := make(map[coordPair]bool)
			for _, e := range entries {
				x := e.Key[0].(int64)
				y := e.Key[1].(int64)
				found[coordPair{x, y}] = true
			}
			for i := 1; i <= n; i++ {
				Expect(found).To(HaveKey(coordPair{int64(i * 100), int64(i * 10)}),
					"missing entry for order %d after rebuild", i)
			}

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})
})

var _ = Describe("DimensionsKeyExpression", func() {
	It("proto round-trip preserves expression", func() {
		wholeKey := Concat(Field("price"), Field("quantity"))
		dimExpr := Dimensions(wholeKey, 0, 2)

		// Serialize to proto.
		protoExpr := dimExpr.ToKeyExpression()
		Expect(protoExpr).NotTo(BeNil())
		Expect(protoExpr.Dimensions).NotTo(BeNil())
		Expect(protoExpr.Dimensions.GetPrefixSize()).To(Equal(int32(0)))
		Expect(protoExpr.Dimensions.GetDimensionsSize()).To(Equal(int32(2)))

		// Deserialize from proto.
		restored, err := KeyExpressionFromProto(protoExpr)
		Expect(err).NotTo(HaveOccurred())

		restoredDim, ok := restored.(*DimensionsKeyExpression)
		Expect(ok).To(BeTrue(), "restored expression should be *DimensionsKeyExpression")
		Expect(restoredDim.PrefixSize).To(Equal(0))
		Expect(restoredDim.DimensionsSize).To(Equal(2))
		Expect(restoredDim.ColumnSize()).To(Equal(dimExpr.ColumnSize()))
		Expect(restoredDim.FieldNames()).To(Equal(dimExpr.FieldNames()))
	})

	It("proto round-trip with prefix", func() {
		// 3-column key: 1 prefix + 2 dimensions
		wholeKey := Concat(Field("tags"), Field("price"), Field("quantity"))
		dimExpr := Dimensions(wholeKey, 1, 2)

		protoExpr := dimExpr.ToKeyExpression()
		restored, err := KeyExpressionFromProto(protoExpr)
		Expect(err).NotTo(HaveOccurred())

		restoredDim := restored.(*DimensionsKeyExpression)
		Expect(restoredDim.PrefixSize).To(Equal(1))
		Expect(restoredDim.DimensionsSize).To(Equal(2))
		Expect(restoredDim.SuffixSize()).To(Equal(0))
		Expect(restoredDim.ColumnSize()).To(Equal(3))
	})

	It("SplitIndexEntry correctly partitions tuple", func() {
		dimExpr := Dimensions(Concat(Field("price"), Field("quantity")), 0, 2)

		entry := tuple.Tuple{int64(100), int64(200)}
		prefix, dims, suffix := dimExpr.SplitIndexEntry(entry)

		Expect(prefix).To(BeNil())
		Expect(dims).To(Equal(tuple.Tuple{int64(100), int64(200)}))
		Expect(suffix).To(BeNil())
	})

	It("SplitIndexEntry with prefix and suffix", func() {
		// 4-column: 1 prefix, 2 dimensions, 1 suffix
		wholeKey := Concat(Field("tags"), Field("price"), Field("quantity"), Field("order_id"))
		dimExpr := Dimensions(wholeKey, 1, 2)

		entry := tuple.Tuple{"group1", int64(100), int64(200), int64(42)}
		prefix, dims, suffix := dimExpr.SplitIndexEntry(entry)

		Expect(prefix).To(Equal(tuple.Tuple{"group1"}))
		Expect(dims).To(Equal(tuple.Tuple{int64(100), int64(200)}))
		Expect(suffix).To(Equal(tuple.Tuple{int64(42)}))
	})
})
