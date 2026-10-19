// Package wgscenarios holds the wire-golden scenarios: deterministic, one
// transaction each, run by one goroutine. Before each scenario a marker
// transaction reads MarkerPrefix+name at the last committed version
// (SetReadVersion), so the marker sends one GetValueRequest and no GRV; a
// final ~end marker closes the run. Add a scenario by appending to scenarios,
// then regenerate the goldens (scripts/wiregolden.sh -update).
package wgscenarios

import (
	"bytes"
	"fmt"

	"fdb.dev/pkg/fdbgo/fdb"
)

var p = []byte("\x02wg/d/")

func k(s string) fdb.Key { return fdb.Key(append(append([]byte{}, p...), s...)) }

type scenario struct {
	name string
	run  func(tr fdb.WritableTransaction) error
}

func get(tr fdb.ReadTransaction, key fdb.Key) error { _, err := tr.Get(key).Get(); return err }

var scenarios = []scenario{
	{"blind_set", func(tr fdb.WritableTransaction) error { tr.Set(k("x"), []byte("1")); return nil }},
	{"get_then_set", func(tr fdb.WritableTransaction) error {
		if err := get(tr, k("a")); err != nil {
			return err
		}
		tr.Set(k("x"), []byte("2"))
		return nil
	}},
	{"read_only_get", func(tr fdb.WritableTransaction) error { return get(tr, k("b")) }},
	{"ryw_set_then_get", func(tr fdb.WritableTransaction) error {
		tr.Set(k("y"), []byte("3"))
		return get(tr, k("y"))
	}},
	{"snapshot_get_then_set", func(tr fdb.WritableTransaction) error {
		if _, err := tr.Snapshot().Get(k("a")).Get(); err != nil {
			return err
		}
		tr.Set(k("x"), []byte("4"))
		return nil
	}},
	{"range_limit3", func(tr fdb.WritableTransaction) error {
		_, err := tr.GetRange(fdb.KeyRange{Begin: k(""), End: k("\xff")}, fdb.RangeOptions{Limit: 3}).GetSliceWithError()
		return err
	}},
	{"range_reverse_limit2", func(tr fdb.WritableTransaction) error {
		_, err := tr.GetRange(fdb.KeyRange{Begin: k(""), End: k("\xff")}, fdb.RangeOptions{Limit: 2, Reverse: true}).GetSliceWithError()
		return err
	}},
	{"range_want_all", func(tr fdb.WritableTransaction) error {
		_, err := tr.GetRange(fdb.KeyRange{Begin: k(""), End: k("\xff")}, fdb.RangeOptions{Mode: fdb.StreamingModeWantAll}).GetSliceWithError()
		return err
	}},
	{"getkey_offset", func(tr fdb.WritableTransaction) error {
		_, err := tr.GetKey(fdb.KeySelector{Key: k("b"), OrEqual: true, Offset: 2}).Get()
		return err
	}},
	{"clear_range_then_set", func(tr fdb.WritableTransaction) error {
		tr.ClearRange(fdb.KeyRange{Begin: k("m"), End: k("n")})
		tr.Set(k("m1"), []byte("5"))
		return nil
	}},
	{"atomic_add", func(tr fdb.WritableTransaction) error {
		tr.Add(k("ctr"), []byte{1, 0, 0, 0, 0, 0, 0, 0})
		return nil
	}},
	{"set_same_key_twice", func(tr fdb.WritableTransaction) error {
		tr.Set(k("z"), []byte("a"))
		tr.Set(k("z"), []byte("b"))
		return nil
	}},
	{"versionstamped_key", func(tr fdb.WritableTransaction) error {
		key := append([]byte(k("vs")), make([]byte, 10)...)
		key = append(key, byte(len(k("vs"))), 0, 0, 0)
		tr.SetVersionstampedKey(fdb.Key(key), []byte("v"))
		return nil
	}},
	{"clear_key", func(tr fdb.WritableTransaction) error { tr.Clear(k("c")); return nil }},
	{"atomic_max_missing", func(tr fdb.WritableTransaction) error { tr.Max(k("mx"), []byte{5, 0}); return nil }},
	{"atomic_min_existing", func(tr fdb.WritableTransaction) error { tr.Min(k("a"), []byte("seed")); return nil }},
	{"atomic_byte_min_max", func(tr fdb.WritableTransaction) error {
		tr.ByteMin(k("b"), []byte("aaa"))
		tr.ByteMax(k("c"), []byte("zzz"))
		return nil
	}},
	{"atomic_and_or_xor", func(tr fdb.WritableTransaction) error {
		tr.BitAnd(k("x1"), []byte{0xf0})
		tr.BitOr(k("x2"), []byte{0x0f})
		tr.BitXor(k("x3"), []byte{0xff})
		return nil
	}},
	{"atomic_append_if_fits", func(tr fdb.WritableTransaction) error { tr.AppendIfFits(k("d"), []byte("+tail")); return nil }},
	{"compare_and_clear_match", func(tr fdb.WritableTransaction) error { tr.CompareAndClear(k("e"), []byte("seed-e")); return nil }},
	{"set_then_add_same_key", func(tr fdb.WritableTransaction) error {
		tr.Set(k("sa"), []byte{1, 0, 0, 0})
		tr.Add(k("sa"), []byte{2, 0, 0, 0})
		return nil
	}},
	{"add_then_get", func(tr fdb.WritableTransaction) error {
		tr.Add(k("ag"), []byte{1, 0, 0, 0})
		return get(tr, k("ag"))
	}},
	{"versionstamped_value", func(tr fdb.WritableTransaction) error {
		tr.SetVersionstampedValue(k("vv"), append(make([]byte, 10), 0, 0, 0, 0))
		return nil
	}},
	{"set_empty_value", func(tr fdb.WritableTransaction) error { tr.Set(k("ev"), []byte{}); return nil }},
	{"set_large_value", func(tr fdb.WritableTransaction) error { tr.Set(k("lv"), bytes.Repeat([]byte("v"), 10000)); return nil }},
	{"clear_range_empty", func(tr fdb.WritableTransaction) error {
		tr.ClearRange(fdb.KeyRange{Begin: k("q"), End: k("q")})
		tr.Set(k("q1"), []byte("1"))
		return nil
	}},
	{"overlapping_clear_ranges", func(tr fdb.WritableTransaction) error {
		tr.ClearRange(fdb.KeyRange{Begin: k("f"), End: k("h")})
		tr.ClearRange(fdb.KeyRange{Begin: k("g"), End: k("j")})
		return nil
	}},
	{"set_then_clear_same_key", func(tr fdb.WritableTransaction) error {
		tr.Set(k("sc"), []byte("1"))
		tr.Clear(k("sc"))
		return nil
	}},
	{"clear_then_get", func(tr fdb.WritableTransaction) error {
		tr.Clear(k("a"))
		return get(tr, k("a"))
	}},
	{"set_then_range", func(tr fdb.WritableTransaction) error {
		tr.Set(k("b5"), []byte("new"))
		_, err := tr.GetRange(fdb.KeyRange{Begin: k("a"), End: k("d")}, fdb.RangeOptions{}).GetSliceWithError()
		return err
	}},
	{"snapshot_range", func(tr fdb.WritableTransaction) error {
		_, err := tr.Snapshot().GetRange(fdb.KeyRange{Begin: k("a"), End: k("d")}, fdb.RangeOptions{}).GetSliceWithError()
		return err
	}},
	{"range_exact_limit5", func(tr fdb.WritableTransaction) error {
		_, err := tr.GetRange(fdb.KeyRange{Begin: k(""), End: k("\xff")}, fdb.RangeOptions{Limit: 5, Mode: fdb.StreamingModeExact}).GetSliceWithError()
		return err
	}},
	{"range_reverse_want_all", func(tr fdb.WritableTransaction) error {
		_, err := tr.GetRange(fdb.KeyRange{Begin: k(""), End: k("\xff")}, fdb.RangeOptions{Mode: fdb.StreamingModeWantAll, Reverse: true}).GetSliceWithError()
		return err
	}},
	{"range_small_mode", func(tr fdb.WritableTransaction) error {
		_, err := tr.GetRange(fdb.KeyRange{Begin: k(""), End: k("\xff")}, fdb.RangeOptions{Mode: fdb.StreamingModeSmall}).GetSliceWithError()
		return err
	}},
	{"getkey_first_greater_than", func(tr fdb.WritableTransaction) error {
		_, err := tr.GetKey(fdb.FirstGreaterThan(k("b"))).Get()
		return err
	}},
	{"getkey_last_less_than", func(tr fdb.WritableTransaction) error {
		_, err := tr.GetKey(fdb.LastLessThan(k("c"))).Get()
		return err
	}},
	{"getkey_last_less_or_equal", func(tr fdb.WritableTransaction) error {
		_, err := tr.GetKey(fdb.LastLessOrEqual(k("c"))).Get()
		return err
	}},
	{"getkey_negative_offset", func(tr fdb.WritableTransaction) error {
		_, err := tr.GetKey(fdb.KeySelector{Key: k("d"), Offset: -2}).Get()
		return err
	}},
	{"getkey_after_set", func(tr fdb.WritableTransaction) error {
		tr.Set(k("b1"), []byte("1"))
		_, err := tr.GetKey(fdb.FirstGreaterThan(k("b"))).Get()
		return err
	}},
	{"explicit_conflict_ranges", func(tr fdb.WritableTransaction) error {
		if err := tr.AddReadConflictRange(fdb.KeyRange{Begin: k("r1"), End: k("r2")}); err != nil {
			return err
		}
		if err := tr.AddWriteConflictKey(k("w1")); err != nil {
			return err
		}
		tr.Set(k("w2"), []byte("1"))
		return nil
	}},
	{"no_write_conflict_set", func(tr fdb.WritableTransaction) error {
		if err := tr.Options().SetNextWriteNoWriteConflictRange(); err != nil {
			return err
		}
		tr.Set(k("nw"), []byte("1"))
		tr.Set(k("nw2"), []byte("2"))
		return nil
	}},
	{"ryw_disabled_set_get", func(tr fdb.WritableTransaction) error {
		if err := tr.Options().SetReadYourWritesDisable(); err != nil {
			return err
		}
		tr.Set(k("rd"), []byte("1"))
		return get(tr, k("rd"))
	}},
	{"priority_batch_get", func(tr fdb.WritableTransaction) error {
		if err := tr.Options().SetPriorityBatch(); err != nil {
			return err
		}
		return get(tr, k("a"))
	}},
	{"priority_system_immediate_get", func(tr fdb.WritableTransaction) error {
		if err := tr.Options().SetPrioritySystemImmediate(); err != nil {
			return err
		}
		return get(tr, k("a"))
	}},
	{"causal_read_risky_get", func(tr fdb.WritableTransaction) error {
		if err := tr.Options().SetCausalReadRisky(); err != nil {
			return err
		}
		return get(tr, k("a"))
	}},
	{"read_system_key", func(tr fdb.WritableTransaction) error {
		if err := tr.Options().SetReadSystemKeys(); err != nil {
			return err
		}
		return get(tr, fdb.Key("\xff/conf/storage_engine"))
	}},
	{"get_read_version_only", func(tr fdb.WritableTransaction) error { _, err := tr.GetReadVersion().Get(); return err }},
	{"estimated_range_size", func(tr fdb.WritableTransaction) error {
		_, err := tr.GetEstimatedRangeSizeBytes(fdb.KeyRange{Begin: k(""), End: k("\xff")}).Get()
		return err
	}},
	{"range_split_points", func(tr fdb.WritableTransaction) error {
		_, err := tr.GetRangeSplitPoints(fdb.KeyRange{Begin: k(""), End: k("\xff")}, 1000).Get()
		return err
	}},
	{"get_missing_then_set_missing", func(tr fdb.WritableTransaction) error {
		if err := get(tr, k("nope")); err != nil {
			return err
		}
		tr.Set(k("nope"), []byte("1"))
		return nil
	}},
	{"sets_out_of_order", func(tr fdb.WritableTransaction) error {
		tr.Set(k("z9"), []byte("1"))
		tr.Set(k("a9"), []byte("2"))
		tr.Set(k("m9"), []byte("3"))
		return nil
	}},
	{"mixed_ops_out_of_order", func(tr fdb.WritableTransaction) error {
		tr.Add(k("t3"), []byte{1})
		tr.Set(k("t1"), []byte("1"))
		tr.Clear(k("t2"))
		tr.ClearRange(fdb.KeyRange{Begin: k("t0"), End: k("t0z")})
		return nil
	}},
	{"gets_out_of_order_then_set", func(tr fdb.WritableTransaction) error {
		if err := get(tr, k("c")); err != nil {
			return err
		}
		if err := get(tr, k("a")); err != nil {
			return err
		}
		tr.Set(k("x"), []byte("5"))
		return nil
	}},
	{"overlapping_range_reads_then_set", func(tr fdb.WritableTransaction) error {
		if _, err := tr.GetRange(fdb.KeyRange{Begin: k("a"), End: k("c")}, fdb.RangeOptions{}).GetSliceWithError(); err != nil {
			return err
		}
		if _, err := tr.GetRange(fdb.KeyRange{Begin: k("b"), End: k("d")}, fdb.RangeOptions{}).GetSliceWithError(); err != nil {
			return err
		}
		tr.Set(k("x"), []byte("6"))
		return nil
	}},
	{"two_gets", func(tr fdb.WritableTransaction) error {
		if err := get(tr, k("a")); err != nil {
			return err
		}
		return get(tr, k("missing"))
	}},
}

// Names lists the scenarios in run order.
func Names() []string {
	names := make([]string, len(scenarios))
	for i, s := range scenarios {
		names[i] = s.name
	}
	return names
}

// Run seeds the keys (setup, not compared), then runs every scenario after its
// marker, then the ~end marker.
func Run(db fdb.BackendDatabase) error {
	last, err := commit(db, func(tr fdb.WritableTransaction) error {
		tr.ClearRange(fdb.KeyRange{Begin: k(""), End: k("\xff")})
		for _, s := range []string{"a", "b", "c", "d", "e", "m0", "m5"} {
			tr.Set(k(s), []byte("seed-"+s))
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("setup: %w", err)
	}
	for _, s := range scenarios {
		if err := mark(db, s.name, last); err != nil {
			return fmt.Errorf("marker %s: %w", s.name, err)
		}
		v, err := commit(db, s.run)
		if err != nil {
			return fmt.Errorf("%s: %w", s.name, err)
		}
		last = max(last, v)
	}
	return mark(db, "~end", last)
}

// commit runs fn in one transaction, without a retry loop, and returns its
// committed version (-1 for a transaction that wrote nothing).
func commit(db fdb.BackendDatabase, fn func(fdb.WritableTransaction) error) (int64, error) {
	tr, err := db.CreateWritableTransaction()
	if err != nil {
		return 0, err
	}
	if err := fn(tr); err != nil {
		tr.Cancel()
		return 0, err
	}
	if err := tr.Commit().Get(); err != nil {
		return 0, err
	}
	return tr.GetCommittedVersion()
}

// mark reads the scenario's marker key at version, which needs no GRV.
func mark(db fdb.BackendDatabase, name string, version int64) error {
	tr, err := db.CreateWritableTransaction()
	if err != nil {
		return err
	}
	defer tr.Cancel()
	tr.SetReadVersion(version)
	_, err = tr.Get(fdb.Key(MarkerPrefix + name)).Get()
	return err
}

// MarkerPrefix starts a marker key; the scenario name follows.
const MarkerPrefix = "\x02wg/marker/"
