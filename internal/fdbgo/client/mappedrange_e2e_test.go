package client

import (
	"bytes"
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/onsi/gomega"
)

// End-to-end mapped reads against a real storage server. The guard tests in
// mappedrange_guards_test.go all return before a getMappedKeyValues request is
// built; everything here puts one on the wire and reads back rows the server
// actually resolved, which is the only way the two union arms, the
// point-lookup Present flag, the reply-byte continuation and the conflict-range
// narrowing get exercised at all.
//
// The oracle is deliberately NOT libfdb_c here. libfdb_c's public
// FDBMappedKeyValue models only the getRange arm — "It's complicated to map a
// std::variant to C. For now we assume the underlying requests are always
// getRange and take the shortcut" (fdb_c.h) — so for a point-lookup mapper the
// C client hands back a getRange struct overlaid on getValue bytes. The oracle
// used instead is the server's own truth: the same transaction re-reads each
// secondary key with an ordinary Get / GetRange and the two must agree. That is
// a stronger check for the getValue arm, because it is what the mapped read is
// supposed to be a shortcut FOR.

// ---------------------------------------------------------------------------
// Fixture
// ---------------------------------------------------------------------------
//
// The layout is FDB's own GetMappedRange workload
// (fdbserver/workloads/GetMappedRange.actor.cpp:88-95), which is what the
// mapper syntax was designed around:
//
//	index entry:  (prefix, "IDX", indexKey(i), primaryKey(i)) -> ""
//	record:       (prefix, "REC", primaryKey(i))              -> value
//	split record: (prefix, "REC", primaryKey(i), split)       -> value
//
// so {K[3]} in a mapper picks primaryKey(i) out of the index entry's KEY.

var (
	mtIndexPart  = []byte("IDX")
	mtRecordPart = []byte("REC")
)

// mtPack packs a tuple of byte strings. Only the BYTES type code (0x01) is
// needed: every element of the fixture and of every mapper below is a byte
// string, and hand-packing keeps the exact wire bytes the storage server will
// unpack visible in the test rather than hidden behind a codec.
func mtPack(parts ...[]byte) []byte {
	out := make([]byte, 0, 32)
	for _, p := range parts {
		out = append(out, 0x01)
		for _, b := range p {
			out = append(out, b)
			if b == 0x00 {
				out = append(out, 0xFF)
			}
		}
		out = append(out, 0x00)
	}
	return out
}

// mtStrinc is C++ strinc: the smallest key greater than every key with this
// prefix. quickGetKeyValues uses it for the secondary range's End selector
// (storageserver.actor.cpp:4744), so the expected End in every getRange-arm
// assertion is computed the same way the server computes it.
func mtStrinc(k []byte) []byte {
	out := append([]byte{}, k...)
	for i := len(out) - 1; i >= 0; i-- {
		if out[i] != 0xFF {
			out[i]++
			return out[:i+1]
		}
	}
	panic("mtStrinc: key is all 0xFF")
}

type mappedFixture struct {
	prefix []byte
	n      int
}

func newMappedFixture(t *testing.T, n int) mappedFixture {
	return mappedFixture{prefix: []byte(t.Name()), n: n}
}

func (f mappedFixture) primaryKey(i int) []byte { return []byte(fmt.Sprintf("pk-%08d", i)) }
func (f mappedFixture) indexKey(i int) []byte   { return []byte(fmt.Sprintf("ik-%08d", i)) }

func (f mappedFixture) indexEntryKey(i int) []byte {
	return mtPack(f.prefix, mtIndexPart, f.indexKey(i), f.primaryKey(i))
}

func (f mappedFixture) recordKey(i int) []byte {
	return mtPack(f.prefix, mtRecordPart, f.primaryKey(i))
}

func (f mappedFixture) splitRecordKey(i, split int) []byte {
	return mtPack(f.prefix, mtRecordPart, f.primaryKey(i), []byte(fmt.Sprintf("s%d", split)))
}

// indexRange is the PRIMARY range a mapped read scans.
func (f mappedFixture) indexRange() (begin, end []byte) {
	begin = mtPack(f.prefix, mtIndexPart)
	return begin, mtStrinc(begin)
}

// getValueMapper resolves each index entry to the SINGLE record key
// (prefix, "REC", primaryKey). No "{...}", so mapSubquery takes the
// quickGetValue arm (storageserver.actor.cpp:5928-5931).
func (f mappedFixture) getValueMapper() []byte {
	return mtPack(f.prefix, mtRecordPart, []byte("{K[3]}"))
}

// getRangeMapper appends the "{...}" range descriptor, which is the ONLY thing
// that flips isRangeQuery and therefore the union arm (preprocessMappedKey,
// storageserver.actor.cpp:4894-4899).
func (f mappedFixture) getRangeMapper() []byte {
	return mtPack(f.prefix, mtRecordPart, []byte("{K[3]}"), []byte("{...}"))
}

// seed writes the fixture in its own committed transaction, so a later mapped
// read sees it at its own read version with no writes in its own write map.
// recordValue returning nil for an i means "no record for this index entry",
// which is how the absent-secondary case is built.
func (f mappedFixture) seed(t *testing.T, ctx context.Context, db *Database, split int, recordValue func(i int) []byte) {
	t.Helper()
	_, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		for i := 0; i < f.n; i++ {
			tx.Set(f.indexEntryKey(i), []byte{})
			v := recordValue(i)
			if v == nil {
				continue
			}
			if split == 0 {
				tx.Set(f.recordKey(i), v)
				continue
			}
			for s := 0; s < split; s++ {
				tx.Set(f.splitRecordKey(i, s), append(append([]byte{}, v...), byte('0'+s)))
			}
		}
		return nil, nil
	})
	if err != nil {
		t.Fatalf("seed fixture: %v", err)
	}
}

func mappedTestCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 120*time.Second)
}

// ---------------------------------------------------------------------------
// The two union arms
// ---------------------------------------------------------------------------

// TestGetMappedRange_GetRangeArm_MatchesPerRowRangeReads is the range-descriptor
// arm end to end. Every assertion about a secondary read is made against the
// SAME transaction re-reading that range the ordinary way, so the mapped read is
// checked against what a client would have got by paying the round trips.
//
// The resolved selectors are pinned too, not just the rows: quickGetKeyValues
// turns the mapped key into firstGreaterOrEqual(mappedKey) ..
// firstGreaterOrEqual(strinc(mappedKey)) (storageserver.actor.cpp:4743-4744),
// and those exact selectors are what addMappedRangeConflicts records as the
// secondary conflict range — so decoding them wrongly would silently mis-scope
// isolation, not just mis-report a bound.
func TestGetMappedRange_GetRangeArm_MatchesPerRowRangeReads(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	ctx, cancel := mappedTestCtx()
	defer cancel()

	db := openTestDB(t, ctx)
	defer db.Close()

	f := newMappedFixture(t, 5)
	f.seed(t, ctx, db, 3, func(i int) []byte { return []byte(fmt.Sprintf("rec-%08d-", i)) })

	tx := db.CreateTransaction()
	defer tx.Cancel()

	begin, end := f.indexRange()
	rows, more, err := tx.GetMappedRange(ctx, begin, end, f.getRangeMapper(), 0, false)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(more).To(gomega.BeFalse(), "an unlimited read over 5 entries is not truncated")
	g.Expect(rows).To(gomega.HaveLen(5))

	for i := range rows {
		row := rows[i]
		g.Expect(row.Kind).To(gomega.Equal(MappedResultGetRange),
			"row %d: a mapper ending in {...} must take the getRange arm", i)
		g.Expect(row.Key).To(gomega.Equal(f.indexEntryKey(i)), "row %d primary key", i)

		wantPrefix := f.recordKey(i)
		g.Expect(row.GetRange.Begin.Key).To(gomega.Equal(wantPrefix), "row %d secondary begin key", i)
		g.Expect(row.GetRange.Begin.OrEqual).To(gomega.BeFalse())
		g.Expect(row.GetRange.Begin.Offset).To(gomega.Equal(int32(1)), "firstGreaterOrEqual has offset 1")
		g.Expect(row.GetRange.End.Key).To(gomega.Equal(mtStrinc(wantPrefix)), "row %d secondary end key", i)
		g.Expect(row.GetRange.End.Offset).To(gomega.Equal(int32(1)))

		// Oracle: the same range, read the ordinary way, in the same transaction.
		want, wantMore, err := tx.GetRange(ctx, wantPrefix, mtStrinc(wantPrefix), 0)
		g.Expect(err).NotTo(gomega.HaveOccurred())
		g.Expect(wantMore).To(gomega.BeFalse())
		g.Expect(want).To(gomega.HaveLen(3), "row %d: fixture writes 3 split records", i)
		g.Expect(row.GetRange.Rows).To(gomega.Equal(want),
			"row %d: the mapped secondary range must equal an ordinary GetRange over the same bounds", i)
		g.Expect(row.GetRange.More).To(gomega.BeFalse())
	}
}

// TestGetMappedRange_GetValueArm_MatchesPerRowPointGets is the point-lookup arm,
// which libfdb_c cannot represent at all. The oracle is an ordinary Get per row.
func TestGetMappedRange_GetValueArm_MatchesPerRowPointGets(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	ctx, cancel := mappedTestCtx()
	defer cancel()

	db := openTestDB(t, ctx)
	defer db.Close()

	f := newMappedFixture(t, 5)
	f.seed(t, ctx, db, 0, func(i int) []byte { return []byte(fmt.Sprintf("rec-%08d", i)) })

	tx := db.CreateTransaction()
	defer tx.Cancel()

	begin, end := f.indexRange()
	rows, more, err := tx.GetMappedRange(ctx, begin, end, f.getValueMapper(), 0, false)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(more).To(gomega.BeFalse())
	g.Expect(rows).To(gomega.HaveLen(5))

	for i := range rows {
		row := rows[i]
		g.Expect(row.Kind).To(gomega.Equal(MappedResultGetValue),
			"row %d: a mapper with no {...} must take the getValue arm", i)
		g.Expect(row.GetValue.Key).To(gomega.Equal(f.recordKey(i)), "row %d mapped key", i)
		g.Expect(row.GetValue.Present).To(gomega.BeTrue(), "row %d: the record exists", i)

		want, err := tx.Get(ctx, f.recordKey(i))
		g.Expect(err).NotTo(gomega.HaveOccurred())
		g.Expect(row.GetValue.Value).To(gomega.Equal(want),
			"row %d: the mapped point lookup must equal an ordinary Get of the same key", i)
	}
}

// TestGetMappedRange_GetValueArm_BlanksInteriorPrimaryRows pins a shape that is
// invisible from the C header and that a caller will otherwise trip over: on the
// getValue arm the storage server returns the primary row's key and value ONLY
// on the first and last rows of a reply.
//
// mapSubquery sets kvm->key/kvm->value only in its isRangeQuery branch
// (storageserver.actor.cpp:5922-5931); the getValue branch leaves them at the
// ""_sr that mapKeyValues cleared them to (:5991-5992). mapKeyValues then
// restores exactly two of them — "keep index for boundary index entries, so that
// caller can use it as a continuation" (:6024-6031). So the interior rows carry
// no index entry at all. (This is what MATCH_INDEX_ALL exists to change in 7.4;
// 7.3 has no such option — see the MATCH_INDEX note in mappedrange.go.)
//
// The two restored rows are not cosmetic: rangeScanImpl continues a truncated
// scan from keyAfter(lastRow.Key) and primaryConflictRange narrows on
// rows[0].Key / rows[len-1].Key. Both would be reading "" if the restore ever
// stopped happening, which is why this is pinned rather than tolerated.
func TestGetMappedRange_GetValueArm_BlanksInteriorPrimaryRows(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	ctx, cancel := mappedTestCtx()
	defer cancel()

	db := openTestDB(t, ctx)
	defer db.Close()

	f := newMappedFixture(t, 4)
	f.seed(t, ctx, db, 0, func(i int) []byte { return []byte(fmt.Sprintf("rec-%08d", i)) })

	tx := db.CreateTransaction()
	defer tx.Cancel()

	begin, end := f.indexRange()
	rows, _, err := tx.GetMappedRange(ctx, begin, end, f.getValueMapper(), 0, false)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(rows).To(gomega.HaveLen(4))

	g.Expect(rows[0].Key).To(gomega.Equal(f.indexEntryKey(0)),
		"the FIRST row keeps its index entry — it is the continuation anchor")
	g.Expect(rows[3].Key).To(gomega.Equal(f.indexEntryKey(3)),
		"the LAST row keeps its index entry — it is the continuation anchor")
	for _, i := range []int{1, 2} {
		g.Expect(rows[i].Key).To(gomega.BeEmpty(),
			"row %d is interior: 7.3 returns no index entry for a getValue-arm row that is not a boundary", i)
		g.Expect(rows[i].Value).To(gomega.BeEmpty(), "row %d interior primary value", i)
		// The SECONDARY read is fully populated on every row — only the primary
		// side is blanked, so the mapped read still answers the question asked.
		g.Expect(rows[i].GetValue.Key).To(gomega.Equal(f.recordKey(i)), "row %d mapped key is still present", i)
		g.Expect(rows[i].GetValue.Present).To(gomega.BeTrue())
	}
}

// TestGetMappedRange_GetRangeArm_KeepsEveryPrimaryRow is the other half of the
// blanking pin, and the reason it is a separate test: the range arm assigns
// kvm->key/value inside mapSubquery, so it is NOT blanked. Asserting only the
// getValue shape above would leave "the server blanks interior rows" reading
// like a property of mapped reads in general.
func TestGetMappedRange_GetRangeArm_KeepsEveryPrimaryRow(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	ctx, cancel := mappedTestCtx()
	defer cancel()

	db := openTestDB(t, ctx)
	defer db.Close()

	f := newMappedFixture(t, 4)
	f.seed(t, ctx, db, 1, func(i int) []byte { return []byte(fmt.Sprintf("rec-%08d", i)) })

	tx := db.CreateTransaction()
	defer tx.Cancel()

	begin, end := f.indexRange()
	rows, _, err := tx.GetMappedRange(ctx, begin, end, f.getRangeMapper(), 0, false)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(rows).To(gomega.HaveLen(4))
	for i := range rows {
		g.Expect(rows[i].Key).To(gomega.Equal(f.indexEntryKey(i)),
			"row %d: the getRange arm sets kvm->key for EVERY row, interior included", i)
	}
}

// TestGetMappedRange_AbsentSecondaryIsNotAnError refutes the obvious reading of
// mapper_no_such_key (2031), whose message is "A mapped key is not set in
// database". In 7.3 that code is never thrown: the only occurrence outside its
// definition is a retriability switch (storageserver.actor.cpp:153). A mapper
// that resolves to a key with no record yields Present=false and a successful
// read, so the absent case is reported in the DATA and a caller that only checks
// the error would silently treat a missing record as an empty one.
func TestGetMappedRange_AbsentSecondaryIsNotAnError(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	ctx, cancel := mappedTestCtx()
	defer cancel()

	db := openTestDB(t, ctx)
	defer db.Close()

	f := newMappedFixture(t, 4)
	// Only even i get a record; odd i are index entries pointing at nothing.
	f.seed(t, ctx, db, 0, func(i int) []byte {
		if i%2 == 1 {
			return nil
		}
		return []byte(fmt.Sprintf("rec-%08d", i))
	})

	tx := db.CreateTransaction()
	defer tx.Cancel()

	begin, end := f.indexRange()
	rows, _, err := tx.GetMappedRange(ctx, begin, end, f.getValueMapper(), 0, false)
	g.Expect(err).NotTo(gomega.HaveOccurred(), "a dangling index entry is not mapper_no_such_key in 7.3")
	g.Expect(rows).To(gomega.HaveLen(4))
	for i := range rows {
		g.Expect(rows[i].GetValue.Key).To(gomega.Equal(f.recordKey(i)),
			"row %d: the probed key is reported whether or not it existed", i)
		g.Expect(rows[i].GetValue.Present).To(gomega.Equal(i%2 == 0),
			"row %d: Present must distinguish a missing record from an empty one", i)
		if i%2 == 1 {
			g.Expect(rows[i].GetValue.Value).To(gomega.BeEmpty())
		}
	}

	// The same absence via the range arm is an EMPTY row vector, not an error and
	// not a missing row: the mapped read still reports the bounds it probed.
	tx2 := db.CreateTransaction()
	defer tx2.Cancel()
	rows2, _, err := tx2.GetMappedRange(ctx, begin, end, f.getRangeMapper(), 0, false)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(rows2).To(gomega.HaveLen(4))
	g.Expect(rows2[1].GetRange.Rows).To(gomega.BeEmpty(), "index entry 1 has no record; the probe still reports its bounds")
	g.Expect(rows2[1].GetRange.Begin.Key).To(gomega.Equal(f.recordKey(1)))
}

// ---------------------------------------------------------------------------
// Server-raised mapper errors, propagated unchanged
// ---------------------------------------------------------------------------

// TestGetMappedRange_ServerMapperErrorsPropagateUnchanged drives the mapper
// failure modes that need ROWS to reach — unlike mapper_not_tuple, which the
// server raises before its per-row loop and which the guards test already covers
// with an empty range.
//
// The client parses no mapper, so every code here is proof of propagation, not
// of validation: each is thrown from constructMappedKey / preprocessMappedKey
// and must arrive at the caller with its numeric code intact.
func TestGetMappedRange_ServerMapperErrorsPropagateUnchanged(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	ctx, cancel := mappedTestCtx()
	defer cancel()

	db := openTestDB(t, ctx)
	defer db.Close()

	f := newMappedFixture(t, 3)
	f.seed(t, ctx, db, 0, func(i int) []byte { return []byte(fmt.Sprintf("rec-%08d", i)) })
	begin, end := f.indexRange()

	// Two extra fixtures for the errors that are properties of the PRIMARY ROW
	// rather than of the mapper. Both live outside the index range above so they
	// cannot perturb the mapper-only cases.
	//
	//   rawBegin: a primary key that is not a packed tuple  -> key_not_tuple
	//   valBegin: a primary VALUE that is not a packed tuple -> value_not_tuple
	//
	// The index entries the fixture writes have an EMPTY value, and an empty
	// string unpacks fine as a zero-element tuple — {V[0]} on those would report
	// mapper_bad_index, not value_not_tuple, so a non-tuple value has to be
	// written explicitly. 0x6e ('n') is not a valid tuple type code.
	rawBegin := append([]byte(t.Name()), []byte("_raw/")...)
	rawEnd := mtStrinc(rawBegin)
	valBegin := append([]byte(t.Name()), []byte("_val/")...)
	valEnd := mtStrinc(valBegin)
	_, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.Set(append(append([]byte{}, rawBegin...), []byte("\xfe-not-a-tuple")...), []byte("v"))
		tx.Set(append(append([]byte{}, valBegin...), 'k'), []byte("not-a-tuple"))
		return nil, nil
	})
	g.Expect(err).NotTo(gomega.HaveOccurred())

	cases := []struct {
		name         string
		begin, end   []byte
		mapper       []byte
		wantCode     int
		whyReachable string
	}{
		{
			name: "mapper_bad_index_out_of_range", begin: begin, end: end,
			mapper:   mtPack(f.prefix, mtRecordPart, []byte("{K[9]}")),
			wantCode: ErrMapperBadIndex,
			whyReachable: "the index entry tuple has 4 elements, so K[9] is out of range " +
				"(constructMappedKey, storageserver.actor.cpp:4943-4945)",
		},
		{
			name: "mapper_bad_index_not_a_number", begin: begin, end: end,
			mapper:       mtPack(f.prefix, mtRecordPart, []byte("{K[xx]}")),
			wantCode:     ErrMapperBadIndex,
			whyReachable: "std::stoi fails on the placeholder body (:4932-4936)",
		},
		{
			name: "mapper_bad_range_descriptor_not_last", begin: begin, end: end,
			mapper:   mtPack(f.prefix, []byte("{...}"), mtRecordPart),
			wantCode: ErrMapperBadRangeDescrptor,
			whyReachable: "{...} must be the last element (preprocessMappedKey, :4895-4897); " +
				"raised before any row is touched",
		},
		{
			name: "value_not_tuple", begin: valBegin, end: valEnd,
			mapper:       mtPack(f.prefix, mtRecordPart, []byte("{V[0]}")),
			wantCode:     ErrValueNotTuple,
			whyReachable: "the val fixture's primary value is not a packed tuple (unpackValueTuple, :4834-4844)",
		},
		{
			name: "key_not_tuple", begin: rawBegin, end: rawEnd,
			mapper:       mtPack(f.prefix, mtRecordPart, []byte("{K[0]}")),
			wantCode:     ErrKeyNotTuple,
			whyReachable: "the raw fixture's primary key is not a packed tuple (unpackKeyTuple, :4820-4830)",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := gomega.NewWithT(t)
			tx := db.CreateTransaction()
			defer tx.Cancel()
			_, _, err := tx.GetMappedRange(ctx, tc.begin, tc.end, tc.mapper, 0, false)
			g.Expect(err).To(gomega.HaveOccurred(), "expected the storage server to reject this mapper: %s", tc.whyReachable)
			g.Expect(mappedFDBErrCode(t, err)).To(gomega.Equal(tc.wantCode),
				"the server's code must arrive unchanged (%s)", tc.whyReachable)
		})
	}
}

// ---------------------------------------------------------------------------
// Reverse, limits, and multi-reply continuation
// ---------------------------------------------------------------------------

// TestGetMappedRange_Reverse_ReturnsDescendingRows exercises reverse against a
// server. The RYW-level `throw unsupported_operation()` for a backwards mapped
// read (ReadYourWrites.actor.cpp:1139-1141) sits in RYWImpl::read, the
// read-from-cache path; readWithConflictRangeRYW goes to readThrough instead
// (:1200), which passes Reverse straight to NativeAPI. So reverse is supported,
// and this is what proves it rather than the absence of an error.
func TestGetMappedRange_Reverse_ReturnsDescendingRows(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	ctx, cancel := mappedTestCtx()
	defer cancel()

	db := openTestDB(t, ctx)
	defer db.Close()

	f := newMappedFixture(t, 5)
	f.seed(t, ctx, db, 1, func(i int) []byte { return []byte(fmt.Sprintf("rec-%08d", i)) })

	tx := db.CreateTransaction()
	defer tx.Cancel()

	begin, end := f.indexRange()
	rows, more, err := tx.GetMappedRange(ctx, begin, end, f.getRangeMapper(), 0, true)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(more).To(gomega.BeFalse())
	g.Expect(rows).To(gomega.HaveLen(5))
	for i := range rows {
		want := f.n - 1 - i
		g.Expect(rows[i].Key).To(gomega.Equal(f.indexEntryKey(want)), "reverse row %d", i)
		// The SECONDARY range is still resolved forward — mapSubquery always
		// issues firstGreaterOrEqual(prefix)..firstGreaterOrEqual(strinc(prefix)),
		// and reverse applies only to the primary scan.
		g.Expect(rows[i].GetRange.Begin.Key).To(gomega.Equal(f.recordKey(want)))
		g.Expect(rows[i].GetRange.End.Key).To(gomega.Equal(mtStrinc(f.recordKey(want))))
	}
}

// TestGetMappedRange_LimitStopsMidMapping pins that a row limit truncates the
// mapped scan and reports more=true, in both directions. This is the "limit
// stops mid-mapping" shape: the server maps only the rows it returns, so the
// secondary reads for the rest never happen and never appear as conflict ranges.
func TestGetMappedRange_LimitStopsMidMapping(t *testing.T) {
	t.Parallel()
	ctx, cancel := mappedTestCtx()
	defer cancel()

	db := openTestDB(t, ctx)
	defer db.Close()

	f := newMappedFixture(t, 10)
	f.seed(t, ctx, db, 0, func(i int) []byte { return []byte(fmt.Sprintf("rec-%08d", i)) })
	begin, end := f.indexRange()

	for _, tc := range []struct {
		name    string
		reverse bool
		want    []int
	}{
		{"forward", false, []int{0, 1, 2}},
		{"reverse", true, []int{9, 8, 7}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := gomega.NewWithT(t)
			tx := db.CreateTransaction()
			defer tx.Cancel()
			rows, more, err := tx.GetMappedRange(ctx, begin, end, f.getValueMapper(), 3, tc.reverse)
			g.Expect(err).NotTo(gomega.HaveOccurred())
			g.Expect(rows).To(gomega.HaveLen(3), "the row limit must be exact")
			g.Expect(more).To(gomega.BeTrue(), "7 of 10 entries were never scanned")
			for i, want := range tc.want {
				g.Expect(rows[i].GetValue.Key).To(gomega.Equal(f.recordKey(want)), "row %d", i)
			}
		})
	}
}

// TestGetMappedRange_ByteLimitSpansMultipleReplies drives the case a row limit
// cannot reach: the SERVER stops mapping mid-range because it ran out of reply
// bytes, and the client has to continue.
//
// mapKeyValues decrements remainingLimitBytes by the index size PLUS the mapped
// result size for every row (storageserver.actor.cpp:6012-6021), starting from
// the request's limitBytes — 80000, CLIENT_KNOBS->REPLY_BYTE_LIMIT. With 24 KiB
// per record, a handful of rows exhausts it, so the read below cannot complete
// in one reply and rangeScanImpl must re-issue from keyAfter(lastRow.Key).
//
// That makes this the test that would catch the interior-row blanking above
// being mistaken for something safe to ignore: the continuation anchor is a
// getValue-arm primary key, and if the server ever stopped restoring it the
// re-issue would start from keyAfter("") and this test would return the wrong
// rows rather than merely fewer.
func TestGetMappedRange_ByteLimitSpansMultipleReplies(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	ctx, cancel := mappedTestCtx()
	defer cancel()

	db := openTestDB(t, ctx)
	defer db.Close()

	const rows = 24
	const valueSize = 24 * 1024
	f := newMappedFixture(t, rows)
	f.seed(t, ctx, db, 0, func(i int) []byte {
		v := bytes.Repeat([]byte{byte('a' + i%26)}, valueSize)
		return v
	})

	tx := db.CreateTransaction()
	defer tx.Cancel()

	begin, end := f.indexRange()
	got, more, err := tx.GetMappedRange(ctx, begin, end, f.getValueMapper(), 0, false)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(more).To(gomega.BeFalse(), "an unlimited read must drive the continuation to exhaustion")
	g.Expect(got).To(gomega.HaveLen(rows),
		"every index entry must come back even though %d x %d bytes cannot fit one 80000-byte reply", rows, valueSize)
	for i := range got {
		g.Expect(got[i].GetValue.Key).To(gomega.Equal(f.recordKey(i)),
			"row %d out of order or duplicated — the continuation anchor is wrong", i)
		g.Expect(got[i].GetValue.Present).To(gomega.BeTrue())
		g.Expect(got[i].GetValue.Value).To(gomega.HaveLen(valueSize), "row %d value truncated", i)
	}
}

// ---------------------------------------------------------------------------
// Conflict-range narrowing
// ---------------------------------------------------------------------------
//
// A mapped read is issued at Snapshot::True and its conflict ranges are added by
// hand afterwards, and the SAME ranges feed the mustUnmodified check that raises
// get_mapped_range_reads_your_writes. So the width of the primary conflict range
// is directly observable: a write inside it makes the read fail, a write outside
// it does not. That is what these three tests measure — they are the only way to
// tell a narrowed range from an unnarrowed one from outside the client.

// TestGetMappedRange_TruncatedForwardReadDoesNotConflictPastLastRow pins C++
// addConflictRange's forward overload (ReadYourWrites.actor.cpp:245-281): when
// the result is truncated, rangeEnd starts at read.begin and is then stretched to
// keyAfter(last returned key) — so the conflict range covers what was READ, not
// what was REQUESTED. A key past the last returned row was never looked at and a
// write there must not fail the read.
func TestGetMappedRange_TruncatedForwardReadDoesNotConflictPastLastRow(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	ctx, cancel := mappedTestCtx()
	defer cancel()

	db := openTestDB(t, ctx)
	defer db.Close()

	f := newMappedFixture(t, 10)
	f.seed(t, ctx, db, 0, func(i int) []byte { return []byte(fmt.Sprintf("rec-%08d", i)) })

	tx := db.CreateTransaction()
	defer tx.Cancel()

	// Inside the REQUESTED range, far past the 3 rows the limit will return.
	tx.Set(f.indexEntryKey(9), []byte("written-by-this-transaction"))

	begin, end := f.indexRange()
	rows, more, err := tx.GetMappedRange(ctx, begin, end, f.getValueMapper(), 3, false)
	g.Expect(err).NotTo(gomega.HaveOccurred(),
		"index entry 9 is past the last row a limit-3 read returned, so the read never observed it")
	g.Expect(more).To(gomega.BeTrue())
	g.Expect(rows).To(gomega.HaveLen(3))
	g.Expect(rows[2].Key).To(gomega.Equal(f.indexEntryKey(2)))
}

// TestGetMappedRange_TruncatedReverseReadDoesNotConflictBeforeLastRow is the
// mirror, and it is a separate test because the reverse overload
// (ReadYourWrites.actor.cpp:284-318) narrows the OTHER end: rangeBegin, not
// rangeEnd. A fix that only handled the forward direction would pass the test
// above and fail this one.
func TestGetMappedRange_TruncatedReverseReadDoesNotConflictBeforeLastRow(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	ctx, cancel := mappedTestCtx()
	defer cancel()

	db := openTestDB(t, ctx)
	defer db.Close()

	f := newMappedFixture(t, 10)
	f.seed(t, ctx, db, 0, func(i int) []byte { return []byte(fmt.Sprintf("rec-%08d", i)) })

	tx := db.CreateTransaction()
	defer tx.Cancel()

	// Inside the requested range, BELOW every row a reverse limit-3 read returns.
	tx.Set(f.indexEntryKey(0), []byte("written-by-this-transaction"))

	begin, end := f.indexRange()
	rows, more, err := tx.GetMappedRange(ctx, begin, end, f.getValueMapper(), 3, true)
	g.Expect(err).NotTo(gomega.HaveOccurred(),
		"index entry 0 is below the last row a reverse limit-3 read returned, so the read never observed it")
	g.Expect(more).To(gomega.BeTrue())
	g.Expect(rows).To(gomega.HaveLen(3))
	g.Expect(rows[2].Key).To(gomega.Equal(f.indexEntryKey(7)))
}

// TestGetMappedRange_UntruncatedReadConflictsWholeRequestedRange is the third
// direction, and without it the two tests above are satisfied by simply deleting
// the primary conflict range. An UNtruncated read really did observe the whole
// requested range — including the empty space past the last row, where an insert
// would have changed the answer — so the narrowing must NOT apply and a write
// anywhere inside it must still raise get_mapped_range_reads_your_writes.
func TestGetMappedRange_UntruncatedReadConflictsWholeRequestedRange(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	ctx, cancel := mappedTestCtx()
	defer cancel()

	db := openTestDB(t, ctx)
	defer db.Close()

	f := newMappedFixture(t, 3)
	f.seed(t, ctx, db, 0, func(i int) []byte { return []byte(fmt.Sprintf("rec-%08d", i)) })
	begin, end := f.indexRange()

	// A key inside the requested range but AFTER every index entry the fixture
	// wrote, i.e. exactly the region the narrowing would have cut off if it fired
	// on an untruncated read.
	beyondLast := append(append([]byte{}, f.indexEntryKey(2)...), 0xff)
	g.Expect(bytes.Compare(beyondLast, end) < 0).To(gomega.BeTrue(), "the probe key must be inside the requested range")

	for _, reverse := range []bool{false, true} {
		t.Run(fmt.Sprintf("reverse=%v", reverse), func(t *testing.T) {
			g := gomega.NewWithT(t)
			tx := db.CreateTransaction()
			defer tx.Cancel()
			tx.Set(beyondLast, []byte("written-by-this-transaction"))
			_, _, err := tx.GetMappedRange(ctx, begin, end, f.getValueMapper(), 0, reverse)
			g.Expect(err).To(gomega.HaveOccurred())
			g.Expect(mappedFDBErrCode(t, err)).To(gomega.Equal(ErrGetMappedRangeReadsYourWrites),
				"an untruncated read observed the whole requested range, so a write anywhere in it is a RYW conflict")
		})
	}
}

// TestGetMappedRange_SecondaryReadConflictsOnResolvedKey pins the other half of
// addConflictRangeAndMustUnmodified: the SECONDARY reads get conflict ranges too,
// over keys the caller never named and the client could not have known before the
// reply arrived. A write to a mapped-to record key must fail the read even though
// that key is nowhere near the primary range.
func TestGetMappedRange_SecondaryReadConflictsOnResolvedKey(t *testing.T) {
	t.Parallel()
	ctx, cancel := mappedTestCtx()
	defer cancel()

	db := openTestDB(t, ctx)
	defer db.Close()

	f := newMappedFixture(t, 3)
	f.seed(t, ctx, db, 1, func(i int) []byte { return []byte(fmt.Sprintf("rec-%08d", i)) })
	begin, end := f.indexRange()

	for _, tc := range []struct {
		name     string
		mapper   []byte
		writeKey []byte
	}{
		// The point-lookup arm conflicts on exactly [k, keyAfter(k)).
		{"getValue_arm", f.getValueMapper(), f.recordKey(1)},
		// The range arm conflicts on the resolved [begin, end) selectors, so a
		// write anywhere under the record prefix counts.
		{"getRange_arm", f.getRangeMapper(), f.splitRecordKey(1, 0)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := gomega.NewWithT(t)
			tx := db.CreateTransaction()
			defer tx.Cancel()
			tx.Set(tc.writeKey, []byte("written-by-this-transaction"))
			_, _, err := tx.GetMappedRange(ctx, begin, end, tc.mapper, 0, false)
			g.Expect(err).To(gomega.HaveOccurred())
			g.Expect(mappedFDBErrCode(t, err)).To(gomega.Equal(ErrGetMappedRangeReadsYourWrites),
				"a write to a SECONDARY key the server resolved must be caught, not silently read around")
		})
	}
}

// ---------------------------------------------------------------------------
// Where a mapped key is allowed to land
// ---------------------------------------------------------------------------

// TestGetMappedRange_MapperCannotEscapeTheTupleKeyspace answers the "mapper
// resolving outside the allowed keyspace" question, and the answer is that it
// cannot — by construction, on the server.
//
// This matters because of an asymmetry that looks like a hole. GetMappedRange
// checks the caller's PRIMARY range against maxReadKey and refuses anything past
// it (key_outside_legal_range, 2004), but it applies no such check to the
// SECONDARY key, and it could not: that key does not exist until the server has
// resolved the mapper. So on the face of it a mapper could name \xff and read the
// system keyspace through a transaction that is not allowed to read it directly.
//
// It cannot, because constructMappedKey ends in mappedKeyTuple.pack()
// (storageserver.actor.cpp:4949): the mapped key is always a PACKED TUPLE, so it
// always begins with a tuple type code. A literal "\xff" in the mapper becomes
// the three bytes 01 FF 00 — an ordinary user key — not the one byte FF. The
// system keyspace (\xff) and the special keyspace (\xff\xff) are therefore
// unreachable from any mapper, whatever it says.
//
// The test writes the tuple-encoded destination and shows the read lands THERE,
// so this is not merely "no error occurred": the mapper really did resolve, and
// it resolved inside the tuple keyspace. Nothing is written to \xff — that would
// be both illegal for this transaction and unsafe on a shared container.
func TestGetMappedRange_MapperCannotEscapeTheTupleKeyspace(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)
	ctx, cancel := mappedTestCtx()
	defer cancel()

	db := openTestDB(t, ctx)
	defer db.Close()

	f := newMappedFixture(t, 1)
	// The mapper's first element is the single byte 0xFF — the system keyspace
	// prefix — followed by the primary key placeholder.
	systemish := []byte{0xFF}
	mapper := mtPack(systemish, []byte("{K[3]}"))
	// What the server will actually build: pack(0xFF, pk), not 0xFF || pk.
	wantMappedKey := mtPack(systemish, f.primaryKey(0))
	g.Expect(wantMappedKey[0]).To(gomega.Equal(byte(0x01)),
		"a packed tuple starts with a type code; if this ever starts with 0xFF the premise is gone")

	_, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.Set(f.indexEntryKey(0), []byte{})
		tx.Set(wantMappedKey, []byte("landed-in-the-tuple-keyspace"))
		return nil, nil
	})
	g.Expect(err).NotTo(gomega.HaveOccurred())

	tx := db.CreateTransaction()
	defer tx.Cancel()

	begin, end := f.indexRange()
	rows, _, err := tx.GetMappedRange(ctx, begin, end, mapper, 0, false)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(rows).To(gomega.HaveLen(1))

	got := rows[0].GetValue.Key
	g.Expect(got[0]).NotTo(gomega.Equal(byte(0xFF)),
		"a mapper naming \\xff must NOT produce a \\xff-prefixed mapped key; if it does, a mapped read "+
			"can reach the system keyspace through a transaction forbidden to read it directly, and "+
			"GetMappedRange's maxReadKey check on the primary range is guarding the wrong half of the read")
	g.Expect(got).To(gomega.Equal(wantMappedKey),
		"the mapper's \\xff must be tuple-ENCODED (01 FF 00), not concatenated raw")
	g.Expect(rows[0].GetValue.Present).To(gomega.BeTrue(),
		"the record written at the tuple-encoded key must be what the read found — otherwise this "+
			"test proves only that nothing happened")
	g.Expect(rows[0].GetValue.Value).To(gomega.Equal([]byte("landed-in-the-tuple-keyspace")))
}
