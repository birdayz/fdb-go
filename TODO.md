# TODOs

Java Record Layer version: **4.11.1.0**. FDB wire protocol: **7.3.75**.

---

## Active work

### Bytes IN-list Ginkgo harness flake (491→492/492)

1 remaining cross-engine conformance failure. `bytesAdvancedScenario` query #2: `SELECT id FROM t WHERE payload IN (X'DEADBEEF', X'CAFEBABE') ORDER BY id` returns 0 rows in the Ginkgo shared-container context. Same code passes in 4 independent test contexts (yamsql, FDB integration, goSQLRunner, bytes_advanced scenario). Needs FDB transaction-level tracing within the Ginkgo shared container to diagnose.

### NormalizePredicatesRule existential guard

Go skips NormalizePredicatesRule for SelectExpressions with Existential quantifiers. Java fires on all. Root cause: `ImplementSimpleSelectRule` requires exactly 1 quantifier (line 34). Fix: handle ForEach+Existential in ImplementSimpleSelectRule. Documented in DIVERGENCES.md.

### DecorrelateValuesRule test gap (22/29 Java tests)

7 remaining Java tests need push-down-into-child infrastructure or Reference multi-member variant testing.

### Covering index for SQL (multi-shift)

Port `IndexKeyValueToPartialRecord` (826 LOC), `computeIndexEntryToLogicalRecord`, `CollapseRecordConstructorOverFieldsToStar`. Root cause fully mapped in DIVERGENCES.md.

---

## Remaining TODO items

### Phase 5 — DDL + cache + driver completion

- [ ] **#29** D1 DDL action types — `CreateTableAction` / `CreateIndexAction` / `DropTableAction` / `DropIndexAction` / `SetStoreStateAction`. These do NOT exist in Java 4.11.1.0's MetadataOperationsFactory — Go-only extension for standalone DDL UX. Low priority for conformance.
- [ ] **#30** D3 Online indexer integration via DDL — CREATE INDEX triggers background build. Gate: #29.
- [ ] **#33** D5 driver adapter gaps — remaining: custom `database/sql.Scanner`/`driver.Valuer` types for complex values (Struct/Array/Versionstamp/Continuation). Low priority — standard scalar types cover 95% of use cases. NamedValueChecker, ColumnTypeNullable, ColumnTypeLength all done.
- [ ] **#34** E1 Go-vs-Java SQL perf bench — Go-side benchmarks done. Remaining: Java conformance server for cross-engine comparison.

### Phase 6 — Cross-language verification + perf

- [ ] **#35** A4 INFORMATION_SCHEMA cross-engine byte-equivalence. Gate: upstream (Java doesn't support INFORMATION_SCHEMA).
- [ ] **#36** Catalog wire format reverse direction (Go writes → Java reads). Needs Java conformance server.
- [ ] **#37** E2 ANTLR parser DoS hardening. Gate: upstream ticket.
- [ ] **#39** Go-only GROUP BY — Java rejects ALL GROUP BY forms. Go has full support. Keep as Go extension; revisit when Java adds GROUP BY upstream.

---

## Completed (summary)

All Cascades planner subsystems ported: ~65 rules, 34 plan types, 48 value types, 18 properties, 12 match candidates, 24 comparison operators, 9 predicates. Phase 1 (quick wins #1–#9), Phase 1.5 (divergence fixes #40–#64), Phase 2 (Cascades core #10–#13), Phase 3 (rule batches #14–#19), Phase 4 (executor #20–#25), partial Phase 5 (#26–#28, #31–#32), partial Phase 6 (#38, #99). Cascades alignment items C-1 through C-7 done. D-2/D-4/D-5/D-7/D-8/D-11 done. Java alignment refactors 1–3 done. 6,553 tests, 105 fuzz targets, 491/492 cross-engine conformance specs, 1725 yamsql scenarios.
