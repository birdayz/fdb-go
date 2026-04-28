package cascades

import (
	"reflect"
	"sync"
	"testing"
)

func TestRuleRegistry_RoundTrip(t *testing.T) {
	t.Parallel()
	// Register a fresh rule under a unique name (avoid collision
	// with anything else in the registry from package init).
	name := "TestRoundTripRule_" + t.Name()
	r := NewFilterMergeRule()
	got := RegisterRule(name, r)
	if got != r {
		t.Fatal("RegisterRule didn't return the rule unchanged")
	}
	back := LookupRule(name)
	if back != r {
		t.Fatalf("LookupRule(%q) = %v, want %v", name, back, r)
	}
}

func TestRuleRegistry_DuplicateNamePanics(t *testing.T) {
	t.Parallel()
	name := "TestDuplicate_" + t.Name()
	RegisterRule(name, NewFilterMergeRule())
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic on duplicate name")
		}
	}()
	RegisterRule(name, NewDistinctMergeRule()) // should panic
}

func TestRuleRegistry_NotFound(t *testing.T) {
	t.Parallel()
	if got := LookupRule("nonexistent"); got != nil {
		t.Fatalf("LookupRule(nonexistent) = %v, want nil", got)
	}
}

func TestRuleRegistry_NamesSorted(t *testing.T) {
	t.Parallel()
	// Register a few rules with predictable names; verify the names
	// list is sorted. Use a unique prefix to avoid collisions across
	// concurrent tests.
	prefix := "_sortcheck_" + t.Name() + "_"
	RegisterRule(prefix+"C", NewFilterMergeRule())
	RegisterRule(prefix+"A", NewDistinctMergeRule())
	RegisterRule(prefix+"B", NewNoOpFilterRule())

	all := RegisteredRuleNames()
	// Filter to our prefix.
	var ours []string
	for _, n := range all {
		if len(n) >= len(prefix) && n[:len(prefix)] == prefix {
			ours = append(ours, n)
		}
	}
	want := []string{prefix + "A", prefix + "B", prefix + "C"}
	if !reflect.DeepEqual(ours, want) {
		t.Fatalf("registered names %v, want sorted %v", ours, want)
	}
}

// Concurrency smoke — spam Register/Lookup from multiple goroutines.
// The registry's mutex prevents data races; this test just ensures
// the lock is held in the right places.
func TestRuleRegistry_Concurrent(t *testing.T) {
	t.Parallel()
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			name := "concurrent_" + t.Name() + "_" + itoa(i)
			RegisterRule(name, NewFilterMergeRule())
			if got := LookupRule(name); got == nil {
				t.Errorf("concurrent lookup missed %q", name)
			}
		}()
	}
	wg.Wait()
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	negate := n < 0
	if negate {
		n = -n
	}
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if negate {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
