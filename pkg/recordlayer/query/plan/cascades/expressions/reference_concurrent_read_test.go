package expressions

import (
	"maps"
	"sync"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// TestSharedReferenceSurvivesConcurrentFlowedTypeReads pins that deriving a
// quantifier's flowed object value does not WRITE to the Reference in a way two
// goroutines can collide on.
//
// The property is not "the planner is concurrent" — it is not; the task loop is
// sequential. It is that a Reference is an ordinary handle, and
// RequireFlowedObjectValue sits under nearly every expression CONSTRUCTOR, so
// "constructing two expressions over one reference" is a shape that tests and
// helpers reach for freely. It was safe until the flowed-type memo existed:
// before that, the derivation only read the member list.
//
// A FRESH REFERENCE PER ROUND IS THE WHOLE DESIGN. Sharing ONE reference across
// all iterations — the shape this test had first — leaves the memo filled after
// the first hit, so every later iteration is a read-only hit and the entire
// collision window is the single write at t=0. That is enough to be detected but
// it is INCIDENTAL: it depends on the scheduler interleaving one write against the
// reads. Minting a reference per round makes the window structural, one first-write
// race per round, so the detector does not rest on timing.
//
// Both shapes were measured against the mutation this pins — the memo's
// atomic.Pointer reverted to a plain *flowedTypeMemo, which is the shape that
// shipped the race — and both reddened 15 runs in 15, reproduced independently on a
// second machine. So the restructure buys robustness, not a red rate: the number to
// state is that the mutation is caught 15/15 at goroutines=8, rounds=32.
//
// Unlike the interner's concurrency detector, this rate is NOT scheduler-dependent,
// and the fresh-reference-per-round shape is why: there are 32 first-write windows
// instead of one, so the detector does not need the scheduler to cooperate on any
// particular one of them.
//
// It is worth nothing without -race — the whole point is the detector.
func TestSharedReferenceSurvivesConcurrentFlowedTypeReads(t *testing.T) {
	t.Parallel()

	const goroutines = 8
	const rounds = 32

	for round := 0; round < rounds; round++ {
		// Fresh, so its memo is EMPTY and every goroutine below races the first
		// write rather than reading a filled slot.
		shared := InitialOf(mustExpression(
			NewFullUnorderedScanExpression([]string{"T"}, testRecordType())))

		var start sync.WaitGroup
		var done sync.WaitGroup
		start.Add(1)
		failures := make([]error, goroutines)
		types := make([]values.Type, goroutines)
		for g := 0; g < goroutines; g++ {
			done.Add(1)
			go func(slot int) {
				defer done.Done()
				start.Wait()
				// A fresh quantifier over the shared reference: this is what an
				// expression constructor does.
				qov, err := ForEachQuantifier(shared).RequireFlowedObjectValue()
				if err != nil {
					failures[slot] = err
					return
				}
				types[slot] = qov.FlowedType()
			}(g)
		}
		start.Done()
		done.Wait()

		for slot, err := range failures {
			if err != nil {
				t.Fatalf("round %d goroutine %d: %v", round, slot, err)
			}
		}
		// Non-vacuity: every goroutine must have reached the derivation and agreed
		// on the row, or a silently-skipped loop would make the race unreachable.
		for slot := range types {
			if types[slot] == nil {
				t.Fatalf("round %d goroutine %d never derived a flowed type", round, slot)
			}
			if !types[slot].Equals(types[0]) {
				t.Fatalf("round %d goroutine %d derived %s, goroutine 0 derived %s — one "+
					"shared reference reported two different rows",
					round, slot, values.DescribeType(types[slot]), values.DescribeType(types[0]))
			}
		}
	}
}

// Property reads may share a stable Reference even though memo mutation is
// single-threaded. Exercise cold publication on a fresh graph each round, and
// check the actual free aliases on cold and warm reads rather than merely
// comparing readers that could all return the same incorrect empty set.
func TestSharedReferenceSurvivesConcurrentCorrelationReads(t *testing.T) {
	t.Parallel()

	for _, shape := range []string{"empty", "transitive", "shared_dag", "forwarded"} {
		t.Run(shape, func(t *testing.T) {
			t.Parallel()
			const goroutines = 8
			const rounds = 32
			outer := values.NamedCorrelationIdentifier("outer")
			want := map[values.CorrelationIdentifier]struct{}{}
			if shape != "empty" {
				want[outer] = struct{}{}
			}

			for round := 0; round < rounds; round++ {
				shared := InitialOf(mustExpression(
					NewFullUnorderedScanExpression([]string{"T"}, testRecordType())))
				if shape != "empty" {
					inner := ForEachQuantifier(shared)
					pred := predicates.NewComparisonPredicate(
						mustExpression(inner.RequireFlowedObjectValue()),
						predicates.Comparison{Type: predicates.ComparisonEquals, Operand: mustQOV(outer)},
					)
					shared = InitialOf(mustExpression(NewLogicalFilterExpression([]predicates.QueryPredicate{pred}, inner)))
				}
				if shape == "shared_dag" {
					shared = InitialOf(mustExpression(NewLogicalUnionExpression([]Quantifier{
						ForEachQuantifier(shared), ForEachQuantifier(shared),
					})))
				}
				root := shared
				var middle *Reference
				if shape == "forwarded" {
					middle = &Reference{forwardedTo: root}
					shared = &Reference{forwardedTo: middle}
				}

				var start, done sync.WaitGroup
				start.Add(1)
				cold := make([]map[values.CorrelationIdentifier]struct{}, goroutines)
				warm := make([]map[values.CorrelationIdentifier]struct{}, goroutines)
				for slot := 0; slot < goroutines; slot++ {
					done.Add(1)
					go func() {
						defer done.Done()
						q := ForEachQuantifier(shared)
						start.Wait()
						if slot%2 == 0 {
							cold[slot] = shared.GetCorrelatedTo()
							warm[slot] = q.GetCorrelatedTo()
						} else {
							cold[slot] = q.GetCorrelatedTo()
							warm[slot] = shared.GetCorrelatedTo()
						}
					}()
				}
				start.Done()
				done.Wait()

				for slot := 0; slot < goroutines; slot++ {
					if cold[slot] == nil || !maps.Equal(cold[slot], want) {
						t.Fatalf("round %d goroutine %d cold correlations = %v, want non-nil %v", round, slot, cold[slot], want)
					}
					if warm[slot] == nil || !maps.Equal(warm[slot], want) {
						t.Fatalf("round %d goroutine %d warm correlations = %v, want non-nil %v", round, slot, warm[slot], want)
					}
				}
				if middle != nil && (shared.forwardedTo != middle || middle.forwardedTo != root) {
					t.Fatal("correlation reads compressed shared forwarding topology")
				}
			}
		})
	}
}

// A stable graph may be read concurrently; edits and invalidation must still be
// sequential. Exercise each invalidation site after warming the cache, including
// an absorb that adds no members and therefore cannot rely on Insert to clear it.
func TestReferenceCorrelationCacheSequentialInvalidation(t *testing.T) {
	t.Parallel()

	for _, operation := range []string{"insert", "insert_final", "prepared_exploratory", "prepared_final", "absorb", "absorb_duplicate", "invalidate_forwarded"} {
		t.Run(operation, func(t *testing.T) {
			t.Parallel()
			outer := values.NamedCorrelationIdentifier("outer")
			member := mustExpression(NewSelectExpression(mustQOV(outer), nil, nil))
			ref := &Reference{}
			before := map[values.CorrelationIdentifier]struct{}{}
			if operation == "absorb_duplicate" {
				ref = InitialOf(member)
				before[outer] = struct{}{}
			}
			borrowed := ref.GetCorrelatedTo()
			if borrowed == nil || !maps.Equal(borrowed, before) || ref.correlatedToCache.Load() == nil {
				t.Fatalf("cold read = %v, want cached non-nil %v", borrowed, before)
			}

			switch operation {
			case "insert":
				if !ref.Insert(member) {
					t.Fatal("first exploratory member was not inserted")
				}
			case "insert_final":
				if !ref.InsertFinal(member) {
					t.Fatal("first final member was not inserted")
				}
			case "prepared_exploratory", "prepared_final":
				relation := mustExpression(values.ExactRelationOf(member.GetResultValue().Type()))
				var exploratory, final []RelationalExpression
				if operation == "prepared_exploratory" {
					exploratory = []RelationalExpression{member}
				} else {
					final = []RelationalExpression{member}
				}
				if err := ref.ApplyPreparedMemberBatch(ref.AdmissionView(), relation, exploratory, final, 0); err != nil {
					t.Fatalf("prepared apply: %v", err)
				}
			case "absorb", "absorb_duplicate":
				loser := InitialOf(member)
				ref.Absorb(loser)
				if !loser.IsForwarded() || loser.Canonical() != ref {
					t.Fatal("absorb did not forward the loser to the survivor")
				}
			case "invalidate_forwarded":
				forwarded := &Reference{forwardedTo: ref}
				forwarded.InvalidateCorrelatedToCache()
			}
			if ref.correlatedToCache.Load() != nil {
				t.Fatal("sequential mutation retained a previously computed correlation cache")
			}
			if !maps.Equal(borrowed, before) {
				t.Fatalf("invalidation mutated a previously returned map: %v, want %v", borrowed, before)
			}
			want := map[values.CorrelationIdentifier]struct{}{outer: {}}
			if operation == "invalidate_forwarded" {
				want = map[values.CorrelationIdentifier]struct{}{}
			}
			for read := 0; read < 2; read++ {
				if got := ref.GetCorrelatedTo(); got == nil || !maps.Equal(got, want) || ref.correlatedToCache.Load() == nil {
					t.Fatalf("read %d after invalidation = %v, want cached non-nil %v", read, got, want)
				}
			}
		})
	}
}
