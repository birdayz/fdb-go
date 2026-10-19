// Package wgscenarios holds the wire-golden scenarios: deterministic, one
// transaction each, run by one goroutine. Before each scenario a marker
// transaction reads MarkerPrefix+name at the last committed version
// (SetReadVersion), so the marker sends one GetValueRequest and no GRV; a
// final ~end marker closes the run. Add a scenario by appending to scenarios,
// then regenerate the goldens (scripts/wiregolden.sh -update).
package wgscenarios

import (
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
