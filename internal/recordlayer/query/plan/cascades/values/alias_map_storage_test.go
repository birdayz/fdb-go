package values

import (
	"errors"
	"fmt"
	"testing"
)

func TestAliasMapSingletonStorage(t *testing.T) {
	t.Parallel()
	a, b := NamedCorrelationIdentifier("a"), NamedCorrelationIdentifier("b")
	for _, pair := range []AliasPair{{Source: a, Target: b}, {Source: a, Target: a}, {Source: CurrentCorrelation(), Target: CurrentCorrelation()}} {
		t.Run(pair.Source.Name()+"->"+pair.Target.Name(), func(t *testing.T) {
			t.Parallel()
			pairs := []AliasPair{pair}
			aliases := mustAliasMap(t, pairs...)
			if _, ok := aliases.(*singletonAliasMap); !ok {
				t.Fatal("singleton alias map allocated hash maps")
			}
			pairs[0] = AliasPair{}
			if aliasMapEmpty(aliases) {
				t.Fatal("singleton was classified as empty")
			}
			if got, ok := aliases.Target(pair.Source); !ok || got != pair.Target {
				t.Fatalf("forward lookup = %v, %t", got, ok)
			}
			if got, ok := aliases.Source(pair.Target); !ok || got != pair.Source {
				t.Fatalf("reverse lookup = %v, %t", got, ok)
			}
			for _, missing := range []CorrelationIdentifier{{}, NamedCorrelationIdentifier("absent")} {
				if got, ok := aliases.Target(missing); ok || !got.IsZero() {
					t.Fatalf("missing forward lookup = %v, %t", got, ok)
				}
				if got, ok := aliases.Source(missing); ok || !got.IsZero() {
					t.Fatalf("missing reverse lookup = %v, %t", got, ok)
				}
			}
		})
	}
}

func TestAliasMapSingletonLayoutEquality(t *testing.T) {
	t.Parallel()
	row := NewRecordType("", false, []Field{{Name: "A", FieldType: NotNullLong}})
	a, b := NamedCorrelationIdentifier("a"), NamedCorrelationIdentifier("b")
	aliases := mustAliasMap(t, AliasPair{Source: a, Target: b})
	validated, ok := asAliasMap(aliases)
	if !ok || !layoutCorrelationEqual(a, b, validated) {
		t.Fatal("layout correlation equality ignored the singleton renaming")
	}
	layouts := make([]OrdinalLayout, 0, 2)
	for _, alias := range []CorrelationIdentifier{a, b} {
		windows := []OrdinalWindowSpec{{Source: mustQOV(t, alias, row), FieldPaths: [][]int{{0}}}}
		layout, err := NewOrdinalLayout(mustLayoutCurrentQOV(t, row), []OrdinalTileSpec{{Start: 0, Width: 1, Kind: OrdinalTileFlat}}, windows)
		if err != nil {
			t.Fatal(err)
		}
		layouts = append(layouts, layout)
	}
	if layouts[0].RawEqual(layouts[1]) || !layouts[0].EqualUnderAliases(layouts[1], aliases) {
		t.Fatal("layout window equality did not apply the singleton renaming")
	}
}

func TestAliasMapOwnedStorageAdmission(t *testing.T) {
	t.Parallel()
	a, b := NamedCorrelationIdentifier("a"), NamedCorrelationIdentifier("b")
	singleton := mustAliasMap(t, AliasPair{Source: a, Target: b})
	for _, invalid := range []AliasMap{
		(*aliasMap)(nil), (*singletonAliasMap)(nil),
		&embeddedLayoutAliasMap{AliasMap: singleton},
	} {
		if _, ok := asAliasMap(invalid); ok || aliasMapEmpty(invalid) {
			t.Fatalf("invalid %T alias map was admitted", invalid)
		}
		if got, compatible, err := ExtendAliasMap(invalid, nil); got != nil || compatible || err == nil {
			t.Fatalf("invalid %T extension = %v, %t, %v", invalid, got, compatible, err)
		}
	}
	for _, empty := range []AliasMap{nil, EmptyAliasMap()} {
		if !aliasMapEmpty(empty) {
			t.Fatalf("empty %T alias map was rejected", empty)
		}
		got, compatible, err := ExtendAliasMap(empty, []AliasPair{{Source: a, Target: b}})
		if err != nil || !compatible {
			t.Fatalf("extend empty %T: %v", empty, err)
		}
		if target, ok := got.Target(a); !ok || target != b {
			t.Fatal("extending empty map lost singleton binding")
		}
	}
}

func FuzzAliasMapStorage(f *testing.F) {
	f.Add([]byte{}, byte(0))
	f.Add([]byte{1, 2}, byte(1))
	f.Add([]byte{1, 2, 2, 1}, byte(1))
	f.Add([]byte{1, 2, 1, 2}, byte(1))
	f.Add([]byte{1, 2, 3, 2}, byte(1))
	f.Add([]byte{4, 4}, byte(1))
	f.Fuzz(func(t *testing.T, data []byte, split byte) {
		t.Parallel()
		ids := []CorrelationIdentifier{{}, NamedCorrelationIdentifier("a"), NamedCorrelationIdentifier("b"), NamedCorrelationIdentifier("c"), CurrentCorrelation()}
		count := min(len(data)/2, 8)
		pairs := make([]AliasPair, count)
		for i := range pairs {
			pairs[i] = AliasPair{Source: ids[int(data[2*i])%len(ids)], Target: ids[int(data[2*i+1])%len(ids)]}
		}
		cut := int(split) % (count + 1)
		basePairs, added := pairs[:cut], pairs[cut:]
		model, code := aliasMapModel(basePairs)
		base, err := NewAliasMap(basePairs)
		assertAliasMapModel(t, base, err, model, code, ids)
		if err != nil {
			return
		}
		all := append([]AliasPair(nil), basePairs...)
		compatible := true
		for _, pair := range added {
			if target, exists := model[pair.Source]; exists && target != pair.Target {
				compatible = false
				break
			}
			for source, target := range model {
				if target == pair.Target && source != pair.Source {
					compatible = false
				}
			}
			if !compatible {
				break
			}
			if target, exists := model[pair.Source]; exists && target == pair.Target {
				continue
			}
			all = append(all, pair)
		}
		got, gotCompatible, gotErr := ExtendAliasMap(base, added)
		assertAliasMapModel(t, base, nil, model, 0, ids)
		if !compatible {
			if got != base || gotCompatible || gotErr != nil {
				t.Fatalf("conflict changed base or became an error: %v, %t, %v", got, gotCompatible, gotErr)
			}
			return
		}
		want, code := aliasMapModel(all)
		if gotCompatible != (code == 0) {
			t.Fatalf("extension compatible=%t, want %t", gotCompatible, code == 0)
		}
		assertAliasMapModel(t, got, gotErr, want, code, ids)
	})
}

func aliasMapModel(pairs []AliasPair) (map[CorrelationIdentifier]CorrelationIdentifier, ResolutionErrorCode) {
	forward := make(map[CorrelationIdentifier]CorrelationIdentifier)
	reverse := make(map[CorrelationIdentifier]CorrelationIdentifier)
	for _, pair := range pairs {
		if pair.Source.IsZero() || pair.Target.IsZero() {
			return nil, CorrelationZero
		}
		if pair.Source == CurrentCorrelation() && pair.Target != CurrentCorrelation() || pair.Target == CurrentCorrelation() && pair.Source != CurrentCorrelation() {
			return nil, CorrelationKindMismatch
		}
		if _, exists := forward[pair.Source]; exists {
			return nil, CorrelationTypeConflict
		}
		if _, exists := reverse[pair.Target]; exists {
			return nil, CorrelationTypeConflict
		}
		forward[pair.Source] = pair.Target
		reverse[pair.Target] = pair.Source
	}
	return forward, 0
}

func assertAliasMapModel(t testing.TB, got AliasMap, err error, want map[CorrelationIdentifier]CorrelationIdentifier, code ResolutionErrorCode, ids []CorrelationIdentifier) {
	t.Helper()
	if code != 0 {
		var coded interface{ Code() ResolutionErrorCode }
		if got != nil || !errors.As(err, &coded) || coded.Code() != code {
			t.Fatalf("invalid map = %v, %v; want code %v", got, err, code)
		}
		return
	}
	if err != nil || got == nil || aliasMapEmpty(got) != (len(want) == 0) {
		t.Fatalf("map = %v, %v; want %v", got, err, want)
	}
	for _, id := range ids {
		target, targetExists := want[id]
		if value, exists := got.Target(id); value != target || exists != targetExists {
			t.Fatalf("forward[%v] = %v, %t; want %v, %t", id, value, exists, target, targetExists)
		}
		var source CorrelationIdentifier
		sourceExists := false
		for from, to := range want {
			if to == id {
				source, sourceExists = from, true
			}
		}
		if value, exists := got.Source(id); value != source || exists != sourceExists {
			t.Fatalf("reverse[%v] = %v, %t; want %v, %t", id, value, exists, source, sourceExists)
		}
	}
}

func BenchmarkAliasMapStorage(b *testing.B) {
	for _, size := range []int{0, 1, 2, 8} {
		b.Run(fmt.Sprintf("pairs=%d", size), func(b *testing.B) {
			pairs := make([]AliasPair, size)
			for i := range pairs {
				pairs[i] = AliasPair{Source: NamedCorrelationIdentifier(fmt.Sprintf("s%d", i)), Target: NamedCorrelationIdentifier(fmt.Sprintf("t%d", i))}
			}
			b.ReportAllocs()
			b.ResetTimer()
			var aliases AliasMap
			for i := 0; i < b.N; i++ {
				var err error
				aliases, err = NewAliasMap(pairs)
				if err != nil {
					b.Fatal(err)
				}
			}
			b.StopTimer()
			for _, pair := range pairs {
				if got, ok := aliases.Target(pair.Source); !ok || got != pair.Target {
					b.Fatal("map lost pairing")
				}
			}
		})
	}
}
