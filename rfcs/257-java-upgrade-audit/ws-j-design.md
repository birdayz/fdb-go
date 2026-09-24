# RFC-257 WS-J design — catalog existence policies and index-definition fidelity

Status: design v16, for Graefe + Torvalds + storage review. v16 applies the owner's ruling of
2026-09-24 that data written by pre-release Go builds is not supported, and answers v15's
Graefe NAK (`ws-j-design-review-v15/graefe.txt`; the v15 Torvalds and storage lenses were stopped
when the ruling made their subject moot). Section 4d is rewritten, and its decisions supersede
v15's 4d and every earlier legacy-framing decision:
- Go reads every records file as the target reads it, on every path; the landed shape (d)
  refusal is deleted on this tree, with its tests turned to the target's outcome;
- v7 to v15's migration, edited-file route, writer argument, R1, R2, reservation rule, store-open
  probe and detector are withdrawn (none had landed), and 9 (p), (r) and (y) with them;
- `SetRecords` processes the target's extension options (schema, record type and field options);
- the deserialization port (3.6) with v15 Graefe's corrections: `NullStandin`'s meaning and its
  three read sites, function-key arity and column size, the refusal of a key function the target
  cannot load, the stored-bytes reach of absent children, and Go's four untyped refusals declared;
- 9 (x)'s Java fact, 9 (z)'s wording and a citation corrected.

v15 answered the three v14 NAKs
(`ws-j-design-review-v14/`) in a new section 4d, whose decisions supersede the v14
sentences marked [v15 → 4d] at their sites:
- one framing criterion for shapes (d), (f) and (g) (read as the target reads wherever the
  target loads), which flips the landed ordinary-load refusal of (d), with the owner's
  confirmation made a merge gate;
- the implicit key stated for every pre-upgrade framing;
- the explicit key named as `RecordType.explicit_key`, and `SetRecords` reading the option
  as the target's in-code path does;
- the store-open probe made sound (every key type-prefixed, no type keyed at the number,
  snapshot read, a conditional remedy, index roots and the count key covered, a detector
  for operators);
- `checkReservedNumbers` pairing as-framed;
- 3.6's limits: function names still loaded, arity for Go's registered functions,
  parse-level required fields, absent children, count-key wrapping, and `NullStandin`
  ported, a wire fix;
- smaller corrections to (f), (x), (z), the lock helper and the mutation's binding.

v14 answered the three v13 NAKs
(`ws-j-design-review-v13/`). Each change is marked v14 at its site.

The framing shapes (4c; all three gates):
- v13 refused, on every load, a union both builds choose where the pre-upgrade framing
  differs from the target's (shapes (f) and (g)). Its (g) also compared the wrong
  number: Java's implicit record-type key is the SMALLEST union field of the type
  (`RecordType.java:171-179`), not the field records are written under.
- Such a file's records may be either engine's, and nothing says which. A refusal
  would lock Go out of every store Java wrote in those shapes. So v14 WITHDRAWS
  `LegacyFramingError`, and the load reads such a union as the target does.
- A store of those shapes that only pre-upgrade Go wrote is re-framed through the
  edited-file route, which now takes the records' writer as an argument and compares
  record-type keys at (iii).
- The one legacy form a bounded read can find, a type keyed by its last field whose
  primary key begins with `recordType()`, is refused at store open.
- The rest is a declared residual and an owner report item (9 (y)).
- A scalar `_T` (shape (e)), which Java cannot load, is framed as pre-upgrade Go's
  without being asked. Java's `test_records_import.proto` is pinned as loading.
- The v13 finding on the refusal's scope (in-process `SetRecordsWithUnionName` and
  `SetRecords` framing) falls away with the refusal. The route reconstructs only STORED
  metadata, whose pre-upgrade reading was `findUnionDescriptorName`'s. A store whose
  records were written through in-process metadata that a pre-upgrade `SetRecords` framed
  otherwise than that loader is outside what the route reconstructs, and 9 (y) declares
  it.

The restore (section 2):
- A fourth check runs the relational validator, so an enum's value order and a VECTOR
  column's options are compared.
- The no-rename check is `SetDisallowTypeRenames`, which Go already had.
- The literal-carrier arm is symmetric for the restore, so a WIDENED pair is admitted in
  either order (v13 said so, and check (i) refused one order).

The route (4c):
- R2's indexes must be at last-modified stored + 1.
- Reservations are checked by all three metadata writers, over every reservation, and a
  dropped one is refused (declared).

Also:
- Key-expression deserialization is completed as Java's (3.6), with Java's class
  hierarchy and the loader's wrapping.
- `CreateTemplate`'s concurrent writes are pinned (9 (z)).
- (x) declares that an inverted history has no exit, and names a later ordinary save as
  R2's remedy.
- The lock helpers are named by what they do with the locks.
- Stale text is swept: section 8 step 3, the section-9 cross-reference, a line citation,
  the CHANGELOG plan, and the table's RepairSchema row.
- DIVERGENCES.md gains the PENDING entries for (w), (x) and (y), and the reservation entry
  is corrected.
- The mutation record v13 lacked is `ws-j-oracle/evidence-v14.txt`.

The v13 paragraph follows. Its ordinary-load refusal, its (g) comparison, its three-check
restore and its "never an exported method" are superseded as above.

v13 answered the three v12 NAKs
(`ws-j-design-review-v12/`). The restore's check (iii) is sound across two histories: an
index both versions define must be EQUIVALENT or WIDENED unless the higher version raised
its last-modified version above the LOWER version's metadata version (a store bound to the
lower version rebuilds exactly those), and check (i) states its own options,
`allowIndexRebuilds` among them (v12 exempted any raised index and cited the rebind's
options, which lack that flag until step 3). A union both builds choose is defined by the
pre-upgrade loader's `findUnionDescriptorName` (UnionDescriptor, else RecordTypeUnion,
else a usage=UNION message, each framed name-first), and the ORDINARY load refuses such a
union whose two framings differ visibly (a `_X` typed by another message, two union fields
of one type with an implicit record-type key), not only the edited-file route; the
as-framed union handed to the validator is a new message nothing references, so a holder
of the kept union is compared against its declared fields, and a union that is both a
holder and retyped by its framing is refused. R1's reservation pairs messages as the
validator does and is monotone (dropping one is refused), and the two relaxations have
one named hook. `CreateTemplate` refuses an exact duplicate first, as Java does, and its
other refusals are declared (section 9 (w)); the restore's own refusals and R2's scoping
are declared ((x)). The locked sections call unlocked internal helpers. The WIDENED lane
arm, which no lane-table row can reach, is not built; the table property that makes it
unnecessary is pinned instead. Code on this tree: the absent-root refusal is Java's
`KeyExpression.DeserializationException` (`KeyExpressionDeserializationError`), and Go no
longer builds or writes a rootless index (`Build` and `indexToProto` refuse it);
`HeldBy`'s array-element arm is pinned, and its seed branch for a union-less metadata,
which no built metadata reaches, is deleted. Citations to the validator name the function
and the arm's message instead of a line. Evidence: `ws-j-oracle/evidence-v13.txt`. The
v12 paragraph follows; its restore exemption, its "no writer stores a version at or below
the latest", its duplicate order and its lane rule for WIDENED are superseded as above.

v12 answered the three v11 NAKs
(`ws-j-design-review-v11/`). The restore admits a version only when it is ONE HISTORY with
every stored version of t: carry compatibility, the evolution validator with the rebind
options plus no rename in either direction plus every index field EQUIVALENT at an equal
last-modified version, the predicate included (v11 compared record-type keys and union
numbers, which admitted the predicate-only difference the design names as the hazard and
a rename collision); a fresh t's guard range is the whole `(t)` prefix. `CreateTemplate`
refuses `v′ <= latest` and runs the relational validator itself, so no build-path writer
stores a version at or below the latest. The edited-file route hands the validator the stored side
AS-FRAMED, since the validator pairs union fields by declared type and would undo the
name-first framing, compares the framing directly at (iii) and in the migration's output
check, defines the direction for shape (b) and for a union both builds chose, scopes R1
to messages the validator compares, refuses R1 beside an index predicate or a record
count key through the removed field (neither is checked by the load), enforces R1's
reservation on every later save, and scopes R2's rebuild admission to its indexes, which
the default validator refuses. A WIDENED index is lane-checked in both forms and refused
only when widening removes a lane, so a pre-upgrade tenant's lane-less key carries. The
in-memory `SaveSchema` and `RepairSchema` hold the store mutex across their template reads.
The carried route marks a (d) refusal as a template being stored. Code on this tree: an
absent index root is refused as Java refuses it (both engines, a fifth WSJIXK shape), and
`HeldBy` is found by reachability from the union (a STRUCT nested in a STRUCT, a new WSJTT
shape, and its value asserted per shape). Stale citations to the validator are re-taken.
The v11 paragraph follows; its restore check, its lane rule for WIDENED and its "nothing
exported takes a template's bytes" are superseded as above.

v11 answered the three v10 NAKs
(`ws-j-design-review-v10/`). The edited-file route has its own entry,
`SaveEditedLegacyMetaData`, and loads its stored side AS THE RECORDS WERE WRITTEN: the
named union is taken as the union by name (no `fetchUnionDescriptor` search, which v10's
`SetRecordsWithUnionName` path ran and which refuses the pre-upgrade choice in every (d)
file), framed toward the pre-upgrade choice by the pre-upgrade loop's name-first rule,
with `validateRecords` and the (d) refusal suspended and every other check of the load
run; the validator then applies two relaxations only, R1 (a holding field of either
candidate removed, its number reserved; a retype goes through the ordinary field-by-field
check, which refuses one that re-reads bytes under other numbers) and R2 (an unsigned
field made signed, under arm (c)'s rules), and a migration output is validated by its
re-application alone, so the route has one spec (4c). `CreateTemplate` is the one write
entry of both catalogs and carries a new version of a stored name itself: v10's public
bytes writer `CreateTemplateFromProto` is dropped, and the lane check runs over every
index carried as a rebuilt proto, WIDENED included (3.2, the write-path table). The
in-memory catalog's guard is wired (its template catalog holds the store catalog and takes
its mutex first), it refuses `DeleteTemplateVersion` while a schema binds the version, and
its `CreateTemplate` runs the (d) refusal (section 2, 4c). The restore checks md against
every stored version of t by the carry invariant, so a catalog that already holds a fresh
t beside schemas bound to a dropped version is not made a two-history catalog, and its
limit-1 read's conflict range is stated as it is (section 2). Code on this tree, measured:
the loader reads an index's options before its root and key, as Java does (a repeated
option beside an empty key, pinned on both engines); `LegacyUnionTemplateError` marks the
(d) error in place, keeping every wrapper, only for the SQL family (`HeldBy`), with no
rename remedy for a stored template (`Stored`); a former index's subspace key is read as
Java reads it, `SetSubspaceKey(nil)` is refused, `RecordCoreArgumentError` renders only
its set fields, and the subspace-key specs read Go's side from marshalled bytes and
require the class per shape (the last four are in ws-c-design.md 7.7, where the
subspace-key comparison is ported). Evidence: `ws-j-oracle/evidence-v11.txt`. The v10
paragraph follows.

v10 answered the three v9 NAKs
(`ws-j-design-review-v9/`). The edited-file route runs the ORDINARY save's evolution
validation: it loads the stored bytes with the union named (the (d) refusal suspended for
that one load), validates old to edited with the store's `MetaDataEvolutionValidator`,
whose union correspondence is the framing check, relaxes only the holding fields of type
UnionDescriptor (removal, or a retype to another message), and archives the old bytes as
`SaveRecordMetaData` does (4c, (p)); v9 checked framing alone and would have admitted a
changed primary key or an index edited in place. The restore decides "some schema binds
(t, v)" inside its restoring transaction (a limit-1 read, and a read of t's template
rows), so a DROP SCHEMA followed by a fresh CREATE SCHEMA TEMPLATE in the window cannot
put two histories under one name (section 2, with the interleavings as FDB tests). The
version guard applies to the in-memory catalog too, since its `RepairSchema` rebinds by
name. The carried route is specified end to end: `LoadTemplateProto` reads the stored
bytes, `CreateTemplateFromProto` runs `deserializeTemplate`'s whole path in both catalogs
(the (d) refusal's 0A000 included), `fleet.SaveTemplate` returns the stored template, and
the lane check runs over the indexes a save defines, never an index carried unchanged, so
a template Go stored with one of the eight lane-less DDL shapes keeps it until a version
omits it (3.2, (m), (t)). (t) is scoped: from step 3 the ten single-fault key shapes and a
new two-fault shape agree with the target; a lane-less operator in a WHERE and the
precedence against translation-time faults stay Go's until step 6; an operand with no
Java Value fails closed. Measured and pinned: WSJLANE's two-fault shape (the later table's
42F18 in both engines); WSJIX's `index_type` beside a stored option list (both ignore the
list); the two SQL shapes the replay names and nothing had measured (a column named `_A`,
a table named "UUID"), refused by Go on the target's stored template and through its
driver. Two code changes: the loader reads a stored SUBSPACE KEY as Java does, refusing an
empty key, a key of several items and a null item with Java's classes and messages where
it used to fall back to the index's name (measured on both engines); and the SQL
family's 0A000 names the SQL remedy (another name for the table or STRUCT) instead of
passing on the records-file edit. Superseded text is corrected in DIVERGENCES.md (the (t)
entry, the guard's title and the restore's F11 limitation), TODO.md (a merge-unit block
at the end) and the umbrella's gate 8. The v9 paragraph follows.

v9 answered the three v8 NAKs
(`ws-j-design-review-v8/`). The EQUIVALENT carrier has a named write route: a new
version of a stored name is written by `api.SchemaTemplateCatalog.CreateTemplateFromProto`
from the carried `gen.MetaData`, both catalogs, with a catalog-layer pin that the spliced
Index message survives and a driver test that an index stored with `value_expression`
survives a new version made by DDL (3.2, the write-path table's carried-version row).
The version guard reads bindings from `(t, latest + 1)`, since a save may skip versions
and v8's floor at v′ missed a skipped one (a three-step sequence, now an FDB test,
section 2). The restore refuses a version no schema binds, reads headers through the
caller's keyspace (`RestoreTemplateVersion(ks, …)`, so over Java-created schemas it
refuses until F11, declared in (s)), and its conflict-range claim is scoped to writes
after the final transaction's read version. The lane check types every operand by the
RESULT type of the Value the target builds for it, so `bitand(order_desc(a), 1)` is
(BYTES, INT) and refused and `x & cardinality(a)` (LONG, INT) is accepted; the DDL-origin
XX000 lands in step 3 with the build-path check, the key generator consulting the same
lane table, so no commit gives a DDL shape Go's 42F59 (section 8, (t)). The (d) replay
is stated as "only if" with its omissions, which all err toward refusing (4c), and the
SQL family is defined by the replay (a column the loop frames, `_X` included), in the
design, CHANGELOG, DIVERGENCES and the code comments. An EDITED records file whose
framing matches the stored one is admitted by `SaveMigratedLegacyMetaData`, which
unstrands the holder shapes ((p)). The template-statement walk checks an IN predicate at
entry, pinned by `x IN (SELECT … LIMIT 1)` (the nested-SELECT message) and `x IN
(EXISTS (SELECT … LIMIT 1), NULL)` (the NULL message; the grammar has no scalar subquery
in an IN list) (3.3). Tests: WSJIX now asserts the options Java returns (the
`index_type` UNIQUE case's `{unique=true}`); WSJLANE pins Go's outcome on this tree
(`goNow`: eight OK, two XX000), so the "Go stores eight" premise reddens if it moves;
the faults test reports every arm (`t.Errorf`), and the v8 mutations wm8-1 to wm8-12,
re-run under Bazel as wm9-1 to wm9-12, redden as before, the check-first mutation now
all six fault arms (evidence-mutations.txt, v9 block, with wm9-17 to wm9-19 for the
v9 pins). `AmbiguousLegacyUnionError` names the remedy that applies on this tree ("edit
it so that only the union its records were written with is a candidate"), and the
CHANGELOG tells users to rebuild a `value_expression` index a pre-upgrade Go build
maintained. The v8 paragraph follows.

v8 answered the three v7 NAKs
(`ws-j-design-review-v7/`). Shape (d) is decided by what the pre-upgrade build could have
FRAMED, not by reachability (4c): the pre-upgrade union loop is replayed over the message
named UnionDescriptor (`legacyUnionRecordTypes`, name first as its switch was), a message
it made no record type of loads wherever it sits, and for STORED metadata the pre-upgrade
loader's outcome is replayed too (every record type the loop made needs a stored primary
key, and no index may cover a record type it did not make), so a table named
UnionDescriptor with a STRUCT or UUID column loads while one with a table-typed column is
refused (landed; 17 unit shapes on four paths, 35 JVM descriptor shapes, five target DDL
shapes through Go's catalog and driver). The Go catalog now refuses to STORE a template
its loader would refuse, where v7 stored one and then failed every load (measured), both
with 0A000; `FDBMetaDataStore.SaveRecordMetaData` refuses metadata built with a named
union over such a file, and `RefuseAmbiguousLegacyUnionAsStored` says so in advance
(landed). The loader reads a stored index's `value_expression` and absent `added_version`
as Java does (landed; five protos equal to Java's reading on the JVM). The EQUIVALENT
carrier is the stored `gen.MetaData` bytes the save validates, not a field on `Index`; the
version guard reads bindings at or above the saved version (v9 moves the floor to the
latest stored version plus one, section 2, since a save may skip versions and v8's floor at v′ missed the
skipped ones), which closes the
restore/create/restore two-history sequence; the restore resolves stores through the
opener's keyspace function, refuses a missing header, and holds a read-conflict range
instead of re-reading; the write-path table gains `Initialize` and the (d) column; the
lane check is scoped to the ArithmeticValue family with nested result types, and Go's DDL
producing no-lane keys is measured (the target refuses ten shapes at the clause, Go
stores eight) and answered at the clause by section 6; the template-statement walk also
refuses an IN over a nested SELECT and a NULL in an IN list; the migration's arms follow
the new rule (a field referring to RecordTypeUnion refuses its removal, a self-holding
union may be removed); DIVERGENCES gains the pending (j), (o) and (t) entries. v7 answered the three v6 NAKs
(`ws-j-design-review-v6/`): shape (d) is refused by `Build` whenever the union was found
rather than named, so metadata built in code with `SetRecords` is refused as stored
metadata is (landed; the pre-upgrade `SetRecords` also took UnionDescriptor by name), and
the data-message exemption is narrowed to messages the found union REACHES through its
fields, so an envelope holding legacy unions is refused (landed; 31 descriptor shapes on
both engines, and the UnionDescriptor spec now runs through Go's catalog and SQL driver);
the EQUIVALENT class compares the known Index fields as Java's `new Index(proto)` reads
them and carries unknown fields and extensions, uncompared, through a stored-proto carrier
`indexToProto` emits, claiming proto equality rather than bytes; the no-lane check sits
on the BUILD path (both `CreateTemplate` implementations), every template write path is
enumerated with the checks it runs, and the raw-write routes (the restore, the F11 copy,
the fixtures) are named; the lane table is ArithmeticValue's whole operator table, which
section 6 extends; the restore refuses only a header ABOVE the restored version and bounds
the stores it checks, and the fresh-template guard covers the whole name; LIMIT and OFFSET
are checked over every limit clause of the template statement in tree order; test 5's
fixture comes from the pre-port builder, stored raw, and names its discriminating reads;
the F2b spec measures the saturated operand itself (landed); DIVERGENCES records the
synthetic-type refusal; stale counts corrected. v6 answered the three v5 NAKs
(`ws-j-design-review-v5/`): shape (d)'s refusal no longer rejects a SQL table or STRUCT
named "UnionDescriptor", which the target creates and serves (a message that is a field's
type is a data message; landed, measured through the target's DDL and on 29 descriptor
shapes); the (d) migration removes the candidate the target would otherwise still take,
so every output loads in both engines, and refuses a file that loads; the loader reads
each index's record types before the index and refuses an unknown RecordType entry, as
Java does, with the Go-only refusal after every target check (landed); section 3.5's
coverage rule is measured directly (an index-served read of a function value or its
operand fetches and recomputes in the target) and a function-key column exposes nothing;
LIMIT and OFFSET are refused where the target refuses them, over the whole template
statement; the carry rule compares the raw catalog bytes; test 5 sits above the inline
rebuild threshold and starts from the pre-port builder; the merge unit carries the
delete refusal and the restore (with a header check) in step 1 and the no-lane refusal
at the catalog's `CreateTemplate` in step 3, and the tree merges as one; the F2b spec measures
the FLOAT arm's overflow on its own rows; `SetRecordsWithUnionName` refuses an empty name
and `literalKeyCarrier` a value with no static type (landed); DIVERGENCES marks the
unbuilt WS-J entries PENDING. v5 answered the three v4 NAKs
(`ws-j-design-review-v4/`): section 4's carry rule has three classes, and a width-only
index is WIDENED (the rebuilt root with the stored versions and subspace key, decided by
the validator's own `literalCarriersEquivalent`), so the widening path actually moves a
pre-F2 tenant to the target's key; the classes compare every Index field generically, so
no difference is silently dropped; section 8 defines the MERGE UNIT (the landed fixes
with steps 1 to 3) and the umbrella RFC and TODO.md record it; section 4c adds shape (d),
metadata the pre-upgrade loader read under a different union than the target's, refused
on load (landed, Go-only, declared) and migrated with the union the caller names, and the
migration takes a named union for shape (b); the proto loader follows Java's load order
and the target's unknown-record-type message (landed); template save refuses a function
key with no lane (3.2); section 3.2 decides that 3.5 never covers a function-key value
(MEASURED: the target cannot plan the ordered projection at the INT edge, and recomputes
where it serves one); `literalKeyCarrier` refuses every unrecognised (type, Go kind) pair
(landed); and the duplicate-option spec drives Go's loader beside the JVM (landed). NOTHING
MERGES IN PIECES SMALLER THAN THE UNIT OF SECTION 8. The interim states the v3 reviews
describe (a DDL rebuild that the rebind validator refuses, a recreated template name
rebinding live schemas) exist only on this unmerged branch. The v4 changes, kept: section 3.3 names every Go expression and Value class the IndexSpec port meets on
Go's translated graph (Go lowers WHERE and HAVING to LogicalFilterExpression, SELECT to
LogicalProjectionExpression, and builds Project(Sort(Filter))), with the top-node check
Java's DdlVisitor makes, and lands after section 6; section 3.5 decides a stored function
key with no lane (MEASURED: the target fails every query of the table; Go declines the
candidate, declared) and records the literal-matching coupling to the plan cache at both
code sites; section 3.2 states the widening upgrade path is reachable only through
section 4 and makes the widening arm one-directional, and the carrier follows the static
type for every Go integer kind (the INT edges and two cross-type literals are new shapes,
byte-equal); section 4 defines an EQUIVALENT index, decides a re-added index name and
ports the validator arm Go lacked; section 4c states F13's scope, what stops loading and
the migration; section 2's guard keys on (name, version); and the instrument pins Go's
own outcome per run beside its class, builds every accepted body twice, compares the F2b
entries' raw bytes, measures the INT_MAX lane edge and the duplicate-option message on the
JVM, and hashes every landed file. Umbrella section: `rfcs/257-java-4.14.2.0-upgrade.md`
"WS-J — Catalog policies and index-definition fidelity". Java reference:
`fdb-record-layer/` at tag 4.14.2.0 (`fdacd162a`).

Every Java behaviour below is MEASURED on the live 4.14.2.0 JVM by the WS-J oracle
(`conformance/ws_j_index_fidelity_conformance_test.go`, target
`//conformance:rfc257_oracle_test`, evidence in `ws-j-oracle/`) or labelled SOURCE
with a file:line cite. Go facts about index DDL are measured on the tooling builder
(`embedded.BuildSchemaTemplateFromDDLNamed` + `ToProto`) unless they say otherwise;
the production path (`execCreateSchemaTemplate`) is pinned separately for every landed
fix (`TestFDB_IndexDefinitionProductionPathStoresTargetShapes`), and section 3.4
retires the difference.

## 0. The instrument

The umbrella asks for "exact key-expression protobuf equivalence for overlapping
supported index DDL". The oracle makes that checkable instead of sampled.

- **Corpus and census.** `conformance/testdata/rfc257/java_index_templates.json` is
  every schema template in the 4.14.2.0 `yaml-tests` corpus that declares an index,
  `CREATE VECTOR INDEX` included: 81 templates, 352 index statements, 77 files.
  `ws-j-oracle/harvest.py` also writes a statement CENSUS and refuses to write when it
  does not reconcile: every `create [unique|vector] index` occurrence in every
  `.yamsql` file (360) is either inside a harvested template body (352) or on a comment
  line (8, recorded by file and line). The spec asserts the 81 templates, the 360
  occurrences, the reconciliation, re-counts the 352 statements from the stored
  bodies, and pins `variant_duplicates == 0` (harvest.py derives that field as
  harvested minus in-body, so v2's sum check could not fail; what can fail is a
  versioned template entering the corpus, whose variants repeat statements).
- **Runs, all STRUCTURAL.** Per corpus template: the full body; a
  `{-views-functions}` body whenever it has a VIEW or FUNCTION clause, with every
  index clause that depends on a cut view or function cut too (dependency found on
  the typed parse tree, transitively); and, for every template with two or more
  index clauses, one `{index:<name>}` body per index. None of them is conditioned on
  either engine's outcome. The four templates Go's grammar cannot parse are pinned as
  `wsjUnsplittable`. Plus 63 hand-written shapes (v3 added INT literals over FLOAT and
  DOUBLE columns, a DOUBLE literal over an INTEGER column, and a quoted table name
  holding a dot and a dollar sign; v4 adds the INT literal's edges 2147483647 and
  -2147483648, the first LONG 2147483648, a FLOAT literal over a DOUBLE column and a
  LONG literal over an INTEGER column; v5 adds four `top_*` shapes, an index query whose
  top node is not a Project). Total: 81 corpus + 21 derived + 314 isolation + 63 shapes
  = 479 runs.
- **Comparison.** Java persists the template into the shared catalog; Go reads the
  RAW stored bytes; Go's DDL builder builds the identical text under the identical
  template name. Per index and per stored field (record_type, type, options, root,
  predicate, subspace_key, versions, every other field generically), each root or
  predicate difference classified STRICT (proto2-default presence only) or REAL.
  Whole-MetaData bytes, with the differing top-level fields named, before and after
  canonicalization.
- **Class.** Go-only indexes are split first: a verified RFC-209 group-existence
  companion of an index the target also stores (`isGroupExistenceCompanionOf`: COUNT
  type, no grouped column, the owner's grouping signature and predicate, valid
  versions, no clearWhenZero) is a declared Go extension; any other Go-only index is
  an index divergence. Everything is then compared against Go's metadata with the
  companions removed and the versions they took given back (`wsjRemoveCompanions`:
  each companion occupies one version slot, so every index version above a slot and
  the metadata version move down by one per slot below them; a companion whose added
  and last-modified versions differ occupies no single slot and is not removed). A
  both-accept run is:
  - `both-accept-index-diverge` when an index differs on any field (versions
    included), is missing, or is a non-companion extra;
  - `both-accept-metadata-diverge` when the indexes agree and the canonical MetaData
    does not (record-type keys, union numbers, versions, descriptor order);
  - `both-accept-companion` when it is equal once its verified companions are
    removed;
  - `both-accept-equal` otherwise: byte-for-byte what the target stores, up to the
    canonicalization below.
  Three mutations redden the class pins (`evidence-mutations.txt`, v3): giving no
  version back moves the four companion runs out of their class; refusing every
  companion moves 27 runs to index-diverge; reverting the option order (F12) moved
  three runs' classes at v3, and at v5, over the 475 runs of the run it was measured on
  (before the four both-reject `top_*` shapes, which store no options), moves 20: 19
  change class and 1 changes only its pinned digest (the v5 block of
  `evidence-mutations.txt`).
- **Canonicalization, declared in full** (`wsjCanonical`, `wsjJavaDigest`):
  `record_types` sorted by name (a HashMap's iteration order in Java,
  RecordMetaDataBuilder.java:1473, iterated at RecordMetaData.java:673); anonymous
  `__type__<uuid>` messages renamed IN PLACE by the path that reaches them from a named
  message (ProtoUtils.java:96-98 draws the UUID per build); within each maximal run of
  consecutive anonymous messages, those ordered by canonical name, because Java emits
  a table's types from a TreeSet keyed by name (TypeRepository.java:269-271) and so
  orders two anonymous types by their random names (measured to move between two
  builds of in-predicate.yamsql:20), while their position relative to named types does
  not move; for the pin only, the per-run template name (the records file is named
  after it) and the correlation aliases Java draws per build inside stored function
  and view plans (measured to move in three corpus templates) are replaced by same-
  length placeholders. Named descriptor messages are NOT reordered: their order is
  what Java emits and what section 4 ports.
- **Pins, both engines.** Every run's Java outcome is pinned in
  `conformance/testdata/rfc257/wsj_java_pins.json` (acceptance with the index count and
  a digest of the WHOLE canonical MetaData, or the rejection's SQLSTATE and exception
  class) and every run's Go CLASS AND Go's OWN OUTCOME in `wsj_go_classes.json`
  (`<class> OK <digest of the whole canonical MetaData Go stores>`, or `<class> ERROR
  <Go's SQLSTATE> <digest of its scrubbed message>`), each with set equality both ways
  and run ids asserted unique. The class pin is the ratchet v2 lacked: a run that stops
  being `both-accept-equal` reddens the oracle even where no unit golden covers it, and
  a run that becomes equal is a reviewed pin update; the outcome beside it catches what
  the class cannot, a change INSIDE a run whose class does not move (a column type or a
  root inside one of the 180 metadata-diverge or 4 index-diverge runs, a different Go
  rejection inside a reject run). The digest is sound only if a build is a function of
  its input, so the spec builds every body Go accepts TWICE in-process and asserts the
  two stored byte strings equal (354 runs in v4, `WSJ-GO-BUILT-TWICE`, floored at 300 so
  the check cannot silently stop running).
- **Query half.** "WS-J literal-width query oracle": 24 probes, the target's result
  type, value or rejection of each pinned with set equality, values printed exactly:
  bitmap and bit operators over INT and LONG columns and literals, the INT_MIN edges,
  DOUBLE and STRING operands, ENUM, UUID and ARRAY operands and `bitmap_bucket_offset`
  over an ENUM (a second schema), `NULL & 1` and `CAST(NULL AS BIGINT) & 1`,
  `bitmap_bucket_number`, `%` over INT (section 6).
- **Go stores, Java plans.** "WS-J Go-stored template planned by the target": one
  template stored four ways, through the Go SQL driver (pinned invisible to the target,
  42F55, F11; one read), through Go's catalog library at the Java-compatible
  `(NULL, NULL, 0)` subspace, through Java, and as the library-stored metadata with
  every INT literal rewritten to `long_value` (the width a Go build before F2 stored;
  the rewrite is asserted to touch a literal, and touches 3). The target runs 15 reads
  over the last three: over the library-stored template it must answer exactly as over
  its own (pinned), and over the `long_value` variant its answers are pinned too. NINE
  of the 15 reads tell the widths apart (asserted): the three bitmap reads (XX000 over
  `long_value`), the EXPLAIN of the two index-served equalities, `WHERE d & 1 = 1` and
  `WHERE d + 1 = 5` (COVERING over `int_value`; a full index scan with a FILTER over
  `long_value`, the stored LONG literal no longer matching the query's INT Value), and,
  new in v4, four reads of T1 that no bitmap expression serves (a primary-key equality,
  its EXPLAIN, an ordered scan and a count): over `long_value` every one of them is the
  same XX000, so a stored key the target cannot expand fails EVERY query of its table
  (section 3.5 decides Go's answer). The four ORDER BY reads v2 counted are UnableToPlan
  over every width; they stay, recorded as not discriminating.
- **Non-integer operands.** "WS-J long arithmetic key functions over non-integer
  operands": the target's inserts and index reads over a long-only arithmetic key
  function maintained on DOUBLE and FLOAT columns and a DOUBLE literal, and (v4) over
  an INTEGER column at the edge of its lane (`i + 1` with i = 2147483647, v5 adding the
  function value projected in the index's order, v6 the six coverage reads); 27 pinned
  target answers AND Go's 27 answers and plans pinned beside them (section 3.2).
- **Duplicate option message.** "WS-J duplicate index option": the target reads a
  stored Index proto whose option list repeats a key (`new Index(proto)`, Java step
  `wsjDuplicateIndexOptionJava`), for the exact inputs of Go's
  `TestIndexOptionOrder_FromProtoRefusesDuplicateKey` and a control without a repeat;
  the target's IllegalArgumentException messages are pinned verbatim, and (v5) the same
  inputs are loaded by Go's `RecordMetaDataFromProto` inside stored metadata in the same
  spec, whose DuplicateIndexOptionError text must equal the target's message, so a drift
  of either engine's message reddens it (a mutation swapping Go's operand order does,
  `evidence-mutations.txt` v5) (section 4b).
- **Both engines write one index.** "WS-J F2b index entries written by both engines":
  the target creates a template with `d + d` over a DOUBLE, `n + 1` over a BIGINT and
  (v5) `f + f` and `f + 1.5f` over a FLOAT column holding the same values narrowed, the
  last a FLOAT literal inside long arithmetic, the one position 3.2's float arm admits;
  the target inserts rows through SQL and the Go record layer saves the same rows into
  the SAME store (opened at the prefix the target resolves); every entry's RAW key
  bytes below the index subspace (lowercase hex from the target's step) are checked to
  end in the tuple encoding of that row's primary key, and the bytes before it are
  compared between the target-written and the Go-written row, byte for byte (the
  decoded value is printed beside them for readability), and the `n + 1` entries of
  both engines are compared with the tuple encoding of `n + 1`; the target then serves
  an index read over a Go-written row (section 3.2).
- **Plans over nested-leaf indexes.** "WS-J nested-grouping aggregate index plan
  oracle": 5 Java EXPLAIN trees (section 3.3b). "WS-J nested-leaf value index plan
  oracle": 6 reads over value indexes on nested leaves, the target's tree AND Go's
  physical tree pinned (section 3.5).
- **Production path.** `TestFDB_IndexDefinitionProductionPathStoresTargetShapes`
  (sqldriver, FDB) executes CREATE SCHEMA TEMPLATE through the SQL driver and reads the
  stored template back from the driver's catalog: the nested-then-top root, the INT
  literal of `d + 1`, the INT bitmap entry size and a permuted index's option order are
  the target's, verbatim from the oracle's WSJ-JAVA-ROOT lines.

Measured tally (479 runs; evidence `ws-j-oracle/evidence-v13.txt`, as in v5 to v12, two uncached passes
with identical Java outcomes and identical Go classes and outcomes, `WSJ-TALLY` 166 / 4 /
180 / 4 / 24 / 101), as equal / companion / metadata-diverge / index-diverge /
both-reject / Java-accepts-Go-rejects: corpus 81 = 32 / 2 / 14 / 2 / 4 / 27; derived 21 =
12 / 1 / 1 / 1 / 1 / 5; isolation 314 = 88 / 1 / 165 / 0 / 2 / 58; shapes 63 = 34 / 0 /
0 / 1 / 17 / 11. No run differs in option order (F12 landed). The
four index-diverge runs differ only in index versions from table order (F3:
`aggregate-index-tests-count-empty.yamsql:21`, `index-ddl.yamsql:27{-views-functions}`,
`index-documentation-queries.yamsql:5`, `shape:multi_table_order`). All 180
metadata-diverge runs differ in exactly `records,record_types` after canonicalization
(each run's WSJ line in the pass logs names the differing top-level fields, counted in
evidence-v13.txt: `md-canonical-differ(records,record_types)` 180 times, and the only other
value, `(records,indexes,record_types)`, 4 times, on exactly the four index-diverge runs
above; v5 to v8 cited evidence-vN.txt for these lines, which quoted only the tallies; record-type keys
and union numbers are F3), and Go's own digest of each is pinned, so a further
difference inside those fields reddens even though the class would not move; 23 of them also carry companions and become companion runs when F3
lands. The 101 Java-accepts/Go-rejects runs are, by Go's error: views 44, enum types
22, SQL functions 11, unnest and derived-table sources 21, grammar 3.

## 1. Findings

| # | Finding | Evidence | Status |
|---|---------|----------|--------|
| F1 | Field-path trie: a top-level column after a nested one dropped the nested subtree | shapes nested_then_top / nested_covering / nested_desc / deep_nesting_shared; production-path pin | FIXED (3.1) |
| F2 | Literal carrier width: Java stores the literal's own object (`int_value` for INT); Go stored `long_value`, and the target could not plan any bitmap query over a Go-stored template | Go-stored spec incl. the `long_value` variant; corpus bitmap-aggregate-index, indexed-functions; 21 literal shapes (the `literal_*` rows of the oracle corpus); production-path pin | FIXED (3.2), with the rebind arm for stored templates |
| F2b | Go's long-arithmetic key evaluator rejected every non-int64 operand; Java reads any Number through longValue() | F2b entries spec (both engines, one index); non-integer operands spec | FIXED (3.2); read side DECLARED (Go does not serve these indexes, the target does and answers wrong) |
| F3 | Table order: Java re-appends a table each time an index attaches; the final order drives record-type keys, union field numbers, descriptor order and index versions | 180 metadata-diverge + 4 index-diverge runs | section 4 |
| F4 | Bit/bitmap operators: Java checks the operands are primitive, then resolves a physical lane per operand type (II → INT, no lane for DOUBLE/STRING/NULL); Go fixes BIGINT, computes in int64 and returns NULL where Java rejects | query half (24 pins) | section 6 |
| F5 | Unnest and derived-table index sources rejected by Go, some with a misleading 42F00 | 21 runs; shapes unnest_nested, unnest_nested_derived, derived_no_unnest, unnest_outer_predicate | section 3.3 |
| F6 | CREATE TYPE AS ENUM rejected by Go | 22 runs; shapes enum_* | section 5 |
| F7 | Existence policies, CREATE SCHEMA order, a fresh template recreated under live schemas | SOURCE (section 2) | section 2 |
| F8 | Two DDL front ends | `cascades_generator.go` | section 3.4 |
| F9 | Aggregate indexes over nested leaf fields: Java serves them, Go stores but does not use them | 5 pinned Java trees | section 3.3b |
| F10 | The target cannot plan an enum comparison inside an index predicate: XXXXX RecordCoreException "attempt to create PoJo index comparison from unsupported comparison" (the index side of #4624 is still open in 4.14.2.0) | shape enum_predicate_index | section 5 |
| F11 | The Go SQL driver stores the catalog at `(__SYS, __SYS, CATALOG)` strings and schemas at `(dbPath, schemaName)`; Java uses `(NULL, NULL, 0)` and directory-layer paths, so a template created through the Go driver is invisible to Java (42F55) | Go-stored spec (pinned) | TODO.md block (now also: a migration copies MetaData bytes verbatim); put to the OWNER (wire hard line, predates RFC-257) |
| F12 | Index option ORDER: Java stores options in insertion order; Go ranged over a map, so its stored bytes varied per build | oracle `options-order` dimension: 18 runs in v2, 0 in v3 | FIXED (4b) |
| F13 | `uint32`/`fixed32` fields: Go read them unsigned, protobuf-java hands Java a signed Integer, so index and primary-key bytes differed for values ≥ 2^31; Java refuses unsigned fields in record descriptors (`validateRecords`), finds the union only by `(record).usage = UNION` or the name RecordTypeUnion, and has no union-less mode | conformance spec "Java refuses the same records descriptors" (35 shapes, both engines, precedence included); `TestKeyExpressionFastPath_UnsignedFieldsReadSigned`, `TestPreviouslyStoredMetaDataIsRefusedOnLoad` | FIXED; scope and what stops loading (4c; v16 withdraws the migration, 4d) in section 4c |
| F14 | Go's planner uses no value index whose key holds a scalar nested leaf, and no function-key index (arithmetic, bitmap): it scans and sorts where the target serves the index | nested-leaf value plan oracle (6 reads, both engines pinned); Go-stored spec EXPLAINs; bitmap AGG_BUCKET | section 3.5 (query engine) |

Rows OWNED ELSEWHERE (the Go rejection is another workstream's missing capability,
kept so the burn-down is complete): CREATE VIEW (44 isolation and derived runs) and
SQL FUNCTION / temporary functions / stored queries (11) — WS-H; `vector(3, half)`,
`USING GUARDIANN` vector indexes and vector indexes ON a view — WS-D/WS-K (two of
the four unsplittable templates are GuardiANN grammar), including the vector clause
options' stored order (Java stores them in a HashMap's iteration order after
`unique`, 4b); ARRAY_AGG index rejection code (Java 0A000 "Aggregate result value does
not align with grouping value", Go 42803) — WS-G; case-sensitivity.yamsql (Java rejects
without the corpus's case-sensitive connection option) — WS-K harness.

Closed with no change: the legacy-versus-tuple extremum question of v1 (storage #6).
The index metadata is byte-equal in both spellings (shapes `extremum_ever_legacy` and
`extremum_ever_tuple`, `both-accept-equal`), and the entries each index type writes are
compared across engines by the existing MAX_EVER_TUPLE/MIN_EVER_TUPLE and
MAX_EVER_LONG conformance specs (`min_max_ever_tuple_index_conformance_test.go`,
`min_max_ever_index_conformance_test.go`).

## 2. Existence policies (F7)

SOURCE: `SchemaExistsBehavior.java` (whole file), `RecordLayerStoreCatalog.java:
232-269` (`saveSchema`), `:271-279` (`repairSchema`), `:180-189` (initialize),
`:480-489` (`parseSchemaTable`), `RecordLayerCreateSchemaConstantAction.java:75-105`,
`CopyPlan.java:441-454`.

Port `api.SchemaExistsBehavior` with the four Java values and their exact
`shouldWrite` contracts and messages (all `SCHEMA_ALREADY_EXISTS`, 42F06):

- `ERROR` — always refuse: "Schema <db>/<name> already exists."
- `ERROR_IF_DIFFERENT` — no-op iff template NAME and VERSION are identical
  (`areSchemasIdentical`); else refuse with "... already exists with a different
  template (<old>@<v> vs <new>@<v>)."
- `DO_NOTHING` — no-op, no comparison.
- `UPGRADE` — refuse on a different template name, refuse a lower version, no-op on
  equal, write on strictly greater; Java's two messages verbatim.

`StoreCatalog.SaveSchema(txn, schema, createDatabaseIfNecessary, existsBehavior)`
follows Java's order exactly: validate the schema → database exists (create, or 42F00
UNDEFINED_DATABASE with Java's message) → template exists at version
(UNKNOWN_SCHEMA_TEMPLATE, Java's message) → load the existing row, including its
template (`parseSchemaTable`: when the existing row's template VERSION is gone the
load fails with Java's "SchemaTemplate=<n>, version=<v> does not exist"; Go today
tolerates it, `validateSchemaRebind` returns nil on UNKNOWN_SCHEMA_TEMPLATE, and
follows Java instead) → `shouldWrite` → write. A no-op issues NO write, so it adds no
write conflict range (Java's comment at :254-256).

Go's rebind validator, reconciled with the policy rather than kept beside it. Under
UPGRADE, `shouldWrite` already refuses a different template name and a lower version
and makes an equal version a no-op that writes nothing, so `validateSchemaRebind`'s
name-change arm and version-monotonicity arm (`fdb_store_catalog.go`, the
`newTmpl.Version() <= oldTmpl.Version()` check and the cross-name structural branch)
are unreachable, or reached with a different error code: they are DELETED, and so is
its second `LoadSchema` (SaveSchema passes the existing row it already loaded). What remains is the Go extension proper: on every write over an
EXISTING row (UPGRADE's strictly-greater case is the only one), the ported
MetaDataEvolutionValidator runs between the old and new template metadata and refuses
a key-changing rebind (section 4 explains why the carry rule makes that refusal
unreachable for Go-built templates, and why the validator stays as the guard for
anything else).

| Caller | Java | Go today | Go after |
|--------|------|----------|----------|
| catalog initialize | ERROR_IF_DIFFERENT, createDb=true | always writes | ERROR_IF_DIFFERENT, createDb=true; the explicit create goes |
| CREATE SCHEMA | database exists, else 42F00 UNDEFINED_DATABASE "Database <path> does not exist" (:80-82); load the template (UNKNOWN_SCHEMA_TEMPLATE); saveSchema(ERROR); then create the record store with ERROR_IF_EXISTS and map RecordStoreAlreadyExists to 42F06 "Schema <x> already exists" (:84-105) | database check first, with its own message (`database "<path>" does not exist`, `create_schema.go:43`); then a LoadSchema pre-check with its own 42F06 message BEFORE the template load; then the store; then the save | Java's order and messages: database, template, SaveSchema(ERROR), store creation with the same mapping (`RecordStoreAlreadyExistsError`, store_builder.go:1384, becomes 42F06 there); the pre-check is deleted |
| repair (RepairSchema) | UPGRADE, onto the LATEST version of the bound template (:272-279) | binds through `saveSchemaHeld`, the unlocked helper `SaveSchema` also runs, so the rebind validator refuses lower versions (section 2, the locks) | UPGRADE through `saveSchemaHeld` and its rebind validator; the in-memory catalog's RepairSchema, which bypasses the rebind validator today, runs it too |
| fleet.Migrate (`fleet/migrate.go:96`) | none (a Go fleet tool) | RepairSchema per tenant | unchanged caller of RepairSchema; covered by section 4's FDB tests |
| COPY | imports the stored MetaData BYTES (CopyPlan.java:441-454), policy ERROR_IF_DIFFERENT | no COPY statement in Go | no Go route (WS-K accounting); COPY never rebuilds from DDL, so it is not a section-4 consequence |

The in-memory catalog (`store_catalog.go`) takes the same order (it checks the
template BEFORE the database today, `store_catalog.go:75-110`, contrary to Java) and
the same four-way decision, so the two implementations cannot drift; a test runs every
policy case against both.

DROP coordination with WS-B: dropping a schema does not go through SaveSchema; the
existing header/cache invalidation owns it. No change.

A template VERSION re-issued under live schemas (Go extension, declared). Neither engine
checks for bound schemas when a template, or one version of it, is dropped (Go
`fdb_template_catalog.go:198-229` for DROP SCHEMA TEMPLATE and `DeleteTemplateVersion`,
`:232`, which the public catalog API exposes; Java
`RecordLayerMetadataOperationsFactory.java:51-53`), and a schema row names its template by
(name, version). Two Go paths therefore re-issue a (name, version) that schemas still
bind: DROP SCHEMA TEMPLATE t followed by CREATE SCHEMA TEMPLATE t (version 1 again), and
`DeleteTemplateVersion(t, v)` of the LATEST version followed by a save, because the next
version only has to exceed the remaining latest (`save_schema_template.go:36`;
`fleet.NextTemplateVersion` is that latest + 1, `fleet/migrate.go:17-33`). Either binds
every schema still naming (t, v) to whatever the new build holds, and the store's version
check does nothing when the new metadata version equals the old. Before RFC-257 the same
DDL rebuilt the same bytes; after F1, F2 and section 4 it does not (a changed root
re-reads existing index entries under a different key; section 4's numbering re-reads
rows under different record-type keys), and nothing on the read path notices. Go
therefore refuses to create a template version (t, v) while some schema binds (t, v) and
no template (t, v) is stored: 42F59 INVALID_SCHEMA_TEMPLATE "schema template <t> version
<v> cannot be created: schemas are still bound to a dropped template of that name and
version (<db>/<schema>)", naming the first bound schema. The check is one range read of
the catalog's `TEMPLATES_VALUE_INDEX` (Java's own index on `(TEMPLATE_NAME,
TEMPLATE_VERSION, DATABASE_ID, SCHEMA_NAME)`, SchemaSystemTable.java:58-63, ported in
`catalog/metadata.go`), limit 1, inside the creating transaction: for a save of (t, v′),
the bindings of t at every version ABOVE THE LATEST STORED ONE, the range from `(t, latest
+ 1)` to the end of `(t)`, whatever v′ the save names. v8 started the range at v′ and
assumed v′ = latest + 1, which nothing enforces: `SaveSchemaTemplateConstantAction`
refuses only `v′ <= latest` (save_schema_template.go:33-36) and `fleet.SaveTemplate`
passes the caller's version (fleet/migrate.go:50-54), so with (t, 3) bindings dangling,
`Restore(t, 1)`, then a save of v5 carried from v1 (admitted: nothing binds (t, 5) and
up), then `Restore(t, 3)` put an original v3 beside a carried v5. From latest + 1 the
range covers every version the save skips as well as v′ itself. One rule covers both
cases. A FRESH template (no version of t stored) has NO latest, and the range is the whole
`(t)` prefix, every version of t (v11 said latest = 0 and read from 1, which missed a
binding of (t, 0): `validateSchema` admits version 0): a fresh CREATE SCHEMA TEMPLATE t is refused while any schema binds any
dropped version of t, naming that schema and version, since a fresh t admitted beside
(t, 3) bindings would, after a restore of (t, 3), put two numbering histories under one
name, breaking section 4's premise that every stored version of a name agrees with its
latest. A NEW VERSION of a stored template leaves a schema bound to the latest or a LOWER
version of t untouched (that binding is unaffected by the new version), and is refused by
a binding above the latest. v7 read only the `(t, v′)` prefix for a new version, and that
let the two-history state in through a restore: with (t, 1) and (t, 3) both dropped and
bound, `RestoreTemplateVersion(t, 1)`, then a new v2 (carried from the restored v1,
admitted because nothing binds (t, 2)), then `RestoreTemplateVersion(t, 3)` stored an
original v3 above a carried v2. Under the rule the new v2 (and any v′ above it) is refused
while (t, 3) bindings dangle, and so is a v4 between restores of (t, 3) and (t, 5). The
only writes that store versions of t while some binding of t dangles are therefore
restores (below), each of a version's own bytes, and no carried version is ever stored
beside a dangling binding above it. FDB tests pin three sequences: Restore(1), create v2
(refused, naming the (t, 3) schema), Restore(3), create v4 (accepted); Restore(3), create
v4 (refused, naming the (t, 5) schema), Restore(5), create v6 (accepted); and Restore(1),
save v5 (refused, naming the (t, 3) schema), Restore(3), save v5 (accepted); each
followed by opening every bound schema and reading its rows back. Concurrency, named by key: a schema can
bind (t, v) only while the template row (t, v) is stored, because CREATE SCHEMA reads
that row (`DoesSchemaTemplateExistAtVersion`, its UNKNOWN_SCHEMA_TEMPLATE check) and the
creation writes it; so a CREATE SCHEMA that ran before the new row committed read it
absent and failed on its own, and one after binds the NEW version legitimately. What
the guard's range read serializes against is a concurrent DROP SCHEMA of a bound schema
(its index entry is deleted inside the range): that pair conflicts once and converges.
The target accepts the re-issue, so this is a declared divergence (section 9): the guard
only refuses a state in which the target itself silently rebinds data to different
metadata. Java's gone-version refusal lands with it (step 1 of section 8): SaveSchema's
load of the existing row fails with Java's "SchemaTemplate=<n>, version=<v> does not
exist" when the row's template version is gone, where `validateSchemaRebind` returns nil
today (`fdb_store_catalog.go:311-313`). FDB tests: a template with a bound schema
dropped and recreated is refused, naming the schema; a latest version deleted with
`DeleteTemplateVersion` and re-saved at the same number is refused the same way; with
the schema dropped first both are accepted; a schema bound to v1 does not block a new
v2; the guard's read against a concurrent DROP SCHEMA conflicts once and converges; and
a RepairSchema of a schema whose bound version is gone fails with Java's message.
Driver-stored templates (F11) are covered because the guard runs in the Go catalog, and
the F11 migration copies MetaData bytes verbatim rather than rebuilding (TODO.md F11
block); because the guard reads the schema rows' index, that migration copies every
TEMPLATE row before any SCHEMA row that binds it (a schema row copied first would make
the template's own copy trip the guard), which the TODO F11 block now states.
A schema bound to a version that is gone has no exit that keeps its data in either
engine short of re-issuing the version: the gone-version refusal (the target's) blocks
RepairSchema, and CREATE SCHEMA over it fails. The target's re-issue is the silent
rebind this guard refuses, so Go closes the Go-only way INTO that state and gives the
one safe way out. In: `DeleteTemplateVersion(t, v)`, the Go-only API, is REFUSED while
any schema binds (t, v), with the same 42F59 message shape naming the first bound
schema and the same one-range read of `TEMPLATES_VALUE_INDEX`; DROP SCHEMA TEMPLATE
keeps the target's behaviour (it drops regardless), declared. Out: `fleet.
RestoreTemplateVersion(ks, t, v, md)` re-creates a dropped (t, v) while schemas bind it,
bypassing the guard, and is correct only when md is the dropped version's exact metadata
(the caller's backup of the stored MetaData bytes). It writes those bytes raw and runs
no build-path check (section 3.2's table of write paths). What it CAN check, it does: it
refuses when a template (t, v) is stored, so it cannot overwrite a live version; it
refuses when NO schema binds (t, v), since re-creating a bound version is its only
purpose, and a restore nothing binds would put a dropped history's bytes under a name that
may have started a new one (drop t and every schema bound to it, CREATE SCHEMA TEMPLATE t
afresh at v1, then `Restore(t, 3)` from the old history: v8 admitted it, and a
`RepairSchema` onto a latest version foreign to v1 that differs only in a predicate, with
matching index versions, would pass the validator, which compares no predicates); and it
lists every schema bound to (t, v) (the same `TEMPLATES_VALUE_INDEX` range the guard
reads), resolves each bound store's subspace through a keyspace the CALLER names,
`RestoreTemplateVersion(ks *keyspace.RelationalKeyspace, t, v, md)`, the keyspace the
deployment opens its schemas with (`ks.SchemaSubspace(databaseID, schema)`, keyspace.go:
56-64, the F11 layout over the root the caller supplies; v8 said the catalog's schema
opener supplies it, and `RecordLayerStoreCatalog` has none, opening only its own store,
fdb_store_catalog.go:166-175), reads each store's header, and refuses when any header records a metadata version
ABOVE md's, or when a bound schema's store has NO header (every CREATE SCHEMA creates its
store, so a missing header means the subspace is not the store's or the store is gone,
and a check that cannot read the header must not pass it). Declared, with (s): the Go
keyspace is F11's Go-only layout, so over a catalog whose schemas Java created (Java's
keyspace) every bound store reads "no header" through it and the restore refuses: it fails
closed, and it is unusable there until F11's keyspace question is decided (TODO.md F11). A header above md is the
impossible state: a store never records a metadata version its template lacks, so a
header above md means md is not the metadata that store was opened under. A header BELOW md is the normal state of a schema rebound to (t, v) by
`RepairSchema` and not opened since (`RepairSchema` writes only the catalog row,
fdb_store_catalog.go:482-491), and on its next open the store upgrades from its header's
version as for any rebind. So `header <= md` is the check: necessary, not sufficient, as
the documentation says (an equal-numbered foreign metadata passes it). The headers are
read in batches of 1,000 bound schemas per read-only transaction BEFORE the restore,
because the check has no limit on the number of bound stores and one transaction cannot
read them all. That is sound through the relational catalog: while (t, v) is not stored,
no store bound to it can be opened through the catalog (the gone-version refusal, in both
engines), so no bound header moves, and no schema can newly bind (t, v) (CREATE SCHEMA
reads the template row). The claim is scoped to the catalog: a store opened by another
path in the window (a record-layer program opening the subspace with its own metadata)
can move its header above md after the batch read, and that race fails CLOSED, not open:
the first open of the restored template over that store finds the header above md and
refuses with `StaleMetaDataVersionError`, as any open under metadata older than the
header does. The restoring transaction does not re-read the WHOLE bound set (v7 re-read
it, which put an O(n) read back into the one transaction the batching exists to keep
small). It makes three reads of its own: that (t, v) is still not stored; that SOME
schema still binds (t, v), a limit-1 read of the `(t, v)` range of
`TEMPLATES_VALUE_INDEX`, refused as at the listing when it is empty; and the template
rows of t, a range read over every version of t. Each read is serializable, so its
range is a read-conflict range. The limit-1 read is what makes "nothing binds" the
restoring transaction's own decision rather than the listing's: v9 decided it at the
listing and called a DROP SCHEMA in the window harmless ("only shrinks the set"), which
it is for the header check but not for this refusal, which fires exactly when the set
shrinks to EMPTY. The race it closes: the listing sees S, the only schema bound to (t,
3); DROP SCHEMA S commits; a fresh CREATE SCHEMA TEMPLATE t is admitted, since nothing
binds t any more, and stores a new v1; v9's restore then found (t, 3) absent and put the
old v3 beside the new v1, the two-history state the version guard exists to prevent.
With the limit-1 read, a DROP that committed before the restoring transaction's read
version leaves the range empty and the restore refuses; one that commits after it writes
the range and conflicts. The read is sound because bindings of a version that is not
stored can only shrink (no CREATE SCHEMA binds a version that is not stored, and a
restore adds none), so a non-empty read means no fresh t was admitted before it; the
template-row range makes the same sure of a template write of t that lands after the
read version (a fresh create, or another restore), which then conflicts. The limit-1
read's conflict range ends just past the binding it returns, the first in key order, so
what conflicts is a write that removes THAT binding or adds one before it; a DROP of a
later binding does not conflict, and needs not to, since the binding the read returned
still binds (t, v) (v10 said every write to the bound set conflicts). Every write to t's
templates after the read version conflicts, whoever writes it. A conflict makes the
restore retry from the listing; a schema row written in the window by a path that
bypasses CREATE SCHEMA's template check is outside what the restore guarantees, as a
store opened by another path is (above).
TWO HISTORIES ALREADY STORED (v10 missed it): a catalog can hold a fresh t v1 beside
schemas bound to a dropped (t, 3) before any guard ran, since pre-upgrade Go and Java
both write that state. The refusal of a stored (t, v) does not see it, (t, 3) being
absent, so v10's restore admitted it and made the two-history state itself. The
restoring transaction therefore checks md against every stored version w of t it has just
read, and admits the restore only when the two are ONE HISTORY: with L the lower of md and
w by metadata version and H the higher (the lower-numbered template version is the lower;
a template version below another with a higher metadata version is itself refused), CARRY
COMPATIBILITY, the invariant section 4's carry keeps from each version to the next, holds
from L to H. v11 compared record-type keys and union numbers only, which admitted the
predicate-only difference section 2 names as the hazard (a foreign version equal in every
key and version, its sparse index's WHERE different), and two names on one key admitted as
a rename. Carry compatibility is four checks, each refused naming the version and the
first difference: (i) the store's evolution validator from L to H, with an option set the
restore states itself, not the rebind's of the day: `SetAllowNoVersionChange(true)`,
`SetDisallowTypeRenames(true)` (v14, check (ii) below), the literal-carrier arm (section
3.2) made SYMMETRIC for the restore (v14, below) and `SetAllowIndexRebuilds(true)` (v12 cited the
rebind's options at `fdb_store_catalog.go:352-353`, which lack `allowIndexRebuilds` until
step 3, so its planned "raised and changed, admitted" test would have been refused with
"last modified version of index changed"), which checks what section 4's carry keeps for
indexes and former indexes (pairing by subspace key, added and last-modified versions, a
former index for each index H lacks, a former-index key a live index reuses, WS-C 7.7)
and each record type's fields; (ii) NO RENAME, which the validator's default admits. v13
re-implemented it as a name-to-key check beside the validator, while Go's validator
already carries Java's option: `SetDisallowTypeRenames(true)` in (i)'s option set refuses
a record type whose name changed at the same union field ("record type name changed",
`getTypeRenames`, as MetaDataEvolutionValidator.java:354-377). With the validator's own
refusals of a changed record-type key and a removed union field, that covers two names on
one key or one number and one name on two. The duplicate check is deleted (Graefe v12 F6).
(iii) EVERY
INDEX FIELD, which (i) does not compare: each index both define is EQUIVALENT or WIDENED
under section 4's comparison (the predicate included; WIDENED stores the same bytes, and
either order of L and H is compared, since a fresh v1 built at this build's width may be
the lower of the two). Either order reaches (iii) only because (i)'s literal-carrier
arm is symmetric in the restore (v14). The rebind's arm is one-way (`literalValuesEquivalent`
accepts a stored `long_value` against a rebuilt `int_value` only), so under it the design's
own case, a fresh v1 at `int_value` as L and an older `long_value` version as H at an equal
last-modified version, was refused by (i) with "index key expression changed" before (iii)
ran. Both carriers of one literal store the same tuple bytes, and the restore compares two
stored histories, neither of which is a rebuild of the other. So the restore's validator
accepts the two carriers in either direction, and the rebind's stays one-way. The
option is set by `validateSchemaRebind` (one-way) and by the restore (symmetric), and by
nothing else. (iii)'s exception: UNLESS H's last-modified version of it exceeds L's METADATA
version. A store bound to L has a header of at most L's metadata version and rebuilds on
open exactly the indexes whose last-modified version exceeds its header
(`checkRebuildIndexes`; Go `GetIndexesSince`, store_builder.go:395), so an index H raised
above L's metadata version is rebuilt and its definition may differ, and one H raised to
at most L's metadata version is NOT rebuilt, and a difference there would read L's
entries under H's definition; that index is refused, naming it. (v12 exempted any index H
raised, which holds within one history, where the carry puts a raised index above the
stored metadata version, and not across two: a fresh v1 at metadata version 3 whose index
has last-modified 2, against an old (t, 3) at metadata version 4 whose index has
last-modified 3 and another root, passed every check and left v1's entries read under
(t, 3)'s root.) (iv) THE RELATIONAL VALIDATOR from L to H (v14): the check
`CreateTemplate` runs between consecutive versions (`RelationalSchemaEvolutionValidator`,
moved into `CreateTemplate` above). The store's validator compares an enum only by
checking that the old numbers still exist (`validateEnum`, as MetaDataEvolutionValidator
.java:340-349) and never compares field options. So two histories whose column is
`ENUM('A','B')` in one and `ENUM('B','A')` in the other, or a VECTOR column whose
`vectorOptions` differ, passed (i) to (iii). Stores bound to the lower version would
then read A as B. The relational validator compares column types whole (`DataType.Equal`,
an enum's whole value list, datatype_composite.go:189-203), so it refuses both. Enum DDL
(step 5) lets Go itself build such templates. When every stored version passes, the restore is admitted. A unit test
drives the batching with the batch size lowered to 1 over three bound stores, one of
them above md, and one whose subspace holds no header; FDB tests commit, between the
restore's listing and its restoring transaction, (i) a DROP SCHEMA of the only bound
schema and then a fresh CREATE SCHEMA TEMPLATE t, and assert the restore is refused
("no schema binds") and the fresh v1 stands alone; (ii) the same DROP alone, refused;
and, between the restoring transaction's reads and its commit, (iii) a DROP SCHEMA of the
binding the limit-1 read returned (the first in key order; a test drops a later one and
asserts the commit does NOT conflict) and (iv) a template write of t (a concurrent restore
of another dropped version of t, and a CREATE SCHEMA TEMPLATE t written as an engine
without the guard writes it, since Java's can land at the shared subspace; v10 called the
restore the only template write that can land while a binding of t dangles), each
asserting the restore's first commit conflicts (1020) and its retry decides again from
the listing; and the two-history cases: a raw fresh t v1 whose record-type keys differ
from (t, 3)'s, refused naming v1; one equal to (t, 3)'s history in every key and version
whose sparse index's predicate differs, refused naming the index (v11 admitted it); one
whose two record types swap their keys (a rename both ways), and one where a second name
takes a key the other version gives another, each refused by (ii); a v1 whose index (t, 3)
raised ABOVE v1's metadata version and changed, admitted; one whose index (t, 3) raised to
at most v1's metadata version and changed (the two-history example above), refused naming
the index; two that differ only by a literal carrier (WIDENED), admitted in BOTH
directions (L at `int_value` with H at `long_value`, and the reverse); two that differ only
in an enum's value order, and two that differ only in a VECTOR column's options, each
refused by (iv); and one that agrees in every check, admitted. FDB tests: the refused
`DeleteTemplateVersion`; the restore of a dropped bound version from its saved bytes,
after which the bound schema opens and its rows read back; the rebind → drop → restore
sequence with NO open in between (the header below md), accepted, the store opening
afterwards under the upgrade; the restore refused over a stored version; the restore
refused for a metadata whose version is below a bound store's header; the restore
refused when nothing binds (t, v), after the drop / fresh v1 / Restore(t, 3) sequence
above; and a fresh CREATE SCHEMA TEMPLATE t refused while a schema binds a dropped
version of t other than 1.

Tests (FDB): each behaviour × {absent, identical, different name, lower, equal,
higher version, existing row whose template version is gone}; a no-op save asserted
to buffer no mutation directly (the transaction's write set is empty after the call)
AND observably (the no-op saver's transaction reads the row, a second transaction
writes the row and commits FIRST, and the no-op saver still commits); two concurrent
`Initialize` transactions from an ALREADY-INITIALIZED catalog both commit (a
first-time initialization writes the database and template rows in Java too,
:180-189, 239-242, so two fresh ones legitimately conflict: a second test shows the
first-time pair conflicting once with 1020 and converging on retry); CREATE SCHEMA
precedence pinned arm by arm against the target (a conformance spec runs each through
both engines and compares SQLSTATE and message): missing database + missing template →
UNDEFINED_DATABASE with Java's message; missing template + existing schema →
UNKNOWN_SCHEMA_TEMPLATE; a duplicate CREATE SCHEMA → 42F06 from saveSchema(ERROR), and
the store-exists mapping driven by a store created under an unsaved schema row;
repair to a lower-version template refusing with Java's message. Whether today's code fails the
concurrent-initialize test with 1020 is measured when the test is written, not
asserted here.

## 3. Index generator fidelity

### 3.1 Field-path trie (F1) — landed locally

`computeTrieAtDepth` (`pkg/relational/core/query/ddl/generator.go`) tested the path
LENGTH before the prefix. Java tests `prefix.equals(fieldPath)` then
`!prefix.isPrefixOf(fieldPath)` (`FieldValueTrieNode.java:212-218`). The fix moves the
prefix check first. Pinned by `TestIndexDDLFieldTrieMatchesJavaStoredRoots` (11
shapes; roots are the oracle's WSJ-JAVA-ROOT lines verbatim) and
`TestIndexDDLFieldTrieRejectsDisconnectedReferences`; reverting the fix reddens 4 of
the 11 (`evidence-mutations.txt`); 13 PASS under Bazel. Through the production path,
`TestFDB_IndexDefinitionProductionPathStoresTargetShapes` creates `nested_then_top`
with SQL and asserts the stored root is the target's. That the planner USES the index
is F14, section 3.5 (measured today: the target covers `ORDER BY s.x, ts` from it, Go
scans and sorts).

A template Go stored BEFORE the fix holds the truncated root, so its index entries lack
the nested key column. A rebind to a template rebuilt from the same DDL changes the
index's key expression; the carry rule (section 4) gives it a new last-modified
version and the store rebuilds it (section 4, "changed index"), and the rebind
validator's key-expression check is what refuses the change if it arrives any other
way. Without the carry rule and `allowIndexRebuilds` (section 4), a RepairSchema or
`fleet.Migrate` onto a DDL rebuild REFUSES every tenant with an F1-affected root ("last
modified version of index changed", or earlier "index key expression changed"), and
every tenant with a FLOAT literal in a key column (whose 0x20/0x21 type code the
widening arm rightly treats as an index change). That fails closed, and it exists only
on this unmerged branch: 3.1 reaches master in the same merge as section 4 (section 8),
so no released build has the refusal without the path around it.

### 3.2 Literal carriers and operand widening (F2, F2b) — landed locally

SOURCE: `ValueToKeyExpressionVisitor.visitLiteralValue` (:196-197) stores
`Key.Expressions.value(element.getLiteralValue())`, the literal's own Java object;
integer literals are `Integer` when they fit (`ParseHelpers.parseDecimal`, :96-98,
with `l`/`i` suffixes forcing LONG/INT and `f`/`d` FLOAT/DOUBLE, :68-104); the bitmap
entry size is an `int` (`SemanticAnalyzer.java:106`, appended at :1115).

The break, MEASURED before the fix and MEASURED again on the `long_value` variant (the
same metadata with every INT literal rewritten to the pre-fix width): the target plans
over the STORED key expression (KeyExpressionExpansionVisitor turns a stored
`long_value` into a LONG literal Value), the bitmap functions have only `_LI` and `_II`
lanes (ArithmeticValue.java:515-522), so every bitmap read fails with XX000
VerifyException "unable to encapsulate arithmetic operation due to type
mismatch(es)"; and an equality over `d & 1` or `d + 1` is no longer sargable on its
index (COVERING with an EQUALS range over `int_value`; `ISCAN(... <,>) | FILTER` over
`long_value`): the same rows at full-index cost.

The fix: the generator's ConstantValue arm (`literalKeyCarrier`) sets the carrier from
the constant's STATIC type, whatever Go kind holds the value (INT → int32, LONG → int64
from any Go integer kind, a platform `int` and `uint64` included; FLOAT → float32;
DOUBLE → float64, a float32 widened), refuses an INT constant outside 32 bits, and an
unsigned value beyond int64, with XX000 instead of wrapping it
(`TestLiteralKeyCarrierRefusesOutOfRangeInt`, every Go kind), and the walker's injected
entry size is INT-typed. v3 narrowed only an int64 and a float64, so a Go `int` typed
INT still reached the wire as `long_value`; `TestLiteralKeyCarrier` now drives every
kind and its wire variant. MEASURED after: the target's 15 reads over the library-stored
template equal its reads over its own (pinned); all ten literal spellings of the shapes
(`1`, `3000000000`, `1.5`, `1.5f`, `1.5d`, `1l`, `1i`, `-1`, `-1.5`, `(1 + 2)`), the INT
literal's edges and the first LONG (`2147483647` and `-2147483648` stored as `int_value`,
`2147483648` as `long_value`, measured) and the five cross-type shapes (INT literal over
FLOAT and DOUBLE columns, DOUBLE literal over an INTEGER column, FLOAT literal over a
DOUBLE column stored as `float_value`, LONG literal over an INTEGER column stored as
`long_value`) are byte-equal (the `1.5h` HALF literal is a syntax error in both
engines' index DDL). Unit pins: `TestLiteralKeyCarrier` (every width and its wire
variant) and the AsSelect goldens; four mutations redden (`evidence-f2.txt`); the
production-path pin reddens when the INT arm keeps int64 (`evidence-mutations.txt`,
v3).

Operand widening (F2b). With INT literals stored as int32, Go's key evaluator must read
them: it required int64 and errored on everything else. Java's
LongArithmethicFunctionKeyExpression reads every operand with
`Key.Evaluated.getNullableLong` (Key.java:579-582): any `java.lang.Number` through
`Number.longValue()`, anything else InvalidResultException. Go's `nullableLong` is that
function (a DOUBLE or FLOAT truncates toward zero with Java's narrowing, NaN → 0,
saturating), and a non-Number is `KeyExpressionInvalidResultError` carrying Java's log
keys, both types named by their Java class (`TestArithmeticWrongType`;
`TestJavaTupleValueClassName` drives all nine arms: a message read through a run-time
descriptor is `com.google.protobuf.DynamicMessage`, as the relational layer's is, and a
Go-GENERATED message keeps its proto full name, because its Java class depends on the
Java build's protoc options, which the Go copy of the descriptor does not carry
faithfully). MEASURED, both engines writing ONE index (spec "WS-J F2b index entries
written by both engines"): the target inserts rows through SQL, Go's record layer saves
the same rows into the same store, and the entries' RAW key bytes before the primary key
are equal row for row (`13fd` for -2, `1504` for 4, `14` for 0, pinned; the decoded
values -1.5 → -2, 2.9999 → 4, NaN → 0 printed beside them); ±∞ and
2^63 saturate, `addExact` overflows, and both engines refuse those rows (the target
with XXXXX ArithmeticException "long overflow"). Those rows measure the OVERFLOW, not the
saturated value, which never reaches an entry there (and a saturation to the wrong sign
would overflow the same way), so a second spec (v7, landed) indexes `d + 0` and `f + 0`,
which cannot overflow: their entries ARE the saturated operand, and ±∞, ±9.3e18, ±1e300
and ±3e38f store Long.MAX_VALUE or Long.MIN_VALUE by sign, byte-equal between the two
engines (`1c7fffffffffffffff`, `0c7fffffffffffffff`); flipping the sign of Go's negative
saturation (a mutation, `wsj7/f2b-mut-signflip.log`) reddens that spec and not the first; `n + 1` over the `int_value` literal is
equal too; and the target serves `WHERE n + 1 = 1011` from the index
(`COVERING(NPLUS ...)`) over the Go-written row. Go-side unit evidence:
`TestFDB_ArithmeticIndex_NonIntegerOperandTruncatesLikeJava` (now in its own file)
pins the entries through Go's writer, and removing `nullableLong`'s float64 arm reddens
it (`evidence-mutations.txt`, v3).

The READ side (decided). The target serves `WHERE d + d = 3` from the index, comparing
a DOUBLE comparand with the truncated LONG entries, and returns no row where the
record's own `d + d` is 3.0 (likewise `c * 1.5 = 3`; `f + 1 = 2.5` it answers by index
scan plus filter, correctly). Go today declines every arithmetic function key and
answers each of those reads from the record (row 1). The decision: Go keeps answering
from the record. When section 3.5 ports function-key matching, a long-arithmetic index
matches only where every operand Value is integral (INT or LONG); over a DOUBLE or
FLOAT operand the entries do not hold the value of the query expression, so no match
is valid. The rule is by TYPING only, and it is not the whole story: an INT operand at
the edge of its lane also yields entries the query expression cannot produce, because
the key computes in long arithmetic while the query's `i + 1` is an INT-lane
ArithmeticValue that overflows. MEASURED ("int_max_plus_one", i = 2147483647 in row 1):
the target serves `WHERE i + 1 = 6` from the index (`COVERING(IX [EQUALS ...])`,
answer [[2]]) and never evaluates row 1's expression, while `WHERE i + 1 =
2147483648` plans an index scan with a residual FILTER and fails on row 1 with the
overflow, as does any read that evaluates the expression per row; Go today answers
every one of them from the record and fails with 22003 on row 1, `= 6` included (its
pins). The index is a faithful match for INT and LONG operands in the target's sense
(it serves exactly the rows whose entry equals the comparand), so section 3.5 keeps it:
after 3.5, Go serves `= 6` from the index and answers [[2]] as the target does, and the
residual reads keep failing as they do in both engines. The function VALUE is never read
from the entry: MEASURED, the target cannot plan `SELECT i + 1 FROM T ORDER BY i + 1` at
all (0AF00, and its EXPLAIN the same), because no access path provides that ordering for
the INT-lane expression, and where it does serve a function key's order (`ORDER BY d + d`
over a DOUBLE operand, pinned above) its plan recomputes the value from the record,
`ISCAN(ARITH_D <,>) | MAP (_.D + _.D AS _0)`, never `COVERING`; Go answers the ordered
read through its sanctioned in-memory sort and fails on row 1 with 22003 (its pins).
MEASURED directly (v6, four reads added to that probe, pinned for both engines): an
index-served equality over a function key FETCHES the record and recomputes whatever it
projects, `SELECT d + d FROM T WHERE d + d = 2` as `ISCAN(ARITH_D [EQUALS ...]) | MAP
(_.D + _.D AS _0)` and `SELECT d FROM T WHERE d + d = 2` as `ISCAN(ARITH_D [EQUALS ...])
| MAP (_.D AS D)`, and at the INT edge `SELECT i + 1 FROM T WHERE i + 1 = 6` as
`ISCAN(IX [EQUALS ...]) | MAP (_.I + @c9 AS _0)`, answering [[6]], and `SELECT i ...`
as `ISCAN(IX ...) | MAP (_.I AS I)`, answering [[5]]; `COVERING` appears only where the
query reads nothing but the primary key (`SELECT id ...`, pinned above). Go today serves
none of these from the index (a residual filter over the record scan, its pins).
Decided: section 3.5 never COVERS a function-key column, and a function-key column
exposes NOTHING through the coverage surface (`metadataSufficientForPlanning` and its
callers): the entry holds the function's value and the primary key, not its operands,
so a read of either the value or an operand fetches the record and evaluates there,
and an entry the INT-lane expression cannot produce (2147483648) is never returned as
an INT. No function-key column of such an index is covered; its primary-key columns are
(the `SELECT id` pin). An index mixing a function-key column with a plain column is
UNMEASURED: 3.5 treats the plain column as any value index's, and a mixed-key read is
added to the oracle, both engines pinned, before 3.5 lands. A plan-harness test
pins that `SELECT i FROM T WHERE i + 1 = 6` and `SELECT i + 1 ...` over the `i + 1`
index are not covering plans once 3.5 serves them from the index.
A plan-harness test over this template asserts that no plan projecting `i + 1` is
covering, and the two ordered-projection rows join the in-memory-sort class of declared
differences (the target 0AF00, Go its sort's answer). Declared in DIVERGENCES.md
beside the non-integer rule: an index-served read can answer where the same read over
the record fails with an overflow, in both engines. Go's 27 answers and plans are pinned beside the target's (`wsjNonIntGoPins`, 27 entries);
the divergence is recorded in DIVERGENCES.md ("A long-arithmetic index over a
non-integer operand is not a valid match"); the upstream report is written there and
unpublished (publication is not authorized).

Templates Go ALREADY stored with `long_value` literals. Only templates stored through
the catalog LIBRARY at the Java subspace are visible to the target (F11: driver-stored
ones are invisible until F11 is resolved, and the F11 migration must copy MetaData
bytes verbatim and rebind rather than rebuild, TODO.md F11 block). Those tenants are
rebound to a template rebuilt on this version, and the rebind validator admits the
carrier difference without an index change through an explicit, relational-rebind-only
option (landed). THAT PATH IS REACHABLE ONLY THROUGH SECTION 4, and nothing claims
otherwise: a relational template seeds its metadata version from its template version
(`metadata/builder.go:487`) and `addIndexCommon` bumps it once per index (`metadata.go:601-606`),
so a v2 rebuilt from the same DDL moves every index's added version up by one, and the
validator refuses in `validateIndex`'s "added version does not match" arm before it reaches the root
comparison where the widening arm lives (`validateIndex`'s root check, `literalCarriersEquivalent`;
from v13 citations to the validator name the function and the arm's message, not a line,
since WS-C's revisions keep moving the file: v11's and v12's re-taken line numbers were both
stale by the time they were reviewed). Section 4's carry rule keeps the
stored versions, which is what makes the arm reachable; its test 5 is the end-to-end
pin (v1 built by the pre-port builder at `e48f5b496`, committed as testdata and stored by
a raw write of its catalog rows, its literals asserted `long_value` as that builder wrote
them, not rewritten; v2
from the same DDL through `fleet.SaveTemplate`, RepairSchema, rows written before and
read after; the validator runs up to its one-way carrier arm and admits it there),
and it lands with section 4. The option:
`MetaDataEvolutionValidatorBuilder.SetAllowLiteralCarrierWidening`, set by
`validateSchemaRebind` (one-way, below) and by the restore of section 2 (in its symmetric
form, `SetAllowLiteralCarrierEitherWay`, v14), and by nothing else, so the ported core
validator keeps Java's
proto-equality refusal ("index key expression changed", MetaDataEvolutionValidator.
java:717-722; Java's `LiteralKeyExpression` compares by proto equality, :213-214). The
criterion is value-preserving, never "width-only", and it runs ONE way, from the stored
pre-F2 width to the rebuilt target width: a stored `long_value` against a rebuilt
`int_value` with the same number is equivalent at any position (the tuple encoding of
an integer does not depend on its width); a stored `double_value` against a rebuilt
`float_value` is equivalent only inside the arguments of a long-arithmetic function and
only when the values are equal
(`float64(float32(v)) == v`, since those operands are read through `longValue()`); a
float/double pair that is itself a key column changes the tuple type code (0x20 versus
0x21) and stays an index change. Pinned by the Ginkgo "literal carrier widening" specs
and `TestFDB_SchemaRebindAcceptsLiteralCarrierWidening`, a VALIDATOR-level pin of the
catalog wiring (hand-built index protos with versions 1/1 and one metadata version in
v1, v2 and v3, no store opened: a v2 differing only in the carrier is rebound by
`RepairSchema` and the binding moves; a v3 changing the literal's VALUE is refused with
"key expression changed" and the binding stays at v2). The arm runs ONE WAY, from the
width a pre-fix Go build stored to the width the target stores (`long_value` →
`int_value`; `double_value` → `float_value` inside long arithmetic only). The one-way
rule decides only whether a stored index's ENTRIES can be kept; it does not decide
whether a key is acceptable, because the rebind also sets `allowIndexRebuilds` (section
4), under which an index whose last-modified version moved is admitted without the root
check (Java :657-662, Go `validateIndex`'s early return when the old last-modified version
is lower). So a reverse move is
not refused by the rebind: it is a CHANGED index, rebuilt (a Ginkgo spec per direction
pins that the arm does not admit it as unchanged). What keeps a key the target cannot
plan (a `long_value` bitmap entry size fails every query of the table, 3.5) out of
stored templates is a separate check, decided here, and it sits on the BUILD path: the
point where Go turns an in-memory template (from DDL, from `fleet.SaveTemplate`, or from
a library caller) into stored bytes. That is `CreateTemplate` of `api.SchemaTemplateCatalog`
(api/catalog.go:36), which has two implementations, the FDB catalog
(fdb_template_catalog.go:127) and the in-memory one (template_catalog.go:87); both call one
shared check before they serialize. Its SCOPE is the ArithmeticValue family, the function
keys Java builds through `ArithmeticValue.encapsulate`: add, sub, mul, div and mod, bitand,
bitor and bitxor, and bitmap_bucket_offset, bitmap_bucket_number and bitmap_bit_position.
Order function keys (the ASC/DESC/NULLS wrappers), CARDINALITY and the collation keys are
not in scope as FUNCTIONS: they are not arithmetic, Java's `encapsulate` never sees them,
and they have no lane to lack. Each in-scope key is expanded through the lane table with
its operands' STATIC types, and every operand, in scope or not, is typed by the RESULT
type of the Value Java builds for it: a nested arithmetic key by its lane's result (in
`(a + b) & 1` over BIGINT columns the inner `ADD_LL` makes the outer operand LONG, and the
outer row is `BITAND_LI`), an order-wrapped operand by `ToOrderedBytesValue`'s BYTES
(ToOrderedBytesValue.java:100-101), a CARDINALITY by INT (CardinalityValue.java:84-86), a
collation key by `CollateValue`'s BYTES (CollateValue.java:124-125). v8 typed an
order-wrapped operand by its column and planned a test that ACCEPTED one; in the target
`bitand(order_desc(a), 1)` is (BYTES, INT), which has no row, so the target fails every
query of the table and Go's own `nullableLong` refuses every write. So it is refused, and
`x & cardinality(a)` (LONG or INT, INT) has a lane and is accepted. One with no row is
refused, 42F59 INVALID_SCHEMA_TEMPLATE naming the index, the function and the operand
types. The lane table is ArithmeticValue's whole `PhysicalOperator` table
(ArithmeticValue.java:406-522), ported once: ADD over every pair of INT, LONG, FLOAT,
DOUBLE and STRING (25 rows, `ADD_II` to `ADD_SS`); SUB, MUL, DIV and MOD over every pair
of INT, LONG, FLOAT and DOUBLE (16 rows each); BITOR, BITAND and BITXOR over the four
integer pairs (4 rows each); and bitmap_bucket_offset, bitmap_bucket_number and
bitmap_bit_position over `_LI` and `_II` (2 rows each): 107 rows. A key has a lane when its
function and its operands' static types name a row; so `d + d` (`ADD_DD`), `f + 1.5f`
(`ADD_FF`) and `f + 1` (`ADD_FI`), the F2b templates the target creates, are cases the
check must ACCEPT (a unit test over each F2b template and each oracle template the target
creates), and a `long_value` bitmap entry size (`BITMAP_BUCKET_OFFSET_LL`, no row) is the
case it refuses. Section 6, which ports the query engine's bit and bitmap lanes, EXTENDS
this table rather than adding a second one.
Go's DDL CAN produce a no-lane key today, and v7's claim that it cannot after F2 was
false: F2 fixed only the literal's carrier, and the key generator builds a bit or bitmap
key over any operand (`ddl/generator.go:851-880`, the ArithmeticValue arm and the bit
operators' ScalarFunctionValue arm, neither of which consults a lane). MEASURED (the spec
"WS-J bit and bitmap index keys over an operand with no lane", ten DDL shapes: `v & 1` and
`bitmap_bucket_offset(v)` over DOUBLE, FLOAT, STRING, BOOLEAN and a STRUCT): the target
refuses each at CREATE SCHEMA TEMPLATE with the query path's outcome, XX000 with
VerifyException "unable to encapsulate arithmetic operation due to type mismatch(es)" for
a primitive operand and SemanticException "The argument to an arithmetic operator
expecting an argument of a primitive type, is invoked with an argument of a complex type,
e.g. an array or a record." for the STRUCT (ArithmeticValue.java:213-231), because it
builds the index's Values through `encapsulate` at the clause. Go's driver stores the
eight primitive-operand shapes, and refuses the two STRUCT shapes only by accident of its
metadata build (XX000 "build RecordMetaData": the key's operand is not a scalar field),
the target's code with another message (the WSJLANE lines, `wsj8-lane.log`, "Ran 10 of
51", all 10 target outcomes as asserted). For a DDL-origin key the outcome must be the TARGET's, at the
clause, not the build-path check's 42F59, and it is from step 3 of section 8, the step
that lands the build-path check, so no commit of the tree gives a DDL shape the Go-only
42F59: the key generator's ArithmeticValue and bit-operator arms (ddl/generator.go:851-881)
consult the lane table with their operands' result types and raise the target's XX000,
class and message before any key is built; the spec's `goNow` (Go's outcome on this tree,
pinned) is then replaced by a comparison with the target's outcome for all ten, on
SQLSTATE and message: Go renders an error as `ERROR <code> "<message>"` with no exception
class, so the target's `VerifyException` or `SemanticException` is not part of what is
compared. PRECEDENCE: the target registers every table before it generates any index
(section 4), so a table fault anywhere in the statement beats a lane-less index key
before it; the generator arm runs where Go generates the key, after the tables, and
WSJLANE gains a shape with both faults, a lane-less index before a later table whose
fault the target reports, pinned on the target and, from step 3, asserted equal in Go. Section 6 later moves the lane
resolution into the Value construction Go's index SELECT already goes through
(`parseAsSelectIndexDefinition`, `NewPlanVisitor(md).VisitQueryTerm`, embedded/ddl.go:565),
and the generator then reads the lane from the Value, one table throughout. The build-path
check keeps its 42F59 for what no DDL clause saw: metadata built by hand, which a test
builds through `fleet.SaveTemplate`, through the FDB catalog directly and through the
in-memory catalog, pinning all three, plus a nested key and `x & cardinality(a)` that
it accepts and `bitand(order_desc(a), 1)` that it refuses (BYTES, INT).
Every template write path, and the checks it runs (the guard is section 2's; the header
check is the restore's; v7 to v15's (d) refusal column is withdrawn in v16, 4d: no path refuses
shape (d)):

| write path | builds? | version guard | lane check | header check |
|---|---|---|---|---|
| any caller (`execCreateSchemaTemplate`, `fleet.SaveTemplate`, a library caller) → `CreateTemplate`, a fresh name | yes | yes | yes | no |
| any caller → `CreateTemplate`, a new version of a stored name: `CreateTemplate` itself refuses an exact duplicate (DUPLICATE_SCHEMA_TEMPLATE, first, as Java), then `v′ <= latest`, then runs the relational validator (both moved from the save action), then runs `carryNumbering` over `LoadTemplateProto` of the stored latest, then writes the carried bytes through the catalog's unexported writer | yes, then carried: the stored Index messages of EQUIVALENT indexes spliced into its proto | yes | yes, over NEW and CHANGED indexes, not over EQUIVALENT or WIDENED indexes (no widening removes a lane, pinned over the whole table) | no |
| catalog `Initialize` → `CreateTemplate` (fdb_store_catalog.go:117, the CATALOG template built in code) | yes | yes (nothing binds a CATALOG version that is not stored, so it passes) | yes | no |
| `fleet.RestoreTemplateVersion` (section 2) | no: the backup's exact bytes | bypassed by design; refuses a stored (t, v) | no: it restores what was stored, a pre-F2 bitmap key included | yes |
| the F11 migration's copy | no: MetaData bytes verbatim | ordering only: every TEMPLATE row before a SCHEMA row that binds it | no | no |
| test fixtures (the oracle's no-lane variant, test 5's v1) | no: raw writes of the template's catalog rows | no | no | no |

`CreateTemplate` is the ONE BUILD-PATH write entry of both catalogs (v11): nothing exported
that builds takes a template's bytes; the one exported writer of bytes is the raw-write
restore, `fleet.RestoreTemplateVersion(ks, t, v, md)`, the table's fourth row, which exists
to write a backup's exact bytes and runs the restore's own checks (section 2) instead of the
build path's (v11 said "nothing exported takes a template's bytes", which that function
contradicts). v10 added `CreateTemplateFromProto(txn, name, version, md)` to
`api.SchemaTemplateCatalog` as the writer of a carried version, with the classification,
the lane check and the validation in the save action that called it, so a library caller
reaching it directly stored bytes that none of the three had seen (v10's gate). The
carried route now runs inside `CreateTemplate`, over the stored latest it reads itself,
and the carried bytes go through each catalog's unexported writer; the save action and
`fleet.SaveTemplate` call `CreateTemplate` and nothing else.
The version guard applies to BOTH catalogs. The FDB catalog reads its
`TEMPLATES_VALUE_INDEX`; the in-memory catalog applies the same rule over its schema rows
(`InMemoryStoreCatalog.schemas`, store_catalog.go:23, :75-115), each of which holds a
schema object whose template names (t, v): a save of t is refused while a held schema
names t at a version above the latest stored, a fresh t included. The WIRING (v10 named
the data but not the reference or the lock order; `InMemorySchemaTemplateCatalog` has its
own mutex and no reference to the schemas): `NewInMemoryStoreCatalog` gives its template
catalog a reference to itself, and the template catalog's guarded writes
(`CreateTemplate`, `DeleteTemplateVersion`) take the store catalog's mutex first and
their own second. The store catalog's binding writes take the SAME order and hold the
store mutex across their template reads, so a bind and a guarded template write are
atomic against each other: `SaveSchema` takes `c.mu` BEFORE its template check
(`DoesSchemaTemplateExistAtVersion`), where today it checks first and locks after
(store_catalog.go:85-101), and `RepairSchema` holds `c.mu` from its read of the schema
through `LoadSchemaTemplate` to its write, where today it releases it around the load
(:142-151; the TOCTOU its comment accepts goes with it). v11 said both already held the
mutex while loading, which neither did, so a bind could land after a guarded
`DeleteTemplateVersion` had seen none. Go's mutexes are not reentrant, so each locked
section calls UNLOCKED internal helpers and never an exported method of either catalog.
The one template read under `c.mu` alone is `SaveSchema`'s template check, which today
calls the template catalog's exported `DoesSchemaTemplateExistAtVersion` (v13 said "never
an exported method" and kept that call). It becomes the helper
`templateExistsAtVersionTakingTC`, which takes `tc.mu` itself and is safe under `c.mu`
because the lock order is `c.mu` then `tc.mu`. The helpers are named by what they do with
the locks, because Go's `…Locked` convention reads as "the caller holds the lock" (v13's
`loadSchemaTemplateLocked` took `tc.mu` itself, which invites a re-lock):
- `…Held`: the caller holds every lock the helper's data needs;
- `…TakingTC`: the caller holds `c.mu`, and the helper takes `tc.mu`.
`SaveSchema` takes `c.mu` and runs `saveSchemaHeld`; `RepairSchema` takes `c.mu`, reads
through `loadSchemaTemplateTakingTC` of the template catalog (which takes `tc.mu` alone,
the second lock, and not `c.mu`) and binds through `saveSchemaHeld`, not `SaveSchema`;
`CreateTemplate` takes `c.mu` then `tc.mu`, reads the stored latest through
`loadSchemaTemplateHeld` (the template read with both locks already held), and hands the
relational validator the templates it loaded, so the validator never calls
`LoadSchemaTemplate` (v12 said `RepairSchema` "goes through
`SaveSchema`" and had `CreateTemplate`'s validator load through the catalog, both of which
self-deadlock on a `sync.Mutex`). The `-race` tests below run each path to completion, so
a re-lock is a hang the test's timeout reports. A `-race` unit test interleaves each pair (a
`SaveSchema` binding (t, v) against `DeleteTemplateVersion(t, v)`, and against a
`CreateTemplate` of a fresh t after DROP SCHEMA TEMPLATE; a `RepairSchema` against the
same) through a hook between the check and the bind, and asserts that exactly one of each
pair wins and that no schema is left bound to a version that is not stored; a template catalog made by
`NewInMemorySchemaTemplateCatalog` alone has no schemas, and its guard reads an empty
set, which is stated at the constructor. `DeleteTemplateVersion` is refused there while a
held schema names (t, v), as the FDB catalog refuses it (v10 refused it in the FDB
catalog only, so the in-memory catalog could strand a schema the guard then froze, with
no in-memory restore to undo it), and `CreateTemplate` of a fresh template runs the (d)
refusal the FDB catalog's `serializeTemplate` runs (v10's in-memory `CreateTemplate`
stored a (d) template and then refused every new version of it). v9 exempted the
in-memory catalog on the ground that its rows hold template OBJECTS, so re-issuing (t, v)
cannot rebind them; but `RepairSchema` rebinds by NAME onto the latest version
(store_catalog.go:136-150), so after DROP SCHEMA TEMPLATE t and a fresh t saved above the
version a schema holds, a `RepairSchema` of that schema UPGRADEs it across histories, and
the rebind validator passes a predicate-only difference: the two-history rebind, reached
without any re-issued number. An in-memory unit test drives that sequence and asserts the
fresh save is refused, naming the schema. `RepairSchema` there also binds through
`saveSchemaHeld` and its rebind validator in step 4 (section 2's table), as the FDB
catalog's does. A RAW WRITE, in the table and below, is a
catalog-store `SaveRecord` of a `Templates` row whose `META_DATA` is the given bytes
verbatim: no build, no `CreateTemplate`, and so none of the build path's checks.
The last three are RAW-WRITE routes: they store bytes Go did not build, so they must not
run a check whose purpose is to keep Go from BUILDING such bytes, and a check there would
refuse exactly the tenants that need them (a pre-F2 bitmap template restored or copied).
The oracle's no-lane fixture (the `long_value` variant of the Go-stored template,
conformance "WS-J Go-stored template planned by the target") is stored through
`CreateTemplate` today, so it moves to a raw write of the template's catalog rows, which is
exactly what a pre-upgrade tenant holds; so does test 5's v1. Declared (section 9): Java's
programmatic API stores such a key without complaint and fails the queries later; Go
refuses it at build. A stored template that already holds a lane-less key is not
refused on its next version for it: the check runs over the indexes a save DEFINES, the
NEW and CHANGED ones (v11 checked WIDENED like NEW and v12 checked it for a lane the
widening would remove, which no move does, below), never over an EQUIVALENT or WIDENED
index, since those bytes are
already the tenant's and refusing them would refuse every new version of the template
(v9's table ran it over the whole load of the carried bytes, which would have). Two
kinds exist. The pre-F2 `long_value` bitmap entry size is section 4's WIDENED class and
3.5's decline. The eight DDL shapes Go stores today (WSJLANE's `goNow`: bit and bitmap
keys over a BOOLEAN, DOUBLE, FLOAT or STRING operand) are neither: after step 3 of
section 8 a new version made by DDL re-states the index and is refused at its clause
(XX000, the target's), and a new version built by hand carries it and is admitted; so
such a template keeps the index until a version omits it (the index then becomes a
FormerIndex and its data is cleared). A tenant holding one was stored by a pre-F2 build,
so its stored root carries `long_value` literals, and a version built by hand at this
build's width carries it as WIDENED, not EQUIVALENT (v11 said EQUIVALENT, which holds only
for a builder that repeats `long_value`, and its lane check over WIDENED would then have
refused it). A WIDENED index is therefore NOT lane-checked (v13; v12 checked it in both
forms and refused one whose stored form had a lane its widened form lacked): widening
moves only a literal's carrier from LONG to INT (or DOUBLE to FLOAT) and stores the same
bytes (section 4), and in the target's lane table no such move removes a lane: every
(X, LONG) lane has an (X, INT) twin, every (LONG, X) an (INT, X), every DOUBLE lane a
FLOAT one, and the bitmap functions have both `_LI` and `_II` (`ArithmeticValue.java:
406-522`, 107 rows). The refusing arm v12 planned could never fire, so it is not built;
what it guarded is PINNED instead, landing with the Go lane table in step 3 (section 8):
a unit test walks the whole ported table and fails if any row with a LONG or DOUBLE
operand lacks the row with that operand INT or FLOAT, so a
later table that adds a LONG-only lane reddens here, which is where the WIDENED check
would then have to come back. A key that had no lane before keeps what it had, as an
EQUIVALENT one does. (For a pre-F2 bitmap entry size the widened form is what HAS the
lane.)
Declared in (m), in DIVERGENCES.md's PENDING (t) entry now, and in the CHANGELOG with
step 3 (the CHANGELOG records what a build does), and pinned: a template stored with `v &
1` over a DOUBLE column at `long_value` (a raw write, as a pre-upgrade tenant holds it),
then a new DDL version that re-states it (refused, XX000 at the clause), a hand-built
version at `long_value` (EQUIVALENT, admitted, the Index message proto-equal) and one at
`int_value` (WIDENED, admitted, the rebuilt proto carried), the lane-table property test
above (no LONG-to-INT or DOUBLE-to-FLOAT move removes a row), and a DDL version that omits
the index (admitted, a FormerIndex).
The same option is on `frl meta evolve-check` as `--allow-literal-carrier-widening`
(`TestMetaEvolveCheck_LiteralCarrierWidening`), so the CLI can reproduce the rebind's
options exactly (`--allow-no-version-change --allow-literal-carrier-widening`).
Mutations, each with its text and presence count in `evidence-mutations.txt` (v4):
the arm refusing the long→int move, the inLongArith gate removed, the arm accepting the
reverse move, the rebind without the option, and the CLI flag unwired. The same change made the validator refuse a lower metadata version
even when an unchanged version is allowed (Java :154; Go accepted the downgrade, after
which every store open failed on stale metadata).

Rolling upgrade (CHANGELOG): a Go node from before F2b refuses to maintain an index
over an `int_value` literal ("must be int64"), so every writer is upgraded before a
template with a literal-bearing index is created or rebound.

### 3.3 Unnest-sourced and derived-table indexes (F5)

SOURCE: Java 4.14's `IndexSpec` is a generic plan visitor, not a shape list:
- specs MERGE bottom-up with a scan count, and a merged spec with other than exactly
  one scan is rejected, "Unsupported index definition, no iteration generator found"
  (IndexSpec.java:255-270, 166);
- an explode contributes NO scan (:414-416); in `merge` (:255-270) a join of two scans
  trips the record-type check first, "Unsupported query, expected to find exactly one
  type filter operator", thrown by `pickOneRecordTypeName` (:273-277), before the
  "join indexes are not supported" assertion (:260-262);
- every node's result is checked, "operator %s returns a non-record value" (:418-433);
- only the INNERMOST select may own a predicate (:387-401), and a HAVING predicate is
  "found predicate in select-having".
Values are resolved by `QuantifierValues.resolve`, which dereferences a quantifier
through its producer and then SIMPLIFIES (QuantifierValues.java:78-106, 126-155), so
`ek.k` over a derived table `(select k from t6.c) as ek` resolves to `t6.c*.k`; each
explode quantifier maps to its collection FieldValue with the LAST accessor wrapped in
an `AnnotatedAccessor(marker)`, marker a per-plan explode counter, and
`ValueToKeyExpressionVisitor.fieldAccessorToKeyExpression` (:377-396, the admission
check at :392) renders an annotated accessor as FanOut both as a leaf and as a
NESTING PARENT (:323, :362), and admits a plain array accessor only under CARDINALITY.

MEASURED (shapes, Java / Go): `unnest_comma`, `unnest_derived`,
`unnest_same_array_twice` Java OK with roots `concat(COL2, nest(COL4, field(values,
FAN_OUT)))`, `concat(A, nest(C, nest(values FAN_OUT, K)))` and two identical `nest(COL4,
values FAN_OUT)`; `unnest_nested` (`t.a as "x", "x".v as "b"`) OK / Go 0A000 type-
filter; `unnest_nested_derived` OK / Go 42F01; `derived_no_unnest` (`from (select a from
t) as d`) OK / Go 0A000; `unnest_outer_predicate` OK / Go 42F00; `unnest_inner_predicate`
Java 0A000 "Unsupported predicate '<alias> GREATER_THAN 1'" / Go 42F00; `having` and
both join shapes (`join_plain`, `join_with_unnest`) the SAME message in both engines
(the type-filter message: the join precedence is measured, not assumed). 21 isolation
runs carry the same Go rejections (42F00 "Unknown database R/T6/row/RR" is the index-
DDL path resolving `alias.arrayField` in FROM as a database-qualified table, a mis-
resolution, not a Java rejection).

Change: port `IndexSpec`, `QuantifierValues` and the generator's use of them onto the
Cascades graph the index query translates to, as Java runs them, not onto Go's
pre-lowering logical tree. Java plans the index definition's query with literal
processing disabled (DdlVisitor.java:266, :371) into a `RelationalExpression` graph and
hands that graph to `MaterializedViewIndexGenerator.generate` (:95-100), which calls
`IndexSpec.collect(expression, QuantifierValues.collect(expression))`. Go's query path
already lowers the same logical tree into that graph (`TranslateToCascadesWithError`,
`cascades_translator.go:65`, over the metadata built so far, as Java builds
`metadataBuilder.build()` before planning the index query). The two graphs are NOT the
same shape, and the port says how each Go node maps rather than pretending they are.
Java's translation of an index query (`LogicalOperator.generateSelect`,
LogicalOperator.java:375-401) is always a LogicalSortExpression over a SelectExpression
that owns the WHERE predicate and whose result value is the SELECT list (an unsorted
sort when there is no ORDER BY, :553-560), and DdlVisitor asserts that top node
(DdlVisitor.java:274): an ORDER BY over an expression the SELECT list lacks becomes
Select(Sort(Select)) and is refused with INVALID_COLUMN_REFERENCE "Cannot create index
and order by an expression that is not present in the projection list". Go's
translator lowers WHERE and HAVING to LogicalFilterExpression (`exactFilter`,
`cascades_translator.go:283-293`), the SELECT list to LogicalProjectionExpression, and
emits Project(Sort(Filter(...))) with an ORDER BY and Project(Filter(...)) without one
(`translateProject`, :6712 on). The port's entry point therefore reads Go's top as
Java's: a LogicalProjectionExpression over a LogicalSortExpression is Java's
Sort(Select) with the projection as the select's result value, a
LogicalProjectionExpression with no sort under it is Java's unsorted Sort(Select), and
a sort key that is not one of the projected values is refused with Java's
INVALID_COLUMN_REFERENCE and message (the Go form of the top-node assertion); any
other top node is refused the same way. The top-node check runs BEFORE the IndexSpec
port, as Java's does (DdlVisitor.java:274 asserts the top before `generate()` runs
`IndexSpec.collect`, MaterializedViewIndexGenerator.java:95-97). MEASURED (v5, four
`top_*` shapes of the oracle, both engines' codes pinned, messages in the run logs):
`select a from t order by b` is 42F10 with that message in both
[top_order_key_not_projected]; the same unprojected key over a JOIN is 42F10 in the
target, whose top check fires first, and 0A000 "expected to find exactly one type
filter operator" in Go today, whose IndexSpec check fires first
[top_order_key_not_projected_join]; an index query with LIMIT is 0AF00 "LIMIT clause is
not supported." in the target and 0A000 in Go today [top_limit]; and an EXISTS predicate
is 0A000 in both, each engine rendering its own predicate [top_exists]. WHERE the target
refuses LIMIT decides its precedence: `AstNormalizer` visits the WHOLE statement in parse
tree order (`visitChildren`, AstNormalizer.java:176-188, and a DDL statement's children,
:315-318, entered from PlanGenerator.java:167), and every `LimitClauseContext` it meets
fails there, OFFSET before LIMIT within one clause (:254-262). The grammar admits OFFSET
only after LIMIT (`limitClause: LIMIT limit (OFFSET offset)?`, RelationalParser.g4:582-584),
so "OFFSET alone" and "both" are one shape, and its message is the OFFSET one. The same
walk refuses two more shapes, in the same tree order: an IN list that is a nested SELECT,
0AF00 UNSUPPORTED_QUERY "IN predicate does not support nested SELECT"
(`visitInPredicate`, :446-463, the assertion at :458-461), and a bare NULL item of an IN
list, 42809 WRONG_OBJECT_TYPE "NULL values are not allowed in the IN list"
(`rejectNullItems`, :507-511). So a limit clause
ANYWHERE in a CREATE SCHEMA TEMPLATE statement, in an index query, a derived table
inside one, a view body or a function body, is refused before any clause of the template
is processed, an earlier faulty table or index included, and across clauses the first in
tree order decides (an earlier clause's LIMIT wins over a later clause's OFFSET), and the
same holds for an IN over a nested SELECT and a NULL in an IN list, anywhere in the
statement. The port runs the check where the target does: one walk of the typed parse
tree of the whole template statement, before its clauses are built (never in an index
query's translation, which would let an earlier clause's fault win), visiting every
`LimitClauseContext` and every `InPredicateContext` in tree order and failing on the
first fault: OFFSET before LIMIT within a limit clause, and within an IN predicate the
nested SELECT (an `inList` with a `queryExpressionBody`) before the NULL items (an
`expressions` list whose item is a bare NULL, found by descending single-child nodes only,
as `isNullLiteral` does; `IN ?param` and `IN column` write no list and are not checked),
each with the target's code and message. The order is PRE-order, as the target's is: an
IN predicate's two checks run at ENTRY, before any of its descendants is visited
(`visitInPredicate` asserts and calls `rejectNullItems` before its item loop,
AstNormalizer.java:458-463), so a LIMIT inside the nested SELECT is never reached (the
nested-SELECT message), and a LIMIT inside an item of a list that also holds a bare NULL
loses to the NULL message even when that item comes first. Go's IN-subquery extension,
were it present, would be a QUERY extension, as LIMIT is (at the base Go refuses every
IN-subquery with 0AF00, as the target does; the umbrella RFC's WS-E section), and a
template statement is not a query, so neither reaches it. It runs even over clauses Go cannot build yet (a view, owned
by WS-K), since it runs before any clause is built. LIMIT is the approved Go extension
for QUERIES, and a template statement is not one (an index, view or function body has no
row limit to honour), so the extension does not reach it. Tests, each asserted against
the target's code and message (oracle shapes added with the port): an index with LIMIT
and OFFSET (the OFFSET message; the one OFFSET shape the grammar admits), with LIMIT in
a derived table of its query, a view body with LIMIT, a function body with LIMIT, an
earlier index with LIMIT before a later one with LIMIT and OFFSET (the LIMIT message), a
template whose earlier clause is faulty (an unknown column in a table's primary key)
beside a later index with LIMIT; an index whose WHERE has `x IN (SELECT ...)`, one with
`x IN (1, NULL)`, and one with `x IN (NULL + 1)` (not a bare NULL, so not refused by the
walk). A nested SELECT and a NULL item cannot share one list, so precedence is pinned
across predicates and clauses: an earlier `IN (1, NULL)` before a later LIMIT (the NULL
message), and an earlier LIMIT before a later `IN (SELECT ...)` (the LIMIT message); and
within one predicate, by the entry order: `x IN (SELECT y FROM t LIMIT 1)` (the
nested-SELECT message, not the LIMIT one) and `x IN (EXISTS (SELECT y FROM t LIMIT 1),
NULL)` (the NULL message, though the LIMIT comes first in the text). The grammar has no
scalar subquery among an IN list's items (`expressions` holds `expression`s, and of those
only `EXISTS '(' query ')'` holds a query, RelationalParser.g4:1245-1276), so the EXISTS
item is the only way to put a LIMIT beside a NULL in one list. Go's other top
shapes, the EXISTS fold's Sort(Select) and a hoisted Limit, are thereby either refused
at the top (LIMIT) or reach IndexSpec's predicate arm as the target's do (EXISTS). The
two rows that change code flip with this section and their Go class pins move with it.
Below the top, every arm, Java's and Go's
(`pkg/recordlayer/query/plan/cascades/expressions/`):

| Java visitor arm (IndexSpec.java) | Go expression | Rule ported |
|---|---|---|
| `visitFullUnorderedScanExpression` (:356-358) | `FullUnorderedScanExpression` (full_unordered_scan.go) | result check, merge, scan count + 1 |
| `visitLogicalTypeFilterExpression` (:362-370) | `LogicalTypeFilterExpression` (logical_type_filter.go) | exactly one record type, else "... exactly one record type in type filter operator, however found <names\|nothing>"; records it |
| `visitGroupByExpression` (:374-379) | `GroupByExpression` (group_by.go) | at most one aggregate, else "... group by expression with more than one aggregation"; records it |
| `visitSelectExpression` (:387-402), predicate half | `LogicalFilterExpression` (logical_filter.go), and `SelectExpression` (select.go) where Go builds one | the innermost node with predicates owns them (normalized as IndexPredicates); a filter or select with predicates ABOVE a group-by (Go's HAVING lowering) is "found predicate in select-having", above an owning filter or select "found predicate in inner-select", both Java's messages, precedence as :387-401 |
| `visitSelectExpression` (:387-402), result half | `LogicalProjectionExpression` (logical_projection.go), and a SelectExpression's result value | result check over the projected Values; the projection is the result value the generator reorders |
| `visitLogicalSortExpression` (:405-407) | `LogicalSortExpression` (logical_sort.go) | records the ordering (resolved values and ordering functions) |
| `visitExplodeExpression` (:414-416) | `ExplodeExpression` (explode.go) | merge only: no scan, no result check |
| every other class (`evaluateAtExpression`, :345-349) | every other Go expression | result check (see the Value table) and merge; the message names the Go expression's class |
| `evaluateAtRef` (:353) | a Reference's members | merge |

The result check admits a RECORD result whose every field is one of Java's six Value
classes (IndexSpec.java:418-442). Go's classes for them, which is where the port reads
them, and why section 6 lands first (section 8):

| Java Value class | Go Value class |
|---|---|
| FieldValue | `values.FieldValue` |
| QuantifiedObjectValue | the quantified object values (`values.AsQuantifiedObjectValue`) |
| AggregateValue | `values.AggregateValue` and `values.IndexOnlyAggregateValue` |
| ArithmeticValue | `values.ArithmeticValue`, which after section 6 carries the bit and bitmap operators (today they are ScalarFunctionValues, `scalar_function_catalog.go:397-409`, and the check would refuse `bitmap-aggregate-index.yamsql:21` and the bitmap and bitand shapes that are equal today) and after RFC-257 WS-E 5.6 carries MOD |
| LiteralValue | `values.ConstantValue` (Go's literal) |
| CardinalityValue | `values.CardinalityValue` (value_cardinality.go) |

`merge` (:255-270) is ported with its precedence (record type, then the join assertion,
then predicate / group by / sort, each "more than one <what> found"), and
`checkValidity` (:166-167) with "no iteration generator found". `QuantifierValues.
collect`/`resolve` (QuantifierValues.java:67-106, 126-155) is ported over Go's
Quantifier and Value types: a quantifier reference is dereferenced through its
producer's result value and then simplified, so `ek.k` over the derived table
`(select k from t6.c) as ek` resolves to the field path through the unnest; each explode
quantifier maps to its collection FieldValue with the LAST accessor wrapped in an
`AnnotatedAccessor(marker)`, marker a per-plan explode counter. The generator then
consumes the spec exactly as `MaterializedViewIndexGenerator.generate` (:95-117):
projection reordering, key/value split, aggregate permutation options, and
`ValueToKeyExpressionVisitor` over the resolved values, which Go already ports (the
field trie of 3.1 and the literal carriers of 3.2 are its arms); `decompose`
(`generator.go:107-154`) and its shape arms are deleted. Consequences:
- the derived table and every unnest are ordinary graph nodes; nothing special-cases
  the shapes above, and the index-DDL path stops treating `alias.arrayField` in FROM as
  a database-qualified table because the translator resolves it as an Explode of the
  enclosing quantifier's field;
- `fieldAccessor` gains Java's marker, with Java's asymmetric equality: an annotated
  accessor equals another only with the same marker, and `FieldPath.isPrefixOf`
  compares with the PREFIX side's equals (FieldValue.java:676-685 against
  QuantifierValues.java:186-194), so Go's `prefixMatches` calls equality with the
  prefix's accessor as the receiver; a test pins both argument orders;
- `fieldLeafExpression` and the nesting renderer render an annotated accessor as
  `FanOut` both as a leaf and as a nesting parent (ValueToKeyExpressionVisitor.java:323,
  :362, :377-396; Go today renders a nesting parent with `recordlayer.Nest(name, ...)`,
  which is SCALAR, `generator.go:773`), and a plain array accessor keeps the "cannot
  create index on array field ... without unnesting" 0A000 (measured, shape
  array_no_unnest);
- `unnest_inner_predicate`'s message embeds a per-plan quantifier alias in the target;
  Go renders its own alias at that position (the message is shared up to the alias),
  and the oracle pins the SQLSTATE and class.
Tests: every arm of both tables driven on the REAL output of
`TranslateToCascadesWithError` for an index-definition query that reaches it (a WHERE,
a HAVING over a GROUP BY, a WHERE inside a derived table, an ORDER BY with and without
a sort key outside the SELECT list, a join, each Value class in the SELECT list, and a
non-record result), asserting the Go node classes the translator produced before
asserting the port's verdict, so a translator change that moves a shape is caught
where it happens; the precedence pairs (join before scan count, select-having versus
inner-select), the top-node refusal on the same real graphs, and the top check's
precedence over IndexSpec (the join and LIMIT shapes above); plus the oracle rows.
Acceptance: every run above becomes byte-equal or the same rejection; the 21 isolation
runs move out of java-accept-go-reject.

### 3.3b Serving nested leaf groupings from aggregate indexes (F9) — query engine

F1 made these indexes storable; the planner must use them the way Java does. The
nested grouping leaves are expanded by section 3.5's visitor (Java's
`AggregateIndexExpansionVisitor` extends the same `KeyExpressionExpansionVisitor`), so
this lands after 3.5. Go's aggregate-index matching (AggregateDataAccessRule and its candidate)
keys a grouping column by a top-level field; it must accept a nested LEAF path
(`home.city`) as one grouping column, one to one with the candidate's leaf key column,
including the leading-equality scan binding and the RFC-248 residual on a later
grouping column. A RECORD-typed grouping key (`GROUP BY home`) stays off the aggregate
index: Java cannot plan it, Go answers it through Scan + Aggregate (a read extension),
and RFC-248's ordinal rewrite has no rows pin over a leaf-expanded key;
`TestAggregateIndexResidual_RecordTypedGroupingKeyIsUnreachable` guards both
populations and flips its leaf arm with this change. Acceptance: the plan harness pins
the three measured Java shapes (unbound, residual FILTER on the third key, leading
equality bound); the indexed/unindexed twin in
`sqldriver/aggregate_index_residual_fdb_test.go` gains the same reads through its DML
sweep, each asserted served by the aggregate index.

### 3.4 Duplicated DDL front ends (F8)

Java has one visitor, `DdlVisitor.visitCreateSchemaTemplateStatement` (:493-566). Go has
`execCreateSchemaTemplate` (`embedded/ddl.go`) and `buildSchemaTemplateFromDDL`
(`embedded/cascades_generator.go`), whose own comment records they already built
different metadata once. They become one function that builds the `metadata.Builder`
from a `CreateSchemaTemplateStatementContext`; the execution path saves what it
returns, the tooling path returns it. The oracle then drives the production builder by
construction, and the "(tooling path)" labels in this document are retired.

### 3.5 Value-index expansion of nested leaves and function keys (F14) — query engine

MEASURED ("WS-J nested-leaf value index plan oracle", both engines' physical trees
pinned; the Go tree through EXPLAIN on the SQL runner): over `nested_then_top` (`s.x,
ts`) and `nested_y` (`s.y`) the target serves all six reads from the index —
`COVERING(NESTED_THEN_TOP <,> ...)` for `ORDER BY s.x, ts`, an EQUALS range for
`s.x = 5`, EQUALS plus GREATER_THAN for `s.x = 5 AND ts > 3`, `COVERING(NESTED_Y ...)`
for `ORDER BY s.y` and `s.y > 2`, and `ISCAN(NESTED_THEN_TOP [EQUALS ...])` for
`SELECT *` — while Go plans `InMemorySort(Scan(T))` or `PredicatesFilter(Scan(T))` for
every one. The Go-stored spec shows the same for function keys: the target serves
`ORDER BY bitmap_bucket_offset(id)` by `ISCAN(AGG_BUCKET <,>)` and `d & 1 = 1`, `d + 1 =
5` by COVERING index ranges; Go matches none of them.

Why Go cannot. Candidate construction drops every index whose root reaches a scalar
nested leaf (`keyExpressionContainsNonFanOutNestedLeaf`, called at
`plan_context_builder.go:171`, `index_expansion.go:54` and
`match_candidate_index.go:1179`), because the flat candidate bridge
(`keyExpressionFlatColumnDescriptors`, `index_expansion.go:908`) names a column by its
LAST field name, so `ADDR.CITY` could bind a top-level `CITY`. The same bridge declines
every function key except CARDINALITY and the four order functions (`index_expansion.go:
937-1000`), so no arithmetic or bitmap index is ever a candidate. The fan-out expansion
beside it (`expandFanOutValueIndex`) models Field, Then and Nesting over FAN_OUT only.

SOURCE, what Java does: every value index is expanded by ONE visitor,
`ValueIndexExpansionVisitor` over `KeyExpressionExpansionVisitor`, into a match
candidate graph whose columns are Values over the base quantifier:
- a scalar field is `FieldValue.ofFieldNames(base, prefix + name)`, the FULL path
  (KeyExpressionExpansionVisitor.java:129-181); a SCALAR nesting parent pushes its name
  onto the prefix and visits the child (:258-300 and on), so `S.X` and a top-level `X`
  are different Values by construction; a FAN_OUT field or parent explodes (:141-161);
  the nullable-array wrapper collapses as `NullableArrayTypeUtils.matchArrayWrapper`
  says (:266-296);
- a function key expands its arguments, then registers `functionKeyExpression.toValue(
  argumentValues)` (:203-233): for the long-arithmetic family,
  `LongArithmethicFunctionKeyExpression.toValue` resolves and encapsulates the SQL
  function of the same logical operator (:121-124), i.e. the very ArithmeticValue the
  query's `d + 1` or `bitmap_bucket_offset(id)` translates to;
- Then, List, KeyWithValue and Empty as at :123, :187, :237, :429, :443.
Matching is then ordinary Value matching of the query graph against the candidate
graph; nothing about nesting or functions is special-cased in the rules.

Change: Go's candidate construction becomes a port of that visitor. The flat bridge and
the FAN_OUT-only expansion are replaced by one `expandValueIndex` over the stored root
that builds the candidate columns as Values over the base quantifier, arm for arm with
the Java visitor above (Field with its three fan types, Nesting with the prefix and the
array-wrapper collapse, Then, List, KeyWithValue, Empty, Function via a Go `toValue` per
function family: arithmetic and bitmap functions encapsulate through the same
construction the SQL translator uses for the expression — which section 6 makes the
lane-resolving ArithmeticValue — CARDINALITY to the cardinality Value, the order
functions to their ordering wrapper). Column identity is the Value, so the reason for
`keyExpressionContainsNonFanOutNestedLeaf` is gone, and the function and its three call
sites are deleted, not relaxed. The candidate's coverage, ordering and PK-suffix
surfaces (`metadataSufficientForPlanning` and its callers) read the expanded Values
instead of flat names. One Go-only rule remains, and it is a declared divergence (3.2,
DIVERGENCES.md): a long-arithmetic function whose argument Values are not all INT or
LONG typed expands to no candidate, because its stored entries are not the value of the
query expression.

A STORED key with no lane, decided. The function arm builds the key's Value through
section 6's construction-time lane resolution, and metadata a Go build before F2 stored
holds keys that have none: `bitmap_bucket_offset(id)` with its entry size as
`long_value` is (LONG, LONG), a pair the bitmap functions have no lane for
(ArithmeticValue.java:515-522). "No other lane is reachable" (section 6) holds for SQL,
not for stored metadata. MEASURED over the `long_value` variant (section 0): the target
fails EVERY query of the table, not only the ones the index would serve (a primary-key
equality, its EXPLAIN, an ordered scan and a count are all XX000 "unable to encapsulate
arithmetic operation due to type mismatch(es)"), because candidate expansion catches
only UnsupportedOperationException (MatchCandidateExpansion.java:101-131) and the
VerifyException from `toValue`/`encapsulate` (LongArithmethicFunctionKeyExpression.
java:121-124, KeyExpressionExpansionVisitor.java:223) escapes it and fails the query.
Go DECLINES THE CANDIDATE instead: when expansion of a function key meets the lane
table's no-lane refusal (the typed error section 6 raises, and nothing broader: any
other expansion error still fails the query, as in the target), that index is not a
candidate, the query is planned without it and answered from the other access paths,
and the decline is counted where the planner counts declined candidates. Propagating
would make every query of an un-rebound Go tenant's table fail after the upgrade where
Go answers it today, for metadata that Java's own DDL never produces; declining loses
only an index the planner could not have used, so the answer is the same rows the
target would return if its expansion declined. Declared in DIVERGENCES.md ("a stored
function key with no lane"), with the four pinned target rows. Tests: the plan harness
over the `long_value` metadata asserts each of the four T1 reads plans without AGG_BUCKET
and BM and answers; a unit test asserts that a different expansion error still fails;
and section 4's carry path (test 5) moves such a tenant to the `int_value` key, after
which the index is a candidate again.

Literal matching and the plan cache, recorded at both sites. The target matches the
query's `_.D & @c7` against the stored literal through `ConstantValueEquivalence` and a
`QueryPlanConstraint` that re-checks the constant when a cached plan is reused
(ValueEquivalence.java:351-395; MEASURED as the COVERING plans of section 0's Go-stored
spec). Go matches a stored literal by value with no such constraint
(`cascades/value_equivalence.go`), which is sound only because the plan cache keys on
the literal text (`embedded/query_hash.go`), so another literal is another entry. Both
sites now say so and point at each other (landed in v4, comments only); literal
parameterization of the cache, a separate reach item there, must port the constraint
in the same change or it would serve `d & 1`'s plan for `d & 2`.

Dependencies and order: the Value a bitmap or bit-operator key expands to must be the
Value the query translates to, so section 6 lands first; section 3.3b's aggregate
grouping keys use this expansion for their nested leaves, so 3.3b lands after.

Tests: the plan harness pins each of the six WSJV trees in Go's plan vocabulary (index
scan, covering where the target covers, the EQUALS and GREATER_THAN ranges), the three
Go-stored function-key EXPLAINs (bitmap ISCAN, `d & 1` and `d + 1` ranges), and the
non-integer declines (the four arithmetic reads of the non-integer spec stay scans); the
oracle's Go pins for WSJV and the Go-stored EXPLAINs move to the index plans in the same
change; a disambiguation test builds `ADDR.CITY` and a top-level `CITY` indexes over
one table and asserts each query binds the right one (the hazard the deleted gate
existed for); the planner determinism loop and the 1M stress comparison run before and
after (candidate construction changes costs everywhere a nested or function index
exists). The implementation takes a Graefe review at the WS-J query-engine milestone,
as every query-engine change does.

### 3.6 Key-expression deserialization is Java's, whole (v14)

[v16 → 4d, "The deserialization port": function names are not refused at load; arity is checked for
Go's registered functions; required-field refusals are parse-level on stored bytes and
constructor-level in memory; absent children, the count key's wrapping and `NullStandin`
are added.]

v13 ported one of Java's `KeyExpression.DeserializationException` throws, the absent
root, and the v13 gates found the rest of the port partial. So a stored key expression
Java refuses at load still loads in Go and fails later, or reads under a default. Step 3
completes it, in `key_expression_proto.go`, `errors.go` and `metadata_proto.go`:
- A `Field` (and a `Nesting`'s parent, which is a Field) without `field_name` is refused
  with "Serialized Field is missing field name", and one without `fan_type` with
  "Serialized Field is missing fan type" (`FieldKeyExpression.java:122-128`). A fan type
  outside SCALAR, FAN_OUT and CONCATENATE is refused with "Invalid fan type N"
  (`KeyExpression.java:190-197`). Go's `fieldFromProto` has no error path today, and it
  reads a missing fan type as SCALAR.
- A `Function` is created at load, as Java's `FunctionKeyExpression.fromProto` creates it
  through `create` (`FunctionKeyExpression.java:117-131, :235-240`). An unknown name is
  refused with "Function not defined", and an argument count outside the function's
  bounds with "Invalid number of arguments provided to function". Each is Java's
  `InvalidExpressionException` message, re-thrown as a `DeserializationException` with
  the same message. Go's `functionFromProto` builds any name today and fails only on
  evaluation (`key_expression.go:1546`).
- The class is Java's hierarchy. `DeserializationException` extends
  `RecordCoreException` (`KeyExpression.java:448`), so `KeyExpressionDeserializationError`
  gains `Unwrap() error` returning a `RecordCoreError` with its message, the pattern
  `UnknownIndexTypeError` uses for its parent. The two assertions that it is NOT a
  `RecordCoreError` (`metadata_proto_test.go:717` and the conformance spec) are inverted.
- The loader wraps it as Java's does. `RecordMetaDataBuilder.loadFromProto` catches a
  `DeserializationException` from an index or a primary key and throws
  `MetaDataProtoDeserializationException`, "Error converting from protobuf", a
  `MetaDataException` with the deserialization error as its cause
  (`RecordMetaDataBuilder.java:205-227, :1540-1543`). Go gains
  `MetaDataProtoDeserializationError{Cause}` with that message, which unwraps to both a
  `MetaDataError` and its cause. The loader returns it where it now wraps with
  `fmt.Errorf` (`metadata_proto.go:292-294`).
- The JVM oracle measures both layers. Its step now also loads a whole MetaData through
  `RecordMetaDataBuilder` and records the outer class and the cause's class and message.
  v13's step measured `new Index(proto)` only, and so compared Java's inner layer with
  Go's loader.
- Pins: a unit test per refusal (missing name, missing fan type, fan type 7, a Nesting
  whose parent lacks a fan type, an unknown function, and a registered function given
  fewer arguments than its minimum), each asserting the message, `errors.As` for `KeyExpressionDeserializationError`
  and `RecordCoreError`, and, through the loader, `MetaDataProtoDeserializationError`
  and `MetaDataError`. The WSJIXK spec rows for the same six shapes run on both engines.
  A mutation per refusal is part of step 3's evidence.

## 4. Metadata assembly order (F3) and the upgrade path

SOURCE, the whole chain:
- `DdlVisitor.java:547-548` registers structs then tables in declaration order into a
  `LinkedHashMap` (`RecordLayerSchemaTemplate.Builder`, :438, :488).
- `:559-564`, AFTER all index clauses have been generated: for each index clause in
  clause order, `extractTable` (remove) then `addTable` (put), so the table MOVES TO THE
  END (a `put` of an existing key would keep its slot; the remove is what moves it).
- `RecordLayerSchemaTemplate.accept` (:392-405) visits tables in that order and each
  table's indexes in insertion order (`RecordLayerTable.accept` :133-143).
- `FileDescriptorSerializer` adds one union field per visited table in visit order
  (:102-112) and emits each table's types from a TreeSet (:131-138, TypeRepository.java:
  269-271); `RecordMetadataSerializer.visit(Table)` sets `recordTypeKey =
  recordTypeCounter++` (:70); `visit(Index)` calls `addIndex` (:83), which bumps the
  metadata version per index (RecordMetaDataBuilder.java:1103-1104) from the template
  version 1 (DdlVisitor.java:497).

MEASURED (multi_table_order: A, B, C; ia(A), ib(B), ia2(A)): final order C, B, A →
type keys C=0, B=1, A=2; union fields C=1, B=2, A=3; index versions IB=2, IA=3, IA2=4;
metadata version 4 (Go, tooling path: A=0, B=1, C=2; IA=2, IA2=3, IB=4). At scale:
every one of the 180 metadata-diverge runs and the 4 index-diverge runs of section 0 is
this.

Why this is wire. The record-type key is the leading element of every record key for
non-intermingled tables and the union field number frames every stored record. Within
one catalog both engines read the ONE stored template, so day-to-day interop holds; the
hazard is every path that REBUILDS a template from DDL text in the other engine.

Change, in the unified front end (3.4): register types and tables in declaration
order, generate every index, THEN for each index clause in clause order move its table
to the end of the builder's table list (a `metadata.Builder` operation mirroring
`extractTable` + `addTable`, used ONLY by the DDL front end, as in Java; the move
happens once, after all indexes are generated, so Go's per-index `Build()` reruns see no
intermediate order). `Build()` then numbers union fields, type keys and descriptor
messages in table order and registers each table's indexes in insertion order, so
versions follow. The `builder.go` comment that calls the type key the "0-based
declaration index" is rewritten.

What the port changes for DDL Go has ALREADY persisted, all of it: the root of an index
whose key has a nested field followed by a top-level column (3.1, F1: the nested
subtree was dropped); literal widths (3.2, F2); record-type keys, union field numbers,
descriptor order and index versions (this section, F3); option order (4b, F12);
companion placement (below). Stored templates keep their bytes; nothing rewrites them.
What changes is what a REBUILD from the same DDL produces, so every path that rebuilds is
decided here. Java has no counterpart to decide it: Java's SQL creates only version 1 of
a template (DdlVisitor.java:497; `createTemplate` saves with ERROR_IF_EXISTS on (name,
version), RecordLayerStoreSchemaTemplateCatalog.java:229-241), only its programmatic
API creates higher versions, and its `saveSchema`/`repairSchema` run no evolution check
at all (RecordLayerStoreCatalog.java:232-279). New versions from DDL are a Go extension
(CREATE SCHEMA TEMPLATE over a stored name, and `fleet.SaveTemplate`), so the policy is
Go's:
- A FRESH template (no version of that name stored) takes Java's numbering exactly. A
  name whose template was dropped while schemas still bind it is refused (section 2),
  because the rebuild would silently re-key those schemas' data.
- A NEW VERSION of a stored template is CARRIED from the latest stored version. Where:
  `SaveSchemaTemplateConstantAction.Execute`, the one save path both CREATE SCHEMA
  TEMPLATE (`execCreateSchemaTemplate` through the factory) and `fleet.SaveTemplate`
  take; it already loads the latest version to validate against. A metadata-level
  `carryNumbering(stored, built)` rewrites the built template before it is validated and
  stored:
  - each record type present in the stored version keeps its record-type key, union
    field number and since-version; a NEW record type is numbered above the stored
    maxima in Java's table order and gets since-version = the new metadata version
    (the validator refuses a new type without one, "new record type is missing since
    version", MetaDataEvolutionValidator.java:436-444, Go `validateRecordTypes`' arm of that message; Go's builder
    never set it);
  - an index is matched to the stored one of the same name and falls in exactly ONE of
    three classes, decided by comparing the two Index protos FIELD BY FIELD over the
    proto's KNOWN fields, each read as Java's `new Index(proto)` reads it
    (Index.java:194-235): a stored `index_type` is compared as the type and options it
    becomes (`indexTypeToType`, `indexTypeToOptions`), and the stored option list of
    such an index is then not compared, because Java does not read it; a stored
    `value_expression` is compared as the root it is folded into
    (`toKeyWithValueExpression`); an ABSENT root is refused, "Exactly one root must be
    specified for an index" (KeyExpression.java:404-405, read before the key; v12, where
    Go loaded the index with no root, pinned on both engines by the WSJIXK spec's fifth
    shape and by `TestStoredIndexSubspaceKeyIsReadAsJavaReadsIt`); an absent `type` is
    VALUE; and a RANK, COUNT,
    MAX_EVER, MIN_EVER or SUM root that is not a grouping is compared as the grouping
    Java wraps it in. Unknown fields and extensions are NOT compared (neither engine
    reads them); they are carried, as EQUIVALENT says. THE STORED SIDE IS THE RAW CATALOG BYTES, never the
    loaded metadata: template save loads the latest version through `LoadSchemaTemplate`
    and `RecordMetaDataFromProto` (save_schema_template.go:32-35), where `indexFromProto`
    reads each Index as Java's `new Index(proto)` does, folding the deprecated
    `index_type` into `type` plus a `unique` option (metadata_proto.go, the `index_type`
    arm) and, since v8, the deprecated `value_expression` into the root
    (`keyWithValue(concat(root, value), root size)`, Index.java:215-218, :245-251) and an
    absent `added_version` into 1 (:227-233; v7's loader ignored `value_expression`, so
    an index stored with one was maintained under the bare root, and defaulted the added
    version to the last-modified one; the JVM spec "WS-J stored index protos read as Java
    reads them" pins six such protos equal to Java's reading, on root, versions, type
    and options, `index_type` beside a stored option list among them, whose list both
    engines ignore, the branch this comparison relies on; it compares ROOTS, and equal
    roots give equal entries because the entry of a KeyWithValue root is itself
    compared between the engines by "Covering Index (KeyWithValueExpression)
    Conformance", conformance/covering_index_conformance_test.go, as decoded tuples with
    integers folded through `toInt64` (v10 said byte-compared; it is not, :57-69); and a stored SUBSPACE
    KEY is read as Java reads it, one non-null item or a refusal with Java's class and
    message: an empty key and a key of two items "subspace key must encode a single
    item tuple" (`RecordCoreError`, Java's RecordCoreException), a null item "Index
    subspace key cannot be null" (`RecordCoreArgumentError`), where the loader used to
    fall back to the index's name and so maintain the index under a subspace Java never
    reads, pinned on both engines by the same spec and by
    `TestStoredIndexSubspaceKeyIsReadAsJavaReadsIt`, both of which since v11 read Go's
    side from the MARSHALLED bytes (v10 handed Go the in-memory message, so the presence
    of an empty key was never driven through Go's decoder) and require the class per
    shape; the options are read with the type, before the root and the key, as
    `Index(proto)` reads them (Index.java:198-221), so a repeated option beside an empty
    key is the repeated option in both engines (v11, the spec's fourth shape; v10 read the
    options last); a stored FORMER index's key is read as `FormerIndex(proto)` reads it,
    the same refusals with "FormerIndex initialized with null subspace key" for a null
    item and an absent key refused too (FormerIndex.java:51-68; v10 converted only the
    Index, and Go read all four former-index shapes as a nil key, so a store's upgrade
    cleared the null item's subspace instead of the dropped index's), and a programmatic
    `SetSubspaceKey(nil)` is refused as Java's setter refuses it; these are in ws-c-design.md
    7.7, where the subspace-key comparison they feed is ported, and measured there on both
    engines (7.7's setter claim held only for a set made before `AddIndex`; ws-c-design.md
    7.8 and 7.9 cover every path); and the decoder's presence rule holds only for a FRESH message: vtproto's
    `ResetVT` keeps a non-nil `SubspaceKey[:0]` (record_metadata_vtproto.pb.go:1993,
    :2002), so a POOLED Index decoded over a message without a key would read the key as
    present and empty, and be refused; nothing pools a MetaData, Index or FormerIndex
    message (`git grep -nE 'MetaDataFromVTPool|IndexFromVTPool|FormerIndexFromVTPool' --
    '*.go' ':!gen'` finds no line; the control `UnionDescriptorFromVTPool` finds two, in
    store_test.go), and `TestReusedIndexMessageKeepsAnEmptySubspaceKey` pins what a reused
    message reads as, so a loader handed one fails loudly rather than silently), and `indexToProto`
    rebuilds a fresh `gen.Index` that drops `index_type`, `value_expression`, extensions
    and unknown fields, so a comparison of loaded against rebuilt would find every such
    difference already erased. `carryNumbering` therefore reads the stored version's
    `gen.MetaData` through a catalog method that returns the row's bytes unmarshalled,
    never passed through the loader: `api.SchemaTemplateCatalog.LoadTemplateProto(txn,
    name, version) (*gen.MetaData, error)` (v9 named no reader; none exists today). The FDB
    catalog unmarshals the `Templates` row's `META_DATA`; the in-memory catalog, which
    keeps template objects and no bytes, returns its stored template's `ToProto()`. That
    loses what its template never held (an unknown field, an extension), which is nothing
    another engine wrote: the in-memory catalog stores only templates Go built through
    `CreateTemplate`, the one BUILD entry (the restore is a raw-write route, but the
    in-memory catalog has no restore, section 2), and no bytes reach it (v10 said the same while
    its `CreateTemplateFromProto` took a library caller's bytes and dropped their unknown
    fields). It compares
    those Index protos, normalized as above, except three fields that the carry rule
    assigns rather than compares: `added_version`, `last_modified_version` and `subspace_key`. The
    remaining fields are compared the way the stored bytes are read: the record types
    by name as a set; the option list as a MAP, order-insensitive (an index stored
    before F12 differs from a rebuild in option ORDER only, which changes nothing a
    reader sees); the predicate proto-equal (both absent included), because a changed
    WHERE changes which records the index holds and neither validator compares
    predicates (Java's MetaDataEvolutionValidator has no predicate check, and Go's port
    has none either), so the carry rule must; every other field proto-equal, the root
    key expression included, except as the second class says.
    - EQUIVALENT: every compared field equal. The stored Index message is carried into
      the saved version as it was stored. The carrier is the BYTES, not the `Index`:
      `carryNumbering` returns the `gen.MetaData` to store, which is the rebuilt
      template's `ToProto` with, for each EQUIVALENT index, the stored raw `gen.Index`
      message spliced in place of the rebuilt one (a clone, at the rebuilt one's
      position). THE ROUTE TO STORAGE (v9; v8 named none: the only template writer,
      `CreateTemplate(txn, api.SchemaTemplate)`, serializes `rl.Underlying().ToProto()`,
      fdb_template_catalog.go:252-275, which rebuilds every index through `indexToProto`
      and so loses the splice). `CreateTemplate` of a stored name is the writer of a
      CARRIED version (v11; v10 put a bytes writer, `CreateTemplateFromProto`, on
      `api.SchemaTemplateCatalog`, and a library caller could reach it with bytes no
      check had seen): it reads the stored latest with `LoadTemplateProto`, runs
      `carryNumbering`, and hands the carried `gen.MetaData` to the catalog's unexported
      writer, which no other path reaches. Both catalogs run `deserializeTemplate`'s whole
      path on exactly those bytes [v16 → 4d: the (d) refusal and its 0A000 mapping are gone; the loader runs the target's checks] (`RecordMetaDataFromProto`, with every target check and the (d) refusal,
      then the tables, schema_template.go:52-93, whose construction can fail too; v9 ran
      the loader only), mapping (d) to the 0A000 of `LegacyUnionTemplateError` with the
      remedy of a template BEING STORED (`Stored` false: another name for the table or
      STRUCT), since those bytes are the new version's and nothing is stored yet (v11
      said "as the FDB catalog's load does", which marks `Stored` true and would have told
      the user no remedy exists; a (d) shape the stored latest already held is refused
      earlier, where the relational validator loads the stored latest, with `Stored` true,
      which is right there), so the invariant "Go never stores a template no Go session
      can load" holds by construction on this route, not only for (d). The FDB catalog
      then writes `proto.Marshal(md)` as the row's `META_DATA`, never re-deriving it
      through `ToProto`; the in-memory catalog stores the template it loaded. The version
      guard runs in both (above). Inside `CreateTemplate`, between the carry and the
      write: the lane check (3.2) over the indexes carried as REBUILT protos (NEW and
      CHANGED; an EQUIVALENT or WIDENED index is carried with the bytes it had and not
      re-checked, since no widening removes a lane, 3.2), then
      the evolution validator over `RecordMetaDataFromProto` of the
      carried `gen.MetaData` against the stored latest. For a fresh name `CreateTemplate`
      carries nothing and writes as it does today. The save path
      (`SaveSchemaTemplateConstantAction.Execute`) calls `CreateTemplate` and nothing else,
      so the two checks `Execute` runs today before it (save_schema_template.go:26-43), the
      refusal of `v′ <= latest` and the relational validator over the stored latest and the
      new template, MOVE INTO `CreateTemplate` of both catalogs, before the carry and
      AFTER the exact-duplicate refusal: `CreateTemplate` first refuses a (t, v′) that is
      stored with DUPLICATE_SCHEMA_TEMPLATE, Java's only refusal
      (`RecordLayerStoreSchemaTemplateCatalog.java:229-245`; `api/catalog.go:33-36` and
      `TestFDB_TemplateDuplicateReturnsError` pin that code), then `v′ <= latest` with
      INVALID_SCHEMA_TEMPLATE, then the relational validator (v12 put `v′ <= latest` first,
      which would have turned an exact duplicate into INVALID_SCHEMA_TEMPLATE). The two
      moved refusals are a LIBRARY-API divergence from Java's `createTemplate`, which
      refuses only the duplicate; section 9 (w) declares it, and the comment at
      `fdb_template_catalog.go:120-128`, which describes today's code, is rewritten with
      the change (v11 said "nothing else" and left them in `Execute`, so a library caller's
      `CreateTemplate(t, v′ < latest)` for a version not stored was carried and written
      below latest, a pre-upgrade `DeleteTemplateVersion` having left exactly such a hole
      and perhaps a dangling (t, v′) binding the guard's range from latest + 1 never
      reads; with the refusal in `CreateTemplate` no BUILD-PATH writer stores a version at
      or below the latest, so section 4's "latest suffices" holds for every build-path
      writer; the restore, a raw writer, stores a lower version by design and runs its own
      checks, section 2). `fleet.SaveTemplate` returns the template AS STORED,
      `(api.SchemaTemplate, error)` (it returns only an error today, fleet/migrate.go:
      49-53), since the caller's object holds the numbering before the carry. So what is validated is what is stored, and the loader reads a
      spliced `index_type` or `value_expression` as the target does (above). v7 carried it on the `Index` instead, through an unexported
      `storedProto` that `indexToProto` emitted and every setter cleared; that is
      dropped: it put a second definition inside every `Index`, one the validator never
      read (it validated the in-memory index while the bytes stored were the carried
      proto), and it depended on every present and future setter remembering to clear
      it. So a stored option order, a deprecated `index_type` or `value_expression`,
      unknown fields and extensions all survive the rebind, and nothing is rebuilt. The
      claim is PROTO equality of that Index message (`proto.Equal` of the stored and the
      saved `gen.Index`, which compares unknown fields as bytes), not byte equality of
      the template: the template is marshalled again, and a marshal is not a splice. A
      unit test stores an index carrying `index_type`, one carrying `value_expression`,
      and one carrying an unknown field and an extension, saves the same DDL again, and
      asserts each is EQUIVALENT, its saved Index message proto-equal to the stored one,
      and the metadata the save validated equal to a load of the saved row. The pin is at
      the CATALOG layer: it reads the `Templates` row's `META_DATA` back from the store and
      compares the Index message there, not a loaded metadata's. And end to end, through
      the Go SQL driver: a template version stored with a `value_expression` index (a raw
      write of its catalog rows), then CREATE SCHEMA TEMPLATE of the same name and DDL
      (the new version), and the new row's Index message is the stored one.
    - WIDENED: every compared field equal except the root, and the roots equal under 3.2's
      one-way literal-carrier arm, decided by the SAME function the validator runs,
      `literalCarriersEquivalent(stored, rebuilt)` (metadata_evolution_validator.go),
      never by a second implementation. The carried index is the REBUILT proto, whose
      root has the target's widths, with the stored `added_version` (as Java reads it:
      absent is 1), `last_modified_version` and `subspace_key`: its entries are the same bytes (the arm
      admits only carrier moves that store the same tuple), so nothing is rebuilt and no
      version moves, and the validator then compares a stored `long_value` root with a
      rebuilt `int_value` one, which is exactly the path on which its arm fires. This is
      what moves a pre-F2 tenant to the target's key (3.5's decline stops applying, and
      the target plans over it, test 5).
    - CHANGED: any other difference, an option the validator ignores included (Java's
      `getChangedOptions`, MetaDataEvolutionValidator.java:776-795, leaves such an
      option out of VALIDATION, not out of the stored bytes, and the carried proto must
      not silently drop the change). The carried index is the rebuilt proto with the
      stored `added_version` (absent is 1) and `subspace_key` and a last-modified version above the
      STORED METADATA version, because the store rebuilds on open exactly the indexes
      whose last-modified version exceeds the metadata version in its header
      (`checkRebuildIndexes`, FDBRecordStore.java:4959-4990; Go `GetIndexesSince`,
      store_builder.go:395), and the validator admits it under `allowIndexRebuilds`
      (:643-658, "old index has last-modified version newer than new index" guards only
      the other direction). A change in an ignored option alone is therefore rebuilt
      once, which costs a rebuild where the target's own evolution would not need one;
      it is the conservative side of the choice, and the carried bytes are the new
      definition in every class but EQUIVALENT.
    Declared: a WIDENED or CHANGED index is carried as the REBUILT proto, so its stored
    `index_type` and `value_expression` (as separate fields), unknown fields and
    extensions are not in the new version. The rebuilt proto states the same definition
    in the form the target itself writes (it reads `index_type` into a type and options
    and `value_expression` into the root), so no reader sees a difference; an unknown
    field or extension on such an index, which neither engine reads, is lost, and the
    unit test pins that it is.
    The field list the comparison walks is the proto's own descriptor, so a field added
    to `Index` by a later proto sync is compared without a code change; a unit test
    drives each class over each field (a change in one field at a time, the three
    assigned fields excepted) and asserts the class. A NEW index takes added = last-modified above the stored
    metadata version, in Java's order (the validator requires it, "new index has version
    that is not newer than the old meta-data version", :546-552). An index the stored
    version has and the new one lacks becomes a FormerIndex (its name, subspace key and
    added version, removed version = the new metadata version; the validator refuses a
    vanished index otherwise, "index missing in new meta-data", :500-503, Go `validateCurrentAndFormerIndexes`'
arm of that message),
    and stored FormerIndexes are carried forward;
  - a NEW index whose name is the subspace key of a carried FormerIndex (an index
    dropped in an earlier version and re-added under the same name) is REFUSED at
    template save, 42F59 INVALID_SCHEMA_TEMPLATE "index <ix> cannot be added: its name
    is the subspace key of index <ix> dropped at version <v>; add it under another
    name". The subspace a FormerIndex names is CLEARED when a store opens under the new
    metadata (store_builder.go:383-390) while the new index would be built into the same
    subspace, and both engines refuse that state: the builder's "former index subspace
    key reused" check (MetaDataValidator.java:103-112, Go `metadata.go:1250-1255`, the
    loop naming "used by index ... and former index") and Java's evolution-validator arm
    "former index key used for new index in meta-data" (MetaDataEvolutionValidator.java:
    487-491), which Go's port has had since WS-C revision 7 (`validateCurrentAndFormerIndexes`, the
    arm with that message; v11 said Go lacked it and would gain it in this step), as the guard for
    metadata built any other way. A fresh subspace key for the re-added
    index was rejected: the relational layer keys every index by its name, and a
    Go-only key scheme on the wire is worse than asking for a new name;
  - the metadata version is the highest version assigned above, and at least the stored
    version + 1 (Java :154-156; a lower version is refused, landed with 3.2).
  Why the LATEST stored version suffices although tenants can be bound to older ones:
  the template save validator refuses removing a table against the latest version
  (`schema_evolution_validator.go:40-45`), so each version's record types include its
  predecessor's; a dropped index leaves a FormerIndex every later version carries; and
  each carried version is carried from its predecessor, so every stored version agrees
  with the latest on every key they share. A tenant is rebound by `RepairSchema`, whose
  validator compares the tenant's BOUND version with the new one; a tenant bound to a
  pre-port version whose numbering an older Go build already shifted (the pre-port
  builder numbered types by declaration position) is refused with "record type key
  changed", which is the correct outcome: rebinding it would re-read its rows under
  other keys.
- The rebind validator's options, decided: `SetAllowNoVersionChange(true)` (an equal
  version is UPGRADE's no-op), `SetAllowLiteralCarrierWidening(true)` (3.2, landed),
  and `SetAllowIndexRebuilds(true)`. Without the last, every carried version that
  changes an index is refused, "last modified version of index changed" (Java :651-655,
  Go `validateIndex`'s arm of that message), including every tenant whose stored F1 root the rebuild corrects. Who
  rebuilds, and in what budget: the record store, when it is next opened under the new
  metadata, exactly as for an added index — `checkVersion` collects every index whose
  last-modified version exceeds the store header's metadata version
  (`GetIndexesSince`, store_builder.go:395; Java `checkRebuildIndexes`,
  FDBRecordStore.java:4959-4990), rebuilds it inline when the store holds at most 200
  records (`DefaultIndexRebuildPolicy`, store_builder.go:1132; Java's
  MAX_RECORDS_FOR_REBUILD) and otherwise leaves it DISABLED for the online indexer, the
  planner not using it meanwhile (pinned for the added-index case by
  `evolution_added_index_gate_fdb_test.go`); a FormerIndex's data is cleared on the same
  open (store_builder.go:383-390). No rebuild runs inside the rebind transaction.

Under this rule a Go-built new version never changes a key that stored data depends
on, and the validator (with the options above) admits it; the validator stays as the
guard for templates built any other way. FDB tests, one per case, each through
`fleet.SaveTemplate` + `RepairSchema` and `fleet.Migrate`, rows written before the rebind
read back by table after it:
1. v1 stored with the OLD Go numbering (built by the pre-port builder, whose output the
   test pins), v2 adds an index on the first table: keys and union numbers unchanged,
   the new index built on open;
2. v2 adds a TABLE (declared before an existing one, so Java's order would renumber):
   the old tables keep their keys, the new one takes the next key and a since-version,
   the rebind is admitted and the new table is writable;
3. v2 CHANGES an index (the pre-F1 truncated `nested_then_top` root against the
   corrected one): last-modified version above the stored metadata version, rebind
   admitted, the store rebuilds the index inline under the threshold (entries equal to a
   fresh build) and leaves it DISABLED above it (the planner does not use it);
4. v2 DROPS an index: a FormerIndex, rebind admitted, the index's data cleared on open;
5. a width-only v2 (3.2's arm), END TO END: v1's bytes are the PRE-PORT builder's, the
   builder at `e48f5b496` (the pre-upgrade commit this design cites for the old loader,
   before F1, F2, F3 and F12), run once
   over the test's DDL and committed as testdata, so the fixture carries the old
   record-type numbering and whatever option order that build produced (the carry rule
   compares options as a map, so the order is not what the test turns on). That builder
   already stores every INT literal as `long_value`, so the test ASSERTS the fixture's
   literals are `long_value` rather than rewriting them (`wsjWidenIntLiterals` would
   change nothing). v1 is stored by a raw write of its catalog rows (section 3.2's table:
   a pre-F2 bitmap key is refused on the build path), and 300 rows are written under it
   (above the 200-record inline-rebuild threshold, so a CHANGED index would be left
   DISABLED, as test 10 arranges). v2 from the SAME DDL through `fleet.SaveTemplate`:
   the index is WIDENED, v2's STORED root holds `int_value` where v1's held
   `long_value` (asserted on the stored proto, not only through a plan), the
   validator's widening arm fired on the rebind (reported by the validator run the
   rebind makes, through a result field of that run, never a process-global counter,
   which a parallel test could move), and the index keeps its added and last-modified
   versions and subspace key. The discriminating reads are the stored index STATE and
   the TARGET's plan, not a Go EXPLAIN: Go's planner declines every function key until
   section 3.5 (step 8, after the unit), so no Go read can be served by this index
   inside the unit. So the test asserts the index is not rebuilt and stays READABLE
   (its state, and its entries byte-equal before and after), the rows read back by
   table, and the target (Go-stored spec) plans the bitmap reads over v2 through the
   index; when 3.5 lands, its own tests add the Go index-served read. A mutation that
   carries the stored proto whole for a WIDENED index (v4's rule) reddens the
   stored-root assertion and the target's plan, and a mutation that classes it CHANGED
   reddens the READABLE-state assertion (the entry equality alone would not: a CHANGED
   index rebuilt inline would write the same bytes, which is why the fixture sits above
   the threshold);
6. a tenant bound to v1 while v3 is carried from v2: admitted;
7. the carry disabled (a mutation) on case 1: refused by the validator;
8. a v2 whose metadata version is lower: refused (landed, Ginkgo spec);
9. v2 changes ONLY an index's predicate (`WHERE v > 1` to `WHERE v > 2`): a CHANGED
   index, rebuilt on open, and a read through it after the rebind returns exactly the
   rows the new predicate admits (the case the validator alone would pass as unchanged);
10. v2 differs from v1 ONLY in an index's option ORDER (v1 stored before F12): an
   EQUIVALENT index, no rebuild, the stored bytes kept, and above the 200-record inline
   threshold the index stays READABLE (it would be left DISABLED if order counted);
   the same v1 with a SPARSE index (a predicate) in the pre-port fixture, built by the
   pre-port builder whose predicate bytes the test pins beside a rebuild's, so that
   predicate equality between an old store and a rebuild is measured, not assumed;
   and v2 changing only an option the validator ignores: CHANGED, the new option value
   stored, rebuilt on open;
11. v2 re-adds under its old name an index v1 dropped: refused with the message above,
   and the same metadata built by hand is refused by the validator's ported
   "former index key used for new index in meta-data" arm.
A fresh template in the same test takes Java's numbering, and section 2's guard has its
own tests.

RFC-209 group-existence companions (a kept Go extension). Today each table's companions
are registered right after that table's declared indexes (`metadata/builder.go:689`), so
each shifts the version of every index of every LATER table by one. They move AFTER ALL
declared indexes of all tables (after the owner table's indexes, today's placement, is
not enough, for that reason), so every Java-declared index carries Java's exact versions;
the metadata version still exceeds Java's by the companion count, the extension's
documented cost, recorded in DIVERGENCES.md (a Java rebuild of the same DDL has the lower
version: the same independent-rebuild class as above, now isolated to the companion
count). For an existing template the carry rule keeps a companion's stored versions.
The oracle's companion class (section 0) compares exactly this: Go's metadata with the
verified companions removed and the version slots they took given back must equal the
target's; after the move, a companion occupies the top slots and only the metadata
version is given back.

The ported validator's messages start with Java's message and carry the index name and
versions after it, in parentheses, where Java carries them as log keys (for example "index
missing in new meta-data (subspace key=..., index=...)"; WS-C revisions 7 and 8 put
Java's text first on every arm, v11 quoted the older Go message); the carry-rule tests
assert the error type and Java's message as the prefix.

NOT ported, with reasons: `record_types` order (a HashMap's iteration order, bound to JDK
bucket layout and Guava sizing: not stable across Java builds, no reader consults it);
anonymous type names and the relative order of anonymous types (random per Java build,
section 0); `indexes` order is already equal (a TreeMap by name, RecordMetaData.java:668).
Named descriptor message ORDER is ported (it is deterministic in Java and the oracle
compares it).

## 4b. Index option order (F12) — landed locally

SOURCE: `RecordLayerIndex.Builder` accumulates options in a Guava `ImmutableMap.Builder`,
whose iteration order is insertion order, and `Index.toProto` (:661-663) writes them in
that order; `Index(proto)` rebuilds them through `buildOptions` (:253-266), another
`ImmutableMap.Builder` in list order, so a stored order survives a Java round trip, and a
key listed twice fails that builder with Guava's IllegalArgumentException "Multiple
entries with same key: k=<later> and k=<earlier>" (MEASURED on the conformance server's
Guava 33.7.1 by the spec "WS-J duplicate index option": "unique=false and unique=true"
for the list unique, x, unique, and "a=2 and a=1" for two adjacent `a`, the inputs and
messages of Go's `TestIndexOptionOrder_FromProtoRefusesDuplicateKey`; a list without a
repeat is read as `{unique=true, x=1}`). The DDL insertion order is the generators' call order:
`MaterializedViewIndexGenerator` calls `setUnique` (:107) before the type-specific
options (`setOption(PERMUTED_SIZE_OPTION, ...)`, :174), so a permuted min/max index
stores `[unique, permutedSize]`. In 4.14.2.0 only the VECTOR index definition adds clause
options (`OnSourceIndexGenerator.addAllIndexOptions` from a HashMap, then
`addAllOptions` after `unique`, :223, :304, :343-352, DdlVisitor.java:333-353); the plain
ON-source form admits no OPTIONS clause (only the LEGACY_EXTREMUM_EVER attribute, which
changes the index type, not its options).

Landed: `recordlayer.Index` records the order keys are first set through `SetOption`,
`OptionKeys` emits that order and then any key written into `Options` directly, sorted,
and `indexToProto` writes `OptionKeys`, so stored bytes are a function of the calls,
never of Go's map iteration; `indexFromProto` sets the stored options in list order and
refuses a repeated key with `DuplicateIndexOptionError` (Guava's message; Go kept the
last value); `GetReplacedByIndexNames` walks the options in order, as Java's does
(:356-364); the constructors that seed an option (`NewPermutedMin/MaxIndex`,
`NewVectorIndex`) set it through `SetOption`. The DDL builder writes `unique` FIRST and
then the generator's options, so a permuted index stores `[unique, permutedSize]`.
Pinned: `TestIndexOptionOrder_*` (insertion order over eight keys, 50 builds each;
re-set keeps its slot; a deleted key drops out; direct writes sort last; constructor
options lead; a stored order survives a round trip; the duplicate refusal and its
message) and `TestIndexOptionOrder_PermutedIndexStoresUniqueFirst` (the DDL, 50
builds); the production-path pin asserts the permuted order through SQL. Measured: 0
oracle runs with a differing order (18 in v2), identical Go classes in two passes.
Mutations under Bazel: ranging the map again reddens five of the six record-layer and
DDL pins; writing `unique` last reddens the DDL pin; the first also reddens the
oracle's class pins, three runs (`evidence-mutations.txt`, v3).

Declared, not ported here: the vector clause options' order (Java stores its HashMap
iteration order after `unique`; Go stores them sorted after the dimension count and
stores no `unique`), owned with the rest of the vector DDL by WS-D/WS-K; the
record-layer programmatic `NewIndex` with a caller-supplied map (no order to preserve)
emits it sorted, where Java's programmatic Index copies the caller's map order.

## 4c. Records-file validation (F13): scope, what stops loading, and the migration

Landed locally, each arm Java's (RecordMetaDataBuilder.java:316-358, 635-747):
- the union is found only as `fetchUnionDescriptor` finds it (a message with
  `(record).usage = UNION`, else one named RecordTypeUnion; "Only one union descriptor is
  allowed", "Union descriptor is required"): `SetRecords` no longer takes the message
  named UnionDescriptor, Go's old default, and there is no union-less mode (the
  pre-upgrade `SetRecords` took that message or, when there was none, went union-less,
  where no record could be written, `serializeUnion` refusing a record type without a
  union field; there was no fallback chain in code: only the pre-upgrade loader tried
  UnionDescriptor, then RecordTypeUnion, then the usage=UNION message);
  `SetRecordsWithUnionName` runs the same search and refuses a name that is not the
  union it finds ("union message <n> is not the union descriptor of the records file
  (found <m>)"), because metadata built around another message is refused by this same
  binary when it is loaded back;
- `validateRecords` runs whenever a records file is set, which includes LOADING stored
  metadata (`RecordMetaDataFromProto`): data types first (unsigned kinds refused, at any
  depth), then the union's fields in field order (non-message, repeated, the relational
  union type, a non-RECORD usage), then every RECORD-usage message must be a union
  field; the builder records a fault instead of throwing (a setter has no error
  channel) and `Build` returns the FIRST recorded fault itself, as Java throws the
  first, never a join of all of them; the proto LOADER follows Java's `loadFromProto`
  order (RecordMetaDataBuilder.java:272-278): the subspace-key settings, then the
  records, whose first fault it returns before any index is read (v4 read the indexes
  first, so "unknown record type" could hide a records fault), then the indexes ONE AT A
  TIME as `loadProtoExceptRecords` reads them (:187-219): each index's record types are
  resolved BEFORE the index is read and the index is added before the next is read, so
  neither a repeated option or bad key expression on the same index nor one on a later
  index hides an unknown record type (v5 read every index first), and a duplicate index
  name is refused where Java refuses it; then the RecordType entries, one naming no
  record type refused (:221, via `getRecordType`, :986-990; v5 skipped it); an unknown
  type is refused with the target's message, "Unknown record type <name>"
  (`throwUnknownRecordType`, :994-996). Java also resolves an index's type among the
  SYNTHETIC (joined, unnested) record types; Go carries those verbatim without
  modelling them, so an index over one is refused as unknown (DIVERGENCES.md, "An index
  over a synthetic record type is refused on load"). The Go-only refusal of shape (d)
  below runs LAST in `Build`, after every check the target makes, so a file the target
  refuses is refused with the target's fault (`TestAmbiguousLegacyUnionRefusalComesAfter-
  TheTargetsFaults`, a primary-key fault and an index fault on both paths), and it runs
  whenever the union was FOUND rather than named: when stored metadata is loaded and when
  metadata is built in code with `SetRecords`. The "Records already set." guard, the union search
  and the name check live in ONE builder function, `setRecords`, which `SetRecords`,
  `SetRecordsWithUnionName` and the loader all call, and `SetRecordsWithUnionName`
  refuses an empty name, which names no union (v5 accepted it as `SetRecords`);
- a 32-bit unsigned field is read as protobuf-java's signed Integer by every Go reader
  (the key evaluator and index predicates, `key_expression.go`, `index_predicate.go`;
  the row reader `values.ProtoScalarKindToRowValue`; the driver reader
  `functions.ProtoValueToDriver`), typed INT as Type.java:909-914 types it (the value
  type map and the catalog's SQL type), and written from the INT range as its 32 bits
  (`functions.ConvertToProtoValue`, the executor's converter), so a value read back
  writes back to the same bytes; a 64-bit unsigned field is a LONG by the same rule.
  v3 converted only the key evaluator, so row values and key values disagreed above
  2^31; v4 converts the rest (`TestScalarProtoToGo_Uint32Kinds`,
  `TestConvertToProtoValue_Uint32_IntRange`, `TestRoundTrip_UnsignedKinds`).
MEASURED: the conformance spec "Java refuses the same records descriptors" gives each of
35 records files to both engines' LOADERS (`deserializeMetaData` on the JVM,
`RecordMetaDataFromProto` in Go) and compares the exception class and message. Eight load
in both: the valid file, the one whose union is found by the name RecordTypeUnion, a
valid file with an index, (v6) a record type named UnionDescriptor in the usage=UNION
union and a nested message named UnionDescriptor, the two shapes a SQL table or STRUCT of
that name with scalar columns produces, and (v8) a scalar UnionDescriptor only an
unreached message holds, a table named UnionDescriptor with a STRUCT-typed column, and
one with a table-typed column whose file also indexes it (the replayed pre-upgrade loader
refuses the last two, so nothing was framed). 22 are refused with the same message in
both, among
them the precedence pairs (a non-message field before a repeated one and the reverse,
an unsigned field before a union fault), the relational union as its own union field,
the file a pre-port Go build stored, whose union is named UnionDescriptor with no usage
option ("Union descriptor is required"), and the load order: an index on an unknown
record type ("Unknown record type Nope"), the same index beside a union fault (the union
fault), a subspace-key-counter fault beside a union fault (the counter fault), and (v6)
an unknown record type on an index that also repeats an option, one before an index that
repeats an option, and a RecordType entry naming no record type (each "Unknown record
type Nope"). Five load in the target and are refused by Go, the declared divergence:
shapes (d1) and (d2) below, (v7) a legacy UnionDescriptor held by an envelope message,
and (v8) one held by a record type and a table named UnionDescriptor with a table-typed
column. Run record `wsj8-vr-jvm.log` ("Ran 35 of 1560", all 35 passed; 8 load in both,
22 refused in both, 5 refused by Go only), in `ws-j-oracle/evidence-v8.txt`. The SQL path
is measured too, through the production paths: the spec "WS-J a table or struct named
UnionDescriptor" has the target create a template with `CREATE TABLE "UnionDescriptor"`
and one with `CREATE TYPE AS STRUCT "UnionDescriptor"` used as a column; Go's catalog
library loads the target's stored template (`LoadSchemaTemplate`), and the Go SQL driver
creates the same DDL, binds a schema to it and runs a query, both outcomes taken before
either is asserted (the WSJUD lines). The spec "WS-J a column typed by a table" (v8) has
the target create five templates with a table or STRUCT named UnionDescriptor holding a
message-typed column, asserts the target stores a message-typed field on it, and takes
Go's catalog load of the stored template and the Go driver's `CREATE SCHEMA TEMPLATE` of
the same DDL: a table-typed column in a table and in a STRUCT are refused by both with
0A000 wrapping the (d) error, and a STRUCT column, a UUID column, and a table-typed column
whose table is indexed load in both (the WSJTT lines).

What stops loading, and what a pre-release Go build wrote (v16, the owner's ruling of
2026-09-24; section 4d). The records file is read as the target reads it on every path: the
ordinary load, `SetRecords`, `SetRecordsWithUnionName`, both catalogs' `CreateTemplate` and the
SQL driver. A file the target refuses ((a) a union found only by the name UnionDescriptor, (b) no
union, (c) an unsigned field) is refused with the target's message, pinned above. Every Go-only
refusal of a framing shape is gone: shape (d) (a message named UnionDescriptor beside the union
the target finds), which v7 to v15 refused in some form, loads and is served as the target serves
it, and so do (f) and (g). Metadata or records that only a pre-release Go build wrote under its own
framing (name-first union, `_X` framed by name, the last union field as the implicit key) are
pre-release data, which the owner ruled unsupported: nothing detects, migrates or repairs them.
v7 to v15's migration, edited-file route, writer argument, relaxations R1 and R2, reservation rule
and store-open probe are withdrawn with it.

## 4d. v16: pre-release data, `SetRecords`'s extension options, and the deserialization port

v16 replaces v15's section 4d, whose framing criterion, probe and legacy routes the owner's ruling
removes. v15's text is kept at `/var/tmp/fdb-upgrade-recovery/ws-j-design.v15-final.md`.

**The owner's ruling (2026-09-24, umbrella RFC "Verification and review gates" item 9).** "We are
pre-release, so old releases of this Go stuff are not required to be supported." Decisions:
1. Go reads every records file as the target reads it, on every path, and refuses only what the
   target refuses (4c). The one Go-only refusal that had landed, shape (d), is deleted on this
   tree: `refuseAmbiguousLegacyUnion`, `AmbiguousLegacyUnionError`,
   `RefuseAmbiguousLegacyUnionAsStored`, the replay helpers (`legacyUnionRecordTypes`,
   `preUpgradeLoaderLoads`, `legacyUnionHolder`), the builder's `unionNamed` and
   `loadedFromProto`, the catalogs' store and load refusals, and the relational
   `LegacyUnionTemplateError` (0A000). Its tests now assert the target's outcome:
   `TestRecordsFileWithALegacyNamedUnionDescriptorLoadsAsTheTargetLoadsIt` (every shape, every
   path), `TestLegacyNamedUnionDescriptorGetsTheTargetsFaults`, the metadata store spec, and the
   JVM specs "Java refuses the same records descriptors" (its five Go-only refusals now load in
   both) and "WS-J a column typed by a table" (the target's stored template and Go's driver-built
   one both served). DIVERGENCES.md loses the shape (d) entry, the reservation rule's entry and
   v15's framing entry; the CHANGELOG says pre-release data is unsupported.
2. Withdrawn, none of it landed: 4c's migration (`MigrateLegacyRecordsFile`,
   `SaveMigratedLegacyMetaData`, `frl meta migrate-legacy`), the edited-file route
   (`SaveEditedLegacyMetaData`, its writer argument `PreUpgradeGo`/`Target`, relaxations R1 and
   R2, the framing comparison at (iii)), the reservation rule on every writer
   (`checkReservedNumbers`; Java's validator compares no reserved ranges, and without R1 no
   stored bytes sit under a reserved number), the route's and migration's writes of
   `RecordType.explicit_key`, the store-open probe (`LegacyRecordTypeKeyError`) with its detector
   (`frl meta legacy-framing`), v15's table of implicit keys per pre-upgrade framing, the
   rolling-upgrade and shared-store declarations, and the (d) refusal v15 kept for a new
   template's `CreateTemplate` (which refused DDL the target accepts). Section 9 loses (p), (r)
   and (y).

**`SetRecords` processes the target's extension options** (v15 Graefe M1). Java's
`setRecords(FileDescriptor)` builds with `processExtensionOptions` true; its loader builds stored
metadata with it false. With it true, Java reads:
- the file's schema options, `split_long_records` and `store_record_versions`
  (`RecordMetaDataBuilder.java:161-174`);
- per record type, `(record).since_version` and `(record).record_type_key`
  (`processRecordType`, :890-908), and refuses a second record type of one name ("There is
  already a record type named X");
- per field, `(field).index` or the deprecated `(field).indexed`, which adds an index named
  `Type$field` over the field (a RANK over the field ungrouped), and `(field).primary_key`, which
  sets the primary key and refuses a second ("Only one primary key per record type is allowed
  have: K; adding on f") and a repeated field ("Primary key cannot be set on a repeated field")
  (`protoFieldOptions`, :910-957).

Go reads none of them: a Go program over a records file that carries them builds other metadata
than a Java program over the same file, and, since the record type key is part of every key, keys
its records elsewhere. Step 3 ports all of them, with Java's texts, into `setRecords` (not the
loader), each pinned against the JVM's in-code build of the same file. What they change for Go's
own test protos is measured at the step (an index `Type$field` that a test also adds by hand is
Java's "Index ... already defined").

**The deserialization port (3.6), with v15's corrections.**
- Function names are loaded whether or not Go registers them (declared in DIVERGENCES.md). The
  arity of a function Go registers is checked as the target counts it: the ARGUMENT expression's
  column size (`FunctionKeyExpression.java:124`). A `FunctionKeyExpression`'s own column size is
  therefore its function's, a field of the registry entry, where Go returned 1 for every function;
  a loaded name Go does not register keeps 1 (declared). A builder-time arity refusal is the
  target's `RecordCoreException`, whose text is pinned against the JVM.
- `get_versionstamp_incarnation` is a key function in Go only: the target has it as a SQL function
  (`IncarnationValue.java:171`) and not in its key-function registry, so a Go index over it is
  metadata the target refuses to load ("Function not defined"). `Build` refuses a key function Go
  registers that the target's core registry lacks, so Go never stores metadata the target cannot
  load. The SQL path is measured at the step against the target's DDL for the same statement.
- Only a Nesting's child can be absent in stored bytes: Grouping, Dimensions, KeyWithValue, Split
  and Function children are proto2 `required` (`record_key_expression.proto:57, 62, 68, 73, 100`)
  and fail at parse in both engines. The absent-Nesting-child refusal ("Exactly one root must be
  specified for an index", Java's class) is pinned from stored bytes, the others from in-memory
  protos. "Invalid fan type N" cannot be produced in Java (the enum is closed), and its pin is
  withdrawn.
- 3.6's "Java's, whole" is narrowed. Go keeps four untyped load refusals of its own, listed from
  `key_expression_proto.go`'s `fmt.Errorf` returns: a key expression nested deeper than
  `maxKeyExpressionDepth`, a grouping's `grouped_count` outside `[0, columns]`, a
  key-with-value's `split_point` outside `[0, columns]`, and a split size below one; each is
  declared. The fifth untyped return, a Dimensions with no `whole_key`, is the absent-child case
  above and becomes Java's refusal.
- `NullStandin` is ported (v15 Graefe H2, which found v15's reading of NULL_UNIQUE backwards). The
  target's three: NULL, a null that a unique index ignores; NULL_UNIQUE, a null that a unique
  index does NOT ignore, so two nulls collide (`Key.java:109-111`); NOT_NULL, an unset field
  evaluated as its type's default (`Key.java:397`), and an unset parent message as the parent's
  default message (`NestingKeyExpression.java:77-87` over the parent field). When the message itself
  is null (an absent nested parent), NOT_NULL yields null, not the default
  (`FieldKeyExpression.java:80-89, :229-239`; the target's issue #4141), and that is ported as the
  target has it. An absent field is protobuf-java's `hasField` false, so an implicit-presence
  (proto3) field at its default is absent (`FieldKeyExpression.java:205`); Go read its value.
  - Where the standin is read. The target reads it off the evaluated value
    (`IndexEntry.java:64-79`); Go evaluates every standin to a plain nil, so
    `nonUniqueNullColumns` reads it off the leaf that produced each key column, which is exact
    because each leaf's nulls are of one kind: a field's null is only its `getNullResult` (a
    nullable tuple-field message converts to a primitive, `TupleFieldsHelper.java:110-160`), a
    nesting's is its child's, `Literal(null)`, a version-less record and a null record are
    `Key.Evaluated.NULL`, list and split columns never hold one, and a function's is NULL only for
    `collate_*` and `cardinality` (`CollateFunctionKeyExpression.java:169`,
    `CardinalityFunctionKeyExpression.java:148`): the arithmetic functions return
    `Key.Evaluated.scalar(null)` (`LongArithmethicFunctionKeyExpression.java:98`), a null that
    collides. A Go function whose Java twin returns `Key.Evaluated.NULL` registers with
    `RegisterNonUniqueNullFunction`. `keyContainsNonUniqueNull` replaces `indexKeyContainsNull` at
    the unique checks (`index_maintainer.go`, `rank_index_maintainer.go`) and COUNT_NOT_NULL's
    grouped columns (`evaluateGroupingKeysNotNull`, `AtomicMutation.java:165-171`).
  - `IndexMaintenanceFilter.NO_NULLS` (`IndexMaintenanceFilter.java:45-56`) is the target's third
    reader, and Go has no host for it: Go has no `IndexMaintenanceFilter` at all (the store
    builder option, `IndexMaintenanceUtils.getFilterTypeForRecord`, and its readers in
    `StandardIndexMaintainer`, `VectorIndexMaintainer`, `SlidingWindowIndexMaintainer` and the
    scrubber). `keyContainsNonUniqueNull` is the predicate NO_NULLS needs; the filter is a Java
    store API Go lacks, found here and raised with the owner (TODO.md, the WS-J NullStandin
    block).
  - Correction to v16: `keyExpressionEquals` does NOT compare the standin, because the target's
    `FieldKeyExpression.equals` does not (`FieldKeyExpression.java:406-410`) and
    `NestingKeyExpression.equals` compares the parent with it. An index whose field changes only its
    null interpretation is the same index to the evolution validator in both engines (measured:
    `validateMetaDataEvolutionAnyVerdict` accepts it). v16's sentence was written without reading
    `equals`.
  - The same `hasField` rule is the query side's (`MessageHelpers.getFieldOnMessage`,
    `MessageHelpers.java:124-142`: a repeated field reads its list, a singular one when set or
    declaring an explicit default). Go's three readers disagreed with it and with the index: the
    Cascades `FieldValue` ordinal and by-name reads gated on `HasPresence` (a proto3 field at its
    default read as its value), and the executor's record-to-row layer and the by-name read read an
    unset proto2 field's explicit default as null. `values.ProtoFieldReadsValue` is the rule, used by
    all three.
  - Pins (each run red on the WS-C r11 tree, green here, except the three controls the change does
    not move): conformance "RFC-257 NullStandin" (14 cases, each standin over an unset field,
    an absent nested parent both ways, Concatenate under an absent parent, a Then, a function's
    plain null, a unique RANK index, COUNT_NOT_NULL over NULL and NULL_UNIQUE, proto3 at default;
    the save verdicts and every index key-value byte equal Java's, and Java's verdicts pinned; plus
    the evolution case); conformance "a query reads a field as Java's getFieldOnMessage" (11 cases
    against `MessageHelpers.getFieldOnMessage` on the JVM); Go unit pins
    `TestNullStandin_*`, `keyContainsNonUniqueNull` (every arm of `nonUniqueNullColumns`), and
    `TestProtoFieldByNameReadsAsGetFieldOnMessage`.

**Corrections.**
- 9 (x) stated that Java admits an inverted history and serves the tenant. It does not: the
  target's `repairSchema` loads the schema's template (`RecordLayerStoreCatalog.java:272-279`),
  which fails on a version that is gone. Both engines refuse it, and a history an earlier Go build
  inverted is pre-release data. (x) is rewritten.
- 9 (z) keeps its conflict argument (an unbounded reverse scan read once) and loses its sentence
  on `LegacyRecordTypeKeyError`.
- The `DdlVisitor` citation for the template's build is `:263, :281, :319`; `:223-224` is
  nullability code.
- "Go refuses on load only what the target also refuses" holds with 9 (u)'s exception (an index
  over a synthetic record type), which the sentence now names.

## 5. Enum DDL (F6, F10)

SOURCE: `DdlVisitor.visitEnumDefinition` (:480-490): values numbered 0..n-1 in
declaration order, names via `normalizeStringLiteral`; the enum arm runs inside the
clause loop (:519-521) and registers an auxiliary type. Emission is NOT registration
order: `FileDescriptorSerializer` emits, per table in visit order, the types that
table's closure reaches, from a per-table TreeSet (:118-140), so an enum no table
references is never stored, an enum used by a table that moves (section 4) is emitted
with that table, and two enums sharing a value name are distinct enum types with their
own value lists.

MEASURED: `enum_value_index`, `enum_on_moved_table`, `enums_sharing_value_name` and
`enum_unreferenced` are all accepted by the target (their full stored MetaData is under
the pin, so emission order and the absence of the unreferenced enum are pinned too); Go
rejects every enum template, 22 runs.

Change: `rejectUnsupportedTemplateClauses` stops rejecting enum clauses, the front end
registers the enum type, and the builder emits `EnumDescriptorProto`s by Java's
emission rule above (not in registration order). Go already reads Java-created enum
descriptors and rebinds enum numbers (WS-A). Acceptance: the enum rows become byte-equal
under the canonical comparison, and an FDB test round-trips an enum column written by Go
and read by Java and back.

The index-predicate side of #4624 (F10). MEASURED: `CREATE INDEX ix AS SELECT id FROM t
WHERE m = 'HAPPY' ORDER BY id` over an enum column fails in the target with XXXXX
RecordCoreException "attempt to create PoJo index comparison from unsupported
comparison": the target converts the enum comparison into an index predicate and has no
PoJo form for it. After F6, Go rejects the same shape at the same point with the same
message and its internal-error code XX000 (the target's XXXXX is unmapped; shared
SQLSTATE class, shared message), pinned by a Go test; when the target fixes it, the
oracle row moves and this section is revisited.

## 6. Bit and bitmap operator lanes (F4) — query engine

SOURCE: `ArithmeticValue.java` defines BITAND/BITOR/BITXOR and
BITMAP_BUCKET_OFFSET/BITMAP_BIT_POSITION as LogicalOperators with physical operators per
(left, right) type: `_II` → INT, `_LI`/`_IL`/`_LL` → LONG for the bit operators
(:500-513); the bitmap functions have ONLY `_LI` and `_II` (:515-522), with
`multiplyExact`/`subtractExact` in the operand width. `encapsulate` (:213-231) refuses in
TWO steps, in order: an operand whose type is not primitive (ENUM, RECORD, UUID, ARRAY,
Type.java:786-789) is a SemanticException ARGUMENT_TO_ARITHMETIC_OPERATOR_IS_OF_COMPLEX_TYPE
(:217, :220); then a missing physical operator for the (left, right) type codes is the
VerifyException "unable to encapsulate arithmetic operation due to type mismatch(es)"
(:226-229).
BITMAP_BUCKET_NUMBER's lanes (:518-519) are not reachable from SQL: the target's
function catalog has no entry for it and answers 0AF00 "Unsupported operator
bitmap_bucket_number" (measured).

MEASURED (24 pinned probes): over an INTEGER column the target returns INTEGER for all
five operators and raises int overflow at INT_MIN for the bitmap functions; a LONG or
LONG-literal operand gives BIGINT; a DOUBLE or STRING operand is XX000 VerifyException
"unable to encapsulate arithmetic operation due to type mismatch(es)" for `&` and for
`bitmap_bucket_offset`; an ENUM, UUID or ARRAY operand of `&`, and an ENUM argument of
`bitmap_bucket_offset`, is XX000 SemanticException "The argument to an arithmetic
operator expecting an argument of a primitive type, is invoked with an argument of a
complex type, e.g. an array or a record." (the first step); an untyped `NULL & 1` passes
the first step and fails the second (VerifyException), while `CAST(NULL AS BIGINT) & 1`
has a lane and returns a BIGINT NULL; `%` over INT is INTEGER in both engines. Go returns
BIGINT, the int64 values -2147490000 and 6352 at INT_MIN, and NULL for the DOUBLE and
STRING operands; the ENUM, UUID and ARRAY probes run on a schema Go cannot create yet
(F6), so their Go lines move with section 5.

Go models these as ScalarFunctionValues with a fixed `NullableLong` result
(`scalar_function_catalog.go:397-410`), and its generic `ArithmeticValue.Type()`
promotes (values.go:3642-3659), so even moved onto ArithmeticValue a DOUBLE operand
would be typed DOUBLE. Change: they become ArithmeticValue ops (`OpBitAnd`, `OpBitOr`,
`OpBitXor`, `OpBitmapBucketOffset`, `OpBitmapBitPosition`) whose PHYSICAL LANE is
resolved ONCE, at construction, from the operand types by Java's two steps, exactly as
`encapsulate` does: first a non-primitive operand (ENUM, RECORD, UUID, ARRAY) is refused
with XX000 and the SemanticException message above; then the lane table fixes the
result type (II → INT, else LONG) and the arithmetic (int32
`multiplyExact`/`subtractExact` on the bitmap `_II` lane), and an operand type with no
lane (DOUBLE, FLOAT, STRING, BOOLEAN, BYTES, an untyped NULL) is refused with XX000 and
the VerifyException message, never typed by promotion. A test drives every row of the
refusal table (each non-primitive type code, each lane-less primitive, NULL untyped and
cast) through both steps and asserts which step answers. The same
construction-time lane resolution serves WS-E's untyped-NULL arithmetic row
(`arith_untyped_null_param`). The second argument of the bitmap functions is always the
injected INT entry size, so no other lane is reachable from SQL; STORED metadata can
hold one (a pre-F2 `long_value` entry size), and section 3.5 decides that case (Go
declines the candidate; the target fails every query of the table). `bitmap_bucket_number`
keeps its 0AF00 with the target's lower-case operator name. Java's key-expression naming
(`getLogicalOperator().name().toLowerCase()`) then falls out of the same
`arithmeticFunctionName` path the other arithmetic ops use, and `bitFunctionName`
disappears. Explain/plan-hash/cache identity follow ArithmeticValue's; bitmap index
matching is re-verified by the existing bitmap tests, a plan-harness pin, and the Go-
stored spec's EXPLAIN rows (the target's plans over a Go-stored template, pinned).

The XXXXX-vs-22003 SQLSTATE difference for overflow is the one WS-E records in
DIVERGENCES (the target leaks an unmapped ArithmeticException); Go keeps 22003 and the
VALUE behaviour becomes the target's (an error, not a widened number).

## 7. Acceptance

The oracle is the acceptance test, and it pins Go as well as the target: every run's
Go class AND Go's own outcome are in `wsj_go_classes.json`, so each change below lands
with the pins it moves, class or digest, reviewed in the same diff.

1. Every both-accept run (corpus, derived, isolation, shape) becomes `both-accept-equal`,
   or `both-accept-companion` where a verified RFC-209 companion is its only difference
   (index protos equal on every field, versions included, and the canonical MetaData
   equal once the companions are removed and their version slots given back; section
   0's canonicalization only).
2. Every java-accept-go-reject run not owned elsewhere (section 1) becomes both-accept-
   equal or the same rejection: unnest and derived sources (21 runs + 4 shapes), enums
   (22 runs + 4 shapes), and the enum-predicate rejection (F10).
3. Rows owned elsewhere stay recorded with their owner named in the spec.
4. The query half's Go lines equal the target's except the declared SQLSTATE; the
   non-integer spec's Go pins stay the record-served answers for the DOUBLE and FLOAT
   operands (3.2) and become the index-served answers of the INT_MAX probe (3.5 serves
   `i + 1 = 6` from the index, [[2]]); the nested-leaf value plan oracle's Go pins
   become index plans (3.5); Go over the Go-stored `long_value` variant answers the four
   T1 reads the target fails (3.5's decline, asserted in the plan harness).
5. The Go-stored spec stays green, and gains the carry rule's v1 → v2 pair: a template
   stored by Go with the old numbering and a v2 built by the port, both planned by the
   target over the same rows.
6. The production-path pin extends to every class of change as it lands (the enum
   column, an unnest index, a carried v2), so the production builder, not only the
   tooling builder, is pinned against the target.

Unit tests next to each change pin Java's measured roots verbatim (as 3.1 and 3.2 do),
so the fast suite carries the contract without a JVM; mutation evidence for every new
guard, run under Bazel; `just test`, race, the plan harness for sections 3.3b, 3.5 and
6, and the 1M stress comparison for 3.5.

## 8. Order

THE MERGE UNIT is the landed fixes (3.1, 3.2, 4b, 4c's validation and its load-order
fix, with their production-path pins) together with steps 1 to 3 below, and nothing
smaller: those three steps close every interim hazard the landed fixes open (F1's
corrected roots and 3.2's carriers rebound without the carry rule, a re-issued
version, F13's refusal without its migration), so no commit that carries a landed fix
without steps 1 to 3 is a release candidate, which is what keeps the interim states
named in 3.1, 3.2 and 4c off master. Steps 4 to 9 open no interim hazard of their own
(each changes behaviour only where it lands, behind its own tests), so they need not land
WITH the unit's commits; four of them are query-engine milestones with their own Graefe
gates. (v8 said each "merges after the unit on its own gate", which contradicts the
next sentences: nothing merges separately, the tree merges once, and "after the unit"
orders the WORK and its gates, not merges.) The unit's gate is recorded where the person merging reads
it: the umbrella RFC's "Verification and review gates" (item 8) and a TODO.md block;
this working tree also holds WS-C to WS-F, whose files overlap WS-J's (`executor.go`,
`values.go`, `sql_plan_steps.java`, CHANGELOG, DIVERGENCES, TODO), and no mechanism
takes the WS-J hunks out of it, so THE TREE MERGES AS ONE: the RFC-257 upgrade is one
merge, gated on every workstream's ACK, the unit's among them, and nothing of it
reaches master before then. Within the unit and after it:
1. 2's version guard (keyed on (name, version), read from (t, latest + 1), or the whole
   `(t)` prefix for a fresh t, in both catalogs, the in-memory one with `SaveSchema` and
   `RepairSchema` holding the store mutex across their template reads, and its `-race`
   interleaving tests), the gone-version refusal, AND the two exits from the state the
   refusal creates, the `DeleteTemplateVersion` refusal and `fleet.RestoreTemplateVersion`
   with its header check, its in-transaction binding read and its carry-compatibility
   check against every stored version of t (the evolution validator with
   `SetDisallowTypeRenames` and the symmetric carrier arm, every index field, and the
   relational validator): the guard is what makes F1's
   corrected roots and 3.2's carriers safe against a re-issued version, so it comes
   first, and without the delete refusal the gone-version refusal would add a Go-only
   way to strand a schema (today a schema whose bound version was deleted is still
   repairable, because `validateSchemaRebind` returns nil, `fdb_store_catalog.go:
   311-313`), with its FDB tests.
2. 3.4 unify the front ends (no behaviour change; oracle unchanged).
3. 4 metadata order, companions last, and the carry rule (the EQUIVALENT definition over
   the raw catalog bytes, the re-added-name refusal (the former-index-key arm it relies on
   is ported already, WS-C 7.7), `allowIndexRebuilds`) with its FDB tests, the widening arm's end-to-end test 5 among
   them (the 184 F3 runs become equal or companion runs); the no-lane refusal on the build
   path (both catalogs' `CreateTemplate`, over NEW and CHANGED indexes; a WIDENED index is
   carried with its bytes and not re-checked, since no widening removes a lane, and the
   lane-table property that makes that true is pinned instead, 3.2) with its lane table,
   and the carried route itself (inside `CreateTemplate` in both catalogs, first the
   exact-duplicate refusal with DUPLICATE_SCHEMA_TEMPLATE, then the `v′ <= latest` refusal
   and the relational validator moved there from the save action, then the carry, with its
   unexported writer, `LoadTemplateProto`, `fleet.SaveTemplate` returning the stored
   template, and a (d) refusal of the carried bytes marked as a template being stored), since `allowIndexRebuilds` admits a lane-less or reverse-moved key as
   CHANGED from this step on, AND the DDL-origin refusal at the clause with the target's
   outcome: the key generator's ArithmeticValue and bit-operator arms consult the same lane
   table and raise the target's XX000 (VerifyException "unable to encapsulate arithmetic
   operation due to type mismatch(es)" for a primitive with no row, SemanticException for
   a non-primitive operand), so from this step no DDL shape reaches the build-path 42F59,
   and the WSJLANE spec's `goNow` becomes the target's outcome here, not at step 6 (v8 left
   the eight primitive shapes on Go's 42F59 until step 6, an interim divergence; step 6
   then moves the lane resolution into the query engine's Value construction, which the
   generator reads, keeping one table). And (v16) every decision of section 4d: the target's
   extension options in `SetRecords`, and the key-expression deserialization of section 3.6
   with `NullStandin`, function-key arity and column size, and the refusal of a key function
   the target's registry lacks, each with its FDB and JVM pins. The (d) refusal's removal has
   landed (4d); v7 to v15's migration, edited-file route, reservation rule and probe are
   withdrawn.
4. 2 existence policies and the CREATE SCHEMA order (FDB tests and the cross-engine
   precedence spec).
5. 5 enum DDL and the F10 rejection.
6. 6 bit/bitmap lanes (query engine; plan harness + query half), BEFORE the IndexSpec
   port, whose result check reads ArithmeticValue for the bit and bitmap operators.
7. 3.3 IndexSpec and QuantifierValues on the translated graph (after 6).
8. 3.5 value-index expansion, with the stored-key decline (query engine; after 6).
9. 3.3b nested leaf groupings on aggregate indexes (query engine; after 3.5; flips the
   guard's leaf arm).

## 9. Declared divergences and kept extensions

- (a) `record_types` order, anonymous type names and anonymous-type relative order are
  not reproduced (section 4).
- (b) RFC-209 companions stay; they sort after all declared indexes; the metadata version
  exceeds Java's by their count.
- (c) The rebind validator on writes over an existing row stays (Go extension; Java's
  relational catalog runs none), with its unreachable name and version arms deleted and
  its options decided (section 4).
- (d) New template versions from DDL (Go extension) carry the stored numbering; Java has
  no such path.
- (e) A literal whose carrier changed and whose value did not is an unchanged index on
  the relational rebind path only (3.2); the core validator keeps Java's refusal.
- (f) Overflow SQLSTATE 22003 vs the target's unmapped XXXXX (shared with WS-E); the
  enum-predicate rejection's XX000 vs XXXXX (F10).
- (g) COPY: no Go route (WS-K accounting); it imports stored bytes and is unaffected by
  section 4.
- (h) F11, the Go SQL driver's keyspace, is not a WS-J divergence to declare but a wire
  gap put to the owner (TODO.md "Go SQL driver stores the relational catalog and user
  schemas on a Go-only keyspace").
- (i) A long-arithmetic index over a non-integer operand is never a match in Go; the
  target serves it and returns wrong rows (3.2, 3.5, DIVERGENCES.md).
- (j) Saving a template version of t is refused while a schema binds t at a version above
  the latest one stored (a fresh CREATE SCHEMA TEMPLATE t is refused while any dropped
  version of t is still bound); the target accepts it and silently rebinds those schemas
  (section 2).
- (k) The vector clause options' stored order and the vector index's `unique` option are
  WS-D/WS-K's (4b).
- (l) The ported evolution validator's messages carry names and versions inline where
  Java carries log keys (section 4).
- (m) A stored function key with no lane (a pre-F2 `long_value` bitmap entry size): Go
  declines that index as a candidate and answers the query; the target fails every
  query of the table (3.5, DIVERGENCES.md, four pinned target rows). A stored template
  holding one of the eight no-lane DDL shapes Go stores today (a bit or bitmap key over
  a BOOLEAN, DOUBLE, FLOAT or STRING operand) keeps the index through a hand-built new
  version (carried as EQUIVALENT when the builder repeats the stored `long_value`
  literals, as WIDENED at this build's width, and in neither class refused, since
  widening never gives such a key a lane it lacked), while a new version made by DDL
  re-states it and is refused at the clause from step 3; the index goes when a version
  omits it (3.2).
- (n) An index-served read over a long-arithmetic key at the INT lane's edge answers
  where the same read over the record fails with the overflow: true of the target
  (measured) and of Go once 3.5 lands, recorded beside (i) (3.2).
- (o) A new template version may not re-add under its old name an index a carried
  FormerIndex holds (section 4); Java's DDL creates no second version to face it.
- (p) [withdrawn in v16, 4d: the migration and the edited-file route.]
- (q) A Go-generated message argument of a long-arithmetic key logs its proto full name
  where Java logs its generated class, which the Go copy of the descriptor cannot name
  (3.2).
- (r) [withdrawn in v16, 4d: Go reads shape (d) as the target reads it, on every path.]
- (s) `DeleteTemplateVersion` is refused while a schema binds the version (section 2), in
  both catalogs;
  the Go-only API has no Java counterpart, and DROP SCHEMA TEMPLATE keeps the target's
  behaviour. `RestoreTemplateVersion` reads headers through the Go keyspace the caller
  names, so over schemas Java created it refuses (no header), failing closed until F11.
- (t) An ArithmeticValue-family function key with no lane, in metadata built by HAND, is
  refused on the build path with 42F59 (3.2, in both catalogs' `CreateTemplate`, the one
  BUILD-PATH write entry, over the NEW and CHANGED indexes); Java's programmatic API
  stores it and its queries fail later.
  From DDL, once step 3 lands, both engines refuse a lane-less KEY at the clause with
  the target's XX000 and message, for the ten measured single-fault key shapes and the
  two-fault precedence shape (on this tree Go's DDL stores eight of the ten, pinned as
  `goNow`). Until section 6 (step 6) moves the lane resolution into Value construction,
  two cases keep Go's outcome: a no-lane operator in an index's WHERE, which the
  generator's key arms never see, and the precedence of a lane-less key against a fault
  the target raises while TRANSLATING the index query (a WHERE fault, the top-node
  check), which in Go come before key generation; both are interim, and step 6 closes
  them. An operand with no Java Value (a Go-only function key) has no lane type, and
  the check fails closed: it refuses the key with 42F59 on the build path. Operands are
  typed by their Values' result types (an order-wrapped or collated operand is BYTES, a
  CARDINALITY INT). The lane check runs over the indexes a save defines; an EQUIVALENT
  index carried as stored is not re-checked (3.2). The raw-write routes (the restore,
  the F11 copy) store such bytes unchanged.
- (u) An index over a SYNTHETIC (joined or unnested) record type is refused on load as an
  unknown record type; Java loads it (4c; synthetic types are out of the port's scope and
  carried verbatim; DIVERGENCES.md "An index over a synthetic record type is refused on
  load", which also records that the pre-upgrade loader refused it the same way).
- (v) LIMIT and OFFSET, an IN over a nested SELECT and a NULL in an IN list, anywhere in
  a template statement, are refused where the target refuses them, at the whole template
  statement in tree order (3.3); LIMIT stays the approved Go extension for queries (the
  approved IN-subquery extension is not present at the base; Go refuses it everywhere).
- (w) `CreateTemplate`, a library API of both catalogs, refuses more than Java's
  `createTemplate`, which refuses only an exact duplicate (DUPLICATE_SCHEMA_TEMPLATE,
  `RecordLayerStoreSchemaTemplateCatalog.java:229-245`). Go refuses that duplicate first,
  with the same code, and then a version at or below the latest stored (INVALID_
  SCHEMA_TEMPLATE) and runs the relational validator against the stored latest (moved
  from the save action, section 4), and it runs the version guard (section 2). A Java library caller can store a version below the latest; a Go one
  cannot.
- (x) The restore's own refusals have no Java counterpart (Java has no restore). A template
  history whose versions invert (a lower template version with a higher metadata version) is
  refused. The schemas bound to such a history bind a (t, v) that is not stored, so neither
  engine can repair them: the target's `repairSchema` loads the bound template
  (`RecordLayerStoreCatalog.java:272-279`) and fails on the gone version, as Go's gone-version
  refusal does (section 2); v15 said the target admits and serves them, which it does not. An
  unbound inverted version can be removed with `DeleteTemplateVersion`. Check (iv) refuses any
  enum value added between two histories (`EnumType.Equal` compares the whole list), which
  strands a one-history restore across such an addition. A history an earlier Go build inverted
  is pre-release data (4d).
- (y) [withdrawn in v16, 4d: the probe and the legacy framings' residual.]
- (z) Concurrent template writes of one t (v14; storage v13 L4). Two `CreateTemplate`
  calls carrying new versions from the same latest, or a new version racing a restore,
  serialize through FDB. Each reads the stored latest through `LoadSchemaTemplate`'s
  reverse range scan of `(t)`, unbounded and consumed for its first record
  (`fdb_template_catalog.go:68-78`), which adds a read-conflict range over what it read, from
  the key it returns to the end of `(t)`, covering every higher version. The other's write of `(t, v′)` lands in that range, so the second commit
  fails with 1020 and its retry reads the new latest. An FDB test commits one
  `CreateTemplate` of (t, 3) between the other's read of latest 2 and its commit of
  (t, 3), and asserts the conflict, and then that the retry is refused with
  DUPLICATE_SCHEMA_TEMPLATE, the exact-duplicate refusal `CreateTemplate` runs first. A
  second test races (t, 4) against (t, 3) from latest 2, and asserts the loser's retry is
  refused with INVALID_SCHEMA_TEMPLATE when it is (t, 3) and admitted when it is (t, 4).

