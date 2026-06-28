package recordlayer

import (
	"errors"
	"fmt"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
)

// Exported SPFresh verification/observability surface. These are the SPFresh
// analogs of SearchVectorIndex (which is HNSW-only): a kNN search entry and a
// structured integrity check, both usable from outside the recordlayer package
// (model-based chaos verification, ground-truth recall monitoring, operational
// diagnostics). The unexported maintainer path (ScanByDistance /
// searchCurrentGeneration) and the string-formatting SPFreshDebugIntegrity stay
// as-is; this file adds the programmatic, structured equivalents.

// SearchSPFreshIndex runs a kNN search against a readable SPFresh index,
// returning up to k nearest primary keys (with exact, sidecar-re-ranked
// distances, ascending) to queryVector using the index's default probe budget.
// It is the SPFresh counterpart to FDBRecordStore.SearchVectorIndex
// (vector_index_maintainer.go), which type-asserts the HNSW maintainer and
// rejects SPFresh. Callers needing per-query probe knobs (k, kc, w, c, ε) drive
// ScanByDistance through the executor instead.
func SearchSPFreshIndex(store *FDBRecordStore, indexName string, queryVector []float64, k int) ([]VectorSearchResult, error) {
	idx := store.GetMetaData().GetIndex(indexName)
	if idx == nil {
		return nil, fmt.Errorf("spfresh search: index %q not found", indexName)
	}
	if idx.Type != IndexTypeVectorSPFresh {
		return nil, fmt.Errorf("spfresh search: index %q has type %q, not %q", indexName, idx.Type, IndexTypeVectorSPFresh)
	}
	maintainer, err := store.getIndexMaintainer(idx)
	if err != nil {
		return nil, err
	}
	m, ok := maintainer.(*spfreshIndexMaintainer)
	if !ok {
		return nil, fmt.Errorf("spfresh search: index %q maintainer is %T, not *spfreshIndexMaintainer", indexName, maintainer)
	}
	// Zero probe knobs => the searcher's frozen defaults (kc=64, w default,
	// c=200, ε=7); generous for the small topologies these callers inspect.
	results, err := m.searchCurrentGeneration(queryVector, k, 0, 0, 0, 0, false)
	if err != nil {
		return nil, err
	}
	out := make([]VectorSearchResult, len(results))
	for i, r := range results {
		out[i] = VectorSearchResult{PrimaryKey: r.PrimaryKey, Distance: r.Distance}
	}
	return out, nil
}

// SPFreshIntegrityViolation is one structural defect found by
// SPFreshCheckIntegrity: a membership row referencing a posting/centroid that
// the LIRE invariants forbid.
type SPFreshIntegrityViolation struct {
	PrimaryKey tuple.Tuple
	FineID     int64
	// Kind: "missing_posting" (membership target lacks this pk's posting
	// entry), "absent_target" (target fineID is in no cell), "forward_target"
	// (target centroid split away), "dead_target" (target centroid GC'd).
	Kind string
}

func (v SPFreshIntegrityViolation) String() string {
	return fmt.Sprintf("%s pk=%v fine=%d", v.Kind, v.PrimaryKey, v.FineID)
}

// SPFreshIntegrityReport is the structured result of SPFreshCheckIntegrity —
// the same membership⊆postings + target-state invariants SPFreshDebugIntegrity
// formats as a string, returned as data for programmatic assertions. The
// report is descriptive; the caller sets policy. After the maintenance queue is
// drained to quiescence the strict invariant is MembershipWithoutEntry == 0 AND
// BadTargets == 0 (every membership target ACTIVE, every posting entry present);
// mid-flight, transient SEALED targets and in-progress forwards are expected.
type SPFreshIntegrityReport struct {
	Members                int            // total membership rows in the index
	Sampled                int            // pks actually checked
	OK                     int            // sampled pks whose every membership target holds the posting entry
	MembershipWithoutEntry int            // sampled pks with >=1 membership target missing its posting entry
	TargetStates           map[string]int // membership-target references by centroid state: active/sealed/forward/dead/absent
	BadTargets             int            // target references in forward/dead/absent state (unambiguously bad post-drain)
	Violations             []SPFreshIntegrityViolation
}

// spfreshStateName maps a centroid lifecycle-state byte to a stable label.
func spfreshStateName(st byte) string {
	switch st {
	case spfreshStateActive:
		return "active"
	case spfreshStateSealed:
		return "sealed"
	case spfreshStateForward:
		return "forward"
	case spfreshStateDead:
		return "dead"
	default:
		return fmt.Sprintf("state%d", st)
	}
}

// SPFreshCheckIntegrity samples up to `sample` pks evenly from the index's own
// membership rows and verifies, for each, that every membership target holds
// the pk's posting entry (membership ⊆ postings) and classifies the target
// centroid's lifecycle state. It is the structured, programmatic form of
// SPFreshDebugIntegrity. O(index) reads (it streams the membership keyspace) —
// never call it on a serving path. `sample` <= 0 checks a single pk.
//
// Violations beyond a small cap are counted but not individually recorded
// (BadTargets / MembershipWithoutEntry stay exact; Violations is truncated).
func SPFreshCheckIntegrity(rtx *FDBRecordContext, store *FDBRecordStore, indexName string, sample int) (SPFreshIntegrityReport, error) {
	const maxRecordedViolations = 64

	report := SPFreshIntegrityReport{TargetStates: map[string]int{}}
	idx := store.GetMetaData().GetIndex(indexName)
	if idx == nil {
		return report, fmt.Errorf("spfresh integrity: index %q not found", indexName)
	}
	if idx.Type != IndexTypeVectorSPFresh {
		return report, fmt.Errorf("spfresh integrity: index %q has type %q, not %q", indexName, idx.Type, IndexTypeVectorSPFresh)
	}

	tx := rtx.Transaction()
	metaStorage := newSPFreshStorage(store.indexSubspace(idx), 0)
	gen, err := spfreshReadGenerationSnapshot(tx, metaStorage)
	if err != nil {
		if errors.Is(err, errSPFreshNotFound) {
			return report, nil // never bootstrapped: empty, vacuously consistent
		}
		return report, fmt.Errorf("spfresh integrity: read generation: %w", err)
	}
	s := newSPFreshStorage(store.indexSubspace(idx), gen)

	// Map every fineID to its centroid lifecycle state. A target absent from
	// this map points at a centroid in no cell (orphaned membership).
	ids, _, lerr := spfreshLoadAllCoarse(tx, s)
	if lerr != nil {
		return report, fmt.Errorf("spfresh integrity: load coarse: %w", lerr)
	}
	fineState := map[int64]byte{}
	for _, cellID := range ids {
		rows, _, _, cerr := spfreshLoadCell(tx, s, cellID)
		if cerr != nil {
			return report, fmt.Errorf("spfresh integrity: load cell %d: %w", cellID, cerr)
		}
		for _, r := range rows {
			fineState[r.fineID] = r.row.state
		}
	}

	// Collect membership pks (keys only), then sample evenly by stride.
	r, rerr := fdb.PrefixRange(s.membership.Bytes())
	if rerr != nil {
		return report, fmt.Errorf("spfresh integrity: membership range: %w", rerr)
	}
	kvs, gerr := tx.Snapshot().GetRange(r, fdb.RangeOptions{Mode: fdb.StreamingModeWantAll}).GetSliceWithError()
	if gerr != nil {
		return report, fmt.Errorf("spfresh integrity: membership scan: %w", gerr)
	}
	var pks []tuple.Tuple
	for _, kv := range kvs {
		if pk, uerr := s.membership.Unpack(kv.Key); uerr == nil {
			pks = append(pks, pk)
		}
	}
	report.Members = len(pks)
	if sample <= 0 {
		sample = 1
	}
	step := len(pks) / sample
	if step < 1 {
		step = 1
	}

	record := func(v SPFreshIntegrityViolation) {
		if len(report.Violations) < maxRecordedViolations {
			report.Violations = append(report.Violations, v)
		}
	}

	for i := 0; i < len(pks); i += step {
		pk := pks[i]
		mem, merr := spfreshReadMembership(tx, s, pk)
		if merr != nil {
			// Raced a concurrent delete between the scan and this read — the pk
			// is gone, not a violation. (Snapshot-consistent within the tx, but
			// belt-and-suspenders for the diagnostic path.)
			continue
		}
		report.Sampled++
		allPresent := true
		for _, fineID := range mem {
			data, perr := tx.Snapshot().Get(s.postingKey(fineID, pk)).Get()
			if perr != nil {
				return report, fmt.Errorf("spfresh integrity: read posting (fine=%d): %w", fineID, perr)
			}
			if data == nil {
				allPresent = false
				record(SPFreshIntegrityViolation{PrimaryKey: pk, FineID: fineID, Kind: "missing_posting"})
			}
			st, known := fineState[fineID]
			if !known {
				report.TargetStates["absent"]++
				report.BadTargets++
				record(SPFreshIntegrityViolation{PrimaryKey: pk, FineID: fineID, Kind: "absent_target"})
				continue
			}
			report.TargetStates[spfreshStateName(st)]++
			switch st {
			case spfreshStateForward:
				report.BadTargets++
				record(SPFreshIntegrityViolation{PrimaryKey: pk, FineID: fineID, Kind: "forward_target"})
			case spfreshStateDead:
				report.BadTargets++
				record(SPFreshIntegrityViolation{PrimaryKey: pk, FineID: fineID, Kind: "dead_target"})
			}
		}
		if allPresent {
			report.OK++
		} else {
			report.MembershipWithoutEntry++
		}
	}
	return report, nil
}
