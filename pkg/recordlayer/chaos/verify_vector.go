package chaos

import (
	"context"
	"fmt"

	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"google.golang.org/protobuf/proto"

	"fdb.dev/pkg/recordlayer"
)

// verifyVectorIndexes checks VECTOR (HNSW) indexes by verifying:
//  1. Self-search: for each model record, searching for its own vector with k=1
//     should return the record itself as the nearest neighbor.
//  2. Count: searching with a large k should return exactly the number of
//     model records that have this index.
//
// Unlike VALUE or MULTIDIMENSIONAL indexes, VECTOR indexes do not support
// TupleRange-based ScanIndex — we must use SearchVectorIndex (kNN search).
func verifyVectorIndexes(ctx context.Context, store *recordlayer.FDBRecordStore, model *StoreModel) []Violation {
	var violations []Violation
	md := model.metadata

	for _, idx := range md.GetAllIndexes() {
		if idx.Type != recordlayer.IndexTypeVector {
			continue
		}

		// Collect model records that apply to this index, with their vectors.
		type modelEntry struct {
			pk     tuple.Tuple
			vector []float64
		}
		var entries []modelEntry

		for _, rec := range model.Records {
			if !model.indexAppliesToType(idx, rec.TypeName) {
				continue
			}

			storedRec := &recordlayer.FDBStoredRecord[proto.Message]{
				PrimaryKey: rec.PrimaryKey,
				RecordType: md.GetRecordType(rec.TypeName),
				Record:     rec.Message,
			}

			tuples, err := idx.RootExpression.Evaluate(storedRec, rec.Message)
			if err != nil {
				violations = append(violations, Violation{
					Invariant:  "vector_index_eval_error",
					PrimaryKey: rec.PrimaryKey,
					Expected:   fmt.Sprintf("index %q evaluable", idx.Name),
					Actual:     err.Error(),
				})
				continue
			}

			for _, values := range tuples {
				vec := valuesToFloat64(values)
				if vec == nil {
					violations = append(violations, Violation{
						Invariant:  "vector_index_convert_error",
						PrimaryKey: rec.PrimaryKey,
						Expected:   fmt.Sprintf("index %q values convertible to float64", idx.Name),
						Actual:     fmt.Sprintf("values: %v", values),
					})
					continue
				}
				entries = append(entries, modelEntry{pk: rec.PrimaryKey, vector: vec})
			}
		}

		expectedCount := len(entries)

		// 1. Count verification: search with large k, compare result count.
		if expectedCount > 0 {
			// Use the first entry's vector as the query — we just need a valid vector
			// to get all results back when k >= expectedCount.
			results, err := store.SearchVectorIndex(idx, entries[0].vector, expectedCount+10, 400)
			if err != nil {
				violations = append(violations, Violation{
					Invariant: "vector_index_search_error",
					Expected:  fmt.Sprintf("index %q searchable", idx.Name),
					Actual:    err.Error(),
				})
			} else if len(results) != expectedCount {
				violations = append(violations, Violation{
					Invariant: "vector_index_count",
					Expected:  fmt.Sprintf("index %q: %d entries", idx.Name, expectedCount),
					Actual:    fmt.Sprintf("%d entries", len(results)),
				})
			}
		}

		// 2. Self-search: for each model record, searching for its own vector
		//    should return that record as the nearest neighbor (distance ~0).
		for _, entry := range entries {
			results, err := store.SearchVectorIndex(idx, entry.vector, 1, 400)
			if err != nil {
				violations = append(violations, Violation{
					Invariant:  "vector_index_self_search_error",
					PrimaryKey: entry.pk,
					Expected:   fmt.Sprintf("index %q self-search succeeds", idx.Name),
					Actual:     err.Error(),
				})
				continue
			}
			if len(results) == 0 {
				violations = append(violations, Violation{
					Invariant:  "vector_index_self_search_empty",
					PrimaryKey: entry.pk,
					Expected:   "at least 1 result",
					Actual:     "0 results",
				})
				continue
			}

			// The nearest neighbor should be the record itself.
			// With unique vectors, this is always true. With duplicate vectors,
			// any record with the same vector at distance 0 is acceptable.
			found := false
			for _, r := range results {
				if string(r.PrimaryKey.Pack()) == string(entry.pk.Pack()) {
					found = true
					break
				}
			}
			if !found {
				// Check if result is at distance 0 (duplicate vector case).
				if results[0].Distance > 1e-9 {
					violations = append(violations, Violation{
						Invariant:  "vector_index_self_search_miss",
						PrimaryKey: entry.pk,
						Expected:   fmt.Sprintf("self or duplicate at distance ~0"),
						Actual:     fmt.Sprintf("nearest PK=%v distance=%f", results[0].PrimaryKey, results[0].Distance),
					})
				}
			}
		}

		// 3. Verify no orphans: every HNSW result PK should exist in the model.
		if expectedCount > 0 {
			results, err := store.SearchVectorIndex(idx, entries[0].vector, expectedCount+10, 400)
			if err == nil {
				for _, r := range results {
					if !model.Has(r.PrimaryKey) {
						violations = append(violations, Violation{
							Invariant:  "vector_index_orphan",
							PrimaryKey: r.PrimaryKey,
							Expected:   fmt.Sprintf("index %q: PK in model", idx.Name),
							Actual:     "exists in HNSW but not in model",
						})
					}
				}
			}
		}
	}

	return violations
}

// valuesToFloat64 converts index expression evaluation results to a float64 vector.
// Returns nil if any element is not numeric.
func valuesToFloat64(values []any) []float64 {
	vec := make([]float64, 0, len(values))
	for _, v := range values {
		switch n := v.(type) {
		case float64:
			vec = append(vec, n)
		case float32:
			vec = append(vec, float64(n))
		case int64:
			vec = append(vec, float64(n))
		case int32:
			vec = append(vec, float64(n))
		case int:
			vec = append(vec, float64(n))
		default:
			return nil
		}
	}
	return vec
}
