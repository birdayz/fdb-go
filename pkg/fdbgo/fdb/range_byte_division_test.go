package fdb_test

import (
	"fmt"
	"testing"

	gofdb "fdb.dev/pkg/fdbgo/fdb"
)

// TestRangeIterator_DivisionIsByteDrivenNotRowDriven pins the pure-Go client's batch division as
// a function of ROW SIZE, matching libfdb_c's per-mode byte targets.
//
// It replaces an assertion of the opposite property. Before the GetRangeLimits port this file
// pinned the division as row-driven and SIZE-INVARIANT — deliberately, as a recorded divergence
// — and said that when the port landed it must be rewritten against the C table rather than
// relaxed. This is that rewrite; the old expectations are not casualties, they were the
// divergence itself.
//
// WHERE THE DIVISION COMES FROM, since it is not where it looks. libfdb_c derives a per-fetch
// byte target from mode_bytes_array (fdb_c.cpp:1002) — SMALL 256, MEDIUM 1000, LARGE 4096,
// SERIAL 80000 — and applies it in TWO places, both required:
//
//  1. transformRangeLimits (NativeAPI.actor.cpp:4223) puts it on the REQUEST, and the STORAGE
//     SERVER truncates the reply there. That truncation is the batch boundary.
//  2. The scan loop's soft byte limit (getExactRange:4415) ends the call at that one reply
//     instead of absorbing it and re-querying.
//
// Neither half alone reproduces C. The request alone is invisible (the loop just re-queries);
// the loop alone cuts at a different row, because the server's reply-size accounting is not the
// client's — measured at 19 where C cuts 18, in
// libfdbc:TestLibFDBC_ByteTargetCutIsNotTheClientSideBudget.
//
// The FAT divisions below are the values libfdb_c itself produced over the same 60 rows of
// 200-byte values in libfdbc:TestLibFDBC_RangeBatchDivision — [2]x30, [5]x12, [18 18 18 6].
// They are asserted as LITERALS rather than against a re-derivation of the mode table, because
// a comparison whose two sides share a derivation holds vacuously under any change that breaks
// both.
func TestRangeIterator_DivisionIsByteDrivenNotRowDriven(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)

	const n = 60

	// division indexes the expected batches by row size. The two sizes are the whole point: a
	// byte-driven client MUST divide them differently, and the previous row-driven client could
	// not tell them apart at all. Trailing zero batches are kept as measured — a fetch whose
	// reply was byte-truncated reports more=true even when it happened to take the last row, so
	// the drain spends one empty fetch discovering the end. C shows the same artifact.
	type arm struct {
		mode gofdb.StreamingMode
		want []int
	}

	for _, seed := range []struct {
		name     string
		valueLen int
		arms     []arm
	}{
		{
			// 1-byte values: the whole 60-row range is well under even SMALL's 256-byte
			// target per fetch only for a few rows, so the divisions are wide.
			name:     "thin",
			valueLen: 1,
			arms: []arm{
				{gofdb.StreamingModeSmall, []int{7, 7, 7, 7, 7, 7, 7, 7, 4}},
				{gofdb.StreamingModeMedium, []int{24, 24, 12}},
				{gofdb.StreamingModeLarge, []int{60}},
			},
		},
		{
			// 200-byte values: these are libfdb_c's own measured divisions.
			name:     "fat",
			valueLen: 200,
			arms: []arm{
				{gofdb.StreamingModeSmall, repeat(2, 30, true)},
				{gofdb.StreamingModeMedium, repeat(5, 12, true)},
				{gofdb.StreamingModeLarge, []int{18, 18, 18, 6}},
			},
		},
	} {
		seed := seed
		t.Run(seed.name, func(t *testing.T) {
			t.Parallel()
			pfx := fmt.Sprintf("bytediv_%s_", seed.name)
			if _, err := db.Transact(func(tr gofdb.WritableTransaction) (any, error) {
				for i := 0; i < n; i++ {
					tr.Set(gofdb.Key(fmt.Sprintf("%s%04d", pfx, i)), make([]byte, seed.valueLen))
				}
				return nil, nil
			}); err != nil {
				t.Fatalf("seed: %v", err)
			}

			for _, a := range seed.arms {
				keys, batches := drainClientBatches(t, db, pfx, pfx+"~", gofdb.RangeOptions{Mode: a.mode})
				if len(keys) != n {
					t.Errorf("%s/%v: %d rows, want %d — the division may differ but the ANSWER may not",
						seed.name, a.mode, len(keys), n)
				}
				if !equalIntSlice(batches, a.want) {
					t.Errorf("%s/%v: division %v, want %v. These are libfdb_c's own per-mode byte "+
						"targets applied to this row size; a change means the mode table moved, the "+
						"per-mode LimitBytes stopped reaching the request, or the soft byte limit "+
						"stopped ending the call — do not relax this, find which half broke",
						seed.name, a.mode, batches, a.want)
				}
				t.Logf("MEASURED %-6s %-6v rows=%d division=%v", seed.name, a.mode, len(keys), batches)
			}
		})
	}
}

// TestRangeIterator_DivisionIsSizeSensitive asserts the PROPERTY the port established, separately
// from the literal values above: the same row COUNT at two different row SIZES must divide
// differently. This is the exact property that was false before the port, and stating it on its
// own means a future change cannot satisfy the literals by accident while losing the byte
// dimension — for instance by hard-coding per-mode row counts that happen to match one size.
func TestRangeIterator_DivisionIsSizeSensitive(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)

	const n = 60
	seedRange := func(pfx string, valueLen int) {
		if _, err := db.Transact(func(tr gofdb.WritableTransaction) (any, error) {
			for i := 0; i < n; i++ {
				tr.Set(gofdb.Key(fmt.Sprintf("%s%04d", pfx, i)), make([]byte, valueLen))
			}
			return nil, nil
		}); err != nil {
			t.Fatalf("seed %s: %v", pfx, err)
		}
	}
	seedRange("bytesens_thin_", 1)
	seedRange("bytesens_fat_", 200)

	for _, mode := range []gofdb.StreamingMode{
		gofdb.StreamingModeSmall, gofdb.StreamingModeMedium,
	} {
		_, thin := drainClientBatches(t, db, "bytesens_thin_", "bytesens_thin_~", gofdb.RangeOptions{Mode: mode})
		_, fat := drainClientBatches(t, db, "bytesens_fat_", "bytesens_fat_~", gofdb.RangeOptions{Mode: mode})
		if equalIntSlice(thin, fat) {
			t.Errorf("mode %v divided 1-byte and 200-byte rows IDENTICALLY (%v). The division is "+
				"supposed to be driven by a per-fetch BYTE target, so a 200x change in row size "+
				"must move it; identical divisions mean the byte dimension is gone and the client "+
				"is back to counting rows", mode, thin)
		}
		t.Logf("MEASURED %-6v thin=%v fat=%v", mode, thin, fat)
	}
}

// TestRangeIterator_FirstFetchUsesFirstProgressionEntry pins WHICH entry of
// iteration_progression a fresh ITERATOR scan's first fetch takes.
//
// This dimension was unprobed and a real off-by-one shipped through every other test here. The
// iterator's per-fetch byte target was derived from `iteration + 1` while its own counter was
// already 1-based, so the first fetch targeted 6144 bytes instead of 4096 and
// iterationProgression[0] was unreachable by any real scan. Nothing caught it: the cross-client
// differential drives its own `for iteration := 1` loop and never consults the iterator's
// bookkeeping; the saturation test asserts only relative shape (grow 1.5x, then plateau), which
// 6144->9216 satisfies exactly as well as 4096->6144; and the table test pins the progression's
// VALUES without pinning who indexes it.
//
// The C API's iteration is 1-based — bindings/java/.../RangeQuery.java:121 holds `iteration = 0`
// and passes `++iteration` at :225 — so the first fetch is iteration 1 and takes 4096.
//
// The fixture is chosen so 4096 and 6144 give DIFFERENT row counts, which is the only way this
// test can see the difference: at 128 bytes per row the two targets admit 32 and 48 rows.
func TestRangeIterator_FirstFetchUsesFirstProgressionEntry(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)

	// Row cost against a byte target is key + value + 24 (a measured per-row server overhead;
	// see pkg/simfdb's serverRowOverheadBytes for the evidence). A 12-byte key ("firstprog_" +
	// 4 digits is 14) with a 90-byte value gives 14+90+24 = 128 bytes per row, so:
	//   4096 / 128 = 32 rows  <- correct, iteration 1
	//   6144 / 128 = 48 rows  <- the off-by-one
	const (
		pfx      = "firstprog_"
		n        = 200
		valueLen = 90
	)
	if _, err := db.Transact(func(tr gofdb.WritableTransaction) (any, error) {
		for i := 0; i < n; i++ {
			tr.Set(gofdb.Key(fmt.Sprintf("%s%04d", pfx, i)), make([]byte, valueLen))
		}
		return nil, nil
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	_, batches := drainClientBatches(t, db, pfx, pfx+"~", gofdb.RangeOptions{
		Mode: gofdb.StreamingModeIterator,
	})
	if len(batches) == 0 {
		t.Fatalf("no batches recorded — the drain did not run")
	}
	const wantFirst = 32
	if batches[0] != wantFirst {
		t.Errorf("first ITERATOR fetch returned %d rows, want %d. The first fetch must use "+
			"iterationProgression[0] = 4096 bytes; %d rows is what a 6144-byte target admits at "+
			"this row size, i.e. the iteration number is off by one and the progression's first "+
			"entry is unreachable. Full division: %v", batches[0], wantFirst, 48, batches)
	}
	t.Logf("MEASURED first-fetch division=%v", batches)
}

// repeat builds a division of count batches of size each, optionally with the trailing empty
// fetch a byte-truncated final reply forces.
func repeat(size, count int, trailingEmpty bool) []int {
	out := make([]int, 0, count+1)
	for i := 0; i < count; i++ {
		out = append(out, size)
	}
	if trailingEmpty {
		out = append(out, 0)
	}
	return out
}

func equalIntSlice(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
