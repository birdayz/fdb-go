package recordlayer

import (
	"errors"
	"testing"

	"fdb.dev/gen"
)

func validatorBuilder(t testing.TB) *RecordMetaDataBuilder {
	t.Helper()
	b := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
	b.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
	b.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
	b.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
	return b
}

func TestFormerIndexVersionBoundaries(t *testing.T) {
	t.Parallel()

	t.Run("addedVersion_exceeds_metadata_version", func(t *testing.T) {
		t.Parallel()
		b := validatorBuilder(t)
		b.SetVersion(5)
		b.formerIndexes = append(b.formerIndexes, &FormerIndex{
			SubspaceKey:    "old_idx",
			AddedVersion:   10,
			RemovedVersion: 10,
			FormerName:     "old_idx",
		})
		_, err := b.Build()
		if err == nil {
			t.Fatal("expected error for addedVersion > metadata version")
		}
		var me *MetaDataError
		if !errors.As(err, &me) {
			t.Fatalf("expected MetaDataError, got %T: %v", err, err)
		}
	})

	t.Run("removedVersion_exceeds_metadata_version", func(t *testing.T) {
		t.Parallel()
		b := validatorBuilder(t)
		b.SetVersion(5)
		b.formerIndexes = append(b.formerIndexes, &FormerIndex{
			SubspaceKey:    "old_idx",
			AddedVersion:   3,
			RemovedVersion: 10,
			FormerName:     "old_idx",
		})
		_, err := b.Build()
		if err == nil {
			t.Fatal("expected error for removedVersion > metadata version")
		}
		var me *MetaDataError
		if !errors.As(err, &me) {
			t.Fatalf("expected MetaDataError, got %T: %v", err, err)
		}
	})

	t.Run("valid_versions_within_bounds", func(t *testing.T) {
		t.Parallel()
		b := validatorBuilder(t)
		b.SetVersion(10)
		b.formerIndexes = append(b.formerIndexes, &FormerIndex{
			SubspaceKey:    "old_idx",
			AddedVersion:   3,
			RemovedVersion: 7,
			FormerName:     "old_idx",
		})
		_, err := b.Build()
		if err != nil {
			t.Fatalf("expected no error, got: %v", err)
		}
	})

	t.Run("versions_equal_to_metadata_version_ok", func(t *testing.T) {
		t.Parallel()
		b := validatorBuilder(t)
		b.SetVersion(5)
		b.formerIndexes = append(b.formerIndexes, &FormerIndex{
			SubspaceKey:    "old_idx",
			AddedVersion:   5,
			RemovedVersion: 5,
			FormerName:     "old_idx",
		})
		_, err := b.Build()
		if err != nil {
			t.Fatalf("expected no error for versions == metadata version, got: %v", err)
		}
	})
}

func TestIndexAddedVsLastModifiedVersion(t *testing.T) {
	t.Parallel()

	t.Run("addedVersion_greater_than_lastModifiedVersion", func(t *testing.T) {
		t.Parallel()
		b := validatorBuilder(t)
		b.SetVersion(10)
		idx := NewIndex("test_idx", Field("price"))
		idx.AddedVersion = 8
		idx.LastModifiedVersion = 5
		b.AddIndex("Order", idx)
		_, err := b.Build()
		if err == nil {
			t.Fatal("expected error for addedVersion > lastModifiedVersion")
		}
		var me *MetaDataError
		if !errors.As(err, &me) {
			t.Fatalf("expected MetaDataError, got %T: %v", err, err)
		}
	})

	t.Run("equal_versions_ok", func(t *testing.T) {
		t.Parallel()
		b := validatorBuilder(t)
		b.SetVersion(10)
		idx := NewIndex("test_idx", Field("price"))
		idx.AddedVersion = 5
		idx.LastModifiedVersion = 5
		b.AddIndex("Order", idx)
		_, err := b.Build()
		if err != nil {
			t.Fatalf("expected no error, got: %v", err)
		}
	})

	t.Run("zero_versions_not_checked", func(t *testing.T) {
		t.Parallel()
		b := validatorBuilder(t)
		idx := NewIndex("test_idx", Field("price"))
		// Both zero — should not trigger validation
		b.AddIndex("Order", idx)
		_, err := b.Build()
		if err != nil {
			t.Fatalf("expected no error for zero versions, got: %v", err)
		}
	})
}

func TestIndexReplacementValidation(t *testing.T) {
	t.Parallel()

	t.Run("replacement_not_in_metadata", func(t *testing.T) {
		t.Parallel()
		b := validatorBuilder(t)
		idx := NewIndex("old_idx", Field("price"))
		idx.Options["replacedBy"] = "nonexistent_idx"
		b.AddIndex("Order", idx)
		_, err := b.Build()
		if err == nil {
			t.Fatal("expected error for replacement index not in metadata")
		}
		var me *MetaDataError
		if !errors.As(err, &me) {
			t.Fatalf("expected MetaDataError, got %T: %v", err, err)
		}
	})

	t.Run("chained_replacements_disallowed", func(t *testing.T) {
		t.Parallel()
		b := validatorBuilder(t)
		idxC := NewIndex("idx_c", Field("price"))
		b.AddIndex("Order", idxC)

		idxB := NewIndex("idx_b", Field("price"))
		idxB.Options["replacedBy"] = "idx_c"
		b.AddIndex("Order", idxB)

		idxA := NewIndex("idx_a", Field("price"))
		idxA.Options["replacedBy"] = "idx_b"
		b.AddIndex("Order", idxA)

		_, err := b.Build()
		if err == nil {
			t.Fatal("expected error for chained replacement indexes")
		}
		var me *MetaDataError
		if !errors.As(err, &me) {
			t.Fatalf("expected MetaDataError, got %T: %v", err, err)
		}
	})

	t.Run("valid_single_replacement", func(t *testing.T) {
		t.Parallel()
		b := validatorBuilder(t)
		newIdx := NewIndex("new_idx", Field("price"))
		b.AddIndex("Order", newIdx)

		oldIdx := NewIndex("old_idx", Field("price"))
		oldIdx.Options["replacedBy"] = "new_idx"
		b.AddIndex("Order", oldIdx)

		_, err := b.Build()
		if err != nil {
			t.Fatalf("expected no error, got: %v", err)
		}
	})

	t.Run("multiple_replacements_via_prefix", func(t *testing.T) {
		t.Parallel()
		b := validatorBuilder(t)
		b.AddIndex("Order", NewIndex("new_a", Field("price")))
		b.AddIndex("Order", NewIndex("new_b", Field("quantity")))

		old := NewIndex("old_idx", Field("price"))
		old.Options["replacedBy_0"] = "new_a"
		old.Options["replacedBy_1"] = "new_b"
		b.AddIndex("Order", old)

		_, err := b.Build()
		if err != nil {
			t.Fatalf("expected no error, got: %v", err)
		}
	})
}

func TestGetReplacedByIndexNames(t *testing.T) {
	t.Parallel()

	t.Run("empty", func(t *testing.T) {
		t.Parallel()
		idx := NewIndex("test", Field("x"))
		if len(idx.GetReplacedByIndexNames()) != 0 {
			t.Fatal("expected empty for fresh index")
		}
	})

	t.Run("single", func(t *testing.T) {
		t.Parallel()
		idx := NewIndex("test", Field("x"))
		idx.Options["replacedBy"] = "new_idx"
		names := idx.GetReplacedByIndexNames()
		if len(names) != 1 || names[0] != "new_idx" {
			t.Fatalf("expected [new_idx], got %v", names)
		}
	})

	t.Run("multiple_with_prefix", func(t *testing.T) {
		t.Parallel()
		idx := NewIndex("test", Field("x"))
		idx.Options["replacedBy_0"] = "a"
		idx.Options["replacedBy_1"] = "b"
		names := idx.GetReplacedByIndexNames()
		if len(names) != 2 {
			t.Fatalf("expected 2 names, got %d: %v", len(names), names)
		}
	})

	t.Run("non_matching_options_ignored", func(t *testing.T) {
		t.Parallel()
		idx := NewIndex("test", Field("x"))
		idx.Options["unique"] = "true"
		idx.Options["replacedBy"] = "new_idx"
		idx.Options["clearWhenZero"] = "true"
		names := idx.GetReplacedByIndexNames()
		if len(names) != 1 {
			t.Fatalf("expected 1 name, got %d: %v", len(names), names)
		}
	})
}
