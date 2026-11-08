//go:build cgo && libfdbc

package libfdbc_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/libfdbc"
	foundationdbtc "fdb.dev/pkg/testcontainers/foundationdb"
	cgofdb "github.com/apple/foundationdb/bindings/go/src/fdb"
)

// REPORT_CONFLICTING_KEYS against libfdb_c 7.3.77. Each backend runs the same conflict
// patterns through the fdb facade on its own key prefix and records what
// \xff\xff/transaction/conflicting_keys/ then reads: whole map, reversed, limited, through
// selectors, key by key, after OnError and after Reset. The two transcripts must be
// identical, and the maps are also checked against values derived from C++
// (NativeAPI.actor.cpp:6700-6717: "1" where a coalesced conflicting read range begins, "0"
// where it ends; kept across onError/reset, cloneAndReset :3531; replaced only by the next
// in-band conflict reply, so a conflict without the option leaves the map alone,
// CommitProxyServer.actor.cpp:2448-2466).

const ckModule = "\xff\xff/transaction/conflicting_keys/"

var ckModuleEnd = ckModule + "\xff\xff"

type ckObs struct {
	Step string
	Rows []string // "key=value", the backend prefix written <P>
	Code int      // FDB error code, 0 on success
}

type ckRun struct {
	t      *testing.T
	db     fdb.BackendDatabase
	prefix string
	obs    []ckObs
}

func (r *ckRun) key(k string) fdb.Key { return fdb.Key(r.prefix + k) }

func (r *ckRun) render(kvs []fdb.KeyValue) []string {
	rows := make([]string, 0, len(kvs))
	for _, kv := range kvs {
		k := strings.Replace(string(kv.Key), ckModule+r.prefix, "<M><P>", 1)
		rows = append(rows, fmt.Sprintf("%q=%q", k, kv.Value))
	}
	return rows
}

func errCode(err error) int {
	if err == nil {
		return 0
	}
	var fe fdb.Error
	if errors.As(err, &fe) {
		return fe.Code
	}
	return -1
}

func (r *ckRun) record(step string, kvs []fdb.KeyValue, err error) {
	r.obs = append(r.obs, ckObs{Step: step, Rows: r.render(kvs), Code: errCode(err)})
}

// ckMap returns the expected map rows for boundaries given as alternating begin,end suffixes.
func ckMap(bounds ...string) []string {
	rows := make([]string, 0, len(bounds))
	for i, b := range bounds {
		v := "1"
		if i%2 == 1 {
			v = "0"
		}
		rows = append(rows, fmt.Sprintf("%q=%q", "<M><P>"+b, v))
	}
	return rows
}

func (r *ckRun) rangeRead(rt fdb.ReadTransaction, rng fdb.Range, opts fdb.RangeOptions) ([]fdb.KeyValue, error) {
	return rt.GetRange(rng, opts).GetSliceWithError()
}

// observe reads the module every way the test compares and returns the whole map.
func (r *ckRun) observe(step string, tr fdb.WritableTransaction) []string {
	full, err := r.rangeRead(tr, fdb.KeyRange{Begin: fdb.Key(ckModule), End: fdb.Key(ckModuleEnd)}, fdb.RangeOptions{})
	r.record(step+"/full", full, err)
	if err != nil {
		r.t.Fatalf("%s: full read: %v", step, err)
	}
	reads := []struct {
		name string
		rt   fdb.ReadTransaction
		rng  fdb.Range
		opts fdb.RangeOptions
	}{
		{"reverse", tr, fdb.KeyRange{Begin: fdb.Key(ckModule), End: fdb.Key(ckModuleEnd)}, fdb.RangeOptions{Reverse: true}},
		{"limit1", tr, fdb.KeyRange{Begin: fdb.Key(ckModule), End: fdb.Key(ckModuleEnd)}, fdb.RangeOptions{Limit: 1}},
		{"limit3_reverse", tr, fdb.KeyRange{Begin: fdb.Key(ckModule), End: fdb.Key(ckModuleEnd)}, fdb.RangeOptions{Limit: 3, Reverse: true}},
		{"exact_mode_limit2", tr, fdb.KeyRange{Begin: fdb.Key(ckModule), End: fdb.Key(ckModuleEnd)}, fdb.RangeOptions{Limit: 2, Mode: fdb.StreamingModeExact}},
		{"snapshot", tr.Snapshot(), fdb.KeyRange{Begin: fdb.Key(ckModule), End: fdb.Key(ckModuleEnd)}, fdb.RangeOptions{}},
		{"from_module_begin", tr, fdb.KeyRange{Begin: fdb.Key("\xff\xff/transaction/"), End: fdb.Key(ckModuleEnd)}, fdb.RangeOptions{}},
		{"sel_first_greater_than", tr, fdb.SelectorRange{
			Begin: fdb.FirstGreaterThan(fdb.Key(ckModule + r.prefix + "m1")),
			End:   fdb.FirstGreaterOrEqual(fdb.Key(ckModuleEnd)),
		}, fdb.RangeOptions{}},
		{"sel_last_less_than_begin", tr, fdb.SelectorRange{
			Begin: fdb.LastLessThan(fdb.Key(ckModule + r.prefix + "n")),
			End:   fdb.FirstGreaterOrEqual(fdb.Key(ckModuleEnd)),
		}, fdb.RangeOptions{}},
		{"sel_last_less_or_equal_end", tr, fdb.SelectorRange{
			Begin: fdb.FirstGreaterOrEqual(fdb.Key(ckModule)),
			End:   fdb.LastLessOrEqual(fdb.Key(ckModule + r.prefix + "p")),
		}, fdb.RangeOptions{}},
		{"sel_end_offset_back", tr, fdb.SelectorRange{
			Begin: fdb.FirstGreaterOrEqual(fdb.Key(ckModule)),
			End:   fdb.KeySelector{Key: fdb.Key(ckModuleEnd), Offset: -1},
		}, fdb.RangeOptions{}},
		{"sel_left_past_module_impl", tr, fdb.SelectorRange{
			Begin: fdb.KeySelector{Key: fdb.Key(ckModule + r.prefix + "zz"), Offset: -20},
			End:   fdb.FirstGreaterOrEqual(fdb.Key(ckModuleEnd)),
		}, fdb.RangeOptions{}},
	}
	for _, rd := range reads {
		kvs, err := r.rangeRead(rd.rt, rd.rng, rd.opts)
		r.record(step+"/"+rd.name, kvs, err)
	}
	if len(full) >= 3 {
		// Forward moves stay inside conflicting_keys/ only while the map has rows to move over.
		kvs, err := r.rangeRead(tr, fdb.SelectorRange{
			Begin: fdb.KeySelector{Key: fdb.Key(ckModule), Offset: 3},
			End:   fdb.FirstGreaterOrEqual(fdb.Key(ckModuleEnd)),
		}, fdb.RangeOptions{})
		r.record(step+"/sel_begin_offset3", kvs, err)
	}
	for i, kv := range full {
		v, err := tr.Get(kv.Key).Get()
		r.record(fmt.Sprintf("%s/get%d", step, i), []fdb.KeyValue{{Key: kv.Key, Value: v}}, err)
		after := make(fdb.Key, len(kv.Key)+1)
		copy(after, kv.Key)
		v, err = tr.Get(after).Get()
		r.record(fmt.Sprintf("%s/get%d_after", step, i), []fdb.KeyValue{{Value: v}}, err)
	}
	return r.render(full)
}

// conflictOn makes tr fail with not_committed: it fixes its read version, runs reads,
// writes a key of its own, and a second transaction commits writes before tr commits.
func (r *ckRun) conflictOn(tr fdb.WritableTransaction, reads func(tr fdb.WritableTransaction), writes ...string) error {
	if _, err := tr.GetReadVersion().Get(); err != nil {
		r.t.Fatalf("GRV: %v", err)
	}
	reads(tr)
	tr.Set(r.key("own"), []byte("x"))
	if _, err := r.db.Transact(func(w fdb.WritableTransaction) (any, error) {
		for _, k := range writes {
			w.Set(r.key(k), []byte("w"))
		}
		return nil, nil
	}); err != nil {
		r.t.Fatalf("writer: %v", err)
	}
	return tr.Commit().Get()
}

func (r *ckRun) newTx() fdb.WritableTransaction {
	tr, err := r.db.CreateWritableTransaction()
	if err != nil {
		r.t.Fatalf("create transaction: %v", err)
	}
	return tr
}

func (r *ckRun) must(step string, err error, want int) {
	r.obs = append(r.obs, ckObs{Step: step, Code: errCode(err)})
	if got := errCode(err); got != want {
		r.t.Fatalf("%s: error %v (code %d), want code %d", step, err, got, want)
	}
}

func (r *ckRun) expect(step string, got, want []string) {
	if !reflect.DeepEqual(got, want) {
		r.t.Errorf("%s: conflicting keys\n got %q\nwant %q", step, got, want)
	}
}

func (r *ckRun) scenario() {
	t := r.t
	if _, err := r.db.Transact(func(w fdb.WritableTransaction) (any, error) {
		for _, k := range []string{"a", "b", "c", "d", "e", "f", "k"} {
			w.Set(r.key(k), []byte("seed"))
		}
		return nil, nil
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	report := func(tr fdb.WritableTransaction) {
		if err := tr.Options().SetReportConflictingKeys(); err != nil {
			t.Fatalf("SetReportConflictingKeys: %v", err)
		}
	}

	// A fresh transaction reads an empty map.
	fresh := r.newTx()
	kvs, err := r.rangeRead(fresh, fdb.KeyRange{Begin: fdb.Key(ckModule), End: fdb.Key(ckModuleEnd)}, fdb.RangeOptions{})
	r.record("fresh/full", kvs, err)
	fresh.Cancel()

	// One key.
	single := r.newTx()
	report(single)
	r.must("single/commit", r.conflictOn(single, func(tr fdb.WritableTransaction) {
		_, _ = tr.Get(r.key("k")).Get()
	}, "k"), 1020)
	r.expect("single", r.observe("single", single), ckMap("k", "k\x00"))
	single.Cancel()

	// A range read.
	rng := r.newTx()
	report(rng)
	r.must("range/commit", r.conflictOn(rng, func(tr fdb.WritableTransaction) {
		_, _ = tr.GetRange(fdb.KeyRange{Begin: r.key("b"), End: r.key("f")}, fdb.RangeOptions{}).GetSliceWithError()
	}, "c"), 1020)
	r.expect("range", r.observe("range", rng), ckMap("b", "f"))
	rng.Cancel()

	// Overlapping, duplicate and disjoint explicit read conflict ranges; RYW merges them
	// before shipping, and only the conflicting ones are reported.
	multi := r.newTx()
	report(multi)
	r.must("multi/commit", r.conflictOn(multi, func(tr fdb.WritableTransaction) {
		for _, kr := range [][2]string{{"m1", "m3"}, {"m2", "m5"}, {"m2", "m5"}, {"p", "q"}, {"x", "y"}} {
			if err := tr.AddReadConflictRange(fdb.KeyRange{Begin: r.key(kr[0]), End: r.key(kr[1])}); err != nil {
				t.Fatalf("AddReadConflictRange: %v", err)
			}
		}
		if err := tr.AddReadConflictKey(r.key("n")); err != nil {
			t.Fatalf("AddReadConflictKey: %v", err)
		}
	}, "m4", "n", "p5"), 1020)
	multiMap := ckMap("m1", "m5", "n", "n\x00", "p", "q")
	r.expect("multi", r.observe("multi", multi), multiMap)

	// The map survives OnError; the option does not, so a retry that conflicts again
	// without it gets not_committed as an error reply and leaves the map as it was.
	r.must("multi/onerror", multi.OnError(fdb.Error{Code: 1020}).Get(), 0)
	r.expect("after_onerror", r.observe("after_onerror", multi), multiMap)
	r.must("retry_no_option/commit", r.conflictOn(multi, func(tr fdb.WritableTransaction) {
		_, _ = tr.Get(r.key("j")).Get()
	}, "j"), 1020)
	r.expect("retry_no_option", r.observe("retry_no_option", multi), multiMap)

	// And Reset.
	multi.Reset()
	r.expect("after_reset", r.observe("after_reset", multi), multiMap)

	// The next conflict reported replaces the map; a successful commit leaves it.
	report(multi)
	r.must("replaced/commit", r.conflictOn(multi, func(tr fdb.WritableTransaction) {
		_, _ = tr.Get(r.key("j2")).Get()
	}, "j2"), 1020)
	r.expect("replaced", r.observe("replaced", multi), ckMap("j2", "j2\x00"))
	r.must("replaced/onerror", multi.OnError(fdb.Error{Code: 1020}).Get(), 0)
	report(multi)
	r.must("success/commit", r.conflictOn(multi, func(tr fdb.WritableTransaction) {
		_, _ = tr.Get(r.key("unwritten")).Get()
	}), 0)
	r.expect("after_success", r.observe("after_success", multi), ckMap("j2", "j2\x00"))
	multi.Cancel()

	// Without RYW the native transaction ships the ranges as added: the proxy reports the
	// duplicate and touching ones by index, and the map coalesces them.
	norw := r.newTx()
	if err := norw.Options().SetReadYourWritesDisable(); err != nil {
		t.Fatalf("SetReadYourWritesDisable: %v", err)
	}
	report(norw)
	r.must("norw/commit", r.conflictOn(norw, func(tr fdb.WritableTransaction) {
		for _, kr := range [][2]string{{"r1", "r3"}, {"r2", "r5"}, {"r2", "r5"}, {"r5", "r6"}, {"r8", "r9"}} {
			if err := tr.AddReadConflictRange(fdb.KeyRange{Begin: r.key(kr[0]), End: r.key(kr[1])}); err != nil {
				t.Fatalf("AddReadConflictRange: %v", err)
			}
		}
	}, "r2", "r55"), 1020)
	r.expect("norw", r.observe("norw", norw), ckMap("r1", "r6"))
	norw.Cancel()

	// Module boundaries.
	errs := r.newTx()
	_, err = r.rangeRead(errs, fdb.KeyRange{Begin: fdb.Key("\xff\xff/transaction/"), End: fdb.Key("\xff\xff/worker_interfaces0")}, fdb.RangeOptions{})
	r.must("cross_module", err, 2112)
	_, err = r.rangeRead(errs, fdb.KeyRange{Begin: fdb.Key("\xff\xff/aaa"), End: fdb.Key("\xff\xff/aab")}, fdb.RangeOptions{})
	r.must("no_module", err, 2113)
	_, err = errs.Get(fdb.Key("\xff\xff/aaa")).Get()
	r.must("get_no_module", err, 2113)
	// Original anchors route before removeOrEqual (RYW:1716-1721); keyAfter preserves
	// allKeysEnd (FDBTypes.h:581-593), while the special-space end remains a module error.
	for _, tc := range []struct {
		name string
		rng  fdb.SelectorRange
		code int
	}{
		{"selector_all_keys_end", fdb.SelectorRange{
			Begin: fdb.FirstGreaterOrEqual(fdb.Key("\xff\xff")),
			End:   fdb.FirstGreaterThan(fdb.Key("\xff\xff")),
		}, 0},
		{"selector_special_space_end", fdb.SelectorRange{
			Begin: fdb.FirstGreaterOrEqual(fdb.Key("\xff\xff/transaction/")),
			End:   fdb.FirstGreaterThan(fdb.Key("\xff\xff\xff")),
		}, 2112},
	} {
		for _, snapshot := range []bool{false, true} {
			for _, reverse := range []bool{false, true} {
				var reader fdb.ReadTransaction = errs
				if snapshot {
					reader = errs.Snapshot()
				}
				step := fmt.Sprintf("%s/snapshot=%v/reverse=%v", tc.name, snapshot, reverse)
				rows, readErr := r.rangeRead(reader, tc.rng, fdb.RangeOptions{Reverse: reverse})
				r.record(step, rows, readErr)
				r.must(step+"/code", readErr, tc.code)
				if len(rows) != 0 {
					t.Fatalf("%s: rows %q, want empty", step, r.render(rows))
				}
			}
		}
	}
	errs.Cancel()
	relaxed := r.newTx()
	if err := relaxed.Options().SetSpecialKeySpaceRelaxed(); err != nil {
		t.Fatalf("SetSpecialKeySpaceRelaxed: %v", err)
	}
	kvs, err = r.rangeRead(relaxed, fdb.KeyRange{Begin: fdb.Key("\xff\xff/aaa"), End: fdb.Key("\xff\xff/aab")}, fdb.RangeOptions{})
	r.record("relaxed/no_module", kvs, err)
	kvs, err = r.rangeRead(relaxed, fdb.KeyRange{Begin: fdb.Key(ckModule), End: fdb.Key(ckModuleEnd)}, fdb.RangeOptions{})
	r.record("relaxed/full", kvs, err)
	relaxed.Cancel()
}

// dbDefaultScenario: with TRANSACTION_REPORT_CONFLICTING_KEYS every attempt reports, so
// each conflict replaces the map, across OnError and Reset.
func (r *ckRun) dbDefaultScenario() {
	tr := r.newTx()
	r.must("dbdefault/d1", r.conflictOn(tr, func(tr fdb.WritableTransaction) { _, _ = tr.Get(r.key("d1")).Get() }, "d1"), 1020)
	r.expect("dbdefault/d1", r.observe("dbdefault_d1", tr), ckMap("d1", "d1\x00"))
	r.must("dbdefault/onerror", tr.OnError(fdb.Error{Code: 1020}).Get(), 0)
	r.must("dbdefault/d2", r.conflictOn(tr, func(tr fdb.WritableTransaction) { _, _ = tr.Get(r.key("d2")).Get() }, "d2"), 1020)
	r.expect("dbdefault/d2", r.observe("dbdefault_d2", tr), ckMap("d2", "d2\x00"))
	tr.Reset()
	r.must("dbdefault/d3", r.conflictOn(tr, func(tr fdb.WritableTransaction) { _, _ = tr.Get(r.key("d3")).Get() }, "d3"), 1020)
	r.expect("dbdefault/d3", r.observe("dbdefault_d3", tr), ckMap("d3", "d3\x00"))
	tr.Cancel()
}

func compareTranscripts(t *testing.T, c, g []ckObs) {
	t.Helper()
	if len(c) == 0 {
		t.Fatal("libfdb_c transcript is empty")
	}
	n := max(len(c), len(g))
	for i := 0; i < n; i++ {
		switch {
		case i >= len(c):
			t.Errorf("pure-Go extra step %+v", g[i])
		case i >= len(g):
			t.Errorf("pure-Go missing step %+v", c[i])
		case !reflect.DeepEqual(c[i], g[i]):
			t.Errorf("step %d differs:\n libfdb_c %+v\n pure-Go  %+v", i, c[i], g[i])
		}
	}
}

func TestConflictingKeys_Conformance(t *testing.T) {
	t.Parallel()
	clusterFile := ckStartCluster(t)
	cdb, err := libfdbc.Open(clusterFile)
	if err != nil {
		t.Fatalf("open libfdb_c backend: %v", err)
	}
	defer cdb.Close()
	gdb, err := fdb.OpenDatabase(clusterFile)
	if err != nil {
		t.Fatalf("open pure-Go database: %v", err)
	}
	defer gdb.Close()

	c := &ckRun{t: t, db: cdb, prefix: "ckconf/c/"}
	g := &ckRun{t: t, db: gdb, prefix: "ckconf/g/"}
	c.scenario()
	g.scenario()
	compareTranscripts(t, c.obs, g.obs)

	// The database default, on handles of their own: a second cluster-file path is a
	// distinct database in both bindings, and Apple's OpenDatabase caches by path
	// (bindings/go/src/fdb/fdb.go:336-348), so libfdbc.Open shares the handle the option
	// was set on.
	defaultFile := t.TempDir() + "/fdb_default.cluster"
	content, err := os.ReadFile(clusterFile)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(defaultFile, content, 0o600); err != nil {
		t.Fatal(err)
	}
	raw, err := cgofdb.OpenDatabase(defaultFile)
	if err != nil {
		t.Fatalf("open libfdb_c database: %v", err)
	}
	if err := raw.Options().SetTransactionReportConflictingKeys(); err != nil {
		t.Fatalf("libfdb_c SetTransactionReportConflictingKeys: %v", err)
	}
	cdef, err := libfdbc.Open(defaultFile)
	if err != nil {
		t.Fatalf("open libfdb_c backend: %v", err)
	}
	defer cdef.Close()
	gdef, err := fdb.OpenDatabase(defaultFile)
	if err != nil {
		t.Fatalf("open pure-Go database: %v", err)
	}
	defer gdef.Close()
	if err := gdef.Options().SetTransactionReportConflictingKeys(); err != nil {
		t.Fatalf("pure-Go SetTransactionReportConflictingKeys: %v", err)
	}
	cd := &ckRun{t: t, db: cdef, prefix: "ckconf/cd/"}
	gd := &ckRun{t: t, db: gdef, prefix: "ckconf/gd/"}
	cd.dbDefaultScenario()
	gd.dbDefaultScenario()
	compareTranscripts(t, cd.obs, gd.obs)
}

func ckStartCluster(t *testing.T) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	container, err := foundationdbtc.Run(ctx, "", foundationdbtc.WithDirectIP())
	if err != nil {
		if foundationdbtc.SkipWithoutDocker(err) {
			t.Skipf("FDB not available (no Docker): %v", err)
		}
		t.Fatalf("FDB container startup failed: %v", err)
	}
	t.Cleanup(func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), foundationdbtc.TerminateTimeout)
		defer stopCancel()
		foundationdbtc.TerminateT(t, stopCtx, container)
	})
	content, err := container.ClusterFile(ctx)
	if err != nil {
		t.Fatalf("cluster file: %v", err)
	}
	path := t.TempDir() + "/fdb.cluster"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write cluster file: %v", err)
	}
	return path
}
