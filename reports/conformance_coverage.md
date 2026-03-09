# Conformance Test Coverage Report (2026-03-09)

## Current Coverage: 149 conformance specs, 453 unit tests

### Conformance Tests (18 files, cross-validated Go↔Java)

| File | Specs | Coverage |
|------|-------|----------|
| crud_test.go | 22 | Save, load, delete, update, boundary values |
| existence_check_conformance_test.go | 27 | 4 existence check modes |
| customer_conformance_test.go | 15 | Multi-type records (Customer) |
| split_conformance_test.go | 10 | Split records: 250KB/150KB/100KB/small |
| delete_conformance_test.go | 8 | Delete operations |
| count_conformance_test.go | 6 | Record counting (atomic mutations) |
| count_index_conformance_test.go | 6 | COUNT index |
| isolation_conformance_test.go | 8 | Snapshot vs serializable |
| conflict_conformance_test.go | 9 | Write-write, read-write conflicts |
| scan_conformance_test.go | 6 | Scan ordering, limits |
| reverse_scan_conformance_test.go | 6 | Reverse scan + continuations |
| continuation_conformance_test.go | 3 | Cross-platform continuation resume |
| index_conformance_test.go | 5 | VALUE index entry format |
| fanout_index_conformance_test.go | 7 | Fan-out indexes |
| composite_index_conformance_test.go | 3 | Composite PK dedup |
| rebuild_index_conformance_test.go | 4 | Index rebuild cross-validation |
| version_conformance_test.go | 4 | Record version inline storage |

### Unit-Tested Only (no Java cross-validation)

- RangeSet (32 tests) — wire format not cross-validated
- SUM index (11 tests) — wire format not cross-validated
- Index state (14 tests) — persistence across reopen not tested
- Cursor combinators (39 tests) — Go-only

---

## GAPS — Prioritized Missing Conformance Tests

### CRITICAL — Wire format at risk

1. **SUM index conformance** — New atomic index type with no cross-platform validation. ~6-8 specs.
2. **RangeSet wire format** — Foundation for index building. ~4 specs.
3. **DeleteAllRecords cross-validation** — Clears 9 subspaces, easy to miss one. ~4 specs.

### HIGH — Important for production

4. **Store header format** — Format/user/metadata version persistence. ~2 specs.
5. **Index state persistence across reopen** — ~3-4 specs.
6. **FormerIndex tracking** — Prevents subspace key reuse. ~2 specs.
7. **Store delete+recreate lifecycle** — ~3 specs.

### MEDIUM

8. **INDEX_STATE_SPACE (subspace 5) format** — ~2 specs.

---

## Summary

- **Strong foundation**: 149 conformance + 453 unit specs
- **8 gaps identified**: ~26 new specs needed
- **Release-blocking**: SUM index, RangeSet, DeleteAllRecords conformance (CRITICAL)
