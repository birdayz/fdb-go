# Large-context compression test

Regenerate with `uv run --script tools/largecontext/fixture.py`. The script pins
`tiktoken==0.12.0` and reads distinct tracked production Record Layer Go files
and top-level technical documentation from commit
`4af32c3bc68698fbe26438a2f84f82ba5ec8d23f`. It orders paths by SHA-256, interleaves
four code files with one document, excludes generated Go and review/audit/RFC
notes, and truncates at exactly 500,000 `cl100k_base` tokens. Each source occurs
once; the final file may be partial. No repeated padding is added.

The user message's context text is exactly **500,000 tokens / 1,984,939 UTF-8
bytes**. The complete compact messages JSON is **2,091,980 bytes / 566,941 tokens**
(JSON escaping changes tokenization). The manifest records tokenizer/rank-table
fingerprints, source paths and hashes, included byte ranges, and context/JSON
hashes. This is a code-and-documents RAG-like fixture, not sampled user traffic
and not a claim about every 500k-token prompt. The gzip file only reduces the
checked-in fixture's size: tests decompress and verify it before serializing.

Run the repository's whole `just test` lane; do not select individual targets.
`TestLargeContextCompression` measures plain, zlib1, zlib6, Brotli1 and Brotli4
on a protobuf bytes-field union containing the raw JSON. It also tests labelled
repetitive-positive and random-byte-negative controls of the same byte length.
After one unmeasured warm-up per codec it runs five rotated rounds, checks exact
bytes/SHA-256, and logs stored size, wall time, process CPU time (Linux; -1 means
unavailable elsewhere), allocated bytes and allocation counts. No 80% reduction
is asserted for the real-source fixture. Process-wide counters include runtime
background work; the test is serial within its process, but other Bazel targets
may compete for CPU. These are bounded local diagnostics, not isolated-host
throughput or latency SLO measurements.

The actual-FDB specs use normal `StoreBuilder`, transformed serialization,
`SaveRecordBatch`, split storage and `LoadRecord`. A full-record dry run is
rolled back. Persistent records contain at most 256 KiB of raw content each,
with the existing 300,000-byte conservative estimate, 500,000-byte approximate
transaction size, one-second save/guard limit, 1500 ms transaction timeout and
retry limit 10. Exact reconstruction, physical 100,000-byte value limits, split
accounting, two small native indexes, corrupt-old-image rejection, overwrite
and deletion are verified. Body bytes are unindexed. This generic content test
is **not** the OTel eight-attribute-posting/two-index ingestion benchmark.
Full-record compression and independently compressed persistent chunks are
reported separately. Transaction approximate size is not persisted byte size.
Brotli remains explicitly Go-only; zlib retains the Java-compatible format.
