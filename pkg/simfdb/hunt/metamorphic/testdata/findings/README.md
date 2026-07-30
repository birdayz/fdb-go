# Metamorphic findings — reproducers (NOT auto-run)

Two real query-engine bugs the LLM-adversarial metamorphic loop found on its first run (RFC-199 Tier 2).
These JSON scenarios are kept as durable reproducers — run them through the judge:

    go build -o /tmp/dst-generate ./cmd/dst-generate
    /tmp/dst-generate -dir pkg/simfdb/hunt/metamorphic/testdata/findings/

They are NOT wired into any auto-run test (they are RED until the engine bugs are fixed — no skips, no
red CI). See TODO.md `## DST findings` for the writeup. Query-engine bugs → Graefe/owner to fix.
