package values

import "testing"

// RFC-176 P1 pins: the Go-only vector/HNSW config on the windowed row-number
// family joins value identity — EqualsWithoutChildren AND writeSemanticHash in
// lockstep, same discriminator set:
//
//	DistanceRowNumberValue:  Metric, EfSearch, IsReturningVectors
//	RowNumberValue:          EfSearch, IsReturningVectors
//	RowNumberHighOrderValue: EfSearch, IsReturningVectors
//
// Pointer fields: nil==nil, nil≠&v, otherwise pointee equality.
//
// Red→green (RFC-176 §2): pre-fix, two DistanceRowNumberValues with different
// Metric compared EQUAL (type-assertion-only arm) yet HASHED APART (the generic
// "v:"+Name() bucket embeds the Metric via Name()) — hash finer than equality,
// a live equal⟹same-hash memo violation. TestRFC176_EqualImpliesSameHash_Family
// and TestRFC176_DistanceRowNumber_MetricJoinsIdentity fail on the pre-fix arms.

func intPtr(i int) *int { return &i }

// mkDist builds a DistanceRowNumberValue with no children so only the
// non-child attributes vary.
func mkDist(metric DistanceOperator, ef *int, rv *bool) *DistanceRowNumberValue {
	return NewDistanceRowNumberValue(metric, nil, nil, ef, rv)
}

func mkRowNum(ef *int, rv *bool) *RowNumberValue {
	return NewRowNumberValue(nil, nil, ef, rv)
}

// assertIdentityDiffers pins the differ-by-one-field contract: not equal AND
// (hash intent) different semantic hash.
func assertIdentityDiffers(t *testing.T, label string, a, b Value) {
	t.Helper()
	if EqualsWithoutChildren(a, b) {
		t.Errorf("%s: EqualsWithoutChildren = true, want false", label)
	}
	if EqualsWithoutChildren(b, a) {
		t.Errorf("%s (swapped): EqualsWithoutChildren = true, want false", label)
	}
	if SemanticHashCode(a) == SemanticHashCode(b) {
		t.Errorf("%s: SemanticHashCode collides — the discriminator must join the hash", label)
	}
}

// assertIdentitySame pins the equal-config contract: equal AND same hash.
func assertIdentitySame(t *testing.T, label string, a, b Value) {
	t.Helper()
	if !EqualsWithoutChildren(a, b) {
		t.Errorf("%s: EqualsWithoutChildren = false, want true", label)
	}
	if SemanticHashCode(a) != SemanticHashCode(b) {
		t.Errorf("%s: equal values must have equal SemanticHashCode", label)
	}
}

func TestRFC176_DistanceRowNumber_MetricJoinsIdentity(t *testing.T) {
	t.Parallel()
	// The §2 violation case: pre-fix these compared EQUAL (type-only arm) but
	// hashed apart (Name() embeds the metric). Post-fix: unequal, distinct hash.
	a := mkDist(DistanceCosine, nil, nil)
	b := mkDist(DistanceEuclidean, nil, nil)
	assertIdentityDiffers(t, "Metric cosine vs euclidean", a, b)
}

func TestRFC176_DistanceRowNumber_PerField(t *testing.T) {
	t.Parallel()
	base := func() *DistanceRowNumberValue { return mkDist(DistanceCosine, intPtr(200), boolPtr(true)) }

	assertIdentitySame(t, "identical config", base(), base())

	differ := base()
	differ.Metric = DistanceDotProduct
	assertIdentityDiffers(t, "differ by Metric", base(), differ)

	differ = base()
	differ.EfSearch = intPtr(100)
	assertIdentityDiffers(t, "differ by EfSearch pointee", base(), differ)

	differ = base()
	differ.EfSearch = nil
	assertIdentityDiffers(t, "differ by EfSearch nil vs &200", base(), differ)

	differ = base()
	differ.IsReturningVectors = boolPtr(false)
	assertIdentityDiffers(t, "differ by IsReturningVectors pointee", base(), differ)

	differ = base()
	differ.IsReturningVectors = nil
	assertIdentityDiffers(t, "differ by IsReturningVectors nil vs &true", base(), differ)
}

func TestRFC176_DistanceRowNumber_PointerSemantics(t *testing.T) {
	t.Parallel()
	// nil == nil.
	assertIdentitySame(t, "both nil config", mkDist(DistanceCosine, nil, nil), mkDist(DistanceCosine, nil, nil))
	// Distinct pointers, same pointee: pointee equality, not pointer identity.
	assertIdentitySame(t, "distinct pointers same pointee",
		mkDist(DistanceCosine, intPtr(200), boolPtr(true)),
		mkDist(DistanceCosine, intPtr(200), boolPtr(true)))
}

func TestRFC176_RowNumber_PerField(t *testing.T) {
	t.Parallel()
	base := func() *RowNumberValue { return mkRowNum(intPtr(200), boolPtr(true)) }

	assertIdentitySame(t, "identical config", base(), base())
	assertIdentitySame(t, "both nil config", mkRowNum(nil, nil), mkRowNum(nil, nil))

	differ := base()
	differ.EfSearch = intPtr(100)
	assertIdentityDiffers(t, "differ by EfSearch pointee", base(), differ)

	differ = base()
	differ.EfSearch = nil
	assertIdentityDiffers(t, "differ by EfSearch nil vs &200", base(), differ)

	differ = base()
	differ.IsReturningVectors = boolPtr(false)
	assertIdentityDiffers(t, "differ by IsReturningVectors pointee", base(), differ)

	differ = base()
	differ.IsReturningVectors = nil
	assertIdentityDiffers(t, "differ by IsReturningVectors nil vs &true", base(), differ)
}

func TestRFC176_RowNumberHighOrder_PerField(t *testing.T) {
	t.Parallel()
	base := func() *RowNumberHighOrderValue { return NewRowNumberHighOrderValue(intPtr(200), boolPtr(true)) }

	assertIdentitySame(t, "identical config", base(), base())
	assertIdentitySame(t, "both nil config",
		NewRowNumberHighOrderValue(nil, nil), NewRowNumberHighOrderValue(nil, nil))

	differ := base()
	differ.EfSearch = intPtr(100)
	assertIdentityDiffers(t, "differ by EfSearch pointee", base(), differ)

	differ = base()
	differ.EfSearch = nil
	assertIdentityDiffers(t, "differ by EfSearch nil vs &200", base(), differ)

	differ = base()
	differ.IsReturningVectors = boolPtr(false)
	assertIdentityDiffers(t, "differ by IsReturningVectors pointee", base(), differ)

	differ = base()
	differ.IsReturningVectors = nil
	assertIdentityDiffers(t, "differ by IsReturningVectors nil vs &true", base(), differ)
}

// TestRFC176_TypeOnlyArms_StayTypeOnly pins the Java-consistent arms: RankValue
// and the four metric specialisations carry no non-child attributes (RFC-176
// §2 — WindowedValue class+name identity), so same-type instances are equal
// and hash alike, and cross-type instances are unequal.
func TestRFC176_TypeOnlyArms_StayTypeOnly(t *testing.T) {
	t.Parallel()
	variants := []Value{
		NewCosineDistanceRowNumberValue(nil, nil),
		NewDotProductDistanceRowNumberValue(nil, nil),
		NewEuclideanDistanceRowNumberValue(nil, nil),
		NewEuclideanSquareDistanceRowNumberValue(nil, nil),
		NewRankValue(nil),
	}
	same := []Value{
		NewCosineDistanceRowNumberValue(nil, nil),
		NewDotProductDistanceRowNumberValue(nil, nil),
		NewEuclideanDistanceRowNumberValue(nil, nil),
		NewEuclideanSquareDistanceRowNumberValue(nil, nil),
		NewRankValue(nil),
	}
	for i := range variants {
		assertIdentitySame(t, "same-type metric/rank variant", variants[i], same[i])
		for j := range variants {
			if i == j {
				continue
			}
			if EqualsWithoutChildren(variants[i], variants[j]) {
				t.Errorf("cross-type %T vs %T: EqualsWithoutChildren = true, want false",
					variants[i], variants[j])
			}
		}
	}
}

// TestRFC176_EqualImpliesSameHash_Family is the equal⟹same-hash property test
// over the whole windowed/vector family (RFC-176 §6 P1). Pre-fix it fails on
// the DistanceRowNumberValue Metric case: type-only equality called
// different-metric values equal while the generic hash bucket (Name() embeds
// the metric) hashed them apart.
func TestRFC176_EqualImpliesSameHash_Family(t *testing.T) {
	t.Parallel()
	efs := []*int{nil, intPtr(100), intPtr(200)}
	rvs := []*bool{nil, boolPtr(false), boolPtr(true)}
	metrics := []DistanceOperator{
		DistanceEuclidean, DistanceEuclideanSquare, DistanceCosine, DistanceDotProduct,
	}

	var family []Value
	for _, m := range metrics {
		for _, ef := range efs {
			for _, rv := range rvs {
				family = append(family, mkDist(m, ef, rv))
			}
		}
	}
	for _, ef := range efs {
		for _, rv := range rvs {
			family = append(family, mkRowNum(ef, rv))
			family = append(family, NewRowNumberHighOrderValue(ef, rv))
		}
	}
	family = append(family,
		NewCosineDistanceRowNumberValue(nil, nil),
		NewDotProductDistanceRowNumberValue(nil, nil),
		NewEuclideanDistanceRowNumberValue(nil, nil),
		NewEuclideanSquareDistanceRowNumberValue(nil, nil),
		NewRankValue(nil),
	)

	for i, a := range family {
		for j, b := range family {
			if EqualsWithoutChildren(a, b) && SemanticHashCode(a) != SemanticHashCode(b) {
				t.Errorf("family[%d] (%T) and family[%d] (%T): equal but hash apart — equal⟹same-hash violated",
					i, a, j, b)
			}
		}
	}
}
