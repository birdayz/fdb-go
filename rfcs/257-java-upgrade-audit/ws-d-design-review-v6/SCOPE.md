# WS-D design gate v6

TREE `0a7977fc48e0e10f8ee52f4c04deabf4d94ac73c`; design SHA256 `1f0fd83139e31f2efb4943711398f7ca9046fd695bd626424e6d247f30fff1f2`. Answers the four v5 NAKs with new oracle
measurements (inline mode, split fallbacks). Delta outside rfcs/TODO since the
accepted WS-C tree: the test-only oracle and the owner-requested removal of the
nightly RowDiff watcher; no WS-D production code. Reviewers: `claude -p --model
opus --effort xhigh`, read-only policy in `claude-reviewer-settings.json`
(owner-authorized substitution), sequential with session-limit retry.
