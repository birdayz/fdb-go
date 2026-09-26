# WS-D source research — historical reports, not ACKs

The three .txt reports are the actual read-only research outputs; prompts/logs
are retained alongside. They do not establish runtime parity or implementation.

All four detailed-design v1 gates returned NAK. Their corrections govern over
research shorthand; see ../ws-d-design.md and ../ws-d-design-review-v1/.
In particular, signature input is the supplied working representation, not
universally persisted bytes: plain pretraining references transform on load;
encoded references pass through. GuardiANN's Config does not share HNSW's enabled
RaBitQ1–15 constructor check. Complete-drain-before-merge can strand queued
capacity work; the revised design specifies checkpointed merge/replay recovery.
Java GuardiANN executes in merger-supplied transactions, not a nested runner.
The design also defines cache isolation/locking, operational capability admission,
continuation identity, conditional upstream-defect handling and Java HashMap
ordering at the KMeans input boundary. Reports remain unedited historical outputs
so the original overclaims remain auditable, not silently rewritten as discoveries.
