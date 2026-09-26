# WS-D revised design gate

TREE `236888f5b174f2e5d8894be7845992a8650bd8d9`; design SHA256 `91f08f59ce2325b10c8ec73b3f48180af7d8df23fda6cd018112611b1defd4c8`.
Read full revised design against prior four NAKs; prompts define exact scope.
No production source delta since accepted WS-C; no implementation or CI claim.

## Attempt 1 result: INCOMPLETE (no verdicts)

All four actual gpt-6-astra/xhigh/read-only sessions launched 2026-09-22 aborted
mid-review with the Codex account error "You've hit your usage limit ... try again
at Sep 27th, 2026 5:03 PM". No `.txt` verdict was produced. The partial logs are
kept as `*.incomplete-usage-limit.log`. An incomplete review is not an ACK or a
NAK; the v1 NAKs remain authoritative. Relaunch the unchanged prompts against the
same tree/design hash once the limit resets (or credits are added). No substitute
model or reviewer is permitted.

## Attempt 2: owner-authorized reviewer substitution

On 2026-09-22 the owner ruled "use claude -p instead" for the exhausted Codex
account. Gates now run as `claude -p --model opus --effort xhigh` (resolved model
`claude-opus-5-5`; the `fable` alias had no usage credits) with
`claude-reviewer-settings.json`: `dontAsk` mode, Read/Grep/Glob plus read-only
git/grep/sed/pdftotext Bash, Edit/Write/Web denied, no MCP servers. A probe
confirmed the policy denies `touch` and exposes no Write tool. Prompts are the
attempt-1 prompts with only the reviewer-identity sentence changed; originals
are kept as `*.codex-attempt1.prompt`. Full stream-json traces are `*.log`;
verdicts `*.txt` are extracted only from a successful final result event.

## Attempt 2 result: INCOMPLETE (Claude session limit)

All four parallel `claude -p` Opus/xhigh runs stopped after roughly four minutes
(26–46 turns each) with "You've hit your session limit · resets 12:10am
(Europe/Berlin)". No verdicts. Traces kept as `*.claude-attempt2-session-limit.log`.
A small Opus call succeeded immediately afterwards, so attempt 3 runs the gates
sequentially with identical prompts and policy.

## Attempt 3 result: Graefe NAK; the other three INCOMPLETE

Sequential runs: `graefe.txt` completed (NAK, P1 zero-final-primary clusters after
final ownership; P2 queued replay divergence, lock scope, per-operation capability
table; P3 RNG split, tie rule, hash pinning, reconcile wording). Torvalds stopped
at turn 69 and storage/spfresh at turn 1 on the Claude session limit (resets 00:30
Europe/Berlin); traces kept as `*.claude-attempt3-session-limit.log`. Graefe's P1
requires a v3 design, so all four lenses re-review v3 in `../ws-d-design-review-v3/`.
