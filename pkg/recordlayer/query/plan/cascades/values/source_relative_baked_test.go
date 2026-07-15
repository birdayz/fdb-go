package values

import "testing"

// TestSourceRelativeBaked pins the provenance discriminator the translator's
// rebase/collection/safety-net walks key on: a SINGLE-accessor UNPINNED path
// (the resolver's construction-time source bind) must be treated like a lazy
// reference — rebind/count/flag it — while machinery-owned nodes
// (FrontierPinned box ofOrdinals, multi-accessor paths) are final.
func TestSourceRelativeBaked(t *testing.T) {
	t.Parallel()

	if (&FieldValue{Field: "A"}).SourceRelativeBaked() {
		t.Fatal("lazy node must NOT be source-relative baked (it is not baked at all)")
	}
	if !NewFieldValueWithResolvedOrdinal("A", 1, UnknownType).SourceRelativeBaked() {
		t.Fatal("unpinned single-accessor bake IS source-relative")
	}
	pinned := &FieldValue{Field: "A", Resolved: NewFieldPathOfSingle("A", 1, true)}
	if pinned.SourceRelativeBaked() {
		t.Fatal("FrontierPinned node is machinery-owned, never source-relative")
	}
	multi := &FieldValue{Field: "B", Resolved: &FieldPath{Accessors: []ResolvedAccessor{
		{Field: "A", Ordinal: 0}, {Field: "B", Ordinal: 1},
	}}}
	if multi.SourceRelativeBaked() {
		t.Fatal("multi-accessor path is machinery-owned, never source-relative")
	}
}
