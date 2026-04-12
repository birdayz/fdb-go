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

```bash
# Determine shift name from current time + next number
git checkout -b {shift-name} master
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

3. **Wait for reviewer feedback.** The reviewer (bot or human) will comment on the PR. Read the feedback:
   ```bash
   gh api repos/{owner}/{repo}/issues/{pr}/comments
   ```

4. **Address feedback** — but stay critically thinking. Don't blindly apply every suggestion. If a suggestion is wrong or unnecessary, explain why in a reply. For valid feedback:
   - Fix the issue
   - Reply to the reviewer's comment confirming the fix
   - Commit, push

5. **Repeat** steps 3-4 until the reviewer approves or feedback is fully addressed.

## Step 5: End shift

When the user says shift is over, or 8 hours have passed:

1. **Write handover** — create `shifts/{shift-name}.md` with:
   - Date, time range, PR number
   - What was done (grouped by category)
   - Current state (test counts, CI status, open issues)
   - Known issues / tech debt discovered
   - What to work on next (prioritized)

2. **Final commit + push** the handover doc.

3. **Wait for CI green.**

4. **Merge PR** (only if review is approved):
   ```bash
   gh pr merge --squash --subject "{shift-name}: {summary}"
   ```

5. **Clean up:**
   ```bash
   gh pr list --state open  # close any stale PRs
   git push origin --delete {shift-name}  # delete remote branch
   ```

## Identifying shift state

If you're unsure whether a shift is in progress:
- `gh pr list --state open` — open PR = shift in progress
- `ls -t shifts/*.md | head -1` — newest handover = last completed shift
- If no open PRs → start a new shift
