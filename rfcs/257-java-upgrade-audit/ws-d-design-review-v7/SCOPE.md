# WS-D design gate v7

TREE `65d33289a2d6a410884ef98945557d3ff6a6ff21`; design SHA256 `21279344492055e04a6c81caef2c6ed3c7c0692df635050b98999420bf253a48`. Answers the four v6 NAKs: consumer-point
capability errors, withdrawn config-only insert refusals, iterated split peel with
a measured bound, inline residue cap, lease-wait sizing and claim-race wait; new
oracle measurements (13 specs). Delta outside rfcs/TODO since v6: test-only
oracle extensions, the Go default heartbeat lease corrected to Java's 10 s
(online_indexer.go, pinned test), in-place watcher-claim corrections. Reviewers:
`claude -p --model opus --effort xhigh`, read-only policy in
`claude-reviewer-settings.json` (owner-authorized substitution), sequential with
session-limit retry.
