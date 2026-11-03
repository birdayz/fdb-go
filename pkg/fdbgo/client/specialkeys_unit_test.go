package client

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"

	"fdb.dev/pkg/fdbgo/transport"
	"fdb.dev/pkg/fdbgo/wire"
	"fdb.dev/pkg/fdbgo/wire/types"
)

// skTx is a transaction at API version 730, routed to the special key space, whose
// conflicting keys map holds boundaries, given as alternating begin,end key suffixes.
func skTx(bounds ...string) *Transaction {
	tx := &Transaction{db: &database{apiVersion: 730}, txOptions: txOptions{tenantId: NoTenantID}}
	tx.state.Store(int32(txStateActive))
	if len(bounds) > 0 {
		m := &conflictingKeyMap{}
		for i, b := range bounds {
			v := conflictingKeysTrue
			if i%2 == 1 {
				v = conflictingKeysFalse
			}
			m.entries = append(m.entries, KeyValue{Key: []byte(string(conflictingKeysBegin) + b), Value: v})
		}
		tx.conflictingKeys.Store(m)
	}
	return tx
}

// skRows renders rows as suffix=value, the module prefix stripped.
func skRows(kvs []KeyValue) []string {
	out := []string{}
	for _, kv := range kvs {
		k := string(kv.Key)
		if len(k) >= len(conflictingKeysBegin) && k[:len(conflictingKeysBegin)] == string(conflictingKeysBegin) {
			k = k[len(conflictingKeysBegin):]
		}
		out = append(out, k+"="+string(kv.Value))
	}
	return out
}

// skTxWithDB is an active transaction on a database with no option defaults.
func skTxWithDB() *Transaction {
	tx := &Transaction{db: &database{}, readVersionState: &readVersionState{}, txOptions: txOptions{tenantId: NoTenantID}}
	tx.state.Store(int32(txStateActive))
	return tx
}

func skCode(err error) int {
	var fe *wire.FDBError
	if errors.As(err, &fe) {
		return fe.Code
	}
	if err != nil {
		return -1
	}
	return 0
}

func skKey(s string) []byte { return []byte(string(conflictingKeysBegin) + s) }

// newConflictingKeyMap is NativeAPI.actor.cpp:6700-6717 over CoalescedKeyRangeMap: distinct
// indices, ranges prefixed, overlapping and touching ones merged, empty ones and indices the
// request did not ship ignored.
func TestConflictingKeyMap_Build(t *testing.T) {
	t.Parallel()
	reads := []KeyRange{
		{Begin: []byte("r5"), End: []byte("r6")}, // 0, touches 2
		{Begin: []byte("x"), End: []byte("y")},   // 1, not reported
		{Begin: []byte("r1"), End: []byte("r3")}, // 2
		{Begin: []byte("r2"), End: []byte("r5")}, // 3, overlaps 2
		{Begin: []byte("k"), End: []byte("k\x00")},
		{Begin: []byte("e"), End: []byte("e")}, // 5, empty
		{Begin: []byte("a"), End: []byte("b")},
	}
	m := newConflictingKeyMap([]int32{3, 0, 2, 3, 4, 4, 5, 6, 99, -1}, reads)
	got := skRows(m.entries)
	want := []string{"a=1", "b=0", "k=1", "k\x00=0", "r1=1", "r6=0"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("map %q, want %q", got, want)
	}
	// Nested ranges.
	m = newConflictingKeyMap([]int32{0, 1}, []KeyRange{
		{Begin: []byte("a"), End: []byte("z")}, {Begin: []byte("c"), End: []byte("d")},
	})
	if got := skRows(m.entries); !reflect.DeepEqual(got, []string{"a=1", "z=0"}) {
		t.Fatalf("nested: %q", got)
	}
	// A range up to \xff\xff ends at conflictingKeysRange.end, outside every read.
	m = newConflictingKeyMap([]int32{0}, []KeyRange{{Begin: []byte("\xff/x"), End: []byte("\xff\xff")}})
	if got := skRows(readConflictingKeys(&Transaction{}, conflictingKeysBegin, conflictingKeysEnd)); len(got) != 0 {
		t.Fatalf("nil map read: %q", got)
	}
	tx := skTx()
	tx.conflictingKeys.Store(m)
	if got := skRows(readConflictingKeys(tx, conflictingKeysBegin, conflictingKeysEnd)); !reflect.DeepEqual(got, []string{"\xff/x=1"}) {
		t.Fatalf("map ending at the module end: %q", got)
	}
}

// The option rides on the commit request (NativeAPI.actor.cpp:6887-6889) and only there.
func TestReportConflictingKeys_CommitRequest(t *testing.T) {
	t.Parallel()
	for _, set := range []bool{false, true} {
		tx := skTxWithDB()
		if set {
			tx.SetReportConflictingKeys()
		}
		input := tx.captureCommit(nil, nil)
		if input.reportConflictingKeys != set {
			t.Fatalf("captured reportConflictingKeys %v, want %v", input.reportConflictingKeys, set)
		}
		input.readVersion = 5
		body, buf := buildCommitInputRequest(input, transport.UID{})
		r, err := wire.NewReader(body)
		if err != nil {
			t.Fatal(err)
		}
		var req types.CommitTransactionRequest
		if err := req.UnmarshalFromReader(r); err != nil {
			t.Fatal(err)
		}
		marshalBufPool.Put(buf)
		input.release()
		if req.Transaction.Report_conflicting_keys != set {
			t.Fatalf("request report_conflicting_keys %v, want %v", req.Transaction.Report_conflicting_keys, set)
		}
	}
}

// REPORT_CONFLICTING_KEYS and SPECIAL_KEY_SPACE_RELAXED are not persistent: both reset paths
// restore the database default (TransactionOptions::clear, NativeAPI.actor.cpp:6143;
// ReadYourWrites.actor.cpp:2079, 2722, 2748). The conflicting keys map survives both
// (cloneAndReset, NativeAPI.actor.cpp:3531), and a successful commit's turnover.
func TestReportConflictingKeys_OptionLifecycle(t *testing.T) {
	t.Parallel()
	for _, userReset := range []bool{false, true} {
		tx := skTxWithDB()
		tx.SetReportConflictingKeys()
		tx.SetSpecialKeySpaceRelaxed()
		m := &conflictingKeyMap{}
		tx.conflictingKeys.Store(m)
		tx.reset(userReset)
		if tx.reportConflictingKeys || tx.specialKeySpaceRelaxed {
			t.Fatalf("reset(%v) kept report=%v relaxed=%v", userReset, tx.reportConflictingKeys, tx.specialKeySpaceRelaxed)
		}
		if tx.conflictingKeys.Load() != m {
			t.Fatalf("reset(%v) dropped the conflicting keys", userReset)
		}
		tx.postCommitReset()
		if tx.conflictingKeys.Load() != m {
			t.Fatal("post-commit turnover dropped the conflicting keys")
		}
	}

	// The database default applies to new transactions and is re-applied on both resets.
	d := &Database{db: &database{}}
	d.SetTransactionReportConflictingKeys()
	tx := d.CreateTransaction()
	if !tx.reportConflictingKeys {
		t.Fatal("TRANSACTION_REPORT_CONFLICTING_KEYS not applied to a new transaction")
	}
	for _, userReset := range []bool{false, true} {
		tx.reportConflictingKeys = false
		tx.reset(userReset)
		if !tx.reportConflictingKeys {
			t.Fatalf("reset(%v) did not re-apply the database default", userReset)
		}
	}
}

// SpecialKeySpace::getRange over the conflicting keys module (SpecialKeySpace.actor.cpp:120-493,
// 711-729): selectors normalize over the map, limits and direction apply, and a get is a
// one-key range.
func TestSpecialKeys_ConflictingKeysReads(t *testing.T) {
	t.Parallel()
	bounds := []string{"a", "b", "k", "k\x00", "m1", "m5"}
	all := []string{"a=1", "b=0", "k=1", "k\x00=0", "m1=1", "m5=0"}
	sel := func(s string, orEqual bool, off int32) KeySelector {
		return KeySelector{Key: skKey(s), OrEqual: orEqual, Offset: off}
	}
	modBegin := KeySelector{Key: conflictingKeysBegin, Offset: 1}
	modEnd := KeySelector{Key: conflictingKeysEnd, Offset: 1}
	txBegin := KeySelector{Key: []byte("\xff\xff/transaction/"), Offset: 1}
	for _, tc := range []struct {
		name       string
		begin, end KeySelector
		limit      int
		reverse    bool
		want       []string
		more       bool
	}{
		{"whole", modBegin, modEnd, 0, false, all, false},
		{"reverse", modBegin, modEnd, 0, true, []string{"m5=0", "m1=1", "k\x00=0", "k=1", "b=0", "a=1"}, false},
		{"limit2", modBegin, modEnd, 2, false, all[:2], true},
		{"limit2 reverse", modBegin, modEnd, 2, true, []string{"m5=0", "m1=1"}, true},
		{"limit exactly all", modBegin, modEnd, 6, false, all, true},
		{"from module begin", txBegin, modEnd, 0, false, all, false},
		{"first greater than", sel("b", true, 1), modEnd, 0, false, all[2:], false},
		{"inside a range", sel("a0", false, 1), sel("m", false, 1), 0, false, all[1:4], false},
		{"begin offset 3", KeySelector{Key: conflictingKeysBegin, Offset: 3}, modEnd, 0, false, all[2:], false},
		{"begin last less than", sel("k", false, 0), modEnd, 0, false, all[1:], false},
		{"begin last less or equal", sel("k", true, 0), modEnd, 0, false, all[2:], false},
		{"begin far left clamps", sel("zz", false, -20), modEnd, 0, false, all, false},
		{"end offset back", modBegin, KeySelector{Key: conflictingKeysEnd, Offset: -1}, 0, false, all[:4], false},
		{"end last less or equal", modBegin, sel("k", true, 0), 0, false, all[:2], false},
		{"inverted", sel("m1", false, 1), sel("b", false, 1), 0, false, nil, false},
		{"inverted by offsets", sel("b", false, 2), sel("b", false, 1), 0, false, nil, false},
	} {
		tx := skTx(bounds...)
		kvs, more, err := tx.specialKeyRange(tc.begin, tc.end, tc.limit, 0, tc.reverse)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if got := skRows(kvs); !reflect.DeepEqual(got, append([]string{}, tc.want...)) || more != tc.more {
			t.Errorf("%s: rows %q more %v, want %q more %v", tc.name, got, more, tc.want, tc.more)
		}
	}

	tx := skTx(bounds...)
	for key, want := range map[string]string{"a": "1", "b": "0", "k\x00": "0", "a0": "", "": ""} {
		v, err := tx.getSpecialKey(skKey(key))
		if err != nil || string(v) != want || (want == "") != (v == nil) {
			t.Errorf("get %q = %q, %v; want %q", key, v, err, want)
		}
	}
	// The byte limit counts 8 plus key and value per row (NativeAPI.actor.cpp:2823-2829) and
	// ends the read once spent, minRows being met.
	row := 8 + len(skKey("a")) + 1
	kvs, more, err := tx.specialKeyRange(modBegin, modEnd, 0, row, false)
	if err != nil || len(kvs) != 1 || !more {
		t.Errorf("byte limit of one row: %d rows more %v err %v", len(kvs), more, err)
	}
	if _, _, err := tx.specialKeyRange(modBegin, modEnd, -2, 0, false); skCode(err) != ErrRangeLimitsInvalid {
		t.Errorf("limit -2: %v, want 2012", err)
	}
	// An empty map, or none, reads nothing.
	if kvs, _, err := skTx().specialKeyRange(modBegin, modEnd, 0, 0, false); err != nil || len(kvs) != 0 {
		t.Errorf("no map: %q %v", skRows(kvs), err)
	}
}

// moduleToBoundary (SpecialKeySpace.actor.cpp:61-80, 331-352): a read must stay in one known
// module unless SPECIAL_KEY_SPACE_RELAXED; anything that reaches an impl other than
// conflicting_keys/ fails with unsupported_operation instead of answering.
func TestSpecialKeys_ModulesAndUnservedImpls(t *testing.T) {
	t.Parallel()
	ks := func(s string) KeySelector { return KeySelector{Key: []byte(s), Offset: 1} }
	for _, tc := range []struct {
		name       string
		relaxed    bool
		tenant     bool
		begin, end KeySelector
		code       int
		rows       int
	}{
		{"cross module", false, false, ks("\xff\xff/transaction/"), ks("\xff\xff/worker_interfaces0"), ErrSpecialKeysCrossModuleRead, 0},
		{"no module", false, false, ks("\xff\xff/aaa"), ks("\xff\xff/aab"), ErrSpecialKeysNoModuleFound, 0},
		{"no module at space begin", false, false, ks("\xff\xff"), ks("\xff\xff/a"), ErrSpecialKeysNoModuleFound, 0},
		{"read_conflict_range", false, false, ks("\xff\xff/transaction/read_conflict_range/"), ks("\xff\xff/transaction/read_conflict_range/\xff\xff"), ErrUnsupportedOperation, 0},
		{"write_conflict_range", false, false, ks("\xff\xff/transaction/write_conflict_range/"), ks("\xff\xff/transaction/write_conflict_range/\xff\xff"), ErrUnsupportedOperation, 0},
		{"whole transaction module", false, false, ks("\xff\xff/transaction/"), ks("\xff\xff/transaction0"), ErrUnsupportedOperation, 0},
		{"status json", false, false, ks("\xff\xff/status/json"), ks("\xff\xff/status/json\x00"), ErrUnsupportedOperation, 0},
		{"forward past the map", false, false, KeySelector{Key: conflictingKeysBegin, Offset: 9}, ks(string(conflictingKeysEnd)), ErrUnsupportedOperation, 0},
		{"relaxed gap", true, false, ks("\xff\xff/aaa"), ks("\xff\xff/aab"), 0, 0},
		{"relaxed map", true, false, ks(string(conflictingKeysBegin)), ks(string(conflictingKeysEnd)), 0, 2},
		{"relaxed across modules", true, false, ks("\xff\xff/transaction/"), ks("\xff\xff/worker_interfaces0"), ErrUnsupportedOperation, 0},
		{"relaxed whole space", true, false, ks("\xff\xff"), ks("\xff\xff\xff"), ErrUnsupportedOperation, 0},
		{"tenant map", false, true, ks(string(conflictingKeysBegin)), ks(string(conflictingKeysEnd)), 0, 2},
		// LastLessOrEqual(\xff\xff\xff) ends at keyAfter(\xff\xff\xff), past the space.
		{"end past the space", false, false, ks("\xff\xff/transaction/"), KeySelector{Key: specialKeysEnd, OrEqual: true}, ErrSpecialKeysCrossModuleRead, 0},
		{"relaxed end past the space", true, false, ks("\xff\xff/transaction/"), KeySelector{Key: specialKeysEnd, OrEqual: true}, ErrUnsupportedOperation, 0},
	} {
		tx := skTx("a", "b")
		tx.specialKeySpaceRelaxed = tc.relaxed
		if tc.tenant {
			tx.tenantId = 5
		}
		kvs, _, err := tx.specialKeyRange(tc.begin, tc.end, 0, 0, false)
		if skCode(err) != tc.code || len(kvs) != tc.rows {
			t.Errorf("%s: %d rows, err %v; want %d rows, code %d", tc.name, len(kvs), err, tc.rows, tc.code)
		}
	}
	if _, err := skTx().getSpecialKey([]byte("\xff\xff/aaa")); skCode(err) != ErrSpecialKeysNoModuleFound {
		t.Errorf("get outside every module: %v, want 2113", err)
	}
	if _, err := skTx().getSpecialKey([]byte("\xff\xff/status/json")); skCode(err) != ErrUnsupportedOperation {
		t.Errorf("get status/json: %v, want 2108", err)
	}
	// keyAfter(\xff\xff) is \xff\xff (FDBTypes.h:581-586): the get is an empty range.
	if v, err := skTx().getSpecialKey([]byte("\xff\xff")); v != nil || err != nil {
		t.Errorf("get \\xff\\xff = %q, %v", v, err)
	}
}

// The routes of RYW get/getRange (ReadYourWrites.actor.cpp:1634-1638, 1716-1721) run before
// the legal-range, limit and state checks, do no I/O, add no read conflict and are not reads
// for the commit barrier; only the deferred error comes first (ThreadSafeTransaction.cpp:431,
// 476). Below API version 630 nothing is routed.
func TestSpecialKeys_Routing(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	tx := skTx("a", "b")
	if v, err := tx.Get(ctx, skKey("a")); err != nil || string(v) != "1" {
		t.Fatalf("Get: %q %v", v, err)
	}
	if v, err := tx.Snapshot().Get(ctx, skKey("b")); err != nil || string(v) != "0" {
		t.Fatalf("Snapshot.Get: %q %v", v, err)
	}
	if v, p, err := tx.GetPipelined(ctx, skKey("a")); err != nil || p != nil || string(v) != "1" {
		t.Fatalf("GetPipelined: %q %v %v", v, p, err)
	}
	for name, read := range map[string]func() ([]KeyValue, bool, error){
		"GetRange": func() ([]KeyValue, bool, error) {
			return tx.GetRange(ctx, conflictingKeysBegin, conflictingKeysEnd, 0)
		},
		"Snapshot.GetRangeReverse": func() ([]KeyValue, bool, error) {
			return tx.Snapshot().GetRangeReverse(ctx, conflictingKeysBegin, conflictingKeysEnd, 0)
		},
		"StartGetRange": func() ([]KeyValue, bool, error) {
			return tx.StartGetRange(ctx, conflictingKeysBegin, conflictingKeysEnd, 0, 0, false).Get()
		},
		"GetRangeSelectors": func() ([]KeyValue, bool, error) {
			return tx.GetRangeSelectors(ctx, KeySelector{Key: skKey("a"), OrEqual: true, Offset: 0},
				KeySelector{Key: conflictingKeysEnd, Offset: 1}, 0, 0, false)
		},
		"StartGetRangeSelectors": func() ([]KeyValue, bool, error) {
			return tx.Snapshot().StartGetRangeSelectors(ctx, KeySelector{Key: conflictingKeysBegin, Offset: 1},
				KeySelector{Key: conflictingKeysEnd, Offset: 1}, 0, 0, false).Get()
		},
	} {
		kvs, _, err := read()
		if err != nil || len(kvs) != 2 {
			t.Errorf("%s: %q %v", name, skRows(kvs), err)
		}
	}
	if err := tx.checkRangeArgs(ctx, conflictingKeysBegin, conflictingKeysEnd, -5); err != nil {
		t.Errorf("checkRangeArgs on a routed range: %v (the routed read checks its own limits)", err)
	}
	if reads := tx.readConflictRanges(); tx.hadRead.Load() || len(reads) != 0 || tx.readLife != nil {
		t.Errorf("routed reads touched read state: hadRead=%v conflicts=%d readLife=%v",
			tx.hadRead.Load(), len(reads), tx.readLife)
	}

	tx.deferredErr.Store(&wire.FDBError{Code: 2006})
	if _, err := tx.Get(ctx, skKey("a")); skCode(err) != 2006 {
		t.Errorf("Get after a deferred error: %v", err)
	}
	if _, _, err := tx.GetRange(ctx, conflictingKeysBegin, conflictingKeysEnd, 0); skCode(err) != 2006 {
		t.Errorf("GetRange after a deferred error: %v", err)
	}
	if err := tx.checkRangeArgs(ctx, conflictingKeysBegin, conflictingKeysEnd, 0); skCode(err) != 2006 {
		t.Errorf("checkRangeArgs after a deferred error: %v", err)
	}

	for _, tc := range []struct {
		api                int
		b, e               string
		wantKey, wantRange bool
	}{
		{730, "\xff\xff", "\xff\xff\xff", true, true},
		{630, "\xff\xff/a", "\xff\xff/b", true, true},
		{620, "\xff\xff/a", "\xff\xff/b", false, false},
		{730, "\xff\xfe", "\xff\xff/b", false, false},
		{730, "\xff\xff/a", "\xff\xff\xff\x00", true, false},
		{730, "\xff\xff\xff", "\xff\xff\xff", false, false},
	} {
		tx := &Transaction{db: &database{apiVersion: tc.api}}
		if got := tx.routesSpecialKeyRange([]byte(tc.b), []byte(tc.e)); got != tc.wantRange {
			t.Errorf("routesSpecialKeyRange(api %d, %q, %q) = %v", tc.api, tc.b, tc.e, got)
		}
		if got := tx.routesSpecialKey([]byte(tc.b)); got != tc.wantKey {
			t.Errorf("routesSpecialKey(api %d, %q) = %v", tc.api, tc.b, got)
		}
	}
}

// The module and impl tables are the RangeMap C++ builds: boundaries at the space, at each
// module and at each registered impl, impls only where registered.
func TestSpecialKeys_Tables(t *testing.T) {
	t.Parallel()
	var got []string
	for _, e := range skReadImpls.entries {
		name := "-"
		if e.value != nil {
			name = e.value.name
		}
		got = append(got, fmt.Sprintf("%q:%s", e.key, name))
	}
	// "", the space's begin and end, each module's begin and end, each TRANSACTION impl's.
	if len(skReadImpls.entries) != 3+2*len(skModuleBoundaries)+6 {
		t.Errorf("readImpls has %d entries: %v", len(skReadImpls.entries), got)
	}
	i := skReadImpls.rangeContaining(skKey("x"))
	if v := skReadImpls.entries[i].value; v == nil || v.name != "conflicting_keys" {
		t.Errorf("conflicting keys impl not found: %v", got)
	}
	i = skReadImpls.rangeContaining([]byte("\xff\xff/transaction/a"))
	if skReadImpls.entries[i].value != nil {
		t.Errorf("\\xff\\xff/transaction/a has an impl: %v", got)
	}
	i = skModules.rangeContaining([]byte("\xff\xff/transaction/x"))
	if skModules.entries[i].value != skModuleTransaction {
		t.Errorf("transaction module not found")
	}
	if i := skReadImpls.rangeContainingKeyBefore([]byte{}); i != 0 {
		t.Errorf("rangeContainingKeyBefore(\"\") = %d", i)
	}
}
