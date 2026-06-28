package chaos

import (
	"testing"

	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"google.golang.org/protobuf/proto"

	"fdb.dev/gen"
	"fdb.dev/pkg/recordlayer"
)

// buildTextMetadata creates metadata with a TEXT index on Customer.name.
func buildTextMetadata() *recordlayer.RecordMetaData {
	builder := recordlayer.NewRecordMetaDataBuilder()
	builder.SetRecords(gen.File_record_layer_demo_proto)
	builder.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
	builder.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
	builder.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
	builder.SetRecordCountKey(recordlayer.EmptyKey())

	idx := recordlayer.NewTextIndex("customer_name_text", recordlayer.Field("name"))
	builder.AddIndex("Customer", idx)

	md, err := builder.Build()
	if err != nil {
		panic("chaos: failed to build text metadata: " + err.Error())
	}
	return md
}

// TestTextBasicVerify tests the TEXT index with no fault injection.
// Validates the verification framework itself works correctly.
func TestTextBasicVerify(t *testing.T) {
	t.Parallel()
	md := buildTextMetadata()
	s := NewScenario(t, testRealDB, md)

	s.SaveRecord(&gen.Customer{CustomerId: proto.Int64(1), Name: proto.String("Hello World")})
	s.Verify()

	s.SaveRecord(&gen.Customer{CustomerId: proto.Int64(2), Name: proto.String("Alice Bob Charlie")})
	s.Verify()

	s.SaveRecord(&gen.Customer{CustomerId: proto.Int64(3), Name: proto.String("Hello Alice")})
	s.Verify()

	// Delete one and re-verify.
	s.DeleteRecord(tuple.Tuple{int64(2)})
	s.Verify()

	// Re-insert with different name.
	s.SaveRecord(&gen.Customer{CustomerId: proto.Int64(2), Name: proto.String("New Name Here")})
	s.Verify()
}

// TestTextCommitUnknownInsert injects commit-unknown on a TEXT index insert.
// TEXT indexes are idempotent under commit-unknown because removeCommonEntries
// skips identical token→position mappings on retry.
func TestTextCommitUnknownInsert(t *testing.T) {
	t.Parallel()
	md := buildTextMetadata()
	s := NewScenario(t, testRealDB, md)

	s.InjectOnce(FaultCommitUnknown)
	s.SaveRecord(&gen.Customer{CustomerId: proto.Int64(1), Name: proto.String("Hello World")})
	s.Verify()

	s.InjectOnce(FaultCommitUnknown)
	s.SaveRecord(&gen.Customer{CustomerId: proto.Int64(2), Name: proto.String("Foo Bar Baz")})
	s.Verify()

	// Third save, no fault.
	s.SaveRecord(&gen.Customer{CustomerId: proto.Int64(3), Name: proto.String("Testing Chaos")})
	s.Verify()
}

// TestTextCommitUnknownOverwrite injects commit-unknown on a record overwrite
// that changes the name (and thus the tokens). This is the dangerous scenario:
// First commit: remove old tokens, add new tokens.
// Retry: old record now has the new tokens → removeCommonEntries → no-op.
func TestTextCommitUnknownOverwrite(t *testing.T) {
	t.Parallel()
	md := buildTextMetadata()
	s := NewScenario(t, testRealDB, md)

	s.SaveRecord(&gen.Customer{CustomerId: proto.Int64(1), Name: proto.String("Hello World")})
	s.SaveRecord(&gen.Customer{CustomerId: proto.Int64(2), Name: proto.String("Foo Bar")})
	s.Verify()

	// Overwrite pk=1: "Hello World" → "Goodbye Universe" with commit-unknown.
	s.InjectOnce(FaultCommitUnknown)
	s.SaveRecord(&gen.Customer{CustomerId: proto.Int64(1), Name: proto.String("Goodbye Universe")})
	s.Verify()

	// Overwrite pk=2: "Foo Bar" → "Baz Qux" with commit-unknown.
	s.InjectOnce(FaultCommitUnknown)
	s.SaveRecord(&gen.Customer{CustomerId: proto.Int64(2), Name: proto.String("Baz Qux")})
	s.Verify()
}

// TestTextCommitUnknownDelete injects commit-unknown on deletes.
// First commit: clear token entries for the record.
// Retry: old=nil (already deleted) → no entries to process → no-op.
func TestTextCommitUnknownDelete(t *testing.T) {
	t.Parallel()
	md := buildTextMetadata()
	s := NewScenario(t, testRealDB, md)

	s.SaveRecord(&gen.Customer{CustomerId: proto.Int64(1), Name: proto.String("Hello World")})
	s.SaveRecord(&gen.Customer{CustomerId: proto.Int64(2), Name: proto.String("Foo Bar")})
	s.SaveRecord(&gen.Customer{CustomerId: proto.Int64(3), Name: proto.String("Baz Qux")})
	s.Verify()

	// Delete pk=2 with commit-unknown.
	s.InjectOnce(FaultCommitUnknown)
	s.DeleteRecord(tuple.Tuple{int64(2)})
	s.Verify()

	// Delete pk=1 with commit-unknown.
	s.InjectOnce(FaultCommitUnknown)
	s.DeleteRecord(tuple.Tuple{int64(1)})
	s.Verify()
}

// TestTextUpdateSameName overwrites a record with the same name.
// This exercises the removeCommonEntries fast path — all entries are identical,
// so no writes should happen.
func TestTextUpdateSameName(t *testing.T) {
	t.Parallel()
	md := buildTextMetadata()
	s := NewScenario(t, testRealDB, md)

	s.SaveRecord(&gen.Customer{CustomerId: proto.Int64(1), Name: proto.String("Hello World")})
	s.Verify()

	// Overwrite with identical name — removeCommonEntries should skip everything.
	s.SaveRecord(&gen.Customer{CustomerId: proto.Int64(1), Name: proto.String("Hello World")})
	s.Verify()

	// Overwrite again with commit-unknown — double no-op should still be fine.
	s.InjectOnce(FaultCommitUnknown)
	s.SaveRecord(&gen.Customer{CustomerId: proto.Int64(1), Name: proto.String("Hello World")})
	s.Verify()
}

// TestTextUpdateDifferentName overwrites a record with a different name.
// Old tokens should be removed, new tokens should be added.
func TestTextUpdateDifferentName(t *testing.T) {
	t.Parallel()
	md := buildTextMetadata()
	s := NewScenario(t, testRealDB, md)

	s.SaveRecord(&gen.Customer{CustomerId: proto.Int64(1), Name: proto.String("Alpha Beta Gamma")})
	s.Verify()

	// Change to completely different tokens.
	s.SaveRecord(&gen.Customer{CustomerId: proto.Int64(1), Name: proto.String("Delta Epsilon Zeta")})
	s.Verify()

	// Change to partially overlapping tokens.
	s.SaveRecord(&gen.Customer{CustomerId: proto.Int64(1), Name: proto.String("Delta Theta Iota")})
	s.Verify()

	// Change to single token.
	s.SaveRecord(&gen.Customer{CustomerId: proto.Int64(1), Name: proto.String("Omega")})
	s.Verify()
}

// TestTextDeleteAllRecords verifies DeleteAllRecords clears all text entries.
func TestTextDeleteAllRecords(t *testing.T) {
	t.Parallel()
	md := buildTextMetadata()
	s := NewScenario(t, testRealDB, md)

	for i := int64(1); i <= 10; i++ {
		s.SaveRecord(&gen.Customer{
			CustomerId: proto.Int64(i),
			Name:       proto.String("Customer Number " + string(rune('A'-1+i))),
		})
	}
	s.Verify()

	s.DeleteAllRecords()
	s.Verify()

	// Re-add after delete-all.
	s.SaveRecord(&gen.Customer{CustomerId: proto.Int64(99), Name: proto.String("New Customer")})
	s.Verify()
}

// TestTextRandomFaults runs many random operations with continuous fault
// injection (5% commit-unknown) and periodic verification.
func TestTextRandomFaults(t *testing.T) {
	t.Parallel()
	md := buildTextMetadata()
	s := NewScenario(t, testRealDB, md, WithSeed(31337), WithFaults(FaultsRetryHeavy))

	const numOps = 100
	const verifyEvery = 20
	maxPK := int64(20)

	names := []string{
		"Hello World",
		"Foo Bar Baz",
		"Alice Bob Charlie",
		"Quick Brown Fox",
		"Lazy Dog Jumps",
		"Red Green Blue",
		"Coffee Tea Water",
		"North South East",
		"Sun Moon Stars",
		"Rock Paper Scissors",
	}

	for i := 0; i < numOps; i++ {
		pk := s.Rng.Int64N(maxPK) + 1
		if s.Rng.Float64() < 0.7 {
			// 70% saves with random names.
			name := names[s.Rng.IntN(len(names))]
			s.SaveRecord(&gen.Customer{
				CustomerId: proto.Int64(pk),
				Name:       proto.String(name),
			})
		} else {
			// 30% deletes.
			s.DeleteRecord(tuple.Tuple{pk})
		}

		if (i+1)%verifyEvery == 0 {
			s.Verify()
		}
	}
	s.Verify()

	t.Logf("completed %d ops with seed=%d, %d faults injected",
		numOps, s.Seed(), len(s.FaultLog()))
}

// TestTextHeavyFaultStress runs with a very high fault injection rate (20%)
// to maximize the chance of catching non-idempotent behavior.
func TestTextHeavyFaultStress(t *testing.T) {
	t.Parallel()
	md := buildTextMetadata()
	s := NewScenario(t, testRealDB, md, WithSeed(42424), WithFaults(FaultsRetryVeryHeavy))

	const numOps = 200
	const verifyEvery = 20
	maxPK := int64(15) // smaller key space = more overwrites

	names := []string{
		"Alpha Beta",
		"Gamma Delta",
		"Epsilon Zeta",
		"Eta Theta",
		"Iota Kappa",
	}

	for i := 0; i < numOps; i++ {
		pk := s.Rng.Int64N(maxPK) + 1
		if s.Rng.Float64() < 0.6 {
			name := names[s.Rng.IntN(len(names))]
			s.SaveRecord(&gen.Customer{
				CustomerId: proto.Int64(pk),
				Name:       proto.String(name),
			})
		} else {
			s.DeleteRecord(tuple.Tuple{pk})
		}

		if (i+1)%verifyEvery == 0 {
			s.Verify()
		}
	}
	s.Verify()

	t.Logf("completed %d ops with seed=%d, %d faults injected",
		numOps, s.Seed(), len(s.FaultLog()))
}
