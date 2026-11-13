package executor

import (
	"runtime"
	"testing"
	"time"
	"weak"

	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// saltMemoCase is one memo key's coordinates besides the plan.
type saltMemoCase struct {
	name     string
	scanType recordlayer.IndexScanType
	covering bool
	columns  []string
}

// saltMemoCases differ pairwise in the memo key. Nil and empty columns are
// distinct keys although the salt encodes both as an empty list.
var saltMemoCases = []saltMemoCase{
	{"fetch by value", recordlayer.IndexScanByValue, false, nil},
	{"fetch by rank", recordlayer.IndexScanByRank, false, nil},
	{"fetch with columns", recordlayer.IndexScanByValue, false, []string{"ab", "c"}},
	{"covering nil columns", recordlayer.IndexScanByValue, true, nil},
	{"covering empty columns", recordlayer.IndexScanByValue, true, []string{}},
	{"covering ab,c", recordlayer.IndexScanByValue, true, []string{"ab", "c"}},
	{"covering a,bc", recordlayer.IndexScanByValue, true, []string{"a", "bc"}},
	{"covering ab,c by rank", recordlayer.IndexScanByRank, true, []string{"ab", "c"}},
}

func saltMemoKey(plan *plans.RecordQueryIndexPlan, c saltMemoCase) indexSaltKey {
	return indexSaltKey{plan: weak.Make(plan), scanType: c.scanType, covering: c.covering, columns: identityColumns(c.columns)}
}

// directIndexSalt is the salt computed without the memo.
func directIndexSalt(t testing.TB, plan *plans.RecordQueryIndexPlan, c saltMemoCase) string {
	t.Helper()
	salt, err := indexScanRangeFingerprintSaltFieldsWithFlowedType(plan, c.scanType, c.covering, c.columns, plan.GetFlowedType())
	return mustScanIdentity(t, salt, err)
}

func memoIndexSalt(t testing.TB, plan *plans.RecordQueryIndexPlan, c saltMemoCase) string {
	t.Helper()
	salt, err := indexScanRangeFingerprintSaltFields(plan, c.scanType, c.covering, c.columns)
	return mustScanIdentity(t, salt, err)
}

// memoEntriesOf counts indexSaltMemo's entries keyed by plan.
func memoEntriesOf(plan *plans.RecordQueryIndexPlan) int {
	ptr := weak.Make(plan)
	n := 0
	indexSaltMemo.Range(func(k, _ any) bool {
		if k.(indexSaltKey).plan == ptr {
			n++
		}
		return true
	})
	return n
}

func saltMemoTestPlan(name string) *plans.RecordQueryIndexPlan {
	return indexIdentityTestPlan(name, false, nil, values.NotNullDouble, []string{"T"}, scanIdentityResultType(values.NotNullLong))
}

// Each memo key holds the salt computed without the memo, a warm memo
// returns it again, and no two keys of one plan share an entry: a key that
// collapsed two cases would return the first case's salt for the second.
func TestIndexSaltMemoHoldsTheDirectSaltPerKey(t *testing.T) {
	t.Parallel()
	plan := saltMemoTestPlan("idx_memo_keys")
	if n := memoEntriesOf(plan); n != 0 {
		t.Fatalf("a fresh plan has %d memo entries", n)
	}
	want := make(map[string]string, len(saltMemoCases))
	for _, c := range saltMemoCases {
		want[c.name] = directIndexSalt(t, plan, c)
		if got := memoIndexSalt(t, plan, c); got != want[c.name] {
			t.Errorf("%s: cold memo salt differs from the direct salt", c.name)
		}
		if stored, ok := indexSaltMemo.Load(saltMemoKey(plan, c)); !ok || stored != want[c.name] {
			t.Errorf("%s: memo entry present=%t, want the direct salt", c.name, ok)
		}
	}
	if n := memoEntriesOf(plan); n != len(saltMemoCases) {
		t.Fatalf("plan has %d memo entries, want one per case (%d)", n, len(saltMemoCases))
	}
	// Every case is now warm, so each is served by its own entry.
	for range 2 {
		for _, c := range saltMemoCases {
			if got := memoIndexSalt(t, plan, c); got != want[c.name] {
				t.Errorf("%s: warm memo salt differs from the direct salt", c.name)
			}
		}
	}
	base := want["fetch by value"]
	variants := map[string]string{}
	for _, c := range saltMemoCases {
		if c.name != "fetch by value" && c.name != "covering empty columns" {
			variants[c.name] = want[c.name]
		}
	}
	assertScanIdentitiesDistinct(t, base, variants)
}

// A With* copy (or a plan rebuilt with another flowed type, which no With*
// sets) is its own memo key: its salt is computed from it, the original's
// entries are untouched, and a copy that changes nothing the salt names (the
// control) computes the original's salt.
func TestIndexSaltMemoKeysEachPlanCopy(t *testing.T) {
	t.Parallel()
	base := saltMemoTestPlan("idx_memo_copies")
	baseSalts := make(map[string]string, len(saltMemoCases))
	for _, c := range saltMemoCases {
		baseSalts[c.name] = memoIndexSalt(t, base, c)
	}
	for _, variant := range []struct {
		name    string
		plan    *plans.RecordQueryIndexPlan
		changes bool
	}{
		{"physical key type", base.WithKeyComponentTypes([]values.Type{values.NotNullFloat}), true},
		{"primary-key columns", base.WithIndexMetadata([]string{"K"}, []string{"OTHER_ID"}, false), true},
		{"flowed type", indexIdentityTestPlan("idx_memo_copies", false, nil, values.NotNullDouble, []string{"T"},
			scanIdentityResultType(values.NullableString)), true},
		{"strictly sorted (control)", base.WithStrictlySorted(), false},
	} {
		if variant.plan == base {
			t.Fatalf("%s: fixture is the base plan itself", variant.name)
		}
		for _, c := range saltMemoCases {
			want := directIndexSalt(t, variant.plan, c)
			if got := memoIndexSalt(t, variant.plan, c); got != want {
				t.Errorf("%s / %s: memo salt differs from the copy's direct salt", variant.name, c.name)
			}
			if changed := want != baseSalts[c.name]; changed != variant.changes {
				t.Errorf("%s / %s: salt differs from the base's = %t, want %t", variant.name, c.name, changed, variant.changes)
			}
		}
	}
	for _, c := range saltMemoCases {
		if got := memoIndexSalt(t, base, c); got != baseSalts[c.name] || got != directIndexSalt(t, base, c) {
			t.Errorf("%s: the base plan's salt changed after its copies were memoised", c.name)
		}
	}
}

// A plan's entries are deleted once it is collected, and a live plan's stay.
func TestIndexSaltMemoForgetsCollectedPlans(t *testing.T) {
	t.Parallel()
	// memoise fills plan's entries and returns their keys, which hold the
	// plan weakly. The entries are checked while the plan is still alive.
	memoise := func(plan *plans.RecordQueryIndexPlan) []indexSaltKey {
		keys := make([]indexSaltKey, 0, len(saltMemoCases))
		for _, c := range saltMemoCases {
			want := directIndexSalt(t, plan, c)
			if got := memoIndexSalt(t, plan, c); got != want {
				t.Fatalf("%s: memo salt differs from the direct salt", c.name)
			}
			key := saltMemoKey(plan, c)
			if stored, ok := indexSaltMemo.Load(key); !ok || stored != want {
				t.Fatalf("%s: salt was not memoised", c.name)
			}
			keys = append(keys, key)
		}
		runtime.KeepAlive(plan)
		return keys
	}
	resident := func(keys []indexSaltKey) int {
		n := 0
		for _, k := range keys {
			if _, ok := indexSaltMemo.Load(k); ok {
				n++
			}
		}
		return n
	}

	live := saltMemoTestPlan("idx_memo_live")
	liveKeys := memoise(live)
	collectedKeys := memoise(saltMemoTestPlan("idx_memo_collected"))

	for cycle := 0; cycle < 100 && resident(collectedKeys) > 0; cycle++ {
		runtime.GC()
		time.Sleep(10 * time.Millisecond)
	}
	if n := resident(collectedKeys); n != 0 {
		t.Fatalf("%d of %d entries of an unreachable plan survived 100 GC cycles", n, len(collectedKeys))
	}
	for _, k := range collectedKeys {
		if k.plan.Value() != nil {
			t.Fatal("an entry was deleted while its plan is still reachable")
		}
	}
	if n := resident(liveKeys); n != len(liveKeys) {
		t.Fatalf("%d of %d entries of a live plan survived, want all", n, len(liveKeys))
	}
	for _, c := range saltMemoCases {
		if memoIndexSalt(t, live, c) != directIndexSalt(t, live, c) {
			t.Errorf("%s: the live plan's salt changed across GC", c.name)
		}
	}
	runtime.KeepAlive(live)
}
