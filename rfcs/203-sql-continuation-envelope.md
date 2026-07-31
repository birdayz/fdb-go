# RFC-203 — The SQL continuation envelope: `EXECUTE CONTINUATION`, `ContinuationProto`, and per-execution `MAX_ROWS`

**Status:** DRAFT, **revision 2**. Revision 1 was **NAK'd by both reviewers**, with
the design's core judgments — the cross-engine decline, the `MAX_ROWS`
retirement, the record-layer layering, and the gate discipline — explicitly
endorsed. The central mechanism was ruled on by the coordinator and is folded
here.

**Decision in one sentence:** adopt Java's `ContinuationProto` verbatim, **carry
Go's own serialized physical plan in `compiled_statement.plan` under
`plan_serialization_mode = "GO_V0"`**, execute `EXECUTE CONTINUATION` by
deserializing that plan behind hash and constraint gates, give `MAX_ROWS` its
Java per-execution page meaning **while preserving transparent transaction
rollover**, surface the token through a connection-scoped driver seam, and
target same-engine resume — declining cross-engine plan resume on measured
grounds with a **per-path** fence.

### What changed from revision 1

1. **The mechanism is PLAN TRANSPORT, not re-planning.** Revision 1 rejected
   plan serialization (§5.4) and had `EXECUTE CONTINUATION` re-plan through
   Cascades from a canonical query text recovered out of the literals table.
   Both reviewers proved that text is **unrecoverable**: `TypedQueryArgument`
   addresses literals by ordinal, and the query skeleton lives only in
   `AstNormalizer`'s `sqlCanonicalizer`, which is never serialized. Revision 1's
   §4.3 rested on a mechanism that cannot exist. Deleted.
2. **The premise behind that rejection was wrong.** Revision 1 read CLAUDE.md's
   no-parallel-pipelines rule as barring plan deserialization. It bars a second
   **SQL→plan** path. Deserializing a plan *the same planner produced* is
   **transport across a page boundary**. §5.4 reopens and is now the design.
3. **The fence is stated PER PATH.** Java has two consumption paths and they
   fail differently: Path 1 (`EXECUTE CONTINUATION`) is fenced by the mode
   string; Path 2 (a continuation on a planned statement) by plan-hash
   divergence. §3.2.
4. **`R2` is gated on `R3`.** R2 without R3 removes the Path-2 fence while Go's
   `execution_state` is still privately framed — wrong rows through a gate we
   opened. §3.4.
5. **Java behaviour is MEASURED, not inferred.** Revision 1 twice wrote "this
   repo cannot run Java". **False** — the conformance server drives a full
   fdb-relational JDBC stack. Both sentences are deleted; §9 reports real runs.
6. **The ledger prediction is re-denominated to `+0`**, and revision 1's
   "fifteen files claimed by higher-priority classes" attribution is corrected
   — the mechanism is a six-arm priority ladder *followed by* first-suppressing
   in **insertion** order, which is not document order (§0.4).
7. **§4.2's "write `LIMIT N` instead" guidance is WITHDRAWN as actively wrong**:
   `LIMIT` is itself a Go-only extension Java rejects with `0AF00`, and it is
   the *measured* second blocker on `maxRows.yamsql` (§9.5).
8. **Three new findings from reading:** on Path 1 Java validates **neither
   hash** against anything external (§2.4); there is a **third** Go-private
   inner shape (§9.4); and `DIVERGENCES.md`'s "in-memory sort has no Java
   counterpart" is **false** (§9.4).
9. **Two self-corrections made while drafting revision 2**, both from measuring
   a claim this document was about to assert:
   - The in-memory sort is **not** the only unrepresentable plan node. Seven Go
     plan types have no message in the shared proto at all. §3.3 carries the
     correction; G14 turns it into a maintained census. This widens the
     `GO_V0` limitation materially and must not be soft-pedalled.
   - Java's Path-1 fence is **loud but not well-formed**: SQLSTATE `XXXXX`,
     not `24F00` (§9.1). An earlier phrasing of §3.2 credited Java's "own
     existing gate"; the token in fact dies before that gate.

**Closes:** CQ-69.2's continuation half (`TODO.md:12552-12556`); unblocks CQ-69.4
(`TODO.md:12564-12575`).
**Supersedes:** `OptMaxRows` semantics from RFC-106a §3 (`rfcs/106:47-50`).
**Amends, in this RFC's own commits:** `DIVERGENCES.md:1161-1206` and `:1187-1189`;
RFC-191 condition (D) at `rfcs/191:774-779` + `TODO.md:905-910` (§8.3).

Go citations are at `ef2b5911a`. Java citations are at tag **4.12.11.0** under
`fdb-record-layer/` (present in the shared checkout, gitignored, not in this
worktree).

---

## 0. Four corrections to the framing this RFC was commissioned under

**0.1 — There is no "Phase-2 STOP" document.** Searched `TODO.md`, all of
`rfcs/`, `DIVERGENCES.md`, `shifts/`. No such text; every `STOP` in `TODO.md` is
unrelated to continuations. The **four absences are all real** and each is
re-verified below — but they were never recorded together, and nothing may be
quoted from a document that does not exist.

**0.2 — RFC-181 WS-C C2 did not decide; it framed.** C2 (`rfcs/181:290-302`)
ends "Decision needed: adopt ContinuationProto + hashes, or declare Go SQL
tokens engine-private". The decision was recorded in `DIVERGENCES.md:1161-1206`;
§3.1 quotes and splits it clause by clause.

**0.3 — Java has ONE row-limit option, and it is a page size.**

| Java option | Meaning | Wiring |
|---|---|---|
| `MAX_ROWS` | "the maximum number of records to return **before prompting for continuation**" (`Options.java:58-63`) — per-execution PAGE SIZE. Default `Integer.MAX_VALUE` (`:280`), range `[0, Integer.MAX_VALUE]` (`:532`) | `setReturnedRowLimit` (`QueryPlan.java:434`) |
| `EXECUTION_SCANNED_ROWS_LIMIT` | per-TRANSACTION **scan** limit; throws `SCAN_LIMIT_REACHED` (`:187-192`) | scan limiter |

There is **no statement-wide returned-row total in Java.** Go's is a Go-only
semantics sitting on a Java option name.

**0.4 — The "16" counts `maxRows`-carrying files; the honest pass delta of this
RFC as drafted is `+0`; and the suppression mechanism is not what revision 1
said.** Measured over
`third_party/apple/fdb-record-layer/yaml-tests/src/test/resources/`: 16 files
contain `maxRows`, 14 mention "continuation", intersection 9. The ledger books
`unsupported:continuation=1` file-level and `=16` query-level
(`pinned_ledger_test.go`).

Revision 1 attributed the 15-file gap to "higher-priority classes". The
correction, measured (`javacorpus/runner.go`, `testblock.go`):

1. **A six-arm file-level priority ladder runs first** and does rank:
   negative-parse (`runner.go:71`) → fragment (`:75`) → aborting `skipFileError`
   (`:102`) → DDL gap (`:112`) → the `engineGaps` table (`:120`) →
   fixed-version-meta (`:135`) → negative-execution (`:152`).
2. **Only if all seven decline** does `suppressedBy` (`:210-219`) pick the
   **first *suppressing* skip in `res.Skips` insertion order** (`:199-208`).
3. **Insertion order is NOT document order.** `testblock.go` records
   `SkipResultMetadata` **eagerly** inside the config loop (`:263`) but defers
   `skipQuery = SkipContinuation` (`:261`) until **after** it (`:274-275`). So in
   a query carrying both, result-metadata is appended first even when `maxRows:`
   comes first in the file.

Accurate phrasing, to be used in place of revision 1's: *the file class is the
first suppressing skip in `res.Skips` insertion order, and only after seven
higher-priority arms have declined; `maxRows`' skip is appended last-in-query,
so any eagerly-recorded suppressing skip in the same query beats it.*

The file-level carrier is **`initial-version/mid-query.yamsql`** — its only query
is `select * from t1;` + `maxRows: 1`, so `SkipContinuation` is its sole
suppressing skip. Its manifest reason is itself the proof
(`javayamsql/manifest.go:374-375`): *"maxRows leaves a continuation open, so a
version check is reached mid-query"*.

And RFC-201's own `+16` is a **directive census** number which RFC-201 explicitly
disclaims at `rfcs/201:155-158`: *"those are file counts derived from directive
census, not from execution, and each should be re-stated as a measurement when
its phase lands."* This RFC does that: gate G9 reports, and does not predict.

---

## 1. The defect, measured — four absences and the causal link

### 1.1 (A) `OptMaxRows` is a statement-wide total ending in a silent `io.EOF`

`cascades_generator.go:1465-1472`:

```go
	// MAX_ROWS statement-wide cap (RFC-106a §3): a TOTAL returned-row
	// budget across ALL pages. ... A clean stop (io.EOF), not an error — JDBC
	// setMaxRows semantics. ...
	if r.maxRows > 0 && r.emitted >= r.maxRows {
		return io.EOF
	}
```

Silent: a bare `io.EOF` is `Rows.Next() == false` with `Rows.Err() == nil`,
indistinguishable from natural exhaustion, while `r.continuation` (`:1317`) may
hold a live position. The sibling byte cap twenty lines down *does* signal
(`ErrCodeExecutionLimitReached`, `:1487-1489`). Field comment `:1333-1337`; sole
read `:1252`; option `api/options.go:42`, default `:175`; pinned by
`TestFDB_RFC106a_MaxRowsStatementWide` (`sqldriver/resource_limits_fdb_test.go:158-188`).

**Java's behaviour, executably.** `maxRows.yamsql`:

```yaml
      - query: select ta.* from ta;
      - maxRows: 2
      - result: [{1, 2}, {3, 4}]
      ... five pages ...
      - result: [] # Even multiple, it takes one more to know it is exhausted
```

Ten rows, `maxRows: 2`, **six pages**. Under Go's semantics: two rows, done.

**RFC-106a's reasoning, refuted** (`rfcs/106:47-50`):

> `OptMaxRows` → a **STATEMENT-WIDE** returned-row cap (codex P2): because
> `paginatingRows` auto-follows continuations, wiring it to per-page
> `ReturnedRowLimit` would make it a page size. Instead track a remaining-row
> budget … — Java's JDBC `setMaxRows` semantics (total, not per-page).

Java gives JDBC's `setMaxRows` a *per-page* meaning on this driver and says so
in the option's own doc (`Options.java:58-62`); the yaml suite drives it per page
(`QueryExecutor.java:279-280`, `:311`). "Would make it a page size" names the
target, not a hazard.

**Root cause is (B)+(D).** `paginatingRows` auto-follows inside one `Execute`
because Go cannot hand a page boundary to a caller. With no observable page, a
page size was unobservable, so the option was re-pointed at the only observable
quantity.

**Unreachable today:** `OptMaxRows` has no production setter. `SetOptions`'
non-definition callers (`rowdiff/run.go:147`, `simfdb/hunt/sqlpage`) set only
scan limits; DSN options are never mapped into `api.Options`
(`sqldriver/dsn.go:48-49`); there is no `SET MAX_ROWS`. Only `conn.Raw` reaches
it. That is why the wrong semantics survived green.

### 1.2 (B) `api.ResultSet.Continuation()` has zero production callers

Declared at `api/resultset.go:42-44`; `api.Continuation`
(`api/continuation.go:8-21`) already mirrors Java — `Serialize()`,
`ExecutionState()`, `Reason()`, with `ContinuationReason`'s iota order matching
Java's enum ordinals and `String()` returning Java's names (`:26-56`). The shape
is not the problem.

`grep -rn "\.Continuation()"` returns one hit, in a test
(`executor/resultset_test.go:506`). `RecordLayerResultSet` is constructed in
production (`cascades_generator.go:1813`) but the drain bypasses the api
accessor (`:1831`), so `liveContinuation` (`executor/resultset.go:423`) and
`exhaustedContinuation` (`:619`) are constructed by nothing live. The bytes are
an unexported field on the unexported `paginatingRows` (`:1317`, written `:1845`,
read `:1808`).

**No plumbing either.** `paginatingRows` implements `driver.Rows` plus
`RowsColumnType*` (`:1374-1450`); `sqldriver` declares no custom interface;
`database/sql` has no `Rows.Raw()`. §4.4.

### 1.3 (C) There is no envelope — and there are **three** Go-private inner shapes

Everything named `*Continuation` in `gen/` is a record-layer cursor-internal
message; none carries `version`, `reason`, `binding_hash`, `plan_hash`, or a
statement reference. There is no `continuation.proto` in `proto/relational/` for
the SQL layer.

The nearest construct disclaims the role (`executor/executor.go:1606-1612`):

```go
// LimitContinuation is the RFC-128 §3.3 envelope for a RecordQueryLimitPlan's
// continuation: ... It is Go-only and INTERNAL to executeLimit — it never
// becomes a SQL resume token or a wire/Java-interop continuation (no .proto, no
// magic-6773487359078157740 surface).
```

`DIVERGENCES.md:1173-1189` enumerates **two** Go-private `execution_state`
shapes. There are **three** — §9.4 adds `DistinctHashContinuation` and corrects
the memory-sort entry, which is false as written. All three are conditions of R3
(§3.4) and all three appear in G3's shape list (§7).

The absence is a *recorded decision* (`DIVERGENCES.md:1161` ff., §3.1), enforced
at `cascades_generator.go:1215-1218`, pinned by `TestOptContinuation_RejectsLoudly`
(`continuation_option_test.go:18-36`).

### 1.4 (D) The grammar parses `EXECUTE CONTINUATION`; nothing implements it

`parser/grammar/RelationalParser.g4:683-686`:

```antlr
executeContinuationStatement
    : EXECUTE CONTINUATION packageBytes=continuationAtom
      queryOptions?
    ;
```

with `continuationAtom : bytesLiteral | preparedStatementParameter` (`:389-392`),
`bytesLiteral : HEXADECIMAL_LITERAL | BASE64_LITERAL` (`:814-815`), reachable
from `administrationStatement` (`:73-81`) and `describeObjectClause` (`:735`).
Lexer tokens at `RelationalLexer.g4:234`, `:782`.

Every `VisitExecuteContinuationStatement` is generated code — zero overrides.
`cascadesGenerator.planOne` (`:145`) never descends, so the statement falls
through to `:182-183`:

```go
		return nil, api.NewError(api.ErrCodeUnsupportedOperation,
			"only SHOW administration statements are supported")
```

`packageBytes` is discarded unread. Zero tests contain "EXECUTE CONTINUATION".

### 1.5 The link: (A) is a symptom of (B)+(D)

(C)+(D) are the missing mechanism, (B) the missing surface, (A) what happened to
a Java option name in their absence. Fixing (A) alone yields a page nobody can
observe; fixing (B) alone yields a token nobody can redeem.

---

## 2. Java is the spec

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
  optional bytes execution_state = 2;
  optional int32 binding_hash = 3;
  optional int32 plan_hash = 4;
  optional CompiledStatement compiled_statement = 5;
  optional Reason reason = 6;
  oneof other_plan { CopyPlan copyPlan = 7; }
}
```

`CompiledStatement` (`:85-98`): `plan_serialization_mode` (string), `plan`
(`PRecordQueryPlan`), `extracted_literals`, `arguments`, `plan_constraint`,
`queryMetadata`.

`ContinuationImpl` writes `version = CURRENT_VERSION = 1` (`:44`, `:55`) and
encodes the boundaries in `execution_state` **presence**
(`Continuation.java:66-73`):

```java
    default boolean atBeginning() { return getExecutionState() == null; }
    default boolean atEnd() {
        byte[] bytes = getExecutionState();
        return bytes != null && bytes.length == 0;
    }
```

**Java writes `version` and never validates it** — the only read is the EXPLAIN
display (`QueryPlan.java:359`); there is no version-mismatch throw site anywhere.
§4.1 does not make it a gate, because a gate Java lacks would reject valid Java
tokens for a reason Java never intended.

### 2.2 Producing a token

`QueryPlan.java:433-436` sets the page size; `:443-448` installs the enricher;
`:451-493` is the enricher:

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

Hashes and reason always; compiled statement only when not at end. Serialization
ORDER is load-bearing — plan, arguments in ordinal order, constraints — because
type-repository dictionary compression depends on it (`:461-467`, restated
`PlanGenerator.java:324-330`).

The reason comes from the result set (`RecordLayerResultSet.java:121-135`), and
`getContinuation()` throws unless exhausted (`:110-113`).

### 2.3 Consuming a token — two paths

**Path 1 — `EXECUTE CONTINUATION`.** `AstNormalizer` flags and un-caches in one
breath (`:465-472`); `PlanGenerator` branches (`:228-234`); the payload is
mandatory (`:272-294`). **There is no re-plan-from-SQL-text path, because there
is no SQL text.** `generatePhysicalPlanForCompiledStatementContinuation`
(`:313-408`) deserializes (`:334-335`) and checks the blob against itself
(`:340-342`).

**Path 2 — a continuation on a normally-planned statement.**
`PhysicalQueryPlan.validatePlanAgainstEnvironment` (`:279-293`) →
`PlanValidator.validateHashes` (`PlanValidator.java:72-96`). Reached through the
Direct Access `Options.Name.CONTINUATION`, not through SQL.

### 2.4 **On Path 1, Java validates NEITHER hash against anything external**

New in revision 2; it drives §4.3's gates and §4.7's severity.

`validateHashes` has exactly **one** call site — `QueryPlan.java:283`, inside
`PhysicalQueryPlan`. `ContinuedPhysicalQueryPlan` **overrides**
`validatePlanAgainstEnvironment` (`:552-573`) and does **not** call it. So on
Path 1:

- **binding_hash is self-referential.** `PlanGenerator.java:370-373` seeds the
  new context's parameter hash *from the token*:
  `new MutablePlanGenerationContext(..., Objects.requireNonNull(continuation.getBindingHash()))`.
  Any later comparison compares a value to itself.
- **plan_hash is a blob self-consistency check.** `:340` compares the token's
  hash to the hash of the plan deserialized *from the same token* — it detects
  serializer drift and corruption, never an environment change.
- **the plan constraint — the only external gate — is swallowed** (`:561-565`,
  §4.7).

Java's `EXECUTE CONTINUATION` therefore validates **nothing** about the
environment it resumes into. Go does not copy this (§4.3 steps 6-7, §4.7).

### 2.5 The two hashes

**`binding_hash`'s algorithm is portable.** `AstNormalizer.java:153-154`:

```java
        parameterHash = Hashing.murmur3_32_fixed().newHasher().putInt("ParameterHash".hashCode());
```

fed per literal at `:546-556`:

```java
            final String canonicalName = parameterName == null ? "?" : "?" + parameterName;
            sqlCanonicalizer.append(canonicalName).append(" ");
            parameterHash.putInt(Objects.hash(canonicalName, literal));
```

`String.hashCode` is JLS-fixed, `Objects.hash` is a 31-fold over element
`hashCode`, boxed-scalar `hashCode`s are Javadoc-fixed, murmur3-32 is portable.
**But `literal` is an arbitrary object, and for `x'…'` it is a `byte[]` from
`ParseHelpers.parseBytes` whose `hashCode` is identity** — so Java's own binding
hash is not reproducible for bytes-literal queries. Measured §9.3; decided §4.1.

**`plan_hash` is not portable.** `PlanHashable.objectPlanHash` (`:242-269`)
bottoms out at `return obj.hashCode();`, folding `31 * result + …` (`:282-288`)
into a Java `int`. Go's is FNV-64a over `HashCodeWithoutChildren()`
(`plans/plan_hash.go:19-40`), whose comment says its value "may change across
releases (RFC-176 P2 did) without compatibility impact" *because* nothing
wire-facing embeds it. That stops being true here — §6 step 1.

### 2.6 The layering

```
  SQL resume token = ContinuationProto              <- this RFC adds
                       .version / .reason
                       .binding_hash / .plan_hash
                       .compiled_statement{ mode, plan, literals, constraint, metadata }
                       .execution_state = ───┐
                                             │ opaque bytes, unmodified
  record-layer cursor continuation <─────────┘
    … structural cursors (union / intersection / flat-map / distinct / sort) …
      leaf KeyValueCursorContinuation
        { inner_continuation, magic_number = 6773487359078157740 }
          pkg/recordlayer/key_value_cursor.go:15-18, :41-45
```

Java draws exactly this line: `ContinuationImpl.fromRecordCursorContinuation`
(`:185-187`) stores `cursorContinuation.toBytes()` uninspected, and
`executePhysicalPlan` hands `getExecutionState()` straight to `executePlan`
(`:439`). The magic number is `KeyValueCursorBase`'s **leaf-scan** framing
discriminator; it is not, and must not become, a SQL-envelope marker. The
envelope's discriminator is that it is a valid `ContinuationProto`.

---

## 3. THE CENTRAL DECISION — cross-engine resume

### 3.1 What is reversed, precisely

`DIVERGENCES.md:1161-1206`:

> Java's fdb-relational wraps SQL continuations in a ContinuationProto envelope
> (version + plan_hash + binding_hash + execution_state) and gates resumes
> through PlanValidator. Go has no envelope and NO SQL resume entry point at
> all […]
> Decision: Go SQL tokens are ENGINE-PRIVATE until a real resume surface
> exists. The boundary is loud, not silent: supplying api.OptContinuation
> fails the statement with ErrCodeUnsupportedOperation […] **Adopting
> the ContinuationProto envelope + PlanValidator hashes is the follow-up arc
> if/when a resume surface ships; hash values would deliberately differ per
> engine so cross-engine resume attempts REJECT loudly in both directions.**

The decision is **conditional**; this RFC discharges its condition.

| Clause | Status |
|---|---|
| "engine-private **until a real resume surface exists**" | **Condition discharged** (§4.3, §4.4). |
| "`api.OptContinuation` fails … `ErrCodeUnsupportedOperation`" | **REVERSED.** Guard deleted; the code becomes `24F00`, not `0A000` (§4.8); `TestOptContinuation_RejectsLoudly` rewritten (G5). |
| "Adopting the ContinuationProto envelope + PlanValidator hashes is the follow-up arc" | **This is that arc.** |
| "hash values would deliberately differ per engine" — **`plan_hash`** | **UPHELD**, and on Path 2 the plan-hash divergence **IS** the fence (§3.2). |
| "hash values would deliberately differ per engine" — **`binding_hash`** | **REVERSED.** `binding_hash` is a port of Java's algorithm and is *intended to agree* (§4.1, G7). It is never a fence — relying on it as one would break the moment the port succeeded. |
| RFC-181 non-goal "Wire-format changes (none required)" (`rfcs/181:343-344`) | **REVERSED.** |

**The ground that shifted:** the corpus arrived with two consumers that did not
exist when C2 was decided — sixteen `maxRows` files (`TODO.md:12552-12556`) and
the `ForceContinuations` oracle (`rfcs/201:192-199`, `TODO.md:12564-12575`).
"Tokens never cross the API boundary" was true of a system with no consumers.

### 3.2 The decision, and the fence PER PATH

> **Adopt `ContinuationProto` verbatim — field numbers, semantics, enum values,
> and the `execution_state` boundary encoding. Carry Go's OWN serialized
> physical plan in `compiled_statement.plan` under
> `plan_serialization_mode = "GO_V0"`. Target SAME-ENGINE resume. DECLINE
> cross-engine plan resume: neither engine executes a plan that came out of the
> other's token.**

Nothing Go-only is added to the proto. **Scoping is by VALUE, never by SCHEMA**,
because a schema divergence is permanent while a value divergence is a gate that
opens later without a wire migration.

The fence differs by path, and both must be stated because a reader who knows
only one will believe the other is unprotected:

- **Path 1 (`EXECUTE CONTINUATION`) — the mode string.** Java calls
  `PlanHashMode.valueOf(compiledStatement.getPlanSerializationMode())`
  (`PlanValidator.java:54-56`) before the `validPlanHashModes.contains` check.
  `"GO_V0"` is not an enum member, so the token throws and no Go plan is ever
  executed by Java. **Measured (§9.1) — and measured to be LOUD BUT NOT
  WELL-FORMED:** the SQLSTATE is `XXXXX` (`ErrorCode.UNKNOWN`), not `24F00`,
  because the failure happens *before* the typed continuation gate rather than
  inside it. The safety property holds; the error quality does not, and nothing
  in this RFC may promise a client a `24F00` from the Java side.
- **Path 2 (`OptContinuation` on a planned statement) — plan-hash divergence.**
  No mode string is consulted; `validateHashes` compares the token's `plan_hash`
  against the *freshly planned* plan's. Go's `GO_V0` hash and Java's `VC0` hash
  are different algorithms at different widths, so `resolveValidPlanHashMode`
  (`:113-129`) returns `null` and Java throws "Continuation plan does not match
  query". This is why the `plan_hash` half of C2's clause is **upheld**: on
  Path 2 it is load-bearing.
- **Go applies the mode fence on its OWN Path-2 entry too.** `OptContinuation`
  checks `plan_serialization_mode` first, so a Java token is refused **by name**
  rather than by an incidental hash mismatch — a specific error beats a generic
  one, and it means the Go side does not depend on hash divergence surviving a
  future R2.

### 3.3 The evidence — three legs

**Leg 1 — Java's SQL resume structurally requires the peer's serialized physical
plan.** `EXECUTE CONTINUATION` names no query, so the token must be
self-describing; a token with neither `compiled_statement` nor `copy_plan` is an
error by construction (`PlanGenerator.java:290-293`). Cross-engine resume is
therefore "each engine executes the other's physical plan", not "make the hashes
agree".

**Leg 2 — Go's plan vocabulary is not losslessly representable in the shared
proto.** Go writes no `PRecordQueryPlan` today — asserted by
`pkg/docscheck/plan_proto_schema_test.go:17-18` ("Go marshals no plan through
these protos — see RFC-135 §3"), corroborated at `rfcs/135:77-79`. That half is a
missing *serializer*, and this RFC builds it (§4.1).

The structural half remains. `RecordQueryInMemorySortPlan` is a sanctioned
read-side plan (CLAUDE.md) firing for any `ORDER BY` no access path satisfies.
Its key is `plans/in_memory_sort.go:25-30`:

```go
type SortKey struct {
	Field      string
	Desc       bool
	NullsFirst bool
	ValueExpr  values.Value // REQUIRED: the plan-time-baked key Value, evaluated per row
}
```

The only sort message is `proto/apple/record_query_plan.proto:2188-2196`:

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

A `KeyExpression` and one `reverse` bit. No `Value`, no per-key direction, no
`nulls_first`. A Value-keyed, per-key-directed, NULLS-ordered sort is **not
encodable**. §4.1 gives it a loud, specific rejection rather than a lossy
encoding, and R1 names the permanent fix.

**Correction, measured while drafting revision 2: the in-memory sort is NOT the
only unrepresentable node, and an earlier draft of this section said it was.**
A name-level survey of Go's 41 `RecordQuery*Plan` types
(`grep '^type RecordQuery.*Plan struct' pkg/recordlayer/query/plan/plans/`)
against the 38 oneof arms of `PRecordQueryPlan`
(`proto/apple/record_query_plan.proto:1699-1741`) finds **seven Go plan types
with no message in the shared proto at all** — not merely no oneof arm:

```
$ grep -n "^message PRecordQueryComparatorPlan\|^message PRecordQuerySelectorPlan\
\|^message PRecordQueryTextIndexPlan\|^message PRecordQueryLoadByKeysPlan\
\|^message PRecordQueryFilterPlan\|^message PRecordQueryVectorIndexPlan\
\|^message PRecordQueryLimitPlan" proto/apple/record_query_plan.proto
1876:message PRecordQueryFilterPlanBase
```

`Comparator`, `Selector`, `TextIndex`, `LoadByKeys`, `VectorIndex`, `Limit` and
`Distinct` have nothing; `FilterPlanBase` exists but is not a oneof arm. Of
these, `Limit` (RFC-128), `VectorIndex` (RFC-045/094) and `Distinct` (§9.4) are
Go-only extensions with no Java operator to encode into.

A further group — `InJoin`, `Intersection`, `Union`, `MergeSortUnion`,
`Projection`, `NestedLoopJoin`, `Filter` — has no *same-named* arm but plausibly
maps onto a differently-named Java arm (Go's `InJoinPlan` onto Java's three
`In*JoinPlan` arms, `Projection` onto `Map`, `NestedLoopJoin` onto `FlatMap`).
**That mapping is a step-1 deliverable and a gate (G14), not an assumption** — a
name-level survey is evidence that the set is larger than one, not evidence of
its exact size.

**What this does and does not change.** It *strengthens* the cross-engine
decline: the vocabulary gap is wider than revision 1 or this section's first
draft claimed. It *narrows* the `GO_V0` resumable surface: a query whose plan
contains any unrepresentable node is not resumable, and that is a materially
bigger surface than "everything except `ORDER BY`". It does **not** overturn the
mechanism ruling — `PRecordQueryPlan` still covers the scan / index / filter /
join / union shapes that dominate paged queries, and no alternative container
covers more — but the RFC must not sell transport as near-total coverage when it
has not been measured. G14 measures it before step 4 depends on it.

*(Note, corrected from revision 1: Java's `RecordQuerySortPlan` is not merely a
legacy artifact — it has a real deserializer at
`sorting/RecordQuerySortPlan.java:250-271`, it pages through
`MemorySortCursor.createSort` (`:112`), and it produces a real
`MemorySortContinuation`. What Java's **Cascades** lacks is an implementation
rule that emits it. The obstacle is the KeyExpression-vs-Value key
representation, not the absence of a Java sort operator — §9.4.)*

**Leg 3 — plan-hash agreement would exceed what Java gives itself.** Java does
not hold its own plan hash stable across versions: hence `PlanHashMode`
VL0/VC0/VC1 (`PlanHashable.java:68-78`), `VALID_PLAN_HASH_MODES`
(`Options.java:222`), `resolveValidPlanHashMode` (`PlanValidator.java:113-129`),
and `CONTINUATION_DOWN_LEVEL` (`:90-92`). And `execution_state` is not uniformly
compatible either — three Go-private inner shapes (§9.4).

### 3.4 Re-arm conditions — with the dependency stated

Cross-engine plan resume is reconsidered when **all three** hold, each
demonstrated by a committed test:

- **R1 (vocabulary).** A bijection between Go's physical plan node set and
  `PRecordQueryPlan`'s oneof arms for every plan the corpus produces, measured by
  an enumeration test reporting zero unrepresentable nodes. Falsified today by
  `RecordQueryInMemorySortPlan` alone. Closing it means either an upstream
  Value-keyed sort message or Go ordering coverage that removes the operator from
  corpus plans.
- **R3 (execution state).** Every `execution_state` payload Go can emit is
  byte-identical to Java's for the same cursor position, retiring all three
  Go-private shapes (§9.4). Two of the three are **checkable** work against a
  real Java counterpart (aggregate, memory sort); the third
  (`DistinctHashContinuation`) has no Java counterpart and is a genuine
  extension, so R3's condition for it is "the plan that produces it is not
  transported", not "make it match".
- **R2 (hash), GATED ON R3.** Go's plan hash becomes a port of `PlanHashable`
  under a shared `PlanHashMode`, with per-plan-class goldens cross-checked
  through the conformance server. **R2 must not land before R3.** Satisfying R2
  alone makes the hashes agree, which **removes the Path-2 fence** while Go's
  `execution_state` is still privately framed — Java would accept the token and
  feed Go's private payload into Java's cursors. Wrong rows, delivered by a gate
  we opened. This is the single most dangerous ordering in this RFC and is named
  here so no future step takes it. (Go's own Path-2 mode fence, §3.2, is what
  keeps the *Go* direction safe under a premature R2; the Java direction has no
  such backstop, which is why the ordering is a hard constraint and not a
  preference.)

**The envelope already delivers partial cross-engine value without R1-R3:**
because the structure is Java-identical, each engine can *parse* the other's
token and read `version`, `reason`, `execution_state`. What is fenced is
*executing the peer's plan*, not *reading the peer's envelope*.

---

## 4. The design

### 4.1 The proto, and what Go writes

New `proto/relational/continuation.proto`, a verbatim structural copy of Java's,
with Go package options, vendored under the `proto/apple/` rules (NOTICE clause,
version pin). It imports `record_metadata.proto`, `record_query_plan.proto`,
`record_query_runtime.proto` — all three already vendored and generated.
`CopyPlan` (tag 7) is copied so the tag is permanently reserved and a Java `COPY`
token is rejected *by name* rather than read as statement-less.

| Field | Go value |
|---|---|
| `version` | `1`. Written, **not gated** (§2.1). |
| `execution_state` | record-layer bytes, unmodified. Absent ⇒ BEGIN, present-and-empty ⇒ END. |
| `binding_hash` | Java's murmur3-32 algorithm (§2.5) over Go's literals table. Intended to AGREE with Java; never a fence. |
| `plan_hash` | Go's plan hash under `GO_V0`, computed over the serialized form. |
| `compiled_statement.plan_serialization_mode` | `"GO_V0"` — the Path-1 fence. |
| `compiled_statement.plan` | **SET** — Go's physical plan serialized into `PRecordQueryPlan`. This is the mechanism ruling. |
| `compiled_statement.extracted_literals` / `arguments` | the ordinal literals table, in ordinal order, in Java's `TypedQueryArgument` / `LiteralObject` shape. |
| `compiled_statement.plan_constraint` | the continuation plan constraint as `PQueryPlanConstraint`. |
| `compiled_statement.queryMetadata` | the result `PType`, so a resumed page reports identical column metadata — what the two `check-result-metadata/*-continuation-page.yamsql` corpus files assert. |
| `reason` | §4.5. |

Set-only-when-not-at-end for `compiled_statement` is copied exactly
(`QueryPlan.java:459-460`), as is the serialization ORDER (`:461-467`).

**The bytes-literal `binding_hash` divergence, DECIDED.** §9.3 measures whether
Java's own `binding_hash` is reproducible for a bytes-literal query. **Go hashes
bytes literals by CONTENT.** If Java's value is identity-derived, there is no
value to reproduce — Java cannot reproduce its own — and a content hash is the
only definition under which `binding_hash` means anything for these queries.
The consequence is stated rather than hidden: for a bytes-literal query a Go
`binding_hash` will not equal a Java one, and this is a divergence **from a value
Java itself cannot produce twice.** G7 asserts agreement only over the literal
types where Java is self-consistent and asserts the bytes divergence explicitly,
so the deliberate difference is pinned rather than latent.

**Unrepresentable plan nodes fail loudly at mint time.** Serializing a plan
containing `RecordQueryInMemorySortPlan` — or any of the seven Go plan types with
no message in the shared proto (§3.3) — fails with an error naming the node and
citing R1, never a lossy encoding. For the sort specifically, a lossy
`PRecordQuerySortPlan` would drop `NullsFirst` and resume in a different order
than the page it continues, which is a wrong-rows bug wearing a compatibility
costume.

Consequence, stated at full width rather than minimised: `ORDER BY` without a
satisfying access path, `LIMIT`/`OFFSET`, vector search, text index,
`SELECT DISTINCT`, and the comparator/selector/load-by-keys shapes are **not
resumable under `GO_V0`**. That is a large, stated, tested limitation (G11 for
the loudness, G14 for the exact set), not a silent gap — and it is the honest
price of transporting through a proto that was written for a different plan
vocabulary.

### 4.2 `MAX_ROWS` becomes a page size — while transaction rollover is PRESERVED

`OptMaxRows` takes Java's meaning: the returned-row limit for ONE execution.

**The critical scope limit — the real-user hazard revision 1 missed.**
`paginatingRows` serves two different auto-follow needs from one loop, and only
one is changing:

- **the user-requested `MAX_ROWS` boundary** — stops, yields a token. NEW.
- **transparent transaction rollover** — a page ends because the cursor hit an
  out-of-band scan/byte/time budget inside one FDB transaction
  (`pageContinuationState` returns `(false, bytes, nil)`,
  `cascades_generator.go:1717-1720`; the next `fetchPage` runs in a fresh
  `Transact`). **PRESERVED, unchanged.** A long scan with no `MAX_ROWS` must keep
  rolling transactions transparently and return complete results.

Turning rollover into a user-visible stop would break every long scan on the
5-second transaction limit and hand callers a token they never asked for. The
test is: *did the caller ask for a page boundary?* Only then does one appear.
Gated by G12.

**The statement-wide total is deleted, not renamed.** There is no Java option to
map it to (§0.3), and a second row-limit knob has no reference definition for its
interaction with the page size. `TestFDB_RFC106a_MaxRowsStatementWide` is
**deleted and replaced** by its inverse (G1).

**Revision 1's "write `LIMIT N` instead" guidance is WITHDRAWN, as actively
wrong.** Revision 1 told callers wanting N rows total to use `LIMIT N`, "which is
a real operator in the plan rather than an egress truncation". Measured (§9.5),
`LIMIT`/`OFFSET` is itself a **Go-only read-side extension that Java rejects with
`0AF00 UNSUPPORTED_QUERY`** (`DIVERGENCES.md:696`, RFC-082), and it is booked as
the corpus's single `conformance:go-accepts-what-java-rejects` entry —
`javacorpus/gaps.go:91` — **with `maxRows.yamsql` itself as the witness.**
Recommending it as the substitute for a retired option would have told users to
lean harder on the one construct the corpus already books as an unreviewed
widening of the shared surface. Callers wanting N rows total should page with
`MAX_ROWS` and stop; §9.5 books the `LIMIT` question separately and names it as
the **second prerequisite** for the user-visible unlock.

Retained: `OptExecutionScannedRowsLimit` / `…BytesLimit` / `OptExecutionTimeLimit`
map to Java's `EXECUTION_SCANNED_*`; the Go-local statement timeout and
result-byte cap (RFC-106a §4-5) are Go extensions with no Java option name and
are unaffected.

Consequences to implement rather than discover:

- **`pageRowBudget`** (`:1606-1624`) derives the scan hint from
  `rowCap - r.emitted`, the remaining TOTAL. It becomes the page size.
- **The trailing empty page is real, and conditional.** `maxRows.yamsql` asserts
  it; Java's oracle asserts it as an invariant (`QueryExecutor.java:347`). It
  appears when the row count is an exact multiple of the page size — G1 pins both
  the multiple and the non-multiple case.
- **Validation.** `maxRows: -27525 → INVALID_PARAMETER` is a corpus assertion;
  range-check `[0, math.MaxInt32]` per `Options.java:532`.

### 4.3 `EXECUTE CONTINUATION` — plan transport, not a second plan producer

**The premise correction.** CLAUDE.md's no-parallel-pipelines rule bars a second
**SQL→plan** path. Deserializing a plan *the same planner produced, in the same
engine, for the same statement* moves one plan across a page boundary; it adds no
second producer, and Cascades remains the only thing that turns SQL into a plan.
Revision 1 read the rule as barring deserialization and consequently invented a
re-plan-from-canonical-text mechanism that **cannot exist** —
`TypedQueryArgument` addresses literals by ordinal
(`continuation.proto:56-72`), and the skeleton lives only in `AstNormalizer`'s
`sqlCanonicalizer`, never serialized. That mechanism is deleted.

`EXECUTE CONTINUATION x'…'` in Go:

1. Parse; `proto.Unmarshal`. Failure → Java's code for this case, which is
   `INTERNAL_ERROR` / `XX000` (§4.8), message "unable to parse continuation".
2. Reject BEGIN (`execution_state` absent) and END (present-and-empty), matching
   `AstNormalizer.java:333-334` — "Illegal query with BEGIN continuation." /
   "Illegal query with END continuation.", `INVALID_CONTINUATION`.
3. Reject a foreign `plan_serialization_mode` (§3.2) and a `copyPlan` token.
   Missing both → "Continuation does not have statement to continue",
   `INTERNAL_ERROR` (§4.8).
4. Deserialize `compiled_statement.plan` into a Go physical plan; rebuild the
   ordinal literals table from `extracted_literals` + `arguments` exactly as
   `PlanGenerator.java:349-368`.
5. **Gate on plan hash** — recompute `GO_V0` over the deserialized plan and
   compare. This is Java's `:340-342` check and means the same thing here: **blob
   integrity and serializer-drift detection, NOT an environment gate** (§2.4).
   Mismatch → `INVALID_CONTINUATION`, "cannot continue query due to mismatch
   between serialized and actual plan hash".
6. **Gate on binding hash — and NOT the way Java does.** Recompute by folding the
   `(canonicalName, literal)` pairs of the **reconstructed** literals table per
   `AstNormalizer.java:553-555`, and compare to the token's `binding_hash`.
   **Never seed the expected value from the token's own hash**, which is what
   Java does (`PlanGenerator.java:370-373`) and what makes its Path-1 check
   vacuous (§2.4). Mismatch → `INVALID_CONTINUATION`, "Continuation binding does
   not match query".
7. **Gate on the plan constraint** — evaluate `compiled_statement.plan_constraint`
   against the live store and **reject** on failure. Under transport this is
   **the only external gate that exists** (step 5 is self-consistent by
   construction), so it is load-bearing rather than belt-and-braces. §4.7.
8. Execute the deserialized plan from `execution_state` at the requested page
   size.

**Cache behaviour is copied:** the statement is uncacheable, as Java marks it in
the same visitor that flags it (`AstNormalizer.java:467-468`).

**`EXPLAIN EXECUTE CONTINUATION` is in scope** — the grammar reaches it (`:735`)
and Java emits a `PLAN_CONTINUATION` struct of `(execution_state, version,
plan_serialization_mode, plan_hash, complexity)` (`QueryPlan.java:346-363`). Go
emits the same struct; `complexity` is now populatable because a plan *is*
deserialized.

**A build item this exposes.** Go's `QueryPlanConstraint` exists
(`cascades/match_info.go:14-25`) but is **not plumbed to the chosen plan** on the
SQL side — there is no `Constraint()` on the planned statement. Step 7 requires
that plumbing; it is a named step-6 deliverable, not an assumption.

### 4.4 Surfacing the token — a CONNECTION-scoped seam

Revision 1 proposed a `ContinuationRows` interface on the driver `Rows`, reached
by type assertion from `*sql.Rows`. **That seam does not exist**: `database/sql`
gives no route from a `*sql.Rows` to the underlying `driver.Rows`, and `Raw` is
defined on `sql.Conn`, not on `Rows`. Revision 1's §4.4 was unimplementable.

The seam is **connection-scoped**. The driver `Conn` remembers the token of the
statement most recently executed on it; the caller reaches it through
`conn.Raw` — the mechanism already working at `rowdiff/run.go:142-149`:

```go
		if rerr := conn.Raw(func(dc any) error {
			ec, ok := dc.(*embedded.EmbeddedConnection)
			if !ok {
				return fmt.Errorf("driver conn is %T, want *embedded.EmbeddedConnection", dc)
			}
			ec.SetOptions(...)
			return nil
		}); rerr != nil {
```

So `EmbeddedConnection` gains `LastContinuation() (api.Continuation, error)`, set
when a `paginatingRows` reaches its page boundary and cleared when a new
statement starts on that connection. Java's precondition is copied — asking
before the page is exhausted is an error (`RecordLayerResultSet.java:110-113`).

To keep callers out of `package embedded`, the assertion targets a narrow
exported interface in `pkg/relational/sqldriver` which `*embedded.EmbeddedConnection`
satisfies:

```go
// ContinuationConn is implemented by this driver's connections; reach it with
// sql.Conn.Raw. LastContinuation returns the resume token of the most recent
// statement on this connection, or nil if that statement was exhausted.
type ContinuationConn interface {
	LastContinuation() (api.Continuation, error)
}
```

The caller must pin the connection (`db.Conn(ctx)`) for the token to be
meaningful — the constraint a connection-scoped seam imposes, stated in the
accessor's doc comment and pinned by G2. This is the one piece with no Java
analogue; Java gets it free from interface inheritance on `ResultSet`.

`api.ResultSet.Continuation()` (`api/resultset.go:42-44`) gains its production
caller at the same time: `cascades_generator.go:1831` stops reaching past it to
`rs.GetContinuation()`, which is what makes `liveContinuation` /
`exhaustedContinuation` live.

### 4.5 Reason codes — with the dead arm re-derived

Go's `ContinuationReason` (`api/continuation.go:26-56`) already matches Java's
enum by ordinal and `String()`. What is missing is the assignment, ported from
`RecordLayerResultSet.continuationReason()` (`:121-135`) including its order:

1. a row was consumed on this page, or the cursor still has rows →
   `USER_REQUESTED_CONTINUATION`;
2. the cursor terminated early (out-of-band scan/byte/time limit) →
   `TRANSACTION_LIMIT_REACHED`;
3. `NoNextReason == RETURN_LIMIT_REACHED` → `QUERY_EXECUTION_LIMIT_REACHED`;
4. otherwise → `CURSOR_AFTER_LAST`.

The order is load-bearing: `terminatedEarly` precedes `RETURN_LIMIT`, so a page
hitting both reports the transaction limit.

**Arm 2's reachability, re-derived under §4.2** (revision 1 asserted the arm
without checking; a dead arm must be deleted, not shipped): because rollover is
preserved, an out-of-band stop normally causes a transparent next page and never
reaches the user. It reaches the user only when the page is handed back — i.e.
**only when `MAX_ROWS` is also set** and the out-of-band stop lands at or before
that boundary. So arm 2 is **reachable, but exclusively in combination with
`MAX_ROWS`**. (With `FailOnScanLimitReached` set the path errors instead and no
token is produced.) Arm 2 is kept, and G13 pins it in the only configuration that
reaches it — a reason-code test without `MAX_ROWS` would assert a state the
engine cannot enter.

### 4.6 The record-layer boundary

`execution_state` is opaque to the SQL layer in both directions. The envelope
does not inspect, re-frame, version or re-wrap the record-layer continuation and
does not touch `KeyValueCursorContinuation`'s magic (`key_value_cursor.go:15-18`).
`LimitContinuation`'s disclaimer (`executor/executor.go:1606-1612`) stays true: it
remains internal to `executeLimit`, nested inside `execution_state`.

What changes: `execution_state` now genuinely leaves the process, so
`DIVERGENCES.md:1191`'s "**Both** are SAFE because they never cross an engine
boundary" is wrong twice over — there are three shapes (§9.4), and they now do
cross. The replacement justification is the per-path fence (§3.2), held by G6.

### 4.7 The plan-constraint gate — Java's fail-open, and Go's divergence

`ContinuedPhysicalQueryPlan.validatePlanAgainstEnvironment` (`QueryPlan.java:552-573`):

```java
            try {
                PlanValidator.validateContinuationConstraint(fdbRecordStore, getContinuationConstraint());
            } catch (final PlanValidator.PlanValidationException pVE) {
                metricCollector.increment(RelationalMetric.RelationalCount.CONTINUATION_REJECTED);
            }
            ...
            metricCollector.increment(RelationalMetric.RelationalCount.CONTINUATION_ACCEPTED);
```

Caught, counted, swallowed; control falls through to `CONTINUATION_ACCEPTED`.
`validateContinuationConstraint` itself throws (`PlanValidator.java:62-70`) and
Path 2's `validateHashes` propagates (`:80-89`), so the swallow is specific to
this arm.

**Combined with §2.4 this is sharper than revision 1 stated.** On Path 1 the
constraint is not one gate among three — it is the **only** external gate, and it
is the one that is swallowed.

**Go rejects** (§4.3 step 7), documents the divergence at the call site, and
reports upstream (§8.5).

**The asymmetry, named — because transport gives Go the same stale-plan question
it gives Java.** Under transport *both* engines execute a plan minted earlier, so
both face "is this plan still valid?". Revision 1 could pretend otherwise because
its re-plan mechanism produced a fresh plan every time; under the ruled mechanism
that comfort is gone and must not be implied. The difference is not that Go
avoids stale plans — **it is that Go honours the answer the constraint gives.**
Java computes the answer, counts it, and proceeds. G8 is re-derived against this:
it must demonstrate **wrong rows**, not mere acceptance. §9.2.

### 4.8 Error codes — 1:1 with Java, including the two Java routes to `XX000`

Java's mapping, enumerated (`ErrorCode.java:101`, `:177`):

- `INVALID_CONTINUATION` = **`24F00`** (Class 24, Invalid Cursor State)
- `INTERNAL_ERROR` = **`XX000`**

Java routes **structural malformation of the envelope** to `XX000` — unparseable
proto (`PlanGenerator.java:283-284`) and an envelope with neither
`compiled_statement` nor `copy_plan` (`:291-292`) — and **semantic mismatch of a
well-formed envelope** to `24F00`: hash mode (`PlanValidator.java:57`),
constraint (`:68`), binding hash (`:82`), plan hash (`:88`), the plan-hash
mismatch on both `PlanGenerator` arms (`:307`, `:340`), BEGIN/END sentinels
(`AstNormalizer.java:333-334`), literal type (`SemanticAnalyzer.java:953-958`),
and direct-access misuse (`EmbeddedRelationalStatement.java:274`).

Sites 1 and 2 are arguably Java bugs — a caller-supplied bad blob surfacing as an
internal error — but they are the pinned behaviour at 4.12.11.0 and the
conformance principle binds Go to reproduce them. Go does, and notes the
reservation at the call site.

**No new Go constant is needed.** `api.ErrCodeInvalidContinuation = "24F00"`
already exists (`api/errcode.go:69-71`), matches Java exactly, and is in the
Java-parity pin (`errcode_java_parity_test.go:27`) — **with zero producers**. Its
only non-test uses are its declaration and the `allErrorCodes` table
(`errcode.go:218`). This RFC gives it its first producer.

Today `cascades_generator.go:1215-1218` rejects with `0A000`
(`ErrCodeUnsupportedOperation`). That is arguably correct **today** — Java has no
"no resume surface" analogue — and it **flips** when the surface ships: `XX000`
for parse/structure, `24F00` for every semantic mismatch. G5 pins the flip.

**A Go-only version check would be a Go-only error path.** Java has no
version-mismatch throw site (§2.1), so adding one would need its own booking.
This RFC does not add one.

---

## 5. Rejected alternatives

**5.1 — Keep tokens engine-private; give the corpus runner an internal paging
hook.** **Rejected:** the corpus's value is that it executes the *shipped*
surface. A hook only tests call proves `paginatingRows` can page internally, not
that a client's token works, and leaves CQ-69.4 testing an unreachable path.
RFC-201 §2 principle 6 makes the corpus the specification of the *supported*
surface.

**5.2 — A Go-native token format.** **Rejected on the hard line:** continuations
are named wire-compat in CLAUDE.md. A Go-native format forecloses even *parsing*
a peer token, kills R1-R3 permanently, and would need migrating exactly when
cross-engine resume became valuable.

**5.3 — Add a Go-only field to `ContinuationProto`.** **Moot under the mechanism
ruling** — plan transport needs no added field; `compiled_statement.plan` is
Java's own field used for Java's own purpose. The prohibition stands anyway, and
**revision 1's argument for it was wrong** and is corrected here: revision 1
claimed a Java parser "would see an unknown field on a message it believes it
owns", implying breakage. Protobuf 3 does not work that way — an unknown field is
preserved and ignored. The real objections are (a) re-vendoring the proto stops
being a copy and becomes a merge, and (b) a field Java does not read cannot carry
meaning across the boundary, so it buys nothing the mode string does not.

**5.4 — REOPENED, AND IS NOW THE DESIGN. Serialize Go's plans into
`PRecordQueryPlan` and deserialize on resume.** Revision 1 rejected this on the
premise that deserializing a plan constitutes a second plan producer. That
premise is wrong (§4.3): it is transport. The one genuine obstacle revision 1
identified survives and is handled rather than used to reject the approach —
`RecordQueryInMemorySortPlan` has no lossless arm, so §4.1 fails loudly on it and
R1 names the fix. Every other node in Go's physical vocabulary has an arm.

**5.5 — Keep the statement-wide total under a Go-only option name alongside the
page size.** **Rejected:** two row-limit knobs with no reference definition for
their interaction. Note this rejection no longer carries revision 1's "`LIMIT N`
already expresses it" rationale, which §4.2 withdraws.

**5.6 — Re-plan from a canonical query text carried in the envelope.** Revision
1's mechanism. **Rejected because it cannot be built:** the canonical text is not
in `CompiledStatement` and is not recoverable from it — `TypedQueryArgument`
addresses literals by ordinal (`continuation.proto:56-72`) and the skeleton
exists only in `AstNormalizer`'s `sqlCanonicalizer`, never serialized. Carrying
it would require §5.3's forbidden field.

**5.7 — Resume via `Options.Name.CONTINUATION` only, skipping the SQL
statement.** **Rejected:** it does not run the corpus. Java's oracle drives every
page through `prepareStatement("EXECUTE CONTINUATION ?")`
(`QueryExecutor.java:278`). Path 2 is implemented anyway (§3.2) but is not
sufficient.

---

## 6. Sequencing

**Step 1 — plan serialization (`GO_V0`).** Go physical plans → `PRecordQueryPlan`
and back; the loud `RecordQueryInMemorySortPlan` rejection; a `GO_V0` plan hash
over the serialized form. Updates `plan_hash.go:14-18`'s comment, which today
says nothing wire-facing embeds the hash — that stops being true here and the
comment must move with the fact. Deliverables: G11, **G14 (the unrepresentable
census — this must land before step 4 depends on the resumable surface)**, and
the per-plan-class round-trip golden.

**Step 2 — the `AstNormalizer` port.** **Still built, on its own step**, though
§4.3 no longer needs it to recover query text. Two independent justifications:
 (a) it is the literals table `binding_hash` needs, and the **source of G4a's
 expected value** — recomputed by folding the reconstructed
 `(canonicalName, literal)` pairs per `AstNormalizer.java:553-555`, **never**
 seeded from the token's own hash;
 (b) it independently closes the documented plan-cache miss at
 `query_hash.go:33-36` — *"Java's AstNormalizer also PARAMETERIZES literals so
 `... WHERE x = 1` and `... WHERE x = 2` share a plan with different bindings. Go
 keys on the literal text, so those miss"*.
Deliverables: G7 and a plan-cache hit-rate measurement before/after.

**Step 3 — the proto and the envelope type.** Vendor
`proto/relational/continuation.proto`; generate; `ContinuationImpl`-equivalent
construction/parsing with BEGIN/END in `execution_state` presence. Deliverable:
G6.

**Step 4 — `MAX_ROWS` page semantics.** Retire the statement-wide total; delete
`TestFDB_RFC106a_MaxRowsStatementWide`; repoint `pageRowBudget`; range-check;
**preserve rollover**; wire reason codes. Deliverables: G1, G12, G13. At the end
of this step `maxRows.yamsql` still cannot pass — pages exist but no caller sees
them, and §9.5's `LIMIT` blocker is still open.

**Step 5 — surfacing.** `ContinuationConn` + `LastContinuation`; the
`api.ResultSet.Continuation()` production caller; the exhausted-only
precondition; delete the `OptContinuation` guard and flip its error codes (§4.8).
Deliverables: G2, G5.

**Step 6 — `EXECUTE CONTINUATION`.** The statement kind through `planOne`;
literals reconstruction; the three gates; the `QueryPlanConstraint` plumbing
(§4.3); `EXPLAIN EXECUTE CONTINUATION`; the RFC-191 and `DIVERGENCES.md` edits
(§8.3, §8.4). Deliverables: G3, G4, G8. **Graefe implementation lap here.**

**Step 7 — runner wiring.** The corpus runner consumes `maxRows:` and multi-page
`result:` sequences; ledger + assignment digest move in the same commit.
Deliverable: G9.

**Step 8 — the oracle (CQ-69.4).** `ForceContinuations` as a Go mode: every
eligible SELECT re-executed at `maxRows=1`, pages reassembled, compared to the
one-shot result, with Java's two loop invariants ported — "Received continuation
shouldn't be at beginning" (`QueryExecutor.java:367`) and
`MAX_CONTINUATIONS_ALLOWED = 100` (`:59-60`, `:352-354`). Deliverable: G10.

---

## 7. Gates

**G1 — `MAX_ROWS` is a page size, in BOTH the multiple and non-multiple cases.**
10 rows, `MAX_ROWS=2` → `2,2,2,2,2,0`, at-end only on the sixth. **And 10 rows,
`MAX_ROWS=3` → `3,3,3,1` with at-end on the FOURTH and NO trailing empty page.**
The trailing empty page appears only on an exact multiple; a test asserting it
unconditionally encodes an off-by-one as law. *Mutations:* (a) revert to the
statement-wide total; (b) drop the trailing empty page in the multiple case; (c)
emit a spurious trailing empty page in the non-multiple case; (d) make the page
per-transaction rather than per-execution.

**G2 — the token is reachable and round-trips.** `sql.Conn.Raw` yields
`ContinuationConn`; `LastContinuation` before exhaustion errors; the serialized
token re-parses to an equal envelope; and an **unpinned** connection does not
silently return another statement's token. *Mutations:* leave the field
unexported → compile failure; drop the precondition → the error assertion fails.

**G3 — page-by-page equals one-shot, with a page-count floor.** Concatenated
pages at `MAX_ROWS=1` equal the unpaged result, row for row and order for order,
**and the observed page count is asserted `>= 2`** — without the floor a
regression returning everything in one page passes trivially. Must include one
query per Go-private inner shape: aggregate, memory sort, **and distinct** (§9.4).
*Mutations:* drop a boundary row; duplicate a boundary row; collapse to one page.

**G4 — rejection, three independent directions, each mutated separately.**
 (a) **binding hash** (`24F00`) — resume with a doctored literals table →
 "Continuation binding does not match query". The expected value is recomputed
 from the reconstructed pairs (§6 step 2); the test must **fail** if the
 implementation seeds it from the token, which is Java's vacuous form (§2.4).
 (b) **plan hash** (`24F00`) — a doctored `plan_hash` against an intact plan.
 Under transport this detects **blob/serializer drift, not schema change**; a
 test calling it schema-change coverage is testing nothing.
 (c) **plan constraint** (`24F00`) — resume after a schema change that
 invalidates the constraint → rejected.

**On M7 (exhibit the constraint-fails-while-hash-equal case, or drop G4c):**
under transport no exhibit is needed, because the case is the **generic** one.
The plan hash is computed over the plan carried *in the token*, so it is equal by
construction for every uncorrupted token — including every token whose
environment changed underneath it. The constraint is therefore not supplementary
but **the sole schema gate**; G4c is load-bearing and G4b cannot substitute for
it. (This is a consequence of the mechanism ruling: under revision 1's re-plan
design the plan hash *would* have been the schema gate, which is why the question
was open then and is closed now.)

**G5 — the reversal is pinned, not merely deleted.**
`TestOptContinuation_RejectsLoudly` today asserts an error, code
`ErrCodeUnsupportedOperation` (`0A000`), and the substring `"engine-private"`
(`continuation_option_test.go:29-35`). Replaced by: a valid `OptContinuation`
*resumes*; a foreign-mode one is rejected with `ErrCodeInvalidContinuation`
(`24F00`) by the Go-side Path-2 mode fence; a structurally malformed one with
`XX000` (§4.8). *Mutation:* restore the guard → the resume assertion reds; leave
the code at `0A000` → the code assertions red.

**G6 — wire-format golden, both directions, both fences.** A committed byte
golden of a fully-populated Go envelope asserted field-by-field against the Java
schema; a **Java-authored token fixture** that Go parses, reads
`version`/`reason`/`execution_state` from, and then refuses for plan resume — on
Path 1 by mode and on Path 2 by mode. *Mutations:* renumber a field; convert the
fence to a schema divergence; drop the Go-side Path-2 mode check so the Java
token falls through to a generic hash mismatch.

**G7 — `binding_hash` agrees with Java where Java is self-consistent, and
diverges where it is not.** Cross-checked goldens through the conformance server
for integer / string / float / boolean / null literals; **plus an explicit
assertion of the bytes divergence** (§4.1, §9.3) so the deliberate difference is
pinned rather than latent. *Mutations:* change the murmur seed or the
`Objects.hash` fold; make bytes hash by identity.

**G8 — the §4.7 divergence produces WRONG ROWS, not mere acceptance.** Same
transported token + the same invalidating schema change on both engines: Java
executes the stale plan and returns a result the current schema contradicts; Go
rejects with `24F00`. Asserting only "Java accepted" would leave the divergence
looking cosmetic. §9.2 records the measurement. If Java rejects by another route,
the divergence note is deleted and this becomes an equivalence assertion.

**G9 — the ledger transition, reported not predicted.** In one commit:
`inner_skips{unsupported:continuation=16}` → `0`;
`file_skips{unsupported:continuation=1}` → `0` (the carrier being
`initial-version/mid-query.yamsql`, §0.4); `pinnedFileTotal` unchanged at `238`;
`pinnedAssignmentDigest` updated. **The pass delta as drafted is `+0`**; the
per-file before/after assignment is the deliverable. `maxRows.yamsql` stays
non-passing until §9.5's `LIMIT` blocker is closed, and its file class stays
`conformance:go-accepts-what-java-rejects`.

**G10 — the oracle is instrumented and floored.** `ForceContinuations` as a Go
mode with the per-run count of queries actually multiplied REPORTED and FLOORED.
*Mutation:* make the eligibility predicate return false → the floor reds.

**G11 — the in-memory-sort rejection is loud and specific.** Minting a token for
a plan containing `RecordQueryInMemorySortPlan` fails with an error naming the
node and citing R1. *Mutations:* encode it lossily as `PRecordQuerySortPlan` →
the error assertion reds, **and** a companion test showing a `NULLS FIRST` sort
resuming in the wrong order under that lossy encoding reds too — the second
mutation is what proves the rejection is protecting correctness and not taste.

**G14 — the unrepresentable set is MEASURED before anything depends on it.** An
enumeration test walks every plan the corpus produces and reports, per Go plan
type, whether it has a `PRecordQueryPlan` encoding: representable, representable
only via a differently-named Java arm (with the mapping named), or
unrepresentable. The census is **pinned as a number and a per-type assignment**,
in the `pinned_ledger_test.go` style, so a plan type silently gaining or losing
an encoding fails loudly. This is the gate that keeps §3.3's corrected claim
honest: the name-level survey there shows the unrepresentable set is larger than
one, and G14 is what turns "larger than one" into an exact, maintained figure.
*Mutation:* remove one arm from the encoder → the census count moves and the
digest reds.

**G12 — transaction rollover stays transparent.** A scan long enough to exceed
the 5-second transaction limit, with **no** `MAX_ROWS`, returns the complete row
set with no user-visible token and no error. *Mutation:* make the rollover
boundary yield a token → the completeness assertion reds.

**G13 — reason codes, in the only configurations that reach them.**
`CURSOR_AFTER_LAST` on exhaustion; `QUERY_EXECUTION_LIMIT_REACHED` at a
`MAX_ROWS` boundary; `TRANSACTION_LIMIT_REACHED` **only** with `MAX_ROWS` set
plus a scan limit that fires first (§4.5); `USER_REQUESTED_CONTINUATION` when
rows remain. *Mutation:* swap arms 2 and 3 → the combined-limit case reds.

---

## 8. Residues

**8.1 — Cross-engine plan resume**, fenced per path (§3.2), with R1/R2/R3 and the
**R2-after-R3** ordering constraint (§3.4). Not filed as a TODO: it is a gate in
this document with three named measurements.

**8.2 — `COPY` continuations.** `CopyPlan` is copied so the tag is reserved and a
Java `COPY` token is rejected by name; `COPY` is not implemented.

**8.3 — RFC-191 condition (D) is re-derived HERE, and both sites are edited in
this RFC's step-6 commit.** Today `rfcs/191:774-779` and `TODO.md:905-910`
conclude that an InJoin/InUnion plan-choice divergence "is not a cross-engine
wire concern" *because* "Go continuations are engine-private by construction: a
Java-minted token is rejected outright". After this RFC a Java-minted token is
**parsed**, so the stated reason no longer holds.

**The conclusion survives, on the per-path fence rather than on outright
rejection — and transport strengthens it:**

- A Java token is refused before execution on both paths (§3.2): by mode on Path
  1, and by mode-then-hash on Path 2.
- Under transport the plan and its continuation travel **together in one token**,
  so a continuation is never decoded by a plan-tree shape other than the one that
  produced it — by construction, not by luck. `ExecutePlan`'s type-switch
  (`executor/executor.go:88-120`) and `UnsupportedContinuationError` (`:71-80`)
  remain as the loud backstop for the shapes that cannot resume at all.

Both sites get that re-derivation, with the "rejected outright" sentence and the
`cascades_generator.go:1205-1216` citation replaced.

**8.4 — `DIVERGENCES.md` is corrected in three places, not just amended.**
(i) `:1161-1206` — the engine-private entry becomes the record of the per-path
fence. (ii) `:1187-1189` — the in-memory-sort "no Java counterpart at all" claim
is **false** and is rewritten per §9.4. (iii) `:1191` — "**Both** are SAFE because
they never cross an engine boundary" is wrong twice: three shapes, and they now
cross. The third shape is added.

**8.5 — The upstream report is a named deliverable, not an intention — and it
now carries THREE measured findings, not one.** An unreported upstream bug is a
divergence Go maintains forever. The deliverable is one filed fdb-record-layer
issue, with its URL recorded at the `DIVERGENCES.md` divergence entry, covering:

1. **The plan-constraint fail-open** (`QueryPlan.java:561-565`) — measured inert
   in §9.2, with the reproducer; strengthened by G8's wrong-rows outcome before
   filing. Also note the metric defect: the same continuation increments both
   `CONTINUATION_REJECTED` and `CONTINUATION_ACCEPTED`.
2. **`Enum.valueOf` on a caller-supplied mode string** (`PlanValidator.java:55`)
   — measured in §9.1 to turn a continuation-validation failure into SQLSTATE
   `XXXXX`. Moving the membership test before the `valueOf` would yield the
   `24F00` the code already has an error for.
3. **`binding_hash` is not reproducible for bytes-literal queries**
   (`AstNormalizer.java:555` over a `byte[]` with identity `hashCode`) —
   measured in §9.3. This breaks Java's *own* Path-2 resume for such queries, so
   it is not merely a Go-interop concern.

All three were found by instrumenting Java rather than by reading it, which is
why they are worth filing rather than absorbing.

**8.6 — `TypedQueryArgument` scope handling for prepared parameters** follows
`deserializeArgumentsForParameters` (`PlanGenerator.java:367-368`).

**8.7 — A proto-package namespace squat to fix while we are here.**
`proto/relational/distinct_continuation.proto:11` declares
`package com.apple.foundationdb.relational.cursors;` for a **Go-private** message.
Java has no proto in that package today, so there is no collision now, but the
squat means a future upstream message there would collide by full name. §9.4.

---

## 9. Measurements

Revision 1 twice wrote that this repo cannot execute Java. **That was false.**
`conformance/sql_plan_steps.java` builds a full `EmbeddedRelationalEngine` and
registers `EmbeddedRelationalDriver`; Go drives it over HTTP `/invoke`
(`plandiff/httpclient.go:53`). Both sentences are deleted. §9.1-§9.3 are run
through a new `continuationProbe` step; the probe is **committed as a test**, per
CLAUDE.md, because the conclusions below outlive the measurement.

Instrument: `@ConformanceStep("continuationProbe")` in
`conformance/sql_plan_steps.java`, driven by
`conformance/continuation_probe_conformance_test.go`. Run:

```
bazelisk test //conformance:conformance_test --test_output=streamed \
  --test_arg="--ginkgo.focus=ContinuationProbe" --test_arg="--ginkgo.v"
Ran 1 of 1362 Specs in 6.877 seconds
SUCCESS! -- 1 Passed | 0 Failed | 0 Pending | 1361 Skipped
```

Five mutation directions, each RED separately: revert the step (`No conformance
step found with name: continuationProbe`); `GO_V0`→`VC0`; false constraint→true;
false constraint + perturbed `plan_hash`; bytes literal→int literal. The last two
matter most — they pin that the probe *can* observe Java rejecting, so §9.2's
non-rejection is not a blind spot.

### 9.1 Java's Path-1 rejection of a `GO_V0` token — CONFIRMED, and the fence is NOT a clean rejection

```
probeA_threw:          true
probeA_exceptionClass: com.apple.foundationdb.relational.api.exceptions.ContextualSQLException
probeA_message:        java.lang.IllegalArgumentException: No enum constant
                       com.apple.foundationdb.record.PlanHashable.PlanHashMode.GO_V0
probeA_causeChain:     ContextualSQLException <- RelationalException <- java.lang.IllegalArgumentException
probeA_sqlState:       XXXXX
```

The Path-1 fence works: a `GO_V0`-moded token cannot execute in Java.
`Enum.valueOf` (`PlanValidator.java:55`) runs before the membership test
(`:56`), so the foreign mode never reaches the typed rejection.

**But the SQLSTATE is `XXXXX`, not `24F00`** — `ErrorCode.UNKNOWN`
(`ErrorCode.java:172`), not `INVALID_CONTINUATION` (`:101`). §3.2's earlier
phrasing, that the token "dies in Java's own existing gate", is **too
flattering**: it dies *before* the gate, in the unknown-internal-error channel.
The fence is **loud but not well-formed**. Three consequences, folded:

1. §3.2's claim is narrowed to what was measured: Java refuses the token by
   throwing, and no Go plan is ever executed by Java. That is the safety
   property the fence must deliver, and it holds.
2. Any documentation or client guidance that promises a `24F00` on a
   cross-engine token would be **wrong**. Go's own fence raises `24F00`
   deliberately (§4.8); Java's raises `XXXXX`. G6 asserts both values rather
   than assuming symmetry.
3. This is a second, smaller upstream finding to fold into §8.5's report: a
   caller-supplied mode string reaching `Enum.valueOf` unguarded turns a
   continuation-validation failure into an unknown internal error.

### 9.2 The Path-1 plan-constraint fail-open — CONFIRMED, by execution as well as by source

The measured continuation's real constraint is
`AND(compatible_type_evolution_predicate, database_object_dependencies_predicate)`
— the two predicates Go must port for §4.3 step 7. Replacing **only**
`compiled_statement.plan_constraint` with `constant_predicate { value: false }`,
leaving plan, `plan_hash`, `binding_hash`, `execution_state` and literals
byte-identical:

```
probeB_control_rowsReturned: 3      (untouched bytes — baseline)
probeB_threw:                false
probeB_rowsReturned:         3      (constant-FALSE constraint — identical)
probeB_falsePlanConstraintProto: predicate { constant_predicate { ... value: false } }
```

**A continuation whose plan constraint evaluates FALSE executes and returns
every remaining row.** The gate is inert, not merely lenient. The source is as
read (`QueryPlan.java:561-565`), and the caught `pVE` is bound and never used;
note also that the same continuation increments **both** `CONTINUATION_REJECTED`
and `CONTINUATION_ACCEPTED`, so the metric pair cannot be read as a rejection
count either.

The contrast is established in the same instrument: with the plan hash also
perturbed, Java **does** reject —

```
probeB_threw: true, probeB_sqlState: "24F00",
probeB_message: "cannot continue query due to mismatch between serialized and actual plan hash"
```

so the constraint path is the odd one out, not general leniency, and the probe
demonstrably *can* see Java refuse.

**What this does NOT yet show, stated so G8 is not mistaken for satisfied:** a
constant-FALSE constraint proves the gate is inert; it does not exhibit the
*wrong rows* a real invalidating schema change would produce. G8 remains
outstanding and is the step-6 deliverable, and §8.5's upstream report waits on
it — an inert gate is a defect, but the report is stronger with the consequence
attached.

### 9.3 `binding_hash` stability for a bytes-literal query — CONFIRMED unstable

```
probeC_bytes_hash1: 1045894765
probeC_bytes_hash2: 1264480623
probeC_bytes_equal: false
probeC_int_hash1:   677421547
probeC_int_hash2:   677421547
probeC_int_equal:   true
```

Two executions of the identical query text `SELECT * FROM T WHERE B = x'0a0b'`,
**same connection, same JVM**, produce different binding hashes. The integer
control `WHERE N = 3` agrees, and agreed at `677421547` across four separate JVM
launches — so the integer hash is stable even across processes while the bytes
hash is not stable within one. `AstNormalizer.java:145` routes
`BytesConstantContext` through `ParseHelpers.parseBytes`, which returns a
`byte[]` (`ParseHelpers.java:147`); `Objects.hash` → `Arrays.hashCode(Object[])`
→ `byte[].hashCode()` → identity.

This settles §4.1's decision on evidence rather than on reading: **Go hashes
bytes literals by content.** There is no Java value to reproduce — Java cannot
reproduce its own twice in one process — so "diverging from Java" here is not a
choice between two definitions but the only definition under which
`binding_hash` means anything for such a query. G7 pins the agreement for the
stable literal kinds and pins the bytes divergence explicitly.

*(A consequence worth noting for Java users, not just for Go: a Java client
resuming a bytes-literal query through Path 2 will have its own token rejected
by its own binding-hash check. That is an upstream defect in the same family as
§9.1 and joins the §8.5 report.)*

### 9.4 The third Go-private inner shape, and the `MemorySortCursorContinuation` correction

**A third Go-private `execution_state` shape exists** and
`DIVERGENCES.md:1173-1189` misses it:
`proto/relational/distinct_continuation.proto:25-31`

```proto
message DistinctHashContinuation {
    // innerContinuation is the wrapped inner cursor's resume bytes.
    optional bytes innerContinuation = 1;
    // seenKeys are the packed dedup keys (distinctKey) already emitted.
    repeated bytes seenKeys = 2;
}
```

consumed at `executor/distinct_stream.go:267` and `executor/executor.go:1849`.
Unlike the other two it does not even reuse a Java message name — it is a wholly
new message, and its placement in `proto/relational/` rather than `proto/apple/`
is itself the statement of privacy. Its header's no-counterpart claim is
**accurate**, independently confirmed: Java's `RecordQueryUnorderedDistinctPlan`
holds `final Set<Key.Evaluated> seen = new HashSet<>();` as a per-call local and
passes the **inner's** continuation down untouched
(`RecordQueryUnorderedDistinctPlan.java:109-111`), so there is nothing to be
compatible with. R3's condition for this shape is therefore "the plan that
produces it is not transported", not "make it match".

**`DIVERGENCES.md:1187-1189` is FALSE.** It reads:

> - **In-memory sort** (`MemorySortContinuation`): a Go extension with no
>   Java counterpart at all (Java's Cascades has no physical sort
>   operator), so there is nothing Java could produce or consume.

Java has all of it: the message
(`fdb-record-layer-core/src/main/proto/record_sorting.proto:33-37`), the
continuation class (`sorting/MemorySortCursorContinuation.java:42`, parse entry
`:108`), the cursor that produces and parses it on every page
(`sorting/MemorySortCursor.java:82-83,103,112,146`), and the physical operator
that drives it (`query/plan/sorting/RecordQuerySortPlan.java:112`,
`MemorySortCursor.createSort(...)`). **Go vendors Java's message verbatim** at
`proto/apple/record_sorting.proto:33` and encodes into it
(`executor/continuation.go:993`, whose own comment says "using **Java's**
MemorySortContinuation proto") — you cannot vendor a message with no counterpart.
`DIVERGENCES.md` also contradicts itself at `:1082-1084` ("Go-owned typed payload
inside **Java's** MemorySortContinuation wrapper") and at `:698`.

Only the parenthetical survives, and only in narrowed form: Java's **Cascades**
has no implementation rule that emits `RecordQuerySortPlan`; the legacy
`RecordQueryPlanner` does.

**What is actually Go-private** is (i) the payload inside `SortedRecord.message`
— Go writes a 3-element JSON array where Java writes record-proto bytes
(`continuation.go:1005-1012`) — and (ii) the repurposed `minimum_key` as an
emit-phase presence marker (`continuation.go:967`,
`sortInnerExhaustedMarker = []byte{1}`), which is not Java's minimum key. **Both
are checkable R3 work against a real Java counterpart**, which the "no
counterpart" framing had made look impossible. The safety argument survives; its
premise does not.

### 9.5 `maxRows.yamsql`'s second blocker — and why revision 1's `LIMIT` guidance was backwards

`maxRows.yamsql` has 13 active tests. Seven carry `maxRows:` and are skipped
`unsupported:continuation` (inner), contributing 0 to `QueriesRun`. **Four run,
and every one of them asserts `error: UNSUPPORTED_QUERY` (`0AF00`) on a `LIMIT`
or `OFFSET` query that Go executes successfully:**

```
select ta.* from ta LIMIT 2 OFFSET 2;
select * from ta limit 5
select p.* from (select ta.* from ta where ta.a > 5 limit 1) as p;
select p.* FROM ta as p where exists (select * from ta where ta.a = p.a limit 1);
```

Only the second appears in the pin because the block aborts at the first failing
assertion under a deterministic shuffle (`testblock.go:181-192`); the other three
are a real, unmeasured tail. The file's booked class is
`javacorpus/gaps.go:91`:

```go
	{"maxRows.yamsql", SkipConformanceGoAccepts, `"select * from ta limit 5": expecting statement to throw an error 0AF00, however it succeeded`, "CQ-72"},
```

**The direction is the opposite of what a reader might assume, and of what
revision 1 implied.** Go is the *permissive* side: `LIMIT`/`OFFSET` is a Go-only
read-side extension (RFC-082; `DIVERGENCES.md:696`;
`plan_visitor.go:1832 parseLimitClause`) that Java rejects. This is the corpus's
**single** `conformance:go-accepts-what-java-rejects` entry. Revision 1's §4.2
told callers wanting N rows total to "write `LIMIT N`" — i.e. to lean on exactly
the construct booked as an unreviewed widening of the shared surface, in the very
file this RFC is trying to turn green. That guidance is withdrawn (§4.2).

**Consequence for the gate:** page semantics alone do not unblock this file. Both
are required — the continuation envelope **and** a settled `LIMIT` conformance
question (Go declines `LIMIT` to match Java, or the widening is reviewed and the
corpus expectation overridden). §4.2 names it as the second prerequisite; G9
records that `maxRows.yamsql` stays non-passing until it is closed.
