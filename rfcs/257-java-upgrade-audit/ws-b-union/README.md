# WS-B union identity and index-scope implementation checkpoint

Java 4.14.2.0 fdacd162a9c8acfadc49082b89185c823ab8ae4a.
Published Go HEAD remains 71ccd8cf8b3fd0dbafe283e91171818e36af555e.
No commit/push/merge/PR-state change; no final implementation ACK claimed.

Implemented:
- Identity follows old/new union field numbers, using the metadata's selected
  union (including custom names). Descriptor mappings are bijective; retained
  regressions reject split, merge and removal. Renames include unchanged names.
- Recursive schema validation memoizes descriptor PAIRS, so one old child paired
  with two different new children is checked twice; recursive self-edges terminate.
  The final name-based validation pass is gone. Unionless schemas use typed-key
  inference separately; mixed union presence is rejected. Historical synthetic
  key-inference tests explicitly identify themselves as unionless now.
- Builder resolves field.Message(), never the spelling of an underscore-prefixed
  union field. Default type key is the smallest alias tag; serialization prefers
  the canonical name, otherwise the highest tag. Both deserializers accept every
  alias. A generated message factory is reused only for the current descriptor;
  revised/imported descriptors use dynamicpb.
- Existing-index record scope is validated before rewritten expressions. The
  general allowNoSinceVersion setting no longer waives the strict newer-since
  requirement for scope expansion. The old test asserting that waiver was
  corrected against Java MetaDataEvolutionValidator.validateIndex (lines669–693);
  union-scope-red.log records its newly strict assertion failing before the fix.

Retained verification:
- union-red.log: both new tag-identity/alias tests executed and failed before fix.
- union-scope-green.log:158 focused Ginkgo specs PASS (population3332), including
  real-FDB old-write/name-swap/reopen/rewrite and revised-descriptor persistence.
- union-java.log:3 live-Java specs PASS. Custom Envelope union swaps are accepted
  or rejected against explicit expected results by both engines; Java-produced
  metadata reopens in Go. Go's tag1 record is read as Beta in Java, Java writes
  tag9, and Go cold-reads both records with intact payload and type key1.
  Output has two UNION_METADATA_INTEROP and one UNION_FDB_INTEROP records.
- union-final-just-test.log:93 targets PASS,19 executed/74 cached,659.719s.
- union-final-race.log:uncached whole recordlayer target PASS,3331/3332 Ginkgo
  specs (existing opt-in million-record case not enabled), plus explicit RUN/PASS
  records for the new union unit tests and split/merge/removal subtests.
- union-final-freeze.json:4531 source/build files verified unchanged after full
  and race runs, and again after restoring eight compiled mutation kills.
- union-mutations.py/.json and individual logs: smallest-key selection, canonical
  preference, alias map, known-type alias decoding, factory descriptor matching,
  pair memoization, name-based identity and since-version waiver all killed on
  the3332-spec tree. Focus ran3 Ginkgo specs plus the selected union unit tests.
  union-restored.log confirms the restored focused tree passes.

The first full run's three failures are preserved, not waived:
1. A new test called the builder's deliberately panicking GetRecordType for an
   absent type; fixture now checks membership before lookup.
2. The selected-union helper became dead production code; removed, with tests
   calling the real metadata accessor rather than adding a gate exemption.
3. Existing Java-metadata scan conformance silently discarded dynamic messages
   with a *gen.Order type assertion. It now asserts the actual loaded descriptor
   and decodes protobuf bytes into the expected test value without dropping rows;
   original row count and value assertions are unchanged.

Still open: finish the accepted metadata acceptance matrix (including broader
Java descriptor/index-association and failed-evolution persistence cases), rank
values, final milestone performance/interoperability checks and actual reviews.
This checkpoint does not claim the public proto-editor API exists or WS-B done.

## Follow-on: persisted metadata history

The custom-union fixture now carries the actual UNION extension, so its selected
union survives protobuf persistence in both engines, not just an explicit builder
argument. A retained real-FDB test proves an incompatible tag-paired schema is
rejected without changing current metadata or creating a history entry. A valid
swap then persists, archives the old version and cold-reloads with type key1 and
preferred tag9 intact.

Latest evidence: union-history-focused.log (4 focused Ginkgo specs plus Go unit
tests PASS); union-history-just-test.log (93 targets PASS,2 executed/91 cached);
union-history-race.log (uncached full recordlayer3332/3333 Ginkgo PASS; existing
opt-in million-record case not enabled). All4531 source/build files in
union-history-freeze.json stayed unchanged across full/race verification. Earlier
mutation and final-run logs above describe their stated3332-spec population.
The accepted metadata matrix and final milestone gates remain open; this closes
the persisted-current/history atomicity case, not every remaining acceptance case.

## Follow-on: nested enum, name reuse and index association interoperability

The Java fixtures now include nested enum State descriptors and a persisted
by_payload index associated with the identity being renamed. Both engine
round-trips preserve Beta.State.READY=1 and the Beta index association. The real
FDB Go→Java→Go case checks the enum value and both exact index entries after
Java writes the second record. Additional positive/negative Java fixtures cover
Alpha→Beta, Beta→Gamma and a genuinely new Alpha at tag3: valid associations
succeed and moving the existing index to Gamma is rejected by both engines.

union-matrix-java.log:5 focused Java specs PASS, with five explicit interop
records. union-matrix-just-test.log:93 targets PASS,2 executed/91 cached.
union-matrix-race.log:uncached full conformance race target PASS,1377/1496 Ginkgo
specs (119 excluded by the existing target selection). union-matrix-fuzz.log:
FuzzRecordMetaDataFromProto ran all12 seeds then6,986,509 executions in30s with
four workers, PASS without coverage guidance. All4531 hashes in
union-matrix-freeze.json unchanged. Production Go sources and recordlayer tests
are unchanged from the prior full recordlayer race pass above.
These cases close the broader descriptor/index-association interoperability
follow-up named at the previous checkpoint; final milestone review remains open.
