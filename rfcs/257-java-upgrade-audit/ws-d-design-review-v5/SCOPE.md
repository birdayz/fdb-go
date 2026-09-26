# WS-D design gate v5

TREE `d0ce9ed51b2ffce11e96ae5db2a98cda56882e07`; design SHA256 `c3b80bc41e3d34e134410a089e420414bc793f5e7b8e7632e60038d2309379cb`. Answers the four v4 NAKs using the measured
live-JVM oracle (`../ws-d-oracle/`). Only test-only conformance files changed since
the accepted WS-C tree; no production source delta under pkg/gen/proto. Reviewers:
`claude -p --model opus --effort xhigh`, widened read-only git policy in
`claude-reviewer-settings.json` (owner-authorized substitution), sequential with
session-limit retry.

## Result: four NAKs (all runs complete; torvalds retried once after a session limit)

Shared findings: the hard cap in inline mode stalls indexes the target keeps
healthy (unmeasured inline mode); index-wide merge refusal contradicts the knob
golden, which shows the target failing only at the consuming task kind;
deleteConcurrency must be checked where Java consumes it; poisoning must be
limited to mutation entry points and continuation replay never refused; the
foreign-lease wait must not let the session heartbeat lapse; the progress
baseline must be the stored count and the hand-off drain bounded; the reconcile
lowers the primary count; SPFresh asks for a balanced fallback split. Addressed by
design v6 with new oracle measurements (inline mode; lambda has no effect; the
outlier-excluded refit is usable). Storage also flagged the red docscheck target:
the flaky rowdiff watcher test, since removed at the owner's request.
