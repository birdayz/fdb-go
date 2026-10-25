# Validation lane helpers

| Script | Use |
|---|---|
| `run-verified-lane.sh <worktree> fast\|race\|full [args]` | Runs `just test` (`fast`), `just test-race` (`race`: the fast-lane targets under `-race` on the race output base) or `just test-full` (`full`) on a clean worktree. Records HEAD, `git status` and a sha256 manifest of every tracked and untracked input before and after, and the recipe with its expanded command; exits 4 if any input changed during the run. Then runs `bazelisk shutdown` for the lane's output base and records its exit; the script's exit status stays the lane's. Evidence goes to a fresh directory under `${TMPDIR:-/tmp}`, printed first. `FDB_LANE_LOCK` (default `${TMPDIR:-/tmp}/fdb-heavy-lane.lock`) serializes heavy lanes on a shared machine; only the script holds it (children run with the descriptor closed), so a killed lane frees it even if its Bazel server lives on. |
| `tree-hash.sh` | Prints a 16-hex-digit digest over the tracked and untracked, non-ignored files of the current worktree; compare it before and after a run to show the inputs did not move. |
