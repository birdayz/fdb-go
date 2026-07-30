package atomicops

import (
	"bytes"
	"testing"

	fdb "fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/simfdb"
	"fdb.dev/pkg/simfdb/hunt"
)

// TestAtomicOps_Determinism pins byte-identical replay: the same seed must produce the same final
// keyspace fingerprint on two independent runs.
func TestAtomicOps_Determinism(t *testing.T) {
	t.Parallel()
	w := Workload{Keys: 6, Txns: 12, OpsPerTxn: 8, RYWChecks: true}
	for seed := uint64(0); seed < 30; seed++ {
		a := w.run(seed)
		b := w.run(seed)
		if a.Report.Failed() {
			t.Fatalf("seed %d unexpectedly failed: %s", seed, a.Report)
		}
		if a.Report.Fingerprint == "" || a.Report.Fingerprint != b.Report.Fingerprint {
			t.Fatalf("seed %d not deterministic: %q vs %q", seed, a.Report.Fingerprint, b.Report.Fingerprint)
		}
	}
}

// TestAtomicOps_AllOpsFire is the NO-FAKE-CHECKBOX proof: over a batch of seeds every one of the
// ten atomic ops (plus Set and Clear) is actually executed, so the differential covers the whole
// mutation surface rather than just the easy cases.
func TestAtomicOps_AllOpsFire(t *testing.T) {
	t.Parallel()
	total := map[opcode]int{}
	w := Workload{Keys: 6, Txns: 12, OpsPerTxn: 8, RYWChecks: true}
	for seed := uint64(0); seed < 200; seed++ {
		res := w.run(seed)
		if res.Report.Failed() {
			t.Fatalf("seed %d failed: %s", seed, res.Report)
		}
		for op, n := range res.Counts {
			total[op] += n
		}
	}
	want := append([]opcode{opSet, opClear}, atomicOps...)
	for _, op := range want {
		if total[op] == 0 {
			t.Errorf("op %s never fired across 200 seeds — coverage gap", op)
		}
	}
}

// TestAtomicOps_CleanSweep runs every profile across a band of seeds and asserts the two oracles
// hold everywhere. A failure here is either a real SimFDB atomic-op divergence or a model bug — in
// both cases the DFS rule is: root-cause and fix, never quarantine.
func TestAtomicOps_CleanSweep(t *testing.T) {
	t.Parallel()
	for _, p := range Profiles() {
		p := p
		t.Run(p.Name, func(t *testing.T) {
			t.Parallel()
			w := p.Cfg.Workload.(Workload)
			for seed := uint64(0); seed < 500; seed++ {
				res := w.run(seed)
				if res.Report.Failed() {
					t.Fatalf("profile %s seed %d: %s", p.Name, seed, res.Report)
				}
			}
		})
	}
}

// TestAtomicOps_EdgeSweep hammers the nil-vs-empty branches specifically.
func TestAtomicOps_EdgeSweep(t *testing.T) {
	t.Parallel()
	w := Workload{Keys: 3, Txns: 16, OpsPerTxn: 10, RYWChecks: true, EdgeBias: true}
	for seed := uint64(0); seed < 2000; seed++ {
		res := w.run(seed)
		if res.Report.Failed() {
			t.Fatalf("edge seed %d: %s", seed, res.Report)
		}
	}
}

// atomicCase is one load-bearing scenario: seed a key into a known state (absent / present-empty /
// present), apply one atomic op, and assert SimFDB reads back exactly the value the independent
// model predicts. These pin the absent-vs-present-empty distinction end-to-end.
type atomicCase struct {
	name    string
	seedVal []byte // nil => leave the key absent; non-nil (incl. empty) => Set it first
	apply   func(tx fdb.WritableTransaction, k fdb.Key)
	want    []byte // expected read-back; nil => key must be absent
	wantAbs bool   // true => the key must be absent after the op
}

// TestAtomicOps_PresentEmptyVsAbsent is the focused regression the sweep generalises: it drives the
// exact cases where AndV2 / MinV2 / ByteMin / CompareAndClear branch on absent vs present-empty,
// straight through the SimFDB backend, and checks the read-back. If SimFDB dropped a present-empty
// value on commit or on GetRange, the present-empty rows here would flip to their absent answers.
func TestAtomicOps_PresentEmptyVsAbsent(t *testing.T) {
	t.Parallel()
	empty := []byte{}
	five := []byte{0x05}
	ff := []byte{0xff}

	cases := []atomicCase{
		// Min: absent folds to param (MinV2); present-empty runs V1 -> all-zeros at param width.
		{"min/absent->param", nil, func(tx fdb.WritableTransaction, k fdb.Key) { tx.Min(k, five) }, five, false},
		{"min/present-empty->zeros", empty, func(tx fdb.WritableTransaction, k fdb.Key) { tx.Min(k, five) }, []byte{0x00}, false},
		// And: absent folds to param (AndV2); present-empty -> all-zeros at param width.
		{"and/absent->param", nil, func(tx fdb.WritableTransaction, k fdb.Key) { tx.And(k, ff) }, ff, false},
		{"and/present-empty->zeros", empty, func(tx fdb.WritableTransaction, k fdb.Key) { tx.And(k, ff) }, []byte{0x00}, false},
		// ByteMin: absent folds to param; present-empty is the smallest string and wins (-> empty).
		{"bytemin/absent->param", nil, func(tx fdb.WritableTransaction, k fdb.Key) { tx.ByteMin(k, five) }, five, false},
		{"bytemin/present-empty->empty", empty, func(tx fdb.WritableTransaction, k fdb.Key) { tx.ByteMin(k, five) }, empty, false},
		// CompareAndClear: absent always clears (stays absent); present-empty with empty param clears.
		{"cac/absent->absent", nil, func(tx fdb.WritableTransaction, k fdb.Key) { tx.CompareAndClear(k, five) }, nil, true},
		{"cac/present-empty-match->cleared", empty, func(tx fdb.WritableTransaction, k fdb.Key) { tx.CompareAndClear(k, empty) }, nil, true},
		{"cac/present-empty-nomatch->kept", empty, func(tx fdb.WritableTransaction, k fdb.Key) { tx.CompareAndClear(k, five) }, empty, false},
		// Or/Xor don't distinguish absent from present-empty — both fold to param — but pin them too.
		{"or/present-empty->param", empty, func(tx fdb.WritableTransaction, k fdb.Key) { tx.Or(k, five) }, five, false},
		// Add: absent and present-empty both fold to param.
		{"add/present-empty->param", empty, func(tx fdb.WritableTransaction, k fdb.Key) { tx.Add(k, five) }, five, false},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env := hunt.NewSimEnv(1, 0)
			db := simfdb.New(env)
			defer db.Close()
			sub := subspace.FromBytes(tuple.Tuple{"pe"}.Pack())
			k := sub.Pack(tuple.Tuple{int64(0)})

			// Phase 1: establish the seed state (absent, or Set to seedVal) in its own transaction.
			if tc.seedVal != nil {
				commit(t, db, func(tx fdb.WritableTransaction) { tx.Set(fdb.Key(k), tc.seedVal) })
				// A present-empty seed must survive the round-trip before we even test the op.
				if len(tc.seedVal) == 0 {
					if v, present := readKey(db, sub, k); !present {
						t.Fatalf("present-empty seed did NOT survive commit+GetRange (buildView dropped it) "+
							"— key %s read back ABSENT", tc.name)
					} else if len(v) != 0 {
						t.Fatalf("present-empty seed came back non-empty: %x", v)
					}
				}
			}

			// Phase 2: apply the atomic op.
			commit(t, db, func(tx fdb.WritableTransaction) { tc.apply(tx, fdb.Key(k)) })

			// Phase 3: read back and compare to the model's prediction.
			gotVal, present := readKey(db, sub, k)
			if tc.wantAbs {
				if present {
					t.Fatalf("%s: expected key ABSENT, got present = %s", tc.name, hexOrAbsent(gotVal))
				}
				return
			}
			if !present {
				t.Fatalf("%s: expected present = %s, got ABSENT", tc.name, hexOrAbsent(tc.want))
			}
			if !bytes.Equal(gotVal, tc.want) {
				t.Fatalf("%s: SimFDB read back %s, want %s", tc.name, hexOrAbsent(gotVal), hexOrAbsent(tc.want))
			}
		})
	}
}

func commit(t *testing.T, db *simfdb.SimDB, fn func(tx fdb.WritableTransaction)) {
	t.Helper()
	tx, err := db.CreateWritableTransaction()
	if err != nil {
		t.Fatalf("create txn: %v", err)
	}
	fn(tx)
	if err := tx.Commit().Get(); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

// readKey returns (value, present). A present-empty value is (zero-length slice, true); an absent
// key is (nil, false). It uses a full-range read so presence is observable (a point Get can't
// always distinguish present-empty from absent through the byte-slice future).
func readKey(db *simfdb.SimDB, sub subspace.Subspace, key []byte) ([]byte, bool) {
	all := readAll(db, sub)
	v, ok := all[string(key)]
	return v, ok
}

// FuzzAtomicOps is the coverage-guided complement: any seed the fuzzer discovers must satisfy both
// oracles. A crash or a Failed report is a reproducible finding.
func FuzzAtomicOps(f *testing.F) {
	for _, s := range []uint64{0, 1, 2, 7, 42, 99, 1000} {
		f.Add(s)
	}
	w := Workload{Keys: 4, Txns: 10, OpsPerTxn: 8, RYWChecks: true}
	f.Fuzz(func(t *testing.T, seed uint64) {
		res := w.run(seed)
		if res.Report.Failed() {
			t.Fatalf("fuzz seed %d: %s", seed, res.Report)
		}
		// Determinism must also hold for fuzzer-found seeds.
		if res.Report.Fingerprint != w.run(seed).Report.Fingerprint {
			t.Fatalf("fuzz seed %d not deterministic", seed)
		}
	})
}
