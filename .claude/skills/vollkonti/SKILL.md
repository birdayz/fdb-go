---
name: vollkonti
description: Start or continue a shift in the Vollkonti 24/7 shift system. Determines shift type (day/swing/night) from current time, reads handover, creates branch+PR.
---

# Vollkonti Shift System

You are starting a shift in a Vollkontinuierliches Schichtsystem (continuous 24/7 shift operation). This project is industrialized — work is done in 8-hour shifts with structured handovers.

## Shift types

| Shift | Hours (CEST) | Name pattern |
|---|---|---|
| Day shift | 06:00 — 14:00 | `dayshift-N` |
| Swing shift | 14:00 — 22:00 | `swingshift-N` |
| Night shift | 22:00 — 06:00 | `nightshift-N` |

Determine which shift type matches the current time. The number N increments globally — check `ls shifts/*.md | sort -r | head -1` to find the last shift, extract the number, then increment.

**Filenames are date-prefixed:** `shifts/YYYY-MM-DD-{shift-name}.md` (e.g., `shifts/2026-04-12-swingshift-8.md`). Date prefix means `ls | sort -r` gives reverse-chronological order instantly.

## Step 1: Check for active shift

**Before anything else**, check if a shift is already in progress:
```bash
gh pr list --state open
```
- If there's an open PR → a shift is active. Read the PR, check out that branch, and **continue the shift** instead of starting a new one.
- If no open PRs → start a new shift (Step 2).

## Step 2: Read handover

Read the latest handover document:
```
ls shifts/*.md | sort -r | head -1
```
Read it thoroughly. This is your ONLY briefing — you have zero prior context.

## Step 3: Start shift

GitHub won't create a PR with zero commits between branches. Use `--allow-empty` to bootstrap:

```bash
# Determine shift name from current time + next number
git checkout -b {shift-name} master
git commit --allow-empty -m "{shift-name}: start shift"
git push -u origin {shift-name}
gh pr create --draft --title "{shift-name}: {one-line goal from handover priorities}"
```

## Step 4: Work

**Shift end times are FIXED clock times, not durations.** Day shift ends 14:00 regardless of when you started. Swing ends 22:00. Night ends 06:00. Starting late means a SHORTER shift, not a later end. If you start at 09:18 on a day shift, you have until 14:00 — wind-down begins at 13:30, hard stop at 14:00. Compute the wind-down mark from the shift-end clock, not from your start time.

**Within those bounds, work is continuous.** Work doesn't pause for clock checks. Stopping happens for two reasons only: the user explicitly intervenes (mid-shift review prompt, feedback, redirect), or wind-down begins. Everything else is "keep working."

### Mid-shift check-in — event-driven, NOT timer-driven

There IS a mid-shift check-in concept — a quality gate where the user (or a reviewer process) evaluates what's been shipped so far. **Critical rules:**

- **DO NOT watch the clock for it.** Don't compute a T+3:30 mark, don't ScheduleWakeup for it, don't refuse to start new work because "mid-shift is in 30 min." You'll just burn shift time pacing.
- **DO NOT stop working in anticipation.** Keep shipping commits. The check-in happens when it happens — when the user pings, when a reviewer comment arrives, when an external trigger fires. Until then, work.
- **Late is fine.** Better to be 30 min late on a check-in (because you were heads-down on a substantive piece) than to idle for 30 min watching the clock.
- **When the check-in DOES happen** — pause, request `@claude review` on the PR, iterate with the reviewer until LGTM, then keep going. The review is a quality gate, not a stop signal.

The wall-clock numbers below are descriptive context, NOT triggers:

| Phase | Approx when | What |
|---|---|---|
| Active work | most of the shift | Work the highest-priority TODOs. Ship features. One thing at a time. |
| Mid-shift check-in | somewhere around the middle (event-driven) | Reviewer evaluates state, you iterate to LGTM, then keep going. |
| Wind-down | last 30 min before shift-end clock | Stop starting NEW features. Final review iteration, verification (fuzz / stress are passive — fine), write handover, merge. |

Wind-down DOES have a clock trigger (last 30 min) because the shift literally ends. Mid-shift does NOT — that one's event-driven.

If you finish the handover's priorities early, keep working — see "When the main task is done, keep working" below. Idling until the shift clock runs out is a failure mode.

Set up a 15-minute kick timer immediately:
```
/loop 15m get back to work
```

Then start working on the highest-priority items from the handover. Follow the working rhythm in CLAUDE.md:
- One thing at a time
- Implement, test (`just test`), commit, push, move on
- Delegate grunt work to subagents, review their output
- Update TODO.md as work completes

### Rules during shift
- **One PR per shift.** All work goes to the shift branch. Do NOT merge early — the shift runs until the clock says it's over. Keep working, keep committing, keep pushing. Merge only at end-of-shift after review.
- **Never push directly to master.** Always through the PR.
- **Never force-push master.** Always verify branch before amend/force: `git branch --show-current`
- **C++ is the spec.** If our Go client diverges from C++, fix our code. Never skip tests.
- **CI must be green** on every push. Pre-commit hooks catch most issues.
- **Mid-shift review is event-driven, NOT timer-driven.** When the user prompts for it (or an external trigger fires), pause, request `@claude review`, iterate to LGTM, then keep going. NEVER stop working in anticipation of mid-shift; NEVER compute a T+3:30 mark and pace yourself toward it. Being a bit late is fine; idling for the clock is not. A clean mid-shift review is a quality signal, not a stop signal.
- **When the main task is done, keep working.** Write more tests, investigate performance, update docs, run binding stress, profile allocations, audit code you haven't touched. A foreman doesn't clock out early because the main job finished — there's always cleanup, testing, and prep for the next shift.
- **You MUST keep working until wind-down.** Idling "because piling on more changes risks regressions" or "because the reviewer already approved" is not an option. The risk profile is low (CI + review catch regressions); the cost of an idle shift is high (less ground covered, more work dumped on the next shift). If you genuinely have nothing to do, that means you haven't looked hard enough — audit code you haven't touched, extend fuzz corpus, tighten tests, polish docs. Sitting waiting for review feedback is only acceptable inside the wind-down window. Scheduling wakeups to pass time before wind-down is a mis-use.
- **Wind down 30 min before shift-end clock.** Day shift wind-down 13:30, hard stop 14:00. Swing wind-down 21:30, hard stop 22:00. Night wind-down 05:30, hard stop 06:00. Compute the wind-down mark from the END clock, not from your start time — late starts cut into work time, not into wind-down. Use the last 30 min for: final review iteration, verification (fuzz / stress are passive — fine), handover doc, merge. New code stops; passive verification continues.
- **In wind-down, once the reviewer LGTMs, merge and stop.** Do not pile on more changes inside the last 30 min just to fill time — a clean LGTM in wind-down is the merge signal. Write the handover, merge the PR, clean up. Sitting through the rest of the wind-down window after a clean review is fine; shipping more code is not.
- **NEVER push to master directly.** Even after merging a shift PR, create a NEW branch for any remaining work. Single-line doc fixes go through a PR. If you catch yourself typing `git push origin master`, STOP. This rule is non-negotiable — dayshift-14 pushed 40+ unreviewed commits to master.

## Step 5: Review loop

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

## Step 6: End shift

**Only when the shift clock runs out** (not when work feels "done"). Keep working until end of shift. Then, only after the reviewer approves (no new issues in the latest review round):

1. **Write handover** — create `shifts/YYYY-MM-DD-{shift-name}.md` with:
   - Date, **actual** start and end times (not the planned window — record when you started and when you're writing the handover), PR number
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
- `ls shifts/*.md | sort -r | head -1` — newest handover = last completed shift
- If no open PRs → start a new shift
