package values

import (
	"sync"
	"testing"
)

func internTestRecord(name string, nullable bool, fieldName string, fieldType Type) *RecordType {
	return &RecordType{
		RecordName: name,
		Nullable:   nullable,
		Fields:     []Field{{Name: fieldName, Ordinal: 0, FieldType: fieldType}},
	}
}

// TestCompositeExactTypesAreShared pins that a snapshot of an equal composite
// type returns the SAME object, for every composite kind. The lookup runs
// before the node is built, so a hit allocates nothing at all — which is the
// entire reason the interner exists: over the pure-planner sweep,
// snapshotExactType ran 243.8 million times and produced 170 distinct types.
func TestCompositeExactTypesAreShared(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		build func() Type
	}{
		{"record", func() Type {
			return internTestRecord("SHARED_R", false, "ID", NewPrimitiveType(TypeCodeLong, false))
		}},
		{"nested record", func() Type {
			return internTestRecord("SHARED_OUTER", false, "INNER",
				internTestRecord("SHARED_INNER", true, "X", NewPrimitiveType(TypeCodeString, true)))
		}},
		{"array", func() Type {
			return &ArrayType{ElementType: NewPrimitiveType(TypeCodeDouble, false)}
		}},
		{"enum", func() Type {
			return &EnumType{EnumName: "SHARED_E", Values: []EnumValue{{Name: "A", Number: 1}}}
		}},
		{"relation", func() Type {
			return &RelationType{InnerType: internTestRecord("SHARED_REL", false, "ID",
				NewPrimitiveType(TypeCodeLong, false))}
		}},
		{"any record", func() Type { return NewAnyRecordType(false) }},
	}
	for _, tc := range cases {
		first, err := SnapshotExactType(tc.build())
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		second, err := SnapshotExactType(tc.build())
		if err != nil {
			t.Fatalf("%s (again): %v", tc.name, err)
		}
		if first != second {
			t.Errorf("%s: two snapshots of an equal type produced two objects; "+
				"the probe does not match, so every snapshot of this kind rebuilds "+
				"the node and its canonical encoding", tc.name)
		}
	}
}

// TestExactInterningKeepsRecordNamesApart is the other half, and the one that
// would go wrong silently. Canonical identity EXCLUDES the record name (Java's
// Type.Record equality does), so a interner keyed on canonical identity would
// collapse these two — and thaw() rebuilds RecordName from the node, so one of
// them would then report the other's name from Type().
func TestExactInterningKeepsRecordNamesApart(t *testing.T) {
	t.Parallel()

	long := NewPrimitiveType(TypeCodeLong, false)
	left, err := SnapshotExactType(internTestRecord("NAME_LEFT", false, "ID", long))
	if err != nil {
		t.Fatalf("left: %v", err)
	}
	right, err := SnapshotExactType(internTestRecord("NAME_RIGHT", false, "ID", long))
	if err != nil {
		t.Fatalf("right: %v", err)
	}
	if left == right {
		t.Fatal("two records differing only in name were interned as one object; " +
			"Type() would then report the wrong RecordName for one of them")
	}
	// They ARE canonically equal — that is why this needed its own guard.
	if !exactTypesEqual(left.(*exactType), right.(*exactType)) {
		t.Error("the two records are not canonically equal, so this test is not " +
			"exercising the name axis it was written for")
	}
	if got := left.Type().(*RecordType).RecordName; got != "NAME_LEFT" {
		t.Errorf("left thaws as %q, want NAME_LEFT", got)
	}
	if got := right.Type().(*RecordType).RecordName; got != "NAME_RIGHT" {
		t.Errorf("right thaws as %q, want NAME_RIGHT", got)
	}
}

// TestInterningRechecksUnderTheWriteLock pins the second lookup in
// storeInternedExactType, DETERMINISTICALLY.
//
// The re-check exists for a race, and the concurrent test below does detect its
// removal — but only at a SCHEDULER-DEPENDENT rate, so the rate is not a number to
// compare against. With the re-check deleted it measured 10 RED / 12 runs on one
// machine and 6 RED / 12 on another; the control with the re-check present is
// 0 RED / 12 on both, which is what attributes those reds to the mutation rather
// than to baseline flakiness. Do not read a lower rate elsewhere as a regression.
//
// Either way it is a detector, not a gate: the greens are runs where the scheduler
// let only one goroutine miss under the read lock, and a pointer-identity invariant
// that reports correct in a third to a sixth of runs is the flake class this repo
// treats as a latent bug rather than a safety net.
//
// So drive the miss path directly as well. Calling storeInternedExactType twice
// with equal probes IS the interleaving the re-check handles: the first stores,
// the second arrives as though it had just missed under the read lock. No
// scheduler is involved, so it is 12/12.
//
// Without the re-check the second call builds a second equal node and appends
// it, and its caller walks away holding an object no later lookup returns
// again. Nothing is a wrong TYPE, so nothing fails — children compare by
// pointer, so every composite built over the loser silently stops sharing.
func TestInterningRechecksUnderTheWriteLock(t *testing.T) {
	t.Parallel()

	source := internTestRecord("RECHECK_R", false, "ID", NewPrimitiveType(TypeCodeLong, false))
	child, err := SnapshotExactType(source.Fields[0].FieldType)
	if err != nil {
		t.Fatalf("child snapshot: %v", err)
	}
	newProbe := func() *exactProbe {
		return &exactProbe{
			code:      TypeCodeRecord,
			name:      source.RecordName,
			srcFields: source.Fields,
			children:  []*exactType{child.(*exactType)},
		}
	}
	build := func() *exactType {
		return &exactType{
			code: TypeCodeRecord,
			name: source.RecordName,
			fields: []exactField{{
				name: source.Fields[0].Name,
				typ:  child.(*exactType),
			}},
		}
	}

	first := newProbe()
	hash := first.internHash()
	shard := &exactInterned[hash%exactInternShards]
	stored := storeInternedExactType(shard, hash, first, build)
	if stored == nil {
		t.Fatal("the first store produced nothing")
	}

	// Non-vacuity: the bucket must actually hold it, or the second call below
	// would be reaching the build path for a reason this test does not name.
	shard.mu.RLock()
	depth := len(shard.buckets[hash])
	shard.mu.RUnlock()
	if depth != 1 {
		t.Fatalf("bucket depth after one store = %d, want 1", depth)
	}

	second := storeInternedExactType(shard, hash, newProbe(), func() *exactType {
		t.Error("the write-lock re-check was skipped: an equal node was already in " +
			"the bucket and the store built a second one anyway")
		return build()
	})
	if second != stored {
		t.Error("the miss path admitted two objects for one type; children compare by " +
			"POINTER, so every composite over the loser stops sharing — and neither " +
			"node is a wrong type, so nothing else fails")
	}
	shard.mu.RLock()
	depth = len(shard.buckets[hash])
	shard.mu.RUnlock()
	if depth != 1 {
		t.Errorf("bucket depth after a second equal store = %d, want 1", depth)
	}
}

// TestExactInterningConvergesUnderConcurrency drives the double-checked store
// under -race. The planner snapshots from many goroutines, so the failure it
// guards is a torn table or two winners for one type.
//
// It is a DETECTOR, not the gate. Deleting the write-lock re-check reddens it at a
// SCHEDULER-DEPENDENT rate — 10 of 12 runs on one machine, 6 of 12 on another, both
// under -race at goroutines=16, against a 0-of-12 control with the re-check present.
// The greens are runs where only one goroutine missed under the read lock, so no
// duplicate was ever appended. A lower rate on other hardware is not a regression;
// the rate is a property of the scheduler, not of the code. This is why
// TestInterningRechecksUnderTheWriteLock exists — it is the deterministic half.
func TestExactInterningConvergesUnderConcurrency(t *testing.T) {
	t.Parallel()

	const goroutines = 16
	results := make([]ExactTypeHandle, goroutines)
	var start sync.WaitGroup
	var done sync.WaitGroup
	start.Add(1)
	for i := range results {
		done.Add(1)
		go func(slot int) {
			defer done.Done()
			start.Wait()
			handle, err := SnapshotExactType(internTestRecord(
				"CONCURRENT_R", false, "ID", NewPrimitiveType(TypeCodeLong, false)))
			if err != nil {
				t.Errorf("goroutine %d: %v", slot, err)
				return
			}
			results[slot] = handle
		}(i)
	}
	start.Done()
	done.Wait()
	for i := 1; i < len(results); i++ {
		if results[i] != results[0] {
			t.Fatalf("goroutine %d got a different object than goroutine 0; "+
				"the double-checked store admitted two winners for one type", i)
		}
	}
}
