package expressions

import (
	"fmt"
	"maps"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

func correlationReadGraph(depth int, alias values.CorrelationIdentifier) (*Reference, *Reference) {
	leaf := InitialOf(mustExpression(NewSelectExpression(
		mustExpression(values.NewQuantifiedObjectValue(alias, values.NotNullLong)), nil, nil,
	)))
	root := leaf
	for range depth {
		left := InitialOf(mustExpression(NewLogicalDistinctExpression(ForEachQuantifier(root))))
		right := InitialOf(mustExpression(NewLogicalDistinctExpression(ForEachQuantifier(root))))
		root = InitialOf(mustExpression(NewLogicalUnionExpression([]Quantifier{ForEachQuantifier(left), ForEachQuantifier(right)})))
	}
	return root, leaf
}

func TestReferenceCorrelationReadsKeepBorrowedSnapshots(t *testing.T) {
	t.Parallel()
	a, b := values.NamedCorrelationIdentifier("a"), values.NamedCorrelationIdentifier("b")
	root, leaf := correlationReadGraph(3, a)
	other, _ := correlationReadGraph(3, b)
	wantA := map[values.CorrelationIdentifier]struct{}{a: {}}
	wantB := map[values.CorrelationIdentifier]struct{}{b: {}}
	borrowed := root.GetCorrelatedTo()
	for range 16 {
		if got := other.GetCorrelatedTo(); !maps.Equal(got, wantB) {
			t.Fatalf("second graph inherited another read: %v", got)
		}
		if !maps.Equal(borrowed, wantA) || !maps.Equal(root.GetCorrelatedTo(), wantA) {
			t.Fatal("reading another graph changed a borrowed snapshot")
		}
	}
	leaf.PruneWith(mustExpression(NewSelectExpression(
		mustExpression(values.NewQuantifiedObjectValue(b, values.NotNullLong)), nil, nil,
	)))
	if got := root.GetCorrelatedTo(); !maps.Equal(got, map[values.CorrelationIdentifier]struct{}{a: {}, b: {}}) {
		t.Fatalf("scratch reuse ignored the new final descendant: %v", got)
	}
	leaf.AdvancePlannerStage(StagePlanned)
	if got := root.GetCorrelatedTo(); !maps.Equal(got, wantB) {
		t.Fatalf("scratch reuse retained an old descendant: %v", got)
	}
	if !maps.Equal(borrowed, wantA) {
		t.Fatal("refresh changed the previously borrowed snapshot")
	}
}

func TestReferenceCorrelationLeafUsesNoScratch(t *testing.T) {
	t.Parallel()
	root, _ := correlationReadGraph(0, values.NamedCorrelationIdentifier("outer"))
	root.GetCorrelatedTo()
	var reader referenceCorrelationReader
	if snapshot := reader.reference(root); snapshot != root.correlatedToCache.Load() || len(snapshot.correlations) != 1 {
		t.Fatal("leaf read did not reuse the validated snapshot")
	}
	if reader.memo != nil || reader.active != nil || reader.expressions != nil {
		t.Fatal("a cached leaf allocated traversal scratch without dependencies to visit")
	}
}

func TestReferenceCorrelationScratchReset(t *testing.T) {
	t.Parallel()
	alias := values.NamedCorrelationIdentifier("outer")
	root, _ := correlationReadGraph(2, alias)
	reader := referenceCorrelationReader{publish: true}
	snapshot := reader.reference(root)
	if len(reader.memo) == 0 || len(reader.expressions) == 0 {
		t.Fatal("fixture did not populate traversal scratch")
	}
	reader.active[root] = struct{}{}
	reader.reset()
	if len(reader.memo) != 0 || len(reader.expressions) != 0 || len(reader.active) != 0 || reader.publish {
		t.Fatal("scratch reset retained graph references or publication permission")
	}
	if snapshot == nil || len(snapshot.correlations) != 1 {
		t.Fatal("scratch reset changed the published snapshot")
	}
	if _, present := snapshot.correlations[alias]; !present {
		t.Fatal("scratch reset changed the published free correlation")
	}
	other, _ := correlationReadGraph(2, values.NamedCorrelationIdentifier("other"))
	if next := reader.reference(other); len(next.correlations) != 1 || other.correlatedToCache.Load() != nil {
		t.Fatal("reset reader leaked correlations or published during a read-only traversal")
	}
}

func FuzzReferenceCorrelationScratch(f *testing.F) {
	f.Add([]byte{0, 1, 2, 3, 4}, byte(2))
	f.Add([]byte{3, 3, 4, 0, 1, 2, 0}, byte(0))
	f.Fuzz(func(t *testing.T, data []byte, depth byte) {
		t.Parallel()
		aliases := []values.CorrelationIdentifier{
			values.NamedCorrelationIdentifier("a"), values.NamedCorrelationIdentifier("b"), values.NamedCorrelationIdentifier("c"),
		}
		root, leaf := correlationReadGraph(int(depth%3), aliases[0])
		other, _ := correlationReadGraph(1, values.NamedCorrelationIdentifier("other"))
		exploratory := map[values.CorrelationIdentifier]struct{}{aliases[0]: {}}
		finals := make(map[values.CorrelationIdentifier]struct{})
		for _, op := range data[:min(len(data), 16)] {
			borrowed := root.GetCorrelatedTo()
			before := maps.Clone(borrowed)
			alias := aliases[int(op/5)%len(aliases)]
			member := mustExpression(NewSelectExpression(
				mustExpression(values.NewQuantifiedObjectValue(alias, values.NotNullLong)), nil, nil,
			))
			switch op % 5 {
			case 0:
				leaf.Insert(member)
				exploratory[alias] = struct{}{}
			case 1:
				leaf.InsertFinal(member)
				finals[alias] = struct{}{}
			case 2:
				leaf.PruneWith(member)
				finals = map[values.CorrelationIdentifier]struct{}{alias: {}}
			case 3:
				leaf.AdvancePlannerStage(StagePlanned)
				exploratory, finals = finals, make(map[values.CorrelationIdentifier]struct{})
			case 4:
				survivor := InitialOf(member)
				survivor.Absorb(leaf)
				leaf = survivor
				exploratory[alias] = struct{}{}
			}
			other.GetCorrelatedTo()
			want := maps.Clone(exploratory)
			maps.Copy(want, finals)
			if got := root.GetCorrelatedTo(); !maps.Equal(got, want) {
				t.Fatalf("operation %d: correlations=%v, want %v", op, got, want)
			}
			if !maps.Equal(borrowed, before) {
				t.Fatal("later read changed a borrowed snapshot")
			}
		}
	})
}

func BenchmarkReferenceCorrelationRead(b *testing.B) {
	for _, depth := range []int{0, 4, 12} {
		b.Run(fmt.Sprintf("diamond-depth=%d", depth), func(b *testing.B) {
			alias := values.NamedCorrelationIdentifier("outer")
			root, _ := correlationReadGraph(depth, alias)
			root.GetCorrelatedTo()
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if got := root.GetCorrelatedTo(); len(got) != 1 {
					b.Fatal("lost free correlation")
				} else if _, ok := got[alias]; !ok {
					b.Fatal("changed free correlation")
				}
			}
		})
	}
}
