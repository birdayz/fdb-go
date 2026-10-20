#!/bin/sh
# sha256 over tracked+untracked (non-ignored) files of the cwd worktree
{ git ls-files -z; git ls-files -z --others --exclude-standard; } | sort -zu | xargs -0 sha256sum 2>/dev/null | sha256sum | cut -c1-16
