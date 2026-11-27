package executor

import (
	"math/rand/v2"
	"sync"
	"testing"

	"fdb.dev/internal/recordlayer/query/plan/cascades/values"
)

func memoRecordType(name string, nullable bool) *values.RecordType {
	return &values.RecordType{RecordName: name, Nullable: nullable, Fields: []values.Field{
		{Name: "A", FieldType: values.NewPrimitiveType(values.TypeCodeLong, false), Ordinal: 0},
		{Name: "B", FieldType: values.NewArrayType(true, values.NewPrimitiveType(values.TypeCodeString, false)), Ordinal: 1},
	}}
}

// recordTypesEqual is RecordType.Equals: memoising a pair that compared equal
// must not make an unequal pair equal, by value or by a reused object.
func TestRecordTypesEqualMatchesEquals(t *testing.T) {
	t.Parallel()
	a, b := memoRecordType("R", true), memoRecordType("R", true)
	other := memoRecordType("R", false)
	for range 3 {
		if !recordTypesEqual(a, b) || !recordTypesEqual(b, a) || !recordTypesEqual(a, a) {
			t.Fatal("equal record types compared unequal")
		}
		if recordTypesEqual(a, other) || recordTypesEqual(other, b) {
			t.Fatal("unequal record types compared equal")
		}
	}
	if recordTypesEqual(a, nil) || recordTypesEqual(nil, a) {
		t.Fatal("a record type compared equal to nil")
	}
}

// A full memo starts over and keeps answering correctly. Not parallel: it
// reads the process-wide memo's size, which parallel tests' evaluations move.
func TestRecordTypesEqualMemoIsBounded(t *testing.T) {
	keep := make([]*values.RecordType, 0, 2*(recordTypeEqualMemoEntries+8))
	for range recordTypeEqualMemoEntries + 8 {
		a, b := memoRecordType("R", true), memoRecordType("R", true)
		keep = append(keep, a, b)
		if !recordTypesEqual(a, b) {
			t.Fatal("equal record types compared unequal")
		}
	}
	gen := recordTypeEqualMemo.current.Load()
	if n := gen.size.Load(); n > recordTypeEqualMemoEntries {
		t.Fatalf("memo holds %d pairs, bound %d", n, recordTypeEqualMemoEntries)
	}
	var actual int64
	gen.pairs.Range(func(_, _ any) bool {
		actual++
		return true
	})
	if actual > recordTypeEqualMemoEntries || actual != gen.size.Load() {
		t.Fatalf("memo map holds %d pairs (tracked %d), bound %d", actual, gen.size.Load(), recordTypeEqualMemoEntries)
	}
	if recordTypesEqual(keep[0], memoRecordType("R", false)) {
		t.Fatal("unequal record types compared equal after a reset")
	}
}

// TestRecordTypesEqualMemoConcurrentRollover exercises concurrent reads,
// inserts, and generation rollovers across multiple cap cycles under -race.
func TestRecordTypesEqualMemoConcurrentRollover(t *testing.T) {
	t.Parallel()

	const (
		workers        = 8
		pairsPerWorker = 1600 // 12,800 total distinct pairs -> >= 3 rollovers
		hotPoolSize    = 32
	)

	type typePair struct {
		a, b *values.RecordType
	}
	hotPairs := make([]typePair, hotPoolSize)
	for i := range hotPairs {
		nullable := i%2 == 0
		hotPairs[i] = typePair{
			a: memoRecordType("Hot", nullable),
			b: memoRecordType("Hot", nullable),
		}
	}
	unequalRef := memoRecordType("Unequal", false)
	unequalField := &values.RecordType{
		RecordName: "UnequalField",
		Nullable:   true,
		Fields: []values.Field{
			{Name: "C", FieldType: values.NewPrimitiveType(values.TypeCodeLong, false), Ordinal: 0},
		},
	}

	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		seenGens = make(map[*memoGeneration]struct{})
		startGen = recordTypeEqualMemo.current.Load()
	)
	seenGens[startGen] = struct{}{}

	for w := range workers {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			rng := rand.New(rand.NewPCG(uint64(workerID+1), 0x5eed1234))
			keep := make([]*values.RecordType, 0, 2*pairsPerWorker)
			localGens := make(map[*memoGeneration]struct{})

			for i := range pairsPerWorker {
				a := memoRecordType("W", true)
				b := memoRecordType("W", true)
				keep = append(keep, a, b)

				// Insert on miss, then immediately re-read on hit.
				if !recordTypesEqual(a, b) {
					t.Errorf("worker %d iter %d: equal record types compared unequal on miss", workerID, i)
					return
				}
				if !recordTypesEqual(a, b) {
					t.Errorf("worker %d iter %d: equal record types compared unequal on hit", workerID, i)
					return
				}

				// Interleave shared hot-pair reads and unequal checks.
				hp := hotPairs[rng.IntN(len(hotPairs))]
				if !recordTypesEqual(hp.a, hp.b) || !recordTypesEqual(hp.b, hp.a) {
					t.Errorf("worker %d iter %d: hot equal pair compared unequal", workerID, i)
					return
				}
				if recordTypesEqual(a, unequalRef) || recordTypesEqual(a, unequalField) || recordTypesEqual(a, nil) {
					t.Errorf("worker %d iter %d: unequal record types compared equal", workerID, i)
					return
				}

				if i%16 == 0 {
					gen := recordTypeEqualMemo.current.Load()
					localGens[gen] = struct{}{}
					if n := gen.size.Load(); n > recordTypeEqualMemoEntries {
						t.Errorf("worker %d iter %d: memo generation size %d exceeds bound %d", workerID, i, n, recordTypeEqualMemoEntries)
						return
					}
				}
			}
			if !recordTypesEqual(keep[0], keep[1]) {
				t.Errorf("worker %d: retained pair compared unequal after rollovers", workerID)
			}

			mu.Lock()
			for g := range localGens {
				seenGens[g] = struct{}{}
			}
			mu.Unlock()
		}(w)
	}
	wg.Wait()

	finalGen := recordTypeEqualMemo.current.Load()
	seenGens[finalGen] = struct{}{}
	if len(seenGens) < 3 {
		t.Fatalf("expected multiple generation rollovers, observed only %d generation(s)", len(seenGens))
	}
	if n := finalGen.size.Load(); n > recordTypeEqualMemoEntries {
		t.Fatalf("final memo generation holds %d pairs, bound %d", n, recordTypeEqualMemoEntries)
	}
	var actual int64
	finalGen.pairs.Range(func(_, _ any) bool {
		actual++
		return true
	})
	if actual > recordTypeEqualMemoEntries {
		t.Fatalf("final memo map holds %d pairs, bound %d", actual, recordTypeEqualMemoEntries)
	}
}
