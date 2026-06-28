---
name: todo-worker
description: Pick the next unchecked TODO.md item, write an RFC, get it reviewed by Graefe+Torvalds, implement, review again, open PR, iterate with @claude reviewer until LGTM.
---

# TODO Worker Workflow

Pick the next unchecked item from TODO.md and drive it from RFC through implementation to merged PR. Every step has mandatory review gates.

## Step 0: Find the next item and set the goal

1. Read `TODO.md`.
2. Find the **lowest-numbered unchecked item** whose gates are satisfied. Priority order: P0 > P1 > P2 > Phase items.
3. State clearly which item you're working on.
4. Ask the user to set a `/goal` reflecting the work, so the harness keeps driving it to completion across turns. `/goal` is a built-in CLI command — you cannot invoke it; output the exact line for the user to paste:

   ```
   /goal <item-id>: <short description> — RFC written, reviewed (Graefe+Torvalds ACK), implemented, tests green, PR created, @claude LGTM
   ```

   Example: `/goal P1.2: QOV-based FieldValue migration — RFC written, Graefe+Torvalds ACK, all stripAlias calls eliminated, tests green, PR merged with @claude LGTM`

## Step 1: Load the domain-review skill (sets the reviewer gate)

The two domains with their own architectural reviewer (substituting for the generic
Graefe+Torvalds RFC review) are the query engine and the pure-Go FDB client. Load the
matching skill — it sets the correct reviewer gate, key-file map, and spec anchor for the
RFC/implementation reviews in Steps 5 and 7.

- **Query engine** (planner, optimizer, executor, plan cache): `Skill(skill: "query-engine")`
  — loads the Graefe (Cascades) + Torvalds protocols and Java-as-spec.
- **Pure-Go FDB client** (anything under `pkg/fdbgo/` — transport, transaction, commit path,
  RYW, retry/ctx, wire encoding — or a `TODO-production.md` "client robustness" item):
  `Skill(skill: "fdb-client-review")` — loads the **FDB C++ client developer** (substitute
  for Graefe; validates Go against the 7.3.77 C++ source at `/tmp/fdbsrc`) + Torvalds
  protocols and C++-as-spec. For a *divergence* found via differential/fuzz, also load
  `hunt-divergences`.

When this skill sets a domain reviewer, use it in place of Graefe in Steps 5 and 7 (e.g.
client items → **FDB C++ dev + Torvalds**, not Graefe + Torvalds). If the item is purely
record-layer or generic infra with no client/wire or planner contract, skip — the generic
Graefe+Torvalds RFC review applies.

## Step 2: Research

1. Read the current Go code involved (find files, read them).
2. Read the corresponding **Java source** (`fdb-record-layer/`) to understand how Java handles the same thing.
3. Spawn an `Explore` agent if the search space is broad.
4. **Verify the item is real and correctly framed before designing.** TODO lines rot. Two failure modes to rule out first:
   - *Already done:* part or all of it may already work (e.g. via a normalization or shared path the item's author missed). Grep for the feature + check existing tests before building it again.
   - *Mis-framed as "Java parity":* confirm Java actually supports the feature. If Java doesn't support it **at all** (e.g. its visitor is a no-op with zero tests), you're adding a **Go-only read-side extension** (allowed if wire compat holds + deep tests — see CLAUDE.md "Wire compat is the hard line"), not closing a divergence. That changes the design bar and the reviewer framing — say so explicitly in the RFC.

## Step 3: Branch

```bash
git checkout -b fix/<item-id>-<short-description>
```

Use `fix/` for bugs, `feat/` for features, `refactor/` for cleanups.

## Step 4: Write the RFC

Create `rfcs/NNN-<short-name>.md` (next number after `ls rfcs/ | sort -V | tail -1`).

The RFC must contain:
- **Problem**: what's broken/missing and why it matters
- **Investigation**: what you found reading Java + Go code
- **Fix**: the concrete API/data structure/algorithm change
- **Performance**: why the fix doesn't regress perf (or how it improves it)
- **Test plan**: what tests prove the fix works

Keep it short — 1-2 pages max. No fluff.

## Step 5: RFC review (Graefe + Torvalds, parallel)

Launch BOTH reviewers in parallel as background agents:

```
Agent(description: "Graefe RFC review", prompt: "You are Goetz Graefe, author of the Cascades optimization framework paper. Review the RFC at /home/birdy/projects/fdb-record-layer-go/rfcs/NNN-xxx.md. [describe what changed and why]. Evaluate Cascades alignment. Under 300 words. ACK or NAK with specific reasons.", run_in_background: true)

Agent(description: "Torvalds RFC review", prompt: "You are Linus Torvalds. Review the RFC at /home/birdy/projects/fdb-record-layer-go/rfcs/NNN-xxx.md. Also read the current implementation at [relevant files]. Focus on dead code, logic holes, incomplete conversions, papered-over regressions. Under 300 words. ACK or NAK with specific reasons.", run_in_background: true)
```

### Handling results:
- **Both ACK** → proceed to implementation.
- **Any NAK** → address the specific concern, then re-launch the NAK'ing reviewer. Do NOT proceed with a NAK outstanding.

## Step 6: Implement

1. One logical change at a time.
2. Write tests BEFORE assuming the implementation is correct.
3. Run `just test` after every change.
4. Commit on green.

Follow CLAUDE.md rules: no skips, no mocks, DFS not BFS, Java is the reference.

## Step 7: Implementation review (Graefe + Torvalds, parallel)

After implementation passes all tests, launch BOTH reviewers on the implementation diff:

```
Agent(description: "Graefe implementation review", prompt: "You are Goetz Graefe, author of the Cascades optimization framework paper. Review the diff in /home/birdy/projects/fdb-record-layer-go. Run `git diff HEAD~N` (or `git diff master..HEAD` for all commits). [describe what changed and why]. Evaluate Cascades alignment and correctness. Under 300 words. ACK or NAK.", run_in_background: true)

Agent(description: "Torvalds implementation review", prompt: "You are Linus Torvalds. Review the diff in /home/birdy/projects/fdb-record-layer-go. Run `git diff master..HEAD`. Focus on dead code, logic holes, incomplete conversions, papered-over regressions. Under 300 words. ACK or NAK with file:line references.", run_in_background: true)
```

### Handling results:
- **Both ACK** → proceed to PR.
- **Any NAK** → fix the issue, re-run `just test`, commit, then re-launch the NAK'ing reviewer. Iterate until both ACK.

## Step 8: Update TODO.md

Mark the item as done: `- [x] **P0.X ...** Fixed in RFC-NNN — [short description of what was done].`

Commit this with the implementation (or as a separate commit if already committed).

## Step 9: Push and create PR

```bash
git push -u origin <branch-name>
```

Create the PR:

```bash
gh pr create --title "<type>: <short description> (<item-id>)" --body "$(cat <<'EOF'
## Summary
- [bullet points of what changed and why]

RFC-NNN. Graefe ACK, Torvalds ACK.

## Test plan
- [x] [specific tests that prove the fix]
- [x] N/N test targets pass
- [x] Pre-commit hook green
EOF
)"
```

## Step 10: Tag @claude reviewer and iterate

Post a comment tagging @claude with context:

```bash
gh pr comment <PR#> --body "@claude Please review this PR. <1-2 sentence summary of what changed and why>. See RFC-NNN for the full analysis."
```

### Monitoring — how to wait for @claude's approval

@claude does not post a fresh final comment; it posts ONE comment and **edits
it in place** through three states:
1. a `Claude Code is working…` stub (appears within seconds),
2. a `### Tasks` checklist whose boxes flip from `- [ ]` to `- [x]`,
3. the final review, which **opens with a `**Claude finished … in Xm Ys**` banner** and ends with a verdict (`Approved` / findings / changes requested).

**Detect completion by the `Claude finished` banner (a positive signal), or by the linked GitHub Actions run reaching `completed`.** Never gate on:
- *comment count or body length* — the stub is long; length plateaus mid-review;
- *unchecked `- [ ]` boxes* — the FINISHED review body legitimately contains checkboxes (test plans, nit lists), so a checkbox scan false-positives as "in progress" and the poll spins until timeout (this exact bug burned ~15 min on a review that finished in 75s);
- *the word "working"/"in progress"* — those words appear in normal review prose.

Poll on a 15–20s cadence (a review takes ~1–5 min). Strongest signal — the Actions run:
```bash
runid=$(printf '%s' "$body" | grep -oE 'runs/[0-9]+' | head -1 | cut -d/ -f2)
[ -n "$runid" ] && [ "$(gh run view "$runid" --json status --jq .status)" = "completed" ] && break
```

Then wait for @claude's review to **complete**:

```bash
# @claude posts ONE comment and edits it in place: a "Claude Code is
# working…" stub → a "### Tasks" checklist → the final review, which opens
# with a "**Claude finished … in Xm Ys**" banner.
#
# Detect completion by that POSITIVE banner — NOT by length, comment count,
# or "unchecked - [ ] boxes". The finished review body legitimately contains
# "- [ ]" (test plans, nit lists), so a checkbox scan false-positives as
# "in progress" and the poll spins until timeout. The banner is reliable;
# the GitHub Actions run linked in the comment ("[View job](…/runs/<id>)")
# is an even stronger signal — `gh run view <id> --json status` is
# "completed" when done.
base=$(gh api repos/birdayz/fdb-record-layer-go/issues/<PR#>/comments --jq '[.[]|select(.user.login=="claude[bot]")]|length')
while :; do
  sleep 20
  cnt=$(gh api repos/birdayz/fdb-record-layer-go/issues/<PR#>/comments --jq '[.[]|select(.user.login=="claude[bot]")]|length')
  body=$(gh api repos/birdayz/fdb-record-layer-go/issues/<PR#>/comments --jq '[.[]|select(.user.login=="claude[bot]")]|last|.body')
  [ "$cnt" -gt "$base" ] || continue                                   # new comment appeared?
  printf '%s' "$body" | grep -qiE "Claude finished|finished @" && break # review complete
done
```

### Handling @claude findings:

- **LGTM with no findings** → proceed to Step 11.
- **LGTM with minor findings** / **Blocking findings** → fix each finding, commit, push, then re-request and **wait for a fresh completed review**:
  ```bash
  gh pr comment <PR#> --body "Addressed findings: [list]. Latest HEAD <sha>."
  gh pr comment <PR#> --body "@claude Please re-review — findings addressed in <sha>."
  ```
  Iterate until a clean LGTM **on the current HEAD**.

## Step 11: Done

The item is done only when **@claude's clean LGTM is the LAST comment on the PR and was posted against the current HEAD commit.**

- **A prior LGTM does NOT cover later commits.** Every time you push more commits (even a one-line fix or a reviewer-requested change), the earlier approval is stale — you MUST re-request and wait for a new completed review on the new HEAD. Verify the SHA @claude reviewed matches HEAD.
- **Do not post anything to the PR after @claude's final LGTM** — that would make your comment the last word instead of the approval. If you must comment after, re-request review again so @claude's LGTM is restored as the final comment.
- Only then tell the user the PR is ready to merge, with the URL.

## Key rules

- **Never ship with a NAK** from Graefe, Torvalds, or @claude.
- **Every review cycle is parallel** — Graefe and Torvalds always launch together.
- **@claude iteration is sequential** — fix findings, push, re-request, wait for a completed review.
- **The final LGTM must be on the final code.** Re-request after every push; "done" = @claude's clean LGTM is the last PR comment, on the current HEAD. A stale approval on an earlier commit doesn't count.
- **A discovered bug is not fixed until a regression test pins it.** Reviewer catches (Graefe/Torvalds/@claude) that the suite missed are doubly important: fix the bug AND add the test that should have caught it.
- **RFC status progression**: Draft → Implemented (update when the code ships).
- **Commits pass pre-commit hooks** — never `--no-verify`.
- **One logical change per commit** — don't batch unrelated fixes.
