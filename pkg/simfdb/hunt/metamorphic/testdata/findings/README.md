# Metamorphic findings — regression sentinels

Scenarios the LLM-adversarial metamorphic loop produced when it caught real query-engine bugs
(RFC-199 Tier 2). Each one is a shape that was once wrong.

**They are RUN, by `TestFindingsAreRegressionSentinels` in the parent package**, and each must
report ZERO violations. The bugs are fixed, so the equivalences now hold; running them is what
makes each file a sentinel rather than a text file. A violation here is a regression on a shape
that has already been broken once — the highest-value signal this suite produces. Do not silence
it and do not delete the file.

To run a scenario by hand, or to re-run a freshly generated batch before checking it in:

    go build -o /tmp/dst-generate ./cmd/dst-generate
    /tmp/dst-generate -dir pkg/simfdb/hunt/metamorphic/testdata/findings/

Adding a finding: check the JSON in here and it is picked up automatically. If a NEW finding is
still failing — a live engine bug — do not commit it here; that ships a red test. Fix the bug,
then commit the scenario as the sentinel for it.
