# RFC 024 — Plan-Cache Compatibility with Java

**Status:** Draft — resolves RFC 022 §4.-0.25 (plan-cache-key compatibility spec).
**Author:** dayshift-46 (2026-04-24).
**Scope:** Decide whether the Go Cascades plan cache must produce hashes identical to Java's `QueryCacheKey.hash`, and what "compatibility" concretely means for this cache given Java's implementation shape.

## TL;DR

**Hash-identical cache keys with Java is NOT a goal.** Java's `RelationalPlanCache` is strictly per-process, in-memory (Caffeine), and never serialises its key or its values. There is no Java-side wire format to preserve, no distributed deployment to share, and no client (Java or otherwise) that reads Java's cache externally. Go's 4.4 cost model should optimise for Go-native simplicity rather than chase Java parity. If cross-engine distributed plan caching becomes a real requirement, it gets designed as a new feature shared between engines — not as a retrofit of today's in-process caches.

## Java's plan cache — ground truth

`fdb-relational-core/src/main/java/com/apple/foundationdb/relational/recordlayer/query/cache/`:

| File | What it is |
|---|---|
| `RelationalPlanCache` | Three-tier `MultiStageCache<String, QueryCacheKey, PhysicalPlanEquivalence, Plan<?>>` |
| `MultiStageCache` | Generic cache-of-caches-of-caches, backed by **Caffeine** (`com.github.benmanes.caffeine.cache.Caffeine`) |
| `QueryCacheKey` | Primary-cache key: canonical query string, planner config, schema template version, user version, auxiliary metadata, query hash |
| `PhysicalPlanEquivalence` | Secondary-cache key: query-plan constraints + evaluation context |

Two observations pin the compatibility question:

1. **Caffeine is in-process.** No serialiser, no persister, no replicator. A Java JVM crash empties the cache. There's no API to extract `QueryCacheKey → Plan` bytes and ship them to another process.

2. **`QueryCacheKey.hashCode()` is JVM-local.** The memoized hash is built via `Objects.hash(hash, schemaTemplateVersion, plannerConfiguration, userVersion, auxiliaryMetadata)`, and `Objects.hash` delegates to `Arrays.hashCode(Object[])`, which in turn uses each element's `hashCode()`. `String.hashCode()` is spec-stable across JVMs, but `PlannerConfiguration.hashCode()` is whatever the JVM derives from its internal field layout and is not contracted stable across versions or implementations. So even if a caller reached into `QueryCacheKey.hashCode()` intending to move it across engines, the result is already not portable Java-to-Java.

3. **The `hash` field inside `QueryCacheKey` IS stable** — it's computed by `AstNormalizer` from the query text + literal positions. That's Java-to-Java stable as long as `AstNormalizer` and the parse-tree shape don't change (and they do, across Java release tags). But this field is one of many inputs to the key; it doesn't alone determine cache hit/miss.

4. **No wire format exists for Plans.** `Plan<?>` is not `PlanSerializable` in its full form (the Cascades serialisation goes only as far as `RecordQueryPlan` for replay). Cached plans can't cross process boundaries.

Conclusion: Java offers NO cache-sharing primitive between engines today. A Go-side goal of "match Java's hash" would be optimising against no observable behaviour.

## Go's current state

Per nightshift-45's closing answer on RFC-022 open-question 3:

> The naive `query.Generator` (Phase 1a) today does NOT expose a plan-cache key at all — it parses + plans + executes each statement inline per call with no cross-statement reuse.

Neither side has a cross-engine wire format. There's nothing to preserve.

## The three questions, answered

### (a) Are we targeting hash-identical cache keys with Java?

**No.** The goal is not expressible against Java's behaviour. Java's cache key isn't a stable public interface; its `hashCode()` is process-local Caffeine bookkeeping. Matching `int hash == int hash` across engines would require:
- Pinning Java to a specific tag (so `AstNormalizer` + `PlannerConfiguration` serialisation don't drift).
- Re-implementing `AstNormalizer`'s hash algorithm byte-identical in Go.
- Re-implementing `PlannerConfiguration`'s Java-internal field layout in Go.
- Re-implementing `Objects.hash`'s element-wise dispatch.

The resulting Go code would be a Java-behaviour-emulator coupled to a specific JVM release. Every Java point bump becomes a required Go change. The cost is sustained, and there's no consumer on the other end.

### (b) If yes, which Java version do we pin?

N/A. Decision (a) closes this.

### (c) If no, what's the migration path for clients expecting cross-engine cache sharing?

**There are no such clients.** Java's RelationalPlanCache is not a public cache-sharing contract. If a future user-visible distributed plan cache is designed (e.g. a Redis-backed secondary for RPC servers, or a cache warmed from a Parquet file at startup), it becomes a new cross-engine spec — with its own wire format, its own versioning, and its own explicit compatibility promises. That spec would be driven by the distributed-cache requirements, not retrofitted from Java's current in-process shape.

## Consequences for Phase 4.4 (cost model)

Free to diverge. Ship a Go-native cost model that:
- Produces plans semantically equivalent to Java's (correctness contract — same result sets).
- Does not need to produce plans with the same structural shape or cost values.
- Does not need to produce plans whose `planHash` matches Java's.
- Should be simple enough that future shifts can tweak it without breaking contract.

Plan-TREE equivalence (structural) remains a scoped goal as measured by the RFC-022 §4.-1 plan-equivalence harness — but that harness's purpose is discovering *where Go and Java diverge* so we can decide case-by-case which divergences are bugs (rule missing) vs design (cost model prefers a different plan shape). It is not a gate that all plans must be byte-identical.

## What we DO need to preserve

Even without cross-engine caching, some cache-related invariants still apply to the Go side:

1. **Deterministic hash within Go.** Same SQL + same schema + same planner config → same `QueryCacheKey.Hash()` across Go processes (so a Go distributed cache, if ever added, works at all). Requires canonical query string (literals normalised, whitespace collapsed) + deterministic hash function.

2. **Cache-key stability across Go point releases (within a major).** A Go 1.2 server should hit cache entries populated by a Go 1.1 server running against the same schema. Breaking this is a back-compat event, not a routine change.

3. **Cache-key sensitive to schema version.** If the user alters the schema (new column, new index), old plans must NOT be hit. Java keys on `schemaTemplateVersion` + `userVersion` + planner config; Go should do the same.

4. **Schema name segregation.** Java `QueryCacheKey` carries `auxiliaryMetadata` (schema template name) explicitly to prevent s1's plan from firing on s2. Same SQL + same literals → different plan if tables resolve differently. Go must do the same. The exact encoding is free.

## Phase 4.0 checklist item

Add to Phase 4 work: the Go `QueryCacheKey` type must land WITH:
- A doc comment referencing this RFC.
- A stable hash function (e.g. `hash/fnv` or SHA-256 truncated) — NOT `uintptr` / map-internal JVM-style hash.
- Test fixtures pinning hash stability across Go versions (a golden file of `(SQL, schema-version) → hex-hash`).

## Non-goals

- Does not specify the exact canonicalisation rules for the query string — that's a 4.0-proper deliverable.
- Does not design a distributed cache. If one is ever designed, it's scoped from scratch.
- Does not forbid reading Java's `QueryCacheKey.getHash()` out-of-band for testing/debugging. If a diagnostic tool finds it useful, fine — it just isn't a public contract.

## Open questions

1. **Is there appetite for a Go-side distributed plan cache (e.g. shared across gRPC servers)?** If yes, it's a separate RFC and probably a Phase 9 item (gRPC server in RFC 021 / TODO §Phase 9). If no, Go stays per-process like Java.
2. **Do we want to emit a diagnostic `planHash` in `EXPLAIN` output that is stable across engines?** Would help debugging "why doesn't Go pick Plan X that Java picked." Possible but not required; can be bolted on post-4.4.
