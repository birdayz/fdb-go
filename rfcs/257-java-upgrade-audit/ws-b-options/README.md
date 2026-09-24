# WS-B ignored options and TEXT descriptor validation

Intermediate WS-B section 4 implementation, NOT completion of metadata evolution
or the whole milestone. Java reference: 4.14.2.0 at
fdacd162a9c8acfadc49082b89185c823ab8ae4a. Published Go HEAD is unchanged at
71ccd8cf8b3fd0dbafe283e91171818e36af555e. No commit/push/merge/PR-state changes.

## Implementation and source alignment

MetaDataEvolutionValidator now supports Set/GetIgnoredIndexOptions and AsBuilder.
Exact case-sensitive names, deduplication, nil clearing, sorted defensive getters,
independent builder/validator copies and all existing flags are pinned. Compute
changed options once, subtract exclusions and pass the mutable remainder to
ValidateChangedIndexOptions. The exported entrypoint validates only supplied
names; type handlers may remove names without recomputing differences. Explicit
unique exclusion matches Java; unrelated structural/unique checks stay strict.

Java sources: MetaDataEvolutionValidator.getChangedOptions, builder constructors,
setIgnoredIndexOptions/asBuilder; IndexValidator.validateChangedOptions(Index, Set).
The supplied-set path exposed an invalid assumption in the TEXT handler: raw
option differences are not necessarily different tokenizer names. Ported Java's
TextIndexMaintainerFactory comparison of resolved tokenizer names, including
implicit/explicit default and tokenizer resolution errors.

The live-Java default-tokenizer probe exposed another actual gap: Go built TEXT
indexes over numeric fields whereas Java rejects them. Java's per-record-type
TEXT validator uses the descriptor list returned by KeyExpression.validate.
Go validation now returns those ordered leaf descriptors from its existing
recursive traversal (constants contribute none, nesting contributes child fields,
composites/lists concatenate). Metadata build retains TEXT field lists for
associated and universal indexes, checking the first ungrouped descriptor is
string and not repeated. No independent expression-text detector was introduced.
For an absent text field, Java's strict '>' bounds guard can fall through to
List.get(size); Go returns a typed KeyExpressionError rather than panicking.
The empty-field regression pins that boundary.

Ten existing TEXT option-evolution fixture constructions in
metadata_evolution_validator_test.go used numeric price. They now use the nested
string flower.type so they reach option evolution rather than fail schema build;
their assertions are unchanged. New regressions retain numeric/repeated rejection,
grouped/nested success, missing text and universal-index coverage. The descriptor
result test asserts explicit field names and containing message across 14 cases.

## Evidence

Final source/build population: 4530 files in options-final-freeze.json, verified
unchanged after both final runs below.

- options-final-just-test.log: 93 targets PASS; 43 executed, 50 cached; 700.602s.
- options-final-race.log: uncached full recordlayer target PASS; 3329/3330 Ginkgo
  specs, with the existing opt-in million-record case not enabled. Explicit
  TestValidatedKeyExpressionFields RUN/PASS records include all 14 subtests.
- options-final-java.log: 11 focused live-Java specs PASS, with nine
  IGNORED_OPTIONS_INTEROP records and two TEXT_BODY_INTEROP records. Go-written
  metadata is validated by Java, reserialized by Java and validated again by Go;
  each result is asserted against an explicit expected value. Includes ignored
  additions/removals/changes, case sensitivity, strict default, explicit unique
  exclusion, unrelated unique rejection, implicit/explicit default tokenizer,
  and numeric/repeated TEXT rejection.
- FDBMetaDataStore integration test persists the configured option change,
  verifies cold current/history reads, and proves a failed unrelated uniqueness
  evolution changes neither current metadata nor history.
- ignored-options-red.log: six specs ran, four failed before policy wiring.
- ignored-options-tokenizer-red.log: eight ran, tokenizer-default case failed.
- text-body-red.log: numeric TEXT metadata build wrongly succeeded before fix.
- Six option mutations and four TEXT descriptor mutations compiled, ran and were
  killed on the 3330-Ginkgo-spec tree; scripts/JSON/individual logs retained.
  Option focus ran nine specs, descriptor focus one composite matrix spec.
- ignored-options-supplied-set-build-failure.log is an UNcredited mutation:
  nogo SA4009 rejected the first overwrite spelling; corrected mutation compiled
  and was killed. Java transport-red is also UNcredited semantic evidence:
  the first adapter call used JSON base64 instead of the harness's integer array.
  Java text-body-red is the retained real numeric-TEXT mismatch; corrected
  fixtures plus new explicit rejection tests pass in options-final-java.log.

The earlier ignored-options full/race/focus logs are historical intermediate
populations, not final verification. Replacement evidence remains separately in
../ws-b-retirement/lifecycle/README.md.

## Still open within the accepted milestone

Union-field-number metadata identity, descriptor-pair recursion/bijection,
canonical/default/alias union tags, generated-message descriptor compatibility,
index scope ordering/sinceVersion rules, rank-valued scans, remaining
interoperability/performance verification and final actual implementation ACKs.
No final WS-B ACK or migration/PR-green claim is made by these local greens.
