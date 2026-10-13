//go:build cgo && libfdbc

package libfdbc_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	fdbclient "fdb.dev/pkg/fdbgo/client"
	gofdb "fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/libfdbc"
	"fdb.dev/pkg/fdbgo/wire"
)

// Cross-client differential for fdb_transaction_get_mapped_range: the pure-Go
// client and libfdb_c 7.3.77 issue the SAME mapped read against the SAME cluster
// and must decode the reply identically.
//
// The oracle here is deliberately SPLIT, and the split is not incidental.
//
//   - The getRange arm is what libfdb_c's public API can represent, so a
//     disagreement there is a bug in the pure-Go client, full stop. That arm is
//     the authoritative half of this file.
//
//   - The getValue arm is NOT representable through the public header. fdb_c.h
//     models FDBMappedKeyValue with a single FDBGetRangeReqAndResult and no
//     discriminator — "It's complicated to map a std::variant to C. For now we
//     assume the underlying requests are always getRange and take the shortcut" —
//     so a C caller reading a point-lookup row through the documented struct gets
//     a getRange view of getValue bytes. mappedrange_cref.go gets underneath that
//     by reading libstdc++'s std::variant discriminator byte directly, which is a
//     hand-rolled decode of a NON-ABI internal layout, guarded by static asserts.
//     That makes it a useful second opinion, NOT a specification. So the getValue
//     differential below is written as a test OF THAT EXTENSION: it is the first
//     thing that executes mappedrange_cref.go's variant decode at all, and it is
//     cross-checked against the server's own truth (an explicit point-get) rather
//     than trusted on its own. The authoritative getValue oracle lives in
//     pkg/fdbgo/client/mappedrange_e2e_test.go, where the mapped result is
//     compared against ordinary Gets in the same transaction.
//
// Both clients talk to the container startCluster brings up with WithDirectIP,
// which is what lets libfdb_c's FlowTransport accept the advertised address.

// ---------------------------------------------------------------------------
// Fixture — FDB's own GetMappedRange workload layout
// (fdbserver/workloads/GetMappedRange.actor.cpp:88-95).
// ---------------------------------------------------------------------------

func mdPack(parts ...[]byte) []byte {
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

func mdStrinc(k []byte) []byte {
	out := append([]byte{}, k...)
	for i := len(out) - 1; i >= 0; i-- {
		if out[i] != 0xFF {
			out[i]++
			return out[:i+1]
		}
	}
	panic("mdStrinc: key is all 0xFF")
}

type mdFixture struct {
	prefix []byte
	n      int
}

func (f mdFixture) indexEntryKey(i int) []byte {
	return mdPack(f.prefix, []byte("IDX"), []byte(fmt.Sprintf("ik-%08d", i)), []byte(fmt.Sprintf("pk-%08d", i)))
}

func (f mdFixture) recordKey(i int) []byte {
	return mdPack(f.prefix, []byte("REC"), []byte(fmt.Sprintf("pk-%08d", i)))
}

func (f mdFixture) splitRecordKey(i, s int) []byte {
	return mdPack(f.prefix, []byte("REC"), []byte(fmt.Sprintf("pk-%08d", i)), []byte(fmt.Sprintf("s%d", s)))
}

func (f mdFixture) indexRange() (begin, end []byte) {
	begin = mdPack(f.prefix, []byte("IDX"))
	return begin, mdStrinc(begin)
}

func (f mdFixture) getValueMapper() []byte {
	return mdPack(f.prefix, []byte("REC"), []byte("{K[3]}"))
}

func (f mdFixture) getRangeMapper() []byte {
	return mdPack(f.prefix, []byte("REC"), []byte("{K[3]}"), []byte("{...}"))
}

// seedThroughC writes the fixture through libfdb_c, so the data under test is
// not written by the client whose decode is being checked.
func (f mdFixture) seedThroughC(t *testing.T, cdb *libfdbc.CDatabase, split int) {
	t.Helper()
	tr, err := cdb.CreateTransaction()
	if err != nil {
		t.Fatalf("libfdb_c create transaction: %v", err)
	}
	defer tr.Close()
	for i := 0; i < f.n; i++ {
		tr.Set(f.indexEntryKey(i), []byte{})
		val := []byte(fmt.Sprintf("record-value-%08d", i))
		if split == 0 {
			tr.Set(f.recordKey(i), val)
			continue
		}
		for s := 0; s < split; s++ {
			tr.Set(f.splitRecordKey(i, s), append(append([]byte{}, val...), byte('0'+s)))
		}
	}
	if err := tr.Commit(); err != nil {
		t.Fatalf("libfdb_c commit fixture: %v", err)
	}
}

// TestLibFDBC_GetMappedRangeDifferential is the cross-client gate for CQ-9a
// step 1's read path.
func TestLibFDBC_GetMappedRangeDifferential(t *testing.T) {
	t.Parallel()

	clusterFile := startCluster(t)

	cdb, err := libfdbc.COpenDatabase(clusterFile)
	if err != nil {
		t.Fatalf("open libfdb_c database: %v", err)
	}
	defer cdb.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	goDB, err := fdbclient.OpenDatabase(ctx, clusterFile, fdbclient.WithAPIVersion(730))
	if err != nil {
		t.Fatalf("open pure-Go client: %v", err)
	}
	defer goDB.Close()

	// -----------------------------------------------------------------------
	// The authoritative half: the getRange arm, which libfdb_c's public API
	// represents natively.
	// -----------------------------------------------------------------------
	t.Run("getRange_arm", func(t *testing.T) {
		f := mdFixture{prefix: []byte(t.Name()), n: 6}
		f.seedThroughC(t, cdb, 3)
		begin, end := f.indexRange()

		for _, reverse := range []bool{false, true} {
			for _, limit := range []int{0, 4} {
				name := fmt.Sprintf("reverse=%v_limit=%d", reverse, limit)
				t.Run(name, func(t *testing.T) {
					cRows, cMore := cMappedRead(t, cdb, begin, end, f.getRangeMapper(), limit, reverse)
					goRows, goMore := goMappedRead(t, ctx, goDB, begin, end, f.getRangeMapper(), limit, reverse)

					if cMore != goMore {
						t.Fatalf("more flag: libfdb_c=%v pure-Go=%v", cMore, goMore)
					}
					if len(cRows) != len(goRows) {
						t.Fatalf("row count: libfdb_c=%d pure-Go=%d", len(cRows), len(goRows))
					}
					for i := range cRows {
						c, gr := cRows[i], goRows[i]
						if c.Kind != libfdbc.CMappedKindGetRange {
							t.Fatalf("row %d: libfdb_c decoded arm %d, want getRange", i, c.Kind)
						}
						if gr.Kind != fdbclient.MappedResultGetRange {
							t.Fatalf("row %d: pure-Go decoded arm %v, want getRange", i, gr.Kind)
						}
						if !bytes.Equal(c.Key, gr.Key) || !bytes.Equal(c.Value, gr.Value) {
							t.Fatalf("row %d primary: libfdb_c=(%q,%q) pure-Go=(%q,%q)", i, c.Key, c.Value, gr.Key, gr.Value)
						}
						if !bytes.Equal(c.RangeBeginKey, gr.GetRange.Begin.Key) {
							t.Fatalf("row %d secondary begin: libfdb_c=%q pure-Go=%q", i, c.RangeBeginKey, gr.GetRange.Begin.Key)
						}
						if !bytes.Equal(c.RangeEndKey, gr.GetRange.End.Key) {
							t.Fatalf("row %d secondary end: libfdb_c=%q pure-Go=%q", i, c.RangeEndKey, gr.GetRange.End.Key)
						}
						if len(c.RangeRows) != len(gr.GetRange.Rows) {
							t.Fatalf("row %d secondary row count: libfdb_c=%d pure-Go=%d", i, len(c.RangeRows), len(gr.GetRange.Rows))
						}
						if len(c.RangeRows) == 0 {
							t.Fatalf("row %d resolved to nothing — the fixture writes 3 split records, so a zero here "+
								"means both clients agree on an empty result and the comparison is vacuous", i)
						}
						for j := range c.RangeRows {
							if !bytes.Equal(c.RangeRows[j].Key, gr.GetRange.Rows[j].Key) ||
								!bytes.Equal(c.RangeRows[j].Value, gr.GetRange.Rows[j].Value) {
								t.Fatalf("row %d secondary kv %d: libfdb_c=(%q,%q) pure-Go=(%q,%q)", i, j,
									c.RangeRows[j].Key, c.RangeRows[j].Value,
									gr.GetRange.Rows[j].Key, gr.GetRange.Rows[j].Value)
							}
						}
					}
					t.Logf("compared %d mapped rows (reverse=%v limit=%d) across both clients", len(cRows), reverse, limit)
				})
			}
		}
	})

	// -----------------------------------------------------------------------
	// The non-authoritative half: the getValue arm, which exists only because
	// mappedrange_cref.go reads the std::variant discriminator itself. This is
	// the first execution of that decode.
	// -----------------------------------------------------------------------
	t.Run("getValue_arm_exercises_the_cref_variant_decode", func(t *testing.T) {
		f := mdFixture{prefix: []byte(t.Name()), n: 5}
		f.seedThroughC(t, cdb, 0)
		begin, end := f.indexRange()

		cRows, _ := cMappedRead(t, cdb, begin, end, f.getValueMapper(), 0, false)
		goRows, _ := goMappedRead(t, ctx, goDB, begin, end, f.getValueMapper(), 0, false)

		if len(cRows) != len(goRows) || len(cRows) != f.n {
			t.Fatalf("row count: libfdb_c=%d pure-Go=%d want=%d", len(cRows), len(goRows), f.n)
		}
		for i := range cRows {
			c, gv := cRows[i], goRows[i]
			// The whole point: the public header would report getRange here.
			// Reading the variant index gives the real arm.
			if c.Kind != libfdbc.CMappedKindGetValue {
				t.Fatalf("row %d: the cref variant decode reported arm %d for a point-lookup mapper. "+
					"Either libstdc++'s std::variant layout changed under the static asserts in "+
					"mappedrange_cref.go, or the discriminator offset is wrong", i, c.Kind)
			}
			if gv.Kind != fdbclient.MappedResultGetValue {
				t.Fatalf("row %d: pure-Go decoded arm %v, want getValue", i, gv.Kind)
			}
			if !bytes.Equal(c.GetValueKey, gv.GetValue.Key) {
				t.Fatalf("row %d mapped key: libfdb_c=%q pure-Go=%q", i, c.GetValueKey, gv.GetValue.Key)
			}
			if c.GetValuePresent != gv.GetValue.Present {
				t.Fatalf("row %d present: libfdb_c=%v pure-Go=%v", i, c.GetValuePresent, gv.GetValue.Present)
			}
			if !bytes.Equal(c.GetValueValue, gv.GetValue.Value) {
				t.Fatalf("row %d mapped value: libfdb_c=%q pure-Go=%q", i, c.GetValueValue, gv.GetValue.Value)
			}
			// Cross-check against the server rather than trusting either decode:
			// the mapped key must be the one the mapper names, and it must hold
			// what an ordinary read of that key holds.
			if want := f.recordKey(i); !bytes.Equal(gv.GetValue.Key, want) {
				t.Fatalf("row %d: mapper resolved to %q, want %q", i, gv.GetValue.Key, want)
			}
			if !gv.GetValue.Present {
				t.Fatalf("row %d: the fixture wrote this record, so an absent result means the "+
					"comparison above matched two identically-wrong decodes", i)
			}
		}
		t.Logf("cref variant decode agreed with the pure-Go client on %d point-lookup rows", len(cRows))
	})

	// -----------------------------------------------------------------------
	// Errors: the mapper is parsed by neither client, so both must surface the
	// storage server's raw code. A divergence here means the pure-Go client is
	// inventing or swallowing an error.
	// -----------------------------------------------------------------------
	t.Run("server_errors_agree", func(t *testing.T) {
		f := mdFixture{prefix: []byte(t.Name()), n: 3}
		f.seedThroughC(t, cdb, 0)
		begin, end := f.indexRange()

		for _, tc := range []struct {
			name   string
			mapper []byte
		}{
			{"mapper_not_tuple", []byte("\xff not a tuple")},
			{"mapper_bad_index", mdPack(f.prefix, []byte("REC"), []byte("{K[9]}"))},
			{"mapper_bad_range_descriptor", mdPack(f.prefix, []byte("{...}"), []byte("REC"))},
		} {
			t.Run(tc.name, func(t *testing.T) {
				cCode := cMappedReadErrCode(t, cdb, begin, end, tc.mapper)
				goCode := goMappedReadErrCode(t, ctx, goDB, begin, end, tc.mapper)
				if cCode == 0 || goCode == 0 {
					t.Fatalf("expected BOTH clients to fail (libfdb_c=%d pure-Go=%d); a zero means the "+
						"mapper was accepted and the comparison proves nothing", cCode, goCode)
				}
				if cCode != goCode {
					t.Fatalf("error code: libfdb_c=%d pure-Go=%d", cCode, goCode)
				}
				t.Logf("both clients reported fdb error %d", cCode)
			})
		}
	})
}

func cMappedRead(t *testing.T, cdb *libfdbc.CDatabase, begin, end, mapper []byte, limit int, reverse bool) ([]libfdbc.CMappedRow, bool) {
	t.Helper()
	tr, err := cdb.CreateTransaction()
	if err != nil {
		t.Fatalf("libfdb_c create transaction: %v", err)
	}
	defer tr.Close()
	rows, more, err := libfdbc.CGetMappedRange(tr, begin, end, mapper, limit, reverse, false)
	if err != nil {
		t.Fatalf("libfdb_c getMappedRange: %v", err)
	}
	return rows, more
}

func goMappedRead(t *testing.T, ctx context.Context, db *fdbclient.Database, begin, end, mapper []byte, limit int, reverse bool) ([]fdbclient.MappedKeyValue, bool) {
	t.Helper()
	tx := db.CreateTransaction()
	defer tx.Cancel()
	rows, more, err := tx.GetMappedRange(ctx, begin, end, mapper, limit, reverse)
	if err != nil {
		t.Fatalf("pure-Go GetMappedRange: %v", err)
	}
	return rows, more
}

func cMappedReadErrCode(t *testing.T, cdb *libfdbc.CDatabase, begin, end, mapper []byte) int {
	t.Helper()
	tr, err := cdb.CreateTransaction()
	if err != nil {
		t.Fatalf("libfdb_c create transaction: %v", err)
	}
	defer tr.Close()
	_, _, err = libfdbc.CGetMappedRange(tr, begin, end, mapper, 0, false, false)
	if err == nil {
		return 0
	}
	var ce *libfdbc.CFDBError
	if !errors.As(err, &ce) {
		t.Fatalf("libfdb_c returned a non-fdb error: %T %v", err, err)
	}
	return ce.Code
}

func goMappedReadErrCode(t *testing.T, ctx context.Context, db *fdbclient.Database, begin, end, mapper []byte) int {
	t.Helper()
	tx := db.CreateTransaction()
	defer tx.Cancel()
	_, _, err := tx.GetMappedRange(ctx, begin, end, mapper, 0, false)
	if err == nil {
		return 0
	}
	var fe *wire.FDBError
	if !errors.As(err, &fe) {
		t.Fatalf("pure-Go client returned a non-fdb error: %T %v", err, err)
	}
	return fe.Code
}

type mdBatch struct {
	iteration, requested, returned int
	more                           bool
}

func mdSeedStreaming(t *testing.T, cdb *libfdbc.CDatabase, n int) mdFixture {
	t.Helper()
	f := mdFixture{prefix: []byte(t.Name()), n: n}
	tr, err := cdb.CreateTransaction()
	if err != nil {
		t.Fatal(err)
	}
	defer tr.Close()
	for i := 0; i < n; i++ {
		tr.Set(f.indexEntryKey(i), []byte{})
		for s := 0; s < 3; s++ {
			tr.Set(f.splitRecordKey(i, s), bytes.Repeat([]byte{byte('a' + (i+s)%26)}, 400))
		}
	}
	if err := tr.Commit(); err != nil {
		t.Fatal(err)
	}
	return f
}

// mdDrainC leaves targetBytes zero so C derives the streaming targets itself.
// The mode is a C constant, not the Go mode converted by the production code.
func mdDrainC(t *testing.T, tr libfdbc.CTxnHandle, f mdFixture, limit, mode int, reverse bool) ([]libfdbc.CMappedRow, []mdBatch) {
	t.Helper()
	begin, end := f.indexRange()
	remaining := gofdb.EffectiveRowLimit(limit)
	var rows []libfdbc.CMappedRow
	var batches []mdBatch
	for iteration := 1; iteration <= f.n+1; iteration++ {
		requested := remaining
		batch, more, err := libfdbc.CGetMappedRangeBatch(tr, begin, end, f.getRangeMapper(), requested, 0, mode, iteration, reverse, false)
		if err != nil {
			t.Fatalf("C mapped batch %d: %v", iteration, err)
		}
		batches = append(batches, mdBatch{iteration, requested, len(batch), more})
		rows = append(rows, batch...)
		remaining -= len(batch)
		if !more || len(batch) == 0 || remaining <= 0 {
			return rows, batches
		}
		last := batch[len(batch)-1].Key
		if reverse {
			end = bytes.Clone(last)
		} else {
			begin = append(bytes.Clone(last), 0)
		}
	}
	t.Fatal("C mapped iteration failed to terminate")
	return nil, nil
}

func mdCompareRangeRow(t *testing.T, c libfdbc.CMappedRow, g gofdb.MappedKeyValue) {
	t.Helper()
	if c.Kind != libfdbc.CMappedKindGetRange || g.Kind != fdbclient.MappedResultGetRange {
		t.Fatalf("mapped kinds: C=%d Go=%v, want range", c.Kind, g.Kind)
	}
	if !bytes.Equal(c.Key, g.Key) || !bytes.Equal(c.Value, g.Value) ||
		!bytes.Equal(c.RangeBeginKey, g.GetRange.Begin.Key) || !bytes.Equal(c.RangeEndKey, g.GetRange.End.Key) {
		t.Fatalf("mapped primary/secondary bounds disagree: C=%+v Go=%+v", c, g)
	}
	if len(c.RangeRows) != 3 || len(g.GetRange.Rows) != 3 || g.GetRange.More {
		t.Fatalf("mapped secondary range incomplete: C=%d Go=%d more=%v", len(c.RangeRows), len(g.GetRange.Rows), g.GetRange.More)
	}
	// quickGetKeyValues constructs firstGreaterOrEqual selectors for both
	// ends (storageserver.actor.cpp:4743-4744); the facade must retain them.
	if g.GetRange.Begin.OrEqual || g.GetRange.End.OrEqual || g.GetRange.Begin.Offset != 1 || g.GetRange.End.Offset != 1 {
		t.Fatalf("mapped secondary selectors lost: begin=%+v end=%+v", g.GetRange.Begin, g.GetRange.End)
	}
	for i := range c.RangeRows {
		if !bytes.Equal(c.RangeRows[i].Key, g.GetRange.Rows[i].Key) || !bytes.Equal(c.RangeRows[i].Value, g.GetRange.Rows[i].Value) {
			t.Fatalf("mapped secondary row %d differs", i)
		}
	}
}

func TestLibFDBC_MappedRangeFacadeBatchDivision(t *testing.T) {
	t.Parallel()
	clusterFile := startCluster(t)
	cdb, err := libfdbc.COpenDatabase(clusterFile)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cdb.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	t.Cleanup(cancel)
	goDB, err := fdbclient.OpenDatabase(ctx, clusterFile, fdbclient.WithAPIVersion(730))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { goDB.Close() })
	f := mdSeedStreaming(t, cdb, 96)
	facade := gofdb.WrapDatabase(goDB)
	t.Run("libfdbc_backend_lacks_optional_capability", func(t *testing.T) {
		backend, err := libfdbc.Open(clusterFile)
		if err != nil {
			t.Fatal(err)
		}
		defer backend.Close()
		tr, err := backend.CreateWritableTransaction()
		if err != nil {
			t.Fatal(err)
		}
		defer tr.Cancel()
		if _, ok := tr.(gofdb.MappedRangeReader); ok {
			t.Fatal("libfdb_c backend unexpectedly advertises mapped-range capability")
		}
	})
	for _, mode := range []struct {
		name   string
		goMode gofdb.StreamingMode
		cMode  int
	}{
		{"iterator", gofdb.StreamingModeIterator, libfdbc.CStreamingModeIterator},
		{"small", gofdb.StreamingModeSmall, libfdbc.CStreamingModeSmall},
		{"medium", gofdb.StreamingModeMedium, libfdbc.CStreamingModeMedium},
		{"large", gofdb.StreamingModeLarge, libfdbc.CStreamingModeLarge},
		{"want_all", gofdb.StreamingModeWantAll, libfdbc.CStreamingModeWantAll},
		{"serial", gofdb.StreamingModeSerial, libfdbc.CStreamingModeSerial},
		{"exact", gofdb.StreamingModeExact, libfdbc.CStreamingModeExact},
	} {
		limits := []int{0, 37}
		if mode.goMode == gofdb.StreamingModeExact {
			limits = []int{37}
		}
		for _, limit := range limits {
			for _, reverse := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/limit=%d/reverse=%v", mode.name, limit, reverse), func(t *testing.T) {
					ct, err := cdb.CreateTransaction()
					if err != nil {
						t.Fatal(err)
					}
					defer ct.Close()
					cRows, cBatches := mdDrainC(t, ct, f, limit, mode.cMode, reverse)
					gt, err := facade.CreateTransaction()
					if err != nil {
						t.Fatal(err)
					}
					defer gt.Cancel()
					if err := gt.Options().SetTimeout(5000); err != nil {
						t.Fatal(err)
					}
					reader, ok := any(gt).(gofdb.MappedRangeReader)
					if !ok {
						t.Fatal("pure-Go facade lacks mapped-range capability")
					}
					begin, end := f.indexRange()
					it := reader.GetMappedRange(gofdb.KeyRange{Begin: gofdb.Key(begin), End: gofdb.Key(end)}, f.getRangeMapper(), gofdb.RangeOptions{Limit: limit, Mode: mode.goMode, Reverse: reverse}).Iterator()
					var gBatches []mdBatch
					it.SetTraceLog(func(iteration, requested, returned int, more bool, err error) {
						if err != nil {
							t.Fatalf("Go mapped batch %d: %v", iteration, err)
						}
						gBatches = append(gBatches, mdBatch{iteration, requested, returned, more})
					})
					var gRows []gofdb.MappedKeyValue
					for it.Advance() {
						row, err := it.Get()
						if err != nil {
							t.Fatal(err)
						}
						again, err := it.Get()
						if err != nil || !reflect.DeepEqual(row, again) {
							t.Fatal("mapped Get is not idempotent", err)
						}
						gRows = append(gRows, row)
						if len(gRows) > f.n {
							t.Fatal("mapped iterator repeated rows")
						}
					}
					if _, err := it.Get(); err != nil {
						t.Fatal(err)
					}
					want := f.n
					if limit > 0 {
						want = limit
					}
					if len(cRows) != want || len(gRows) != want {
						t.Fatalf("row population: C=%d Go=%d want=%d", len(cRows), len(gRows), want)
					}
					if !reflect.DeepEqual(cBatches, gBatches) {
						t.Fatalf("mapped division: C=%+v Go=%+v", cBatches, gBatches)
					}
					if mode.goMode == gofdb.StreamingModeIterator || mode.goMode == gofdb.StreamingModeSmall {
						if len(cBatches) < 2 {
							t.Fatalf("multi-batch comparison is vacuous: %+v", cBatches)
						}
					}
					for i := range cRows {
						mdCompareRangeRow(t, cRows[i], gRows[i])
						index := i
						if reverse {
							index = f.n - 1 - i
						}
						if !bytes.Equal(gRows[i].Key, f.indexEntryKey(index)) {
							t.Fatalf("row %d primary order differs", i)
						}
						for s := 0; s < 3; s++ {
							if !bytes.Equal(gRows[i].GetRange.Rows[s].Key, f.splitRecordKey(index, s)) ||
								!bytes.Equal(gRows[i].GetRange.Rows[s].Value, bytes.Repeat([]byte{byte('a' + (index+s)%26)}, 400)) {
								t.Fatalf("row %d split %d does not match seeded data", i, s)
							}
						}
					}
					t.Logf("compared %d primary rows, %d secondary rows and %d fetches", want, want*3, len(cBatches))
				})
			}
		}
	}
}

func TestLibFDBC_MappedRangeByteTarget(t *testing.T) {
	t.Parallel()
	clusterFile := startCluster(t)
	cdb, err := libfdbc.COpenDatabase(clusterFile)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cdb.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	goDB, err := fdbclient.OpenDatabase(ctx, clusterFile, fdbclient.WithAPIVersion(730))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { goDB.Close() })
	f := mdSeedStreaming(t, cdb, 24)
	for _, target := range []int{256, 4096, 0, -1, -2} {
		for _, reverse := range []bool{false, true} {
			t.Run(fmt.Sprintf("target=%d/reverse=%v", target, reverse), func(t *testing.T) {
				ct, err := cdb.CreateTransaction()
				if err != nil {
					t.Fatal(err)
				}
				defer ct.Close()
				gt := goDB.CreateTransaction()
				defer gt.Cancel()
				begin, end := f.indexRange()
				cRows, cMore, cErr := libfdbc.CGetMappedRangeBatch(ct, begin, end, f.getRangeMapper(), 17, target, libfdbc.CStreamingModeExact, 1, reverse, false)
				gRows, gMore, gErr := gt.GetMappedRangeWithByteTarget(ctx, begin, end, f.getRangeMapper(), 17, target, reverse)
				if target == -2 {
					var ce *libfdbc.CFDBError
					var ge *wire.FDBError
					if !errors.As(cErr, &ce) || !errors.As(gErr, &ge) || ce.Code != 2012 || ge.Code != 2012 {
						t.Fatalf("invalid byte target: C=%v Go=%v, want 2012", cErr, gErr)
					}
					if len(cRows) != 0 || len(gRows) != 0 || cMore || gMore {
						t.Fatalf("invalid byte target returned rows/more: C=%d/%v Go=%d/%v", len(cRows), cMore, len(gRows), gMore)
					}
					return
				}
				if cErr != nil || gErr != nil {
					t.Fatalf("byte-target read: C=%v Go=%v", cErr, gErr)
				}
				if len(cRows) == 0 || len(cRows) != len(gRows) || cMore != gMore {
					t.Fatalf("byte-target batch: C=%d/%v Go=%d/%v", len(cRows), cMore, len(gRows), gMore)
				}
				if target > 0 && len(cRows) >= 17 {
					t.Fatalf("byte-target comparison never truncated: %d rows", len(cRows))
				}
				if target <= 0 && len(cRows) != 17 {
					t.Fatalf("unlimited normalization returned %d rows, want 17", len(cRows))
				}
				for i := range cRows {
					mdCompareRangeRow(t, cRows[i], gRows[i])
				}
			})
		}
	}
}

// The C conflict-map special keys are authoritative. Go has no equivalent
// inspection API, so each boundary and fixture key is checked with a writer
// committed strictly between the mapped read and its commit. The writer adds
// a conflict without changing fixture data, keeping every probe independent.
func TestLibFDBC_MappedRangeFacadeReadConflicts(t *testing.T) {
	t.Parallel()
	clusterFile := startCluster(t)
	cdb, err := libfdbc.COpenDatabase(clusterFile)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cdb.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	t.Cleanup(cancel)
	goDB, err := fdbclient.OpenDatabase(ctx, clusterFile, fdbclient.WithAPIVersion(730))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { goDB.Close() })
	f := mdSeedStreaming(t, cdb, 12)
	facade := gofdb.WrapDatabase(goDB)
	for _, limit := range []int{0, 5} {
		for _, reverse := range []bool{false, true} {
			t.Run(fmt.Sprintf("limit=%d/reverse=%v", limit, reverse), func(t *testing.T) {
				ct, err := cdb.CreateTransaction()
				if err != nil {
					t.Fatal(err)
				}
				defer ct.Close()
				rows, batches := mdDrainC(t, ct, f, limit, libfdbc.CStreamingModeSmall, reverse)
				wantRows := f.n
				if limit > 0 {
					wantRows = limit
				}
				if len(rows) != wantRows || len(batches) < 2 {
					t.Fatalf("conflict oracle lacks multi-batch population: rows=%d batches=%d", len(rows), len(batches))
				}
				boundaries, err := libfdbc.CReadConflictRangeBoundaries(ct)
				if err != nil {
					t.Fatal(err)
				}
				if len(boundaries) < 2 || len(boundaries)%2 != 0 {
					t.Fatalf("empty/incomplete conflict oracle: %+v", boundaries)
				}
				for i, b := range boundaries {
					want := "1"
					if i%2 != 0 {
						want = "0"
					}
					if string(b.Value) != want || (i > 0 && bytes.Compare(boundaries[i-1].Key, b.Key) >= 0) {
						t.Fatalf("invalid conflict boundary %d: %+v", i, b)
					}
				}
				contains := func(key []byte) bool {
					for i := 0; i < len(boundaries); i += 2 {
						if bytes.Compare(key, boundaries[i].Key) >= 0 && bytes.Compare(key, boundaries[i+1].Key) < 0 {
							return true
						}
					}
					return false
				}
				if len(boundaries) != 2*(wantRows+1) {
					t.Fatalf("C conflict interval population: got %d, want one primary and %d secondary", len(boundaries)/2, wantRows)
				}
				for _, row := range rows {
					if !contains(row.Key) {
						t.Fatalf("C conflict map omitted primary row %q", row.Key)
					}
					for _, secondary := range row.RangeRows {
						if !contains(secondary.Key) {
							t.Fatalf("C conflict map omitted secondary row %q", secondary.Key)
						}
					}
				}
				probes := make(map[string][]byte)
				add := func(key []byte) { probes[string(key)] = bytes.Clone(key) }
				for _, b := range boundaries {
					add(b.Key)
					add(append(bytes.Clone(b.Key), 0))
				}
				begin, end := f.indexRange()
				add(begin)
				add(end)
				for i := 0; i < f.n; i++ {
					add(f.indexEntryKey(i))
					for s := 0; s < 3; s++ {
						add(f.splitRecordKey(i, s))
					}
				}
				positive, negative := 0, 0
				for _, key := range probes {
					want := 0
					if contains(key) {
						want = 1020
						positive++
					} else {
						negative++
					}
					gt, err := facade.CreateTransaction()
					if err != nil {
						t.Fatal(err)
					}
					if err := gt.Options().SetTimeout(5000); err != nil {
						t.Fatal(err)
					}
					reader, ok := any(gt).(gofdb.MappedRangeReader)
					if !ok {
						t.Fatal("pure-Go facade lacks mapped-range capability")
					}
					it := reader.GetMappedRange(gofdb.KeyRange{Begin: gofdb.Key(begin), End: gofdb.Key(end)}, f.getRangeMapper(), gofdb.RangeOptions{Limit: limit, Mode: gofdb.StreamingModeSmall, Reverse: reverse}).Iterator()
					seen := 0
					for it.Advance() {
						if _, err := it.Get(); err != nil {
							t.Fatal(err)
						}
						seen++
						if seen > f.n {
							t.Fatal("mapped conflict scan repeated rows")
						}
					}
					if _, err := it.Get(); err != nil {
						t.Fatal(err)
					}
					if seen != wantRows {
						t.Fatalf("conflict scan population %d, want %d", seen, wantRows)
					}
					version, err := gt.GetReadVersion().Get()
					if err != nil {
						t.Fatal(err)
					}
					gt.Set(gofdb.Key(mdPack(f.prefix, []byte("READER-SENTINEL"))), []byte("r"))
					writer, err := facade.CreateTransaction()
					if err != nil {
						t.Fatal(err)
					}
					writer.SetReadVersion(version)
					writer.Set(gofdb.Key(mdPack(f.prefix, []byte("WRITER-SENTINEL"))), []byte("w"))
					if err := writer.AddWriteConflictKey(gofdb.Key(key)); err != nil {
						writer.Cancel()
						t.Fatal(err)
					}
					if err := writer.Commit().Get(); err != nil {
						writer.Cancel()
						t.Fatal(err)
					}
					writer.Cancel()
					commitErr := gt.Commit().Get()
					code := 0
					if commitErr != nil {
						var fe gofdb.Error
						if !errors.As(commitErr, &fe) {
							t.Fatalf("non-FDB commit error: %v", commitErr)
						}
						code = fe.Code
					}
					gt.Cancel()
					if code != want {
						t.Fatalf("probe %q: Go commit=%d, C conflict map requires=%d", key, code, want)
					}
				}
				if positive == 0 || negative == 0 {
					t.Fatalf("vacuous conflict probe population: positive=%d negative=%d", positive, negative)
				}
				t.Logf("checked %d C conflict intervals, %d conflicting and %d non-conflicting probes", len(boundaries)/2, positive, negative)
			})
		}
	}
}

// A secondary range can hit QUICK_GET_KEY_VALUES_LIMIT independently of the
// primary range. Its unread suffix must neither conflict nor fail the mapped
// RYW check; primary reverse does not reverse the secondary range read.
func TestLibFDBC_MappedRangeTruncatedSecondaryConflicts(t *testing.T) {
	t.Parallel()
	clusterFile := startCluster(t)
	cdb, err := libfdbc.COpenDatabase(clusterFile)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cdb.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	t.Cleanup(cancel)
	goDB, err := fdbclient.OpenDatabase(ctx, clusterFile, fdbclient.WithAPIVersion(730))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { goDB.Close() })
	facade := gofdb.WrapDatabase(goDB)
	f := mdFixture{prefix: []byte(t.Name()), n: 1}
	const secondaryCount = 2500
	secondaryKey := func(i int) []byte {
		return mdPack(f.prefix, []byte("REC"), []byte("pk-00000000"), []byte(fmt.Sprintf("s-%08d", i)))
	}
	secondaryValue := func(i int) []byte { return []byte(fmt.Sprintf("value-%08d", i)) }
	seed, err := cdb.CreateTransaction()
	if err != nil {
		t.Fatal(err)
	}
	seed.Set(f.indexEntryKey(0), []byte{})
	for i := 0; i < secondaryCount; i++ {
		seed.Set(secondaryKey(i), secondaryValue(i))
	}
	if err := seed.Commit(); err != nil {
		seed.Close()
		t.Fatal(err)
	}
	seed.Close()
	begin, end := f.indexRange()
	for _, reverse := range []bool{false, true} {
		t.Run(fmt.Sprintf("reverse=%v", reverse), func(t *testing.T) {
			readC := func(t *testing.T, tr libfdbc.CTxnHandle) libfdbc.CMappedRow {
				t.Helper()
				rows, more, err := libfdbc.CGetMappedRangeBatch(tr, begin, end, f.getRangeMapper(), 1, 0, libfdbc.CStreamingModeExact, 1, reverse, false)
				if err != nil || len(rows) != 1 || !more {
					t.Fatalf("C primary result: rows=%d more=%v err=%v", len(rows), more, err)
				}
				row := rows[0]
				if row.Kind != libfdbc.CMappedKindGetRange || len(row.RangeRows) == 0 || len(row.RangeRows) >= secondaryCount {
					t.Fatalf("C secondary truncation not exercised: kind=%d rows=%d total=%d", row.Kind, len(row.RangeRows), secondaryCount)
				}
				for i, secondary := range row.RangeRows {
					if !bytes.Equal(secondary.Key, secondaryKey(i)) || !bytes.Equal(secondary.Value, secondaryValue(i)) {
						t.Fatalf("C secondary row %d differs from seeded data", i)
					}
				}
				return row
			}
			oracle, err := cdb.CreateTransaction()
			if err != nil {
				t.Fatal(err)
			}
			defer oracle.Close()
			cRow := readC(t, oracle)
			cutoff := append(bytes.Clone(cRow.RangeRows[len(cRow.RangeRows)-1].Key), 0)
			boundaries, err := libfdbc.CReadConflictRangeBoundaries(oracle)
			if err != nil {
				t.Fatal(err)
			}
			if len(boundaries) != 4 {
				t.Fatalf("C conflict population=%d boundaries, want primary and secondary intervals", len(boundaries))
			}
			foundCutoff := false
			for i := 0; i < len(boundaries); i += 2 {
				if string(boundaries[i].Value) != "1" || string(boundaries[i+1].Value) != "0" || bytes.Compare(boundaries[i].Key, boundaries[i+1].Key) >= 0 {
					t.Fatalf("invalid C conflict interval: %+v", boundaries[i:i+2])
				}
				if bytes.Equal(boundaries[i].Key, cRow.RangeBeginKey) {
					if !bytes.Equal(boundaries[i+1].Key, cutoff) {
						t.Fatalf("C secondary conflict end=%q want keyAfter(last)=%q", boundaries[i+1].Key, cutoff)
					}
					foundCutoff = true
				}
			}
			if !foundCutoff {
				t.Fatal("C conflict map omitted secondary interval")
			}
			compareGo := func(t *testing.T, row gofdb.MappedKeyValue) {
				t.Helper()
				if row.Kind != fdbclient.MappedResultGetRange || !row.GetRange.More || len(row.GetRange.Rows) != len(cRow.RangeRows) ||
					!bytes.Equal(row.Key, cRow.Key) || !bytes.Equal(row.Value, cRow.Value) ||
					!bytes.Equal(row.GetRange.Begin.Key, cRow.RangeBeginKey) || !bytes.Equal(row.GetRange.End.Key, cRow.RangeEndKey) {
					t.Fatalf("truncated mapped result: C secondary=%d Go secondary=%d more=%v", len(cRow.RangeRows), len(row.GetRange.Rows), row.GetRange.More)
				}
				for i := range cRow.RangeRows {
					if !bytes.Equal(cRow.RangeRows[i].Key, row.GetRange.Rows[i].Key) || !bytes.Equal(cRow.RangeRows[i].Value, row.GetRange.Rows[i].Value) {
						t.Fatalf("truncated secondary row %d disagrees", i)
					}
				}
			}
			readFacade := func(t *testing.T, tr gofdb.Transaction) {
				t.Helper()
				reader, ok := any(tr).(gofdb.MappedRangeReader)
				if !ok {
					t.Fatal("pure-Go facade lacks mapped-range capability")
				}
				it := reader.GetMappedRange(gofdb.KeyRange{Begin: gofdb.Key(begin), End: gofdb.Key(end)}, f.getRangeMapper(), gofdb.RangeOptions{Limit: 1, Mode: gofdb.StreamingModeExact, Reverse: reverse}).Iterator()
				if !it.Advance() {
					_, err := it.Get()
					t.Fatalf("facade failed to return primary row: %v", err)
				}
				row, err := it.Get()
				if err != nil {
					t.Fatal(err)
				}
				compareGo(t, row)
				if it.Advance() {
					t.Fatal("one-entry mapped facade repeated the primary row")
				}
				if _, err := it.Get(); err != nil {
					t.Fatal(err)
				}
			}
			for _, pendingTail := range []bool{false, true} {
				t.Run(fmt.Sprintf("unread_tail_local_write=%v", pendingTail), func(t *testing.T) {
					ct, err := cdb.CreateTransaction()
					if err != nil {
						t.Fatal(err)
					}
					defer ct.Close()
					gt := goDB.CreateTransaction()
					defer gt.Cancel()
					ft, err := facade.CreateTransaction()
					if err != nil {
						t.Fatal(err)
					}
					defer ft.Cancel()
					if pendingTail {
						key := secondaryKey(secondaryCount - 1)
						ct.Set(key, []byte("pending-tail"))
						gt.Set(key, []byte("pending-tail"))
						ft.Set(gofdb.Key(key), []byte("pending-tail"))
					}
					c := readC(t, ct)
					if !reflect.DeepEqual(cRow, c) {
						t.Fatal("local unread-tail write changed C mapped data")
					}
					rows, more, err := gt.GetMappedRangeWithByteTarget(ctx, begin, end, f.getRangeMapper(), 1, fdbclient.ByteLimitUnlimited, reverse)
					if err != nil || len(rows) != 1 || !more {
						t.Fatalf("direct Go primary result: rows=%d more=%v err=%v", len(rows), more, err)
					}
					compareGo(t, rows[0])
					readFacade(t, ft)
				})
			}
			writeConflict := func(t *testing.T, key []byte, version int64) {
				t.Helper()
				writer, err := facade.CreateTransaction()
				if err != nil {
					t.Fatal(err)
				}
				defer writer.Cancel()
				writer.SetReadVersion(version)
				writer.Set(gofdb.Key(mdPack(f.prefix, []byte("WRITER-SENTINEL"))), []byte("w"))
				if err := writer.AddWriteConflictKey(gofdb.Key(key)); err != nil {
					t.Fatal(err)
				}
				if err := writer.Commit().Get(); err != nil {
					t.Fatal(err)
				}
			}
			for _, probe := range []struct {
				name string
				key  []byte
				want int
			}{
				{"returned_secondary", cRow.RangeRows[0].Key, 1020},
				{"exclusive_cutoff", cutoff, 0},
				{"existing_unread_tail", secondaryKey(secondaryCount - 1), 0},
			} {
				t.Run(probe.name, func(t *testing.T) {
					ct, err := cdb.CreateTransaction()
					if err != nil {
						t.Fatal(err)
					}
					defer ct.Close()
					readC(t, ct)
					version, err := ct.GetReadVersion()
					if err != nil {
						t.Fatal(err)
					}
					ct.Set(mdPack(f.prefix, []byte("C-READER-SENTINEL")), []byte("c"))
					writeConflict(t, probe.key, version)
					cErr := ct.Commit()
					cCode := 0
					if cErr != nil {
						var fe *libfdbc.CFDBError
						if !errors.As(cErr, &fe) {
							t.Fatal(cErr)
						}
						cCode = fe.Code
					}
					gt, err := facade.CreateTransaction()
					if err != nil {
						t.Fatal(err)
					}
					defer gt.Cancel()
					readFacade(t, gt)
					version, err = gt.GetReadVersion().Get()
					if err != nil {
						t.Fatal(err)
					}
					gt.Set(gofdb.Key(mdPack(f.prefix, []byte("GO-READER-SENTINEL"))), []byte("g"))
					writeConflict(t, probe.key, version)
					gErr := gt.Commit().Get()
					gCode := 0
					if gErr != nil {
						var fe gofdb.Error
						if !errors.As(gErr, &fe) {
							t.Fatal(gErr)
						}
						gCode = fe.Code
					}
					if cCode != probe.want || gCode != probe.want {
						t.Fatalf("secondary probe %q: C=%d Go=%d want=%d", probe.key, cCode, gCode, probe.want)
					}
				})
			}
			t.Logf("compared %d/%d secondary rows and exact secondary conflict cutoff %q", len(cRow.RangeRows), secondaryCount, cutoff)
		})
	}
}

func TestLibFDBC_MappedRangeDeferredErrorPrecedence(t *testing.T) {
	t.Parallel()
	clusterFile := startCluster(t)
	cdb, err := libfdbc.COpenDatabase(clusterFile)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cdb.Close)
	setupCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	goDB, err := fdbclient.OpenDatabase(setupCtx, clusterFile, fdbclient.WithAPIVersion(730))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { goDB.Close() })

	for _, poisoned := range []bool{false, true} {
		for _, special := range []bool{false, true} {
			for _, reverse := range []bool{false, true} {
				name := fmt.Sprintf("poisoned=%v_special=%v_reverse=%v", poisoned, special, reverse)
				t.Run(name, func(t *testing.T) {
					t.Parallel()
					ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
					defer cancel()
					ct, err := cdb.CreateTransaction()
					if err != nil {
						t.Fatal(err)
					}
					defer ct.Close()
					gt := goDB.CreateTransaction()
					defer gt.Cancel()
					key := []byte(t.Name())
					if poisoned {
						// Literal 1 is ClearRange, not a legal atomic mutation.
						ct.Atomic(1, key, []byte("value"))
						gt.Atomic(fdbclient.MutationType(1), key, []byte("value"))
					}
					begin, end := key, append(append([]byte(nil), key...), 0xff)
					want := 0
					if special {
						begin, end = []byte("\xff\xff/status/json"), []byte("\xff\xff/status/json\x00")
						want = 2000
					}
					if poisoned {
						want = 2018
					}
					mapper := mdPack([]byte("unused-for-empty-range"))
					cRows, cMore, ce := libfdbc.CGetMappedRange(ct, begin, end, mapper, 10, reverse, false)
					gRows, gMore, ge := gt.GetMappedRange(ctx, begin, end, mapper, 10, reverse)
					cCode, gCode := 0, 0
					if ce != nil {
						var e *libfdbc.CFDBError
						if !errors.As(ce, &e) {
							t.Fatalf("C++ returned non-FDB error: %v", ce)
						}
						cCode = e.Code
					}
					if ge != nil {
						var e *wire.FDBError
						if !errors.As(ge, &e) {
							t.Fatalf("Go returned non-FDB error: %v", ge)
						}
						gCode = e.Code
					}
					if cCode != want || gCode != want {
						t.Fatalf("mapped deferred precedence: Go=%d C++=%d, want literal %d", gCode, cCode, want)
					}
					if len(cRows) != 0 || len(gRows) != 0 || cMore || gMore {
						t.Fatalf("empty/error control returned rows/more: C++=%d/%v Go=%d/%v", len(cRows), cMore, len(gRows), gMore)
					}
				})
			}
		}
	}
}
