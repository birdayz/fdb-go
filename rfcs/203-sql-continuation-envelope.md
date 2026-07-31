# RFC-203 — The SQL continuation envelope: `EXECUTE CONTINUATION`, `ContinuationProto`, and per-execution `MAX_ROWS`

**Status:** DRAFT. Query-engine + SQL-driver change; needs a Graefe ACK on this
RFC before implementation starts and one joint implementation lap per phase,
plus Torvalds / codex / @claude per the house gauntlet.

**Decision in one sentence:** adopt Java's `ContinuationProto` verbatim as the
SQL resume token, give `MAX_ROWS` its Java per-execution page meaning, implement
`EXECUTE CONTINUATION` by re-entering Cascades under a plan-identity gate rather
than by deserializing a foreign plan, surface the token through a
driver-specific interface on `database/sql`, and **target same-engine resume —
declining cross-engine plan resume on measured, structural grounds, with the
decline enforced loudly in both directions and its re-arm conditions stated as
measurable gates.**

**Closes:** TODO.md CQ-69.2's continuation half (`TODO.md:12552-12556`) and
unblocks CQ-69.4's `ForceContinuations` oracle (`TODO.md:12564-12575`).
**Supersedes:** the `OptMaxRows` semantics decided in RFC-106a §3
(`rfcs/106-statement-resource-limits.md:47-50`) — refuted in §1.1 below.
**Amends:** `DIVERGENCES.md:1161-1206` (the RFC-181 C2 decision record).
**Invalidates a premise of:** RFC-191 condition (D)
(`rfcs/191-in-join-descending-ordering.md:774-779`, `TODO.md:905-910`) — §8.

Every Go citation is at `ef2b5911a` (master at the time of writing). Every Java
citation is at tag **4.12.11.0**, the pinned reference, under
`fdb-record-layer/`.

---

## 0. Four corrections to the framing this RFC was commissioned under

These are stated first because each one changes what the work is. A reader who
carries the original framing into §4 will design the wrong thing.

**0.1 — There is no "Phase-2 STOP" document.** The commissioning brief cites a
"Phase-2 STOP" recording four measured absences. No such text exists in
`TODO.md`, `rfcs/`, `DIVERGENCES.md`, or any shift handover; every `STOP`
occurrence in `TODO.md` is unrelated to continuations. The **four absences are
all real** and each is independently re-verified below with a citation — but
they were not previously recorded together, and nothing may be quoted from a
document that does not exist. The nearest real constraint text is CQ-69.2's
"under the hard wire-compat line" (`TODO.md:12554-12556`) and RFC-201 §4.2's
"wire-format-critical and therefore under the hard compatibility line"
(`rfcs/201-layered-test-corpus.md:196-199`).

**0.2 — RFC-181 WS-C C2 did not decide anything; it framed the choice.** The
brief asks this RFC to reverse "the RFC-181 WS-C C2 decision". RFC-181's C2
entry (`rfcs/181-query-engine-correctness-wave3.md:290-302`) ends:

> Decision needed: adopt ContinuationProto + hashes, or declare Go SQL tokens
> engine-private — either way pinned by a cross-engine SQL continuation
> conformance test.

The **decision** was recorded when WS-C landed, in `DIVERGENCES.md:1161-1206`.
That is the text this RFC reverses, and §3.1 quotes it in full. The distinction
matters because the recorded decision already names this RFC's direction as its
own successor, which changes the argumentative burden from "overturn a ruling"
to "discharge a stated precondition" — see §3.1.

**0.3 — Java does NOT have two row-limit options; it has one, and it is a page
size.** The brief asks this RFC to "map the two options exactly — both exist in
Java". They do not. Java's entire row-limit surface is:

| Java option | Meaning | Wiring |
|---|---|---|
| `MAX_ROWS` | "the maximum number of records to return **before prompting for continuation**" (`Options.java:58-63`) — a per-execution PAGE SIZE. Default `Integer.MAX_VALUE` (`Options.java:280`), range `[0, Integer.MAX_VALUE]` (`Options.java:532`) | `ExecuteProperties.setReturnedRowLimit` (`QueryPlan.java:434`) |
| `EXECUTION_SCANNED_ROWS_LIMIT` | a per-TRANSACTION **scan** limit; hitting it throws `SCAN_LIMIT_REACHED` (`Options.java:187-192`) | `ExecuteProperties` scan limiter |

There is **no statement-wide returned-row total anywhere in Java.** Go's
statement-wide `OptMaxRows` is therefore not one of two Java options that Go
implemented one of; it is a Go-only semantics with no Java counterpart, sitting
on Java's option NAME. §1.1 shows how that happened and §4.2 retires it.

**0.4 — The "16 continuation corpus files" is a count of `maxRows`-carrying
files, not of files mentioning continuations, and closing this gap will NOT
move 16 files into `pass`.** Measured over the vendored corpus at
`third_party/apple/fdb-record-layer/yaml-tests/src/test/resources/`:

```
$ grep -rl "maxRows" third_party/.../resources/ | wc -l
16
$ grep -rli "continuation" third_party/.../resources/ | wc -l
14
```

Two different sets (intersection 9). The 16 is the right number and it matches
RFC-201's "+16" (`rfcs/201-layered-test-corpus.md:159-162`). But the ledger
books only **one** file-level `unsupported:continuation` skip against **sixteen**
query-level ones (`pinned_ledger_test.go`, `pinnedLedger`) — the other fifteen
`maxRows` files are claimed FIRST by a higher-priority class (struct DDL,
prepared, temporary-function, …). Predicting "16 files move to pass" would be
arithmetic across two denominators, the exact error RFC-200 revision 1 was NAK'd
for. §7 denominates the gate in what is actually measurable.

---

## 1. The defect, measured — four absences and the causal link between them

Each absence is stated with its instrument. Three of the four are *symptoms of
one cause*, and §1.5 draws that link, because a design that treats them as four
independent items will fix them four incompatible ways.

### 1.1 (A) `OptMaxRows` is a statement-wide total that ends in a silent `io.EOF`

`pkg/relational/core/embedded/cascades_generator.go:1465-1472`, inside
`paginatingRows.Next`:

```go
	// MAX_ROWS statement-wide cap (RFC-106a §3): a TOTAL returned-row
	// budget across ALL pages. math.MaxInt32 (the option default) is
	// effectively unlimited. A clean stop (io.EOF), not an error — JDBC
	// setMaxRows semantics. SQL LIMIT is no longer applied here; it is the
	// RecordQueryLimitPlan operator's job inside the plan (RFC-128).
	if r.maxRows > 0 && r.emitted >= r.maxRows {
		return io.EOF
	}
```

The termination is **silent**: a bare `io.EOF`, which `database/sql` turns into
`Rows.Next() == false` with `Rows.Err() == nil` — indistinguishable from natural
exhaustion. `r.continuation` (`:1317`), which at that instant may hold a live
resume position, is dropped on `Close`. The sibling byte cap twenty lines below
does the opposite and signals (`api.ErrCodeExecutionLimitReached`,
`:1487-1489`); the row cap does not. The field comment states the semantics
outright (`:1333-1337`), and `TestFDB_RFC106a_MaxRowsStatementWide`
(`pkg/relational/sqldriver/resource_limits_fdb_test.go:158-188`) pins them with
`perPage = 4` chosen so "a per-page misread would over/under-count".

The option is read at exactly one place (`:1252`,
`optInt64(c.Options(), api.OptMaxRows, math.MaxInt32)`), declared at
`pkg/relational/api/options.go:42`, defaulted at `:175`.

**Java's behaviour, and the executable proof.** `maxRows.yamsql` is not prose —
it is an assertion Java's suite runs:

```yaml
      # maxRows value 2
      - query: select ta.* from ta;
      - maxRows: 2
      - result: [{1, 2}, {3, 4}]
      - result: [{5, 6}, {7, 8}]
      - result: [{9, 10}, {11, 12}]
      - result: [{13, 14}, {15, 16}]
      - result: [{17, 18}, {19, 20}]
      - result: [] # Even multiple, it takes one more to know it is exhausted
```

Ten rows, `maxRows: 2`, **six pages**. Under Go's semantics the query returns two
rows and stops. This file cannot pass against Go's `OptMaxRows` under any
runner; it is the corpus-level falsification of the current semantics, and it is
one of the sixteen.

**RFC-106a's reasoning, and why it is refuted.**
`rfcs/106-statement-resource-limits.md:47-50`:

> `OptMaxRows` → a **STATEMENT-WIDE** returned-row cap (codex P2): because
> `paginatingRows` auto-follows continuations, wiring it to per-page
> `ReturnedRowLimit` would make it a page size. Instead track a remaining-row
> budget across `paginatingRows` pages and stop after `MAX_ROWS` rows are
> emitted — Java's JDBC `setMaxRows` semantics (total, not per-page).

Both halves fail.

1. **"Java's JDBC `setMaxRows` semantics (total, not per-page)" is false for
   fdb-relational.** Java deliberately gives JDBC's `setMaxRows` a *per-page*
   meaning on this driver and says so in the option's own doc — "the maximum
   number of records to return before prompting for continuation. **This can
   also be set via JDBC's setMaxRows**" (`Options.java:58-62`). RFC-106a applied
   the generic JDBC contract instead of reading fdb-relational's, and the yaml
   suite drives it through `Statement.setMaxRows` per page
   (`QueryExecutor.java:311`, `:279-280`).
2. **"wiring it to per-page `ReturnedRowLimit` would make it a page size"** is a
   correct statement of consequence attached to an inverted goal. A page size is
   the target, not the hazard.

**The root cause is (B) and (D), not a reading error.** `paginatingRows`
auto-follows continuations *inside one `Execute`* because Go has no way to hand
a page boundary back to a caller. With no observable page, a per-page limit is
unobservable, so the option was re-pointed at the only observable quantity — the
total. The semantics inverted because the surface was missing. This is why §4.2
cannot land before §4.4.

**A second, unpinned consequence worth recording:** `OptMaxRows` has **no
production setter at all.** `SetOptions` has three callers repo-wide
(`connection.go:122` is the definition; `conformance/rowdiff/run.go:147` and
`simfdb/hunt/sqlpage/sqlpage.go:156` set only scan limits), DSN options are kept
as a raw `map[string]string` and never mapped into `api.Options`
(`sqldriver/dsn.go:48-49`), and there is no SQL `SET MAX_ROWS`. The only reach
today is `conn.Raw` from Go code. So the wrong semantics is currently
unreachable by a SQL user — which is why it survived green.

### 1.2 (B) `api.ResultSet.Continuation()` has zero production callers

The interface is declared at `pkg/relational/api/resultset.go:42-44`, and
`api.Continuation` at `pkg/relational/api/continuation.go:8-21` is already a
faithful mirror of Java's — `Serialize()`, `ExecutionState()`, `Reason()`, with
`ContinuationReason`'s iota order matching Java's enum ordinals and its
`String()` returning the Java enum names (`continuation.go:26-56`). The API
shape is not the problem.

The problem is that nothing reaches it. `grep -rn "\.Continuation()"` over the
repo returns exactly one hit, in a test
(`pkg/recordlayer/query/executor/resultset_test.go:506`). `RecordLayerResultSet`
*is* constructed on the production path (`cascades_generator.go:1813`), but the
page drain calls the record-layer accessor and bypasses the api one entirely
(`:1831`, `pageContinuationState(rs.GetContinuation(), rs.GetNoNextReason())`).
So `liveContinuation` / `exhaustedContinuation`
(`executor/resultset.go:423`, `:619`) are constructed by nothing live.

The bytes themselves are `continuation []byte`, an unexported field on the
unexported `paginatingRows` in `package embedded` (`cascades_generator.go:1317`),
written at `:1845`, read at `:1808`. No getter, no exported wrapper.

**And there is no plumbing even if it were exported.** `paginatingRows`
implements `driver.Rows` plus the `RowsColumnType*` optionals (`:1374-1450`).
The `sqldriver` package declares no custom interface at all. `database/sql`
offers no `Rows.Raw()`; `conn.Raw` reaches the *Conn*, never the driver `Rows`
behind a `*sql.Rows`. Surfacing the token needs a new interface **and** a new
accessor. §4.4.

### 1.3 (C) There is no envelope

Everything named `*Continuation` in `gen/` is a record-layer cursor-internal
message mirroring Java's `record_cursor.proto` — `KeyValueCursorContinuation`,
`UnionContinuation`, `AggregateCursorContinuation`, and eighteen others. Not one
carries `version`, `reason`, `binding_hash`, `plan_hash`, or a statement
reference. There is no `continuation.proto` in `proto/`.

The one construct that resembles an envelope is Go-private and explicitly
disclaims the role, `pkg/recordlayer/query/executor/executor.go:1606-1612`:

```go
// LimitContinuation is the RFC-128 §3.3 envelope for a RecordQueryLimitPlan's
// continuation: ... It is Go-only and INTERNAL to executeLimit — it never
// becomes a SQL resume token or a wire/Java-interop continuation (no .proto, no
// magic-6773487359078157740 surface).
```

The absence is a *recorded decision*, not an oversight — `DIVERGENCES.md:1161`
ff., quoted in §3.1 — enforced loudly at `cascades_generator.go:1215-1218` and
pinned by `TestOptContinuation_RejectsLoudly`
(`pkg/relational/core/embedded/continuation_option_test.go:18-36`).

### 1.4 (D) The grammar parses `EXECUTE CONTINUATION`; nothing implements it

`pkg/relational/core/parser/grammar/RelationalParser.g4:683-686`:

```antlr
executeContinuationStatement
    : EXECUTE CONTINUATION packageBytes=continuationAtom
      queryOptions?
    ;
```

with `continuationAtom : bytesLiteral | preparedStatementParameter` (`:389-392`),
`bytesLiteral : HEXADECIMAL_LITERAL | BASE64_LITERAL` (`:814-815`), reachable
from `administrationStatement` (`:73-81`) and from `describeObjectClause`
(`:735`, i.e. `EXPLAIN EXECUTE CONTINUATION …`). The lexer has both tokens
(`RelationalLexer.g4:234`, `:782`). `EXECUTE CONTINUATION x'0a0b'` lexes and
parses cleanly.

Every `VisitExecuteContinuationStatement` occurrence in the repo is inside the
generated `pkg/relational/core/parser/gen/` package — zero overrides. The
dispatcher `cascadesGenerator.planOne` (`cascades_generator.go:145`) never
descends into it, so an `EXECUTE CONTINUATION` falls through
`admin.ShowStatement() == nil` to `:182-183`:

```go
		return nil, api.NewError(api.ErrCodeUnsupportedOperation,
			"only SHOW administration statements are supported")
```

The user gets a plan-time error that never mentions continuations, and
`packageBytes` is discarded unread. There are **zero** tests anywhere containing
the string "EXECUTE CONTINUATION", so the behaviour is unpinned in either
direction.

### 1.5 The link: (A) is a symptom of (B)+(D)

(C) and (D) are the missing mechanism. (B) is the missing surface. (A) is what
happened to a Java option name when the mechanism and the surface were both
absent: the only way to make `MAX_ROWS` do *anything* observable without a page
boundary was to redefine it as a total. Fixing (A) alone would produce a page
size nobody can observe; fixing (B) alone would surface a token nobody can
redeem. The four are one change, and §6 sequences them accordingly.

---

## 2. Java is the spec — the whole mechanism, and the layering

### 2.1 The envelope

`fdb-relational-core/src/main/proto/continuation.proto:31-54`:

```proto
message ContinuationProto {
  enum Reason {
    USER_REQUESTED_CONTINUATION = 0;
    TRANSACTION_LIMIT_REACHED = 1;
    QUERY_EXECUTION_LIMIT_REACHED = 2;
    CURSOR_AFTER_LAST = 3;
  }
  optional int32 version = 1;
  optional bytes execution_state = 2;      // the underlying (cursor) state
  optional int32 binding_hash = 3;
  optional int32 plan_hash = 4;            // "to be deprecated"
  optional CompiledStatement compiled_statement = 5;
  optional Reason reason = 6;
  oneof other_plan { CopyPlan copyPlan = 7; }
}
```

`CompiledStatement` (`:85-98`) carries `plan_serialization_mode` (string),
`plan` (`PRecordQueryPlan`), `extracted_literals`, `arguments`,
`plan_constraint` (`PQueryPlanConstraint`) and `queryMetadata` (`PType`).

`ContinuationImpl` (`recordlayer/ContinuationImpl.java`) writes
`version = CURRENT_VERSION = 1` (`:44`, `:55`) and encodes the two boundary
states **in the presence of `execution_state`, not in a flag**: `BEGIN` is
`execution_state` absent, `END` is `execution_state` present and length 0
(`:46-48`, and `Continuation.java:66-73`):

```java
    default boolean atBeginning() { return getExecutionState() == null; }
    default boolean atEnd() {
        byte[] bytes = getExecutionState();
        return bytes != null && bytes.length == 0;
    }
```

**Java writes `version` and never validates it.** The only read is the EXPLAIN
display struct (`QueryPlan.java:359`). `version` is therefore not a gate, and
§4.1 does not make it one.

### 2.2 Producing a token — `enrichContinuation`

`QueryPlan.java:433-436` sets the page size and `:443-448` installs the
enricher; `:451-493` is the enricher itself:

```java
            final var continuationBuilder =  ContinuationImpl.copyOf(continuation).asBuilder()
                    .withBindingHash(queryExecutionContext.getParameterHash())
                    .withPlanHash(planHashSupplier.get())
                    .withReason(reason);
            // Do not send the serialized plan unless we can continue with this continuation.
            if (!continuation.atEnd()) {
                ...
                compiledStatementBuilder.setPlan(recordQueryPlan.toRecordQueryPlanProto(serializationContext));
```

Two properties to carry across: the hashes and reason are set **always**; the
compiled statement is set **only when not at end**. And the serialization order
is load-bearing — plan, then arguments in ordinal order, then constraints —
because the type-repository dictionary compression depends on it (`:461-467`,
restated at `PlanGenerator.java:324-330`).

The reason is computed by the result set, not the plan
(`RecordLayerResultSet.java:121-135`):

```java
    private Continuation.Reason continuationReason() {
        if (currentRow != null) { return Reason.USER_REQUESTED_CONTINUATION; }
        if (currentCursor.hasNext()) { return Reason.USER_REQUESTED_CONTINUATION; }
        if (currentCursor.terminatedEarly()) { return Reason.TRANSACTION_LIMIT_REACHED; }
        else if (currentCursor.getNoNextReason().equals(RecordCursor.NoNextReason.RETURN_LIMIT_REACHED)) {
            return Reason.QUERY_EXECUTION_LIMIT_REACHED;
        } else { return Reason.CURSOR_AFTER_LAST; }
    }
```

and `getContinuation()` throws if the result set is not exhausted (`:110-113`).

### 2.3 Consuming a token — two paths, not one

This is the single most important structural fact for §3 and §4.3. Java has
**two** resume mechanisms with different gates.

**Path 1 — `EXECUTE CONTINUATION` (self-describing).** `AstNormalizer` flags the
statement and marks it uncacheable in the same breath (`:465-472`):

```java
    public Object visitExecuteContinuationStatement(...) {
        queryCachingFlags.add(QueryCachingFlags.IS_EXECUTE_CONTINUATION_STATEMENT);
        queryCachingFlags.add(QueryCachingFlags.WITH_NO_CACHE_OPTION);
```

`PlanGenerator.generatePhysicalPlan` branches on the flag (`:228-234`), and
`generatePhysicalPlanForExecuteContinuation` (`:272-294`) demands a payload:

```java
        if (continuation.hasCompiledStatement()) { ... }
        else if (continuation.hasCopyPlan()) { ... }
        else {
            throw new RelationalException("Continuation does not have statement to continue",
                    ErrorCode.INTERNAL_ERROR);
        }
```

**There is no re-plan-from-SQL-text path here, because there is no SQL text** —
`EXECUTE CONTINUATION x'…'` names no query. `generatePhysicalPlanForCompiledStatementContinuation`
(`:313-408`) then deserializes the plan out of the blob
(`RecordQueryPlan.fromRecordQueryPlanProto`, `:334-335`) and checks the blob
against itself:

```java
        if (!Objects.requireNonNull(continuation.getPlanHash()).equals(recordQueryPlan.planHash(serializedPlanHashMode))) {
            throw new PlanValidator.PlanValidationException("cannot continue query due to mismatch between serialized and actual plan hash");
        }
```

Note what that gate *is*: an integrity check that the recorded hash matches the
plan that came out of the same blob. It is **not** a check against a freshly
planned plan. The schema-change gate on this path is the plan **constraint**
(`ContinuedPhysicalQueryPlan.validatePlanAgainstEnvironment`, `:552-573`), not
the plan hash — §4.7.

**Path 2 — a continuation supplied to a normally-planned statement.** Here the
query IS planned (or served from the plan cache) and the continuation is
validated against the result: `PhysicalQueryPlan.validatePlanAgainstEnvironment`
(`:279-293`) → `PlanValidator.validateHashes` (`PlanValidator.java:72-96`), which
throws `PlanValidationException` (`ErrorCode.INVALID_CONTINUATION`, `:131-140`)
on either a binding-hash mismatch ("Continuation binding does not match query")
or an unresolvable plan hash ("Continuation plan does not match query"), and
pushes `CONTINUATION_DOWN_LEVEL` when the resolved mode is not the current one.
This path is reached through the Direct Access `Options.Name.CONTINUATION`, not
through SQL.

Path 2 is the one whose gate semantics Go can implement in full. Path 1's
*mechanism* (deserialize a foreign plan) is what §3 declines.

### 2.4 The two hashes, and how portable they actually are

**`binding_hash` is portable.** `AstNormalizer.java:153-154`:

```java
        parameterHash = Hashing.murmur3_32_fixed().newHasher().putInt("ParameterHash".hashCode());
        parameterHashSupplier = Suppliers.memoize(() -> parameterHash.hash().asInt())::get;
```

fed per literal at `:546-556`:

```java
            final String canonicalName = parameterName == null ? "?" : "?" + parameterName;
            sqlCanonicalizer.append(canonicalName).append(" ");
            parameterHash.putInt(Objects.hash(canonicalName, literal));
```

Every ingredient is specified, not implementation-defined: `String.hashCode` is
fixed by the JLS, `Objects.hash` is `31`-folding over element `hashCode`, the
boxed scalar `hashCode`s (`Integer`, `Long`, `Double`, `Boolean`) are fixed by
their Javadoc, and murmur3-32 is a portable algorithm. A Go port is mechanical.
(The one hole: if `literal` is ever an array or an identity-`hashCode` object,
the hash is unstable across *Java's own* JVM runs. Go must reproduce the
specified cases and reject the unspecified ones loudly rather than guess.)

**`plan_hash` is not.** `PlanHashable.objectPlanHash` (`:242-269`) bottoms out at

```java
        return obj.hashCode();
```

for anything that is not an `Enum`, `Set`, `Map`, `Iterable`, array, or
`PlanHashable`, and folds with `31 * result + …` (`:282-288`), producing a Java
`int`. Go's equivalent is a different algorithm at a different width over
different inputs — `pkg/recordlayer/query/plan/plans/plan_hash.go:19-40`, FNV-64a
over `HashCodeWithoutChildren()`, whose own doc comment says:

```go
// In-memory only: the plan cache is keyed by
// normalized SQL text, and no continuation or wire artifact embeds this hash,
// so its value may change across releases (RFC-176 P2 did) without
// compatibility impact.
```

Agreement would require porting Java's per-node `planHash` contribution across
the whole plan vocabulary. §3.2 explains why that is necessary but not
sufficient.

### 2.5 The layering — where the record-layer magic sits

The SQL envelope **wraps** the record-layer continuation; it never replaces it
and never re-frames it.

```
  SQL resume token  =  ContinuationProto            <- this RFC adds
                         .version                      (int32, written, not gated)
                         .reason                       (enum, Java ordinals)
                         .binding_hash / .plan_hash    (identity gates)
                         .compiled_statement           (statement identity)
                         .execution_state  = ───┐
                                                │  opaque bytes, unmodified
  record-layer cursor continuation  <───────────┘   (already Java-compatible)
     = RecordCursorContinuation.toBytes()
       … structural cursors (union / intersection / flat-map) …
         leaf KeyValueCursorContinuation
           { inner_continuation, magic_number = 6773487359078157740 }
             pkg/recordlayer/key_value_cursor.go:15-18, :41-45
               = an FDB key suffix
```

Java draws exactly this line: `ContinuationImpl.fromRecordCursorContinuation`
(`ContinuationImpl.java:185-187`) takes `cursorContinuation.toBytes()` and puts
it in `execution_state` without inspecting it, and
`QueryPlan.executePhysicalPlan` hands `parsedContinuation.getExecutionState()`
straight back to `recordQueryPlan.executePlan` (`:439`). The magic number is
`KeyValueCursorBase`'s framing discriminator for a **leaf scan**; it is not, and
must not become, a SQL-envelope marker. The envelope's own discriminator is that
it is a valid `ContinuationProto` — §4.1.

`DIVERGENCES.md:1173-1189` records that this layering is **not** uniformly
byte-compatible today: Go's `AggregateCursorContinuation` reuses Java's message
schema with a Go-private slot layout, and `MemorySortContinuation` has no Java
counterpart at all. Those are inside `execution_state`, and they are the reason
§3 does not get to wave at "the inner framing is byte-identical".

---

## 3. THE CENTRAL DECISION — cross-engine resume

### 3.1 What is being reversed, precisely

`DIVERGENCES.md:1161-1206`, the operative paragraphs:

> Java's fdb-relational wraps SQL continuations in a ContinuationProto envelope
> (version + plan_hash + binding_hash + execution_state) and gates resumes
> through PlanValidator. Go has no envelope and NO SQL resume entry point at
> all — statement paging is internal to one execution (`paginatingRows`), and
> tokens never cross the API boundary.
> […]
> Decision: Go SQL tokens are ENGINE-PRIVATE until a real resume surface
> exists. The boundary is loud, not silent: supplying api.OptContinuation
> fails the statement with ErrCodeUnsupportedOperation (cascadesPlan.Execute;
> pinned by TestOptContinuation_RejectsLoudly) — never silently ignored, which
> would replay from row 1 while the caller believes they resumed. **Adopting
> the ContinuationProto envelope + PlanValidator hashes is the follow-up arc
> if/when a resume surface ships; hash values would deliberately differ per
> engine so cross-engine resume attempts REJECT loudly in both directions.**

Read carefully, that decision is **conditional**, and this RFC discharges its
condition rather than overturning its judgement. Splitting it precisely:

| Clause | Status under RFC-203 |
|---|---|
| "Go SQL tokens are ENGINE-PRIVATE **until a real resume surface exists**" | **Condition discharged.** The resume surface ships here (§4.3, §4.4). Tokens now cross the API boundary. |
| "supplying `api.OptContinuation` fails … `ErrCodeUnsupportedOperation`" | **REVERSED.** The guard at `cascades_generator.go:1215-1218` is deleted; `TestOptContinuation_RejectsLoudly` is rewritten, not merely retargeted (§7 G5). |
| "Adopting the ContinuationProto envelope + PlanValidator hashes is the follow-up arc" | **This is that arc.** Adopted verbatim (§4.1). |
| "hash values would deliberately differ per engine so cross-engine resume attempts REJECT loudly in both directions" | **UPHELD, and made concrete.** §3.3 turns "would deliberately differ" from an incidental property into an enforced, tested fence. |
| RFC-181's non-goal "Wire-format changes (none required by any finding)" (`rfcs/181:343-344`) | **REVERSED.** A new wire artifact is the deliverable. |

**The ground that shifted** is named by RFC-201 and CQ-69: the corpus arrived,
and with it two consumers that did not exist when C2 was decided. Sixteen corpus
files carry `maxRows` and cannot execute without a per-page surface
(`TODO.md:12552-12556`), and the `ForceContinuations` oracle — which re-executes
every SELECT at `maxRows=1` and asserts the reassembled pages equal the one-shot
result — is gated behind it (`rfcs/201:192-199`, `TODO.md:12564-12575`). C2's
"tokens never cross the API boundary" was a true statement about a system with
no consumers. It has consumers now.

### 3.2 The decision

> **Adopt `ContinuationProto` verbatim — field numbers, field semantics, enum
> values, and the `execution_state` / `atBeginning` / `atEnd` encoding. Target
> SAME-ENGINE resume. DECLINE cross-engine *plan* resume: neither engine will
> execute a plan that came out of the other's token. The decline is enforced by
> a value-scoped `plan_serialization_mode`, is loud in both directions, and its
> re-arm conditions are stated in §3.4 as measurable gates.**

Nothing Go-only is added to the proto. Not a field, not an extension range, not
a Go-side sibling message. **The scoping is by VALUE, never by SCHEMA** — because
a schema divergence is permanent, and a value divergence is a gate that can be
opened later without a wire migration.

Concretely: Go writes `CompiledStatement.plan_serialization_mode = "GO_V0"`, a
string that is not a member of Java's `PlanHashable.PlanHashMode` (`VL0`, `VC0`,
`VC1` — `PlanHashable.java:68-78`). A Go token handed to Java therefore dies at
`PlanValidator.validateSerializedPlanSerializationMode`
(`PlanValidator.java:52-60`) — Java's own existing gate, using Java's own
existing error path, with no Java change required. Symmetrically, Go rejects any
`plan_serialization_mode` it did not write with `INVALID_CONTINUATION` and a
message naming cross-engine resume.

*A precision, because over-claiming here would be the worst kind of error:*
Java's rejection arrives via `PlanHashMode.valueOf(...)` throwing
`IllegalArgumentException` **before** the `validPlanHashModes.contains` check
(`PlanValidator.java:54-56`). That is loud — an exception, never wrong rows —
but it is an unchecked exception rather than a clean `INVALID_CONTINUATION`.
This is INFERRED from reading Java at 4.12.11.0; it is not measured, because
this repo cannot execute Java. §7 G6 makes measuring it a gate rather than an
assumption, and if it measures otherwise the fence moves to a mechanism that
does hold.

### 3.3 The evidence — three legs, each measured

**Leg 1 — Java's SQL resume path structurally requires the peer's serialized
physical plan.** `EXECUTE CONTINUATION` carries no query text, so the token must
be self-describing; a token with neither `compiled_statement` nor `copy_plan` is
an error by construction (`PlanGenerator.java:290-293`). Cross-engine resume is
therefore not "make the hashes agree" — it is "each engine must execute the
other's physical plan."

**Leg 2 — Go writes no `PRecordQueryPlan`, and Go's plan vocabulary is not
losslessly representable in the shared proto.** The first half is asserted by an
existing test's own doc comment,
`pkg/docscheck/plan_proto_schema_test.go:17-18`:

```go
// This is a descriptor-reflection guard, NOT a stored-bytes test (Go marshals no plan through these
// protos — see RFC-135 §3).
```

corroborated by RFC-135 §3 (`rfcs/135-upgrade-java-4.12.11.0.md:77-79`) and by
grep: every non-test reference to `PRecordQueryPlan` in the repo is in that one
file. The proto itself is fully vendored and generated (2,388 lines,
`proto/apple/record_query_plan.proto`; `gen/record_query_plan.pb.go`), so this
is a missing *serializer*, not a missing schema — and a missing serializer is a
thing to build, not a wall.

The second half is the structural obstacle, and it is not an effort estimate.
Go's `RecordQueryInMemorySortPlan` is a sanctioned read-side plan (CLAUDE.md)
that fires for any `ORDER BY` no access path satisfies. Its sort key is
`pkg/recordlayer/query/plan/plans/in_memory_sort.go:25-30`:

```go
type SortKey struct {
	Field      string
	Desc       bool
	NullsFirst bool
	ValueExpr  values.Value // REQUIRED: the plan-time-baked key Value, evaluated per row
}
```

The only sort message in the shared proto is
`proto/apple/record_query_plan.proto:2188-2196`:

```proto
message PRecordQuerySortPlan {
  optional PPhysicalQuantifier inner = 1;
  optional PRecordQuerySortKey key = 2;
}
message PRecordQuerySortKey {
  optional com.apple.foundationdb.record.expressions.KeyExpression key = 1;
  optional bool reverse = 2;
}
```

A **`KeyExpression`**, and a single **`reverse`** bit. No `Value`, no
per-key direction, no `nulls_first`. A Value-keyed, per-key-directed,
NULLS-ordered sort is **not encodable** — not "hard to encode". Any Go token
whose plan contains an in-memory sort is unrepresentable in the shared
vocabulary, and that is a large share of corpus `ORDER BY` queries. (Java is not
symmetric here: `RecordQuerySortPlan` has a real deserializer,
`sorting/RecordQuerySortPlan.java:250-271` — but Java's Cascades never
constructs it, so the message exists for the legacy planner and does not
represent an equivalent operator.)

**Leg 3 — plan-hash agreement would be a stronger property than Java gives
itself.** Java does not guarantee plan-hash stability across its own versions:
that is the entire reason `PlanHashMode` is versioned (`VL0` / `VC0` / `VC1`,
`PlanHashable.java:68-78`), that `VALID_PLAN_HASH_MODES` is a connection option
(`Options.java:222`), that `resolveValidPlanHashMode` sweeps the accepted set
(`PlanValidator.java:113-129`), and that `CONTINUATION_DOWN_LEVEL` exists as a
metric (`:90-92`). Asking two independent implementations to agree on a hash
that one implementation cannot hold stable across its own releases is asking
for more than the design offers. On top of that, `execution_state` is not
uniformly compatible either — `DIVERGENCES.md:1173-1189` records Go's
Go-private aggregate slot layout under Java's message schema, which Go now
rejects loudly on a foreign shape
(`TestDecodeAggregateContinuation_ForeignShapeFailsLoud`).

### 3.4 Re-arm conditions — what would make cross-engine resume achievable

Stated as measurable gates so this is a fence with a gate in it, not a fence
with a wish behind it. Cross-engine plan resume is reconsidered when **all
three** hold, each demonstrated by a committed test:

- **R1 (vocabulary).** A bijection exists between Go's physical plan node set
  and `PRecordQueryPlan`'s oneof arms for every plan the corpus produces —
  measured by an enumeration test over the corpus's planned trees reporting
  zero unrepresentable nodes. Today this is falsified by
  `RecordQueryInMemorySortPlan` alone (leg 2); closing it means either Java
  gains a Value-keyed sort message upstream or Go's ordering coverage removes
  the operator from corpus plans. Both are real paths; neither is free.
- **R2 (hash).** Go's plan hash is a port of `PlanHashable` under a shared
  `PlanHashMode`, replacing `plans.PlanHash`'s FNV-64a, with a golden vector
  per plan class cross-checked against Java's computed values through the
  conformance server.
- **R3 (execution state).** Every `execution_state` payload Go can emit is
  byte-identical to Java's for the same cursor position, retiring the
  aggregate and memory-sort exceptions at `DIVERGENCES.md:1173-1189`.

Until then the fence stands and is tested. **Note that the envelope adopted here
already delivers partial cross-engine value without R1-R3:** because the
structure is Java-identical, a Java client can parse a Go token and read its
`version`, `reason`, and `execution_state`, and vice versa. What is fenced is
*executing the peer's plan*, not *reading the peer's envelope*.

---

## 4. The design

### 4.1 The proto

New file `proto/relational/continuation.proto`, a verbatim structural copy of
Java's `fdb-relational-core/src/main/proto/continuation.proto` with the Go
package options, vendored under the same rules as `proto/apple/` (NOTICE clause,
version pin). It imports `record_metadata.proto`, `record_query_plan.proto` and
`record_query_runtime.proto`, all three of which are already vendored and
generated.

`CopyPlan` (`other_plan`, tag 7) is copied so the tag is permanently reserved
and a Java `COPY` token is *parsed and rejected by name* rather than silently
read as a statement-less continuation. Go does not implement `COPY`.

What Go writes into each field:

| Field | Go value |
|---|---|
| `version` | `1`, matching `ContinuationImpl.CURRENT_VERSION`. Written, **not gated** — because Java does not gate it either (§2.1), and a gate Java lacks would reject Java tokens for the wrong reason. |
| `execution_state` | the record-layer continuation bytes, unmodified. Absent ⇒ BEGIN, present-and-empty ⇒ END, exactly as `Continuation.java:66-73`. |
| `binding_hash` | a port of `AstNormalizer`'s murmur3-32 (§2.4) over Go's literal-stripping normalizer. Java-identical by construction and cross-checked (§7 G7). |
| `plan_hash` | Go's plan hash under mode `GO_V0`. Engine-scoped by §3.2. |
| `compiled_statement.plan_serialization_mode` | `"GO_V0"` — the fence. |
| `compiled_statement.plan` | **not set.** See §4.3: Go does not serialize a physical plan. |
| `compiled_statement.extracted_literals` / `arguments` | the ordinal literals table, in ordinal order, using Java's `TypedQueryArgument` / `LiteralObject` shape. |
| `compiled_statement.plan_constraint` | Go's continuation plan constraint as `PQueryPlanConstraint`. |
| `compiled_statement.queryMetadata` | the result `PType`, so a resumed page reports identical column metadata — which is what the two `check-result-metadata/*-continuation-page.yamsql` corpus files assert. |
| `reason` | §4.5. |

The set-only-when-not-at-end rule for `compiled_statement` is copied exactly
(`QueryPlan.java:459-460`), as is the serialization ORDER (plan, then arguments
in ordinal order, then constraints — `QueryPlan.java:461-467`), because the
type-repository dictionary compression depends on it.

**Why `plan` stays unset rather than carrying a Go-private encoding.** Putting
non-`PRecordQueryPlan` bytes into a field typed `PRecordQueryPlan` is a lie on
the wire that a Java parser would attempt to decode. An unset optional field is
honest and is exactly what Java's own `getSerializedPlanFromContinuation`
already tolerates (`QueryPlan.java:298-301` returns empty when the plan or the
mode is absent). Go's statement identity travels in the literals table, the
constraint, the metadata and the two hashes — which is sufficient because of
§4.3.

### 4.2 `MAX_ROWS` becomes a per-execution page size; the statement-wide total is retired

`OptMaxRows` takes Java's meaning: the returned-row limit for ONE execution.
`paginatingRows` stops at the page boundary and yields a continuation instead of
transparently fetching the next page.

**The statement-wide total is deleted, not renamed.** There is no Java option to
map it to (§0.3), and a Go-only row-total option would be a second row-limit
knob whose interaction with the page size is undefined in the reference. Callers
who want N rows total write `LIMIT N`, which is a real operator in the plan
(`RecordQueryLimitPlan`, RFC-128) rather than an egress truncation — and which
is what a SQL user means. `TestFDB_RFC106a_MaxRowsStatementWide` is **deleted
and replaced** by its inverse (§7 G1); leaving it renamed would leave a test
asserting refuted semantics.

Retained, and untouched: `OptExecutionScannedRowsLimit`,
`OptExecutionScannedBytesLimit`, `OptExecutionTimeLimit` are per-transaction
SCAN limits mapping to Java's `EXECUTION_SCANNED_*` (§0.3), and the Go-local
statement timeout and result-byte cap (RFC-106a §4-5) are unaffected — they are
Go extensions with no Java option name, which is why they survive a decision
that retires a Go semantics sitting on a *Java* name.

Two consequences to implement rather than discover:

- **`pageRowBudget`** (`cascades_generator.go:1606-1624`) currently derives the
  scan-bounding hint from `rowCap - r.emitted`, the remaining TOTAL. It becomes
  the page size directly.
- **The trailing empty page is real and must be preserved.** `maxRows.yamsql`
  asserts it — "Even multiple, it takes one more to know it is exhausted" — and
  Java's oracle asserts it as an invariant ("End result should not have any
  associated value when maxRows is 1", `QueryExecutor.java:347`). A page that
  returns zero rows with an at-end continuation is correct behaviour, not an
  off-by-one to smooth away.
- **Validation.** `maxRows: -27525 → INVALID_PARAMETER` is a corpus assertion.
  Go must range-check `[0, math.MaxInt32]` per `Options.java:532` and raise the
  matching error class.

### 4.3 `EXECUTE CONTINUATION` through Cascades — one plan producer, no bypass

The tension: Java implements `EXECUTE CONTINUATION` by *bypassing the planner*
and deserializing a plan; CLAUDE.md forbids a second plan pipeline. §3 already
established Go cannot deserialize a plan. The resolution is not a compromise —
it is that **Java has both paths and Go implements the one that is
Cascades-native** (§2.3).

`EXECUTE CONTINUATION x'…'` in Go:

1. Parse (already works), take `packageBytes`, `proto.Unmarshal` into
   `ContinuationProto`. A parse failure is `INVALID_CONTINUATION`.
2. Reject a BEGIN token (`execution_state` absent) and an END token
   (present-and-empty) with `INVALID_CONTINUATION`, matching
   `AstNormalizer.visitContinuationAtom`'s two assertions — "Illegal query with
   BEGIN continuation." / "Illegal query with END continuation."
   (`AstNormalizer.java:333-334`).
3. Reject a foreign `plan_serialization_mode` (§3.2) and a `copyPlan` token.
4. Rebuild the literals table from `extracted_literals` + `arguments` into the
   ordinal table, exactly as `PlanGenerator.java:349-368` does.
5. **Re-plan through Cascades** from the canonical query the literals table
   carries, in the ordinary way, with the ordinary plan cache.
6. **Gate on identity**, which is `PlanValidator.validateHashes`'
   semantics (`PlanValidator.java:72-96`) applied to the freshly planned plan:
   binding hash mismatch → `INVALID_CONTINUATION` "Continuation binding does not
   match query"; plan hash mismatch → `INVALID_CONTINUATION` "Continuation plan
   does not match query". Same error code, same wording, per the conformance
   principle.
7. Additionally evaluate the continuation plan **constraint** against the store
   and reject on failure — §4.7.
8. Execute the re-planned plan from `execution_state` at the requested page
   size.

This satisfies "no parallel pipelines" exactly: there is one plan producer
(Cascades), and the continuation supplies a *position and an identity gate*, not
a plan. It is strictly safer than Java's Path 1, because Java's Path 1 plan-hash
gate only checks the blob against itself (§2.3) whereas this checks the recorded
identity against what the engine would plan *now*.

**The cost, stated plainly rather than buried:** a Go token becomes invalid
whenever re-planning yields a different plan — a metadata change, an index
build, a cost-model change, a planner-rule change between the two requests. Java
survives some of those because it carries the plan. This is a real narrowing and
it is the price of not having a second plan pipeline. It is also the *correct*
narrowing under this repo's rules: a continuation that survives a planner change
by replaying a stale serialized plan is a plan the current engine has never
validated. R2 in §3.4 does not change this trade-off; only carrying the plan
would, and carrying the plan is what §3 declined.

**The canonical query text.** Step 5 needs it, and `CompiledStatement` has no
field for it. It is recovered the same way Java recovers everything else on this
path — Go's literals table is built by the same normalizer that produces the
canonical query string, so the canonicalized text is a `TypedQueryArgument`
`scope`/`parameter_name`-addressed reconstruction. **If implementation shows the
existing `TypedQueryArgument` shape cannot carry it without an added field, the
answer is not a Go-only proto field: it is to fall back to Java's actual
mechanism and reconsider R1** — recorded here so the implementation cannot
quietly grow a field.

**Cache behaviour is copied:** the statement is marked uncacheable, as Java does
in the same visitor that flags it (`AstNormalizer.java:467-468`). The re-planned
inner query still uses the plan cache normally; it is the `EXECUTE CONTINUATION`
statement itself that is not cached.

**`EXPLAIN EXECUTE CONTINUATION` is in scope** — the grammar already reaches it
(`RelationalParser.g4:735`) and Java populates a `PLAN_CONTINUATION` struct of
`(execution_state, version, plan_serialization_mode, plan_hash, complexity)`
(`QueryPlan.java:346-363`). Go emits the same struct, with `complexity` null
since no plan is deserialized.

### 4.4 Surfacing the token through `database/sql`

`database/sql` has no `Rows.Raw()`, so the token is surfaced by a
**driver-specific interface plus an accessor on the driver `Rows`**, which the
caller reaches by type assertion — the standard Go idiom for driver extensions
and the direct analogue of Java's `RelationalResultSet extends ResultSet`.

In `pkg/relational/sqldriver`:

```go
// ContinuationRows is implemented by this driver's *sql.Rows-backed result
// sets. Continuation returns the resume token for the page just consumed.
type ContinuationRows interface {
	Continuation() (api.Continuation, error)
}
```

`paginatingRows` implements it, and `api.Continuation`'s existing three methods
(`api/continuation.go:8-21`) become live for the first time. Java's precondition
is copied: `Continuation()` before the page is exhausted is an error
(`RecordLayerResultSet.java:110-113`).

`api.ResultSet.Continuation()` (`api/resultset.go:42-44`) gains its production
caller at the same time: `cascades_generator.go:1831` stops reaching past it to
`rs.GetContinuation()` and goes through the api accessor, which is what makes
`liveContinuation` / `exhaustedContinuation` live.

Reaching the driver `Rows` from a `*sql.Rows` requires the accessor to be
reachable; the mechanism is the same one already used for `conn.Raw` in
`rowdiff/run.go:142` and `sqlpage/sqlpage.go:151`, extended to the rows side.
**This is the one piece of the design whose Go-side ergonomics have no Java
analogue** — Java gets it free from interface inheritance on `ResultSet` — and
so it is the piece most likely to need a shape change during implementation. The
constraint that must not bend: the token must be reachable without the caller
importing `package embedded`.

### 4.5 Reason codes

Go's `ContinuationReason` (`api/continuation.go:26-56`) already matches Java's
enum 1:1 by ordinal and by `String()`. The proto enum numbers match those
ordinals. What is missing is the *assignment*, ported from
`RecordLayerResultSet.continuationReason()` (`:121-135`) verbatim, including its
order of tests:

1. a row was consumed on this page, or the cursor still has rows →
   `USER_REQUESTED_CONTINUATION`;
2. the cursor terminated early (out-of-band scan/byte/time limit) →
   `TRANSACTION_LIMIT_REACHED`;
3. `NoNextReason == RETURN_LIMIT_REACHED` → `QUERY_EXECUTION_LIMIT_REACHED`
   — this is the page-size stop, and it is the one that only becomes reachable
   once §4.2 lands;
4. otherwise → `CURSOR_AFTER_LAST`.

The order is load-bearing: `terminatedEarly` is tested before `RETURN_LIMIT`, so
a page that hits both reports the transaction limit.

### 4.6 The record-layer boundary

`execution_state` is opaque to the SQL layer, in both directions. The envelope
does not inspect, re-frame, version or re-wrap the record-layer continuation,
and specifically does not touch `KeyValueCursorContinuation`'s magic
(`key_value_cursor.go:15-18`). The `LimitContinuation` disclaimer at
`executor/executor.go:1606-1612` stays true and stays accurate: it remains
internal to `executeLimit`, nested *inside* `execution_state` where it already
is, and never becomes the SQL token itself.

One thing changes: `execution_state` now genuinely leaves the process. The
`DIVERGENCES.md:1173-1189` caveats about Go-private aggregate/memory-sort
payloads stop being "safe because they never cross an engine boundary" and
become "safe because the envelope fences the engine boundary" — the same safety,
resting on a different mechanism, and §7 G6 is what holds that mechanism.

### 4.7 Java's plan-constraint fail-open — a deliberate divergence

`ContinuedPhysicalQueryPlan.validatePlanAgainstEnvironment`
(`QueryPlan.java:552-573`):

```java
            try {
                PlanValidator.validateContinuationConstraint(fdbRecordStore, getContinuationConstraint());
            } catch (final PlanValidator.PlanValidationException pVE) {
                metricCollector.increment(RelationalMetric.RelationalCount.CONTINUATION_REJECTED);
            }
            ...
            metricCollector.increment(RelationalMetric.RelationalCount.CONTINUATION_ACCEPTED);
```

The exception is **caught, counted, and swallowed**; control falls through to
`CONTINUATION_ACCEPTED`. On the `EXECUTE CONTINUATION` path a continuation whose
plan constraint no longer holds — the schema-change gate — is counted as
rejected and then accepted. `validateContinuationConstraint` itself does throw
(`PlanValidator.java:62-70`), and `validateHashes` on the *other* path does
propagate (`:80-89`), so the swallow is specific to this arm.

Read at 4.12.11.0, this is a fail-open. **Go rejects.** Per CLAUDE.md, "it's an
upstream bug" is not a deferral: Go implements the constraint check as an
enforced gate raising `INVALID_CONTINUATION`, documents the divergence at the
call site, and the finding goes upstream. Copying the swallow would mean
executing a plan against metadata it was never validated against — wrong rows,
silently, which is the failure mode this whole RFC exists to prevent.

This is stated as READ, not MEASURED: this repo cannot run Java, so the
behavioural consequence is inferred from the source. §7 G8 measures it through
the conformance server, and if Java turns out to reject by another route the
divergence note is deleted rather than kept as a boast.

---

## 5. Rejected alternatives

**5.1 — Keep tokens engine-private; give the corpus runner an internal paging
hook.** The runner would drive pages through a test-only seam and the sixteen
files would pass. **Rejected:** the corpus's value is that it executes the
*shipped* surface. A hook that only tests exist to call proves the engine can
page internally — which `paginatingRows` already does — and proves nothing about
the token a client would hold. It also leaves `ForceContinuations` (CQ-69.4)
testing a path no user can reach, and RFC-201 §2 principle 6 makes the corpus the
specification of the *supported* surface.

**5.2 — A Go-native token format (not `ContinuationProto`).** Simpler, no
vendored proto, no unset `plan` field to explain. **Rejected on the hard line:**
continuations are named wire-compat in CLAUDE.md. A Go-native format makes even
*parsing* a peer token impossible, forecloses R1-R3 permanently, and would have
to be migrated later at exactly the moment cross-engine resume became valuable.
Adopting the structure now costs one vendored proto and buys the option.

**5.3 — Add a Go-only field (canonical SQL, or a Go plan blob) to
`ContinuationProto`.** Would make §4.3 step 5 trivially implementable.
**Rejected:** it converts a value-scoped fence into a schema fork. A Java parser
would see an unknown field on a message it believes it owns, re-vendoring the
proto becomes a merge instead of a copy, and R1-R3 stop being openable gates.
§4.3 records explicitly that hitting this wall means reconsidering R1, not
growing a field.

**5.4 — Implement Java's Path 1 fully: serialize Go plans into
`PRecordQueryPlan` and deserialize on resume.** The maximal-fidelity port, and
the one CLAUDE.md's "a missing capability is a thing to BUILD" argues for.
**Rejected for this RFC, on leg 2 of §3.3 rather than on effort:** Go's
`RecordQueryInMemorySortPlan` has no lossless encoding in the shared proto
(`PRecordQuerySortKey` is a `KeyExpression` + one `reverse` bit; Go's key is a
`values.Value` with per-key `Desc` and `NullsFirst`). A serializer that silently
lost `NullsFirst` would resume with a different row order than the page it
continues — a wrong-rows bug of exactly the class the repo keeps finding. The
capability is buildable, but it is blocked on a *representation*, not on
willingness, and R1 names the representation work.

**5.5 — Keep the statement-wide total under a Go-only option name alongside the
page size.** **Rejected:** two row-limit knobs whose interaction has no
reference definition. `LIMIT` already expresses "N rows total" as a plan
operator, and RFC-128 moved it there deliberately.

**5.6 — Resume via `Options.Name.CONTINUATION` only, skipping the SQL
statement.** Java's Path 2, and the smaller change — the guard at
`cascades_generator.go:1215` becomes an implementation instead of a rejection.
**Rejected:** it does not run the corpus. Java's yaml oracle drives every page
through `prepareStatement("EXECUTE CONTINUATION ?")` (`QueryExecutor.java:278`),
so the sixteen files and the oracle both require the statement. Path 2 is
implemented anyway as a by-product (§4.3 step 6 *is* Path 2's gate), but it is
not sufficient.

---

## 6. Sequencing

Each step is independently landable and green. Steps 1-3 are driver/runner work
with no planner risk; step 4 touches the planner entry point and is where the
Graefe implementation lap belongs.

**Step 1 — the proto and the envelope type.** Vendor
`proto/relational/continuation.proto`; generate; implement
`ContinuationImpl`-equivalent construction/parsing with `BEGIN`/`END` encoded in
`execution_state` presence; port the `binding_hash` murmur3-32. No behaviour
change to any existing path. Deliverables: G6 (golden), G7 (binding hash),
and the measured answer to "which corpus file carries the file-level
`unsupported:continuation` skip" (§0.4), which the ledger dump yields.

**Step 2 — `MAX_ROWS` page semantics.** Retire the statement-wide total, delete
`TestFDB_RFC106a_MaxRowsStatementWide` and land G1; repoint `pageRowBudget`;
range-check the option; wire the reason codes (§4.5). At the end of this step
`maxRows.yamsql` still cannot pass — the pages exist but the caller cannot see
them — which is the honest state and is why step 3 is not optional.

**Step 3 — surfacing.** `ContinuationRows`, the `api.ResultSet.Continuation()`
production caller, the exhausted-only precondition. Land G2. Delete the
`OptContinuation` guard and rewrite `TestOptContinuation_RejectsLoudly` (G5).

**Step 4 — `EXECUTE CONTINUATION`.** The statement kind through `planOne`, the
literals-table reconstruction, the re-plan, the identity gates, the constraint
gate (§4.7), `EXPLAIN EXECUTE CONTINUATION`. Land G3, G4, G8. **Graefe
implementation lap here.**

**Step 5 — runner wiring.** The corpus runner consumes `maxRows:` and multi-page
`result:` sequences instead of skipping them; the ledger and the assignment
digest move in the same commit. Land G9.

**Step 6 — the oracle (CQ-69.4).** `ForceContinuations` as a Go execution mode:
every eligible SELECT re-executed at `maxRows=1`, pages reassembled, compared to
the one-shot result row for row and order for order, with Java's two loop
invariants ported — "Received continuation shouldn't be at beginning"
(`QueryExecutor.java:367`) and the `MAX_CONTINUATIONS_ALLOWED = 100` abort
(`:59-60`, `:352-354`). Land G10.

---

## 7. Gates

Every gate is falsifiable and names what goes red. Where a fix can be wrong in
more than one direction, the directions are enumerated separately — a fix that
satisfies one is how a bug survives.

**G1 — `MAX_ROWS` is a page size.** New FDB test replacing
`TestFDB_RFC106a_MaxRowsStatementWide`: 10 rows, `MAX_ROWS=2`, six executions
yielding `2,2,2,2,2,0` and an at-end continuation only on the sixth.
*Mutation directions, each pinned separately:* (a) revert to the statement-wide
total → first page returns 2 and the second execution is refused; (b) drop the
trailing empty page → the sixth assertion fails; (c) make the page size
per-transaction rather than per-execution → page counts change under a scan
limit.

**G2 — the token is reachable and round-trips.** A `database/sql` test asserting
`ContinuationRows` is obtainable from a `*sql.Rows`, that `Continuation()`
before exhaustion errors, and that the serialized token re-parses to an equal
envelope. *Mutation:* leave `continuation` unexported → compile failure; drop
the exhausted-precondition → the error assertion fails.

**G3 — page-by-page equals one-shot.** For a fixed query set, the concatenation
of pages at `MAX_ROWS=1` equals the unpaged result, row for row and order for
order. This is the `ForceContinuations` property at unit scale and it is the
single most valuable gate here. *Mutation:* drop a row at the page boundary, or
emit the boundary row twice — both go red. Must include at least one query whose
plan contains an in-memory sort and one with an aggregate, since those are the
`execution_state` shapes `DIVERGENCES.md:1173-1189` flags.

**G4 — mismatch rejection, three independent directions.** Each raises
`INVALID_CONTINUATION` with Java's wording:
 (a) **binding hash** — resume with a different literal → "Continuation binding
 does not match query";
 (b) **plan hash** — resume after a change that re-plans differently → "Continuation
 plan does not match query";
 (c) **plan constraint** — resume after a schema/index change that invalidates
 the constraint → rejected (§4.7), the direction Java swallows.
*This gate must be mutated three times, once per direction.* Note for the
implementer: (b) and (c) are **different gates** and the brief's framing of "the
plan-hash gate red on schema change" conflates them — in Java the schema-change
detector is the constraint, and the plan hash detects a *plan* difference. A test
that only exercises (b) and calls it schema-change coverage is the failure this
note exists to prevent.

**G5 — the reversal is pinned, not merely deleted.**
`TestOptContinuation_RejectsLoudly` currently asserts three things: an error, the
code `ErrCodeUnsupportedOperation`, and the literal substring `"engine-private"`
(`continuation_option_test.go:29-35`). It is replaced by a test asserting a valid
`api.OptContinuation` now *resumes*, and an invalid one is rejected with
`INVALID_CONTINUATION` — not `UnsupportedOperation`. *Mutation:* restore the
guard → the resume assertion goes red.

**G6 — wire-format golden, both directions.** A committed byte golden of a Go
envelope with every field populated, asserted field-by-field against the Java
schema; plus a **Java-authored token fixture** that Go parses, reads
`version`/`reason`/`execution_state` from, and then rejects for plan resume with
the fence message. *Mutation:* renumber any field → golden red; change the fence
to a schema divergence → the Java-parses-Go direction red. G6 is also where §3.2's
INFERRED claim about Java's `valueOf` rejection path is **measured** through the
conformance server; if it measures otherwise, §3.2's fence mechanism changes and
this RFC is amended rather than the finding filed.

**G7 — `binding_hash` is Java-identical.** Golden vectors per literal type,
cross-checked against Java's computed `getParameterHash()` through the
conformance server. *Mutation:* change the murmur seed or the `Objects.hash`
fold → red. This is a genuine cross-engine agreement claim and is the one hash
this RFC asserts portability for, so it is measured, not asserted.

**G8 — the §4.7 divergence is real.** The same continuation + invalidating
schema change run on both engines through the conformance server; Go rejects.
If Java also rejects, the divergence note is deleted and this gate becomes an
equivalence assertion. Either outcome is a result; a green test with an unread
Java side is not.

**G9 — the ledger transition, denominated honestly.** In the same commit:
`inner_skips{unsupported:continuation=16}` → `0`;
`file_skips{unsupported:continuation=1}` → `0`; `pinnedFileTotal` unchanged at
`238`; `pinnedAssignmentDigest` updated. **The number of `maxRows` files that
reach `pass` is REPORTED, not predicted** — fifteen of the sixteen are currently
claimed by a higher-priority class (§0.4), so most will move from
`unsupported:continuation` to *another* class rather than to `pass`, and the
per-file before/after assignment is the deliverable. A gate that predicted "+16
passes" would be the RFC-200 revision-1 error repeated.

**G10 — the oracle is instrumented and floored.** `ForceContinuations` as a Go
mode, with the per-run count of queries actually multiplied REPORTED and
FLOORED. Per RFC-201 and CQ-69.4, an oracle without a gated instrument is prose,
not coverage: a mode that silently multiplies zero queries passes vacuously.
*Mutation:* make `isForcedContinuationsEligible` return false → the floor goes
red.

---

## 8. Residues — what this RFC does not do, stated so it is not discovered later

1. **Cross-engine plan resume**, fenced by §3.2 with re-arm conditions R1-R3.
   Not filed as a TODO: it is a gate in this document with three named
   measurements, which is what makes it reachable.
2. **`COPY` continuations.** `CopyPlan` is copied into the proto so the tag is
   reserved and a Java `COPY` token is rejected by name; `COPY` itself is not
   implemented and is out of scope.
3. **RFC-191 condition (D) loses its supporting premise.** It reasons that an
   InJoin plan-choice divergence "is not a cross-engine wire concern" *because*
   "Go continuations are engine-private by construction (a Java-minted token is
   rejected outright)" (`rfcs/191:774-779`, `TODO.md:905-910`). After this RFC a
   Java-minted token is parsed rather than rejected outright, so that sentence
   must be re-derived against §3.2's fence — the conclusion may well survive on
   the fence instead, but it no longer holds for the stated reason. Step 4
   updates both sites.
4. **`DIVERGENCES.md:1161-1206` is rewritten, not deleted.** The engine-private
   entry becomes the record of §3.2's narrower fence, and the
   aggregate/memory-sort caveats at `:1173-1189` are re-justified per §4.6 —
   they are now safe because of the fence, not because tokens never travel.
5. **The `plan` field stays unset**, and §4.3 records that hitting the canonical-
   query-text wall means reconsidering R1, never adding a proto field.
6. **`TypedQueryArgument` scope handling for prepared parameters** follows
   Java's `deserializeArgumentsForParameters` (`PlanGenerator.java:367-368`);
   `prepared.yamsql` is one of the sixteen and is *also* claimed by
   `unsupported:prepared`, so it will not reach `pass` from this work alone —
   another reason G9 reports rather than predicts.
