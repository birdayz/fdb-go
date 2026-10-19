// Command wgscen runs deterministic single-threaded scenarios on the client
// compiled in (pure Go, or libfdb_c with the libfdbc tag) for wire goldens
// (scripts/wiregolden.sh). Each scenario runs in its own transaction after a
// marker read of \x02wg/marker/<name>, which splits the capture by scenario.
// Add a scenario by appending to scenarios; regenerate the golden from libfdb_c.
package main

import (
	"flag"
	"fmt"
	"os"

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

func main() {
	cluster := flag.String("cluster-file", "", "cluster file")
	flag.Parse()
	if err := fdb.APIVersion(730); err != nil {
		fail(err)
	}
	db, err := open(*cluster)
	if err != nil {
		fail(err)
	}
	defer db.Close()
	// Seed and warm the location cache.
	if _, err := db.Transact(func(tr fdb.WritableTransaction) (any, error) {
		tr.ClearRange(fdb.KeyRange{Begin: k(""), End: k("\xff")})
		for _, s := range []string{"a", "b", "c", "d", "e", "m0", "m5"} {
			tr.Set(k(s), []byte("seed-"+s))
		}
		return nil, nil
	}); err != nil {
		fail(err)
	}
	for _, s := range scenarios {
		if _, err := db.ReadTransact(func(tr fdb.ReadTransaction) (any, error) {
			return tr.Get(fdb.Key("\x02wg/marker/" + s.name)).Get()
		}); err != nil {
			fail(err)
		}
		if _, err := db.Transact(func(tr fdb.WritableTransaction) (any, error) { return nil, s.run(tr) }); err != nil {
			fail(fmt.Errorf("%s: %w", s.name, err))
		}
	}
	if _, err := db.ReadTransact(func(tr fdb.ReadTransaction) (any, error) {
		return tr.Get(fdb.Key("\x02wg/marker/~end")).Get()
	}); err != nil {
		fail(err)
	}
}

func fail(err error) { fmt.Fprintln(os.Stderr, err); os.Exit(1) }
