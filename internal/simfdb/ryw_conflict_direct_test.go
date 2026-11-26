package simfdb

import (
	"fmt"
	"math/rand"
	"testing"
)

// TestConflictForKeyDirectAgreesWithTheMapBuild is a DIFFERENTIAL pin: the
// allocation-free single-key replay must answer exactly what the whole-buffer
// map build answers, for every buffer shape and every key.
//
// It is written as a differential rather than as a table of expected verdicts
// because the two are the same state machine expressed twice, and the failure
// mode is DRIFT — a future change to buildWriteMap's classification that the
// narrowed copy does not learn about. A table would pin today's answers and go
// quietly stale against the definition it is supposed to track; comparing the
// two makes the definition itself the oracle.
//
// The verdict decides a CONFLICT, so a divergence is not a performance
// annoyance: answering false where the map says true under-conflicts, and a
// transaction that really read a key commits over a concurrent writer of it.
func TestConflictForKeyDirectAgreesWithTheMapBuild(t *testing.T) {
	t.Parallel()

	// A small key space is deliberate: it is what makes clears, sets and atomics
	// actually collide on the same keys, which is where the classification has
	// anything to decide.
	keys := []string{"a", "b", "c", "d", "e"}
	kb := func(s string) []byte { return []byte(s) }

	rng := rand.New(rand.NewSource(1))
	kinds := []mutationKind{mutSet, mutClear, mutClearRange, mutAtomic, mutVersionstampedKey, mutVersionstampedValue}

	agreed, conflicts, nonConflicts := 0, 0, 0
	for trial := 0; trial < 400; trial++ {
		tx := &simTxn{}
		n := rng.Intn(8)
		for i := 0; i < n; i++ {
			kind := kinds[rng.Intn(len(kinds))]
			lo := rng.Intn(len(keys))
			m := mutation{kind: kind, key: kb(keys[lo])}
			if kind == mutClearRange {
				hi := lo + 1 + rng.Intn(len(keys)-lo)
				if hi >= len(keys) {
					m.end = kb("z")
				} else {
					m.end = kb(keys[hi])
				}
			}
			tx.buffer = append(tx.buffer, m)
		}
		wm := tx.buildWriteMap()
		for _, k := range keys {
			want := wm.conflictForKey(kb(k))
			got := tx.conflictForKeyDirect(kb(k))
			if got != want {
				t.Fatalf("trial %d key %q: direct replay says %v, map build says %v.\n"+
					"  buffer: %s\n"+
					"  These are one state machine written twice; a divergence here is a wrong "+
					"CONFLICT verdict, and answering false where the map says true under-conflicts.",
					trial, k, got, want, describeBuffer(tx.buffer))
			}
			agreed++
			if want {
				conflicts++
			} else {
				nonConflicts++
			}
		}
	}

	// Both populations must be non-empty, or the agreement is agreement about
	// one answer. A generator that only ever produced "conflict" would pass
	// every comparison above while testing nothing about the other verdict.
	if conflicts == 0 || nonConflicts == 0 {
		t.Fatalf("the generated buffers produced %d conflicts and %d non-conflicts out of %d "+
			"comparisons; the differential is vacuous unless both verdicts occur",
			conflicts, nonConflicts, agreed)
	}
	t.Logf("agreed on %d comparisons (%d conflict, %d no-conflict)", agreed, conflicts, nonConflicts)
}

func describeBuffer(buf []mutation) string {
	out := ""
	for _, m := range buf {
		if out != "" {
			out += " "
		}
		switch m.kind {
		case mutClearRange:
			out += fmt.Sprintf("clearRange[%s,%s)", m.key, m.end)
		case mutSet:
			out += fmt.Sprintf("set(%s)", m.key)
		case mutClear:
			out += fmt.Sprintf("clear(%s)", m.key)
		case mutAtomic:
			out += fmt.Sprintf("atomic(%s)", m.key)
		case mutVersionstampedKey:
			out += fmt.Sprintf("vsKey(%s)", m.key)
		case mutVersionstampedValue:
			out += fmt.Sprintf("vsValue(%s)", m.key)
		default:
			out += fmt.Sprintf("kind%d(%s)", m.kind, m.key)
		}
	}
	return out
}
