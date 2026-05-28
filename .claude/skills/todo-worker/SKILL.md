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

## Step 1: Load the query-engine skill

If the item touches the planner, optimizer, executor, or plan cache:

```
Skill(skill: "query-engine")
```

This loads the Graefe/Torvalds reviewer protocols, key file map, and lessons learned. If the item is purely record-layer or infra, skip this.

## Step 2: Research

1. Read the current Go code involved (find files, read them).
2. Read the corresponding **Java source** (`fdb-record-layer/`) to understand how Java handles the same thing.
3. Spawn an `Explore` agent if the search space is broad.

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

Then wait for @claude's response:

```bash
# Wait for the review to land
until [ "$(gh api repos/birdayz/fdb-record-layer-go/issues/<PR#>/comments --jq 'length')" -gt <current_count> ]; do sleep 10; done
```

**Note:** @claude's "working" spinner updates in-place. Wait for the comment body length to exceed ~500 chars to get the full review, not just the spinner.

### Handling @claude findings:

- **LGTM with no findings** → tell the user it's ready to merge.
- **LGTM with minor findings** → fix each finding, commit, push, then post a follow-up comment:
  ```bash
  gh pr comment <PR#> --body "Addressed findings: [list what was fixed]."
  ```
  Then re-request review:
  ```bash
  gh pr comment <PR#> --body "@claude Please re-review — findings addressed in latest commit."
  ```
  Wait for @claude's re-review. Iterate until clean LGTM.
- **Blocking findings** → same flow but the fix is mandatory before proceeding.

## Step 11: Done

Tell the user the PR is ready to merge. Provide the PR URL.

## Key rules

- **Never ship with a NAK** from Graefe, Torvalds, or @claude.
- **Every review cycle is parallel** — Graefe and Torvalds always launch together.
- **@claude iteration is sequential** — fix findings, push, re-request, wait.
- **RFC status progression**: Draft → Implemented (update when the code ships).
- **Commits pass pre-commit hooks** — never `--no-verify`.
- **One logical change per commit** — don't batch unrelated fixes.
