# WS-D design gate v4

TREE `4b8b8c15a3b65ce74c1856f8175ff2d3e7883c8d`; design SHA256 `81fdd5f9c7a4d15222c0ad47c7056494f108e7e6bc3f689b788adbe2126809a8`. Answers the four v3 NAKs. No production
source delta since the accepted WS-C tree. Reviewers run as `claude -p --model
opus --effort xhigh` with `claude-reviewer-settings.json` (owner-authorized
substitution), sequentially, retried on the Claude session limit.

## Result: four NAKs (all runs complete)

graefe, torvalds, storage, spfresh each completed with NAK. Main findings: the
admission matrix used Java's mutable set wrongly and refused deletes Java performs;
admission cannot run before Go's record write (needs a commit-blocking backstop);
Go's vector Update lacks Java's common-entry filtering; refused queue entries
retried forever; the iterator's own limit schedule must stay; build lease owner
must be the session id; progress must be measured on the blocked cluster; inline
insert tasks consume merge knobs; unsplittable clusters grow unboundedly in inline
mode; kept empty merge cluster never re-merged; executor changes reach SPFresh;
join rule, lock order, pass-2 stats. Git reads were partly denied by the reviewer
policy (compound commands); v5 widens it. Addressed by design v5 plus the
measured live-JVM oracle (`../ws-d-oracle/`).
