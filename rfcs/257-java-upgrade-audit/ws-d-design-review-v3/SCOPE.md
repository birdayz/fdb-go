# WS-D design gate v3

TREE `1390a2feec185fc510f4b17c679bd1ae307951af`; design SHA256 `2686aaeba1ad9eac4a4959ab771abaec6b7dea48f3a86ffc2d0651bee205b349`. Answers the v2 Graefe NAK; v1 NAKs and all
v2 artifacts stay in their own directories. No production source delta since the
accepted WS-C tree. Reviewers run as `claude -p --model opus --effort xhigh` with
`claude-reviewer-settings.json` (owner-authorized substitution). Runs are
sequential; a run that stops on the Claude session limit is kept as
`*.session-limit-<n>.log`, carries no verdict, and is retried after a wait.

## Result: four NAKs (all runs complete)

graefe, torvalds, storage and spfresh each completed (read-only, claude-opus-5-5)
with NAK. Shared findings: split no-usable-candidate orElseThrow stall; cursor-
lifetime lock hold cannot honour cancellation on the sync.RWMutex registry and adds
Go self-waits; build-range capacity step 1 missing (throttle retries only
lessen-work codes); per-batch interleaving over-broad; dropped-cluster replica/
occlusion and cause-set repair; RaBitQ 9-15 admission not decidable pre-buffer;
sliding-window-wrapped delegates; reconcile replica handling; prefix-scoped
recovery predicate; catch-and-commit residue; collector join rule; hash formula
and version pins. Addressed by design v4 (`../ws-d-design-review-v4/`).
