package cascades

import (
	"fmt"
	"os"
	"reflect"
	"testing"
)

// TestMain pins the package's only permanently-mutable global — the
// rule registry — against test-time writes.
//
// The registry has no unregister and RegisterRule panics on a duplicate
// name, so a test that registers into the global leaves residue that
// collides with itself the next time the binary runs its tests in the
// same process: `go test -count=2`, a `--runs_per_test` loop, or any
// harness that calls m.Run twice. Production init is immune (each
// register*Rules loop skips a name LookupRule already resolves); only
// direct RegisterRule calls from tests can do it.
//
// Snapshotting the registry either side of m.Run catches that in a
// SINGLE run, which is the point: a -count=2 run only fails if the
// offending test happens to be selected, and a green -count=1 run would
// otherwise report nothing. Ordering is deterministic here — nothing
// else can be running when TestMain has control — which an assertion
// inside a t.Parallel test could not promise.
func TestMain(m *testing.M) {
	before := RegisteredRuleNames()
	code := m.Run()
	after := RegisteredRuleNames()
	if !reflect.DeepEqual(before, after) {
		fmt.Fprintf(os.Stderr,
			"FAIL: the package-level rule registry was mutated by a test.\n"+
				"  Registered rules must come from package init only; a test that calls\n"+
				"  RegisterRule makes repeated in-process runs panic on the duplicate name.\n"+
				"  Register into a private newRuleRegistry() instead.\n"+
				"  added:   %v\n  removed: %v\n",
			difference(after, before), difference(before, after))
		if code == 0 {
			code = 1
		}
	}
	os.Exit(code)
}

// difference returns the entries of a that are absent from b.
func difference(a, b []string) []string {
	inB := make(map[string]struct{}, len(b))
	for _, s := range b {
		inB[s] = struct{}{}
	}
	var only []string
	for _, s := range a {
		if _, ok := inB[s]; !ok {
			only = append(only, s)
		}
	}
	return only
}
