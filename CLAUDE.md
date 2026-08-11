# FoundationDB Record Layer — Go Port

## ABSOLUTE PRIME DIRECTIVE: NO SKIPS

**NEVER use t.Skip() to defer a failing test.** If a test fails, FIX IT. Immediately. No matter how long it takes, no matter how deep the rabbit hole goes. Skipping is forbidden. The only acceptable t.Skip is the runtime Docker check (`FDB not available (no Docker)`). Every other skip is a bug you're hiding. Hunt it down. Fix it. Principles first.

## THERE ARE NO UNRELATED FLAKES — EVERY FLAKE GETS INVESTIGATED NOW

**A flaky or intermittently-failing test is a REAL BUG, never noise to wave away.** "Unrelated flake," "just rerun CI," "not caused by my change," "known-flaky" are all FORBIDDEN rationalizations. A red CI is red — full stop. The moment any test fails or flakes (in CI, in `just test`, in a stress/fuzz run, anywhere), STOP and root-cause it immediately, even if it lives in a package your change never touched. Flakes are how real concurrency bugs, conflict-handling gaps, ordering assumptions, and resource leaks hide in plain sight — a transaction-conflict (`not_committed 1020`), a timeout, a watch that doesn't fire, a nondeterministic plan: each is a latent defect, not bad luck. Reproduce it (run it in a loop / under `--runs_per_test` / with `-race`), find the actual race or assumption, fix the code (or the test if the test's timing assumption is genuinely wrong — but prove that first), and pin it with a deterministic regression. Never merge or hand off with CI red or a flake outstanding. "It passed the second time" is not a fix; it's the bug still there.

**A red nightly (fuzz / differential / conformance) is ALWAYS in scope — no freeze or owner directive exempts it.** A freeze gates NEW work, never triage of a red safety net; "the freeze says don't touch that package" is one more forbidden rationalization. Root-cause it immediately. If the fix is small, fix it in place; if it's large, surface it to the owner the SAME DAY with the root cause in hand. A safety net that stays red is not a safety net.

## NO EXCUSES — DO THE WORK

**Nothing is "gated," "blocked," or "multi-shift."** If a TODO exists, DO IT. Now. No matter how long it takes. Don't rationalize deferral with "needs Java conformance server," "blocked on upstream," or "multi-shift effort." Those are excuses. Read the Java source, understand the algorithm, port it, test it, ship it. If it takes 826 lines, write 826 lines. If it takes all night, work all night. The TODO list is a list of things TO DO, not a list of things to punt.

**100% Java alignment or it's a bug.** Every component must match Java's behavior exactly. Go-only extensions are fine, but they need DEEP test coverage. If Java has it and Go doesn't, that's not "low priority" — it's missing functionality. Port it.

**Wire compat is the hard line; query reach is not.** Two distinct axes, don't conflate them:
1. **Anything touching the wire** — key encoding, record/index/version format, continuations, what gets written to FDB — MUST match Java exactly. Divergence there is a bug, full stop. This is the whole point: Go and Java apps share a cluster and read/write each other's records.
2. **The read-side query surface MAY go beyond Java.** Net-new query capabilities Java lacks *entirely* (new join flavours, operators, syntax) are welcome — provided (a) wire compat is never sacrificed (Java still reads/writes the exact same records; the extension only lets Go *express* more), and (b) the extension has deep test coverage. "Doing better than Java" on the read path is encouraged, not suspect.

Before treating a TODO "vs Java gap" as parity work, **verify Java actually supports it.** A TODO line can be stale (the feature may already work — e.g. via a normalization the item's author missed) or mis-framed (Java may not support it *at all*, in which case you're adding an allowed extension, not closing a divergence — and the conformance principle below does not forbid it).

## DFS, NOT BFS — GO ALL IN ON EVERY PROBLEM

**When you discover a problem, go ALL IN.** Dig into the rabbit hole. Fix it completely. No matter how long it takes, no matter how deep it goes. Don't "skip this and look for quick wins" — that's BFS thinking and it produces shallow, fragile work. DFS: pick the problem, understand it fully by reading Java first, then fix it properly in Go. One problem at a time, fixed to completion.

**Java is the reference. Always.** Before writing ANY fix, read the corresponding Java code. Understand how Java handles the exact same case. Then port that approach to Go. No invented shortcuts, no "pragmatic alternatives," no "we'll do this differently." If Java uses SemanticAnalyzer.resolveIdentifier, Go uses the semantic scope. If Java walks the ANTLR tree with typed visitors, Go walks the ANTLR tree with typed visitors. 1:1.

**Never paper over a problem.** If a test fails, the fix is in the code, not in the test expectations. If an error code is wrong, trace it to the root cause — don't add a string check at the surface. If a column doesn't resolve, fix the resolution infrastructure — don't strip qualifiers with string hacks.

**SCOPE EVERY COUNT, or paste the command that produced it.** "Exactly one hit", "the only definition in the tree", "five call sites", "nowhere else" — this shape keeps being wrong even when the argument resting on it is right, because a grep run over one directory gets written up as a fact about the repo. Say the claim that is actually true ("three `instanceof` in non-test main sources") or show the command and its output. An unscoped enumeration is a load-bearing number nobody can check, and the conclusion surviving does not make the count true.

**A NEW TEST FILE PROVES NOTHING UNTIL YOU SEE IT RUN UNDER BAZEL.** A file that is not in its target's `srcs` is not skipped and not reported — it produces no signal at all, while `go test` runs it happily and the surrounding suite stays green. That is worse than a filter matching nothing, because a filter at least runs the target. After adding a test file: `just gazelle`, then run the Bazel target and grep the output for a line only that test emits. `go test` green and Bazel green are different claims, and the second is the one CI makes.

The same trap has a second door: a test that resolves paths. Under Bazel a test runs in a runfiles tree containing only its declared inputs, so a helper that returns the runfiles root sees a different world than one that returns the source tree — green under `go test`, and under Bazel the files simply are not there. Both doors are found the same way: run it sandboxed and read the output, rather than inferring from a local pass.

**"For now" is a red flag.** If you're about to write "for now" or "pragmatic approach" or "we'll fix this later" — STOP. That means you're about to create technical debt. Either do it properly or document it as the FIRST priority in TODO.md so it gets done next. No deferred hacks.

**DFS IS ABSOLUTELY CRITICAL. NEVER DEFER — FIX IT NOW.** This is the single most important working rule in this file. When a fix uncovers a second problem, fix that one too, in the same change. When a sweep surfaces something adjacent, chase it to ground. Take as long as it takes. The instinct to write it up and move on is always wrong, and it is strongest exactly when the finding is real — because a real finding looks like scope, and scope looks like something to schedule.

Filing it feels responsible. It is not. A TODO entry ships nothing, deletes the regression sentinel that would have caught the bug, and hands the work to someone with none of the context you have right now — you have the reproducer, the measurement harness, and the Java source already open. That will never be true again as cheaply.

Worse, deferred findings **rot into invisibility**. Write a live defect into the prose of an item you then mark `- [x]` and it is unreachable work: the execution rule is "pick the lowest-numbered UNCHECKED item", so nothing will ever pick it up. That has happened here — a redundant in-memory sort on `WHERE pk IN (...) ORDER BY pk`, fully diagnosed with a reproducer, was written into a completed item and would have sat there indefinitely. DFS'ing it instead removed the sort.

**NEVER PROPOSE — RESEARCH, DECIDE, IMPLEMENT.** Writing up options and asking which one to take is deferral wearing a lab coat. An RFC that says "here are three paths, please rule" has shipped nothing and has moved the decision to someone with less context than you had while writing it. You did the research; you are the one who knows which path is right. Pick it, say why the others lose, and build it. An RFC is for *recording the design you are implementing* — the reasoning, the measurements, the rejected alternatives — not for outsourcing the choice.

"Long-term correct" is the only selection criterion. Not smallest diff, not least blast radius, not what the existing structure makes convenient. If the right answer requires changing a cost formula, deleting a Go-only extension, or reworking a mechanism three commits old, that is the answer. A smaller change that leaves the architecture incoherent is a bigger cost paid later, by someone with none of the context.

**NEVER MOVE LATERALLY WHILE A DFS PATH IS OPEN.** If you are inside a problem and it is not finished, nothing else may be started — not an adjacent cleanup, not a different finding, not a "quick win" that would feel like progress. Lateral motion while a path is open is the same failure as deferral: the depth is abandoned, the context evaporates, and the problem comes back later as someone else's. Finish, or prove the path is genuinely blocked on a capability that does not exist.

**THERE ARE NO LEGITIMATE DEFERRALS. NONE.** Not "separate concern", not "deserves its own item", not "changes plans so it needs its own review" — those are reasons to do the extra work now: run the stress comparison, review the goldens, get the review lap.

**"It needs a capability that doesn't exist yet" is not a deferral — it is the work.** This is the escape hatch that feels most legitimate and is the most damaging, because it is indistinguishable from real engineering right up until nothing gets built. A missing capability is a thing to BUILD, not a wall to stop at. Before you conclude a capability is missing, check Java: this is a port, and the odds are overwhelming that Java already has the machinery you just decided Go cannot have. A cost model that cannot tell a unique probe from a non-unique one is not blocked on statistics nobody has — Java has `CardinalitiesProperty`, so the task is to port it. "Go's substitute for X" in a comment is a standing admission that X is the real answer and someone stopped early.

**"It's an upstream bug" is not a deferral either.** Fix it at the boundary, work around it deliberately with the divergence documented at the call site, and report it upstream. Shipping a known-broken path because the break is someone else's is still shipping a known-broken path.

If you genuinely cannot proceed — the work needs a decision only the owner can make, or an external system nobody here controls — that is not a deferral, it is a **STOP**. Say so directly, in the conversation, with what you found and what you need. Do not convert it into a TODO entry and keep moving; a filed item is how a blocker becomes invisible. Escalating costs one sentence. Filing costs the next person everything you knew.

**A FINDING TRACKED WHERE THE NEXT READER CANNOT SEE IT IS NOT TRACKED.** Conversation state, a coordinating agent's private task list, a scratchpad note — all evaporate, and none of them are visible to the person or agent who inherits the work. This is not the same failure as filing instead of fixing; it is worse, because it *looks* like tracking right up until someone tries to follow the reference. It has happened here: three live findings were carried for a full shift in a session-local tracker, and the gap surfaced only because an agent was told to update "task #86", searched `TODO.md`, found nothing, and refused to proceed on the assumption that it existed somewhere. Durable homes are `TODO.md`, an RFC, or a comment at the fix site — and when a finding spans two of those, each must point at the other, or the halves rot separately. When you hand someone a finding, hand them a location that outlives you.

**And when a claim is superseded, GREP FOR IT — do not fix only the copy you remember writing.** A refuted hypothesis rots wherever it was ever recorded, and the copy you forget is the one the next reader finds. This was demonstrated inside the very edit meant to fix its first instance: an investigation corrected a superseded "the surviving hypothesis…" framing in `TODO.md`, and the identical framing had also settled into a workflow comment, where it would have outlived the fix entirely. The correction is not done until a grep for the superseded phrasing returns zero **across every file**, and that count is what you report — not "I updated the entry".

**A MEASUREMENT RECORDED AS PROSE MUST STATE THE POPULATION IT WAS TAKEN OVER, or it cannot be seen to go stale.** "Mutating X reddens ONLY that arm and leaves the other TWELVE green" was true when measured — over a file with 13 arms. A 14th arm was then added and the measurement never re-run. The lethal part is that **"twelve green" stayed accidentally correct across the change**, so the only half that could have flagged the staleness was the half that happened to still hold, while "only that arm" had silently become false. The claim then contradicted the `TODO.md` entry and commit body that described the same shape correctly — and the wrong copy was the one sitting at the site a later engineer would read while deciding where to add a guard.

Write the scope into the claim ("at 14 arms"), the way an expiry condition is written into a guard. A number with no stated population is a fact about a tree nobody can identify, and it decays without ever looking wrong.

**Book into a shared section as ONE self-contained block appended at its END — never merged into an existing entry.** The durable home is right; the mechanism has to survive concurrency. Several agents editing one region of `TODO.md` produced a conflict in every in-flight branch the moment the first one merged: three PRs, two of them at a clean full-green rollup, all went `DIRTY` at once. Appending at EOF removes the collision at the source instead of asking each author to remember.

And when that conflict does arrive, **read both hunks — a `TODO.md` collision looks content-free and is therefore resolved by reflex.** It is the same trap as the general merge rule above, wearing its most disarming face: two appends to one list genuinely usually are independent, so "keep both" is usually right, which is exactly why the once it isn't goes through unexamined. The check is what establishes independence, not the appearance. Measure it rather than asserting it — grep each side for the other's subsystem terms and show the zero.

## NO FAKE CHECKBOXES — E2E OR IT'S NOT DONE

**A TODO item is done when a SQL query exercises it end-to-end and a test pins the behavior.** "Plan type exists" is not done. "Rule ported but can't fire" is not done. "Infrastructure exists but SQL can't trigger it" is not done. If a user can't write a SQL query that hits the code path and gets the right answer, the checkbox stays unchecked.

The proof is a **yamsql scenario** (for SQL-visible features) or an **FDB integration test** (for record-layer internals). The test must demonstrate the OPTIMIZATION actually fires — not just that the query returns correct results via a slower fallback. For planner optimizations, use `EXPLAIN` assertions to verify the expected plan shape:

```yaml
# GOOD: proves aggregate index scan fires
- query: SELECT status, COUNT(*) FROM orders GROUP BY status
  plan_contains: AggregateIndexScan
  rows:
    - [delivered, 2500]
    - [pending, 2500]

# BAD: just proves GROUP BY returns correct results (could be full scan)
- query: SELECT status, COUNT(*) FROM orders GROUP BY status
  rows:
    - [delivered, 2500]
    - [pending, 2500]
```

If you can't write the e2e test, the feature isn't done. Period.

## NO TEXT MATCHING ON SQL / PARSE TREES

**NEVER detect SQL features by string-matching on SQL text or GetText() output.** The ANTLR parse tree has typed nodes — use them. `strings.Contains(sql, "CROSS JOIN")` is forbidden. `GetText()` concatenates tokens without whitespace and produces garbage like `labelISDISTINCTFROMnull`. Magic length limits (`lparen > 12`) are fragile trash that breaks on `CHARACTER_LENGTH`. Walk the parse tree or Value tree. If you need to detect a function call, find `FunctionCallExpressionAtomContext` / `ScalarFunctionValue` in the tree — don't regex the text.

## QUERY-ENGINE CHANGES REQUIRE A GRAEFE ACK — RFC AND IMPL

**Any change to the Cascades query engine (planner, optimizer, cost model, matching/data-access infra, physical wrappers, executor) needs a Graefe ACK on BOTH the RFC and the implementation before merge. Never merge a query-engine change Graefe hasn't reviewed.** Torvalds + @claude + codex are the other gates — never merge with a NAK from any.

**Review cadence is MILESTONE-LEVEL, not per-commit (owner ruling 2026-07-18):** RFC → review → implement → review. Graefe+Torvalds ACK the RFC before implementation starts; implementation gets ONE joint review lap at workstream/phase completion (the review unit for an umbrella RFC is the workstream/phase, e.g. one lap for all of WS-P, one per WS-N phase), plus codex at the same granularity. Intermediate commits need green tests only — do NOT launch reviewer laps per commit. An ACK covers only the HEAD it reviewed, so review findings get folded and the FINAL head gets one DELTA re-confirmation — not a fresh full lap per fix commit. Codex runs (owner ruling 2026-07-18): for a large span, ONE run with a generous --timeout (2h), never split into scoped fragments; codex banks nothing on timeout, so pick a budget it can actually finish in. Holds always-on, not just when a skill is loaded; mechanics in `.claude/skills/query-engine/` (impl) and `.claude/skills/todo-worker/` (RFC). PR #201 shipped a latent 0-row planner bug because it skipped Graefe entirely — the gate is mandatory; its FREQUENCY is milestone-level.

---

Port `fdb-record-layer-core` from Java to Go with full wire compatibility — Go and Java apps must read/write each other's records and share the same FDB cluster. SQL/relational layer features (UDFs, views, synthetic record types, fdb-relational-*) are out of scope unless a TODO entry calls for them; protobuf round-trips them via unknown-field preservation.

## Keep this file general

This file is for **general instructions only** — project goals, testing rules, design principles, working rhythm. Specific bug findings, gotcha lists, resolved/retracted history, per-shift state belong in:
- TODO.md (open issues)
- `shifts/*.md` handovers (history)
- Code comments at the relevant fix site

If you're tempted to add a 5-line note explaining a divergence, write it as a code comment at the call site instead.

**Never put shift tags in code comments.** No `nightshift-65`, no `swingshift-64`, no `landed in shift X`. Shift refs rot the moment the codebase outlives the shift naming scheme and they leak ephemeral process state into permanent files. Code comments explain WHY the code is the way it is — not WHEN it got there or WHO did it. That belongs in `shifts/*.md` handovers and PR descriptions. Old shift-tag refs already in the codebase are cleanup fodder; don't add new ones.

**Never attribute code comments to a reviewer or a review artifact.** No `Graefe condition 1`, no `Torvalds R1 positive`, no `per @claude`, no `codex finding 3`, no `review round 2`. Reviewer names and review-cycle labels are ephemeral process state exactly like shift tags — they rot and they leak WHO/WHEN into permanent files. Keep the *reasoning* the reviewer surfaced (that's the WHY), drop the attribution: write "an asserted bridge, never a silent fallback", not "(Graefe condition 1: an asserted bridge…)". This is enforced — `pkg/docscheck`'s `TestSourceCommentHygiene` fails the build on reviewer/shift attribution in comments; a genuinely load-bearing exception goes on its `hygieneAllowlist` with sign-off, not inline. RFC numbers are fine (they name durable design docs, not people).

## Testing

**EVERY bug you discover gets a regression test — no exceptions.** The moment you find a problem (a failing probe, a reviewer catch, a "huh, that's wrong"), the fix is incomplete until a test pins the corrected behavior. This is not optional polish; it is the difference between fixing a bug and fixing it *for good*. **A green CI with the bug still latent is the real danger** — it reads as "covered" when it isn't. Most bugs ship green precisely because no test exercises that *dimension*: the gap is dimensional, not volumetric (you can have 100 tests for a feature and still miss the one axis that's broken). When you fix something, ask "what dimension was unprobed that let this through?" and add the test on that axis. Hard-won examples from this codebase: non-correlated `EXISTS` was wrong on master with full CI green because every `NOT EXISTS` test was *correlated*; deleting a DML helper silently dropped the secondary-UNIQUE→23505 and `RecordDoesNotExistError` mappings — caught by review and a deliberate probe, not by the suite. If a reviewer (human or Graefe/Torvalds/@claude) finds a problem the tests didn't, that's a doubly-important signal: fix the bug **and** add the test that should have caught it. Quality is the point.

**A CENSUS OR GATE NEEDS A UNIT PIN THAT DRIVES EVERY ARM — the corpus reading is not a substitute.** A full-suite run exercises only the arms the corpus happens to reach, so an arm that is rare today, or that a pending conversion will make reachable tomorrow, ships untested. Its first real firing is then read as a *finding* rather than as an untested branch, which is the most expensive way to discover a bug in an instrument. Split the decision away from the process globals so it takes explicit state — `assertXCounters(w, state, floors)`, the shape the existing censuses use — and drive every class, every floor, and every vacuity guard from a test. Guard both populations while you are there: the one whose collapse makes a finding vacuous, and the one whose collapse makes the instrument silently dead. This has been skipped on three separate instruments here and caught by review each time; the arm that went untested longest was the one the next change was about to make live.

**A GUARD HAS A SHELF LIFE, AND ITS DIRECTION INVERTS WHEN THE EXPECTED VALUE DOES.** A population watched for COLLAPSE is watched that way because a zero would read like good news while meaning the instrument died. Once a change makes zero the *steady state* — an arm retired, a channel drained — that same floor becomes unsatisfiable and the danger flips to GROWTH: a non-zero now means the thing you removed came back. Reconcile the guard with the new expected value rather than relaxing or deleting it, and say in the failure message which direction is now the alarm. A floor left pointing at the old expectation is a build break; a floor deleted outright is an unwatched revival.

**A TEST FILTER THAT MATCHES NOTHING REPORTS GREEN.** `--test.run` / `--test_filter` with a name that matches no test function runs zero tests and the target still passes — Bazel will print `Executed 1 out of 1 test: 1 test passes` for a target that executed nothing you asked for. So a green from a narrowed run is a statement about the FILTER until you have checked the filter matches: confirm the pattern against the actual function names, or read the per-test output and count what ran. Never bank a narrowed green as evidence without that check.

**A GREEN FROM AN EMPTY SET IS THE DOMINANT FALSE POSITIVE — AND IT WEARS AT LEAST TWELVE FACES.** The narrowed-filter case above is one instance of a general failure: a reporting layer that cannot distinguish *passed* from *never ran* renders both as success, so the absence of a result reads as the absence of a problem. Twelve confirmed here:

- a `--test.run` pattern matching no function (`TestFieldNameDecision` for a test actually named `TestFieldNameNeverDecides` — `PASS`, zero `=== RUN` lines);
- Bazel serving a cached result, printing `Executed 0 out of 1 test: 1 test passes` — a green that ran nothing this invocation. Re-run with `--nocache_test_results` before banking it;
- CI runs held at `action_required` awaiting approval, which `gh pr checks` reports as *"no checks reported"* — indistinguishable from never triggered. All 3 bot-authored PRs in this repo's history ran zero checks and **one of them merged that way**, because `mergeStateStatus` was `UNSTABLE`, not blocked;
- a `gh` JSON query whose `statusCheckRollup` is empty, so a filter for failing checks returns nothing and reads as "all green";
- a PR whose `mergeStateStatus` is `DIRTY`, also reported by `gh pr checks` as *"no checks reported"*. GitHub cannot compute `refs/pull/N/merge` on a conflict, so `pull_request` workflows never fire **at all** — and that is rendered identically to "queued" and to "never triggered". Actions status, workflow triggers and repo permissions all read healthy while nothing runs. Check `mergeStateStatus` before diagnosing a missing check; merging the base fires every workflow within seconds;
- a shell quirk emptying the input to the search itself: `grep --include=*.go` under fish returns 0 hits when the glob fails to expand, which "proves" the symbol does not exist anywhere. Any zero-hit sweep used as evidence of absence must first be shown to be a well-formed command — run it against a term you know is present.
- **a run that executed fully, against bytes that no longer exist.** A suite launched before your last edit lands is a real execution with a real green — describing a tree that is gone. This is the nastiest face because every check for emptiness passes: the population is non-empty, `=== RUN` lines are there, nothing was cached. The defence is to freeze the tree and record a `md5sum` you verify *after* the run, so the green is provably about the committed content. Two full-suite runs were discarded on this basis in one shift.
- **a paired-equality assertion whose two sides move together.** `assert(nested == topLevel)` looks like a strong invariant and is vacuous against any mutation that breaks both — disabling a type-derivation arm made both sides report `UNKNOWN`, the equality held, and only the explicit `want STRUCT` check caught it. The general rule, because the vacuity is not luck: **a pair whose two sides share a derivation cannot be checked by comparing them to each other.** The sibling pairs in that same file DO detect their mutations, and only because each straddles a boundary — one side answers from the stored descriptor, the other from the value — so the shared-route pair was the first that could not. Establish which route each side takes before trusting a comparison, and assert a VALUE as well as a relationship.
- **an infrastructure layer reporting busy while nothing runs.** A self-hosted runner claimed `busy=true` to GitHub for 4h21m while `gh run list --status in_progress` returned nothing, because one job had wedged against a 17400s timeout. Job-level and run-level status disagree by design — GitHub keeps a *run* at `queued` until all its jobs start — so run-level status alone reports "nothing is running" while jobs execute, and reports "busy" while nothing progresses. Read check-level status, and treat a long timeout as a defect in the safety net: at ~5h, one wedge costs a two-runner fleet a day before anyone asks why.
- **a query language quietly not falling through.** `jq`'s `//` operator only substitutes on `null` and `false`, so `.conclusion // .status` returns `""` — not the status — for a check whose conclusion is an empty string. Five real, distinct check states rendered as blank and would have read as "nothing to see". Any defaulting operator needs its falsy set checked against the data's actual shape.
- **a probe that ran, passed, and printed nothing — or printed, and had its evidence truncated on the way to you.** A cross-engine JVM probe reported `1 test passes` with **zero** `PROBE` lines, because Ginkgo buffers `GinkgoWriter` unless `--ginkgo.v` is passed. Read as "the probe found no disagreement", it would have reframed a *measured, real* divergence as conformance and rewritten a correct DONE criterion into one that made Go refuse queries Java accepts. In the same task, two further captures showed zero lines for a different reason: the reporter's own `tail`/`grep` had cut them. Three empty readings, one real and two self-inflicted, all indistinguishable from agreement. When a probe's verdict is its OUTPUT rather than its pass/fail, capture the FULL output to a file and interrogate the file afterwards — never a pipeline you cannot re-inspect — and assert on the line count you expect, because a silent probe and an agreeing probe are the same green.
- **a run still in flight reads exactly like a run that produced nothing.** A grep for a summary line mid-run returns empty, identical to "never ran" — and unlike the other faces, the correct reading changes minute to minute, so a re-read minutes later can flip the verdict without anything being wrong. Confirm the process is gone (`pgrep`) before interpreting an absent summary. The corollary is a working rule, not a nicety: **never end a turn with a run still going and a "monitor armed".** Nothing survives the turn — the OS process keeps running, no monitor reports, and the verdict sits detached from the context that could interpret it. Block on it in the foreground with an explicit timeout, or run a narrower target and say exactly what you skipped.

The defence is always the same and it is cheap: **confirm the population is non-empty before interpreting the verdict.** Count the `=== RUN` lines, count the checks, count the rows. A gate must separate three states — passed, failed, and never-ran — because collapsing the third into the first fails OPEN, which is the direction that ships bugs. When you report a green, you are implicitly claiming something ran; make that claim checkable.

**EVERY PROOF GETS COMMITTED AS A TEST — never as a throwaway probe you delete.** If you wrote a scratch probe to establish a fact, and that fact justified a decision, the probe becomes a test. No exceptions. The instinct is to delete it once it has "done its job", and that is exactly backwards: the conclusion outlives the measurement, so the measurement is what has to survive. A deleted probe is the same failure as a filed-instead-of-fixed finding — the knowledge evaporates, and the next person either re-derives it or silently breaks the assumption it rested on.

This applies with FULL force to two cases that feel exempt:

- **Negative results.** "I probed X and it does not reproduce" is a load-bearing claim — it is what lets you classify something as latent rather than shipped. Pin the fact that makes it unreachable, with a failure message naming what gets re-armed if it changes. A real example: a wrong-order sort bug was unreachable only because the SQL layer rejects a repeated `ORDER BY` column with 42701 before the dedup rule sees it. Nothing pinned that rejection; relaxing it would have silently armed the bug.
- **The exact shape that broke.** Pin the reproducer, not a simpler cousin of it. A signed-zero DISTINCT bug shipped a SINGLE-column test while the defect required a float in a NON-final dedup position — with one column the two zeros are adjacent, both plans agree, and the test passes with the bug fully present. A test that cannot express the defect is not coverage.

If a probe is too slow or too broad to keep as-is, narrow it until it is keepable. Do not delete it.

- Real FDB via testcontainers, never mocks. High and thorough coverage required for every feature/fix/behavior change — edge cases, error paths, zero-value behavior.
- All tests MUST call `t.Parallel()` and be safe to run concurrently (unique key prefixes / subspaces, no shared mutable state).
- Container setup MUST have timeouts: `context.WithTimeout(context.Background(), 2*time.Minute)` around `foundationdbtc.Run()` / `container.InitializeDatabase()`. Bare `context.Background()` blocks forever when Docker is slow.
- Never run binding stress concurrently with `just test` — both spin Docker containers; pre-commit runs `just test`.
- `.bazelrc` has `--local_test_jobs=4` to cap concurrent FDB containers. If a test suite "hangs", check whether 200 tests are cascading through 30s timeouts (kill it; don't wait).

## Shift system

Vollkonti continuous 24/7 shifts via `/vollkonti`. Handovers in `shifts/`. One branch + one PR per shift, merged at end.

**Pacing is NEVER the model's call.** Don't autonomously slow down, "find a stopping point," or rationalize coasting. Stops are EXTERNAL: user intervention, mid-shift check-in, wind-down at T+7:30. Heuristics from human-paced practice ("keep PRs focused", "let big work rest") DO NOT apply — the system is designed for continuous output. Common rationalizations to ignore: trained-SWE-instinct, marginal-value-reasoning, reviewer-empathy projection, milestone pattern-matching, "shipped ahead of demand" guilt. If a "what I would have done differently" entry boils down to "less work would have been fine," delete it before merging.

## Work tracking

`TODO.md` is the authoritative execution order — numbered items in 6 sequential phases, items inside a phase run in parallel unless gated. **At shift start, pick the lowest-numbered unchecked item whose gates are satisfied.** Handover follow-ups are suggestions, not the priority list. Finish what you start before moving on.

**Working rhythm:** one thing at a time. Implement → `just test` → commit → push → next. One logical change per commit; don't batch unrelated features. Don't push unless asked.

**High-output patterns (proven in swingshift-70, 11k+ LOC/shift):**
- **Commit constantly.** Every green test = commit + push. Small commits (5-50 LOC each) maintain momentum and make rollback trivial. 80+ commits/shift is normal when you're flowing.
- **Read Java first, write Go second.** Read the Java source file completely before porting. Understand the algorithm, then translate idiomatically — don't transliterate line-by-line.
- **Tests find bugs.** Write the test BEFORE assuming the implementation is correct. swingshift-70 found 3 real bugs via tests (InJoin chain flat, UnorderedUnion early return, DistinctUnion ascending-only). Tests are not padding — they're debugging tools.
- **Fuzz is non-negotiable.** Run fuzz targets (`bazelisk test ... --test_arg="-test.fuzz=FuzzXxx" --test_arg="-test.fuzztime=15s" --test_arg="-test.fuzzcachedir=/tmp/fuzz-cache" --sandbox_writable_path=/tmp/fuzz-cache`) on new infrastructure. 200k+ execs should produce 0 panics.
- **Prove with FDB.** Integration tests against real FoundationDB (testcontainers) are the gold standard. A unit test proves the code compiles; an FDB test proves it works. `bazelisk test //pkg/relational/sqldriver:sqldriver_test --test_arg="--test.run=TestFDB_Xxx"` runs specific FDB tests.
- **Subagents for boilerplate.** Delegate test writing, wrapper creation, and mechanical porting to subagents. Keep the critical path (algorithms, rule logic, architectural decisions) in the main context.
- **Don't pad tests, do find gaps.** Use `bazelisk coverage //path:target --combined_report=lcov` to find actual coverage gaps. Only write tests that exercise uncovered code paths or prove new behavior.
- **100% Java alignment unless there's a good reason.** Never simplify "for now" — the simplified version rots and the next shift inherits technical debt. Port the full algorithm, handle all edge cases, match the error messages.
- **Java is the spec for the query planner.** Always read the Java source first, understand the architecture, then port. 1:1 port is king — same class structure, same algorithm, same semantics. No Go-only shortcuts, no "temporary" alternative paths. If Java uses Cascades for all queries, Go uses Cascades for all queries. Java does have `RecordQuerySortPlan`, but only its legacy `RecordQueryPlanner` constructs it; Java Cascades never constructs a physical sort because `RemoveSortRule` must eliminate the logical sort or planning fails. Go's `RecordQueryInMemorySortPlan` is a sanctioned, deeply tested read-side fallback for queries whose requested ordering no access path can satisfy, not evidence that ordering-sensitive Cascades rules may skip Java's ordered-variant enumeration. Same goes for tests: port Java's test cases directly, don't invent Go-only test shapes that diverge from Java's expectations.
- **No parallel pipelines.** Java has one query path (Cascades). Go has one query path (Cascades). Don't maintain a "plangen" or "naive" fallback alongside Cascades — it creates divergence, doubles maintenance, and hides Cascades gaps instead of forcing them to be fixed.

**Fix bugs as you find them.** When a corpus probe (parallel-agent batch or otherwise) surfaces a real Go-side divergence, the default response is: investigate root cause → fix in the same shift → pin the corpus entry. Filing a TODO and dropping the entry is a failure mode — it ships nothing, removes the regression sentinel that would have caught the bug, and dumps the work on the next shift. A 30-line TODO writeup is more expensive than the 50-line fix it punts. **Only file a TODO when the fix is genuinely out of scope:** Java upstream bug, gated on a future Phase, or multi-shift effort. Tiny isolated bugs (one comparison op, one missing dedup-key projection, one error-message tweak) MUST be fixed inline. The corpus is the regression net for cross-engine parity; if you can't pin a shape because Go has a bug, fix the bug — don't grow the corpus around it. Nightshift-65 surfaced 23 bugs and fixed zero; that pattern is now explicitly forbidden.

**Divergence grinding:** `DIVERGENCES.md` is the authoritative list of Go vs Java architectural differences. **100% Java alignment is the default. ALWAYS read Java source FIRST before writing any fix, any port, any new code.** The workflow for closing divergences:
1. **Research phase:** spawn parallel read-only subagents to investigate the Java source code for each divergence. Each agent reads the Java class, notes fields/methods/behavioral differences, and reports whether the divergence is (a) fixable now, (b) blocked on execution layer, or (c) deep architectural.
2. **Port phase:** DFS through the fixable items. For each: read Java → understand the exact semantics → implement the identical semantics in Go → write tests → `just test` → update DIVERGENCES.md. One divergence at a time, fixed to completion.
3. **Document phase:** update DIVERGENCES.md with precise findings — what's fixed, what's remaining, what's blocked and why.

Never rationalize a divergence as "intentional" without first reading the Java code. If Java does X and Go does Y, the default is to port X. Only keep Y if there's a real architectural reason (Go has no sealed classes, Go's fixpoint rule architecture requires a different guard, etc.) — and document that reason in DIVERGENCES.md.

**Delegation:** principal-engineer mindset. Delegate mechanical/boilerplate work to subagents with full context (file paths, snippets, patterns). Critical/tricky pieces: do yourself. Never run two big implementation subagents in parallel.

**Build & verify:** always `just test`. Bazel cache makes incremental runs fast. After Go file/dep changes: `just gazelle` then `bazel mod tidy`. Proto codegen: `buf generate` (not in Bazel). **Always `bazelisk`, never `bazel`** when invoking directly. Never `--no-verify` — investigate hook failures.

Update TODO.md as work completes (`- [x]` with a short note).

## Wire types

Never hand-write FDB wire-type structs in `pkg/fdbgo/wire/types/`. Generate via the C++ schema extractor: register in `cmd/fdb-schema-extract/extract.h`, add `extractType<T>(...)` in `main.cpp`, run `just generate-wire-types && just gazelle`. The only exception is `keyrangeref_custom.go` (documented).

## Stack

Go (see `go.mod`); FoundationDB via pure Go client (`pkg/fdbgo/fdb`) or Apple CGo binding; protobuf (Apple's protos in `proto/apple/`); buf for proto codegen; Bazel 9 via bazelisk + gazelle; nogo lint as build error; just as task runner; testcontainers-go.

Top-level dirs (run `ls`/`tree` for detail):
```
pkg/recordlayer/        Record Layer impl + chaos + cascades + plans
pkg/relational/         SQL engine + cross-engine harnesses
pkg/fdbgo/              Pure Go FDB client
gen/                    Generated proto Go code
proto/apple/            Apple's proto defs
conformance/            Java conformance server + Go conformance tests
shifts/                 Per-shift handovers (YYYY-MM-DD-{shift}.md)
TODO.md                 Authoritative priority list
```

## Running

```sh
just build / test / bench / gazelle / generate / tidy / verify
just bench-one NAME
just binding-stress [N M]   # default 100 seeds × 1000 ops
```

Run a specific Ginkgo test: `bazelisk test //pkg/recordlayer:recordlayer_test --test_arg="--ginkgo.focus=NAME" --test_output=streamed`. Continuous fuzz: `bazelisk run //pkg/...:test -- -test.fuzz='^FuzzName$' -test.fuzztime=60s`. Reproduce a binding-stress seed: `bazelisk run //cmd/fdb-binding-stress -- -seeds 1 -seed-start NNN`. FDB crash debugging: see `pkg/fdbgo/client/CRASH_BUG.md`.

**Stress comparison workflow:** When changing the planner, cost model, or executor, run the 1M stress test before AND after to detect regressions. Use a git worktree for the baseline:
```sh
# Both sides MUST live on the same filesystem — a baseline in /tmp or on another
# disk measures the DISKS, not the change. Check free space first: ext4 point-lookup
# latency degrades sharply above ~95% utilisation and reports as a planner regression.
df -h .
git worktree add ../fdb-baseline master     # sibling path, same fs as this tree
cd ../fdb-baseline && bazelisk test //pkg/relational/sqldriver/stress:stress_test \
  --test_output=streamed --test_arg="--test.run=TestFDB_Stress_1M$" --test_arg="--test.v"
git worktree remove ../fdb-baseline --force

# Current branch:
bazelisk test //pkg/relational/sqldriver/stress:stress_test \
  --test_output=streamed --test_arg="--test.run=TestFDB_Stress_1M$" --test_arg="--test.v"
```
Compare row counts + durations. Record results in `TODO.md` "Stress test 1M baseline" table. Key thresholds: point lookups <5ms, full scans ~3s/1M, index equality <10ms.

## Java compatibility — non-negotiable

Wire-level compatibility is the whole point. These match Java exactly: subspace constants, key construction (FDB tuple encoding), protobuf format, record store header, builder pattern, continuation tokens (proto-wrapped, magic `6773487359078157740`), index entry format, split record format (100KB chunks at suffixes 1+; unsplit at 0), record version storage (inline at `pk + -1` suffix, format version ≥ 6).

FDB constraints: 5s tx limit, 100KB value limit, 10MB tx limit, ~10KB key limit. Cursors need `TimeScanLimiter` + continuations; values use split records.

Java source at `fdb-record-layer/` (gitignored, tag **4.12.11.0**, matches MODULE.bazel pins).

## Design principles

1. **Compatibility first** — match Java wire format exactly.
2. **C++ is the spec for the FDB client** — Go divergence from C++ is a bug in Go. Never skip a divergence test; fix Go.
3. **No mocks.**
4. **Explicit errors** — never panic in library code.
5. **Simple code** — three similar lines beats a premature abstraction.
6. **Proto fidelity** — open enums, field presence, wire compat.
7. **Test hard** — `t.Parallel()`, edge cases, Java interop.
8. **Error types, not sentinels** — see below.
9. **Never paper over bugs** — early-return tolerance gates compound across shifts and hide real failures. Pin the actual expected behaviour.
10. **Emergent behaviour over special-case checks** — match the architectural property that produces the behaviour, not a downstream observable. Bolted-on `if X { throw }` checks diverge the moment Java's structure changes.
11. **Resolve a conflict by reading it, never by stripping the markers** — a conflict is not always two additions to merge. It is often one side *correcting* what the other still asserts, and "keep both" then silently reverts the correction while compiling cleanly and passing the whole suite. That failure is invisible to `go vet`, to the compiler and to a green run; the loud ones (a brace mismatch, a duplicate map key) are the lucky cases. Restore the markers with `git checkout --conflict=merge` and read each hunk. The resolution differs *per hunk within one file*: the first may be genuinely balanced and keeping both is right, and the next may need one side outright because it is the newer, corrected text.

## Cross-engine SQL conformance

Conformance principle: **doesn't work in Java → doesn't work in Go**, in the same architectural way (visitor-doesn't-implement → fall-through-to-default), with identical error wording where the message can be cleanly shared.

This governs the **shared** query surface — inputs Java also attempts. It is NOT a ban on capabilities Java lacks entirely: net-new read-side query extensions are allowed when wire compat holds (see "Wire compat is the hard line; query reach is not" near the top). The rule means *don't silently diverge from Java where both engines run the same query*, not *never exceed Java*.

Don't enumerate Java's quirks in this file — find them via the cross-engine harness, document each at the relevant code site, capture open ones in TODO.md.

## Error handling

Java exception class = Go error struct, always. Use `errors.As()` to match (the Go equivalent of `catch (SpecificException e)`). Never use bare `var ErrFoo = errors.New("...")` sentinels — they can't carry the structured context Java's `addLogInfo()` provides.

```go
type RecordAlreadyExistsError struct { PrimaryKey tuple.Tuple }
func (e *RecordAlreadyExistsError) Error() string { ... }

return &RecordAlreadyExistsError{PrimaryKey: pk}

var e *RecordAlreadyExistsError
if errors.As(err, &e) { ... }
```

Carry the same context fields as the Java exception's `addLogInfo()` keys. Wrap with `fmt.Errorf("...: %w", err)` to add call-site context while preserving `errors.As()` unwrapping. For genuinely message-only Java exceptions, use `XxxError{Message string}`.

## Proto definitions

Use Java's protos directly. Canonical at `fdb-record-layer/fdb-record-layer-core/src/main/proto/`; `proto/apple/` mirrors them. Copy the full proto when adding new types — don't hand-maintain a subset.

## Chaos testing

Model-based, at `pkg/recordlayer/chaos/`. An in-memory `StoreModel` shadows the real store; `Verify()` compares them after each operation. `ChaosTransactor` injects faults at tx boundaries via the production-side `NewFDBDatabaseWithTransactor` hook. Targeted (`s.InjectOnce(FaultCommitUnknown)`) or random (`WithSeed(N), WithFaults(FaultsRetryHeavy)`); seed for reproducibility. Extend by adding a `FaultType` + `ChaosTransactor.Transact` arm, or a new `Verify()` invariant, or a new `StoreModel` state field.

## Status

For up-to-date counts and shift-by-shift state, read the most recent file in `shifts/` and run the relevant tooling: `just test` for spec count, `grep -rE "^func Fuzz" pkg/` for fuzz targets, `just coverage` for HTML coverage, `just bench` for benchmarks.
