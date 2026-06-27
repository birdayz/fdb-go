package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

func FuzzRichOrdering_Satisfies(f *testing.F) {
	f.Add(uint8(1), uint8(0), uint8(1), uint8(0))
	f.Add(uint8(2), uint8(1), uint8(1), uint8(1))
	f.Add(uint8(3), uint8(2), uint8(2), uint8(2))
	f.Add(uint8(0), uint8(0), uint8(0), uint8(0))

	f.Fuzz(func(t *testing.T, numKeys uint8, numReqParts uint8, bindKind uint8, reqSort uint8) {
		if numKeys > 5 {
			numKeys = 5
		}
		if numReqParts > numKeys {
			numReqParts = numKeys
		}

		keys := make([]values.Value, numKeys)
		bm := make(map[values.Value][]OrderingBinding, numKeys)
		for i := range keys {
			keys[i] = &values.FieldValue{Field: string(rune('a' + i)), Typ: values.UnknownType}
			switch bindKind % 3 {
			case 0:
				bm[keys[i]] = []OrderingBinding{SortedBinding(ProvidedSortOrderAscending)}
			case 1:
				bm[keys[i]] = []OrderingBinding{SortedBinding(ProvidedSortOrderDescending)}
			case 2:
				bm[keys[i]] = []OrderingBinding{FixedBinding("eq")}
			}
		}

		o := NewRichOrdering(bm, keys, false)

		parts := make([]RequestedOrderingPart, numReqParts)
		for i := range parts {
			var so RequestedSortOrder
			switch reqSort % 3 {
			case 0:
				so = RequestedSortOrderAny
			case 1:
				so = RequestedSortOrderAscending
			case 2:
				so = RequestedSortOrderDescending
			}
			parts[i] = RequestedOrderingPart{Value: keys[i], SortOrder: so}
		}
		req := NewRequestedOrdering(parts, DistinctnessNotDistinct, false)

		o.Satisfies(req)
	})
}

func FuzzMergeOrderings_NoPanic(f *testing.F) {
	f.Add(uint8(2), uint8(2), uint8(0), uint8(0))
	f.Add(uint8(0), uint8(0), uint8(1), uint8(1))
	f.Add(uint8(3), uint8(1), uint8(2), uint8(2))

	f.Fuzz(func(t *testing.T, numA uint8, numB uint8, bindA uint8, bindB uint8) {
		if numA > 5 {
			numA = 5
		}
		if numB > 5 {
			numB = 5
		}

		keysA := make([]values.Value, numA)
		bmA := make(map[values.Value][]OrderingBinding, numA)
		for i := range keysA {
			keysA[i] = &values.FieldValue{Field: string(rune('a' + i)), Typ: values.UnknownType}
			switch bindA % 2 {
			case 0:
				bmA[keysA[i]] = []OrderingBinding{SortedBinding(ProvidedSortOrderAscending)}
			case 1:
				bmA[keysA[i]] = []OrderingBinding{FixedBinding("eq")}
			}
		}
		oA := NewRichOrdering(bmA, keysA, false)

		keysB := make([]values.Value, numB)
		bmB := make(map[values.Value][]OrderingBinding, numB)
		for i := range keysB {
			keysB[i] = &values.FieldValue{Field: string(rune('a' + i)), Typ: values.UnknownType}
			switch bindB % 2 {
			case 0:
				bmB[keysB[i]] = []OrderingBinding{SortedBinding(ProvidedSortOrderAscending)}
			case 1:
				bmB[keysB[i]] = []OrderingBinding{FixedBinding("eq")}
			}
		}
		oB := NewRichOrdering(bmB, keysB, false)

		result := MergeOrderings(oA, oB)
		if result == nil {
			t.Fatal("MergeOrderings should never return nil")
		}
	})
}
