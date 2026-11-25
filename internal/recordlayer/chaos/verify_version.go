package chaos

import (
	"context"
	"fmt"

	"fdb.dev/pkg/fdbgo/fdb/tuple"

	"fdb.dev/pkg/recordlayer"
)

// verifyVersionIndexes checks VERSION index consistency:
//  1. Every model record (of the indexed type) has exactly one VERSION index entry
//  2. No orphan entries (entries for non-existent records)
//  3. Entry versionstamps match stored record versions (inline at pk + -1)
//
// Unlike VALUE indexes, we can't predict exact entry keys because versionstamps
// are assigned at commit time. Instead, we verify structurally via primary key matching.
func verifyVersionIndexes(ctx context.Context, store *recordlayer.FDBRecordStore, model *StoreModel) []Violation {
	var violations []Violation
	md := model.metadata

	for _, idx := range md.GetAllIndexes() {
		if idx.Type != recordlayer.IndexTypeVersion {
			continue
		}
		violations = append(violations, verifyVersionIndex(ctx, store, model, idx)...)
	}

	return violations
}

func verifyVersionIndex(ctx context.Context, store *recordlayer.FDBRecordStore, model *StoreModel, idx *recordlayer.Index) []Violation {
	var violations []Violation

	// Scan actual VERSION index entries and collect PKs + versionstamps.
	type entryInfo struct {
		pk           tuple.Tuple
		versionstamp tuple.Versionstamp
		hasVS        bool
	}
	actualByPK := make(map[string]*entryInfo) // packed PK -> entry info

	cursor := store.ScanIndex(idx, recordlayer.TupleRangeAll, nil, recordlayer.ForwardScan())
	for {
		result, err := cursor.OnNext(ctx)
		if err != nil {
			violations = append(violations, Violation{
				Invariant: "version_index_scan_error",
				Expected:  fmt.Sprintf("index %q scannable", idx.Name),
				Actual:    err.Error(),
			})
			break
		}
		if !result.HasNext() {
			break
		}
		entry := result.GetValue()
		pk := entry.PrimaryKey()
		packedPK := string(pk.Pack())

		info := &entryInfo{pk: pk}
		// Extract versionstamp from entry key. For VersionKeyExpression,
		// the first key element is the versionstamp.
		if len(entry.Key) > 0 {
			if vs, ok := entry.Key[0].(tuple.Versionstamp); ok {
				info.versionstamp = vs
				info.hasVS = true
			}
		}

		if _, exists := actualByPK[packedPK]; exists {
			// Duplicate entry for same PK — always a violation.
			violations = append(violations, Violation{
				Invariant:  "version_index_duplicate_entry",
				PrimaryKey: pk,
				Expected:   fmt.Sprintf("index %q: one entry per PK", idx.Name),
				Actual:     "multiple entries",
			})
		}
		actualByPK[packedPK] = info
	}
	_ = cursor.Close()

	// Build expected set: all model records that have this index.
	expectedPKs := make(map[string]tuple.Tuple)
	for _, rec := range model.Records {
		if !model.indexAppliesToType(idx, rec.TypeName) {
			continue
		}
		expectedPKs[string(rec.PrimaryKey.Pack())] = rec.PrimaryKey
	}

	// Diff: missing entries (model record with no index entry)
	for key, pk := range expectedPKs {
		if _, ok := actualByPK[key]; !ok {
			violations = append(violations, Violation{
				Invariant:  "version_index_entry_missing",
				PrimaryKey: pk,
				Expected:   fmt.Sprintf("index %q has entry for pk %v", idx.Name, pk),
				Actual:     "not in index",
			})
		}
	}

	// Diff: orphan entries (index entry with no model record)
	for key, info := range actualByPK {
		if _, ok := expectedPKs[key]; !ok {
			violations = append(violations, Violation{
				Invariant:  "version_index_entry_orphan",
				PrimaryKey: info.pk,
				Expected:   fmt.Sprintf("index %q: no entry for pk %v", idx.Name, info.pk),
				Actual:     "exists in index but not in model",
			})
		}
	}

	// Count cross-check
	if len(expectedPKs) != len(actualByPK) {
		violations = append(violations, Violation{
			Invariant: "version_index_entry_count",
			Expected:  fmt.Sprintf("index %q: %d entries", idx.Name, len(expectedPKs)),
			Actual:    fmt.Sprintf("%d entries", len(actualByPK)),
		})
	}

	// Verify entry versionstamps match stored inline record versions.
	for key, info := range actualByPK {
		if !info.hasVS {
			continue
		}
		// Only check records that exist in model (orphans already flagged above).
		if _, ok := expectedPKs[key]; !ok {
			continue
		}

		storedVersion, err := store.LoadRecordVersion(info.pk, true)
		if err != nil {
			violations = append(violations, Violation{
				Invariant:  "version_index_load_version_error",
				PrimaryKey: info.pk,
				Expected:   "version loadable",
				Actual:     err.Error(),
			})
			continue
		}
		if storedVersion == nil {
			violations = append(violations, Violation{
				Invariant:  "version_index_no_stored_version",
				PrimaryKey: info.pk,
				Expected:   "stored version exists",
				Actual:     "nil",
			})
			continue
		}

		// Compare global version bytes and local version.
		storedGlobal, err := storedVersion.GetGlobalVersion()
		if err != nil {
			// Incomplete version — shouldn't happen in a read-after-commit context.
			violations = append(violations, Violation{
				Invariant:  "version_index_incomplete_stored_version",
				PrimaryKey: info.pk,
				Expected:   "complete stored version",
				Actual:     err.Error(),
			})
			continue
		}

		globalMatch := true
		for i := 0; i < 10; i++ {
			if storedGlobal[i] != info.versionstamp.TransactionVersion[i] {
				globalMatch = false
				break
			}
		}
		localMatch := storedVersion.GetLocalVersion() == int(info.versionstamp.UserVersion)

		if !globalMatch || !localMatch {
			violations = append(violations, Violation{
				Invariant:  "version_index_version_mismatch",
				PrimaryKey: info.pk,
				Expected:   fmt.Sprintf("stored version %s", storedVersion),
				Actual:     fmt.Sprintf("index entry versionstamp %v (local=%d)", info.versionstamp.TransactionVersion, info.versionstamp.UserVersion),
			})
		}
	}

	return violations
}
