#!/usr/bin/env bash
# Blocks tree-destructive git operations that agents keep reaching for despite
# instructions. `git stash` is the specific one: a stash in a shared working
# tree silently swallows whatever OTHER concurrent agents have uncommitted,
# and recovering it depends on noticing the loss at all. Mutation checks
# should save a diff and use `git apply -R` / `git apply` instead, which
# touches only the files it was given.
#
# Reads the PreToolUse JSON payload on stdin; exit 2 blocks and feeds stderr
# back to the agent as the reason.
payload=$(cat)
cmd=$(printf '%s' "$payload" | jq -r '.tool_input.command // ""')

if printf '%s' "$cmd" | grep -qE '(^|[;&|[:space:]])git[[:space:]]+stash([[:space:]]|$)'; then
  echo "BLOCKED: 'git stash' is forbidden in this repo. The working tree is shared with other concurrent agents and a stash silently swallows their uncommitted work. To toggle a change for a mutation check: save it with 'git diff > /tmp/x.diff', revert with 'git apply -R /tmp/x.diff', restore with 'git apply /tmp/x.diff'." >&2
  exit 2
fi
exit 0
