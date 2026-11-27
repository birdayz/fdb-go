package cascades

import (
	"reflect"
	"sort"
	"strconv"
	"sync"
	"testing"
)

// The registry-mechanics tests below register into their OWN
// ruleRegistry, never the package global. Registration is permanent —
// the registry has no unregister and RegisterRule panics on a duplicate
// name — so a test that wrote into the global would collide with itself
// the second time the binary ran its tests in one process
// (`go test -count=2`, or any repeated in-process run). A per-test
// instance removes the shared state instead of scheduling a cleanup a
// future test can forget. TestMain enforces that the global stays
// untouched.

func TestRuleRegistry_RoundTrip(t *testing.T) {
	t.Parallel()
	reg := newRuleRegistry()
	r := NewFilterMergeRule()
	got := reg.register("RoundTripRule", r)
	if got != r {
		t.Fatal("register didn't return the rule unchanged")
	}
	back := reg.lookup("RoundTripRule")
	if back != r {
		t.Fatalf("lookup(%q) = %v, want %v", "RoundTripRule", back, r)
	}
}

func TestRuleRegistry_DuplicateNamePanics(t *testing.T) {
	t.Parallel()
	reg := newRuleRegistry()
	reg.register("Duplicate", NewFilterMergeRule())
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic on duplicate name")
		}
	}()
	reg.register("Duplicate", NewDistinctMergeRule()) // should panic
}

func TestRuleRegistry_NotFound(t *testing.T) {
	t.Parallel()
	reg := newRuleRegistry()
	if got := reg.lookup("nonexistent"); got != nil {
		t.Fatalf("lookup(nonexistent) = %v, want nil", got)
	}
}

func TestRuleRegistry_NamesSorted(t *testing.T) {
	t.Parallel()
	reg := newRuleRegistry()
	reg.register("C", NewFilterMergeRule())
	reg.register("A", NewDistinctMergeRule())
	reg.register("B", NewNoOpFilterRule())

	want := []string{"A", "B", "C"}
	if got := reg.names(); !reflect.DeepEqual(got, want) {
		t.Fatalf("registered names %v, want sorted %v", got, want)
	}
}

// Concurrency smoke — spam register/lookup from multiple goroutines.
// The registry's mutex prevents data races; this test just ensures
// the lock is held in the right places.
func TestRuleRegistry_Concurrent(t *testing.T) {
	t.Parallel()
	reg := newRuleRegistry()
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			name := "concurrent_" + strconv.Itoa(i)
			reg.register(name, NewFilterMergeRule())
			if got := reg.lookup(name); got == nil {
				t.Errorf("concurrent lookup missed %q", name)
			}
		}()
	}
	wg.Wait()
	if got := len(reg.names()); got != 50 {
		t.Fatalf("registered %d names concurrently, want 50", got)
	}
}

// TestRuleRegistry_ExportedAccessorsAgree covers the exported trio
// against the package global. Read-only by construction: it asserts
// only that RegisteredRuleNames() is sorted, deduplicated, and that
// every name it reports resolves through LookupRule. The mutating half
// of the API is covered on private instances above, so this test can
// run alongside them without depending on their scheduling.
func TestRuleRegistry_ExportedAccessorsAgree(t *testing.T) {
	t.Parallel()
	names := RegisteredRuleNames()
	if len(names) == 0 {
		t.Fatal("RegisteredRuleNames() is empty — package init registered nothing")
	}
	if !sort.StringsAreSorted(names) {
		t.Errorf("RegisteredRuleNames() = %v, want sorted", names)
	}
	for i, n := range names {
		if i > 0 && names[i-1] == n {
			t.Errorf("RegisteredRuleNames() reports %q twice", n)
		}
		if LookupRule(n) == nil {
			t.Errorf("LookupRule(%q) = nil for a name RegisteredRuleNames() reports", n)
		}
	}
}
