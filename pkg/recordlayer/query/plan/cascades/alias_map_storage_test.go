package cascades

import (
	"fmt"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

func TestPlannerAliasMapCompactSnapshots(t *testing.T) {
	t.Parallel()
	ids := plannerAliasStorageIDs()
	for _, pair := range [][2]values.CorrelationIdentifier{
		{ids[2], ids[3]},
		{ids[2], ids[2]},
		{ids[1], ids[1]},
		{ids[0], ids[2]},
		{ids[2], ids[0]},
		{ids[1], ids[2]},
	} {
		t.Run(fmt.Sprintf("%v->%v", pair[0], pair[1]), func(t *testing.T) {
			t.Parallel()
			builder := NewAliasMapBuilder()
			empty := builder.Build()
			if empty != EmptyAliasMap() {
				t.Error("empty builder did not reuse the immutable empty alias map")
			}
			if !builder.Put(pair[0], pair[1]) {
				t.Fatal("first binding rejected")
			}
			want := map[values.CorrelationIdentifier]values.CorrelationIdentifier{pair[0]: pair[1]}
			for _, aliases := range []*AliasMap{builder.Build(), AliasMapOfAliases(pair[0], pair[1])} {
				if aliases.forward != nil || aliases.inverse != nil {
					t.Fatal("singleton alias map allocated hash tables")
				}
				assertPlannerAliasStorage(t, aliases, want, ids)
				sources := aliases.Sources()
				sources[0] = ids[4]
				assertPlannerAliasStorage(t, aliases, want, ids)
			}
			snapshot := builder.Build()
			if !builder.Put(ids[4], ids[5]) {
				t.Fatal("second binding rejected")
			}
			assertPlannerAliasStorage(t, empty, nil, ids)
			assertPlannerAliasStorage(t, snapshot, want, ids)
			want[ids[4]] = ids[5]
			assertPlannerAliasStorage(t, builder.Build(), want, ids)
		})
	}
}

func TestPlannerAliasMapStorageTranslation(t *testing.T) {
	t.Parallel()
	for _, size := range []int{0, 1, 2, 8} {
		t.Run(fmt.Sprintf("size=%d", size), func(t *testing.T) {
			t.Parallel()
			builder := NewAliasMapBuilder()
			pairs := make([]values.AliasPair, size)
			for i := range pairs {
				pairs[i] = values.AliasPair{
					Source: values.NamedCorrelationIdentifier(fmt.Sprintf("s%d", i)),
					Target: values.NamedCorrelationIdentifier(fmt.Sprintf("t%d", i)),
				}
				builder.Put(pairs[i].Source, pairs[i].Target)
			}
			aliases := builder.Build()
			translations := RebaseWithAliasMap(aliases)
			for _, pair := range pairs {
				if !translations.ContainsSourceAlias(pair.Source) {
					t.Fatalf("translation lost binding %v->%v", pair.Source, pair.Target)
				}
				leaf := translationMapQOV(pair.Source, values.NotNullLong)
				translated := requireTranslatedQOV(t, translations.ApplyTranslationFunction(pair.Source, leaf))
				if translated.Correlation() != pair.Target || !translated.FlowedType().Equals(values.NotNullLong) {
					t.Fatal("translation changed binding or flowed type")
				}
			}
			if translations.ContainsSourceAlias(values.NamedCorrelationIdentifier("absent")) {
				t.Fatal("translation invented an absent binding")
			}
			visited := 0
			for range aliases.entries() {
				visited++
				break
			}
			if visited != min(1, size) {
				t.Fatalf("early-stop iterator visited %d bindings, want %d", visited, min(1, size))
			}
		})
	}
}

func plannerAliasStorageIDs() []values.CorrelationIdentifier {
	return []values.CorrelationIdentifier{
		{},
		values.CurrentCorrelation(), values.NamedCorrelationIdentifier("a"),
		values.NamedCorrelationIdentifier("b"), values.NamedCorrelationIdentifier("c"),
		values.NamedCorrelationIdentifier("d"), values.NamedCorrelationIdentifier("absent"),
	}
}

func assertPlannerAliasStorage(t testing.TB, got *AliasMap, want map[values.CorrelationIdentifier]values.CorrelationIdentifier, ids []values.CorrelationIdentifier) {
	t.Helper()
	identity, validBridge := true, true
	for source, target := range want {
		identity = identity && source == target
		validBridge = validBridge && !source.IsZero() && !target.IsZero() &&
			(source == values.CurrentCorrelation()) == (target == values.CurrentCorrelation())
	}
	if got.Size() != len(want) || got.IsEmpty() != (len(want) == 0) || got.IsIdentity() != identity {
		t.Fatalf("size/empty/identity = %d/%t/%t, want %d/%t/%t", got.Size(), got.IsEmpty(), got.IsIdentity(), len(want), len(want) == 0, identity)
	}
	sources := make(map[values.CorrelationIdentifier]bool)
	for _, source := range got.Sources() {
		if _, exists := want[source]; !exists || sources[source] {
			t.Fatalf("unexpected or repeated source %v", source)
		}
		sources[source] = true
	}
	if len(sources) != len(want) {
		t.Fatalf("sources = %v, want keys of %v", sources, want)
	}
	view, err := got.ForwardMap()
	if (err == nil) != validBridge {
		t.Fatalf("ForwardMap error = %v, valid bridge = %t", err, validBridge)
	}
	for _, source := range ids {
		target, exists := want[source]
		if actual, present := got.GetTargetOrEmpty(source); actual != target || present != exists || got.ContainsSource(source) != exists {
			t.Fatalf("source %v: target/present = %v/%t, want %v/%t", source, actual, present, target, exists)
		}
		fallback := target
		if !exists {
			fallback = source
		}
		if got.GetTarget(source) != fallback {
			t.Fatalf("source %v: identity fallback lost", source)
		}
		if validBridge {
			if actual, present := view.Target(source); actual != target || present != exists {
				t.Fatalf("source %v: bridge target/present = %v/%t, want %v/%t", source, actual, present, target, exists)
			}
		}
		for _, other := range ids {
			if got.ContainsMapping(source, other) != (exists && target == other) {
				t.Fatalf("mapping %v->%v differs from model", source, other)
			}
		}
		inverse, found := source, false
		for from, to := range want {
			if to == source {
				inverse, found = from, true
			}
		}
		if got.GetSource(source) != inverse || got.ContainsTarget(source) != found {
			t.Fatalf("inverse %v: got %v/%t, want %v/%t", source, got.GetSource(source), got.ContainsTarget(source), inverse, found)
		}
	}
}

func FuzzPlannerAliasMapStorage(f *testing.F) {
	f.Add([]byte{0, 2, 3, 0, 4, 5})
	f.Add([]byte{1, 2, 3, 1, 2, 3, 1, 4, 3})
	f.Add([]byte{2, 0, 0, 3, 1, 2})
	f.Fuzz(func(t *testing.T, data []byte) {
		t.Parallel()
		ids := plannerAliasStorageIDs()
		builder := NewAliasMapBuilder()
		model := make(map[values.CorrelationIdentifier]values.CorrelationIdentifier)
		for i := 0; i+2 < min(len(data), 96); i += 3 {
			source, target := ids[int(data[i+1])%len(ids)], ids[int(data[i+2])%len(ids)]
			snapshot := builder.Build()
			previous := make(map[values.CorrelationIdentifier]values.CorrelationIdentifier, len(model))
			for from, to := range model {
				previous[from] = to
			}
			op := data[i] % 4
			old, exists := model[source]
			compatible := !exists || op%2 == 1 && old == target
			for from, to := range model {
				if to == target && (from != source || op%2 == 0) {
					compatible = false
				}
			}
			var accepted bool
			switch op {
			case 0:
				accepted = builder.Put(source, target)
			case 1:
				accepted = builder.PutChecked(source, target)
			case 2:
				builder.PutAll(AliasMapOfAliases(source, target))
				accepted = compatible
			case 3:
				accepted = builder.PutAllChecked(AliasMapOfAliases(source, target))
			}
			if accepted != compatible {
				t.Fatalf("operation %d, %v->%v: accepted = %t, want %t", op, source, target, accepted, compatible)
			}
			if compatible {
				model[source] = target
			}
			assertPlannerAliasStorage(t, snapshot, previous, ids)
			assertPlannerAliasStorage(t, builder.Build(), model, ids)
		}
	})
}

func BenchmarkPlannerAliasMapStorage(b *testing.B) {
	for _, size := range []int{0, 1, 2, 8} {
		b.Run(fmt.Sprintf("size=%d", size), func(b *testing.B) {
			builder := NewAliasMapBuilder()
			for i := range size {
				builder.Put(values.NamedCorrelationIdentifier(fmt.Sprintf("s%d", i)), values.NamedCorrelationIdentifier(fmt.Sprintf("t%d", i)))
			}
			b.ReportAllocs()
			b.ResetTimer()
			var result *AliasMap
			for range b.N {
				result = builder.Build()
			}
			b.StopTimer()
			if result == nil || result.Size() != size {
				b.Fatal("builder lost bindings")
			}
		})
	}
}
