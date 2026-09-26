# WS-D design gate v8

TREE `cc94face9f554e277346f6061becf84c658abc3b`; design SHA256 `e8a870a4739015a2c9d3e5e83831c08424ac64e1a64e2443e014ae6c6f889814`. Answers the four v7 NAKs (inline insert cap
withdrawn; residue bounded inside the terminal reconcile; consumerOutcome-based
delete skip; execution errors poison; logarithmic peel bound with n=1000 data).
Also in the tree: RFC-257 oracles moved to //conformance:rfc257_oracle_test,
Java heartbeat admin APIs ported, WS-E design v1 and oracle. `just test` 94/94 on
this tree's inputs (conformance_test re-run, 1405 specs). Reviewers: `claude -p
--model opus --effort xhigh`, read-only policy, sequential with session-limit retry.
