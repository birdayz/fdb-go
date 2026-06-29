package chaos

import (
	"fmt"

	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"google.golang.org/protobuf/proto"

	"fdb.dev/pkg/recordlayer"
)

// verifyVectorSPFreshIndexes checks SPFresh (RFC-094) vector indexes against the
// model. SPFresh is approximate and FDB-native (no exact ScanIndex), so the
// invariants are the queryable + structural form of "the index agrees with the
// records":
//
//  1. Completeness (no silently lost record): every model record is found by a
//     self-search on its own vector — the quiet-corruption detector a vector
//     index uniquely needs (it fails with wrong answers, not a crash).
//  2. No orphans: every pk a search returns exists in the model.
//  3. Structural integrity (SPFreshCheckIntegrity): membership ⊆ postings,
//     every membership target ACTIVE-or-SEALED (sealed postings are still
//     searched and hold valid membership mid-split — only FORWARD/DEAD/absent
//     are bad), and no ACTIVE posting over the 4×Lmax hard envelope (the SPANN
//     balanced-postings / LIRE split guarantee — a split that failed to drain
//     grows a posting unboundedly past it). Strict here, so the caller MUST
//     drain the maintenance queue to quiescence before Verify; the chaos
//     scenario drains via the clean DB before each checkpoint.
//
// These hold under the foreground write path AND after a fault-injected
// rebalance/refine drain: a commit_unknown replay or conflict retry that
// double-applied or dropped work shows up as a self-search miss, an orphan, or
// an integrity violation.
func verifyVectorSPFreshIndexes(store *recordlayer.FDBRecordStore, model *StoreModel) []Violation {
	var violations []Violation
	md := model.metadata

	for _, idx := range md.GetAllIndexes() {
		if idx.Type != recordlayer.IndexTypeVectorSPFresh {
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
					Invariant:  "spfresh_index_eval_error",
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
						Invariant:  "spfresh_index_convert_error",
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
		distinctPKs := make(map[string]bool, expectedCount)
		for _, e := range entries {
			distinctPKs[string(e.pk.Pack())] = true
		}
		if expectedCount == 0 {
			continue
		}

		// k large enough that every record at the queried vector (and beyond)
		// is in range — at chaos scale the topology spans a handful of cells
		// the default probe budget covers entirely.
		k := expectedCount

		// 1. Completeness: every model record found by self-search on its own
		//    vector. With duplicate vectors, any record at distance ~0 with this
		//    pk among results is fine; a true miss means the record is not in
		//    the index (lost insert / orphaned membership / dropped by a fault).
		for _, entry := range entries {
			results, err := recordlayer.SearchSPFreshIndex(store, idx.Name, entry.vector, k)
			if err != nil {
				violations = append(violations, Violation{
					Invariant:  "spfresh_index_self_search_error",
					PrimaryKey: entry.pk,
					Expected:   fmt.Sprintf("index %q self-search succeeds", idx.Name),
					Actual:     err.Error(),
				})
				continue
			}
			if len(results) == 0 {
				violations = append(violations, Violation{
					Invariant:  "spfresh_index_self_search_empty",
					PrimaryKey: entry.pk,
					Expected:   "at least 1 result",
					Actual:     "0 results",
				})
				continue
			}
			found := false
			for _, r := range results {
				if string(r.PrimaryKey.Pack()) == string(entry.pk.Pack()) {
					found = true
					break
				}
			}
			if !found {
				violations = append(violations, Violation{
					Invariant:  "spfresh_index_self_search_miss",
					PrimaryKey: entry.pk,
					Expected:   fmt.Sprintf("index %q returns own pk for its own vector", idx.Name),
					Actual:     fmt.Sprintf("nearest PK=%v distance=%f (%d results)", results[0].PrimaryKey, results[0].Distance, len(results)),
				})
			}
		}

		// 2. No orphans: every pk a search returns must exist in the model.
		if orphans, err := recordlayer.SearchSPFreshIndex(store, idx.Name, entries[0].vector, expectedCount+10); err == nil {
			for _, r := range orphans {
				if !model.Has(r.PrimaryKey) {
					violations = append(violations, Violation{
						Invariant:  "spfresh_index_orphan",
						PrimaryKey: r.PrimaryKey,
						Expected:   fmt.Sprintf("index %q: PK in model", idx.Name),
						Actual:     "returned by SPFresh search but not in model",
					})
				}
			}
		}

		// 3. Structural integrity: membership ⊆ postings + every target ACTIVE.
		//    Sample everything at chaos scale. STRICT — assumes the maintenance
		//    queue is drained (the scenario drains before Verify).
		const sampleAll = 1 << 30
		report, err := recordlayer.SPFreshCheckIntegrity(store.GetContext(), store, idx.Name, sampleAll)
		if err != nil {
			violations = append(violations, Violation{
				Invariant: "spfresh_index_integrity_error",
				Expected:  fmt.Sprintf("index %q integrity checkable", idx.Name),
				Actual:    err.Error(),
			})
			continue
		}
		// Bidirectional count: exactly one membership row per live indexed
		// record. Catches a leaked membership (a deleted record's row not
		// cleared → Members > records) AND a lost one (a fault dropped the
		// insert's membership → Members < records) deterministically — stronger
		// than the single-query orphan probe above, which only reaches pks a
		// query happens to surface.
		if report.Members != len(distinctPKs) {
			violations = append(violations, Violation{
				Invariant: "spfresh_index_membership_count",
				Expected:  fmt.Sprintf("index %q: %d membership rows (one per live record)", idx.Name, len(distinctPKs)),
				Actual:    fmt.Sprintf("%d membership rows", report.Members),
			})
		}
		if report.MembershipWithoutEntry > 0 {
			violations = append(violations, Violation{
				Invariant: "spfresh_index_membership_without_entry",
				Expected:  fmt.Sprintf("index %q: every membership target holds the posting entry", idx.Name),
				Actual:    fmt.Sprintf("%d/%d sampled pks missing a posting entry; e.g. %v", report.MembershipWithoutEntry, report.Sampled, report.Violations),
			})
		}
		if report.BadTargets > 0 {
			violations = append(violations, Violation{
				Invariant: "spfresh_index_bad_targets",
				Expected:  fmt.Sprintf("index %q: every membership target ACTIVE-or-SEALED (post-drain)", idx.Name),
				Actual:    fmt.Sprintf("%d bad targets (forward/dead/absent); states=%v; e.g. %v", report.BadTargets, report.TargetStates, report.Violations),
			})
		}
		// SPANN §3.2.1 balanced-postings / LIRE split guarantee: no ACTIVE
		// posting may exceed the 4×Lmax hard envelope. The <=4Lmax band is the
		// legitimate operating envelope (just-split children, unsampled
		// Lmax..2Lmax postings, closure overcount), so only >4Lmax is a
		// violation — that is a split the lifecycle failed to drain.
		if report.OversizedHard > 0 {
			violations = append(violations, Violation{
				Invariant: "spfresh_index_posting_over_envelope",
				Expected:  fmt.Sprintf("index %q: no ACTIVE posting over 4×Lmax", idx.Name),
				Actual:    fmt.Sprintf("%d posting(s) over 4×Lmax (maxLen=%d, Lmax band exceeded by %d); e.g. %v", report.OversizedHard, report.MaxPostingLen, report.Oversized, report.Violations),
			})
		}
	}

	return violations
}
