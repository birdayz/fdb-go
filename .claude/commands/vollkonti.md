# Vollkonti Shift System

You are starting a shift in a Vollkontinuierliches Schichtsystem (continuous 24/7 shift operation). This project is industrialized — work is done in 8-hour shifts with structured handovers.

## Shift types

| Shift | Hours (CEST) | Name pattern |
|---|---|---|
| Day shift | 06:00 — 14:00 | `dayshift-N` |
| Swing shift | 14:00 — 22:00 | `swingshift-N` |
| Night shift | 22:00 — 06:00 | `nightshift-N` |

Determine which shift type matches the current time. The number N increments globally — check `ls -t shifts/*.md | head -1` to find the last shift number, then increment.

## Step 1: Read handover

Read the latest handover document:
```
ls -t shifts/*.md | head -1
```
Read it thoroughly. This is your ONLY briefing — you have zero prior context.

## Step 2: Start shift

GitHub won't create a PR with zero commits between branches. Use `--allow-empty` to bootstrap:

```bash
# Determine shift name from current time + next number
git checkout -b {shift-name} master
git commit --allow-empty -m "{shift-name}: start shift"
git push -u origin {shift-name}
gh pr create --draft --title "{shift-name}: {one-line goal from handover priorities}"
```

## Step 3: Work

Set up the work loop:
```
/loop 15m keep working until shift over
```

Then start working on the highest-priority items from the handover. Follow the working rhythm in CLAUDE.md:
- One thing at a time
- Implement, test (`just test`), commit, push, move on
- Delegate grunt work to subagents, review their output
- Update TODO.md as work completes

### Rules during shift
- **One PR per shift.** All work goes to the shift branch.
- **Never push directly to master.** Always through the PR.
- **Never force-push master.** Always verify branch before amend/force: `git branch --show-current`
- **C++ is the spec.** If our Go client diverges from C++, fix our code. Never skip tests.
- **CI must be green** on every push. Pre-commit hooks catch most issues.

## Step 4: Review loop

When implementation is done and tests pass:

1. **Push and mark PR ready for review:**
   ```bash
   git push origin {shift-name}
   gh pr ready  # remove draft status
   ```

2. **Request review** by commenting on the PR:
   ```bash
   gh pr comment --body "@claude review"
   ```

3. **Wait for reviewer feedback.** Use `gh run watch <id>` to wait for the review job, then read comments:
   ```bash
   gh run list --branch {shift-name} --limit 1 --json databaseId --jq '.[0].databaseId'
   gh run watch <id> --exit-status
   gh api repos/{owner}/{repo}/issues/{pr}/comments --jq '.[-1].body'
   ```

4. **Address feedback** — but stay critically thinking. Don't blindly apply every suggestion. If a suggestion is wrong or unnecessary, explain why in a reply. For valid feedback:
   - Fix the issue
   - Commit, push
   - **CRITICAL: Request re-review.** Comment on the PR summarizing what was fixed and tag `@claude` to trigger another review:
     ```bash
     gh pr comment --body "@claude Fixed X (commit abc123). Please review again."
     ```

5. **Wait for the new review** (step 3 again). Read the feedback. If new issues found, fix and re-request (step 4). **Keep iterating until the reviewer finds no new issues.** Only then is the PR merge-ready.

## Step 5: End shift

Only after the reviewer approves (no new issues in the latest review round):

1. **Write handover** — create `shifts/{shift-name}.md` with:
   - Date, time range, PR number
   - What was done (grouped by category)
   - Current state (test counts, CI status, open issues)
   - Known issues / tech debt discovered
   - What to work on next (prioritized)

2. **Commit + push** the handover doc.

3. **Merge PR** once CI is green (Bazel cache makes this fast):
   ```bash
   gh pr merge --squash --subject "{shift-name}: {summary}"
   ```

4. **Clean up:**
   ```bash
   git checkout master && git pull origin master
   git push origin --delete {shift-name}  # delete remote branch
   ```

## Identifying shift state

If you're unsure whether a shift is in progress:
- `gh pr list --state open` — open PR = shift in progress
- `ls -t shifts/*.md | head -1` — newest handover = last completed shift
- If no open PRs → start a new shift
