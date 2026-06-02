package bench

import (
	"fmt"
	"os"
	"strings"
	"testing"

	cgofdb "github.com/apple/foundationdb/bindings/go/src/fdb"
	gofdb "github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb"
)

// SNAPSHOT_RYW_ENABLE / SNAPSHOT_RYW_DISABLE differential vs libfdb_c — RFC-061.
//
// libfdb_c models snapshot-RYW as an integer COUNTER (ReadYourWrites.actor.cpp): the option
// starts at 1 (API >= 300), SNAPSHOT_RYW_ENABLE does count++, SNAPSHOT_RYW_DISABLE does count--,
// and a snapshot read bypasses the read-your-writes cache iff count <= 0. So a disable followed
// by an enable returns to count 1 → snapshot reads SEE the txn's own pending writes again; two
// disables need two enables to re-enable.
//
// The Go client modeled this as a boolean with SetSnapshotRywEnable() as a NO-OP, so once
// disabled it could never be re-enabled — a snapshot read after disable→enable wrongly bypassed
// RYW. This pins the counter semantics: for each option sequence, the snapshot read of a pending
// (uncommitted) write must agree between the two clients.

func TestDifferential_SnapshotRYWReenable(t *testing.T) {
	t.Parallel()
	ns := strings.ReplaceAll(t.Name(), "/", "_")
	pfx := fmt.Sprintf("snapryw_%d_%s_", os.Getpid(), ns)

	// Each sequence: in a fresh txn, write a pending (uncommitted) value, apply a series of
	// snapshot-RYW option toggles, then snapshot-read the key. Returns "pending" if the snapshot
	// read saw the txn's own write (RYW active) or "absent" if it bypassed to storage (the key
	// was never committed). The two clients must return the same marker.
	type seq struct {
		name    string
		toggles []bool // true = enable, false = disable, applied in order
	}
	seqs := []seq{
		{"no_option", nil},                                     // default: RYW active → sees pending
		{"disable", []bool{false}},                             // disableCount 1 → bypass → absent
		{"disable_enable", []bool{false, true}},                // disableCount 0 → active → sees pending (THE bug)
		{"enable_disable", []bool{true, false}},                // disableCount 0 → active → sees pending
		{"disable_disable_enable", []bool{false, false, true}}, // disableCount 1 → bypass → absent (needs a COUNTER, not a boolean reset)
		{"disable_enable_enable", []bool{false, true, true}},   // disableCount -1 → active → sees pending
		// Negative-count axis (FDB-C++ dev review): an enable from the default pushes the count
		// negative (C++ enabledCount 2). Proves the counter does not clamp at 0.
		{"enable_only", []bool{true}},                        // disableCount -1 → active → sees pending
		{"enable_then_disable", []bool{true, false}},         // -1 then 0 → recovers to default → sees pending
		{"enable_enable_disable", []bool{true, true, false}}, // -2 then -1 → active → sees pending
	}

	goRun := func(s seq) string {
		tr, err := goClient.CreateTransaction()
		if err != nil {
			t.Fatalf("go create: %v", err)
		}
		defer tr.Cancel()
		k := gofdb.Key(pfx + s.name)
		tr.Set(k, []byte("pending"))
		for _, on := range s.toggles {
			if on {
				_ = tr.Options().SetSnapshotRywEnable()
			} else {
				_ = tr.Options().SetSnapshotRywDisable()
			}
		}
		v, err := tr.Snapshot().Get(k).Get()
		if err != nil {
			t.Fatalf("go snapshot get %s: %v", s.name, err)
		}
		if v == nil {
			return "absent"
		}
		return string(v)
	}
	cgoRun := func(s seq) string {
		tr, err := cgoClient.CreateTransaction()
		if err != nil {
			t.Fatalf("cgo create: %v", err)
		}
		defer tr.Cancel()
		k := cgofdb.Key(pfx + s.name)
		tr.Set(k, []byte("pending"))
		for _, on := range s.toggles {
			if on {
				_ = tr.Options().SetSnapshotRywEnable()
			} else {
				_ = tr.Options().SetSnapshotRywDisable()
			}
		}
		v, err := tr.Snapshot().Get(k).Get()
		if err != nil {
			t.Fatalf("cgo snapshot get %s: %v", s.name, err)
		}
		if v == nil {
			return "absent"
		}
		return string(v)
	}

	for _, s := range seqs {
		s := s
		t.Run(s.name, func(t *testing.T) {
			t.Parallel()
			goVal := goRun(s)
			cgoVal := cgoRun(s)
			if goVal != cgoVal {
				t.Fatalf("%s: snapshot read after toggles differs: go=%q cgo=%q", s.name, goVal, cgoVal)
			}
		})
	}

	// READ_YOUR_WRITES_DISABLE dominates the snapshot-RYW counter (FDB-C++ dev review): a clean
	// (pre-op) RYW-disable forces ALL reads — regular AND snapshot — read-through, regardless of
	// the snapshot-RYW state (ReadYourWrites.actor.cpp:400 checks readYourWritesDisabled BEFORE
	// the snapshot bypass at :402). Clean (before any op) so it does not poison (RFC-059). Both
	// clients must return "absent" (the pending write is never visible to a snapshot read).
	t.Run("ryw_disable_dominates", func(t *testing.T) {
		t.Parallel()
		gk := gofdb.Key(pfx + "rywdom")
		ck := cgofdb.Key(pfx + "rywdom")
		goVal := func() string {
			tr, err := goClient.CreateTransaction()
			if err != nil {
				t.Fatalf("go create: %v", err)
			}
			defer tr.Cancel()
			_ = tr.Options().SetReadYourWritesDisable() // clean: before any op
			tr.Set(gk, []byte("pending"))
			v, err := tr.Snapshot().Get(gk).Get()
			if err != nil {
				t.Fatalf("go snapshot get: %v", err)
			}
			if v == nil {
				return "absent"
			}
			return string(v)
		}()
		cgoVal := func() string {
			tr, err := cgoClient.CreateTransaction()
			if err != nil {
				t.Fatalf("cgo create: %v", err)
			}
			defer tr.Cancel()
			_ = tr.Options().SetReadYourWritesDisable()
			tr.Set(ck, []byte("pending"))
			v, err := tr.Snapshot().Get(ck).Get()
			if err != nil {
				t.Fatalf("cgo snapshot get: %v", err)
			}
			if v == nil {
				return "absent"
			}
			return string(v)
		}()
		if goVal != cgoVal {
			t.Fatalf("ryw_disable_dominates: go=%q cgo=%q", goVal, cgoVal)
		}
	})
}
