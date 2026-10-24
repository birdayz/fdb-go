---
name: todo-worker
description: Pick the next unchecked TODO.md item, claim an RFC number with a draft PR, write the RFC and post a bruh-code-review of it, implement, post reviews until bruh-code-review ACKs the PR's final HEAD.
---

# TODO Worker Workflow

Pick the next unchecked item from TODO.md and drive it from RFC through implementation to merged PR. Every step has mandatory review gates.

## Step 0: Find the next item and set the goal

1. Read `TODO.md`.
2. Find the **lowest-numbered unchecked item** whose gates are satisfied. Priority order: P0 > P1 > P2 > Phase items.
3. State clearly which item you're working on.
4. In an interactive session, ask the user to set a `/goal` reflecting the work, so the harness keeps driving it to completion across turns (skip this in an unattended or headless run: nobody can paste it). `/goal` is a built-in CLI command — you cannot invoke it; output the exact line for the user to paste:

   ```
   /goal <item-id>: <short description> — RFC written, reviewed (bruh-code-review ACK), implemented, tests green, PR created, bruh-code-review ACK on the PR's final HEAD
   ```

   Example: `/goal P1.2: QOV-based FieldValue migration — RFC written, bruh-code-review ACK, all stripAlias calls eliminated, tests green, PR merged with a bruh-code-review ACK on its final HEAD`

## Step 1: Load the domain skill

Every review in Steps 5, 7 and 10 runs the **`bruh-code-review`** skill. The repository's
lenses in `.bruh/review/` apply by path to code: `cascades` (planner), `java-compat` (record
layer and SQL), `fdb-client` (`pkg/fdbgo`), `spfresh` (vector index). An RFC matches none of
them, so Step 5 names them with `lenses:`. Two domains also have a
skill that sets the key-file map and spec anchor; load the matching one:

- **Query engine** (planner, optimizer, executor, plan cache): `Skill(skill: "query-engine")`
  — Java-as-spec and the planner lessons learned.
- **Pure-Go FDB client** (anything under `pkg/fdbgo/` — transport, transaction, commit path,
  RYW, retry/ctx, wire encoding — or a `TODO-production.md` "client robustness" item):
  `Skill(skill: "fdb-client-engineer")` — C++-as-spec, validated against the 7.3.77 C++
  source at `/tmp/fdbsrc`. For a *divergence* found via differential/fuzz, also load
  `hunt-divergences`.

If the item is purely record-layer or generic infra with no client/wire or planner
contract, skip this step.

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

Claim the number first, then write. NNN is one above the highest number on origin/master or in
any PR branch (another run may have taken the next one):

```bash
git fetch -q origin master '+refs/pull/*/head:refs/remotes/origin/pr/*'
for r in origin/master $(git for-each-ref --format='%(refname:short)' refs/remotes/origin/pr); do
  git ls-tree --name-only "$r" rfcs/; done | sed 's#^rfcs/##' | grep -E '^[0-9]{3}-' | sort -V | tail -1
```

Commit a one-line stub `rfcs/NNN-<short-name>.md` (`# RFC-NNN: <title>`, **Status: Draft**),
push, and open the draft PR at once (Step 5's command). The number is taken from then on; then
write the RFC into the stub.

The RFC must contain:
- **Problem**: what's broken/missing and why it matters
- **Investigation**: what you found reading Java + Go code
- **Fix**: the concrete API/data structure/algorithm change
- **Performance**: why the fix doesn't regress perf (or how it improves it)
- **Test plan**: what tests prove the fix works

Keep it short — 1-2 pages max. No fluff.

## Step 5: Draft PR and RFC review (bruh-code-review)

The draft PR is open from Step 4 (`gh pr create --draft --title "<type>: <short description>
(<item-id>)" --body "RFC-NNN, in progress."`). Commit the RFC and push. Every review from here on is posted to it: the PR keeps one bruh summary, a
`bruh/review` commit status, and the open findings as review threads, and each posted run
re-checks those findings itself (no `previous:`).

Run the **`bruh-code-review`** skill on the PR and post it (`review PR <N> and post it`, with
`intent:` naming the TODO item it addresses, and `lenses:` naming the repository lenses of the
area it changes: `fdb-client` for `pkg/fdbgo`, `cascades` for the planner, `java-compat` for
record-layer or relational bytes, `spfresh` for the vector index). An RFC matches no lens path,
so without `lenses:` only the core lenses review it.

### Handling results:
- **ACK** (the summary says ACK on the current HEAD) → set the RFC's **Status: Accepted**,
  commit, and proceed to implementation.
- **NAK** → address the findings, commit, push, and post a new review. Do NOT proceed with a NAK outstanding.

## Step 6: Implement

1. One logical change at a time.
2. Write tests BEFORE assuming the implementation is correct.
3. Run `just test` after every change.
4. Commit on green.

Follow CLAUDE.md rules: no skips, no mocks, DFS not BFS, Java is the reference.

## Step 7: Implementation review (bruh-code-review)

After implementation passes all tests, push and post a review of the PR (`review PR <N> and
post it`, with `intent:` naming the item and RFC-NNN).

### Handling results:
- **ACK** → proceed.
- **NAK** → fix the findings, re-run `just test`, commit, push, and post a new review. Iterate until it ACKs the current HEAD.

## Step 8: Update TODO.md

Mark the item as done: `- [x] **P0.X ...** Fixed in RFC-NNN — [short description of what was done].`
Set the RFC's **Status: Implemented**.

Commit this with the implementation (or as a separate commit if already committed).

## Step 9: Mark the PR ready

Push, write the PR body, and mark it ready:

```bash
gh pr edit <N> --body "$(cat <<'EOF'
## Summary
- [bullet points of what changed and why]

RFC-NNN. [The bruh-code-review verdicts so far, as the PR's bruh summary shows them.]

## Test plan
- [x] [specific tests that prove the fix]
- [x] N/N test targets pass
- [x] Pre-commit hook green
EOF
)"
gh pr ready <N>
```

## Step 10: PR review and iterate

Run the **`bruh-code-review`** skill on the PR and post it: `review PR <PR#> and post it`. It
edits the PR's bruh summary, sets the `bruh/review` status, resolves the threads of findings it
re-checked as fixed, and posts a review only for new findings.

- **ACK** (the report's last line is `bruh-code-review: ACK <sha>`, `<sha>` the PR's current
  HEAD) → proceed to Step 11.
- **NAK** → fix each finding (with a regression for any bug), commit, push, then post a new
  review of the PR. Iterate until it ACKs the current
  HEAD. If you believe a finding is wrong, give the evidence in the PR description and
  review again; the re-review settles it.

## Step 11: Done

The item is done only when **the latest bruh-code-review posted to the PR is an ACK of the
current HEAD**: the PR's bruh summary says ACK on that commit and its `bruh/review` commit
status is success (`gh pr checks <N>`). Headless, `bruh review <N> --post` exits 0 on ACK,
4 on NAK and 3 when the PR moved past the reviewed commit (nothing posted: push, then post
again).

- **An ACK only covers the HEAD it reviewed.** Every push, even a one-line fix or a
  reviewer-requested change, makes it stale: review the new HEAD and post again (`post`
  refuses a report for a commit the PR has moved past).
- Only then tell the user the PR is ready to merge, with the URL.

## Key rules

- **Never ship with a NAK** from bruh-code-review, on the RFC, the implementation or the PR.
- **Review iteration is sequential** — fix findings, push, review again, read the new report.
- **The final ACK must be on the final code.** Review again after every push; "done" = a posted bruh-code-review ACK of the current HEAD. A stale ACK on an earlier commit doesn't count.
- **A discovered bug is not fixed until a regression test pins it.** Review findings that the suite missed are doubly important: fix the bug AND add the test that should have caught it.
- **RFC status progression**: Draft → Accepted (the RFC review ACKs it) → Implemented (the
  code ships).
- **Commits pass pre-commit hooks** — never `--no-verify`.
- **One logical change per commit** — don't batch unrelated fixes.
