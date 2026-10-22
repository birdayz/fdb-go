---
name: pr-review
description: The final PR review gate for this repository, a fresh read-only Claude Opus 5.5 reviewer subagent that replaces the retired `@claude` GitHub Action. Use whenever a workflow asks for the PR reviewer's LGTM or ACK, before calling a PR merge-ready, and after every push that makes the previous verdict stale. Do not tag `@claude` on PRs.
---

# PR review gate (Opus 5.5 reviewer)

The `@claude` GitHub Action no longer completes a review: every run since 2026-10-08 failed
within seconds with an empty error result. Do not tag `@claude`. The final review is a
subagent you start yourself, after the domain reviewers (Graefe or the FDB C++ developer,
Torvalds) and codex.

## 1. Start the reviewer

- **Model:** Claude Opus 5.5 (`claude-opus-5-5`), effort `xhigh`.
- **Fresh agent per PR.** It must not inherit your context or your conclusions.
- **Read-only tools:** shell and file reads. It does not edit, commit, push or comment on
  the PR; you do.
- Run it in the background and keep working. Its reply arrives when it finishes.

Its prompt carries everything it needs, because it knows nothing about the work:

- the PR number and the exact HEAD sha under review;
- how to read the diff without trusting your checkout: `gh pr diff <PR#>` or
  `git diff $(git merge-base origin/master <sha>) <sha>`, and `gh pr view <PR#>`;
- the TODO item or RFC the PR closes, and what the change claims to do;
- the spec for the domain: Java Record Layer 4.14.2.0 (`fdb-record-layer/`) for record-layer
  and SQL behavior, FoundationDB C++ 7.3.77 (`/tmp/fdbsrc`) for `pkg/fdbgo`; every
  behavioral claim cites `file:line`;
- which reviewers already passed it, so it looks for what they could miss;
- the verdict format: ACK or NAK, findings ranked by severity with `file:line`, the sha it
  reviewed, and a word limit.

Prompt skeleton:

```
You are the final PR reviewer for https://github.com/birdayz/fdb-go/pull/<PR#> in
<repo path>. Review only: do not edit files, commit, push or comment on the PR, and do not
run `go test`. Review exactly HEAD <sha>: `git diff $(git merge-base origin/master <sha>) <sha>`
(do not rely on the local checkout). Description: `gh pr view <PR#>`.
The change closes <item>: <two or three sentences on the problem and the fix>.
The spec is <Java 4.14.2.0 at fdb-record-layer/ | FoundationDB C++ 7.3.77 at /tmp/fdbsrc>;
cite file:line for every behavioral claim. Already reviewed: <reviewers and their verdicts>.
Check correctness against the spec, callers the diff does not show, test determinism and
vacuity (would each new test fail on the old code?), stale comments and docs.
Reply ACK or NAK, findings ranked by severity with file:line, under 400 words, naming the
sha you reviewed.
```

## 2. Iterate

- Fix each valid finding, with a regression for any bug the suite missed. Commit and push.
  Explain to the reviewer why you are not acting on a finding you think is wrong.
- Send the delta to the **same** reviewer, which keeps its context: the new sha and
  `git diff <old-sha> <new-sha>`, plus which findings were addressed and which were left.
- Every push makes the previous verdict stale. Only an ACK that names the current HEAD
  counts.

## 3. Record the verdict

- When the reviewer ACKs the current HEAD and CI is green on it, post the verdict on the PR:
  `PR review gate (Opus 5.5 reviewer): ACK on <full sha>`, a short summary of what it
  checked, and any findings accepted as non-blocking with the reason.
- That comment is the last on the PR. Post nothing after it; if you must, run the gate again.
- Never post an ACK the reviewer did not give, or one for a sha it did not review. A NAK
  blocks the merge until a later ACK on a newer HEAD.
