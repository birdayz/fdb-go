# Changelog

All notable changes to `fdb-record-layer-go` are recorded here. Format:
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/); versioning per `RELEASE.md`
(pre-1.0 `v0.MINOR.PATCH`).

**This project is pre-1.0.** The **Go API may change across minor versions**; the **FDB wire
format must stay compatible with each release's declared Java `fdb-record-layer-core` target** (the
shared-cluster hard line — see `RELEASE.md`). Every entry's **Compatibility** block answers the four
questions a user upgrading between two refs needs: wire format, SQL behaviour, FDB client option
semantics, and required dependency versions.

This changelog starts **2026-06-20**; earlier history is in `git log`. The first tagged release is
**v0.1.0** (2026-08-26). The `frl` CLI ships from a parallel nested-module tag, `cmd/frl/v0.1.0` —
that form is what makes `go install fdb.dev/cmd/frl@vX.Y.Z` resolve the same build the release
assets carry (`RELEASE.md` §Versioning).

## [Unreleased]

### Compatibility
- **Wire format:** the Java target is being upgraded under RFC-257. Pins/protos are updated,
  but metadata/vector compatibility failures and required queue/format/plan ports remain open.
  This unreleased upgrade is not certified compatible and must not be deployed.
- **SQL behaviour:** target Java changes structured promotion and other shared contracts;
  restored success regressions currently fail in Go. RFC-257 records the full audit and ports.
- **FDB client option semantics:** unchanged since v0.1.0; the honored / `UnsupportedOptionError` /
  safe-no-op classification in `pkg/fdbgo/fdb/OPTIONS.md` still holds against `libfdb_c` 7.3.77.
- **Data written by v0.1.0 or any other earlier Go build is not supported.** This build reads every records file,
  key expression, index option and stored template as Java 4.14.2.0 reads it. Meta-data or index
  entries that only an earlier Go build wrote, under a meaning of its own, may be refused or read
  differently, and nothing migrates, detects or repairs them: recreate such stores. Java-written
  stores are the compatibility line (RFC-257, "Verification and review gates" item 9).
- **Required versions:** Java `fdb-record-layer-core` **4.14.2.0**, FDB C++ client **7.3.77**, Go
  **1.26.x** (the `MODULE.bazel` / `go.mod` pins; the CI doc-guard enforces docs match them).

### Changed
- **Records descriptors are validated as Java validates them** (`RecordMetaDataBuilder.validateRecords`
  and `fetchUnionDescriptor`, ported in `pkg/recordlayer/metadata_validate_records.go`). A field of
  unsigned type (`uint32`, `uint64`, `fixed32`, `fixed64`) anywhere in a record type's reachable
  messages is refused with Java's `MetaDataException` message, so Go no longer writes index or
  primary-key bytes for such fields (Go read `uint32` values unsigned where protobuf-java hands Java a
  signed `Integer`, so the two engines' bytes differed for values >= 2^31). The union is found as Java
  finds it: the one message with `(record).usage = UNION`, else the one named `RecordTypeUnion`; a
  message merely named `UnionDescriptor` is no longer a union by name, two candidates are an error,
  and a file with no union is refused ("Union descriptor is required") instead of turning every
  top-level message into a record type. Repeated union fields, NESTED-usage union fields and
  RECORD-usage messages missing from the union are refused as in Java, and the FIRST fault is the
  error (Java throws it), no longer a join of every fault. `SetRecordsWithUnionName` finds the union
  the same way and refuses a name that is not it. Each message is pinned against the JVM (conformance
  spec "Java refuses the same records descriptors", precedence included).
  **This is checked when stored metadata is LOADED too**, so stored metadata whose records file
  Java refuses is refused: one that (a) finds its union only by the name `UnionDescriptor` (Go's
  old default, no `(record).usage = UNION` option), (b) has no union (Go's
  `SetRecordsWithUnionName` over an unannotated message of any other name), or (c) declares an
  unsigned field. A records file with a message named `UnionDescriptor` beside the union the target
  finds is read as the target reads it, on every path, SQL templates included: data an earlier Go
  build framed by that message is pre-release data, which is not supported (Compatibility above).
  SQL DDL never produces (a) to (c) (the relational layer names its union `RecordTypeUnion` and has
  no unsigned types); record-layer library users with hand-written records files can, and Java
  refuses them too. Loading also refuses, as Java does, an index or a record-type entry naming an unknown
  record type before any other fault of that index, and `SetRecordsWithUnionName` refuses an empty
  name.
- **`OnlineIndexer.BuildIndex` resolves the session against the index state as Java does**
  (`IndexingPolicy` `IfDisabled` / `IfWriteOnly` / `IfReadable`, Java's `DesiredAction`; zero values
  are Java's defaults). By default a session over a **READABLE index now leaves it alone** and returns
  0 records, where Go cleared and rebuilt it; set `IfReadable: DesiredActionRebuild` to rebuild. A
  READABLE_UNIQUE_PENDING index is published without a build when publication is enabled (it used to
  be rebuilt): the publication fails with the uniqueness violation while violations remain, and with
  `SetMarkReadable(false)` the session does nothing at all. A follower whose state differs from the
  primary's is refused unless its own action is REBUILD on a fresh session, and `DesiredActionError`
  refuses the state before any write. A session that skips, publishes or refuses is no longer
  stopped by another session's live heartbeat, unless the stored metadata is out of date (the
  store's open then reconciles it, which still requires that no other session is live) or a
  heartbeat key is in the legacy or a malformed form (still refused); a session that clears or
  freshly marks the index is still stopped by one, mutual or not. `OnlineIndexer.LastBuildOutcome`
  (Go-only) reports what the last `BuildIndex` call did over all of its attempts: built (including
  a call whose earlier attempt indexed records before a peer published), left a READABLE index
  alone, published without a build, or found a mutual build completed by its peers; after a failed
  call it reports nothing, whatever an earlier call did. `frl index build` uses it and on
  a READABLE index now says it is already readable and builds nothing (`frl index rebuild` rebuilds
  it); a fleet build no longer reports a tenant whose pending index a peer published first as built.
  The old behaviour also made concurrent mutual builders flaky: a builder starting after a peer
  had published cleared the index under every builder still to publish, which then failed with
  `IndexNotBuiltError` (RFC-257 WS-C design section 7).
- **`OnlineIndexer.BuildIndex` recovers from a build begun by another method, as Java's
  `indexingCatcher` does.** `IndexingPolicy.IfMismatchPrevious` (default CONTINUE) and
  `ForbidRecordScan` are new. Under CONTINUE, a single-target build that finds the index partly
  built by BY_RECORDS continues it by a records scan, one partly built MULTI_TARGET continues it as a
  single target where the saved stamp allows (nothing scanned yet, or `TakeoverMultiTargetToSingle`),
  and one partly built from a source index continues from that source; one partly built MUTUAL is
  continued only where `TakeoverMutualToSingle` allows it (the takeover set is empty by default) and
  otherwise returns the `PartlyBuiltError` at once, as Java does (`OnlineIndexer.java:212-213`), as
  does a multi-target build. A blocked BY_RECORDS, MULTI_TARGET or BY_INDEX stamp, and a MULTI_TARGET
  one the takeover rules refuse, are relaunched up to Java's attempt limit (six sessions), each
  meeting the same refusal, and then return the `PartlyBuiltError`, as Java does. REBUILD
  rebuilds by the requested method instead, and ERROR returns the `PartlyBuiltError` at once. The
  error now carries the `Saved` and `Expected` stamps. A BY_INDEX source index that cannot be used
  (not a VALUE index, creating duplicates, or on another record type) is no longer refused by
  `OnlineIndexerBuilder.Build`: it is checked when the build runs, as Java checks it, after the
  session's state transaction has committed, so the target is then WRITE_ONLY under a BY_INDEX stamp
  (and cleared, if its action was REBUILD); the build falls back to a records scan, or returns an
  `IndexingValidationError` with Java's message under `ForbidRecordScan`, leaving the target in that
  state for the next session. A source index the metadata does not define is a `MetaDataError`
  before anything is written, and `Build` refuses a source index with several targets (counted
  before duplicates are removed, as Java counts them) or a mutual policy with Java's
  `IndexingValidationError` messages, before it checks the targets and record types; `Build` also
  refuses a target that is not the metadata's own index object (one that only shares its name), and
  an empty target list, each with Java's `MetaDataError` ("Index <name> not contained within
  specified metadata", "index must be set"), where it used to accept any object of a known name and
  build under that object's subspace key. Duplicate targets are removed by Java's `Index.equals`,
  the first kept: the name, the type as spelled (`min_ever` is not `min_ever_long`), the root by
  `KeyExpression.equals` (a Dimensions or cardinality root included), the subspace key under Java's
  normalization (an `int` key equals an `int64` one, and two byte arrays equal by content), both
  versions, the primary-key component positions, the options, and the predicate (the same object,
  or two row-number windows with equal fields). The targets are sorted by name before the check, so
  a foreign object listed beside the metadata's own is refused in either order and the refusal names
  the alphabetically first. `SetTargetIndexes` copies the list it is given, as Java's does.
  `SetIndex` follows Java's `setIndex`: after a target is already set it is an
  `IndexingValidationError` ("setIndex may not be used when other target indexes are already set",
  returned by `Build`), `SetIndex(nil)` is skipped (so `Build` reports "index must be set" instead of
  panicking, as does a nil `AddTargetIndex`, after the source-index checks, where Java's
  NullPointerException falls), and `SetIndex` followed by `AddTargetIndex` builds both targets,
  where Go refused that combination. A BY_INDEX session stamps and validates the metadata's
  index of the source's name, as Java's policy (which holds a name) does, never the caller's `*Index`
  object where the two differ. `PartlyBuiltError` (with Java's `INDEX_VERSION`, as `IndexVersion`),
  the source-index `IndexingValidationError` and `SynchronizedSessionLockedError` carry Java's
  `INDEXER_ID` as `IndexerID`: the indexer that raised the first two, and the indexer the third
  refused. Every build transaction now classifies its
  targets' states as Java does: a target published meanwhile is an `UnexpectedReadableError`, which
  ends a mutual build successfully when every target is published and otherwise continues it as a
  records scan, and fails any other build; a target disabled meanwhile (or moved between
  WRITE_ONLY and WRITE_ONLY_WITH_QUEUE) is a `RecordCoreStorageError`, where it used to be an
  `IndexingValidationError`. Every attempt of one `OnlineIndexer` writes its heartbeat under one
  indexer ID, as Java's does, and names it by the build's stamp method (`BY_RECORDS`, `BY_INDEX`,
  `MULTI_TARGET_BY_RECORDS`, `MUTUAL_BY_RECORDS`), the heartbeat `info` Java writes, where Go wrote
  "online index build". Since the next attempt writes the same heartbeat key, the bounded cleanup
  reads each key before clearing it, so a clear whose commit lands late conflicts with that write
  instead of erasing a live heartbeat.
- **`OnlineIndexer.MergeIndexes` (new on this branch) holds an indexing session.** Java's standalone
  `mergeIndex` has no heartbeat (`IndexingBase.java:969-972`, `:1085-1096`): it writes none, checks no
  index state and proceeds under a running build. Go's writes its own heartbeat over each WRITE_ONLY
  target for the merge and clears it afterwards, so an exclusive builder (a Java one included) or a
  fresh Go mutual session that starts meanwhile is refused as by any live session; a mutual builder
  is not, and any merge transaction that runs while that builder's heartbeat is live fails the MERGE
  (its exclusive check meets the builder's heartbeat) while the builder carries on (a VALUE target's
  merge is one transaction, so a builder admitted after it lets that merge complete); a live peer's
  heartbeat refuses the merge
  itself (`SynchronizedSessionLockedError`); a DISABLED target fails it (`RecordCoreStorageError`
  "Unexpected index state(s)"); over a READABLE target the merge runs and writes no heartbeat. See
  DIVERGENCES.md, "OnlineIndexer session start and build catcher: where Go differs".
- **Subspace keys are compared as Java compares them** (RFC-257 WS-C). Java normalizes an index or
  former-index subspace key when it is assigned and compares the normalized objects; Go now does the
  same at every comparison: the meta-data validator, `GetIndexFromSubspaceKey`, the evolution
  validator and the online indexer's duplicate targets. So `RecordMetaDataBuilder.Build` accepts a
  byte-array key beside a string key of the same content, and `0.0` beside `-0.0`, each two
  prefixes, which it refused; refuses two NaN keys and two former indexes with one key, which it
  accepted; and reports each collision with Java's message ("Same subspace key K used by both A and
  B", "... used by two former indexes A and B", "... used by index A and former index B"); two
  parts of those messages are Go's: the pair is named in name order where Java names it in its
  HashMap's order, and a `[]byte` or list key renders with Go's `%v`. Its former-index version
  checks report Java's messages too ("Former index X has added version N which is greater than
  the removed version M", and the two meta-data version messages), and an index's version checks
  name the index as Java does ("Index X has added version ..."). `Index.SetSubspaceKey` stores the
  key normalized, as Java's setter does, so an `int32`, a narrow unsigned integer, a `[]any`, a
  protobuf enum or an `fdb.Key` is stored in a form the tuple encoder writes (packing one used to
  panic), and a nil key, a typed nil `*big.Int`, `*FDBRecordVersion` or generated-enum pointer
  included, is refused with Java's `RecordCoreArgumentError` "Index subspace key cannot be null" by
  every `Build` the index was handed to, whether the set came before or after `AddIndex` (an
  `AddIndex` naming an unknown record type included) and also
  when the index was later removed or refused as a duplicate; the refusal is sticky, leaves the key
  and its explicit mark as they were, and comes first in program order among the builder's own
  faults, as Java's throw ends the program's sequence of calls. A refused set on an index of an
  already-built `RecordMetaData` has no `Build` to return it: it changes nothing and
  `Index.SubspaceKeyError` reports it (Go-only; Java throws from the setter). An index built as a
  struct literal is keyed by its name, as every Java constructor keys it (it had a nil key, was
  maintained under the null item and was saved with no key), and a former index with a nil key is
  refused with Java's "FormerIndex initialized with null subspace key".
- **`MetaDataEvolutionValidator` pairs indexes by subspace key, as Java's does**, not by name. An
  index that keeps its name and moves to another subspace key, versions unchanged, is refused ("index
  missing in new meta-data"): Go accepted it, and a store would then have read the index, as
  readable, from a subspace nothing had built. An index that keeps its key under a new name is
  refused as renamed; an index moved to a new key whose old key became a former index is accepted,
  which Go refused. A former index kept from the old meta-data must keep its name even with
  `SetAllowMissingFormerIndexNames(true)`, which admits only an unnamed former index replacing an
  index; the checks run in Java's order and report Java's messages (for example "old index has
  last-modified version newer than new index", "former index added after old index", "former index
  reports added version older than replacing index", "former index key used for new index in
  meta-data", "index type changed", "index key expression changed", "new index removes record
  type", "new index adds record type that is not newer than old meta-data", "new index changes
  primary key component positions", "field renames result in inconsistent index definition for
  multi-type index"), each followed by Go's detail; so do the record-type, field, enum and
  per-index-type option checks ("record type since version changed", "field removed from message
  descriptor", "field type changed", "repeated field is no longer repeated", "enum removes value",
  "index option changed", "index adds uniqueness constraint", "rank levels changed", "rtree splitS
  changed", "attempted to change immutable vector index option", and the rest), all but the Go-only SPFresh
  index's option check, which has no Java counterpart, and the refusal of a `RecordMetaData` that
  was never built (it has no union; every built one has, as every Java one has, so the validator's
  union-less path is gone). An index root and a primary key
  are compared as Java's `KeyExpression.equals` compares them, so a root that changes only a
  field's null interpretation is admitted as Java admits it (Go compared the protos and refused
  it). A field's checks run in Java's order (its type before its label), and its label is checked
  as Java checks it: a required field must stay required, a repeated one repeated, and a field
  must keep whether it tracks presence, so an OPTIONAL FIELD MADE REQUIRED is admitted, as Java
  admits it, where Go refused every change of cardinality. The RANK, R-tree and vector option
  checks compare each option's EFFECTIVE value, as Java's do (for the vector index, its `hnsw*`
  canonical option names; the `vector*` aliases and Java's metric and boolean parsing are WS-D's
  typed option catalog), parsed as the maintainers parse it (above), so an option set to its default where it was unspecified (or the reverse) is admitted,
  where Go compared the raw strings and refused it; an unrecognized vector metric name is still a
  change. An option neither engine's parser accepts refuses the change with the parser's own error
  class, as Java's check does (`RecordCoreArgumentError`, `NumberFormatError`,
  `IllegalArgumentError`). Record types, changed options and renamed types are walked in a fixed
  order (names; the old union's fields for renames, as Java walks them), so which of several
  violations a message names does not depend on map order; so is `RecordTypesForIndex`. `Build`
  walks the record types by name (Java walks a HashMap, so where several types are at fault the
  one named can differ), and runs Java's checks in Java's order: a record type without a primary
  key first ("Record type X must have a primary key"), then the union's oneof ("Union descriptor
  has more than one oneof", "Union descriptor oneof must contain every field"), "No record types
  defined in meta-data", then for each record type, before any index, its primary key validated,
  then "Primary key for X can generate more than one entry", "Same record type key K used by both
  X and Y" and "Record type X has since version of N which is greater than the meta-data version
  M". A key expression that does not fit its descriptor is refused with Java's
  `KeyExpression.InvalidExpressionException` (`KeyExpressionError`) and Java's text ("Descriptor X
  does not have field: f", "f is not repeated with FanType.FanOut", "f is repeated with
  FanType.None", "Child expression of covering expression returns too few columns", "Must have a
  single key before splitting", "Must produce multiple values for splitting"), and a message field
  read as a scalar with Java's `Query.InvalidExpressionException` (`QueryInvalidExpressionError`,
  "f is a nested message, but accessed as a scalar"), all unwrapped as Java throws them, where Go
  wrapped Go texts in a `MetaDataError` naming the record type. Two more are Java's
  now: a nesting into a scalar field is protobuf-java's `UnsupportedOperationException`
  (`UnsupportedOperationError`, "This field is not of message type. (<field's full name>)"), and a
  map field is repeated, as protobuf-java says. One refusal is Go-only: a primary key with no
  columns. A Then of fewer than two children,
  which Java refuses where it is built ("Then must have at least 2 children"), is refused by
  `Build` in program order with the builder's other faults, where Go stored a Then neither
  engine's loader reads back. `RecordTypeKey().Nest(x)` is now `Concat(RecordTypeKey(), x)`, the
  Then Java writes, where it was a record type key holding a child. The field-renaming
  visitor's refusals have Java's text ("field not found in source descriptor", "parent field is not
  of message type", and the rest), and an expression it cannot rename is Java's
  `RecordCoreArgumentException`. `Build`'s index texts are Java's too: "Index X has added version
  N which is greater than the last modified version M", "Index X has replacement index Y that is
  not in the meta-data" and "... that itself has replacement indexes". `RemoveIndex` of a name no
  index has is refused with Java's "No index named X defined" (Go ignored it), in program order
  with the builder's other faults. The conformance spec "Index option
  changes in meta-data evolution" runs 30 option changes through Java's validator and Go's, and
  requires the same class and Java's message as the prefix of Go's. The conformance specs "Subspace-key identity in meta-data validation", "Subspace-key
  pairing in meta-data evolution" and "Field and record-type changes in meta-data evolution" run
  each shape through Java's validators and require Java's whole message as the prefix of Go's.
- **A stored index with no root expression is refused**, as Java's `Index(proto)` refuses it
  ("Exactly one root must be specified for an index"); Go loaded the index with no root. The
  refusal, and those of a nesting with no parent and a then of fewer than two children, are
  `KeyExpressionDeserializationError`, Java's `KeyExpression.DeserializationException`, with Java's
  text. Go no longer builds or writes such an index either: `Build` refuses an index with no root
  ("Index X has no root expression"; Go-only as a refusal, since Java's constructors take a
  non-null root and its validation fails with a NullPointerException), and so does serializing one.
- **RANK and TIME_WINDOW_LEADERBOARD ranked sets hash as Java's do.** The `rankHashFunction`
  option names one of Java's four hash functions, `JDK`, `CRC`, `RANDOM` and `MURMUR3` (Guava's
  murmur3_32, seed 0), exactly; an unknown name is refused with Java's
  `RecordCoreArgumentException` "hash function not found: X" when the index is maintained. Go hashed
  `MURMUR3` and `RANDOM` indexes with the JDK hash, which puts scores on other ranked-set levels than
  Java does: a Java store's ranked set maintained by Go, or the reverse, then had its counts
  corrupted by the other engine's deletes. `rankNLevels` is read with `Integer.parseInt` and refused
  outside [2, 8] ("levels must be between 2 and 8"): an earlier Go kept the default for a value that
  was not a positive int, wrote ONE level for "1" and clamped a value above 8 to 8; such an index, which
  only an earlier Go build wrote, is refused on every write (Compatibility above). `rankCountDuplicates` is read with
  `Boolean.parseBoolean` ("TRUE" is true). A failed read of the randomness `RANDOM` draws from fails
  the write (a zero hash would put the key on every level). The conformance spec "RANK ranked set per hash
  function" saves the same records through Java and Go and requires byte-identical ranked sets for
  each deterministic hash, and Java to read the ranks Go wrote with `RANDOM`.
- **MULTIDIMENSIONAL indexes honour Java's R-tree options.** The `rtreeStorage` option's `BY_SLOT`
  layout (one key-value pair per node slot, Java's `BySlotStorageAdapter`) and the
  `rtreeUseNodeSlotIndex` option's node slot index (an entry per child slot in the index's secondary
  subspace, Java's `NodeSlotIndexAdapter`) are now written and read; Go ignored both and maintained
  such a Java index in the BY_NODE layout with no node slot index, corrupting it. The options are
  read as Java's `MultiDimensionalIndexHelper.getConfig` reads them, including its quirk that
  `rtreeStoreHilbertValues` is read only when `rtreeStorage` is set, and then an absent value is
  false: so `{rtreeStorage}` stores no Hilbert values in leaf slots, and `{rtreeStoreHilbertValues:
  false}` alone stores them (Go did the opposite of both). `rtreeMinimumM`, `rtreeMaximumM` and
  `rtreeSplitS` are read with `Integer.parseInt` (a bad value refused, where Go kept the default),
  the storage name with `RTree.Storage.valueOf` (an unknown one refused), the flags with
  `Boolean.parseBoolean`. The conformance spec
  "MULTIDIMENSIONAL index R-tree options" has Java and Go write, delete and scan the same records
  under six option sets and checks the stored layout after each step.
- **Index options other than the vector index's are parsed as Java parses them.** (The vector
  index's `hnsw*` booleans are WS-D's typed option catalog.) Boolean options (`unique`,
  `clearWhenZero`, the text index options, `rankCountDuplicates`) are true for "true" in any case,
  as Java's `Boolean.valueOf`; Go required exactly "true", so `unique: "TRUE"` made a unique index
  for Java and not for Go. A text index's `textTokenizerVersion` is `Integer.parseInt`'d when
  present, the empty string included, with Java's refusal "tokenizer version could not be parsed as
  int"; Go treated "" as absent. A PERMUTED_MIN/MAX index's `permutedSize` is read with
  `Integer.parseInt` by the maintainer, the executor and the planner, as Java reads it, and `Build`
  runs Java's validator: a grouping with at least one grouped column ("index type requires
  grouping", "... at least 1 fields"), no version column, and a size that is present ("permuted
  size not specified"), parses, and is neither negative nor past the grouping count. An earlier Go
  read the size with `strconv.Atoi`, maintained an absent or unparsable one (such as "٢", which
  Java reads as 2) at size 0, and built none of those refusals.
- **A key expression is a flat Then, as Java's is.** `Concat` flattens a composite child into its
  children, as Java's `ThenKeyExpression` constructor does, so Go writes the flat Then Java writes
  (it wrote a nested one for a nested `Concat`); a stored Then is decoded and flattened before its
  children are counted, so a Then holding one Then of two children loads, as in Java (Go refused
  it), and a nested Then an earlier Go wrote loads flat.
- **`Build` validates indexes as Java's `MetaDataValidator` does, index by index.** Each index runs
  its type's validator (its key validated against every record type it covers, the validator's
  check of the fields, the added version against the last modified, then the type's own checks),
  then its subspace key, its versions against the meta-data version and its replacements; former
  indexes follow, then a key an index and a former index share. Go ran each kind of check over
  every index before the next kind, so where two faults met, a different one was reported. The
  validators are Java's, with Java's texts and classes: VALUE (no grouping, no version), the atomic
  types (the grouping each mutation needs, "index type does not support non-group fields; use
  COUNT_NOT_NULL", "index type only supports single field", an integer field for SUM and the
  `_EVER_LONG` types, "index type does not support clearWhenZero"), RANK and
  TIME_WINDOW_LEADERBOARD (a grouping with a grouped column), PERMUTED_MIN/MAX, BITMAP_VALUE (an
  integer position), TEXT, VERSION and MULTIDIMENSIONAL (a dimensions key). Meta-data Java refuses
  is refused (a VALUE index over a grouping, a RANK index built in code over a plain field, a
  COUNT_NOT_NULL over `GroupAll`, a leaderboard over a plain Then), and a MAX_EVER_VERSION index no
  longer requires record versions, which Java does not. The conformance spec "Index validation at
  build, as Java builds" gives 50 shapes to both loaders (at RFC-257 WS-C revision 13) and compares
  class and text. A Then of
  fewer than two children and an unknown record type named by `GetRecordType` (which panicked) are
  builder faults in program order; `AddMultiTypeIndex` resolves its record types before it adds the
  index, as a Java caller must.
- **A BITMAP_VALUE index's `bitmapValueEntrySize` is read as Java reads it**: `Integer.parseInt`,
  10000 when absent, and "entry size option is too large" above 250000. Go read it with `strconv`
  and fell back to 10000 on every refusal, so "٢" was maintained at size 10000 where Java maintains
  it at 2. A size of zero or below is refused where the index is used (Java fails at its first
  write).
- **A stored former index's subspace key is read as Java reads it.** An absent, empty or
  multi-item key is refused with `RecordCoreError` "subspace key must encode a single item tuple", a
  null item with `RecordCoreArgumentError` "FormerIndex initialized with null subspace key"; Go read
  all four as a nil key, and a store's upgrade then cleared the null item's index subspace instead of
  the dropped index's. A former index is written as Java writes it: the key always, and no name when
  it has none (Go wrote an empty name, which Java reads as a name).
- **`RecordCoreArgumentError` renders only the fields its site set**, where it rendered empty
  `scanType=` and `index=` fields for every error.
- **A stored index's options are read with its type, before its root and subspace key**, as Java's
  `Index(proto)` reads them, so an index with a repeated option and a malformed key or root is refused
  for the repeated option in both engines (RFC-257 WS-J).
- **Indexing heartbeat keys are read as Java reads them** (`getUUID(0)`): a `(UUID, x)` key is that
  UUID's heartbeat rather than a malformed key. Keys whose first element is not a UUID remain an
  `IndexingHeartbeatKeyError` wherever they sort (RFC-257 WS-D).
- **Index definitions are stored as Java stores them** (RFC-257 WS-J; each shape byte-compared against
  the JVM by the WS-J oracle, and through the SQL `CREATE SCHEMA TEMPLATE` path by
  `TestFDB_IndexDefinitionProductionPathStoresTargetShapes`). What a template built from the same DDL
  persists changes; templates already stored keep their bytes (nothing rewrites them):
  - a nested field followed by a top-level column (`ORDER BY s.x, ts`) keeps its nested subtree in the
    root expression, where Go dropped it;
  - a literal is stored with its own width: an INT literal as `int_value` (Go stored `long_value`), a
    FLOAT literal as `float_value`, and the bitmap entry size as the INT `10000`. Over a template Go
    stored before this change the target cannot plan any bitmap query (XX000 "unable to encapsulate
    arithmetic operation due to type mismatch(es)") and serves an arithmetic-index equality by a full
    index scan. Rebinding such a tenant to a template rebuilt on this version is admitted by the
    relational rebind validator when the literal's carrier moved from `long_value` to `int_value` (or,
    inside long arithmetic, `double_value` to `float_value`) without changing its value
    (`SetAllowLiteralCarrierWidening`, one way only; the core validator still refuses it, as Java's
    does; `frl meta evolve-check --allow-literal-carrier-widening` reproduces it). The rebuilt template
    keeps the stored index versions only through the template-version carry rule of RFC-257 WS-J
    section 4, which ships in the same release: without it a DDL rebuild shifts every index's versions
    and the rebind is refused. An INT literal outside 32 bits is refused (XX000) instead of wrapping,
    whatever Go integer kind carries it;
  - index options are stored in Java's insertion order (`unique` first, then the type's options), where
    Go wrote them in map iteration order, so the stored bytes varied from build to build.
- **Long-arithmetic key functions read any numeric operand as Java's `getNullableLong` does**
  (truncating toward zero, NaN to 0, saturating), where Go refused every non-`int64` operand; the
  index entries equal the target's byte for byte (WS-J F2b spec). Go does not serve a query from such
  an index over a non-integer operand: the entries do not hold the value of the query expression, and
  the target, which does serve it, returns no row for `WHERE d + d = 3` over `d = 1.5`
  (DIVERGENCES.md).
- **A stored index's deprecated `value_expression` and a missing `added_version` are read as Java
  reads them** (`Index.java:215-233`): the value expression is folded into the root as
  `keyWithValue(concat(root, value), root's column size)`, and an index stored without an added
  version counts as added at version 1, which is what the evolution validator and a `FormerIndex`
  made by removing it now see. Go ignored `value_expression`, maintaining the index under its bare
  root with no value columns, and took the last-modified version for a missing added version. Go
  never writes a `value_expression`; both shapes come from metadata Java wrote with the deprecated
  field or before `added_version` existed, or from hand-built protos. Entries an earlier Go build
  maintained for such an index lack the value columns (Compatibility above).
- **A stored index's subspace key is read as Java reads it** (`Index.java:80-97`, `:221-225`): a
  present key must pack exactly one non-null item. An empty key or a key of several items is a
  `RecordCoreError` ("subspace key must encode a single item tuple"), a null item a
  `RecordCoreArgumentError` ("Index subspace key cannot be null"), as Java refuses them; Go fell back
  to the index's name, loading metadata Java refuses and maintaining the index under a subspace Java
  never reads.
- **Stored index metadata listing one option key twice is refused** (`DuplicateIndexOptionError`,
  Guava's message), as Java's `Index` constructor refuses it; Go kept the last value.
- **The metadata evolution validator refuses a lower metadata version** even when an unchanged version
  is allowed (`MetaDataEvolutionValidator.java:154`); Go accepted the downgrade, after which every
  store open failed on stale metadata.
- **`uint32` / `fixed32` fields are read signed**, as protobuf-java hands them to Java, by every
  reader (key and index-predicate evaluation, row values, the SQL driver's values), typed `INTEGER`
  as Java types them, and written from the `INTEGER` range as the value's 32 bits; `uint64` /
  `fixed64` are `BIGINT` and written as the value's 64 bits (such fields are refused in record
  descriptors, above; this keeps a descriptor that reaches a reader otherwise consistent).
- **A key field's null interpretation is honoured** (Java's `Key.Evaluated.NullStandin`,
  `Key.java:394-421`): `NOT_NULL` evaluates an unset field as its type's default and an unset
  parent message as the parent's default message; `NOT_UNIQUE` (the default) and `UNIQUE` evaluate
  it as null. A unique index, and a `COUNT_NOT_NULL` index's grouped columns, ignore only the
  default's null (`IndexEntry.keyContainsNonUniqueNull`): a `UNIQUE` or `NOT_NULL` field's null,
  and a function's plain null (the arithmetic functions'), collide and are counted, where Go
  ignored every null. `collate_*` and `cardinality` return the ignored null, as in Java; a Go
  function whose Java twin returns `Key.Evaluated.NULL` registers with
  `RegisterNonUniqueNullFunction`. The interpretation is written back unchanged (Go rewrote
  `NOT_NULL` as `NOT_UNIQUE`) and, as in Java, is not part of an index's definition to the
  evolution validator. New API: `NullStandin`, `FieldWithNullStandin`.
- **`IndexMaintenanceFilter` is ported** (Java's store option): `StoreBuilder.SetIndexMaintenanceFilter`
  with `IndexMaintenanceFilterNormal` (the default) and `IndexMaintenanceFilterNoNulls`, or a filter
  of your own, decides per index and record which entries every maintainer writes; the online
  indexer takes it with `OnlineIndexerBuilder.SetIndexMaintenanceFilter`. A TEXT index now honours
  its predicate, as Java's does. The index entries written under `NO_NULLS` equal Java's for VALUE,
  COUNT, SUM, TEXT and RANK indexes (conformance "RFC-257 NullStandin").
- **A non-idempotent index under a build from a source index is maintained as Java maintains it**:
  a write during a BY_INDEX build of a COUNT, SUM, or duplicate-counting RANK or leaderboard index
  applies where the record's source-index key is built, where Go checked its primary key against
  that range set and miscounted. BITMAP_VALUE, MULTIDIMENSIONAL, VECTOR and non-counting
  TIME_WINDOW_LEADERBOARD indexes are idempotent to the online indexer, as in Java (snapshot scans,
  no per-record read conflicts).
- **A proto3 field at its default value is absent**, as protobuf-java's `hasField` reports it, to
  key evaluation (index and primary-key bytes hold null, not the zero value) and to a query's field
  reads (`MessageHelpers.getFieldOnMessage`); an unset proto2 field declaring an explicit default
  now reads that default in a query, as in Java, where Go read null. Live-JVM comparisons:
  conformance specs "RFC-257 NullStandin" and "RFC-257 a query reads a field as Java's
  getFieldOnMessage".
- **Map fields and groups in key expressions are Java's** (RFC-257 WS-C): a nesting that fans out a
  proto map's entries builds and is maintained (Go refused it), each entry the key/value message
  protobuf-java reads, and a nesting into a proto2 group builds, where Go refused both; a group read
  as a scalar is refused with Java's text. A record's map entries are indexed in the order its
  stored bytes hold them, as Java indexes them, so a covering index whose key two entries share
  stores the last entry's value in both engines, and a key written twice yields both entries; a
  record type that holds a map is now written with its map entries in key order (the order Go
  indexes a record it saves), including a generated type whose vtproto marshal would not sort them.
- **Key validation Java makes at build, Go makes** (RFC-257 WS-C): a MULTIDIMENSIONAL index's
  dimension columns must be protobuf `int64` ("the declared dimension columns have to be of type
  INT64"), so meta-data with dimensions over `int32` fields no longer builds; the prefix and
  dimension sizes must fit the key; and a CARDINALITY key's argument must produce one value and
  name fields the record has. A negative dimensions prefix, which panicked, is refused.
- **`GetSnapshotRecordCountForRecordType` counts from a COUNT index, as Java does** (RFC-257 WS-C;
  a behaviour change): a COUNT index on the type, else a universal COUNT index grouped by record
  type, else `RecordCoreError` "Require a COUNT index on X". It no longer reads the record count
  key; read a count key grouped by record type with `GetSnapshotRecordCount(tuple.Tuple{typeKey})`,
  as Java's `getSnapshotRecordCount` does. `frl record count --type` reads such a count key, then
  the COUNT indexes, and says which index is missing when there is none.
- **A record type whose key is not an integer is handled as Java handles it**: the store-open
  index rebuild counts or probes only that type's records when every new index is on it, and the
  online indexer's build presets the ranges outside the indexed types, ordering the type keys as
  Java's `Tuple.compareTo` does; Go treated a string or bytes key as covering the whole store.
- An unknown record type is refused with Java's text, "Unknown record type X" (`MetaDataError`),
  by `SaveRecord` and the aggregate functions, and a vector index option that does not parse is
  Java's `MetaDataError` "incorrect index options", its parse error the cause (`Unwrap`;
  `MetaDataError` gains `Cause`).
- An index predicate's field path steps into a proto2 group as into a message, as Java's
  `FieldValue` does, where Go treated the record as not matching.
- `KeyExpressionInvalidResultError.ActualType` names the Java class of the offending value, matching
  `ExpectedType` (`com.google.protobuf.DynamicMessage` for a message read through a run-time
  descriptor; a Go-generated message keeps its proto full name).
- **The catalogs guard a template version against re-issue** (RFC-257 WS-J; a declared Go
  extension, `DIVERGENCES.md`): creating a template, or a new version of one, is refused with
  42F59 naming the schema while any schema binds a dropped version of it above the latest one
  stored (every version, for a template stored afresh), and `DeleteTemplateVersion` refuses a
  version a schema binds. The target accepts the first and rebinds those schemas to the new
  metadata. Both the FDB-backed and the in-memory catalog apply it.
- **A schema bound to a template version that is gone is refused as Java refuses it**: `LoadSchema`,
  `SaveSchema` over it and `RepairSchema` fail with 42F55 "SchemaTemplate=<n>, version=<v> is not
  in catalog", where Go's `SaveSchema` (both catalogs) and the in-memory `LoadSchema` and
  `RepairSchema` accepted it, the saves rebinding it without validation. The template loads and `SaveSchema`'s
  database and template checks now use Java's texts ("SchemaTemplate '<n>' is not in catalog",
  "Cannot create schema <s> because schema template <n> version <v> does not exist.").

## [v0.1.0] - 2026-08-26

### Added
- **Record-layer metrics exporter** (`pkg/recordlayer/rlmetrics`): `StoreTimer` (the port of Java's
  `FDBStoreTimer`) rendered in the Prometheus text exposition format under `fdb_recordlayer_`, with
  zero new dependencies — the record-layer counterpart to `pkg/fdbgo/fdbmetrics`. Timed events are
  summaries in seconds, counts and byte totals are counters. `recordlayer.Event` gains a `Kind`
  (timed / count / size) porting Java's `Event`-vs-`Count(isSize)` split, and `StoreTimer` gains
  `Add` and `KeysAndValues` from Java. Reachable from the SQL layer via
  `FDBDatabase.SetTimer` or, for `database/sql`-only deployments,
  `sqldriver.EnableStoreTimer(clusterFile)` — one timer per cluster-file key (per process), with no
  per-tenant label by design; see `docs/mt-saas.md` §4 for the cardinality reasoning.

- **Multi-tenant SaaS operator guide** (`docs/mt-saas.md`): the tenancy model (one database path per
  tenant, subspace-per-database, and why native FDB tenants are not used), the trust boundary and
  `RESTRICT_DDL_TO_SESSION_DATABASE`, the five per-statement quotas and how to arm them (none is
  DSN-settable), per-tenant observability, the pure-Go TLS certificate requirement, and the
  tenant-facing SQL contract. Every claim carries a `file:line` citation; the page is on
  `pkg/docscheck`'s `livingDocs` list so its version anchors are drift-guarded.
- Statement-wide **memory byte budget** for the SQL executor: opt-in `OptMaxStatementMemoryBytes`
  bounds every cardinality-growing buffer by bytes (not just the 100k-row `MaterializationLimit`);
  breach → SQLSTATE `54F01`. Default `0` = unlimited (RFC-130).
- A documentation-consistency CI guard (`pkg/docscheck`) that fails the build if a living doc drifts
  from the `MODULE.bazel` / `go.mod` version pins or reintroduces a known contradiction (RFC-131/132).
- A public **FDB client option matrix** (`pkg/fdbgo/fdb/OPTIONS.md`) classifying every `Set*` option
  as honored / `UnsupportedOptionError` / safe no-op, each with its `libfdb_c` 7.3.77 reference, plus
  a completeness guard that fails CI if an option is added without a matrix row (RFC-133).
- A **panic-boundary release gate** (RFC-134): a `norecover` nogo analyzer fails the build if a
  `recover()` is added outside the documented panic→error boundary allowlist (`docs/panic-audit.md`
  §2), plus docscheck guards that keep the four input-boundary fuzz nets wired and the doc in lockstep
  with the allowlist. Makes the "untrusted input → error, never crash" discipline self-enforcing.

### Changed
- **FDB C++ wire-protocol baseline bumped 7.3.75 → 7.3.77** (RFC-152). Patch bump within the 7.3
  line: no wire-format, error-code, `ClientKnobs`, RYW, or serialization change (regenerated
  `pkg/fdbgo/wire/types/*_generated.go` differ only in the version-string header comment). The only
  client-relevant upstream delta is PR #12935 (a `peer->disconnect` arm in `waitValueOrSignal` so
  `loadBalance` fails an in-flight request immediately on peer disconnect instead of waiting out the
  failure-monitor lag); the pure-Go client already has this structurally (single-owner connection:
  `readLoop` EOF → `failConnection` → `failAllPending`), pinned by
  `TestPeerDisconnect_FailsInFlightReplyImmediately`. No production-code behaviour change.
- SQL `LIMIT`/`OFFSET` now flows through a single uniform `RecordQueryLimitPlan` + continuation
  envelope, including for nested derived tables (RFC-128).

### Fixed
- **Record-layer instrumentation was measuring the wrong things, or nothing at all.**
  `Counter.Increment` wrote its amount into the cumulative-value field as well as the count, so every
  count event reported a bogus duration (Java's `Counter.increment` touches `count` alone). The scan
  events timed cursor *construction* rather than each record produced, which for a lazy cursor
  measures an allocation and reports one occurrence per scan regardless of size (Java instruments the
  cursor's `onNext`). Scan events also sat on entry points the query engine does not use, leaving the
  executor's record scans, aggregate/group index scans, vector scans and primary index scans
  uncounted. And `EventCommit` was recorded only by `CommitWithVersionstamp`, so every commit on the
  SQL path — autocommit and explicit `BeginTx` alike — went unrecorded. Instrumentation only; no
  wire-format, plan or result change.
- **Legacy Java store layouts are now fully readable, writable, and auto-upgraded** — closes the
  `FormatVersion` < 6 / `omit_unsplit_record_suffix` wire-compatibility gap that was previously a
  *silent* data-correctness bug (Go accepted an old-format store header but only understood the modern
  inline layout, so it would silently fail to see a legacy store's record versions and unsplit
  records). Go now mirrors Java's `FDBRecordStore.useOldVersionFormat()` end-to-end:
  record versions are read/written in the separate `RecordVersionKey(8)` subspace for stores below
  `SAVE_VERSION_WITH_RECORD` (format 6), and unsplit records are read/written at the bare primary key
  (no `0` suffix) when `omit_unsplit_record_suffix` is set — across load, scan, `scanRecordKeys`,
  `recordExists`, save, update, delete, and `deleteRecordsWhere`. On open, Go also performs Java's
  transactional format upgrade (`checkRebuild`/`addConvertRecordVersions`): it bumps the stored
  `FormatVersion`, sets `omit_unsplit_record_suffix` for a non-splitting store created before format 5,
  and moves versions from subspace 8 to their inline `pk + -1` location when upgrading a splitting
  store past format 6. Pinned by FDB integration tests that lay down each legacy layout and assert
  byte-level read/write/scan/delete and migration parity with Java. (Closes the `TODO.md` "no read
  path for format-version-<6 record versions / unsplit records" gap surfaced by the RFC-131 audit.)
- SQL pagination no longer treats a non-terminal `StartContinuation` as end-of-results (a latent
  early-truncation bug); exhaustion is decided off `IsEnd()`, not byte-emptiness (RFC-127).
- Pure-Go FDB client: `Get`/`GetRange` read-conflict ranges are clamped to the data actually returned
  and filtered through the RYW overlay, matching `libfdb_c` (no under-conflict; RFC-121).
- `go test ./...` is clean from a fresh checkout: the Bazel-runfiles-only suites are build-tagged so a
  plain `go test` no longer panics (RFC-129), and the heavy million-row stress benchmarks under
  `pkg/relational/sqldriver/stress` now carry a `stress` build tag, so a plain `go test ./...` skips
  them instead of spinning up million/ten-million-row FDB workloads (they still run via Bazel's
  `manual` stress target and the nightly stress workflow).
- Pure-Go FDB client: three **database-level transaction defaults** that change read semantics are now
  honored instead of silently dropped — `SetSnapshotRywDisable`/`Enable` (a cumulative counter,
  matching `libfdb_c`), `SetTransactionBypassUnreadable`, and `SetTransactionCausalReadRisky`. They
  propagate to each new transaction via `applyTxDefaults` and are replayed idempotently across retries
  (RFC-133).

### Compatibility
- **Wire format:** unchanged for modern stores — records, indexes, versions, continuations, and split
  records remain byte-identical to Java `fdb-record-layer-core` 4.12.11.0. **Newly closed gap:** Go now
  reads *and* writes legacy Java store layouts (`FormatVersion` < 6 record versions in the
  `RecordVersionKey(8)` subspace, and `omit_unsplit_record_suffix` bare-key unsplit records) and
  performs Java's on-open format upgrade — previously a silent read gap (see Fixed). A Go client can
  now safely share a cluster with legacy Java stores in either direction.
- **SQL behaviour:** net additions only (memory-budget option; the LIMIT-envelope and pagination
  fixes correct latent bugs, they don't change correct-query results).
- **FDB client option semantics:** now documented option-by-option in `pkg/fdbgo/fdb/OPTIONS.md`
  (honored / `UnsupportedOptionError` / safe no-op, vs `libfdb_c` 7.3.77). **One behavioural change:**
  three database-level defaults (`snapshot_ryw_disable`/`enable`, `transaction_bypass_unreadable`,
  `transaction_causal_read_risky`) that were previously silent no-ops now take effect on each new
  transaction — a caller that set them and relied on them being ignored will now see them applied
  (this is the faithful `libfdb_c` behaviour). The unsafe access/auth/quota family still fails loud
  with `UnsupportedOptionError`; no option's *wire* behaviour changed (RFC-133).
- **Required versions:** Java `fdb-record-layer-core` **4.12.11.0**, FDB C++ client **7.3.77**, Go
  **1.26.x** (the `MODULE.bazel` / `go.mod` pins; the CI doc-guard enforces docs match them).
