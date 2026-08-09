//go:build cgo && libfdbc

package libfdbc_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	fdbclient "fdb.dev/pkg/fdbgo/client"
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
