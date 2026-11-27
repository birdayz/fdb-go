package values

import (
	"fmt"
	"reflect"
	"testing"
)

func TestRebuildFieldValueReusesExactPath(t *testing.T) {
	t.Parallel()
	for _, nullable := range []bool{false, true} {
		for _, pinned := range []bool{false, true} {
			for _, path := range [][]int{{0}, {0, 1}, {1}} {
				t.Run(fmt.Sprintf("nullable=%t/pinned=%t/path=%v", nullable, pinned, path), func(t *testing.T) {
					t.Parallel()
					nested := &RecordType{RecordName: "Nested", Fields: []Field{
						{Name: "DUP", Ordinal: 0, FieldType: NotNullString},
						{Name: "DUP", Ordinal: 1, FieldType: NotNullLong},
					}}
					row := NewRecordType("Root", nullable, []Field{
						{Name: "N", FieldType: nested},
						{Name: "ITEMS", FieldType: NewArrayType(false, nested)},
					})
					source := mustLayoutCurrentQOV(t, row)
					target := mustLayoutCurrentQOV(t, row)
					value, err := ResolveFieldOrdinals(source, path)
					if err != nil {
						t.Fatal(err)
					}
					if pinned {
						value, err = PinValueToExactFrontier(value, source)
						if err != nil {
							t.Fatal(err)
						}
					}
					original := mustReanchorField(t, value)
					rebuilt, err := RebuildFieldValue(original, target)
					if err != nil {
						t.Fatal(err)
					}
					got := mustReanchorField(t, rebuilt)
					if got.Path() != original.Path() {
						t.Fatal("rebuilding on the same exact type re-resolved an immutable field path")
					}
					if got.ChildValue() != target || original.ChildValue() != source {
						t.Fatal("rebuild lost target identity or mutated source identity")
					}
					fresh, err := ResolveFieldOrdinals(target, path)
					if err != nil {
						t.Fatal(err)
					}
					assertRebuiltFieldMatchesResolution(t, got, mustReanchorField(t, fresh), pinned)
					ordinals := got.Path().Ordinals()
					ordinals[0] = 99
					if !reflect.DeepEqual(original.Path().Ordinals(), path) || !reflect.DeepEqual(got.Path().Ordinals(), path) {
						t.Fatal("ordinal read view mutated a shared path")
					}
					pinnedValue, err := PinValueToExactFrontier(rebuilt, target)
					if err != nil || !mustReanchorField(t, pinnedValue).Path().IsFrontierPinned() {
						t.Fatalf("pin rebuilt field: %v", err)
					}
					if got.Path().IsFrontierPinned() != pinned || original.Path().IsFrontierPinned() != pinned {
						t.Fatal("pinning rebuilt field mutated a shared path")
					}
				})
			}
		}
	}
}

func assertRebuiltFieldMatchesResolution(t testing.TB, got, want FieldValue, pinned bool) {
	t.Helper()
	if got.ChildValue() != want.ChildValue() || !got.Type().Equals(want.Type()) ||
		got.DisplayName() != want.DisplayName() || got.Path().RootDomain() != want.Path().RootDomain() ||
		got.Path().IsFrontierPinned() != pinned || !reflect.DeepEqual(got.Path().Ordinals(), want.Path().Ordinals()) ||
		!SemanticEqualsUnderAliasMap(got, want, EmptyAliasMap()) || SemanticHashCode(got) != SemanticHashCode(want) {
		t.Fatalf("rebuilt field differs from fresh resolution: got %v, want %v (pinned=%t)", got, want, pinned)
	}
	if !reflect.DeepEqual(got.ResultType(), want.ResultType()) {
		t.Fatalf("rebuild retained stale nominal result metadata: got %v, want %v", got.ResultType(), want.ResultType())
	}
	for i := 0; i < want.Path().Len(); i++ {
		gotAccessor, _ := got.Path().Accessor(i)
		wantAccessor, _ := want.Path().Accessor(i)
		gotName, gotNamed := gotAccessor.DisplayName()
		wantName, wantNamed := wantAccessor.DisplayName()
		if gotName != wantName || gotNamed != wantNamed || !reflect.DeepEqual(gotAccessor.FieldType(), wantAccessor.FieldType()) {
			t.Fatalf("accessor %d retained stale metadata", i)
		}
	}
}

func TestRebuildFieldValueRejectsMalformedChild(t *testing.T) {
	t.Parallel()
	row := NewRecordType("", false, []Field{{Name: "A", FieldType: NotNullLong}})
	source := mustQOV(t, NamedCorrelationIdentifier("source"), row)
	value, err := ResolveFieldOrdinals(source, []int{0})
	if err != nil {
		t.Fatal(err)
	}
	for _, child := range []Value{
		nil, (*quantifiedObjectValue)(nil), &quantifiedObjectValue{},
		&quantifiedObjectValue{flowed: source.flowed},
		&struct{ QuantifiedObjectValue }{source},
	} {
		if got, err := RebuildFieldValue(mustReanchorField(t, value), child); got != nil || err == nil {
			t.Fatalf("malformed %T child admitted: %v, %v", child, got, err)
		}
	}
}

func TestRebuildFieldValueRecomputesLegacyDomain(t *testing.T) {
	t.Parallel()
	row := NewRecordType("", false, []Field{{Name: "A", FieldType: NotNullLong}})
	source := mustQOV(t, NamedCorrelationIdentifier("source"), row)
	target := mustQOV(t, NamedCorrelationIdentifier("target"), row)
	legacyDomain := OrdinalDomainOfColumnNames([]string{"LEGACY"})
	original, err := resolveFieldOrdinalInDomain(source, 0, legacyDomain)
	if err != nil {
		t.Fatal(err)
	}
	rebuilt, err := RebuildFieldValue(mustReanchorField(t, original), target)
	if err != nil {
		t.Fatal(err)
	}
	if mustReanchorField(t, rebuilt).Path().RootDomain() != OrdinalDomainOfType(row) ||
		mustReanchorField(t, original).Path().RootDomain() != legacyDomain {
		t.Fatal("rebuild kept a stale domain or changed its source")
	}
}

func FuzzRebuildFieldValueMatchesResolution(f *testing.F) {
	for mode := byte(0); mode < 8; mode++ {
		f.Add(byte(2), byte(0), mode)
		f.Add(byte(3), byte(255), mode)
	}
	f.Fuzz(func(t *testing.T, depthByte, flags, mode byte) {
		t.Parallel()
		depth := int(depthByte%4) + 1
		path := make([]int, depth)
		var sourceType, targetType Type = NotNullLong, NotNullLong
		if mode%8 == 3 {
			targetType = NotNullString
		}
		for i := depth - 1; i >= 0; i-- {
			ordinal := int(flags>>i) & 1
			path[i] = ordinal
			sourceFields := []Field{{Name: "A", FieldType: NotNullLong}, {Name: "B", FieldType: NullableString}}
			targetFields := append([]Field(nil), sourceFields...)
			sourceFields[ordinal].FieldType = sourceType
			targetFields[ordinal].FieldType = targetType
			nullable := flags>>(i+4)&1 != 0
			targetNullable := nullable
			targetName := "Row"
			if mode%8 == 7 && i == depth-1 {
				targetName = "OtherNestedRow"
			}
			if i == 0 {
				switch mode % 8 {
				case 1:
					targetNullable = !nullable
				case 2:
					targetFields[ordinal].Name = "RENAMED"
				case 5:
					targetFields = nil
				case 6:
					targetName = "OtherRoot"
				}
			}
			sourceType = NewRecordType("Row", nullable, sourceFields)
			targetType = NewRecordType(targetName, targetNullable, targetFields)
		}
		if mode%8 == 4 {
			targetType = NotNullLong
		}
		source := mustLayoutCurrentQOV(t, sourceType)
		target := mustQOV(t, NamedCorrelationIdentifier("target"), targetType)
		original, err := ResolveFieldOrdinals(source, path)
		if err != nil {
			t.Fatal(err)
		}
		pinned := flags&1 != 0
		if pinned {
			original, err = PinValueToExactFrontier(original, source)
			if err != nil {
				t.Fatal(err)
			}
		}
		got, gotErr := RebuildFieldValue(mustReanchorField(t, original), target)
		want, wantErr := ResolveFieldOrdinals(target, path)
		if wantErr != nil || !original.Type().Equals(want.Type()) {
			if got != nil || gotErr == nil {
				t.Fatalf("incompatible replacement admitted: %v, %v", got, gotErr)
			}
			return
		}
		if gotErr != nil {
			t.Fatal(gotErr)
		}
		assertRebuiltFieldMatchesResolution(t, mustReanchorField(t, got), mustReanchorField(t, want), pinned)
	})
}

func BenchmarkRebuildFieldValue(b *testing.B) {
	for _, depth := range []int{1, 4} {
		b.Run(fmt.Sprintf("depth=%d", depth), func(b *testing.B) {
			var row Type = NotNullLong
			for range depth {
				row = NewRecordType("Row", false, []Field{{Name: "A", FieldType: row}})
			}
			source := mustQOV(b, NamedCorrelationIdentifier("source"), row)
			target := mustQOV(b, NamedCorrelationIdentifier("target"), row)
			value, err := ResolveFieldOrdinals(source, make([]int, depth))
			if err != nil {
				b.Fatal(err)
			}
			field := mustReanchorField(b, value)
			b.ReportAllocs()
			b.ResetTimer()
			var rebuilt Value
			for i := 0; i < b.N; i++ {
				rebuilt, err = RebuildFieldValue(field, target)
				if err != nil {
					b.Fatal(err)
				}
			}
			b.StopTimer()
			if mustReanchorField(b, rebuilt).ChildValue() != target {
				b.Fatal("rebuild lost target")
			}
		})
	}
}
