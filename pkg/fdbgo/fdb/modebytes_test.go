package fdb

import "testing"

// TestModeTargetBytes_MatchesCTable drives the Go->C streaming-mode conversion against BOTH
// numberings explicitly, because the failure it guards is silent.
//
// mode_bytes_array (fdb_c.cpp:1002) is indexed by the C numbering (EXACT=0 .. SERIAL=4) and Go
// uses Apple's, which is C+1. Indexing the table with a Go mode compiles, runs, and hands SMALL
// the MEDIUM target, MEDIUM the LARGE target, LARGE the SERIAL target, and reads out of bounds
// for Serial. Every one of those produces a wrong per-fetch division that a row-count assertion
// downstream cannot distinguish from a correct one.
//
// So this asserts the LITERAL target per mode, not that two derivations agree: a check of the
// form "table[cModeIndex(m)] == modeTargetBytes(m)" shares its whole derivation with the code
// under test and would hold under exactly the off-by-one this exists to catch.
func TestModeTargetBytes_MatchesCTable(t *testing.T) {
	t.Parallel()

	for _, c := range []struct {
		name string
		mode StreamingMode
		want int
	}{
		// fdb_c.cpp:1002, read through the C index each mode maps to.
		{"exact_is_unlimited", StreamingModeExact, ByteLimitUnlimited},
		{"small", StreamingModeSmall, 256},
		{"medium", StreamingModeMedium, 1000},
		{"large", StreamingModeLarge, 4096},
		{"serial", StreamingModeSerial, 80000},
		// fdb_c.cpp:1011 — WANT_ALL is rewritten to SERIAL, so it shares 80000.
		{"want_all_maps_to_serial", StreamingModeWantAll, 80000},
	} {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got, err := modeTargetBytes(c.mode, 1)
			if err != nil {
				t.Fatalf("modeTargetBytes(%v): unexpected error %v", c.mode, err)
			}
			if got != c.want {
				t.Errorf("modeTargetBytes(%v) = %d, want %d. If SMALL reads 1000, MEDIUM 4096 or "+
					"LARGE 80000, the table is being indexed with the GO numbering where it must be "+
					"indexed with the C one (Go = C+1); fix cModeIndex, not this expectation.",
					c.mode, got, c.want)
			}
		})
	}
}

// TestCModeIndex_IsTheCNumbering pins the offset itself, independently of the table, so a
// regression names the conversion rather than surfacing as a wrong byte target three layers down.
func TestCModeIndex_IsTheCNumbering(t *testing.T) {
	t.Parallel()

	// fdb_c_options.g.h:526-544.
	for _, c := range []struct {
		name string
		mode StreamingMode
		want int
	}{
		{"want_all", StreamingModeWantAll, -2},
		{"iterator", StreamingModeIterator, -1},
		{"exact", StreamingModeExact, 0},
		{"small", StreamingModeSmall, 1},
		{"medium", StreamingModeMedium, 2},
		{"large", StreamingModeLarge, 3},
		{"serial", StreamingModeSerial, 4},
	} {
		if got := cModeIndex(c.mode); got != c.want {
			t.Errorf("cModeIndex(%s) = %d, want %d (C numbering)", c.name, got, c.want)
		}
	}

	// The bounds guard is expressed on the C value, so SERIAL must be the LAST in-range index.
	// If the table ever grows or the enum shifts, this is what catches an out-of-bounds read
	// before it becomes a panic on a live range read.
	if cModeIndex(StreamingModeSerial) != len(modeBytesTable)-1 {
		t.Errorf("SERIAL maps to C index %d but mode_bytes_array has %d entries — the guard in "+
			"modeTargetBytes is the bounds check and they must agree",
			cModeIndex(StreamingModeSerial), len(modeBytesTable))
	}
}

// TestModeTargetBytes_RejectsOutOfRangeMode pins the arm that is also the BOUNDS CHECK.
//
// fdb_c.cpp:1022 admits only `mode >= 0 && mode <= FDB_STREAMING_MODE_SERIAL` in C numbering and
// returns client_invalid_operation otherwise. In Go that guard is what keeps the table index in
// range, so if it were dropped or written against the Go numbering an out-of-range mode would
// not error — it would read past the end of modeBytesTable, or silently return a neighbouring
// mode's target. Neither is visible in a division measurement.
func TestModeTargetBytes_RejectsOutOfRangeMode(t *testing.T) {
	t.Parallel()

	for _, c := range []struct {
		name string
		mode StreamingMode
	}{
		{"below_want_all", StreamingMode(-2)},
		{"far_below", StreamingMode(-99)},
		{"just_past_serial", StreamingModeSerial + 1},
		{"far_above", StreamingMode(99)},
	} {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got, err := modeTargetBytes(c.mode, 1)
			if err == nil {
				t.Errorf("modeTargetBytes(%d) returned %d and no error; a mode outside "+
					"EXACT..SERIAL is client_invalid_operation, and this guard is also the "+
					"bounds check on modeBytesTable", int(c.mode), got)
			}
		})
	}
}

// TestModeTargetBytes_IteratorProgression pins the ITERATOR arm, whose target depends on the
// iteration number — the arm most likely to be subtly wrong, since nothing else in the range
// path threaded an iteration count before this port.
func TestModeTargetBytes_IteratorProgression(t *testing.T) {
	t.Parallel()

	// fdb_c.cpp:1006, indexed by iteration-1.
	want := []int{4096, 6144, 9216, 13824, 20736, 31104, 46656, 69984, 80000, 120000}
	for i, w := range want {
		iteration := i + 1
		got, err := modeTargetBytes(StreamingModeIterator, iteration)
		if err != nil {
			t.Fatalf("iteration %d: unexpected error %v", iteration, err)
		}
		if got != w {
			t.Errorf("modeTargetBytes(ITERATOR, %d) = %d, want %d", iteration, got, w)
		}
	}

	// fdb_c.cpp:1019 SATURATES rather than growing or running off the end. Past the table the C
	// client re-uses the last target, so no single fetch ever targets more than 120000 bytes.
	for _, iteration := range []int{11, 12, 50, 1000} {
		got, err := modeTargetBytes(StreamingModeIterator, iteration)
		if err != nil {
			t.Fatalf("iteration %d: unexpected error %v", iteration, err)
		}
		if got != 120000 {
			t.Errorf("modeTargetBytes(ITERATOR, %d) = %d, want 120000 — the progression must "+
				"SATURATE at the table's last entry, not grow or index past it", iteration, got)
		}
	}

	// fdb_c.cpp:1017 — iteration <= 0 is client_invalid_operation, not a silent clamp to 1.
	for _, iteration := range []int{0, -1} {
		if _, err := modeTargetBytes(StreamingModeIterator, iteration); err == nil {
			t.Errorf("modeTargetBytes(ITERATOR, %d) returned no error; C rejects a non-positive "+
				"iteration with client_invalid_operation", iteration)
		}
	}
}
