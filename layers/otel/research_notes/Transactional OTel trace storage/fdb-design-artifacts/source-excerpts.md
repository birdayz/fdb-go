# Short excerpts supporting the design note

Accessed 2026-10-27 UTC. Source URLs/ref caveats and original download digests are in `source-manifest.md`. HTML tags are removed, whitespace normalized and common entities decoded. Excerpts support factual premises only; proposed layouts, algorithms, example arithmetic and gates in the note are not upstream claims. Fenced GenAI snippets preserve literal upstream Markdown; relative links inside them are source-relative text, not links into this worktree.

## fdb-known-limitations.txt

Source: https://apple.github.io/foundationdb/known-limitations.html

> onDB core. Large transactions Transaction size cannot exceed 10,000,000 bytes of affected data. Keys, values, and ranges that you write are included as affected data. Keys and ranges that you read are also included as affected data, but values that you read are not. Likewise, conflict ranges that you add or remove (using a snapshot read or a transaction option ) are also added or removed from the scope of a

> rounds. Large keys and values Keys cannot exceed 10,000 bytes in size. Values cannot exceed 100,000 bytes in size. Errors will be raised by the client if these limits are exceeded. Workarounds FoundationDB provides efficient ways to design keys and values to work around this limitation. Spinning HDDs FoundationDB is only designed for good performance with rotational disk drives when using the durable memory

> five seconds. In particular, after 5 seconds from the first read in a transaction: subsequent reads that go to the database will usually raise a transaction_too_old error (although reads cached by the client will not); a commit with any write will raise a transaction_too_old or not_committed error . Long running read/write transactions are a design limitation, see the discussion in Anti-Features . Workarou

> lity. Not a security boundary Anyone who can connect to a FoundationDB cluster can read and write every key in the database. There is no user-level access control. External protections must be put into place to protect your database. Current limitations These limitations do not reflect fundamental aspects of our design and are likely to be resolved or mitigated in future versions. Administrators should be a

## fdb-read-semantics.txt

Source: https://apple.github.io/foundationdb/developer-guide.html

> cases, commit() will raise a commit_unknown_result exception. The on_error() function treats this exception as retryable, so retry loops that don’t check for commit_unknown_result could execute the transaction twice. In these cases, you must consider the idempotence of the transaction. Note The Python &#64;fdb.transactional decorator and its counterparts in the other language APIs do not check for commit_u

> your transaction idempotent: Avoid generating IDs within the retry loop. Instead, create them prior to the loop and pass them in. For example, if your transaction records an account deposit with a deposit ID, generate the deposit ID outside of the loop. Within the retry loop, check for the completion of one of the transaction’s unique side effects to determine if the whole transaction has previously comple

## rl-index-maintainer.txt

Source: https://github.com/FoundationDB/fdb-record-layer/blob/4.14.2.0/fdb-record-layer-core/src/main/java/com/apple/foundationdb/record/provider/foundationdb/IndexMaintainer.java

> rent.CompletableFuture; /** * Object responsible for translating record saves and deletes into updates to a secondary index. * * This class is essentially an interface, implementing only the storage of an {@link IndexMaintainerState}. * A functional base class is provided by {@link com.apple.foundationdb.record.provider.foundationdb.indexes.StandardIndexMaintainer}. * * An {@code IndexMaintainer} instance i

> sage> CompletableFuture<Void> updateWhileWriteOnly(@Nullable FDBIndexableRecord<M> oldRecord, @Nullable FDBIndexableRecord<M> newRecord); /** * Serialize the old/new record pair into an {@link com.google.protobuf.Any}-packed message describing the deferred * index update. The data will be later provided to {@link #updateFromQueue(Any)} to implement the index update. * <p> * This is only called for maintaine

> the index is not {@linkplain #isIdempotent() idempotent}, * then during an index build, it may need to perform additional checks to make sure each record is only * added to the index once. In particular, it can check the {@link com.apple.foundationdb.async.RangeSet} * associated with the index build to check to see if the record has already been indexed, and then decide * to update (or not update) the index

## rl-indexing-base.txt

Source: https://github.com/FoundationDB/fdb-record-layer/blob/4.14.2.0/fdb-record-layer-core/src/main/java/com/apple/foundationdb/record/provider/foundationdb/IndexingBase.java

> tore, index); return rangeSet.firstMissingRangeAsync() .thenCompose(range -> { if (range != null) { allReadable.set(false); return AsyncUtil.DONE; } // Index is built because there is no missing range. return store.markIndexReadable(index) // markIndexReadable will return false if the index was already readable .thenApply(vignore2 -> null); }); }) ).thenApply(ignore -> allReadable.get()), common.indexLogMes

> ; return AsyncUtil.DONE; } // Index is built because there is no missing range. return store.markIndexReadable(index) // markIndexReadable will return false if the index was already readable .thenApply(vignore2 -> null); }); }) ).thenApply(ignore -> allReadable.get()), common.indexLogMessageKeyValues("IndexingBase::markReadableIfBuilt")); } @Nonnull public CompletableFuture<Boolean> markIndexReadable(boolea

## rl-bitmap.txt

Source: https://github.com/FoundationDB/fdb-record-layer/blob/4.14.2.0/fdb-record-layer-core/src/main/java/com/apple/foundationdb/record/provider/foundationdb/indexes/BitmapValueIndexMaintainer.java

> ALUE; public static final int DEFAULT_ENTRY_SIZE = 10_000; public static final int MAX_ENTRY_SIZE = 250_000; private final int entrySize; private final boolean unique; public BitmapValueIndexMaintainer(IndexMaintainerState state) { super(state); final String sizeArgument = state.index.getOption(IndexOptions.BITMAP_VALUE_ENTRY_SIZE_OPTION); entrySize = sizeArgument != null ? Integer.parseInt(sizeArgument) :

> 8)); state.transaction.mutate(MutationType.BIT_AND, key, bitmap); Arrays.fill(bitmap, (byte)0x00); state.transaction.mutate(MutationType.COMPARE_AND_CLEAR, key, bitmap); } else { if (unique) { // Snapshot read to see if the bit is already set. CompletableFuture<Void> future = state.transaction.snapshot().get(key).thenAccept(existing -> { if (existing != null && (existing[offset / 8] & (byte)(1 << (offset %

## rl-text.txt

Source: https://github.com/FoundationDB/fdb-record-layer/blob/4.14.2.0/fdb-record-layer-core/src/main/java/com/apple/foundationdb/record/provider/foundationdb/indexes/TextIndexMaintainer.java

> f the tokenizer of that name. The version of the tokenizer used to serialize * each record is stored by this index maintainer, so if an index's tokenizer version changes, then this * index maintainer will continue to use the older tokenizer version to tokenize the fields of any records * present in the index prior to the version change. This guarantees that for every record, the same tokenizer * version is

> prior to the version change. This guarantees that for every record, the same tokenizer * version is used when inserting it and when deleting it. If one wants to re-tokenize a record following * a tokenizer version change, then if one takes an existing record (tokenized with an older version) and saves * the record again, then that record will be re-indexed using the newer version. * </p> * * <p> * Because

## otel-common.txt

Source: https://opentelemetry.io/docs/specs/otel/common/

> epts Status : Stable AnyValue AnyValue is either: a primitive type: string, boolean, double precision floating point (IEEE 754-1985), or signed 64 bit integer, a homogeneous array of primitive type values. A homogeneous array MUST NOT contain values of different types. a byte array. an array of AnyValue , a map&lt;string, AnyValue> , an empty value if supported by the language, (e.g. null , undefined in Jav

> ring keys to AnyValue values. The keys in the map are unique (duplicate keys are not allowed). Case sensitivity of keys MUST be preserved. Keys that differ in casing are treated as distinct keys. The representation of the map is language-dependent. The implementation MUST by default enforce that the exported maps contain only unique keys. The enforcement of uniqueness may be performed in a variety of ways a

> he encoding rules below. Note This string representation is lossy. Type information is lost as all values are converted to strings, and precision loss may occur for numeric values (particularly for floating point numbers and large integers that exceed the precision capabilities of the receiving system's string-to-number conversion). Strings Strings SHOULD be represented as-is without any additional encoding

## otel-proto-trace.txt

Source: https://github.com/open-telemetry/opentelemetry-proto/blob/main/opentelemetry/proto/trace/v1/trace.proto

> pans from a Resource. message ResourceSpans { reserved 1000; // The resource for the spans in this message. // If this field is not set then no resource info is known. opentelemetry.proto.resource.v1.Resource resource = 1; // A list of ScopeSpans that originate from a resource. repeated ScopeSpans scope_spans = 2; // The Schema URL, if known. This is the identifier of the Schema that the resource data // is

> InstrumentationScope. message ScopeSpans { // The instrumentation scope information for the spans in this message. // Semantically when InstrumentationScope isn't set, it is equivalent with // an empty instrumentation scope name (unknown). opentelemetry.proto.common.v1.InstrumentationScope scope = 1; // A list of Spans that originate from an instrumentation scope. repeated Span spans = 2; // The Schema URL,

> // // This field is required. bytes trace_id = 1; // A unique identifier for a span within a trace, assigned when the span // is created. The ID is an 8-byte array. An ID with all zeroes OR of length // other than 8 bytes is considered invalid (empty string in OTLP/JSON // is zero-length and thus is also invalid). // // This field is required. bytes span_id = 2; // trace_state conveys information about requ

> ed keys can be unpredictable. repeated opentelemetry.proto.common.v1.KeyValue attributes = 9; // The number of attributes that were discarded. Attributes // can be discarded because their keys are too long or because there are too many // attributes. If this value is 0, then no attributes were dropped. uint32 dropped_attributes_count = 10; // Event is a time-stamped annotation of the span, consisting of use

## otel-otlp.txt

Source: https://opentelemetry.io/docs/specs/otlp/

> sage field MUST be non-empty. The client MUST NOT retry the request when it receives a partial success response where the partial_success is populated. Failures When the server returns an error, it falls into 2 broad categories: retryable and non-retryable: Retryable errors indicate that telemetry data processing failed, and the client SHOULD record the error and may retry exporting the same data. For examp

> error. After such a failure, whether any telemetry data from the request was accepted is unspecified. Full Success The success response indicates telemetry data is successfully accepted by the server. If the server receives an empty request (a request that does not carry any telemetry data) the server SHOULD respond with success. On success, the server response MUST be a Export&lt;signal>ServiceResponse me

## genai-new-readme.txt

Source: https://github.com/open-telemetry/semantic-conventions-genai

~~~~text
> he core semantic conventions. ## Schema URL TODO ## Read the docs The human-readable version of the semantic conventions resides in the [docs](docs/) folder. Major parts of these Markdown documents are generated from the YAML definitions located in the [model](model/) folder. Reference implementations and their tooling live under [reference](reference/). For the Python reference compliance matrix and per-si

~~~~

## current-gen-ai-spans.md

Source: https://github.com/open-telemetry/semantic-conventions-genai/blob/main/docs/gen-ai/gen-ai-spans.md

~~~~text
> red` If available. | string | The unique identifier for a conversation (session, thread), used to store and correlate messages within this conversation. [3] | `conv_5j66UpCpwteGg4YSxUnt7lPY` | | [`gen_ai.tool.call.id`](/docs/registry/attributes/gen-ai.md) | ![Development](https://img.shields.io/badge/-development-blue) | `Conditionally Required` [4] | string | The tool call identifier supplied by the model

> conversation id. For example, a new UUID, a trace identifier, or a hash of request content SHOULD NOT be used as a fallback value. Application developers that manage conversation history MAY add conversation id to GenAI and other spans or logs using custom span or log record processors or hooks provided by instrumentation libraries. **[4] `gen_ai.tool.call.id`:** If and only if the model or provider supplie

> s ### Full (buffered) content Model instructions, user messages, and model outputs are considered sensitive and are often large in size. Recording large or sensitive content in telemetry may be problematic due to high storage costs, regulatory requirements, or the need to enforce different access models for operational and user data. OpenTelemetry instrumentations SHOULD NOT capture them by default, but SHO

> es) section for more details. 3. Store content externally and record references on the spans. This pattern is recommended in production environments where telemetry volume is a concern or sensitive data needs to be handled securely. Using external storage enables separate access controls. See [Uploading content to external storage](#uploading-content-to-external-storage) section for more details. #### Recor

~~~~

## current-gen-ai-agent-spans.md

Source: https://github.com/open-telemetry/semantic-conventions-genai/blob/main/docs/gen-ai/gen-ai-agent-spans.md

~~~~text
> erates fiction stories` | | [`gen_ai.agent.id`](/docs/registry/attributes/gen-ai.md) | ![Development](https://img.shields.io/badge/-development-blue) | `Conditionally Required` If applicable. | string | The stable unique identifier of the GenAI agent created during this operation. [4] | `asst_5j66UpCpwteGg4YSxUnt7lPY`; `arn:aws:bedrock:us-east-1:123:agent/42`; `urn:agent:projects-123:projects:123:locations:

~~~~

## prometheus-index.txt

Source: https://github.com/prometheus/prometheus/blob/main/tsdb/docs/format/index.md

> `len` bytes. ### Symbol Table The symbol table holds a sorted list of deduplicated strings that occur in label pairs of the stored series. They can be referenced from subsequent sections and significantly reduce the total index size. The section contains a sequence of the string entries, each prefixed with the string's length in raw bytes. All strings are utf-8 encoded. Strings are referenced by sequential

> ction is aligned to 16 bytes. The ID for a series is the `offset/16`. This serves as the series' ID in all subsequent references. Thereby, a sorted list of series IDs implies a lexicographically sorted list of series label sets. ``` ┌───────────────────────────────────────┐ │ ┌───────────────────────────────────┐ │ │ │ series_1 │ │ │ ├───────────────────────────────────┤ │ │ │ . . . │ │ │ ├─────────────────

## lucene-docs.txt

Source: https://lucene.apache.org/core/10_3_1/core/org/apache/lucene/codecs/lucene103/Lucene103PostingsFormat.html

> egers inside block) is fixed (currently 128). Additionally blocks that are all the same value are encoded in an optimized way. In VInt blocks, integers are encoded as VInt : the block size is variable. Block structure : When the postings are long enough, Lucene103PostingsFormat will try to encode most integer data as a packed block. Take a term with 259 documents as an example, the first 256 document ids ar

> offset length&gt;. Skipdata : Skipdata is interleaved with blocks on 2 levels. Level 0 skip data is interleaved between every packed block. Level 1 skip data is interleaved between every 32 packed blocks. Positions, Payloads, and Offsets : A position is an integer indicating where the term occurs within one document. A payload is a blob of metadata associated with current position. An offset is a pair of in

## quickwit-docs.txt

Source: https://quickwit.io/docs/overview/architecture

> ich indicates if the split is ready for search the min/max time range computed on the timestamp field if present. This timestamp metadata can be handy at query time. If the user specifies a time range filter to their query, Quickwit will use it to prune irrelevant splits . Index metadata needs to be accessible by every instance of the cluster. This is made possible thanks to the metastore . Index storage ​

> uery, Quickwit will use it to prune irrelevant splits . Index metadata needs to be accessible by every instance of the cluster. This is made possible thanks to the metastore . Index storage ​ Quickwit stores the indexes data (splits files) on cloud storage (AWS S3, Google Cloud Storage, Azure Blob Storage or other S3 compatible storage) and also on local disk for single-server deployment. Metastore ​ Quickw

> maintenance tasks on indexes: garbage collection, delete query tasks, and retention policy tasks. Data sources ​ Quickwit supports multiple sources to ingest data from. A file is ideal for a one-time ingestion like an initial load, the ingest API or a message queue are ideal to continuously feed data into the system. Quickwit indexers connect directly to external message queues like Kafka, Pulsar or Kinesis
