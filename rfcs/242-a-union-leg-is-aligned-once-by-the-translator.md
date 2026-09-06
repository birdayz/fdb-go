# RFC-242 — A union's legs are aligned once, by the translator

**Status:** r72. r71: **Graefe ACK**, **Torvalds NAK**, **codex** — and both code findings were in the fix written the round before. The re-issue added at r71 collapsed "daemon unreachable" into "not present", so an empty answer followed by the daemon going down inside the confirmation window reported a REMOVAL; and `misses` had become dead when the budget went from polls to seconds, leaving both skip branches unreachable and a copy failing during an outage logging `(inspect still succeeds)`. Both fixed and driven — `still_there` is now extracted and run against a scripted `docker`, because the failing case is a race between two calls that no phase fixture can reach. Suite at 50 arms. r70: **Graefe ACK-conditional**, **Torvalds ACK-conditional**, **codex** — and this round the findings were in the RECORD, not the watcher. A blanket `sed` used every round to keep the status line current had been rewriting every HISTORICAL measurement to the current arm count, and the TODO entry booking the hygiene-gate gap was wrong in its population, its pattern and its conclusion (429 files and 48 violations, not 29 and 0 — so not a free ratchet). Both restored and corrected. Code: `maxOutagePolls` was the wrong unit (20 polls = 20min/10min/40s across three callers), and when the backstop tripped the main loop still wrote `GONE (removed)`. Suite at 45 arms. r69: **Graefe ACK**, **Torvalds ACK-conditional**, **codex** — and the fix got SMALLER: `docker ps -aq --filter id=` separates a removed container (rc 0, EMPTY) from an unreachable daemon (rc 1), which `docker inspect` collapses. That collapse is WHY a tolerance count existed; with the right test the arbitrary `3` is deleted, a removal ends the poller immediately, and only an outage is tolerated. Also: a THIRD `inspect` caller was still unconverted, the blip knob separated 2 ways over 3 callers, the df sampler had no observable at all, an arm asserted a MESSAGE and reported it as TERMINATION, and the alarm accepted evidence written by the wrong author. Suite at 43 arms. r68: **Torvalds ACK-conditional**, **codex**, and Graefe reopening my own answer — all three found that a failed `docker inspect` is NOT a removed container, so one transient blip ended trace capture permanently and silently. Both pollers share a bounded-retry helper now, driven from both directions. Also: `exited` was reset per SELECTION not per container, so a re-selected container logged its exit twice and then reported its captured trace as NOT copied; and the test stub had been reading the wrong `docker inspect` argument since it was written. Suite at 41 arms. r67: **Graefe ACK**, **Torvalds ACK-conditional**, plus codex. The fifth hole had THREE consequences and r67 fixed one; codex found a FOURTH (the staging path was still shared). What ships is a guard at the launch site so a container's background jobs start at most once, which closes all four at the cause. The arm asserted TWO launch ids — the mitigation — and now asserts ONE, the guard. Suite at 36 arms. **Graefe ACK** on `b72ac247b`. r66-r67: the one not-covered entry r66 ADDED is driven now instead of defended — but the first attempt shipped an arm that CANNOT FAIL (two copiers only collide if their counters cross, and equal intervals make that impossible), and the same proved true of the periodic path's nesting arm. Both deleted; the list now names which half is driven and why the EXIT path differs. Suite at 33 arms. r65 (head `0b464be55`, CI 7/7 green): **Graefe ACK-conditional, Torvalds ACK-conditional**, and this round the findings are about the NOT-COVERED LIST — two entries claimed something undrivable and both reviewers built the arm that drives it. Also a FIFTH hole in the publish: the generation counter was per-subshell-LAUNCH, not per container, and two copiers can run for one container. Launch and cycle counters are composed now rather than one traded for the other. Suite at 33 arms. r64: **codex** reached the generation collision independently — a third route to a finding invisible to the arm watching that code — and found one nobody else did: `rm -rf` on a retired generation is interruptible, and a deletion cut in half leaves a directory that still matches `fdb-logs-*` and is listed and uploaded as evidence (measured: 6170 of 8000 files left). Generations are retired by an atomic RENAME first, then deleted. Suite at 30 arms. r63 (head `df8a24834`, CI 7/7 green): **Graefe ACK-conditional, Torvalds ACK-conditional**, both having re-driven every mutation claim in frozen worktrees. The publish is now a per-cycle COUNTER plus `mv -T` — a timestamp was not unique, and `mv` onto a pre-existing generation NESTS and returns 0, which a generation COUNT cannot see. The prune remembers its predecessor instead of sorting `ls` lexicographically. A SIGTERM mid-copy left staging behind, so one `trap` cleans it and the suite drives that window deterministically instead of hoping to hit it. Suite at 28 arms. r62: **Torvalds ACK** on `791e33408` (NAK on `8dc1aef2a` for the same three defects Graefe found), and **codex** returned two more on the delta — a `cp -a … || true` that publishes a truncated snapshot while deleting the complete staging copy, and staging assertions racing the copier they assert about. Both folded at r63, and the publish is now an atomic rename onto a fresh generation: the third shape tried, after two that each traded the defect for the same defect one step over. Suite at 28 arms, five consecutive clean runs. r61 first head `8dc1aef2a`: **Graefe NAK** on five findings, all folded at r62 — the blocker (a `rm -rf` + `mv` promote window) was already fixed in `791e33408` before he reported, and the other four were prose claiming more than it had: a population no revision ever had, the tag guard credited to the wrong round, an alias number that is not reproducible across runs, and an exit copy still using `mv -f` where that NESTS onto an existing directory and returns 0. CI 7/7 green on `791e33408`. r60 (head `30229785a`): CI 7/7 green. codex reviewed `bb231d345..fa9d74dca` and returned four findings, all real and all folded at r61 — a trace copy that published a directory when the copy FAILED, a missing-trace fallback that could not fire (the r58 defect one loop over), a dynamic-carrier test whose assertions all held under the substitution it was written to catch, and a blanket-skip description that outlived the blanket skip in two files. Driving those turned up three more: the tag-guard coverage — new here, not a r60 regression; r60 has no tag-guard fold — first covered the forensics step and not the watcher step; the `docker` stub slept for `--tail` as well as `-f`, which wedged the dump loop the first time a case left a container running. Suite is at 43 arms. r59 (head `fa9d74dca`): **Torvalds ACK, Graefe ACK**, each with one non-blocking nit and both about the new suite rather than what it tests. Torvalds: the dump extraction anchored on the first ten-space brace in the file, of which there are three — position, in a suite about not depending on position. Anchored on the step name now and proven against a decoy block inserted above it. Graefe: the alarm below the dump was unpinned, and it is the piece that had already shipped wrong once, annotating without exiting non-zero; five arms now, mutation-verified in both directions. He also caught a message assertion that belonged to no case, reading the shared output after the loop where an appended case would retarget it, and that "the suite covers the dump block" covered two of its arms — so the not-covered list is written down. r59. r58 (head `1561152d2`): **Torvalds NAK, Graefe NAK**, both on the same line and both right: r58 shipped a leftover mutation into the nightly — `[ -d "" ] || continue` in the dump's loop over the copied trace directories. Always false, so the loop body never ran, and because `for` still exits 0 the fallback message never fired either: a night with a dead cluster reads exactly like a clean one. It went in inside the commit whose whole point was letting the dump read that directory, and nothing caught it because the new suite covered the watcher heredoc and not the dump block. The suite covers the dump block now — and immediately failed, because the extracted block ends with `} > fdb-forensics.txt` and writes there rather than to stdout, which is the artifact a night is read from and the right thing to assert. r58. r57 (head `2bb33e609`): **Torvalds ACK, Graefe ACK-conditional**, and both conditions were the same one: the trace measurements behind r57 lived in a scratch directory. They are a committed suite now — and it immediately found that it was pinning the wrong thing, since deleting the exit-transition copy reddened NOTHING while the periodic loop ran at a 1s interval and reached the same file. The interval is a parameter now and one case isolates each path; two mutations, two disjoint sets of failures. Graefe also caught the exit copy being described as exclusive when it is merely PROMPT (`docker inspect` succeeds on a stopped container), and Torvalds three more: two copies writing the same paths concurrently, a fail-closed disarm that was silent, and the CLOCK_REALTIME-vs-boottime seam, all folded. r57. r55+r56 (head `605c748b5`): **Graefe ACK, Torvalds NAK.** The NAK is the one that matters and it partly reinstated the bug it was fixing: the sweeper took the running worker's start time from `stat -c %Y /proc/<pid>`, which is when the procfs inode was allocated rather than when the process began — biased late, unboundedly — so the job's own container could read as older than the worker and be swept, which is the original incident. `ps -o etimes=` now, failing CLOSED where the age is unreadable, and the five arms are a committed test rather than a hand-driven one. Graefe ACKed and independently measured two more failure modes in the streamed trace r55 introduced: the glob expands once at exec time, so a late first file is never captured, and a rotated second file is never followed. That whole approach is replaced by copying the trace directory — periodically while the container lives, and once more on the exit transition, which is the window that catches a fatal line written as the container dies PROMPTLY — the periodic copy would reach it too, within a cycle, since `docker inspect` succeeds on a stopped container. r56. r54 (head `bb231d345`) also drew **@claude ACK-conditional**, arriving after r55 was written: it found the same fourth route Graefe had, traced it further to a different FUNCTION than the pins drive, and showed it is production-reachable and untested. r56 pins it, with both preconditions asserted and mutation-verified — the first mutation attempt failed to BUILD and so proved nothing, which is why the build result is read and not only the test result. @claude's other finding, a job cancelled inside the start step's ten-second pid wait, was already closed by r55's widening of the same gate for Graefe's version of the hole: three reviewers, three paths, one predicate. r55. r54 (head `bb231d345`): **Torvalds ACK-conditional, Graefe ACK-conditional, codex three P2s** — and between them they found that the gate tightened in r54 had opened a hole in the other direction that the LOOSER version had covered, that the sweeper guard traded the leak it fixed for a different one, and that the trace sampler could not capture the event it was added for. Each is the same shape as the defect it was fixing. r55 folds all of them: the stop step gates on `!= 'skipped'` (exact, not lucky — that is the only outcome meaning no line of the start step ran, and the watcher is detached before anything else can fail); the sweeper compares container start times against the running worker's, so a previous job's orphan stays eligible while the live container does not; and the trace is streamed rather than sampled. Two conditionals were sweeps I had left half-done, and the route list was short for the third lap running, which is the argument for the invariant that replaced it. r54. r53 (head `7987d5b1a`): **Torvalds ACK, Graefe ACK**, both with the same non-blocking residual, and it is the one this session had already found by walking the step's paths rather than reasoning about them: a plain `if:` implies `success()`, so on a Checkout failure with the window open the watcher's start step is skipped while an `always() && window.ok` stop step still runs, over a pid file `git clean` never removed. Three independent derivations of one hole. The gate is the start step's own outcome now, with a `/proc/<pid>/cmdline` ownership check behind it, and `pkg/docscheck` pins all of it so the next weakening fails the build rather than a future night. Torvalds also found the carrier enumeration short by one AGAIN — a third route with four production call sites and zero tests — so the RFC states the invariant those routes share instead of counting them, and the route has two mutation-verified pins. r53. r52 (head `405ef67a6`): **Torvalds NAK, Graefe ACK conditional**, and both landed on the same thing: r52's stale-pid fix was in the step that STARTS the watcher, which is gated on the window being open — so it never runs on the closed-window path that makes the hazard reachable, and on the open path `actions/checkout`'s default `git clean -ffdx` has already removed the file. A fix that executes only where it is unnecessary. The real fix is one line on the STOP step. Graefe also caught a byte count that went stale INSIDE the round that wrote it — the guard left 13 bytes of `user_data` headroom, a later trim in the same round moved it to 30, and both prose copies kept saying 13 along with an instruction derived from it that was no longer true. They point at the test that prints it now. Everything else in r52 was verified rather than taken: Torvalds re-ran the child-cleanup harness per-pid and confirmed `kill -- -$$` covers every path out of that loop (no `break`, no `trap`, no `set -e`, deadline expiry the only exit), confirmed 290 minutes against the 300-minute cap, confirmed the tag now derives from `.bazelrc` in both live uses, and confirmed the accessor chain closes. r52. r51 (head `039e03ef7`): **Graefe ACK with two nits, Torvalds NAK on three**, and every one of them was in the fix rather than in what it fixed. The watcher's deadline bounded the parent and leaked its children; the deadline itself was ten minutes longer than the job cap it claimed to be inside; the stop step would have signalled a recycled process group on the skipped-window path, because that path skips `Checkout` and leaves last night's pid file in the workspace. Graefe caught that "one copy of the tag" was one copy short — the forensics loop below it still hardcoded `:7.3.77`, a stale ancestor filter emptying a loop, which is the failure that step exists to report — and that the accessor claim over-reached by one hop. r52 folds all five, each re-measured here rather than taken. It also settles a guard that nearly shipped matching argv: `pgrep -f` reported a runner present on a box that has none, because another agent's `grep` was in its argv, and the correct signal is `-x` on the apphost this fleet's tarball actually ships. r51. r50 (head `b91c5c86c`): **Graefe ACK, Torvalds NAK, codex P1 + four P2s + a P3**, and the P1 is the largest finding of the whole pull request. Chasing a booked nightly failure into `infra/`, codex found that `orphan-fdb-sweep.sh` — this repository's own cloud-init, on every provisioned box — force-removes any FoundationDB container older than 1800s every five minutes, on the premise that no per-test container lives that long. Three nightly lanes hold one for hours. That is the 30–35 minute band those lanes have died in every night since the pool was provisioned, it explains every observation two carefully-refuted memory hypotheses could not, and it converts a STOP that said nothing in this repository could change it into a guard and a deployment step. Torvalds' NAK is three mechanical defects in the watcher r50 shipped — a leaked poller on a persistent box, an alarm blind to the paging lane, and a per-container file that was not — each of which is the same class the watcher was written to fix. r51 folds all of it, plus a P2 that retracts r50's coverage-nightly analysis outright: it traced the kills to `Scaler.shutdown()`, which fits the signature exactly and is undeployed code. CI on r50's head was 7/7 green. r50. r49 (head `725164d72`): **Graefe ACK, Torvalds ACK, codex no findings, @claude NAK** — and the NAK is on the paragraph r49 wrote to fix a scope claim, which had itself become one. It said TWO kinds of positional pointer are dangerous and named the two it had in hand; asking the question of every row finds a third, and asking once more finds a fourth. r50 replaces the taxonomy with the question that generates it and says outright that the examples are a sample. CI on that head is 7/7 green. Both other reviewers ACKed and then found the same thing, which is the one worth leading with: r49's paragraph about not trusting an unscoped count had itself published a count it never took, attributed to another gate. r50 deletes it rather than correcting it and says why. Four more folds and one sharpening, all listed below; the sharpening is Graefe's and it strengthens the RFC's own claim — the fuzz fixture only builds the union when the legs' types are ALREADY `Equals`, so the second alignment is not re-deriving a property, it is manufacturing a disagreement that did not exist. r50 also re-measured the nightly bookings this pull request's report will quote rather than repeating them, and two did not survive: the Coverage lane is not the container-death class (six for six, "the runner has received a shutdown signal", and runner lifetime is OUR code), and the "one repository-editable cause" of Reconcile's red is one short (#745 is DIRTY). The rowdiff forensics step that was supposed to settle the container deaths has now run once and settled nothing, silently — measured, and fixed here with a watcher that takes the evidence while the container is alive. r49. r48 (head `13ba74d63`): **Graefe ACK, Torvalds ACK, @claude ACK, codex no findings** — all four gates, for the first time on this sentence. Three of them verified it by sweeping the comment themselves rather than reading the prose, which is the right instinct: this exact sentence had been wrong three rounds running. r49 folds the three non-blocking things they left. (a) Torvalds and Graefe both flagged the same one: the fold sections had written the pointer count into prose in three places, an unscoped number over a file that grows a row at a time. It now appears once, as a command, with the region it was taken over and both heads it was taken at — and the note beside it records that a line count and an occurrence count happen to agree here, so the agreement is not read as validation of the method. (b) Graefe's second: the comment said THREE attempts inside the paragraph that deliberately refuses to count; it says "every earlier attempt" now, which a fourth cannot make stale. (c) @claude's: the pointers are not all equally dangerous — a comparative or forward-asserting one flips from true to false on a retarget with nothing inconsistent left on the page, while a pointer at evidence merely goes unhelpful. The comment says which is which and where to look first. r48 also carried its own measurement forward: Graefe re-confirmed against `cascades_translator.go` that nothing in the delta moves planner or translator semantics, and the test file has zero changed non-comment lines. r49 adds one thing that is not a fold: a second fuzz corpus entry, `FuzzPlanner_PlanFullPipeline/05cb6c3c202ef15e`, which is the shape the engine fuzz nightly is red on TODAY. It fails at the merge-base with the union leg-type disagreement this RFC removes and passes here — the § Test plan item 1 has the two measurements. r48. r47 (head `e1b4ed839`): Graefe NAK, Torvalds NAK, codex P2 — all three on the SAME sentence, for the THIRD round running, and each time the sentence was narrower than before and still wider than the file. r47 scoped it to "nothing here counts the table's ROWS or points into it by position". The row prose points by position in several places (counted, with the command that counts them, in the r48 fold), and one of those is fourteen words after a pointer the same delta had just removed from the same string. Torvalds also found "read the next two rows", which breaks both halves of the disclaimer in one clause. r48 stops trying to make the file satisfy the sentence and bounds the sentence to the comment: it says what the rows show rather than where they sit, and it says outright that the row prose below DOES cross-reference by position, why that is left, and that it is a real hazard an insertion can retarget. Everything else in r47 was verified and cleared by both: the singular attribution swept to one hit quoting itself as history with a positive control, the round count dropped rather than incremented, the counterpart reference made bidirectional, and the pinned list of ten still matching the tree in order. r47. r46 (head `1935caf8b`): Graefe ACK conditional on one fold, Torvalds NAK on that fold plus one more, codex a P2 and a P3 — and all four findings are the same two things. Graefe cleared his NAK: the attribution now reads identically in all three homes, and he checked BOTH halves' reasons against `cascades_translator.go` rather than taking them on report, confirming the both-anonymous row never reaches the gate (every leg equals the common row, so leg normalisation is not entered) while the promote row does reach it and passes through the anonymous slot. What all four then flagged is r46's own parenthetical: it claimed the comment quotes "no counts, of rows or of anything else" four words after "FOUR successive rounds", and the RFC repeated that over-claim twice. A scope sentence written by describing the intent rather than by probing what it covers — the failure this RFC has a standing rule about — and self-disarming, because it tells the next reader there is nothing to update. r47 scopes it to what is true and load-bearing, and takes codex's better fix for the count itself: the opening is UNNUMBERED now rather than incremented, so a fifth refutation cannot make it stale. Torvalds also found the superseded singular attribution still live in the booking twenty-four lines above the corrected list, and codex two more copies in this RFC. All swept, with a positive control. r46. r45 (head `8398e7b74`): Graefe NAK, Torvalds NAK, codex a P2 — all three on the same class, and all three confirming the substance is verified and stands. The three edits r45 made all landed and were checked independently: the pinned list matches the tree row for row in tree order, and the CTE narrowing checks out against `recursiveCTECommonResultRow`'s per-ordinal maximum. What remains is the class this span keeps failing at. Graefe and codex found the refuting-row attribution split three ways: the table's own row says the BOTH-ANONYMOUS row kills the target-based title on its own, while the booking and an RFC fold credited the PROMOTE row on its own. codex supplied the fact that settles it — equal leg types skip leg normalisation and never reach the gate, so the promote row is the one that proves the gate is exercised while the both-anonymous row is the one that proves an anonymous target can answer. They refute TOGETHER, and r46 says so in all three homes. Torvalds found two more numbers in the same comment r45 was editing: it said THREE rounds named a mechanism and then enumerated a fourth, and it said the table includes TWO shapes that fail where twelve rows fail under five distinct refusals. Both are gone, and the round count with them: the opening is unnumbered now, because incrementing it would have re-armed the same staleness at the next refutation. r45. r44 (head `47d1b0dd7`): Torvalds ACK conditional on two edits, Graefe NAK on one of the same two. Both verified the substance independently rather than reading the delta's claims: they counted the table from 19 rows to 24, checked the previously-unused constant is now referenced, and each MUTATED the recursive-CTE row's error constant to the UNION spelling and watched it redden with the engine's real `0AF00` text — which also proves the table executes against a live store rather than skipping. What both found is a fresh miscount in the sentence this whole span exists to make countable: the booking said the union defect is pinned by NINE rows and there are TEN, because the enumeration folded the promote row into the both-anonymous row's parenthetical. Torvalds separately found the test's head comment still pointing at "the last four rows below" after five more were appended — a positional pointer into a table that has grown every round. Graefe also narrowed the CTE claim: naming that second call site guards against a call-site-local patch, but it is not an argument that the closure needs two fixes, since the prescribed direction covers both callers by construction. r45 makes those three edits and stops quoting a positional count anywhere in the table's own comment. r44. r43 (head `ee9783273`): Graefe NAK and Torvalds NAK, on the same thing, and it is the worst process failure of this span: r43's headline retitling rested on three rows it said it had committed and had NOT. The substitution that would have added them reported one hunk where two were wanted, and that report was read as the wrong hunk having failed. The receipt was sitting in the file — `nestedAnonTarget` declared and used nowhere. An unused local const is legal Go, so it compiled, vet was clean, and the table passed because the rows it was supposed to gain did not exist. Both reviewers found it by counting `why:` lines across the two heads and getting the same number. r44 commits them, verified by that same count before and after, and adds two neither had asked for: a THREE-leg union where two legs agree, and the same gate reached through a RECURSIVE CTE — Torvalds found that second call site, which wraps the identical refusal in a different error code, so a closure written against the UNION spelling alone would have left it. Also folded: two row explanations still carried the refuted target-based title, and the sort helper's comment claimed leg order was not asserted while the new per-row comparison had just started asserting it. r43. r42 (head `a9cb30e9f`): Graefe NAK with one required fold, and it is the most useful finding of the whole span because it replaces a guess with a traced cause. r42 titled the union booking correctly — a common type carrying an ANONYMOUS field is unpromotable — and then pointed its direction at the coercion trie, which was the FOURTH wrong mechanism for that site. Graefe read the source: `exactUnionResultRow` builds the common row with `MaximumType`, and `exactUnionSlotValue` gates each leg on `MaximumType(leg, target).Equals(target)` — a predicate that cannot accept its own common row, because `MaximumType`'s record arm resolves a disagreeing name pair by KEEPING the named side. So the gate is built from a function that is not idempotent under name erasure, and one anonymous field is enough because `RecordType.Equals` is all-or-nothing. Both verified here against `cascades_translator.go` and `values/type.go`. He also settled the fork the booking had left open: Java's `PromoteValue.isPromotionNeeded` recurses records POSITIONALLY over `getElementTypes()` and never reads a field name, so Java ACCEPTS this union and Go's refusal is a conformance divergence, not only a gap — verified against the Java source here. The direction changes with it: not the trie, which belongs to the other two bookings, but Java's positional promotability shape. r43 rewrites the booking around that. It also folds codex's r41 remainder: the union rows asserted only the error family, so a run where alignment picked a NAMED common type would still have passed, and the three walker guards were undriven — one is now extracted so it can be, and all three have unit pins that run without Docker. r42. r41 (head `57c83b567`): Graefe NAK and Torvalds NAK. r41's own corrections were over-broad in two places, and both were caught by varying something no round had varied. Torvalds varied FIELD COUNT: a CASE branch with TWO fields does coerce a record, and the disagreeing field comes back `_1` — so "no record survives as a record to coerce" is true only of the one-field spelling, and r41 had deleted a correct lead ("something on that path does what the array path does not") on the strength of it. He then varied WIDTH in the union: agreeing names with differing widths ANSWER, both legs, and a two-field union with only ONE field anonymised is still refused. So the union booking's "no record coercion on the Go promote" was wrong too: what that site refuses is a common type carrying an ANONYMOUS FIELD, and one is enough. Graefe found the withdrawn CASE claim still standing in the table's own `why` strings — the text printed when the row reddens, handed to whoever lands the fix — and that the multi-row union assertion walked only the first row. r42 adds the three measured rows, restates both sites, restores the CASE lead correctly scoped, walks every returned row (sorted as PAIRS, since sorting names and values independently would assert a pairing no query produced), and narrows the union booking to the anonymous field. r41. r40 (head `8e88873f4`): Graefe NAK and Torvalds NAK, and the four-site framing r40 introduced is itself refuted — by the controls neither r40 nor any earlier round had run. Torvalds varied the one dimension all the rows still held fixed, the BAD NAME itself, and two of the three non-array sites turn out not to depend on it. `SELECT (1 AS A) AS C FROM t UNION ALL SELECT (2 AS B) AS C FROM t` — two perfectly synthesisable names — fails with the SAME 42F65, and the same union with agreeing names answers. So the union site is not an outcome of "a record literal protobuf cannot name" at all: it refuses to promote a record to the anonymised record unification produces, full stop, and that is a legal SQL union Go will not run. Booked separately at r41. The CASE row was worse than wrong, it was vacuous: all four variants return the same thing, and its asserted leaf is a bare number under the outer alias, so no record ever survives as a record there to be coerced — which means r40's "something on that path already coerces, find out what before porting" rested on a row measuring nothing, and it pointed the next engineer AWAY from the element promote, the node Java actually coerces at. Graefe reached the same conclusion independently. r41 adds the controls, restates both sites as measured, splits the union out as its own booking, hardens the leaf walker (it silently dropped non-numeric leaves, and a row declaring no expectation passed vacuously), adds the both-disagree diagonal to the unification pin, and stops quoting a row count that has been wrong in two homes twice running. r40. r39 (head `e7d263cb0`): Graefe NAK and Torvalds NAK, and the account was refuted a THIRD time — by rows that varied the one dimension all nine table rows held fixed. Torvalds dropped the outer record: `SELECT [(1 AS "$lead"), (2 AS A)] FROM t` ANSWERS, with a RAGGED array — one element a message, one a raw map — which is worse than the failure because nothing reports it. He then moved the same two literals to a CASE, where they COERCE and answer cleanly, and to a UNION, where they draw a loud 42F65. Four sites, four outcomes, same literals. Graefe separately measured that the numeric-width refusal is NOT at descriptor synthesis at all: both descriptors synthesise, and the refusal comes from `copyFieldsByNumber`'s kind-mismatch guard at EVALUATION — the "cannot synthesise" prefix is `ProtoTypeError`'s stock wording and it misled the booking. That makes the two bookings ONE site and ONE cause: a stamped parent handed a child the promote never coerced, a map in one and a wrong-kind message in the other. He also showed condition (2) was redundant — a promotion being inserted is entailed by the other two — and required the erasure to be measured rather than hand-built, since the type-level pin asserted an anonymous record it constructed itself. r40 folds all of it: the table grows to cover all four sites, a new Docker-free pin calls the unifier and asserts that a field name is erased when the names disagree and kept when they agree, both bookings are rewritten around the corrected site and cause, and every one of the four outcomes is re-measured at the merge-base, where all four are identical. The CASE row also refutes the booking's own closure sentence — something outside the array path already coerces — and that is now written as an open question to answer before porting. r39. r38 (head `8bd35adb9`): Graefe NAK, Torvalds NAK, codex three P2 — and the mechanism was refuted for the SECOND round running, by rows nobody had run. r38 said the failure needs an unsynthesisable child name AND a type-changing wrapper. Torvalds varied the dimension the pin held fixed and broke it both ways: `[(1 AS "$lead"), (2.5 AS "$lead")]` has both halves and ANSWERS, because the SAME bad name survives into the promotion target so the parent cannot stamp either; and `[(1 AS A), (2.5 AS A)]` has neither half and FAILS, with a different error about int32 against double. So there is a third condition — the target must be SYNTHESISABLE, which unification achieves by anonymising disagreeing fields and thereby erasing the offending name — and a second, separate defect. Both re-measured here and identical at the merge-base. r39 stops asserting a mechanism and commits the MEASUREMENT: a table of texts and outcomes over real SQL, each row asserted, plus a Docker-free three-row pin of the stamping predicate underneath it, which is what Graefe asked for and what makes the outcomes explicable rather than merely recorded. The numeric-width failure is booked separately. codex's three are folded into the table's shape: the neither-factor cell r38 deleted is back, the struct rows assert `api.Struct` rather than not-a-map, and the map rows compare numerically so a value arriving as a STRING cannot pass as "values intact". r38. r37 (head `f075141af`): Graefe NAK, Torvalds NAK, codex four findings — and between them they REFUTED the mechanism r36 and r37 had been writing up. r36 said a type-changing wrapper alone leaves a stamped parent over an unstamped child. It does not. Torvalds measured `[(1 AS B), (2 AS A)]` — two DIFFERENT record shapes, so the promotion is still inserted — and it ANSWERS. Graefe measured the type level and named the missing half: the child's own type must be UNSYNTHESISABLE, which `$lead` is because a protobuf field name cannot start with `$` (Java's own `ProtoUtils` rule, correctly ported). Re-measured here as a full 2x2 over real SQL: both halves fail; wrapper alone answers as a struct; bad name alone answers as a raw map with values intact, which is the ordinary documented cost. So the defect is a CONJUNCTION and each half alone is harmless. Every home is restated, and the pin now asserts all three cells rather than one control that varied two factors — which is what both reviewers and codex flagged, and the same defect this PR spent five rounds closing on the duplicate-name pair. Torvalds also found a THIRD live home of the refuted pre-order reasoning, in a file no round had named, and that the booking's published grep was BRE so its `|` was literal and it returned nothing as written. codex found the adjacency arm proves its claim only for a FIRST-field child, since a later field has preceding subtrees between it and the parent; the claim is narrowed to what the fixture decides and the gap named. Graefe added the two Java details that decide where the port lands: the promote is injected PER ELEMENT so the target is the element record, and `MessageHelpers` casts to `Message`, so the port unit is the trie AND the registration model. r37. r36 (head `b0270554f`): Graefe ACK with one wording fold, Torvalds NAK with two blockers, both right. First, the retraction of the refuted unreachability claim was itself incomplete: two present-tense assertions survived in this RFC's own fold sections, one of them forty lines above the bullet retracting it — the third time this PR has shipped a sweep that fixed the copies its author remembered, on the round whose headline lesson is that failure. Both are corrected in place. Second, the new reproducer's control varied TWO things: it dropped from two array elements to one, so element count moved along with the shape difference, leaving "two-element record arrays fail" unexcluded. Torvalds measured the one-variable control — two elements of the SAME shape, which needs no promotion — and it answers; r37 commits that one. Graefe's fold: the bake pin called its two arms "the states the bake can be in", which over-enumerates, since the reverse pairing is excluded only by the repository's sticky poisoning that nothing asserts; it now says the states this fixture reaches and names the harmful ordering as what is excluded. Graefe also read Java and settled the fork the booking left open, which r37 folds in: Java builds the target's message AT EVALUATION from a coercion trie carried on the PromoteValue, and never stamps the wrapped child with the target's descriptor. r36. r35 (head `7902aff42`): Graefe NAK, Torvalds NAK, and a codex P2 that refutes the round's headline conclusion with a working SQL reproducer — the most valuable finding of the PR and the one no gate had reached before. r35 concluded that a stamped parent over an unstamped child is UNREACHABLE, from two facts about a parent and its direct field child. codex supplied the hole: a type-changing WRAPPER between them. `SELECT ([(1 AS "$lead"), (2 AS A)] AS CH) FROM t` over a non-empty table makes array unification promote two differently-shaped elements to a common anonymous target, so the parent's type carries the TARGET's shape and the constructor underneath is never registered. Measured: seven constructors, three unstamped, two stamped-over-unstamped pairs, and the query fails outright with `cannot store map[string]interface {} in message field`. Measured again at the merge-base `36b97f1e9`, where it fails identically — so it is PRE-EXISTING and not a regression of this work, which is the only reason it is booked rather than fixed here. r36 retracts the unreachability claim in every home, narrows the bake pin to the direct-child case it actually proves, and commits the reproducer with a one-element control that needs no promotion and answers. Graefe NAK and Torvalds NAK. Both accept the production change — collapsing the bake into the exported visitor is behaviour-preserving, is Java's shape (`UsedTypesProperty.evaluate` consumed by `QueryPlan`), and the four prune arms were mutation-verified independently by both. The blocking finding is Torvalds's, and it is the sharpest of this whole PR: the unreachability pin r35 shipped was VACUOUS in the only arm that could exercise it. Over the poisoned repository parent and child are both unstamped, so the implication it asserts has a false antecedent and nothing checked that. He then made `WalkValue` POST-ORDER — a direct violation of the test's own stated load-bearing fact — and the test stayed GREEN, while `TODO.md` and this RFC said CLOSED on its strength. r36 asserts each fact directly instead of reasoning from it: containment by looking the child's type up in the repository that just synthesised its parent and requiring the parent's OWN field message back, and adjacency on the visit ORDER itself; the two reachable states are then asserted positively, both stamped or both unstamped, rather than as the absence of the mixed pair. The post-order mutation now reddens the adjacency arm. Graefe's two required folds are the same refactor's other debris: the deleted plan walk's doc comment was left heading `forEachNodeLocalValue`, and the deleted value walk's was left heading `stampRecordConstructor`, which walks nothing — carrying with it the load-bearing "the walk continues THROUGH a stamped constructor" property, now moved to the function that implements it. r35 fixed the two stale doc verbs it remembered and orphaned two more in the same edit. r35. r34 (head `0d0b5529a`): Graefe NAK, Torvalds NAK and a codex P2, all three on the SAME defect, in the function r34 exported to prevent exactly it. `ForEachPlanRecordConstructor` promised the bake's population and omitted the bake's own prune: `stampPlanNode` stops at `feedsAWrite` before a node's values AND before its children, and the exported walk descended anyway, so it reported constructors `FinalizePlan` never stamps. Two reviewers measured the same divergence with a probe — one constructor visited under an INSERT root, zero stamped. It fails in the INFLATING direction, which is the one that reads like a finding. r35 removes the possibility rather than the symptom: `FinalizePlan` now IS that walk, calling it and stamping what it is handed, so the two are the same code and not two things kept in agreement; the prune moves into the shared walk and is pinned in both directions, measured red by deleting it. Graefe also foreclosed r34's own reachability argument: there is no "poisoning in between" a parent and its child, because `WalkValue` visits them contiguously and the parent's descriptor already contains the child's. r35 states the pair as UNREACHABLE through the bake with that as a pinned property over a clean and a poisoned repository, rather than as an open question with no search behind it — which is what Torvalds asked for when he said a negative needs a measurement. His three smaller ones are taken: the no-number population was named as two and is at least four, so r35 stops counting it; two helper docs still said "stamps" where they now emit; and the mixed-pair pin matched `cannot store`, which the scalar arm emits too, so it now matches the message-field refusal it names. r34. r33 (head `3f99b183c`): Graefe NAK, Torvalds NAK, codex three findings, and between them one real defect in the instrument r33 added plus three miscounts of exactly the kind r33 exists to stop. The defect (codex): the exact-shape guard counted constructors over a traversal narrower than the stamper's — result values and child edges only, where `FinalizePlan` also reaches projections, predicates, grouping keys, scan comparands, defaults and structural plan fields — so the guard could fail OPEN on the very population it expires. r34 exports the stamper's own traversal and the census counts over it, which keeps the two in step by construction; the measurement is unchanged at four and three under the wider walk, so the six sentences were true and are now checked against the right population. Torvalds: the comment justifying the child's stampedness argued the PARENT's, and a stamped parent over an unstamped child is a real shape that does not degrade — the map is refused and the query FAILS. That is now pinned by a committed test and booked, with SQL reachability stated as open rather than assumed either way. codex again: "their rows are maps" is too broad, since the join and flat-map constructors never run Evaluate at all. Graefe and codex both: the guard's own comment said FIVE places where its fatal names six. Graefe: r33 labelled the five-file list "the control claim" after its own edit removed that claim from one of them. r33. r32 (head `ab27c401b`): codex ACK with no findings, Graefe NAK and Torvalds NAK, on two different findings, both real. Torvalds: the shared constant's doc described a computed STRUCT coming back as a raw map and a stored struct column surviving "through the same plan" — and that plan selects `a.id, c.id, d.foo` and has NO struct column at all. The struct cost is measured by three other texts in the same test, which are not shared. The doc also said the census is "the value read's precondition", where the census pin says in as many words that it asserts nothing about the struct texts. Two enumerated homes contradicted each other and the wrong one was the file a reader opens first. Graefe: r32's exemption sentence — `plan_finalize.go` "carries no claim to keep in step" — is false. It carries no CONTROL claim and states the CENSUS claim in full, so one enumerated list was serving two claims with different populations, and the list told a future sweeper of "three of four" not to check the one file that states it. Torvalds also measured the census asserting floors (`constructors >= 4`, `stamped > 0`) while six files quote "three of four" as a fact no gate pins. r33 attaches each claim to the query that exhibits it, enumerates the two populations separately, and turns the measurement into an expiry condition: the census now asserts the exact shape and its failure names all six files to re-measure. A seventh unscoped absolute, found by this round's own sweep rather than by a reviewer, is scoped in `record_constructor_message.go`. r32. r31 (head `36a249a61`): codex ACK with no findings, Graefe NAK and Torvalds NAK on the same sentence — in the file r31 itself added. Moving the shared query text into `queryfixtures.go` closed a real carry-forward, and its doc comment then restated the mechanism with both scopes dropped: "the plan's record constructors are left unstamped", where the measured census is THREE of FOUR with a stamped survivor the importing test asserts, and where the damage is bounded by walk order rather than covering the plan. That is the inverted absolute r22 was NAKed for and r23 closed by stating the population in every home, re-armed in the round whose own root cause is that a sweep is only as good as the population it enumerates — and re-armed in the one file a reader from either package now reaches first. r31 also ADDED a home without re-counting: the population it corrected from three to four became five in the same commit. r32 scopes both halves of that sentence, ENUMERATES the five homes rather than counting them, and gives the grep that reproduces the list. Graefe's two non-blocking folds are taken: the prose the shared constant replaced was stacked above it instead of deleted, and the r30 fold section was left uncorrected where r29's twin had been corrected in place — both now follow the correct-in-place convention. @claude ACK on the same head, with the same hardening Torvalds filed as a nit: the shared library was not marked `testonly`, so nothing but a directory name stopped a production target depending on it. r32 marks it, measured by pointing a production `go_library` at it and reading Bazel's refusal. r31. r30 (head `3bfc8ec37`): codex ACK with no findings, Graefe ACK, Torvalds NAK — and Graefe's one non-blocking fold is Torvalds's blocking one, measured independently. Both found a FOURTH home the sweep never counted: `plan_logging_test.go`, the census pin, which names the FDB read as its precondition and is named back by it, describing the same control as "an `api.Struct` with the repeated name removed" with no wrapper anywhere near it. The root cause is the population, not the copies — every count in r29 and r30 said THREE homes and there are four — so r31 corrects the number wherever it is asserted, not only the sentences. Torvalds also measured a third-direction overstatement in r30's own fold: it said BOTH computed reads now assert their size, and there are THREE. The control read, the arm the attribution rests on POSITIVELY, still looped two `AttributeByName` calls and passed for a struct carrying a third attribute — codex's r30 P2 shape, left standing on the one arm that makes the positive claim. It now asserts `AttributeCount`, measured red. r31 also closes Graefe's carry-forward: the shared query text was duplicated by hand across two packages with a comment asking the next editor to keep them identical, and is now one constant both read, proven consumed on both sides by renaming it and reading the two undefined-symbol build failures. r30. r29 (head `320947d6d`): Graefe ACK, @claude ACK, Torvalds NAK, codex a P2 and a P3 — and all four converge on one wording error plus one sweep failure. The wording: r29 said the SET of three reads varies one thing. It varies TWO, deliberately; it is each adjacent PAIR that isolates one, and the extra variation is what proves the wrapper inert. Graefe filed it as a nit, codex as a P3, Torvalds as a required fold. The sweep: Torvalds found the retired claim still standing twice in `TODO.md`, in prose that never uses the phrasings r29 grepped for — r29 swept the sentences it remembered writing, not the CLAIM, which is the exact failure the standing rule names. Codex and Graefe also measured the value assertion still loose: two named lookups pass for a map carrying an EXTRA field. r30 asserts the map's exact size on BOTH computed reads, rewrites the booking so every statement of the design names all three reads, and reflows the block Torvalds measured at 153 columns. r29. r28 (head `dc9e4b943`): Graefe ACK and Torvalds ACK, each with one required fold, and codex a P2 and a P3 — and the two folds are the same two findings. Graefe and codex measured the third read: it asserted only that the value is NOT an `api.Struct`, where the claim in the booking, this RFC and the test's comments is that it is the SAME raw map the witness gives, so an empty map, a wrong-valued one or a raw protobuf would have passed. Torvalds and codex measured the prose: the superseded two-variable sentence survived in the test's own head comment, the first thing a reader hits, because the r28 delta had corrected its `TODO.md` and RFC twins and left it — and codex found a third copy at the constants' shared header. r29 asserts the map and both values (measured red by moving one of them, which printed the live value `{X:1 Y:10}` and so pins Graefe's measurement rather than restating it) and retires every two-variable sentence, grepped to zero across the homes it counted — a population it put at three, which r31 corrects to four. @claude ACK on the same head, reading the three texts as a 2x2 and confirming the attribution triangulates — witness against wrapper-kept holds the repeat and varies the wrapper, wrapper-kept against control holds the wrapper and varies the repeat — with one non-blocking nit: a 156-column line in the booking where the block wraps at about 95. r29 rewraps it and the two others in the same block. r28. r27 (head `25be07e4b`): Graefe ACK and Torvalds ACK, each with the same required fold, and a codex P2 saying it too — the control also replaces the right leg with a derived-table projection, which is descriptor-relevant, so the pair had two variables and only prose excluded the second. r28 commits the arm all three measured: the wrapper kept, the repeat restored, still a raw map. r27. r26 (head `73c0f8e62`): Graefe NAK, Torvalds NAK and a codex P2, all three measuring the same thing — the one-statement witness repeated `R` as well as `ID`, so the control removed only one of two repeats and could attribute nothing; the retired query was left declared but unrun above the comment claiming it was the control; and the arbitrary-row read had moved to the control rather than being removed. r27 names the CTE's struct apart so exactly one name repeats, reads both texts through one helper that demands a single qualifying row, and deletes the dead query. r26. r25 (head `cbbbc5ed1`): Graefe NAK and Torvalds NAK on the same two — the live booking still described the blunt control r25's own fold had just declared insufficient, and the stored-struct assertion had no witness that its query was poisoned; Torvalds also found "blast radius" doing double duty for two different axes, and the first-non-NULL read pinning an arbitrary row of an unordered result. r26 states the sharp control in the booking, reads the computed and stored structs out of ONE poisoned row, and names the axes apart. r25. r24 (head `04ac0f531`): Graefe ACK with two folds (the planner pin claimed a shared query text covering only the ID half; the booking's reproducer snippet was stale); Torvalds NAK — the r23 fold still carried the stale `(1, 1, true)` measurement and the refuted LATENT classification, in the very bullet r24 edited, and the control removed the join and the duplicate name together; codex three P2 — the pin accepted any non-empty result and any map, and the stored-STRUCT measurement had no committed witness. r25 retracts both in place, keeps the join in the control, and asserts exact rows, values in both representations, and the stored-struct bound. r24. r23 (head `dd64dbe4e`): Graefe NAK and Torvalds NAK — the retracted map framing survived in five more sites and r23 wrongly claimed it had grepped for them, and the FDB negative could go vacuous against a different query; codex P2 went further and refuted the LATENT classification — a computed STRUCT through the poisoned plan comes back a raw map where the same CTE without the join returns an api.Struct, and the no-loss pin could not discriminate with both ID slots equal. r24 sweeps every site, joins the two pins on one query with distinct slot values, and reclassifies the booking as user-visible. r23. r22 (head `8ef36a765`): Graefe NAK, Torvalds NAK and @claude NAK, all three on the same inverted absolute (r22 replaced "every constructor is stamped" with "every constructor is a map"; the measured census is three of four with one survivor) and all three confirming the mechanism sound; codex P2 went further — the data loss those homes claimed does not exist, because the rows flow positionally. r23 states the population in every home, restores the dropped qualifier, and pins the no-data-loss negative. r22. r21 (head `b96603bbf`): Graefe NAK and Torvalds NAK and a codex P2, all three on the same vacuous pin — it planned through a route that never bakes, and its surviving assertion was a tautology; Torvalds also measured the finding understated, the failure being per-repository rather than per-row. r22 bakes the pin, asserts the collateral and the survivor, and states the population in every home. r21. r20 (head `e02ad1451`): Graefe NAK and Torvalds NAK on the same paragraph — the withdrawal of r19's booking rested on a false negative and the both-orders witness was vacuous; codex two P2 — the same two. @claude NAK on the same vacuous witness Graefe found, answered after r21 was cut and folded by it. r21 restores the booking with the reproducer committed as its pin, fixes the witness, and enumerates the swallow arm. r20. r19 (head `c5d07a707`): Graefe ACK (non-blocking: the synthetic counter can collide with an escaped declared name); Torvalds NAK on that collision, measured in both orders; codex P2 — the same collision, with the array witness; @claude ACK. r20 puts synthetic names in a namespace no identifier can reach and moves the guard to Java's own site. r19. r18 (head `eff9e2e4c`): Graefe ACK (a booking: one declared name over two shapes comes back as raw maps, pre-existing; pin the array-of-named-record spelling); Torvalds ACK (the same finding, with Java loud where Go is silent — fold it); codex P2 — the same finding, propagate the conflict as Java does; @claude ACK with the same array-pin ask. r19 makes that failure loud, pins the array spelling, and records the r17 lap below. r18. r17 (head `589a305e5`): Graefe ACK (one booking: the retag guard refuses a named source Java accepts); Torvalds NAK on that same shape, measured — `VALUES (STRUCT RECORD (…)) AS A(W(X, Y))` accepted at the merge-base and refused at r17; codex P2 — the same named-source rejection, with Java's line; @claude ACK (its lap was queued behind the busy runner and answered after r18 was cut). r18 ports Java's rule: the definition renames the fields and keeps the name. r17. r16 (head `46c47a0b8`): Graefe NAK and Torvalds NAK on one measured finding — the inline-values retag minted every VALUES row as a record named RECORD and the bridge laundered it back, so two VALUES rows of different shapes still collided into raw maps (codex's P2 through a second door); codex P2 — the same special case dropped a struct literal DECLARED with the name RECORD to a synthetic name after the bridge; @claude ACK. r17 mints the VALUES row anonymous, drops both name special cases, and pins the VALUES spellings. r16. r15 (head `00a5851d9`): Graefe ACK with one prose fold (the decline set is wider than the bridge's default arm and a NULL literal beside the path reaches it, non-discriminating; book the enum-as-STRING typing); Torvalds ACK with the same scope sentence, the status line's lost @claude mentions, and the same booking; codex P2 — the widened bridge admits an unnamed record and the bridge back named it RECORD, so two anonymous shapes in one derived row claimed one descriptor and their array elements came back as raw maps; @claude ACK. r16 folds the prose, restores the mentions, keeps an anonymous record anonymous through the bridge, and books the enum typing and a fieldless-message table found on the way. r15. r14 (head `ac5cf7ab3`): Torvalds ACK (one nit: a shape-true decline rebuilt the identical body through the net); Graefe NAK — the shape rule's decline must be final, a fallthrough to the leaf lookup re-legitimises the homonym mistyping; codex failed on model capacity (no verdict); @claude lap in flight. r15 returns the exact derivation's answer unconditionally in all three arms and pins, as a negative result, that no shape reaches that decline today. r14. r13 (head `5c80ef758`): Graefe ACK; @claude ACK; Torvalds NAK — the shape rule alone regressed the alias that names a struct column (`st2 AS p`, `p.co`), the post-lookup net r13 deleted must stand beside it in all three arms; codex two P2 — an alias match is not proof of qualification (the same shape), and a declared-STRUCT nested field must survive the exact route. r14 restores the net in all three arms and publishes nominal records through the exact derivation (a pre-existing 0AF00/42703 under codex's finding), pinned in twelve spellings. r13. r12 (head `3315a141d`): Graefe ACK; @claude ACK; Torvalds NAK — the nested-path decision ran after a lookup by the leaf name, so a leaf with a top-level homonym was typed as that column; codex P2 — the newly admitted quoted-dot nested member labels as RFC-238's residual does. r13 decides the nested path by its shape in all three arms, pins the homonym in four spellings, and pins the label residual on the admitted shape. r12. r11 (head `27635cda7`): Graefe NAK and Torvalds NAK on one measurement — the "second gate" r11 read into the climb pin was a fixture artifact (a seed without the MaxMatchMap), and the pin as seeded could never go red; @claude ACK with a bookkeeping item (two fold bullets described a state the diff did not show); codex found no actionable regression. r12 re-seeds the pin as the planner does and shows it red under the mutation, restates the single gate, fixes the nested-field projection of a derived body Torvalds found pre-existing, and restates the two bullets. r11. r10 (head `bd8ec0c66`): Torvalds ACK with five non-blocking folds; Graefe NAK — the group-by rule's PRESERVE branch still stated its keys over its own inner quantifier, and the XX000 yaml pin r10 claimed to remove was still there (a codex run had reverted the working tree under the r10 edits; the removal was redone); @claude NAK on the same yaml fact; codex two P2 (the same preserve branch; a real column named `__ROW_VERSION` classified as the pseudo-column by its name alone). r11 folds all of those, fixes two pre-existing bugs Graefe's probes surfaced (a nested derived table's unaliased qualified output published under its display name; the real `__ROW_VERSION` column), and books the IN-over-aggregate-subquery gap. r10. r9 (head `e92bd661d`): Graefe ACK with one required fold to the BOOKING — the receiving side of the ordering-through-a-projection remainder is not missing ordering parts but a match that never climbs (`correlatedToEquals`), restated in all three homes and pinned; Torvalds NAK with two folds — the swap pin's data was monotone (moved to a fixture whose data discriminates) and an XX000 yaml pin was being credited as a correct rejection (pinned in Go instead) — plus non-blocking folds, all taken; the group-by push rule's synthesized ordering is rebased into its child's current-row space, found when the projection push rule went loud. r8 (head `56a3df6ed`): Graefe ACK with one required fold (the swapped-name body whose sort must stay — a wrong answer at the merge-base); Torvalds NAK on that shape plus six folds; @claude ACK with one residual (the row-versioned remainders unpinned as negatives). r7 (head `cd7bdc5ed`): Graefe ACK (one non-blocking booking: a redundant sort over a renamed grouping key, which r8 fixes as the third adjacent finding); Torvalds ACK with four folds; @claude NAK on coverage (the union-bodied derived table's fix had no regression pin, the added sort on golden #25 was unexplained, a fixture comment cited the wrong RFC-238 section); codex two runs, four P1 findings (a silent wrong answer for quoted case-distinct labels, star bodies bypassing the star-expansion visibility rules, an aliased expression reclassified into a grouping key losing its alias — reported twice). r8 folds all of those. Earlier rounds — r6 folded: 
r5 (head `452479f68`): Torvalds ACK with three folds; Graefe NAK — the loud floor r5
left was wider than stated and the fix is the ordinal-bound edge, not a wider pin (folded as
the second adjacent finding's third and fourth layers); codex five findings, all folded; @claude
ACK with one stale comment, restated. r6 folds all of those. Earlier rounds — r3. r1: Graefe ACK with one non-blocking condition (assert leg alignment at the
logical constructor too — folded as § Fix C); Torvalds NAK with five findings, all folded: a pin
that passed with the defect present now asserts no leg is a `Map` (test plan 2); the translator's
join-leg gate built on the deleted remap premise is deleted too (§ Fix D); two census figures
were overstated and are restated with the commands that produce them; the stale references are
swept (§ Fix E); the scratch probes are gone from the tree. r2 (implementation lap, head
`835d5a462`): Graefe ACK; Torvalds ACK after one residue; @claude review pass with two notes
(folded: `FlowedTypesEqual` in the shared helper, a `TODO-production.md` reference); codex NAK on
three points, all folded in r3 — the rule now declines non-for-each legs (§ Fix A), the adjacent
CTE defect is fixed here (§ Fix F), and the repository-editable nightly cause is fixed with
the host cause escalated. r3 delta: Graefe ACK conditional on the CTE arm's duplicate-name
handling; Torvalds NAK — the coverage-timeout claim refuted by measurement (reverted), the
dispatch step's missing `actions: write`, four stale comments, and the same duplicate-name
hole, resolved on his measurement (the reader's `42702`) rather than by a decline. Awaiting r4
delta confirmation.
**Area:** Cascades implementation rule `ImplementUnorderedUnionRule` and the union executors
(query-engine gate: Graefe + Torvalds).
**Found by:** the engine fuzz nightly, red on `FuzzPlanner_WithBatchA_NoPanic` on 2026-08-30,
08-31, 09-03 and 09-04 (crashers `455b6e6975d288bc`, `9027cb5e7737c749`, `d98799d80e18558c`);
reproduced locally in under two seconds of fuzzing as corpus entry `4a35dedaab03e663`.
**Wire impact:** none. Read path only; no key, record, index or continuation bytes change.

## Problem

A valid `UNION ALL` over a table whose columns were declared with quoted lower-case names does
not plan:

```sql
CREATE TABLE t ("id" BIGINT, "k" BIGINT, PRIMARY KEY ("id"))
SELECT * FROM t UNION ALL SELECT "id", "k" FROM t            -- fails
SELECT "id", "k" FROM t UNION ALL SELECT * FROM t            -- fails
SELECT * FROM t WHERE "k" > 10 UNION ALL SELECT "id", "k" FROM t   -- fails
SELECT * FROM t UNION ALL SELECT "id", "k" FROM t WHERE "k" > 10   -- fails
SELECT * FROM t UNION ALL SELECT * FROM t                    -- plans
SELECT "id" AS w, "k" AS x FROM t UNION ALL SELECT * FROM t  -- plans
```

The same six shapes over `CREATE TABLE u (ID BIGINT, K BIGINT, PRIMARY KEY (ID))` all plan.
Measured over SimFDB with a nine-query probe (the six above plus three upper-case controls):
four fail, five plan and return the six expected rows. The four fail in two different ways,
depending on which leg is the bare scan:

| shape | EXPLAIN | rows |
|---|---|---|
| `SELECT * … UNION ALL SELECT "id", "k" …` (scan leg first) | `XX000 internal error while planning query: unclassified planner failure` | — |
| `SELECT "id", "k" … UNION ALL SELECT * …` (projection leg first) | plans | `resolution error 46 at qov.binding: exact QOV "q$121" (RECORD<id LONG NULL, k LONG NULL> NOT NULL) has no declared runtime binding` |

Same defect, two exits: with the scan leg first the rename produces a leg the union rejects;
with the projection leg first the rename produces a leg the union *accepts* and the executor
cannot bind. Both are the rename firing where nothing needed renaming.

The fuzz target that found it drives the planner API directly, where the same failure shows as
an error from the physical union's constructor:

```
RecordQueryUnorderedUnionPlan result Value: input quantifier 0 type
  PlannerFuzzRow RECORD<K LONG NOT NULL, g LONG NULL, x LONG NULL> NOT NULL
disagrees with input quantifier 1 type
  RECORD<K LONG NOT NULL, G LONG NULL, X LONG NULL> NOT NULL
```

The two legs entered the logical union with the *same* type — the fuzz fixture checks
`Type.Equals` before it builds a union, and the SQL translator states one exact row for every
leg (§ Investigation). Planning then renamed one leg's fields to their upper-case spelling and the
union refused its own rename. Nothing is missing; two spellings of one name were compared under
two different normalizations, which is the defect class RFC-237 exists to end.

## Investigation

### Three mechanisms align a union's legs. Java has one.

| layer | Go | Java |
|---|---|---|
| SQL translator | `exactUnionResultRow` derives one exact positional row (names from leg 0, `MaximumType` per slot) and `normalizeUnionLeg` re-emits any leg that differs as a projection onto that row (`cascades_translator.go`) | `SemanticAnalyzer.validateUnionTypes` folds `Type.maximumType` over the legs; `LogicalOperator.generateUnionAll` wraps any leg whose type differs in a promoting select (`LogicalOperator.java:604-646`) |
| implementation rule | memoize each leg's plan partition, **then read every leg's column names off its physical plan, compare, and wrap a differing leg in a rename `Map`** (`rule_implement_unordered_union.go:92-146`) | memoize each leg's plan partition and yield `RecordQueryUnorderedUnionPlan.fromQuantifiers` (`ImplementUnorderedUnionRule.java:57-71`). Nothing else. |
| executor | `executeUnorderedUnion`, `executeUnionStreaming` and `executeUnionBuffered` each read every leg's column names off its plan again (`planColumnNamesWithMD`) and re-type a differing leg's rows by position (`remapUnionColumnsByPosition`); `executeUnion` additionally chooses a non-resumable *buffered* path when a leg's names cannot be read statically | `RecordQueryUnionPlanBase` takes the first leg's type (`RecordQuerySetPlan.mergeValues`, "let's just pick the first result type for now") and the cursors concatenate rows as they are |

The translator layer is the port of Java's. The other two are Go-only, and they date from before
the translator did this work: RFC-078 added the executor remap in 2026-06 for aggregate legs with
mismatched aliases, RFC-080 relaxed its gate, RFC-183 §14–15 and RFC-184 W2 made the rule's rename
`Map` a memo-reachable compensating operator. RFC-226 (2026-08-10) then made every projection state
its row and the translator state one exact row per leg, at which point every leg of every SQL
union carries the *same* `RecordType` into `NewLogicalUnionExpression`, and the physical
constructor (`newPlanExprBaseForFirstQuantifier`, `plan_expression.go:196-208`) refuses any leg
whose flowed type is not `Equals` to leg 0's.

So at the point either Go-only mechanism runs, the legs are already exactly aligned or planning
has already failed. There is nothing left for them to align.

### What they do instead: fold case on one path and not the other

`physicalPlanColumnNames` (the rule's walker) answers from the first arm that matches:

| leg's physical top | names come from | folded? |
|---|---|---|
| `RecordQueryProjectionPlan` | `GetOutputNames()` | no — exact |
| `RecordQueryMapPlan` | result `RecordConstructorValue` field names | **`strings.ToUpper`** |
| `RecordQueryStreamingAggregationPlan` | returns nil (rename skipped) | — |
| anything with `GetInner()` | descends | — |
| terminal (`Scan`, `CoveringIndexScan`, …) | `GetResultType()` record fields | **`strings.ToUpper`** |

`planColumnNamesWithMD` (the executor's walker) has the same shape with the same two folds at the
tail (`executor.go:3031-3048`), plus a third over the proto descriptor.

A SQL union leg is a bare scan whenever the star projection is an identity over the whole row and
is eliminated: `SELECT * FROM t1 UNION ALL SELECT id, col1, col2 FROM t1` plans as
`UnorderedUnion(Scan(T1), Project([…], Scan(T1)))` (`plan_shape.golden`, `union_star.yaml#2`).
With upper-case DDL the fold is a no-op and both walkers agree. With quoted lower-case DDL the
scan leg reads as `[ID K]` and the projection leg as `[id k]`, so the rule decides they differ
and wraps whichever leg is *not first* in a rename `Map` targeting the first leg's spelling:

- scan leg first: leg 1 (the projection) is renamed to `ID`/`K`; its new type
  `RECORD<ID, K>` is not `Equals` to leg 0's `RECORD<id, k>`; the union constructor refuses it,
  the rule fails the call, and no combination yields — planning fails.
- projection leg first: leg 1 (the scan) is renamed to `id`/`k`; its new type *is* `Equals` to
  leg 0's; the constructor accepts, and the plan carries a `Map` whose rename value reads its
  input through `values.UniqueCorrelationIdentifier()` (`columnRenameValue`,
  `rule_implement_unordered_union.go:231`) — a correlation nothing at runtime declares. The
  executor reports `qov.binding … has no declared runtime binding` on the first row.

So on the only path where the rename is accepted, the rename cannot execute. The mechanism has
been dead on every committed shape (census below) and broken on the first shape that reached it.

The census also shows the executor's walker folds on exactly the same legs, so removing only
the rule's rename would let those queries plan and then hand the same disagreement to
`remapUnionColumnsByPosition`, which would re-type leg 1's rows to names leg 0 does not have.

RFC-237 §1: Java folds an identifier in one place, the parser, and "everything downstream compares
EXACTLY". These four `ToUpper` calls are a second fold, inside the planner and the executor, and
the fuzz found the one input where a fold-then-compare and an exact compare disagree.

### Census: neither Go-only mechanism fires on any committed shape

Instrumented at `36b97f1e9` (markers on the rule's rename, the executor's remap, the buffered
path, and both walkers' case folds) and run over every SQL corpus with `--nocache_test_results`:
`yamsql_test`, `golden_test`, `sqlpage_test`, `metamorphic_test`, `rowdiff_test`, `factory_test`,
`factorycorpus_test`, `factorycorpus/full:full_test`, `explaindiff_test`, and the `Union`-named
subset of `sqldriver_test`.

All eleven targets passed (`Executed 9 out of 9` and `Executed 1 out of 1`, no cached results),
so no marker that panics ever fired. Counts per corpus, from the test logs:

| corpus | rule reached its rename decision | rule inserted a rename `Map` | executor union entered | executor re-typed rows | buffered path | a fold changed a name | walker reached the folding tail |
|---|---|---|---|---|---|---|---|
| `explaindiff_test` | 360 | 0 | 0 (EXPLAIN only) | 0 | 0 | 0 | 0 |
| `rowdiff_test` | 1936 | 0 | 0 (plans only) | 0 | 0 | 0 | 0 |
| `yamsql_test` | 37 | 0 | 36 | 0 | 0 | 0 | 10 (8 `Scan`, 2 `CoveringIndex`) |
| `sqlpage_test` | 47 | 0 | 1049 | 0 | 0 | 0 | 0 |
| `sqldriver_test` (`--test.run=Union`: 42 test functions, 90 `=== RUN` lines with subtests) | 148 | 0 | 123 | 0 | 0 | 0 | 64 |
| `golden`, `metamorphic`, `factory`, `factorycorpus`, `factorycorpus/full` (8151 cases) | 0 | 0 | 0 | 0 | 0 | 0 | 0 — these corpora contain no `UNION` |
| **total** | **2528** | **0** | **1208** | **0** | **0** | **0** | **74** |

Two of those columns carry the finding. "A fold changed a name" is zero because every union
leg the suite has ever named is upper-case: the dimension that was never probed is a union leg
whose column names are not their own upper-case spelling. "Walker reached the folding tail" is
74 because bare-scan legs are common — the fold is *reached* constantly and has simply never had
anything to change.

Independently, the committed plan corpora at the merge-base `36b97f1e9` hold 36
`UnorderedUnion(` plan lines in `plan_shape.golden` and 4 across the yamsql and golden testdata
(the factory corpus has none); **0 of 40** carry a `Map(` leg. Measured as line counts:

```
git grep -c 'UnorderedUnion(' 36b97f1e9 -- pkg/relational/conformance/explaindiff/testdata     # 36
git grep -c 'UnorderedUnion(' 36b97f1e9 -- pkg/relational/conformance/yamsql/testdata \
    pkg/simfdb/hunt/golden/testdata pkg/relational/conformance/factorycorpus/testdata          # 4 (yamsql 3, golden 1)
git grep    'UnorderedUnion(' 36b97f1e9 -- pkg/relational/conformance pkg/simfdb | grep -c 'Map('   # 0
```

(r1 quoted 17 for the second figure; that count matched `Union(` and so included `InUnion(`.)
The rename `Map` that RFC-183 §14 counted ten unreachable edges for has not been produced by any
committed query since RFC-226.

The mechanisms are dead on every shape the suite knows and wrong on the first shape it did not.

## Java

`ImplementUnorderedUnionRule.onMatch` (4.12.11.0, `rules/ImplementUnorderedUnionRule.java:57-71`):

```java
final ImmutableList<Quantifier.Physical> quantifiers =
        Streams.zip(planPartitions.stream(), allQuantifiers.stream(),
                (planPartition, quantifier) -> call.memoizeMemberPlansFromOther(
                        quantifier.getRangesOver(), planPartition.getPlans()))
                .map(Quantifier::physical)
                .collect(ImmutableList.toImmutableList());
call.yieldPlan(RecordQueryUnorderedUnionPlan.fromQuantifiers(quantifiers));
```

`RecordQueryUnorderedUnionPlan.fromQuantifiers` → `RecordQueryUnionPlanBase(quantifiers,
reverse)` → `RecordQuerySetPlan.mergeValues(quantifiers)`, which types the result as the first
non-existential leg's flowed type and never compares legs. `UnorderedUnionCursor` concatenates the
child cursors' results unchanged.

Java can afford that because `LogicalOperator.generateUnionAll` has already promoted every leg
onto `SemanticAnalyzer.validateUnionTypes`'s common type before the logical union exists. Go's
translator does the same job in `exactUnionResultRow` + `normalizeUnionLeg`, and Go's physical
constructor additionally *asserts* the alignment that Java assumes. That assertion is the reason
the two Go-only mechanisms can be deleted rather than repaired: an unaligned union is loud at
construction, so nothing downstream needs to guess.

One divergence from `mergeValues` is pre-existing and untouched here: Java wraps every leg's
flowed value in a `DerivedValue`, so the union's result carries every leg's correlations; Go's
result is leg 0's QOV alone (`logical_union.go:54-58`). That is a difference in what the value
*refers to*, not in the row it states, and this RFC changes neither.

## Fix

One authority per RFC-237 and per Java: the translator aligns; the physical constructor asserts;
nothing below re-derives names.

**A. `ImplementUnorderedUnionRule` becomes Java's rule.** Delete the rename block (lines 92-146),
`physicalPlanColumnNames`, `colNamesEqual`, `columnRenameValue`, `recordTypeFieldCount` and the
`childPlans` slice that existed only to feed them. What remains is: roll up each leg's plan
partitions, decline a combination with an empty leg (the Go partition representation can produce
one, Java's matcher cannot), memoize each leg's plans into a physical quantifier, and yield
`NewRecordQueryUnorderedUnionPlanFromQuantifiers`. The constructor's exact-type check is the
asserted bridge; a leg that disagrees fails the call loudly, as today. The rule also declines a
union with a leg that is not a for-each quantifier: Java's matcher is
`all(forEachQuantifierOverRef(…))` (`ImplementUnorderedUnionRule.java:63-64`), and a
concatenating union over an existential leg would emit that leg's rows. The r2 text listed this
as unreachable and left as found; codex's review held that "does no more than Java" has to be
true of the rule itself, not only of what SQL reaches, and the guard is one line.

**B. The union executors stop re-typing rows.** Delete `remapUnionColumnsByPosition`,
`planColumnNames`, `planColumnNamesWithMD`, `streamingAggOutputNames` (its only caller) and the
`innerPlanAccessor` interface (its only user). `executeUnorderedUnion` executes each leg and
concatenates. `executeUnion` always takes `executeUnionStreaming`, whose `md`/`targetKeys`
parameters go; `executeUnionBuffered` — the path that existed only to peek rows for column names,
and that refuses every continuation — is deleted with them. Comments that describe the buffered
fallback (`executor.go:83`, `memory_budget.go:249`,
`continuable_without_duplicates_property.go:76`) are corrected.

A leg's rows flow under the names its own plan states. Legs state the same names by construction
(translator) and by assertion (constructor), so a downstream by-name read resolves identically on
every leg. Mixed row kinds — a bare scan leg emitting record rows beside a projection leg emitting
positional rows — are already the shape of `union_star.yaml#1` and `#2` and pass today with the
remap a no-op.

**C. The logical constructor asserts too.** `requireSetOperationResult` took leg 0's flowed
value unchecked, so a union built with unaligned legs (a planner-API producer has no translator
in front of it) entered the memo and died later, from a rule body, as `XX000 unclassified
planner failure` — a group with no realizable implementation. It now requires every flowing leg
to state a row `Equals` to leg 0's. The recursive union had refused disagreeing states through
its own `FlowedTypesEqual` check — the same contract in a second implementation — and now
asserts through the same helper (`requireSetOperationResults`) over the same leg list as
Java's `mergeValues(ImmutableList.of(initial, recursive))` — with the equality assertion Go
keeps and Java's `mergeValues` does not make (Rejected, third bullet). The physical constructor's check stays as
the re-check at the plan boundary. Also here: the
result-value comment at `logical_union.go:60` cited `TestSetOperationResultValueStatesChildZerosRow`,
which does not exist in the tree; the pin is `TestSetOperationsStoreFirstNonExistentialExactQOV`.

**D. The translator's join-leg gate goes with the remap it was built on.** `unionOutputColumns`
anchors a UNION used as a join leg or derived table to its first branch's columns, and it
declined — returning nil, which surfaces as an unsupported-shape error — whenever the branches'
names differed and any branch failed `unionBranchNormalizable`, a classification of "whether the
executor's union position-remap can remap this branch" (`cascades_translator.go:1051`). The
remap is gone; `normalizeUnionLeg` re-emits every differing branch onto the union's row by
ordinal, whatever the branch's shape. The gate, `unionBranchNormalizable`,
`aggregateNamesStableForUnion` and their unit test are deleted; the anchoring keeps only the
width check. Probed before deciding: the three operand forms the gate's aggregate arm refused
(qualified `SUM(ga.v)`, constant `COUNT(1)`, group-only) return identical, correct rows at the
merge-base and after the deletion — every SQL-built branch is a `LogicalProject`, so the
aggregate arm was reachable only from directly constructed trees, and deleting it changes no
SQL-visible behaviour. That is the finding: a live gate, eight unit tests, and a paragraph of
justification, all defending a premise no SQL query could reach.

**F. A CTE body publishes its exact row under its SQL names, repeated names included.**
`buildCTEColumnSource` has two arms that build the body and publish its exact row — the
join/derived-bodied arm, and (since this RFC) the single-table aggregate arm, which calls
`buildExactScopeSourceOrBodyError` before `buildDerivedTableSourceFromAgg`, the order the
derived-table path already had (see the first adjacent-finding section for the mechanism). The
parse-tree derivation stays as the aggregate arm's fallback for a row `semantic.Column` cannot
carry — a catalog nested record the exact derivation refuses and the parse-tree one carries
verbatim — never for a row the exact derivation published. Three things about those arms were
wrong, each measured at the merge-base and each in the CTE spelling only, where the
derived-table spelling of the same body already answered as Java does:

- *The join-bodied arm declined a repeated output name.* `scopeSourceNamesUnique` withheld the
  whole source when the row named a column twice, on the theory that a published duplicate
  would bind silently. The decline was the silent bind: the CTE fell to the ON-only class, and
  `u.g` over `WITH u AS (SELECT ga.g, c.id AS g FROM ga, c)` then bound one duplicate in SELECT
  and the other in ORDER BY, in seven read paths, while the derived-table spelling reported
  `42702`. The gate is deleted. A repeated name is published as stated, and every read of it —
  bare or qualified, in SELECT, WHERE, ON, ORDER BY, GROUP BY, HAVING, EXISTS and a scalar
  subquery, Graefe's r4 delta measured all of them — meets the semantic scope's own
  per-source ambiguity check and reports `42702`, Java's `AMBIGUOUS_COLUMN`, byte-identical to
  the derived-table spelling. The first cut of the aggregate arm had the same gate and fell to
  the parse-tree derivation on a repeat; Torvalds's r3 delta measured the reader loud without
  it, and Torvalds's r4 delta measured the join arm silent with it.
- *The join-bodied arm took its names from the exact derivation.* The derivation labels a
  qualified reference by its datum key — `ga.g` is `GA.G` there — so, with the gate gone, the
  row `SELECT ga.g, c.id AS g` carried `GA.G` and `G`: two distinct names for what SQL calls `G`
  twice, and `u.g` bound the second without ever meeting the ambiguity check. Both arms now
  pass the SQL output-name authority the derived-table path passes — `projectionOutputNames`
  for a spelled projection, `aggOutputCols` for an aggregate body — so the two spellings of one
  body publish one row by construction. A star body spells no projection and keeps the
  derivation's labels, as before.
- *`aggOutputCols` named an unaliased grouping key by its qualified spelling.* The parser
  mints a grouping item's `outName` from the reference's display text, so `GROUP BY ga.g`
  published `GA.G` — a name no reader can write — and `u.g` over
  `(SELECT ga.g, SUM(v) AS s FROM ga GROUP BY ga.g) u` was `42703` in both spellings. The
  body's own projection labels that slot `G` (`aggregateProjectionItem` treats an `outName`
  equal to the reference as no alias and labels the stripped reference); `aggOutputCols` now
  applies the same rule and publishes the bare name.

One shape stays out of the global scope on purpose, keyed on its SHAPE and never on its
names: a `SELECT *` body over a lateral unnest that the narrow single-source admission does
not take — a second base table beside the unnest, or an EXISTS in its WHERE — is the gathered
multi-source unnest cluster. The translator flows that cluster as its raw per-leg positional
seed and binds an aggregate's keys and operands over the CTE to the seed by ordinal
(`exactGatheredCTEGroupKeyValue`, which admits only a CTE absent from the scope); a
published exact row minted a read over the CTE's own quantified object instead, which nothing
declares at execution. At the merge-base the uniqueness gate happened to withhold the
repeated-name body of that shape (`A.K` beside `B.K`) and the unique-name body was published
and failed as an undeclared binding; the decline now covers both, and both answer.

Pinned in both spellings: the repeated name through every read path that bound silently
(`cte_published_row_names.yaml`), the qualified grouping key over a single-table and a joined
body, and the unique-name control beside each. The gathered-unnest CTE aggregates are the
sqldriver `TestFDB_UnnestExistsGather` pins (`agg_cte_*`), which the first cut of this round
reddened.

**E. Stale references.** Every comment that described the rename `Map`, the position-remap, the
buffered fallback or the walkers as live — `default_rules.go`, `streaming_cursors.go` (three
sites), `executor_new_plans.go`, `aggregate_index.go`, the RFC-078/080/081 e2e test headers, two
ordering-contract test headers, `DIVERGENCES.md`, and two error-translation fixtures that used
`"buffered union"` as their context string — is restated in terms of the mechanism that remains.
The sweep is the residue of a scoped grep over `pkg`, `TODO.md`, `DIVERGENCES.md` and
`.claude/skills` for the deleted symbols and phrases; what remains names them only as deleted.
Two census gates moved with the deletion and are updated with it: RFC-213's result-type
consumer census loses the three walker reads (GUARDED 12 → 10, PROPAGATED 28 → 27, each site
named in the pinned test's comment and in RFC-213 §3), and RFC-238's line cites into the
edited files are re-pointed at the lines their cited text now occupies. `newChargeReleasingCursor`
lost its only caller with the buffered path and is deleted too, as are the two
`OutputColumnNames` accessors (aggregate index, streaming aggregation) whose only production
caller was the executor walker; the tests that read them read the plan's stated result row
instead, which is what production derives the ordering domain from.

### Rejected

- **Remove the four `ToUpper` calls and keep both mechanisms.** Fixes the fuzz input and the four
  queries, and leaves two dead re-derivations of a name whose only remaining effect is to
  disagree with the translator the next time a walker arm and a projection arm answer
  differently. RFC-183 already paid once for the rename `Map` living outside the memo; RFC-078's
  three walker arms exist because each new physical plan type needed one. "Long-term correct" is
  the criterion, and that means one mechanism.
- **Compare with `EqualFold` instead.** Makes the rule's decision case-insensitive while the
  constructor's stays exact, so the rule would decline to rename `id`/`ID` legs that the constructor
  then rejects. It moves the disagreement, and it adds a case-insensitive comparison at a site
  RFC-237 says compares exactly.
- **Relax the constructor to Java's "pick the first type".** Would make Go accept a union Java
  accepts, but Go's executor reads columns by name where Java's reads a `Message` by descriptor;
  the exact check is what lets the executor drop its defensive remap. Keep the stronger assertion.

## Duplicate mechanisms — what this collapses

Before: three places compute "the union's column names" (translator, rule walker, executor
walker) and two of them fold case. After: one place states them, one place asserts them. The
`values.OutputColumnName` authority RFC-229 established for projections is untouched; the deleted
walkers were readers of it, not authorities.

Not collapsed, and not touched: `planColumnNamesWithMD`'s sibling in the aggregate-index and
streaming-aggregate cursors (`CanonicalAggColumnName`), which name a cursor's *own* output row
rather than compare two legs'.

## Performance

None expected. The deleted code ran once per union implementation (a walk over each leg's plan
chain) and once per union execution (a walk plus, when names disagreed, a per-row `MapCursor`).
The census shows the per-row path never engaged on a committed shape. No cost-model input
changes; no plan choice changes — the goldens are expected to move by zero lines, and the
`just golden` gate is the measurement.

## Test plan

Every proof is committed; each names the dimension that was unprobed.

1. **Fuzz corpus.** `testdata/fuzz/FuzzPlanner_WithBatchA_NoPanic/4a35dedaab03e663` (input
   `[]byte{35, 4, 4, 33}` = `Union(TypeFilter(TypeFilter(Scan)), TypeFilter(Projection(Scan)))`
   over a row type with lower-case fields), plus the same bytes as an `f.Add` seed with the
   regression note, so the shape replays under `go test` and under Bazel (the target globs
   `testdata/**`).

   A **second** entry pins the same defect on a second target, and that target — not the one
   that opened this RFC — is the sole failure of the `Nightly Fuzz` workflow on each of the
   two nights measured: run 33955019772 (2026-09-05) across the 44 target/function pairs that
   rotation executed, and run 34022483592 (2026-09-06) across 45. Both nights the only
   `::error::fuzz FAIL:` annotation names
   `//pkg/recordlayer/query/plan/cascades:cascades_test FuzzPlanner_PlanFullPipeline`.
   "Sole failure" is the workflow's own verdict and not a reading of the word FAIL: the
   second night also has one target hit the golang/go#72104 budget-expiry race
   (`metadata_test FuzzMessageTypeFromDescriptor`), which the job classifies, retries and
   does not count — it consumed its budget and wrote no crasher.

   Two nights is worth more than twice one night here, because the rotation SELECTS: the two
   runs share only 15 of their pairs, so this is not one selection failing twice. Naming the
   runs is the point: "the nightly is red on X" is a claim about a tree that moves nightly,
   and this one stops being true the moment this branch merges.
   `testdata/fuzz/FuzzPlanner_PlanFullPipeline/05cb6c3c202ef15e`, input `[]byte("\x0271!00")` =
   `Distinct(Union(UnsortedSort(Scan), Projection(Scan)))`. It matters that it is a separate
   entry rather than a cousin of the first: it reaches the rule through a DIFFERENT fuzz target
   with a different assertion (`Plan` returning an unexpected error, not a panic), it needs no
   `TypeFilter`, and the leg that carries the exact spelling is the projection while the leg
   that carries the folded one is a bare `Scan` — the TERMINAL arm of `physicalPlanColumnNames`,
   `GetResultType()`'s record fields put through `strings.ToUpper`, which the first entry does
   not exercise. (r49 wrote that as "the table above's last row". The nearest table above it is
   the corpus census, whose last row is `**total**`, so the pointer resolved to the wrong thing
   the day it was written — an ASSERT-class pointer, added by the commit that documents the
   hazard of exactly those. Naming the arm cannot be retargeted by an insertion.)
   Measured both ways with the same command, on both trees:

   ```
   36b97f1e9 (merge-base):  FAIL  FuzzPlanner_PlanFullPipeline/05cb6c3c202ef15e
       Plan: unexpected err RecordQueryUnorderedUnionPlan result Value: input quantifier 0
       type PlannerFuzzRow RECORD<K LONG NOT NULL, g LONG NULL, x LONG NULL> NOT NULL
       disagrees with input quantifier 1 type RECORD<K LONG NOT NULL, G LONG NULL, X LONG NULL>
       NOT NULL
   this branch:             PASS  (all five entries; the other four pass on both sides)
   ```

   The manufactured `G`/`X` appear in neither leg: leg 0 is a `Scan`, so the walker folds its
   record type to `["K","G","X"]`; leg 1 is a `Projection`, so `GetOutputNames()` answers
   `["K","g","x"]` exactly. The rule reads that as a disagreement and renames leg 1 to leg 0's
   FOLDED list, minting a spelling no leg has — and the union constructor then refuses the plan
   the rule just built. Reverse the two legs and it plans, which is the same order-dependence
   the SQL shapes above show, arrived at from random bytes with no SQL in the path.

   The entry proves something sharper than "redundant", and the fixture is what makes it
   provable: `buildFuzzExpression` only BUILDS the union when `plannerFuzzSameResultType(ql, qr)`
   holds, so the two legs' types are already `Equals` before any rule runs — the fixture's
   structural stand-in for the alignment the translator does in SQL. And that guard is not
   merely analogous to the gate that later refuses the plan: it is the same `Equals` predicate
   reaching the same accessor. `plannerFuzzSameResultType` compares `GetFlowedObjectType()` on
   both legs; `newPlanExprBaseForFirstQuantifier` calls `GetFlowedObjectType()` directly for
   legs 1..n and reaches it for leg 0 through `RequireFlowedObjectValue`, which may substitute
   a carrier for the flowed value on the way. The load-bearing part is not how many routes do
   that but the INVARIANT they all hold: every one of them either checks type identity or
   preserves it by construction, so `firstType` is the flowed type whichever ran. Stating it
   as an invariant rather than a list is deliberate — two review laps enumerated the routes and
   each was short by one, which is what a count of branches does when nothing drives them.
   INSTANCES INCLUDE — not "are", because the list has now been short three laps running, and
   the third correction is the one that says stop listing: the provided-layout branch (guarded
   by its own `Equals`); the identity layout (`NewOrdinalLayoutForCarrierType`, identical by
   construction — note the internal carrier/result `Equals` on that path is self-referential,
   both sides coming from the same `provided`, so it preserves identity rather than checking
   it, which is why the concrete-record control is what actually pins it); the erased-record
   fallback, whose identity comes from `SnapshotExactType` and which had no test at all until
   this round; and the `OrdinalLayoutDynamicCarrier` early return in
   `newPlanExprBaseForQuantifier`, which hands back `RequireFlowedObjectValue()` itself and so
   satisfies the invariant trivially — a DIFFERENT function from the other three, gated on the
   quantifier having a child whose layout is unavailable, and production-reachable because
   `proto_field_type.go` constructs an erased record type outside any test.

   Both are pinned now rather than only named:
   `TestResultBaseForAnErasedRecordTakesTheCarrierlessExit` drives the third and
   `TestResultBaseForAQuantifierOverAnErasedChildKeepsTheFlowedValue` the fourth, each beside a
   concrete control that keeps its "no properties" assertion from being vacuous, and each
   mutation-verified: the erased pins redden and the controls stay green. The fourth asserts its
   two preconditions explicitly — that the child's layout error really is
   `OrdinalLayoutDynamicCarrier`, and that the quantifier really has a selected child — because
   without them a carrierless result would be indistinguishable from the fallback one function
   down. A further route would have to break the invariant to break the argument, which is the
   point of stating one. So the closure is:
   the predicate holds before the rule, fails after it, and only the rule ran in between. So the second alignment is
   not re-deriving a property that was established upstream and getting the same answer; it is
   MANUFACTURING a disagreement that did not exist, out of two spellings of a name it folded on
   one path and not the other. That is the RFC's thesis stated as an experiment rather than an
   argument.

   Unguided fuzzing at the merge-base finds it in about a second, and the input it finds is a
   fact about the RUN and not about the target: the nightly minimised a 113-byte input, the run
   that produced the committed entry minimised a 43-byte one to the six bytes here, and a
   reviewer reproducing it independently started from 51. Only the six are committed; the byte
   counts are named so that a later run reporting a different one is not read as a discrepancy.
   The corpus entry makes it a deterministic sub-test that runs on every `go test` and every
   Bazel run of the target.
2. **Planner unit pin, on both exits.** The fuzz shape driven through `Plan` deterministically in
   both leg orders (`union_leg_names_not_refolded_test.go`). Scan leg first asserts the legs'
   flowed types are `Equals`; projection leg first asserts additionally that **no leg is a
   `RecordQueryMapPlan`**, because on that exit the rename's type matched and only the `Map`'s
   presence betrays it — a type-only assertion there passes with the defect present (verified
   against the merge-base by review). The upper-case row is the control.
3. **Both asserted bridges have tests.** `NewRecordQueryUnorderedUnionPlanFromQuantifiers` over
   two legs whose types differ only in field-name case returns the `disagrees` error
   (`unordered_union_leg_types_test.go`) — the constructor check the fix relies on had no test
   driving it. The logical constructor added in § Fix C is pinned the same way for union and
   intersection, beside a pin that an existential leg is still exempt and a positive control
   (`set_operation_leg_types_test.go`).
4. **SQL e2e, real FDB.** New yamsql scenario `union_quoted_identifiers.yaml`: the quoted
   lower-case table, the four previously failing shapes with their rows and `plan_contains`
   pinning a bare-scan leg beside a projection leg with no `Map` between them, the
   scan-first/projection-first pair in both orders (the two exits above), `columns:` pinning
   the quoted spelling of the labels, and the upper-case control.
5. **SQL e2e, SimFDB.** The same shapes as a `golden` corpus scenario so `just test` runs them
   without Docker and the baseline captures both plan and rows.
6. **Executor.** The RFC-078/080/081 e2e tests (`union_aggregate_remap_test.go`,
   `union_scalar_aggregate_alias_test.go`) stay as they are; they now prove the translator alone
   handles mismatched aggregate aliases. `plan_column_names_test.go` and the two
   `TestPhysicalPlanColumnNames_*` tests are deleted with the walkers they test.
7. **Negative result pinned.** A union whose legs' names differ at the *logical* boundary is not
   constructible from SQL (the translator normalizes) — pinned by the yamsql scenario's
   `SELECT "id" AS w, "k" AS x … UNION ALL SELECT *` case, whose plan shows both legs projected.
8. **The gate's shapes.** `union_join_leg_aggregate_forms.yaml` (real FDB) and the golden
   `unionjoinleg` scenario (SimFDB) run the three operand forms the deleted gate refused —
   qualified, constant, group-only — as union join legs, plus the bare-column control, and
   assert rows. They pass at the merge-base and after the deletion, which is the measurement
   § Fix D rests on: the gate's aggregate arm was never reached from SQL.
9. **The rule's matcher.** `TestImplementUnorderedUnionRule_DeclinesANonForEachLeg` fires the
   rule over a union with an existential leg and asserts it yields no union plan, with the same
   two references as for-each legs as the control that does implement.
10. **The CTE aggregate body.** `cte_expression_aggregate_join_leg.yaml` (real FDB) and the
    golden `cteagg` scenario (SimFDB): the failing shape, the same body as a derived table, the
    bare-column control, and the aggregate read through the CTE without a join. The first
    fails at the merge-base with the column-order error and returns the rows after § Fix F.
    The same scenario pins the duplicate-output-name body in both forms as the reader's
    `42702`, so the CTE and derived-table spellings of one body cannot drift apart and the
    parse-tree fallback is never what answers a published row.
11. **The published CTE row.** `cte_published_row_names.yaml` (real FDB): the join-bodied
    repeated name through the ten read paths that bound silently at the merge-base, in the CTE
    and the derived-table spelling, each `42702`; the qualified grouping key over a single-table
    and a joined body, both spellings, rows and labels; the unique-name join-bodied control. The
    ORDER BY metadata pins move with the rule they pin: the repeated-name body that was the
    "underivable" specimen now resolves a computed key and a computed projection
    (`TestOrderByExactMetadata_Computed*OverRepeatedNameCTEResolves`), and the stays-loud pair
    keeps a specimen that is genuinely underivable — a row with a catalog struct column the
    semantic column model cannot state. The SimFDB golden `ctenames` pins the plans and rows
    of the planning shapes. The sqldriver probe suite's Q53–Q56, which pinned
    complete-schema-or-decline (`0AF00` for any repeated name), are re-pinned to Java's
    answers — `42702` for a read that spells the repeated name, rows for a read of a unique
    column beside it — and Q57 pins the reads bound to the quantified object of a body that
    repeats a bare leaf (an aggregate key, a sort key, a WHERE, both spellings) beside the
    aliased control; `TestFDB_UnnestExistsGather`'s `agg_cte_*` pins hold the gathered-unnest
    decline, with a unique-name twin beside the repeated-K pins so the arm tells the shape
    from the names, and an aggregate-bodied CTE over that shape so the decline is known to
    stop at star bodies.
12. **Repeated output names.** `repeated_output_names.yaml` (real FDB) and the SimFDB golden
    `repnames`: the labels of
    `SELECT g AS a, g AS a`, `SELECT id, g AS id` and a star over a body that repeats a name,
    and the values of `SELECT *` over such a body beside another table — the repeated-name leg
    first and second, the CTE and the derived-table spelling, an aggregate body and a plain one,
    the unique-name control beside each. Unit pins: `frozenSchemaRenamesSlot` on the six
    rename-versus-dedup shapes; `mergedRVSequenceDiverges` tolerating a repeated display name
    and still rejecting a different name and a reordering; `derivedOutputColumns` naming a
    repeated output exactly as `values.DedupFieldNames` does, at one, two and three
    repetitions.
13. **Sort elision across a renaming projection.** `TestSortElisionCrossesARenamingProjection`
    (the rule-time winner and extraction both elide `ORDER BY h` over `(SELECT status AS h)`
    whose source is STATUS-sorted; fails with the translation removed) and
    `TestSortElisionDeclinesAComputedSlot`; `cte_published_row_names.yaml` §6 pins
    `plan_not_contains: InMemorySort` over a renamed primary key in both spellings with the
    DESC twins; `plan_shape.golden` records the 16 corpus queries that lose their sort, and
    `ordering_through_a_projection.yaml` pins the swapped-name body (`g AS id, id AS g` over
    non-monotone g) whose sort must STAY, in both spellings, beside the elided twin.
14. **Star-body visibility.** `derived_star_visibility.yaml`: the unnest alias shadowing the
    outer column (three reads, both spellings), quoted case-distinct labels (four reads), the
    reclassified alias (both spellings); `derived_star_row_versions.yaml`: the star over
    row-versioned tables in both spellings and a two-column read.
15. **Ordering through a projection.** `ordering_through_a_projection.yaml`: the base table
    takes the index on `g` and the reverse primary-key scan; through a derived table and a CTE,
    unrenamed and renamed, the rows are right and the sort is pinned as still in memory (the
    booked receiving-side remainder), a computed slot keeps its sort.
    `TestPushRequestedOrderingThroughProjection_*` expect the pushed constraint in the child's
    current-row space.
17. **A real `__ROW_VERSION` column and a nested derived table's bare name.**
    `derived_star_visibility.yaml` §5 (both spellings, GROUP BY, ORDER BY) and
    `cte_published_row_names.yaml` §9 (join body, single-table body, CTE spelling, aliased
    control).
18. **A nested-field projection in a derived body.** `cte_published_row_names.yaml` §10: the
    single-level body, the derived-over-derived body, the aliased control, the CTE spelling.
19. **The nested leaf with a top-level homonym.** `cte_published_row_names.yaml` §11 (four
    spellings, the top-level control) and `TestFDB_QuotedDotNestedMemberLabel` (the quoted-dot
    member's value and RFC-238's label residual, both spellings, the aliased control).
20. **The alias that names a struct column.** `cte_published_row_names.yaml` §12: five spellings
    (the derived table bare and under a WHERE, the derived-over-derived body, the CTE bare and
    under a WHERE) beside the top-level control.
21. **The nested field of a declared STRUCT type.** `cte_published_row_names.yaml` §13: no
    homonym (bare, under a WHERE, the CTE, the derived-over-derived body), a homonym of another
    type, a homonym of the same type, the top-level control;
    `TestSemanticColumnFromExactTypeCarriesRecordName` (the nominal record round-trips under its
    name; the fieldless record still declines).
22. **The shape rule's decline is final, and no shape reaches a decline the walk would answer differently.**
    `TestDerivedNestedEnumFieldTypesAsStringSoTheShapeRuleNeverDeclines` (Java-authored metadata:
    an enum field beside its STRING homonym plans through the derived table and the CTE, the
    exact row is the one STRING column; red once the exact derivation carries enums) and
    `TestSemanticColumnFromExactTypeDeclinesEnum` (the bridge's own contract).
23. **An anonymous record through a derived row.**
    `TestFDB_AnonymousRecordsThroughADerivedRowKeepDistinctIdentities` (two anonymous shapes in
    one derived row, the CTE and derived-over-derived spellings, two top-level controls, and two
    VALUES rows of different shapes at top level and through a derived table: every array element
    an `api.Struct`), the anonymous arm of `TestSemanticColumnFromExactTypeCarriesRecordName`
    (round-trips with no record name; a record named RECORD keeps that name), the inline-values
    pins expecting an anonymous retagged row, and
    `TestRetagInlineValuesRecordTypeIsCopyOnWriteAndKeepsANamedSourcesName` (a named source
    keeps its name with the renamed fields); `TestFDB_ADeclaredRecordNameSurvivesTheBridge`
    adds `STRUCT RECORD` and `STRUCT foo` under a VALUES nested definition, at top level and
    through a derived table.
24. **One declared name over two shapes.** `TestFDB_OneDeclaredNameOverTwoShapesIsRefused` (the
    same-name spellings at top level, under VALUES nested definitions and through a derived
    table fail XX000; two distinct declared names beside them are structs) and
    `TestFinalizePlanReturnsTheNameClashAndKeepsTheMapForNoMessageForm` (the compile error is
    returned; a type with no message form keeps its map and its neighbour is stamped).
25. **The array of named records under a VALUES definition.** Two tuples in
    `TestFDB_ADeclaredRecordNameSurvivesTheBridge`, at top level and through a derived table.
26. **The synthetic namespace is unreachable.**
    `TestSyntheticTypeNamesAreUnreachableFromAnyIdentifier` (seven identifiers escape outside it,
    an identifier that would have to start with it is refused, the `__type$` witness runs in both
    orders under distinct names, the genuine declared clash still errors).
16. **Bodies the walk serves.** `cte_published_row_names.yaml` §7–§8: a WHERE over a star join
    and over a union of star joins, both spellings; the named-STRUCT join body, both spellings.

## Adjacent finding, surfaced by the § Fix D probe — fixed here (§ Fix F)

Probing the gate's operand forms turned up a shape that failed identically at the merge-base,
with no union in it:

```sql
WITH u AS (SELECT g, SUM(v * 2) AS s FROM ga GROUP BY g)
SELECT c.w, u.s FROM u, c WHERE u.g = c.id
-- column "S" resolves against source "U", which declares no column order to bind a plan-time ordinal
```

The same body as a derived table (`FROM (SELECT g, SUM(v * 2) AS s …) u, c`) returned the
correct rows, and the same CTE with a bare-column operand (`SUM(v) AS s`) worked. The r2 text
named this as a separate change; codex's review held it to the DFS rule — a defect surfaced by
a fix is fixed in the same change — and that is right, so it is § Fix F below.

**Mechanism.** A CTE's scope source is built by `buildCTEColumnSource`
(`embedded/logical_predicate.go`). For a single-table body with aggregates it went straight to
the parse-tree derivation `buildDerivedTableSourceFromAgg`, which types each aggregate from its
argument's *catalog* column; an expression argument has no catalog column, so `S` was published
as `UNKNOWN`, and the enclosing query's ordinal bind — which refuses a row it cannot type —
declined the source. The derived-table path (`buildDerivedTableSourceWithCTEs`) had the right
order all along: build the body and publish its **exact** row first
(`buildExactScopeSourceOrBodyError`, the same call the join-bodied CTE arm makes), and fall back
to the parse-tree derivation only when the exact one has nothing to publish. The CTE aggregate
arm now takes that order. A body that does not build raises its own error, as the join arm's
does.

## Second adjacent finding, surfaced by Graefe's r4 delta — fixed here

Measuring every read path of a published repeated name, Graefe found the one that was loud and
wrong: `SELECT * FROM (SELECT g, SUM(v) AS g FROM ga GROUP BY g) u, c` died `XX000` at the
result set's alignment guard, in both spellings, while the unique-name control answered four
columns. His r5 delta then measured the loud floor r5 had left — an aggregate, a sort, and (as
he found) a WHERE over a unique column of a join body that repeats a bare leaf — and named the
fix: bind the CTE or derived quantifier's edge by the row the plan flows, not by the SQL
labels. Java answers every one of these; all were pre-existing at the merge-base in the
derived-table spelling, and the CTE spelling met them once its body was published rather than
served by the name model.

**Mechanism.** The engine names runtime slots by three rules — a record constructor names a
repeated output by the name-addressability suffix (`G`, `G_2`; Java has no such suffix,
`Type.Record` keeps repeated names and binds every read by ordinal, `Expressions.java:91`,
`LogicalOperator.java:367`); a projection over a join names a repeated bare leaf by its
qualified datum key (`GA.G` beside `G`); a raw positional merge keeps every leg name verbatim —
while the SQL names a reference spells are none of those. Four consumers had let one of the
runtime names, or the SQL name, stand where the other belonged:

1. *The result-set label followed the frozen output schema for an aliased item.*
   `deriveColumnsFromProjection` took the frozen name unconditionally when the item carried a
   user alias, so `SELECT g AS a, g AS a` reported `[A A_2]`; for an unaliased reference it took
   the frozen name unless the same label had appeared at an earlier slot — a heuristic that
   reads a column-list rename of a repeated alias as a dedup. Both arms now ask the structural
   question: the NATURAL schema — the freeze site's own naming rule, `values.ProjectionSlotName`,
   deduplicated by `values.DedupFieldNames` — names the slot, and only a frozen name the natural
   schema does not produce is a rename (`frozenSchemaRenamesSlot`). The heuristic is deleted
   with its test.
2. *The derived leg's ordinal layout stated names the row does not carry.* `derivedOutputColumns`
   re-derived a projection's names by a third rule; with the label fixed, `SELECT *` over the
   repeated-name leg answered `[100 100 100 1]` — the grouping key in both `G` columns — because
   the join seed's baked read of slot 1 declined its ordinal domain (`OrdinalIn`) against a
   layout named `[G G]` for a row typed `[G G_2]`, and fell back to a by-name read of the first
   `G`. The layout takes the exact type's names when it has them — the record constructor's
   names for every slot — and applies `values.DedupFieldNames` otherwise; `mergedRVSequenceDiverges`
   compares the merged display sequence under the same rule, exactly, slot for slot.
3. *The scope stated the SQL labels as the row.* A read bound to the CTE's or derived table's
   quantified object — a WHERE, a sort key, an aggregate key or operand over a unique column —
   minted that object from the scope's columns, and the executor's edge check refused the row
   the plan declared (`edge lookup U: read as RECORD(G,G,W), declared RECORD(GA.G,G,W)`) or found
   no binding at all. The scope source already has the carrier for a row that differs from the
   columns exposed for resolution, `FlowedColumns`; `exactVirtualScopeSource` fills it with the
   exact row's own field names, the resolver takes a column's ordinal from its POSITION in the
   SQL-named list and names the read by the flowed slot (`sourceColumnOrdinal`,
   `resolvedSourceColumnRef`), and every site that installs a registered CTE into a reading scope
   carries the source WHOLE (`cteSourceAs`) instead of rebuilding it from its `Table`. The
   layout is stated only for a body whose row IS a record constructor's — a projection or an
   aggregate at the top, or a union, which flows its first leg's — because the exact type of a
   projection-less join names its fields by the leg-qualified datum keys of the retired row map,
   not by the row the executor's merge flows; a first cut stated it for every body and a WHERE
   over a gathered-unnest star derived table regressed (Graefe, r6). The derived-table join body
   and the union-bodied derived table publish their exact row first, as the aggregate and CTE
   arms do (the union-derived spelling of the repeated-leaf body was refused as the same
   edge-layout mismatch while its CTE spelling answered),
   and the exact type's projection arm applies the constructor's dedup so the two agree on a
   repeated alias (`[G G_2 N]`) — while the label derivation, which is what a source publishes
   for RESOLUTION, keeps the SQL names repeated: one per-slot naming rule
   (`projectionSlotSQLName`) feeds both, and only the type deduplicates. The first cut of the
   dedup let it reach the labels too, and `C."X"` over a body that repeats an aggregate alias
   resolved to one column instead of `42702` (Q56).
4. *The dedup could mint an authored name.* `DedupFieldNames` counted occurrences, so
   `[X X X_2]` became `[X X_2 X_2]` and a lateral unnest over the authored `x_2` could not
   address one column (codex, r5). Every authored name is reserved before a suffix is minted.

Two shapes a first cut of this round broke, both answering before it, are pinned with the
rest (codex, r5): a mixed qualified star in a CTE body, whose parsed name list has the wrong
width for the exact row (`projectionOutputNames` yields nothing for it, and the body's own
labels stand), and an aggregate body over the gathered-unnest shape, which the shape-keyed
decline must not catch (its items live in `aggCols`, `projCols` nil). A grouping key's alias
presence is now recorded by the parser (`groupColAliased`) rather than inferred from a string
comparison — `ga.g AS "GA.G"` is aliased.

All four layers are pinned in `repeated_output_names.yaml` and `cte_published_row_names.yaml`
(§ Test plan 11–12) and as Q57 of the sqldriver probe suite, in both spellings.

## Third adjacent finding, surfaced by @claude's r7 delta — fixed here

@claude asked why golden #25 (`SELECT u."GA.G" AS z FROM u ORDER BY z` over an aliased
grouping key) gained an `InMemorySort` at r7 when r6's plan had none. r6's plan had none because
r6 had LOST the alias (codex's r6 finding): the outer read resolved to the bare `G`, the name
coincided with the grouping key the streaming aggregate already orders by, and the sort was
elided on that coincidence. r7 kept the alias, and the same sort was then kept over an input
already in that order. Graefe measured the same redundant sort on a plain alias (`g AS h`) at
the merge-base: it is pre-existing, and it is every renamed column, not one query.

**Mechanism.** Java derives a map plan's ordering by pulling its child's ordering up through
the map's result value (`OrderingProperty.visitMapPlan` → `Ordering.pullUp`), so `RemoveSortRule`
compares `ORDER BY h` against an ordering that already says `H`. Go resolves ordering
satisfaction the other way round: an order-PRESERVING wrapper (`orderingDelegator`) answers
through its source group, and the request walks down the delegator chain to the member that
provides the order. That walk carried the request UNTRANSLATED through a projection or a map, so
`H` was matched against the source's key `ID` and satisfied only when the two spellings happened
to coincide. The dual of Java's pull-up is a push-down at each reshaping delegator:
`requestedOrderingBelow` restates the request through the wrapper's result value
(`RequestedOrdering.PushDownThroughValue`, the translation
`PushRequestedOrderingThroughProjectionRule` already uses for the constraint) and rebases the
pushed parts from the wrapper's child edge into the source group's current-row space
(`requestedOrderingAtInnerCurrent`). The push-down's upper alias is the root the request's parts
name — the group's current carrier for the constraint the sort rule pushed, the sort's own inner
quantifier for the keys as spelled — because both reach the walk. A part the result value cannot
express (a computed slot) drops, and a request that lost a part is not satisfiable below the
wrapper: the sort stays.

**A wrong answer under it.** The coincidence cut both ways. `SELECT u.id FROM (SELECT v AS id,
id AS v FROM ga) u ORDER BY u.id` — the body SWAPS the names — met the scan's primary key `ID`
by spelling, dropped the sort, and answered the rows in ID order for a sort on V: measured on
SimFDB at the merge-base `36b97f1e9` and at `cd7bdc5ed`, `[30],[10],[20]` over rows (1,30),(2,10),
(3,20), in both spellings. With the request pushed through the projection it is a sort on V,
which nothing provides, and the sort stays. Pinned in both spellings with `plan_contains:
InMemorySort` and rows that DISCRIMINATE — `g AS id, id AS g` over g = (200, 100, 300), not
monotone in id, so ID order and G order differ — beside the elided twin on the other column
(`ordering_through_a_projection.yaml`).

**The constraint never crossed either.** The same root mismatch sat in
`PushRequestedOrderingThroughProjectionRule`: it pushed the constraint through the projection's
result value with the INNER quantifier's alias as the upper alias and without the rebase into
the child's current-row space, so a constraint rooted at the projection's current — which is
how every constraint arrives — failed the push-down's root check and nothing was pushed. `ORDER
BY u.g` over `(SELECT g FROM ga) u` therefore never reached the index on `g`, and `ORDER BY u.id
DESC` never produced a reverse scan, while the same ORDER BY over the base table did both;
every sort through a derived table or CTE was an in-memory sort over a forward scan. The rule
now crosses through `requestedOrderingBelow` too — one translation for the constraint going down
and for the satisfaction walk — and the constraint reaches the scan group
(`TestPushRequestedOrderingThroughProjection_*` expect it in the child's current-row space).
The child cannot act on it yet: `SatisfiesRequestedOrdering` sees no order in any candidate
(for the base query too — the ordered full index scan there comes from `OrderedIndexScanRule`,
whose matcher is a sort DIRECTLY over a scan), because the scan group's one partial match is the
unadjusted LEAF and never climbs to the candidate's `MatchableSortExpression`, whose adjustment
would mint the matched ordering parts (that machinery is ported): `matchWithCandidate` refuses
at `correlatedToEquals`, Go's stand-in for Java's correlation-set equality, which demands zero
node-local correlations on the candidate expression while the candidate's own select reports
the placeholder's parameter alias and its own inner alias — two aliases Java's
`getCorrelatedTo` does not count (measured by Graefe's r9 delta, with both Go-only ordered-scan
rules filtered out). It is the only gate: with the match seeded as `MatchLeafRule` seeds it and
that gate admitted under mutation, the leaf match climbs to the `MatchableSortExpression` and
carries an ordering part (r12; an r11 reading of a second gate was a fixture artifact — a seed
without the MaxMatchMap). The closure is that set equality with those exclusions, after which
both Go-only ordered-scan rules retire. Booked in `TODO.md` ("Ordering through a projection reaches
the child group but not the index"), the refusal pinned by
`TestAdjustMatches_LeafMatchDoesNotClimb`, and
`ordering_through_a_projection.yaml` pinning both halves — the base table taking the index and
the reverse scan, the derived table and CTE still sorting. Making the projection rule loud on a
foreign-rooted constraint found the one pusher that stated its ordering over its own inner
quantifier rather than the child's current row — the group-by rule's synthesized ordering, which
reached the projection under `GROUP BY u.w ORDER BY u.w` as a root no child could act on; it
crosses the same rebase the sort and select pushes cross now.

**Where it applies.** All three delegator walks: `memberSatisfiesOrderingDepth` (satisfaction),
`pinOrderedSpineDepth` (the rule-time pin) and extraction's `rebuildOrderedSpine`, which now
carries the translated request level by level instead of re-deriving it from the sort at every
level. `ImplementSortRule` judges an order-preserving member through the walk
(`memberSatisfiesOrdering`) rather than through the member's own derived ordering, which inherits
the source's keys untranslated. `SortElisionSelector.OrderedChildWinner` takes the requested
ordering; `Planner.OrderedChildWinnerForSort` is the sort-expression entry.

**Measured.** Over the yamsql corpus (`plan_shape.golden`), 16 queries lose an in-memory sort
and none gains one — counted between `cd7bdc5ed` and `56a3df6ed` keyed by fixture file AND SQL
text (`InMemorySort` occurrences in the plan line; an entry index is not a key, a fixture's own
additions renumber it), `#25` among them; the recursive-CTE and FlatMap shapes that keep theirs
keep them. A sort over a projection's computed slot still declines
(`TestSortElisionDeclinesAComputedSlot`). The RFC-201 factory corpus moves too: 42 of 8150
committed scenarios (8150 carry a plan-shape header; the plan census dumps 8060, omitting
candidates with fewer than two TLP renderings) across 10 `single|and(…)` family files change
plan shape (re-blessed with
`factory-rebless-plan-shapes`, which verifies the renderings, schema, setup and frozen rows are
unchanged; machine ledger `retirements/2026-09-05-rfc242-a-sort-is-judged-through-its-source-group.json`
over base commit `36b97f1e9`, prose entry in `RETIREMENT_LEDGER.md`, drift classified by
`factory-plan-census` with no regression class present). Those are not renamed columns: `SELECT * FROM t_rd WHERE c = 1 AND id < 3 ORDER BY c
NULLS LAST, id` now plans as `Fetch(PredicatesFilter(IndexScan(IDX_C, [=])))` with no sort, where
the merge-base sorted a filtered scan. The fetch is an order-preserving wrapper, and judging it
through its source group (the walk) sees the index scan's RICH ordering — C equality-bound, ID
ascending under it — where the wrapper's own inherited plain ordering had lost the bound key.
That is Java's `RemoveSortRule` equality-bound arm answering through the delegator, and it is
order-correct: the index stores (c, id), the residual filter preserves order.

## Folds at r8

- **Quoted, case-distinct labels answered the first column twice** (codex r7 P1, a silent wrong
  answer where the merge-base refused): the resolver took a reference's ordinal by a folded first
  match over the source's labels, so `c."x"` and `c."X"` over `(SELECT foo AS "x", bar AS "X")`
  both read slot 0. `sourceColumnOrdinal` matches the exact spelling first and falls back to a
  folded match only when it is unique, in both of its layouts (`derived_star_visibility.yaml`
  §3).
- **A star body bypassed the star-expansion visibility rules** (codex r7 P1): r7's exact-first
  derivation labelled a projection-less star body by the exact row, which neither shadows an
  outer column under an unnest AS/AT alias nor hides the ephemeral `__ROW_VERSION` pseudo-column
  (Java's `nonEphemeralVisible`). The derived join-body builder takes the same order the CTE arm
  takes — the unnest builder first for a star body, then the exact row unless it carries the
  pseudo-column (`exactStarRowCarriesAnEphemeral`), then the catalog walk — and the CTE join arm
  applies the same pseudo-column decline. The uniqueness gate r7 deleted had declined the
  row-versioned star join by accident (both legs carry `__ROW_VERSION`); this declines it by
  rule (`derived_star_visibility.yaml` §1, `derived_star_row_versions.yaml`).
- **An aliased expression reclassified into a grouping key lost its alias** (codex r7 P1,
  reported by both runs): `v / 10 AS bucket … GROUP BY v / 10` is classified as an expression
  item and turned into a grouping key after GROUP BY parsing, and r7's alias-provenance flag was
  set only on items born as grouping keys. It is set on every item now
  (`derived_star_visibility.yaml` §4).
- **The eighth CTE-install site** (Torvalds): `buildWherePredicateFromCTEScope` installed a CTE
  source without `cteSourceAs`; routed. The named-STRUCT join body, the one class the exact
  derivation declines and the walk still serves, is pinned in both spellings
  (`cte_published_row_names.yaml` §8).
- **Coverage** (@claude, Graefe): the union-bodied derived table's motivating shape (a WHERE and
  a GROUP BY, both spellings, plus the repeated-name read) and the star-union shapes Graefe found
  answering at r7 are pinned (`cte_published_row_names.yaml` §5, §7); the DESC twins pin that the
  order is real (§6); the fixture comment cites RFC-238 §2's `qualifierStrippedLabel` residual
  rather than §7d.

## Folds at r9

- **The swapped-name body** (Graefe, Torvalds): a wrong answer at the merge-base and at
  `cd7bdc5ed` — pinned in both spellings with the sort that must stay (§ Third adjacent finding,
  "A wrong answer under it"; `cte_published_row_names.yaml` §6).
- **The constraint crosses the projection** (Torvalds's booking, half): the projection push rule
  translates through `requestedOrderingBelow` and accepts only a current-rooted constraint; the
  receiving side — the leaf match that never climbs past `correlatedToEquals` — is booked (§ Third
  adjacent finding, "The constraint never crossed either"; `ordering_through_a_projection.yaml`).
- **No folded fallback in `sourceColumnOrdinal`** (Torvalds): a panic before both fallback loops
  over the explaindiff corpus (2736 queries) and the SimFDB probes never fired; the loops are
  gone, both walks match by exact spelling, and the doc above them says so.
- **`groupColAliased` on the promoted expression items** (Torvalds), and the claim narrowed to the
  one item not built from a SELECT-list item — the ORDER-BY-harvested aggregate.
- **Counts with their method** (Torvalds): 16 golden entries lose a sort, by entry index; the
  factory ledger names 8150 scenarios with a plan-shape header and 8060 census dump lines.
- **The row-versioned remainders pinned as negatives** (@claude): `derived_star_row_versions.yaml`
  (0AF00 / XX000) and `TestFDB_DerivedStarRowVersionsWhere` (`edge lookup D`).
- The two new fixtures carry their description above `name:` so `FEATURE_MATRIX.md` shows it;
  the status line's dangling round marker is gone; the sort rule's comment records the
  two-space coverage arms.

## Folds at r10

- **The receiving-side booking, restated** (Graefe, measured): not missing ordering parts but a
  leaf match that never climbs past `correlatedToEquals` (§ Third adjacent finding, "The
  constraint never crossed either"; `TODO.md`; the fixture header). The refusal is pinned by
  `TestAdjustMatches_LeafMatchDoesNotClimb`: a value-index candidate, a
  scan-leaf match, no adjusted twin, and the candidate's select reporting the node-local
  correlations the stand-in refuses on.
- **The swap pin discriminates** (Torvalds): moved to `ordering_through_a_projection.yaml`, whose
  `g` is not monotone in `id`, so the rows differ between the two orders.
- **XX000 pinned where it cannot be credited** (Torvalds): the coverage classifier counts any
  non-0A SQLSTATE as a correct rejection, so the CTE spelling of the row-versioned unnest star is
  pinned in Go (`TestFDB_DerivedStarRowVersionsUnnestCTE`); the yaml keeps the 0AF00 half.
- **A foreign-rooted constraint is loud** at the projection push rule (`call.Fail`), as at the
  select push rule; the rule's history comments are cut to the why; its tests expect the failure
  and assert the pushed root and column without the shared rebase helper. Going loud found the
  group-by push rule stating its synthesized ordering over its own inner quantifier; it is
  rebased into the child's current-row space now (its tests expect that space).
- The alias-provenance doc names its two readers and the items they consult; the lost-sort count
  is keyed by fixture file and SQL text; the fixture header separates the negative pins from the
  permanent positives; the status line is one list; the test plan is numbered in order.

## Folds at r11

- **The group-by rule's PRESERVE branch rebases too** (Graefe required, codex P2): the keys it
  pushes under ANY are stated in the child's current-row space, pinned by
  `TestPushRequestedOrderingThroughGroupBy_PreserveWithKeysPushesCurrentRootedKeys`.
- **The XX000 yaml pin is gone** (Graefe, @claude): the codex run that reviewed r9 executed
  `git restore --worktree .` under the uncommitted r10 edits and reverted them; the removal was
  redone from the record, with the duplicate 0AF00 entry and the stale comment.
- **The pseudo-column is the one of VERSION type** (codex P2): a real `"__ROW_VERSION" STRING`
  column is star-visible and is not it; classifying by name alone declined the body's row and
  `WITH d AS (SELECT * FROM rv, rw) SELECT d.v FROM d WHERE d."__ROW_VERSION" = 'a'` did not
  plan — at the merge-base either. Both spellings, a GROUP BY and an ORDER BY are pinned
  (`derived_star_visibility.yaml` §5).
- **A nested derived table's unaliased qualified output** (pre-existing, surfaced by Graefe's
  probes): `SELECT x.w FROM (SELECT u.w FROM (…) u) x` was 42703 because the derived-of-derived
  scope named the column by its display spelling U.W; it is the bare name now, with a join body,
  a single-table body, the CTE spelling and an aliased control pinned (`cte_published_row_names.yaml`
  §9).
- **The climb pin is named for what it measures** (Torvalds): `TestAdjustMatches_LeafMatchDoesNotClimb`.
  (r11 also read a second gate into it; r12 shows that was a fixture artifact — see Folds at r12.)
- Torvalds's other folds: the projection rule's message states the fact (an outer key can arrive
  foreign-rooted through a select pusher without any pusher having erred); a rootless request's
  silent decline is pinned (`…_NoRootDeclinesSilently`); the alias-provenance doc says
  "every SELECT-list-born item" (r12: "every non-aggregate SELECT-list item"); the indexing in the
  climb pin's failure path (`GetQuantifiers()[0]`, a panic on a quantifier-less parent) is gone.
  The "duplicate BUILD entry" two laps saw is not one: `adjust_match_leaf_climb_test.go` appears
  once in the cascades test target's `srcs` and once in its gazelle-generated `embedsrcs`, as
  every test file in that target does; removing the `srcs` entry fails the build-membership gate.
- **Booked, not fixed:** an IN subquery over an aggregate or DISTINCT body does not translate
  (four shapes, identical at the merge-base; `TODO.md`).

## Folds at r12

- **The climb pin is a sentinel now** (Graefe, Torvalds): seeded through `matchLeafWithCandidate`
  — the MaxMatchMap built, as `MatchLeafRule` always builds it — the leaf match climbs through the
  candidate's select to the `MatchableSortExpression` with an ordering part when
  `correlatedToEquals` is admitted under mutation, and `TestAdjustMatches_LeafMatchDoesNotClimb`
  goes red. r11's seed carried no MaxMatchMap, so that mutation stopped at the select adjuster's
  nil-map check, which r11 read as a second gate refusing the placeholder; Go admits an unbound
  placeholder as Java does. `correlatedToEquals` is the only gate, and the three homes say so.
- **A nested-field projection in a derived body** (pre-existing, Torvalds's probe): `SELECT x.x
  FROM (SELECT t1.w.x FROM t1) x` and the derived-over-derived spelling were refused as a
  projection slot with no resolved Value — the catalog walk looked the bare leaf up as a column,
  failed, and declined the whole resolver. The nested path is decided by its shape before any
  lookup, and a failed lookup of two or more segments goes to the exact derivation, in all three
  arms (`cte_published_row_names.yaml` §10; the two rules as they stand at r14).
- **Two r11 bullets restated as measured** (@claude): the "duplicate BUILD entry" is the test
  file's gazelle-generated `embedsrcs` entry beside its `srcs` entry; the climb pin's failure path
  lost an indexing that could panic, not a `panic(`. The alias-provenance doc says "every
  non-aggregate SELECT-list item" (Torvalds).

## Folds at r13

- **The nested path is decided by its shape** (Torvalds, measured): r12 decided it after the
  catalog walk had looked the leaf up as a column, so `SELECT x.sk FROM (SELECT st2.p.sk FROM
  st2) x` over a table with a top-level STRING `sk` beside the struct column typed the slot as
  that STRING (0AF00 "declared RECORD(SK:STRING?)", 42804 under a WHERE, a raw resolution error
  under an expression — identical at the merge-base). `nestedProjectedPath` strips the body
  source's qualifier and sends two or more remaining segments to the exact derivation before any
  lookup, in the single-table arm, the derived-over-derived arm and the CTE arm; r13 deleted the
  post-lookup branches, which r14 restores beside it (see Folds at r14)
  (`cte_published_row_names.yaml` §11: four spellings beside the top-level homonym).
- **The quoted-dot nested member's label** (codex P2): `SELECT x."a.b" FROM (SELECT tq.s."a.b"
  FROM tq) x` is admitted now and reads the member's value; its label is `b`, over the base
  table and through the derived table alike — RFC-238 §2's declared residual
  (`qualifierStrippedLabel`: a nested member is not a top-level field, so the dot in its name is
  stripped as a qualifier). Pinned as that residual end to end
  (`TestFDB_QuotedDotNestedMemberLabel`: value right, label `b`, red once the residual closes);
  closing it is RFC-238's, not this RFC's.
- The alias-provenance doc says "every SELECT-list item that is not a bare aggregate call".

## Folds at r14

- **The post-lookup net returns, in all three arms** (Torvalds, measured): the shape rule strips
  the body source's qualifier, so with `FROM st2 AS p` and a struct column `p`, `p.co` lost its
  qualifier, the leaf lookup found no top-level `co`, and the arm declined — 0AF00 in the derived
  spellings where r12 answered `[6]`. Java's `lookupNestedField` resolves `P.CO` through the
  attribute `P` when the qualified form `P.P` fails; the Go reading of that is the branch r12 had
  and r13 deleted: after a failed lookup, a reference of two or more segments goes to the exact
  derivation, and a one-segment miss still declines without a body build. Both rules now stand
  in the single-table arm, the derived-over-derived arm and the CTE arm. The CTE arm never had
  the net: its bare projection answered through a later fallback, and a WHERE on the published
  column failed to translate (0AF00, identical at the merge-base) because that fallback publishes
  no typed row. Five spellings pinned beside the top-level control
  (`cte_published_row_names.yaml` §12).
- **A nominal record is published by the exact derivation** (codex P2, chased to its cause): the
  shape rule's unconditional return made a declared-STRUCT nested field (`tn.p.child`, `child` a
  `child_s`) 0AF00 where the leaf lookup had answered — but only when the table happened to carry
  a top-level homonym of the same type. With no homonym the read was 0AF00 at the merge-base too,
  and with a homonym of another type it was refused as a column that does not exist (42703):
  `semanticColumnFromExactType` declined every record not literally named `RECORD`, citing a
  carrier `semantic.Column` does not have — it has one, `StructTypeName`, the field the forward
  bridge (`expr.structColumnType`) reads first, added in the same commit as the gate. The exact
  derivation now publishes a nominal record as `RECORD` carrying its name there, so the round trip
  mints the same named `values.RecordType`; record identity is name-insensitive under both
  equalities (`RecordType.Equals`, the exact canonical form) as Java's `Type.Record.equals` is.
  The fieldless record still declines. Pinned in seven spellings (`cte_published_row_names.yaml`
  §13) and at the bridge (`TestSemanticColumnFromExactTypeCarriesRecordName`).
- The §11 fixture comment says four spellings, which is what it pins.

## Folds at r15

- **The shape rule's decline is final again** (Graefe): r14 made the shape rule's exact route a
  first try that fell through to the leaf lookup on a decline — so a shape-decided path whose
  exact derivation declines would be looked up by its leaf and typed as a top-level homonym,
  the r13 error re-legitimised by a comment. All three arms now return the exact derivation's
  answer unconditionally: a decline is the whole source declining, and every reader of it
  reports the unresolved slot. Measured on the way: every pinned shape-true body succeeds on
  the exact route (§11, §13, the derived-over-derived body: 5 of 5), the alias that names a
  struct column never fires the shape rule (one segment after the strip; the post-lookup net
  alone covers it), and the one leaf Graefe named as still declining — an enum-typed field,
  reachable from Java-authored metadata only — does not: the exact logical derivation types
  an enum field as STRING (the catalog kind `ENUM` bridges forward to STRING) before the
  bridge sees it, so no shape reaches a decline the walk would answer differently (the decline set is wider than the bridge's arm and a NULL literal beside the path reaches it; see Folds at r16). That is the negative result
  pinned on a descriptor-built table with a STRING `color` beside the enum `p.color`
  (`TestDerivedNestedEnumFieldTypesAsStringSoTheShapeRuleNeverDeclines`: the derived and CTE
  spellings plan, the exact row is the one STRING column; it goes red when the exact
  derivation starts carrying enums, naming the homonym shape as the one to pin as a loud
  decline), and at the bridge (`TestSemanticColumnFromExactTypeDeclinesEnum`: an enum has no
  lossless semantic carrier). This also removes Torvalds's r14 nit — a shape-true decline no
  longer rebuilds the identical body through the net.
## Folds at r16

- **The decline set stated as it is** (Graefe, measured): r15's comment and test-plan title said
  the exact derivation's decline is "reached by no shape today". The set is wider than the
  bridge's default arm — `exactVirtualScopeSource` declines on any inexact result type, a width
  disagreement or a label failure — and a shape reaches it: `SELECT x.sk FROM (SELECT t.p.sk,
  NULL AS n FROM t) x` declines (`placeholder type is not exact`). It does not discriminate r14
  from r15 — the NULL slot declines the walk arm too, and the translator refuses `SELECT NULL AS
  n FROM t` at top level with the same 0AF00 — so finality decides no outcome there; the enum is
  the only leaf where it would, and the enum arrives typed STRING. The comment at the arm, the
  pin's comment and test plan 22 now say so.
- **An anonymous record stays anonymous through the bridge** (codex P2, reproduced over FDB):
  r14 admitted a record constructor's row through the exact derivation, and the bridge back
  (`expr.structColumnType`) named every record with no `StructTypeName` by the SQL kind
  `RECORD` — so `SELECT [x.s], [x.q] FROM (SELECT (1 AS lat, 2 AS lon) AS s, (3 AS z) AS q FROM t)
  x` put two different shapes under one descriptor name, the synthesized result descriptor did
  not compile, and the driver handed both array elements back as raw maps where the same two
  shapes at top level, never bridged, are structs. `Type` is the SQL kind, never a name: an empty
  `StructTypeName` now rebuilds an anonymous record, and the proto repository mints a unique
  message name per anonymous shape (Java's `ProtoUtils.uniqueTypeName`, deterministic here).
  The public struct name is unchanged (`publicOrdinalTypeName` renders an anonymous record as
  `RECORD`). Pinned over FDB in three bridged spellings beside two top-level controls
  (`TestFDB_AnonymousRecordsThroughADerivedRowKeepDistinctIdentities`, red under the old
  fallback name) and at the bridge (the anonymous arm of
  `TestSemanticColumnFromExactTypeCarriesRecordName`).
- **The enum-as-STRING typing is booked on its own** (Graefe): `sqlTypeToCascadesType("ENUM")`
  is `TypeString`, so the exact derivation is inexact one layer before RFC-232's carrier gap;
  `TODO.md`, "The exact derivation types an enum field as STRING", pointing at the pin that
  goes red when it closes and at the nullable-element entry beside it.
- **A table with a fieldless nested-message column is unqueryable** (found while measuring
  \@claude's r14 shape — an exact-derivation decline for a reason unrelated to the nested
  path): `expr.structColumnType` turns a fieldless record into UNKNOWN and the flowed row then
  resolves nothing, `SELECT t.sk FROM t` included; Java-authored metadata only, identical at
  the merge-base. `TODO.md`, "A table with a fieldless nested-message column cannot be queried
  at all", with the reproducer and the closure.
## Folds at r17

- **A VALUES row is an anonymous record too** (Graefe and Torvalds, both measured over FDB):
  r16's rule — the kind is never a name — was contradicted one site over. The inline-values
  retag (`retagInlineValuesRecordType`) minted every VALUES row as a record NAMED `RECORD`, the
  bridge laundered that name back to anonymous with a special case, and two VALUES rows of
  different shapes (`SELECT a.w, b.v FROM VALUES ((3, 4)) AS a(w(x, y)), VALUES ((5)) AS
  b(v(z))`) still claimed one descriptor and came back as raw maps — codex's P2 through a
  second door, pre-existing at the merge-base. The retag mints an anonymous record, the bridge
  carries a record's name unconditionally (a record that happens to be named `RECORD` is a
  named record and keeps it), and the VALUES spellings — two shapes at top level and through a
  derived table — join the FDB pin.
  One visible change rides with it: a VALUES row's nested record now reports the synthesized
  anonymous name (`__type__…`, Java's `ProtoUtils.uniqueTypeName` spelling) as a record
  constructor's row already did, where the retag's minted name made it read `RECORD`; the
  inline-values execution pin says so (`TestFDB_InlineValuesExactExecution`).
  codex's P2 on r16 is the same finding from the other side: a struct literal DECLARED with the
  name `RECORD` (`STRUCT RECORD (1 AS lat, 2 AS lon)`) came back under a synthetic `__type__`
  name after the bridge while keeping `RECORD` at top level; the unconditional carry keeps it
  (`TestFDB_ADeclaredRecordNameSurvivesTheBridge`: the derived and CTE spellings and the
  top-level control all report the declared name).
- **The fieldless booking names its second population** (Graefe): the catalog also leaves
  `StructFields` empty for a self-referential message re-entered on the descent path
  (`columnForField`'s recursion stop), so a recursive message column poisons its table the
  same way, and a fieldless `values.RecordType` cannot serve that arm without a carrier that
  tells the two apart. Said in the entry.
## Folds at r18

- **A nested column definition keeps the record's name** (Torvalds, measured; Graefe booked the
  same): r17's retag guard refused any NAMED source, which widened a pre-existing divergence to
  the `RECORD`-named literal — `SELECT A.W FROM VALUES (STRUCT RECORD (3 AS P, 4 AS Q)) AS
  A(W(X, Y))`, accepted at the merge-base, was refused at r17 as a nominal record. Java's
  `TypeUtils.setFieldNames` renames a record's fields and keeps its name
  (`fromFieldsWithName` when named, `fromFields` when not) and never rejects a source. The
  retag now does the same: a named source keeps its name with the renamed fields, an anonymous
  source stays anonymous, and the two-shape collision stays fixed. Pinned at the retag
  (`TestRetagInlineValuesRecordTypeIsCopyOnWriteAndKeepsANamedSourcesName`) and over FDB in
  `TestFDB_ADeclaredRecordNameSurvivesTheBridge` (`STRUCT RECORD` and `STRUCT foo` under a
  nested definition, at top level and through a derived table: the declared name and the
  renamed fields).
## Folds at r19

- **One declared name over two shapes fails loudly** (Torvalds; Graefe measured the same and
  booked it): `SELECT [STRUCT foo (1 AS p)], [STRUCT foo (2 AS p, 3 AS q)] FROM t` — two record
  literals declared with one name and two shapes, bridged through a derived table or not —
  came back as raw maps with no error, because `FinalizePlan` swallowed every descriptor error
  as "a type with no message form" and let the constructors keep their map representation.
  Java's `TypeRepository.build` throws on the duplicate message name
  (`IllegalStateException(DescriptorValidationException)`, TypeRepository.java:100). The clash
  is now refused where it is known exactly — at definition, when a DECLARED name is reached for
  a second shape (`values.DeclaredNameClashError`, `defineRecordLocked`) — `FinalizePlan`
  returns it, and the three planning paths report it as XX000. Every other descriptor failure
  stays what it was, swallowed: a type with no message form, a name protoname cannot escape, and
  a synthesised file that does not validate for any other reason — the last reached by a join row
  carrying one field name twice, which a nested FULL OUTER JOIN produces and answers today.
  Making every compile failure a query failure broke that working query, measured
  (`TestNestedFullOuter_AncestorNullExtensionReachesLeg`). That row is booked (`TODO.md`, "A join
  row that names one field twice leaves its plan's rows unstamped") and pinned at r21.
  Pinned over FDB
  (`TestFDB_OneDeclaredNameOverTwoShapesIsRefused`: the same-name spellings at top level, under
  VALUES nested definitions and through a derived table fail XX000, and two distinct declared
  names beside them are structs; red with the swallow restored) and at the walk
  (`TestFinalizePlanReturnsTheNameClashAndKeepsTheMapForNoMessageForm`). Pre-existing at the
  merge-base for the unbridged spelling; r18 had opened one more spelling to it.
- **The array-of-named-record spelling is pinned** (Graefe): `VALUES ([STRUCT foo (3 AS p, 4 AS
  q)]) AS a(w(x, y))` at top level and through a derived table, in
  `TestFDB_ADeclaredRecordNameSurvivesTheBridge` — the shared retag arm, measured rather than
  inferred.
## Folds at r20

- **The synthetic namespace is unreachable, and the guard is Java's** (Torvalds, measured;
  Graefe named the same non-blocking): r19 refused the clash in the record arm and its comment
  claimed anonymous shapes never take part. They can. `__type$` escapes to exactly `__type__1`
  (protoname preserves a leading `__` and turns `$` into `__1`), the same name the counter
  mints, so with an anonymous record defined first the query was refused as a clash it is not —
  Java names its anonymous types by random UUID and runs it — and with the declared struct
  first the file failed to validate and EVERY record in the plan fell back to a map. Skipping a
  taken name at mint time fixes only the second order. Go now buys by CONSTRUCTION what Java
  gets from randomness: synthetic names live under `__0type__`, and `__0` is an invalid START
  sequence for an identifier while nothing escapes INTO a leading `__0` (the escapes insert
  `__0`/`__1`/`__2`, each starting with an underscore, so an output starting `__0` needs an
  input starting `__0`, which is rejected). The guard moves to `registerLocked`, where Java
  keeps it (`registerTypeToTypeNameMapping` throws `IllegalArgumentException` on a name already
  bound to a different type) so every path that names a type is covered by one check, and it
  now only ever sees a genuine declared-vs-declared clash. Pinned
  (`TestSyntheticTypeNamesAreUnreachableFromAnyIdentifier`: seven identifiers escape outside the
  namespace, an identifier that would have to start with it is refused, the witness runs in both
  orders under distinct names, and the genuine clash still errors).
- **The swallow arm says what it covers** (Torvalds): its comment named only "a type with no
  message form", while a file that fails to validate for another reason returns through the same
  call. Both sites in FinalizePlan say so; a THIRD — RecordConstructorValue.Evaluate's own doc,
  where a reader lands asking why a row is a map — still asserted that every constructor in a
  plan is stamped, and r22 corrects it.
- **The r19 booking was withdrawn here on a FALSE NEGATIVE; r21 restores it.** r20 argued the
  booked row cannot exist because a record constructor disambiguates repeated names, and cited a
  probe reporting the swallow arm entering zero times over the whole `core/embedded` suite. Both
  halves were wrong, and r21 records the correction with the measurements — the probe printed to
  the test binary's stderr, which `go test` discards for a passing package unless `-v` is given.
  Read the r21 fold, not this paragraph.

## Folds at r21

- **The r19 booking is RESTORED, and r20's withdrawal is retracted** (Graefe and codex each
  measured the shape; codex named the reproducer): r20 reasoned that a record constructor
  disambiguates a repeated field name — true of `NewRecordConstructorValue` (`ID`, `ID_2`) and
  FALSE of `NewRawRecordConstructorValue`, which keeps names VERBATIM by design for ordinal-join
  seeds, where two legs of `SELECT * FROM a JOIN b` both carry `ID` and positional access makes
  the duplicate unambiguous. r20 also leaned on a probe reporting the swallow arm entering zero
  times; that was a false negative, because the probe printed to the test binary's stderr and
  `go test` discards a passing package's output without `-v`. Re-run with `-v`, the very query
  r19 named produces the row: `RECORD<ID, S, BID, FOO, ID>` fails descriptor validation on the
  duplicate `ID` — and the damage is not that row: compilation is per-repository, so THREE of the
  four constructors in that plan end up without a descriptor though only ONE repeats a name (r22 measures it). The entry is restored with r19's attribution intact, and the
  reproducer is committed as the pin rather than a probe deleted afterwards
  (`TestFinalizePlanLeavesTheDuplicateNameJoinRowUnstamped`, as r22 re-cut it: the planned FULL OUTER
  JOIN still holds a duplicate-name row, a row repeating NO name lost its descriptor beside it,
  and one constructor is stamped — the last being what proves the plan was baked at all).
  Java refuses the row outright (`normalizeFields` disambiguates INDEXES, not names; two `ID`s
  throw in `computeFieldNameFieldMap`), so the silence is Go's divergence.
- **The both-orders witness was vacuous** (Graefe and codex, measured): the arm declared a record
  named `__type__1`, which escapes to `__type__01` and never collided with the counter under any
  prefix. The witness is `__type$`, which escapes to exactly `__type__1`; the arm uses it now.
  The test's first loop is what went red under the old prefix, and it carried the vacuous arm.
- **The swallow arm enumerates rather than counts** (Torvalds): r20 replaced "only a type with no
  message form" with "TWO failures reach that arm", the same over-claim one layer up — a record
  or field name protoname cannot ESCAPE is a third, and is not a missing message form. The
  comment lists all three and names the shape that reaches the last.
- **`DeclaredNameClashError.Name` is the escaped message name** (both reviewers): its doc said
  "the declared record name" while the guard, like Java's, compares proto names (`a.b` is
  reported as `a__2b`).

## Folds at r22

- **The blast radius is the whole repository, and three durable sentences understated it**
  (Torvalds, measured): a row that names one field twice has a message form, so the failure
  surfaces only when the FILE is compiled — and compilation is per-repository, with the bad
  message left in it. Every type asked for afterwards fails the same way: on the pinned query THREE of the four
  constructors end up without a descriptor though only ONE repeats a name, while the fourth, resolved before the
  bad message was appended, keeps its descriptor, so which rows survive is walk-order dependent. `TODO.md`, the r21 fold and
  the swallow comment each said "three constructors" or "the row"; all three now state the
  population and the order dependence, and the mechanism is pinned where it was found
  (`TestDuplicateFieldNameRowPoisonsTheWholeRepository`, in `values`).
- **The SQL pin baked nothing, and its surviving arm was a tautology** (Graefe measured the first,
  Torvalds the second): the reproducer plans through `PlanRecordQueryWithSubqueries`, which
  returns the plan UNBAKED, so every constructor was trivially unstamped; and "every
  duplicate-name row is unstamped" cannot fail, because such a row can never be stamped. The r21
  mutation that appeared to confirm it only exercised the other arm. The pin now bakes, and asserts what
  discriminates: a COLLATERAL row (repeats no name, lost its descriptor anyway) and the SURVIVOR
  beside it, the survivor being what proves the bake ran — measured red both ways, with the bake
  removed and with the raw constructor deduplicating.

## Folds at r23

- **The absolute was inverted, not removed, and three copies said so** (Graefe and Torvalds,
  both measuring `constructors=4 duplicates=1 unstamped=3 stamped=1`): r22 replaced "every
  constructor in a plan is stamped" with "leaves every constructor in that plan a map" — false in
  the other direction, contradicting the pin's own survivor assertion in the same commit. Three
  homes carried it: `RecordConstructorValue.Evaluate`'s doc (written by r22), the pin's own
  header (untouched since r21, six lines above the assertions that disprove it), and the r22
  fold's description of what the pin asserts — that last inside the bullet claiming to fix
  exactly this, and naming a "control query" r22 had removed. The r21 fold's description was a
  fourth, still calling the pin the tautology r22 re-cut. All four stated three-of-four with the survivor
  and the order dependence after r23 — but only four: r24 found five more sites still carrying
  the map framing, so r23's claim that they were "found by grepping for the claim rather than by
  remembering where it was written" was itself false, and is retracted in the r24 fold.
- **The data loss was never there** (codex P2, measured over FDB): every home said the unstamped
  join row keeps a map "in which the SECOND `ID` overwrites the first". It does not. The plan
  paths that emit a row — `executeProjection`, the flat-map cursor's record-constructor arm,
  `evaluateOrdinalJoinRow` — build a dense positional row field by field, and the result set
  reads those slots by ORDINAL, so both `ID`s arrive with their own values. The map branch of
  `RecordConstructorValue.Evaluate` is not even entered for those constructors (probed: the only
  entries during that query are the INSERT path's). What the poisoning costs is descriptor
  IDENTITY — such a row cannot be handed out as an `api.Struct` — and the title says "leaves its
  plan's rows unstamped" now. TWO claims in this bullet were later refuted and are retracted
  here rather than left standing: the measurement `(1, 1, true)`, which was taken on the
  equal-slot fixture r24 replaced (the pinned shape returns `(1, 2, true)`, `(2, NULL, true)`,
  `(NULL, 1, NULL)`), and the conclusion that the booking is therefore LATENT — r24 measured a
  computed STRUCT coming back a raw map through this plan, which is user-visible. See Folds at
  r24 and r25. The pin is
  `TestFDB_ADuplicateNameJoinRowLosesItsStructTypeNotItsValues`.
- **The third exception's qualifier was dropped** (both reviewers): `Evaluate`'s enumeration said
  "a row whose descriptor cannot VALIDATE" and then "none of the three fails the query" — but a
  declared-name clash is a validation failure that DOES fail the query. The arm carries its "for
  a reason other than a declared-name clash" qualifier again, as `FinalizePlan`'s does.

## Folds at r24

- **The retracted framing survived in five more sites, and r23 claimed otherwise** (Graefe and
  Torvalds, each grepping): r23 removed the data-loss claim from the homes it remembered and
  wrote that it had found them by grepping. It had not. `plan_finalize.go` still said "end up
  maps" three lines above r23's own "costs descriptor IDENTITY rather than data"; `TODO.md` said
  it too, and kept "nothing reads those rows by name" inside the entry r23 retitled; the values
  mechanism pin's header still said "flows as a map" and "fall back to maps" and quoted the
  RETIRED entry title, leaving its closure pointer dangling; and the planner pin's own NAME still
  ended `…JoinRowAMap`. All are swept, the pin is
  `TestFinalizePlanLeavesTheDuplicateNameJoinRowUnstamped`, and the sweep was verified with the
  greps in both reviews rather than asserted.
- **The LATENT classification was wrong** (codex P2, measured over FDB): a computed STRUCT
  selected through the poisoned plan comes back as a raw `map[string]any` where the same CTE read
  without the duplicate-name join returns an `api.Struct` — same values, wrong type, because
  there is no descriptor to present it with. The booking is user-visible, not latent, and says
  so. codex also showed the no-loss half could not discriminate: both `ID` columns were inserted
  as 1 and the join predicate required them equal, so one slot read twice passed as a preserved
  pair. The predicate is `a.id + 1 = c.id` over ids 1 and 2 now, and the pin requires a row whose
  two slots differ. Its scope sentence is corrected too: the loss falls on constructors resolved
  AFTER the bad message, not on every computed row — the root projection was resolved first and
  keeps its descriptor.
- **The negative could go vacuous** (both reviewers): `TestFDB_ADuplicateNameJoinRowStillReturnsEveryField`
  asserted no census, and its sibling asserted one on a DIFFERENT query, so nothing pinned that
  the shape it reads back is still the poisoned one — the day the booking closes it would pass
  silently. Both pins run the same query text now, the planner one asserting the census that is
  the other's precondition, each naming the other.
- Torvalds probed the latent framing and it holds: a stored STRUCT column read through the
  poisoned plan still returns an `api.Struct`, because it carries the STORED descriptor rather
  than a constructor's.

## Folds at r25

- **The refuted classification was still standing in the bullet r24 edited** (Torvalds): r24
  updated that bullet's pin NAME and left above it both the stale measurement `(1, 1, true)` —
  taken on the equal-slot fixture r24 itself replaced — and the sentence "so the booking is
  LATENT, not a wrong answer", which codex had refuted. That is the failure r24 was cut to fix,
  reproduced inside r24; both are retracted in place now, and the greps re-run.
- **The pin asserts values and the whole result, not shapes** (codex, three P2): it accepted any
  non-empty result containing one unequal `ID` pair and never checked `foo`, so a dropped
  null-extended row or an aliased pair stayed green; it accepted ANY map for the poisoned struct,
  including an empty one, and checked the control only for its type; and Torvalds's stored-STRUCT
  measurement had no committed witness. The pin now asserts the exact unordered rows
  `(1,2,true) (2,NULL,true) (NULL,1,NULL)`, the values `X=1 Y=10` in BOTH representations, and a
  stored struct column through the same poisoned join keeping its `api.Struct` and its value —
  which is also what bounds the blast radius to COMPUTED rows.
- **The control is sharpened to isolate the duplicate name** (Torvalds, measured): the shipped
  control removed the join AND the duplicate name together, so it could not tell which caused
  the raw map. Removing only the duplicate name — the same FULL OUTER JOIN topology over
  `(SELECT id AS cid FROM c_md)` — still returns an `api.Struct`, so the attribution is to the
  repeated name rather than to joining. That control is what the pin runs.

## Folds at r26

- **The booking still shipped the retired control** (Graefe and Torvalds, independently): r25's
  own fold declares the blunt control insufficient — it removed the join AND the duplicate name,
  so it could not say which caused the raw map — and the live `TODO.md` entry went on describing
  exactly that control, in two places. It was also false about what the pin runs. The entry
  states the sharp control now: the same FULL OUTER JOIN with only the repeated name removed.
  Third round in a row that a retraction reached the code and missed a durable home; the greps
  r25 re-ran covered the LATENT wording, not this.
- **The stored half had no poison witness** (both reviewers): it ran a THIRD query text with no
  census and no control, so "the damage is confined to computed rows" rested on an assertion
  that stays green if the shape stops being poisoned. Graefe's guard is the one taken: read the
  computed struct and the stored column out of ONE row of the poisoned join — the computed value
  beside it is the witness, and if the plan is ever clean that assertion fails instead of
  passing as a bound. (r27 had to finish the job: the first cut of that guard made the witness
  row repeat TWO names, so the control removed only one of them and could attribute nothing.)
- **The two axes are named apart** (Torvalds): "blast radius" was doing double duty eight lines
  apart — WITHIN a plan the failure spreads across the whole repository, ACROSS row kinds it
  stops at computed ones. Both say which they mean now.
- The planner pin's header said the struct half runs "a different text"; there are two, and it
  names them and says the census speaks for neither.

## Folds at r27

- **The witness repeated TWO names, so the control attributed nothing** (Graefe, Torvalds and
  codex, each measuring it independently): r26's one-statement guard reads a computed struct
  beside a stored one, and both were called `R` — so the join row repeated `R` as well as `ID`,
  and the control, which removes only the repeated `ID`, still ran over a poisoned plan. Graefe
  measured the table: strip the `ID` repeat alone and the computed value is STILL a map; strip
  both and it is a struct. The comment and the booking each claimed the pair "differs only in
  whether a leg repeats `ID`", which was false of the read being performed — a scope sentence
  written from intent again. The CTE names its struct `RR` now, so the witness repeats exactly
  one name and the control removes exactly that; measured red by putting the repeat back.
- **Two more of the same shape, both found by all three**: `computedThroughTheDuplicate` was
  dead — declared, never run, sitting directly above the comment claiming the control matched it,
  which is what made the false sentence read true; and the arbitrary-row read was not removed at
  r26 but MOVED to the control, which returns two non-NULL structs while the assertion names one.
  Both texts are read by one helper now, which requires exactly ONE row carrying both columns and
  fails if a shape ever yields more.

## Folds at r28

- **The control changed the leg's topology too, and only prose said that was inert** (all three
  reviewers, each measuring it): removing the repeated `ID` requires renaming `c_md`'s column,
  and the dialect cannot rename a base column in place, so the control also wraps that leg in a
  derived table — and derived-table projections are descriptor-relevant, which is what the tests
  above this one pin. Two variables, not one. Each reviewer measured the missing arm and got the
  same answer; it is committed now as a third read that keeps the wrapper and restores the repeat
  (`SELECT id AS id` beside the control's `SELECT id AS cid`, differing in the alias and the
  reference it forces) and comes back the same raw map the witness gives. Measured red by
  removing the repeat from it. The
  booking says the wrapper is forced and why the third read is what makes the attribution sound.

## Folds at r29

- **The third read asserted NOT-a-struct where the claim is a SPECIFIC raw map** (Graefe
  required fold; codex the same as a P2): the booking, the RFC and the comment all say the
  wrapper-kept read "still comes back a raw map", and it does — `map[string]any{X:1 Y:10}`,
  the witness's value exactly. The assertion said only `!isStruct`, which an empty map, a
  wrong-valued map or a raw protobuf all satisfy, so the arm showed the wrapper HARMLESS where
  the attribution needs it INERT. It now asserts the map type and both values, as the witness
  does. Measured red by moving one expected value: the mutation was grepped present, the target
  built and ran, and the failure printed the live value — which is what turns the reviewer's
  measurement into a committed one.
- **A superseded sentence survived in the homes the r28 delta did not edit** (Torvalds required
  fold; codex a P3 naming a third copy): r28 corrected the two-variable framing in `TODO.md` and
  in this RFC and left the test's own head comment saying the control "changes only the name — it
  keeps the join", and the constants' shared header saying the witness and control "differ in
  exactly one thing". Both are the claim r28 exists to retire, sitting where a reader arrives
  first. Both now say the control changes the name AND wraps the leg, that the wrapper is forced
  by the rename rather than chosen, and that the third read is what shows it inert — so it is each
  adjacent PAIR that isolates one factor, while the SET varies both, which is how it proves one of
  them inert. (r29 wrote that inverted, saying the set varies one thing; r30 corrects it.)
  The correction was closed the way the
  standing rule asks: a grep of every superseded phrasing across the homes it counted, returning
  zero, reported beside a positive control proving the greps were well-formed — but it counted
  THREE homes and there are four, which is what r31 corrects.

## Folds at r30

- **The sweep ran over the phrasings, not the claim** (Torvalds NAK, required): r29 reported every
  superseded phrasing grepped to zero, and the greps were well-formed — but they were greps for
  `changes only the name` and `differ in exactly one thing`, the two sentences r29 had itself
  written. The CLAIM survived twice in `TODO.md` in prose using neither: "the SAME statement with
  only the repeated name removed" and "an `api.Struct` with only the name removed", the second of
  them in the sentence describing what the test pins, twenty lines below the paragraph r29 did
  correct. This is the standing rule's own failure mode — the copy you forget is the one the next
  reader finds — reached by a sweep that looked correct and measured the wrong thing. r30 rewrites
  the booking body so the wrapper is named wherever the removal is described, and states the
  three-read design once, in full, rather than in fragments a later edit can desynchronise.
- **The set varies two factors; each pair isolates one** (Torvalds required fold; Graefe and codex
  the same, as a nit and a P3): r29's correction overshot into a claim that is false in the other
  direction. Varying only the repeat is what the two-read pair did, and it is what was wrong with
  it. The third read exists precisely to vary the wrapper as well, so that holding each factor
  fixed in turn shows one of them has no effect. Every home r30 had counted says that, and names
  the two pairwise comparisons instead of asserting a one-variable design. (r30 counted three homes
  and there were four; r31 corrects the population, and r32 corrects it again to five.)
- **Two named lookups are not the whole map** (codex P2; Graefe raised it as optional, and noted
  the witness arm had the same gap): `m["X"] == 1 && m["Y"] == 10` passes for `{X:1 Y:10 Z:99}`,
  which is neither the asserted two-field map nor the witness's result. The two raw-map reads now
  assert the map's exact size alongside its values, so a shape regression in either cannot pass as
  a content match. (r30 wrote "BOTH computed reads" and called the arms symmetric; there are THREE
  computed reads and the control was left unsized, which r31 closes.)
- **The booking's wrapping** (@claude nit, carried into r30 by Torvalds's measurement): r29 fixed
  the 156-column line by inserting newlines rather than reflowing, which left 13- and 23-character
  orphans behind and a 153-column line further down that r29 never measured. The block is reflowed
  as a whole, every line at most 96 columns.

## Folds at r31

- **The population was three and is four — and r31 made it five** (Torvalds NAK; Graefe the same,
  non-blocking): r30
  fixed the two copies Torvalds pointed at and swept nothing else, which is the same failure r30
  was itself written to close, one level up. The copy it missed is in `plan_logging_test.go`, the
  census pin — not an obscure file: the FDB read names it as this test's precondition and it names
  the FDB read back, so the sweep cross-referenced the file it did not open. What makes this the
  root cause rather than a fourth copy is that every home count written in r29 and r30 says THREE
  homes. A sweep is only as good as the population it enumerates, and an unstated or wrong
  population is the failure the standing rule on scoping counts names directly. r31 corrects the
  number where it is asserted and adds the wrapper clause where the census pin describes the
  control. It also ADDS a home — the shared constant below — and did not re-count, which is what
  r32 fixes. The population at r32 is five: `TODO.md`, this RFC, the FDB read, the census pin, and
  `queryfixtures.go`. Enumerated, not counted, so the next reader can check it —
  `git grep -l -E 'DuplicateNameJoinRow|duplicateNameJoinQuery|DuplicateNameJoinQuery'` returns
  those five plus `plan_finalize.go`. r32 wrote that `plan_finalize.go` "carries no claim to keep
  in step", which is false and is corrected at r33: it carries no CONTROL claim, and it states the
  CENSUS claim in full. Those are two different claims with two different populations, and one
  list was being used for both — see the r33 fold.
- **Three computed reads, two of them sized** (Torvalds NAK): r30's fold said BOTH computed reads
  now assert their size and called the arms symmetric. There are three. The control read — the
  only one carrying the POSITIVE claim, that removing the repeat yields a struct with the same
  values — still looped two `AttributeByName` calls, so a struct carrying a third attribute passed
  it. That is exactly codex's r30 P2, closed on the two raw-map arms and left open on the arm that
  matters most. It now asserts `AttributeCount() == 2`, measured red with the mutation grepped
  present and the failure printing the live count.
- **The shared query text is shared, not copied** (Graefe carry-forward, pre-existing): the census
  is a statement about the query that produced the plan, so it is worth nothing if the FDB read's
  text drifts from it — and the two texts sat in two packages with a comment asking the next editor
  to keep them identical. Prose cannot hold an invariant across a package boundary. Both now read
  one `queryfixtures.DuplicateNameJoinQuery`, and the comment says the compiler holds them together
  rather than asking the reader to. Proven consumed on both sides: renaming the constant produces
  an undefined-symbol build failure in each package, read one at a time because the build stops at
  the first.

## Folds at r32

- **The shared constant re-armed the absolute r22 closed** (Graefe NAK and Torvalds NAK, the same
  sentence, measured independently): `queryfixtures.go` said a duplicate-name row's descriptor
  cannot validate "so the plan's record constructors are left unstamped and a computed STRUCT read
  through the plan comes back as a raw map". Neither half is scoped. The census is three of four
  with a survivor — the importing test asserts that survivor and fails if nothing survived — and
  the damage is bounded by walk order, reaching only constructors resolved after the bad message.
  An unscoped absolute here is worse than in the prose homes, because this file is what a reader
  of either package now opens first. Both halves are scoped, and the survivor is named as
  something the census asserts rather than a rounding error.
- **r31 added a home and did not re-count** (both reviewers): the round whose finding was "the
  population is four, not three" shipped a fifth in the same commit. Counting is the failure mode
  itself — a number goes stale silently and cannot be checked — so r32 ENUMERATES the five homes
  and prints the grep that reproduces the list, including the two files the grep returns that
  carry no claim and why.
- **The replaced prose was stacked, not deleted** (Graefe, non-blocking): the comment asking the
  next editor to keep two copies identical sat directly above the constant that made that
  impossible. Two comments, one obsolete, the obsolete one first. Merged into one that records why
  the text is shared.
- **The package's test-only-ness is a BUILD invariant, not a directory name** (@claude and
  Torvalds, both non-blocking): the new library held a SQL fixture and nothing stopped a future
  production target under `pkg/relational` from depending on it and shipping that text into a real
  binary. Its test-only-ness rested on the directory name and the doc comment — which is the same
  "prose cannot hold an invariant across a boundary" argument this round makes about the query
  text, one level up. `testonly = True` makes it the build's job. Measured: pointing a production
  `go_library` at it fails with "non-test target depends on testonly target and doesn't have
  testonly attribute set". The first attempt at that measurement was malformed and failed on a
  duplicate `deps` keyword instead — a build failure that proves nothing, which is why the
  well-formed rerun and its actual message are what is recorded here. The repo uses `testonly`
  nowhere else, so this breaks no convention; gazelle preserves it under a `# keep` comment.
- **One retraction convention** (Graefe, non-blocking): r29's fold was corrected in place with a
  note saying it had been wrong, and r30's was left standing while its claims were superseded.
  r30's is now corrected the same way. A fold section is a record of what a round said, so a
  superseded claim in it is retracted in place, never silently rewritten and never left.

## Folds at r33

- **The shared constant described a plan that cannot exhibit the claim** (Torvalds NAK): its doc
  said a computed STRUCT through this query comes back a raw map while a stored struct column
  "through the same plan" survives. The query is `SELECT a.id, c.id, d.foo` — no struct column,
  computed or stored, so neither half can be read off it. The struct cost is measured by three
  other texts in the same FDB test, which are not shared and do not belong on this constant; the
  shared text's FDB half is the row-arrival read. The doc further claimed the census is "the value
  read's precondition" while the census pin states the opposite in as many words — that it asserts
  nothing about the struct texts. Two enumerated homes disagreeing is worse than one being wrong,
  and the wrong one was the file a reader of either package opens first. The doc now says what
  this text carries and, explicitly, what it does not.
- **One list was serving two claims** (Graefe NAK): r32 enumerated the homes of the CONTROL claim
  and then used that list to exempt `plan_finalize.go` as carrying "no claim to keep in step". It
  carries no control claim and states the CENSUS claim in full, so the enumeration told a future
  sweeper of "three of four" not to check the one file that states it — the same failure as a
  wrong count, wearing an enumeration's clothes. Enumerating fixes a population only once the
  population is tied to a specific claim. Both are now named: the control claim lives in five
  files, the census claim in six — this RFC, `TODO.md`, `plan_logging_test.go`, `queryfixtures.go`,
  `plan_finalize.go` and `values.go`. (r33 put the control claim at five and included
  `queryfixtures.go`, which r33's own edit had just emptied of it; r34 re-verifies after the edit
  and enumerates four: this RFC, `TODO.md`, `anonymous_record_identity_fdb_test.go` and
  `plan_logging_test.go`. A population labelled by claim has to be re-derived after any edit that
  changes what a file claims.) Other files state the walk-order half WITHOUT a number and so
  cannot go stale with the count; r34 tried to enumerate them as "two" and was wrong by at least
  two, so r35 stops counting them at all. That second population is not what the guard governs,
  and carrying a count of it inside a message about the first is how both of the last two
  miscounts happened.
- **"Three of four" was prose no gate pinned** (Torvalds, filed non-blocking, taken as blocking):
  the census asserts floors — at least four constructors, more unstamped than repeat a name, at
  least one survivor — which is right for the INVARIANT and cannot keep a MEASUREMENT true. A
  fifth constructor leaves every floor green while all six sentences go stale. The exact shape is
  now asserted beside the floors as an expiry condition, and its failure message names the six
  files to re-measure and says not to relax the guard. Measured red by moving the expected count;
  the failure prints the live measurement, which is also what confirms the six sentences today.
- **A seventh unscoped absolute, found by sweeping rather than by a reviewer**
  (`record_constructor_message.go`): "the plan-time bake stamps every constructor in the plan from
  ONE repository" is the r22 shape exactly, and predates this branch, which is why five rounds of
  sweeping over this finding's own homes never reached it. The claim is true of the path that code
  is on — it runs only for a stamped constructor, by construction — and false as stated over a
  plan. It now says which, and why the distinction is load-bearing there.

## Folds at r34

- **The expiry guard counted a narrower population than the bake** (codex P2): r33 added an
  exact-shape assertion so "three of four" could not rot behind a floor, and counted it with a
  traversal written in the test — result values and child edges. `FinalizePlan` reaches further:
  projection lists, predicates, grouping keys, scan comparands, defaults, and structural plan
  fields the plan walk never descends into. A constructor appearing in any of those would leave
  the guard green while the population changed, which is the guard failing OPEN on the one thing
  it exists to catch. The stamper's traversal is now exported and the census counts over it, so
  the two cannot diverge — the same reason the aggregate-index and covering-index arms recurse
  rather than re-listing their wrapped plan's fields. Under the wider walk the measurement is
  still four and three, so nothing that was written was wrong; it simply was not being checked.
- **A stamped parent over an unstamped child fails the query** (Torvalds NAK): r33's comment said
  the surrounding code is "only on the stamped path by construction" because Evaluate calls
  buildRecordMessage with a non-nil descriptor. That establishes the PARENT's stampedness and the
  sentence was about the CHILD. They are independent: a parent's descriptor comes from its own
  inferred type, which already carries the child's shape, and the plan walk is pre-order, so a
  poisoning between the two leaves exactly that pair. It matters because the pair does NOT degrade
  the way every home describes — a stamped parent builds a message, the child hands it a map, and
  the map is refused, so the query FAILS rather than answering in a weaker type. Pinned by
  `TestAStampedParentWithAnUnstampedChildFailsTheQuery` and booked. Whether SQL can build the pair
  is open and is written as open: the shape is constructible over the values API and, r33 wrote,
  no query was known to produce it — REFUTED at r36, where a reviewer supplied one and it is now
  committed as a pin, and the booking says to settle it before closing rather than to assume it.
- **"Their rows are maps" is too broad** (codex P2): on the pinned plan the join and flat-map
  result constructors never run Evaluate — the emitting paths build dense positional rows read by
  ordinal — so only a constructor whose Evaluate fallback actually runs hands back a name-keyed
  map. Written unscoped it restores the map framing r22 retired, in a fold written to scope
  something else.
- **Two more miscounts, both inside the fold that exists to stop them** (Graefe and codex on the
  first, Graefe on the second): the guard's introducing comment said the measurement is quoted in
  FIVE places eight lines above its own fatal naming six; and the five-file "control claim" list
  included `queryfixtures.go` after r33's own edit had removed that claim from it. The second is
  the sharper lesson: a population labelled by CLAIM must be re-derived after any edit that
  changes what a file claims, and r33 changed one and re-used the old list in the same commit.

## Folds at r35

- **The exported walk promised the bake's population and did not prune where the bake prunes**
  (Graefe NAK, Torvalds NAK, codex P2 — one defect, three independent measurements): the bake
  stops at `feedsAWrite` before a node's values and before its children, because a plan feeding
  an INSERT, UPDATE, DELETE or temp-table insert has its row shape fixed by the TARGET's declared
  descriptor rather than the constructor's inferred type. The exported walk had no such guard, so
  its first sentence was false for every plan carrying a DML node, and false for the whole
  subtree. Both reviewers probed it: one constructor visited under an insert root, zero stamped.
  This is the scope-claim failure in its purest form — the sentence was written by describing the
  code just added, in a function whose ENTIRE justification is that two populations cannot
  diverge, shipping already diverged. The fix is not the missing line: `FinalizePlan` now IS this
  walk, so there is no second traversal to keep in agreement, and the prune lives in the one
  place. `TestTheCensusWalkPrunesWriteFedSubtreesAsTheBakeDoes` asserts both directions — the
  write-fed constructor is not visited, and a read-only plan's constructor IS, so the first
  assertion cannot be satisfied by a walk that visits nothing — and was measured red by deleting
  the prune, reproducing the reviewers' probe exactly.
- **The reachability question was foreclosed, not open** (Graefe NAK; Torvalds asked what the
  negative was searched over): r34 wrote that a poisoning "in between" a parent and its child
  leaves the failing pair, and there is no in-between. `WalkValue` visits a parent immediately
  before its children, and the parent's descriptor is synthesised from its own type, which
  CONTAINS the child's — so a stamped parent means the child's message was already compiled.
  Parent stamped implies child stamped for a DIRECT field child, and r35 concluded from that
  that the pair was reachable only by calling `SetMessageDescriptor` directly, and wrote the
  route closed. That conclusion is REFUTED at r36: a type-changing wrapper between parent and
  child defeats containment, and `SELECT ([(1 AS "$lead"), (2 AS A)] AS CH) FROM t` reaches the
  pair from plain SQL. The two facts r35 proved are true and remain pinned by
  `TestTheBakeStampsAParentAndItsChildTogetherOrNeither`; the inference from them to unreachability
  was not, and is corrected here rather than left standing forty lines above its retraction.
- **Three smaller ones from Torvalds**: the no-number population was named as TWO and is at least
  four, one of the extras added by the same commit that counted it — r35 stops counting that
  population entirely, since it is not what the guard governs and carrying its count inside a
  message about a different population is how both recent miscounts happened. Two helper doc
  comments still opened "stamps" after the emit/stamp split. And the mixed-pair pin matched
  `cannot store`, which the scalar arm emits too, so a scalar refusal would have satisfied a test
  whose message claims the message-field one; it now matches `in message field`.

## Folds at r36

- **The unreachability conclusion was false, and a reviewer's SQL proved it** (codex P2): r35
  reasoned from two facts about a parent and its DIRECT field child — the walk visits them
  contiguously, and the parent's type contains the child's — to a general claim that the bake
  never leaves a stamped parent over an unstamped child. Both facts are true; the inference is
  not, because a type-changing WRAPPER between the two makes the parent's type carry the
  wrapper's TARGET shape instead of the constructor underneath it. Array unification inserts
  exactly such a promotion for an array literal whose elements have different record shapes, and
  the resulting query does not answer at all. This is the general lesson of the whole PR arriving
  at its own subject: a claim written from the code in front of you, checked by a test built the
  same way — the pin wired the child in as a direct field value, making the implication true by
  construction. r36 retracts the claim in each home rather than rewriting it away, narrows the
  bake pin to the case it proves, and books the defect with the reproducer committed. It is
  pre-existing at the merge-base, measured, which is why it is booked and not fixed inside a PR
  about something else.
- **The unreachability pin was vacuous, and its own mutation proved it** (Torvalds NAK): r35
  asserted that the bake never leaves a stamped parent over an unstamped child by checking that
  the mixed pair does not appear. A poisoned repository satisfies that by stamping NOTHING — the
  implication had a false antecedent in the only arm that could have exercised it, and no
  assertion said otherwise. The demonstration is the part worth keeping: making `WalkValue`
  post-order violates the first fact the test's own comment calls load-bearing, and the test
  stayed green, while the booking and this RFC read CLOSED on its strength. That is the
  empty-set green wearing its documentation face — a comment cannot fail. r36 drives each fact
  instead of narrating it: CONTAINMENT is asserted by looking the child's type up in the
  repository that just synthesised its parent and requiring the parent's own `CH` field message
  back, so a second message for the child reddens; ADJACENCY is asserted on the visit order
  itself, so post-order reddens. The two states the bake can reach are then asserted positively —
  both stamped, or both unstamped — because "the mixed pair is absent" is also true of nothing
  at all. Each arm measured red by a mutation aimed at it alone. (r36 wrote "the states the bake
  can reach"; they are the states that FIXTURE reaches, corrected at r37. And its adjacency arm
  decides a FIRST-field child only, narrowed at r38.)
- **The refactor orphaned two production doc comments** (Graefe required fold): deleting the plan
  walk and the value walk left their godoc headings on the functions that followed them, so
  `forEachNodeLocalValue` was documented as walking a plan DAG with a seen-set it does not have,
  and `stampRecordConstructor` as walking a value tree when it walks nothing. The second carried
  the load-bearing sentence about the walk continuing THROUGH a stamped constructor — the
  containment property this round pins — detached from `ForEachPlanRecordConstructor`, the only
  function that implements it. It now sits there, beside the test that pins it. r35's fold bullet
  claimed stale doc verbs were fixed; it fixed the two it remembered and created two more in the
  same edit, which is the sweep failure of this PR in miniature, on the round that names it.
- **"Pins both directions" over-claimed** (Graefe): the poisoned repository is poisoned globally
  before the walk, so it exercises neither contiguity nor containment — it shows the consequence
  over the states available, which is all that arm can do. The homes now say what each arm
  actually drives.

## Folds at r37

- **The retraction was incomplete, in this RFC's own folds** (Torvalds NAK): r36 retracted the
  unreachability claim in the code and the booking and left two present-tense assertions of it
  in the r33 and r35 fold sections — "no query is known to produce it" (a query is now known and
  committed) and "the pair is reachable only by calling SetMessageDescriptor directly… the route
  is closed", forty lines above the bullet retracting it. Both corrected in place, which is the
  convention r31 established for this document. The pattern is worth naming rather than just
  fixing: this is the third round here whose sweep covered the copies its author had written and
  missed the ones it had merely read, and it happened on the round that exists to record exactly
  that failure.
- **The control varied two things** (Torvalds NAK, measured): the reproducer's counterweight
  dropped from two array elements to one, so it varied element COUNT as well as the shape
  difference that forces the promotion — leaving "two-element record arrays fail" as an
  unexcluded explanation. The one-variable control is two elements of the SAME shape, and it
  answers. That is the same defect this PR spent r27 through r32 closing on the duplicate-name
  pair, reappearing in the first control written after it.
- **"The states the bake can be in" over-enumerated** (Graefe): the two arms are the states the
  FIXTURE reaches. What they exclude is the harmful ordering, a stamped parent over an unstamped
  child; the reverse pairing is out of reach only because the repository's poisoning is sticky,
  which nothing there asserts. Said that way now.
- **Java settles the closure fork** (Graefe, folded into the booking): when the target is a
  record, Java fetches the target's message descriptor from the type repository and calls
  `MessageHelpers.coerceObject` with a coercion trie carried on the PromoteValue itself, so the
  target's message is built AT EVALUATION. It never stamps the wrapped child with the target's
  descriptor — wrong by construction, since the two shapes differ, which is why the promotion
  exists. Go's value-path PromoteValue carries no trie and passes a record child through
  unchanged. The port unit is named in the booking so the next reader does not re-derive it.

## Folds at r38

- **The mechanism was wrong, and three gates refuted it independently** (Torvalds NAK, Graefe
  NAK, codex P2): r36 booked "a type-changing wrapper leaves a stamped parent over an unstamped
  child" and r37 wrote a control around it. Both are false as stated. A wrapper over
  synthesisable names stamps everything and answers. The defect needs a SECOND factor the
  booking never named: a child whose own type cannot be synthesised at all, here a field name
  starting with `$`, which protobuf will not carry. Alone that is the ordinary cost this RFC
  has documented throughout — the parent's synthesis fails too, everything degrades to maps
  together, and the query answers in a weaker type. Only the conjunction fails: the wrapper
  hides the unsynthesisable child from the parent's type, so the parent stamps alone and then
  refuses the map its child hands back. Measured as a 2x2 over real SQL and committed as one,
  each cell asserted, values included. (r38's conjunction is itself refuted at r39 — the same bad
  name on BOTH elements has both factors and ANSWERS, because the target keeps the name — and the
  whole site-based framing is refuted at r40, where the same literals give four different
  outcomes at four sites. Read the table, not this bullet.)
- **The control varied two factors again** (all three gates): r37's control removed the shape
  difference AND the unsynthesisable name in one edit, so its success supported nothing. This is
  the same defect the duplicate-name pin took r27 through r32 to close, reappearing in the first
  control written after it — which is worth recording as the shape of the failure rather than
  as one more instance: a control is built by removing what you BELIEVE is the cause, so it
  inherits whatever the belief got wrong, and it passes either way.
- **A third live home of the refuted pre-order reasoning** (Torvalds NAK): the values-package
  test still said a duplicate-name row "anywhere in between" poisons the repository, the exact
  route foreclosed at r35 — in a file no round had named, four rounds after the first
  retraction. Corrected to what is now measured.
- **The booking's published grep could not run** (Torvalds): `git grep -ln 'A|B'` is BRE, so the
  `|` is literal and the command returns nothing. The conclusion it supported was right; the
  command a reader would paste was not. Now `-lnE`, and re-run to confirm it returns the three
  files the booking names.
- **The adjacency arm decides only a first-field child** (codex P2): a child in a later field has
  the preceding fields' subtrees between it and its parent, so `childAt == parentAt+1` is not a
  claim about direct children generally. The comment now says which case the fixture decides and
  that a poisoning inside a preceding subtree reaches the harmful pair by a second route.
- **Two Java details that decide where the port lands** (Graefe): the promote is injected per
  ELEMENT, so the target is the element record and an "array is promoted" reading aims the port
  at the wrong node; and Java's coercion consumes a MESSAGE the child already built, so the port
  unit is the coercion trie AND the registration model, not the trie alone.

## Folds at r39

- **The mechanism was refuted a second time, and is now a table rather than a claim**
  (Torvalds NAK, measured both ways): r38's conjunction is neither necessary nor sufficient.
  `[(1 AS "$lead"), (2.5 AS "$lead")]` satisfies both stated halves and ANSWERS — the same bad
  name on both elements survives into the promotion target, so the parent cannot synthesise
  either and everything degrades together. `[(1 AS A), (2.5 AS A)]` satisfies neither and FAILS,
  for an unrelated reason. The missing condition is that the target must itself be
  SYNTHESISABLE: unification anonymises disagreeing fields, and that erasure is what leaves the
  parent stampable over a child that is not. Two rounds in a row wrote a mechanism, built a
  control around it, and had it refuted by a row nobody had run — which is the argument for
  what r39 does instead. The pin is a table of texts and outcomes (nine rows at r39, more
  since — r41 stops counting them, because a count in a summary of a table is one more thing to
  go stale); the prose is
  downstream of it and says so. A control is built by removing what you BELIEVE is the cause, so
  it inherits whatever the belief got wrong and passes either way; a table has no belief in it.
- **A second, separate defect** (Torvalds, found while varying types rather than names):
  unifying record literals of differing NUMERIC WIDTH fails descriptor synthesis outright —
  `field number 1 is int32 in the source but double in the target` — with every field name one
  protobuf will carry and no map anywhere in it. Identical at the merge-base, so pre-existing.
  Booked on its own, pinned as the table's last row asserting that error specifically so it
  cannot be satisfied by the other defect's failure, and pointed at the same missing machinery:
  Java's promotion trie builds a coercion per field instead of requiring source and target field
  types to agree.
- **A third home of two claims this PR had already corrected twice** (@claude NAK):
  `record_constructor_message.go` still carried the unqualified adjacency sentence and the
  "states the bake can reach" over-enumeration, in the round that corrected both in its two
  sibling files. Fixed there, and the r36 fold entry that first wrote them now carries the two
  in-place corrections rather than leaving a reader to find them four sections later. @claude's
  other ground — the neither-factor cell deleted without replacement — is the same one codex
  filed and is closed by the table above.
- **The stamping predicate is pinned without Docker** (Graefe required fold): the SQL table
  observes outcomes and skips entirely without a container, so the claim that a bad-named record
  cannot be stamped rested on a test that may never run. Three rows in the values package now
  assert it directly — the bad-named record refused, a record CONTAINING it refused (the
  containment that makes the ordinary case degrade safely), and a record containing an ARRAY of
  an ANONYMOUS record granted one, which is the erasure the failing case needs.
- **Three assertion gaps in the table** (codex P2 x3): the neither-factor cell was claimed as
  measured in three homes and had been DELETED by r38's rewrite, so a proof already green was
  thrown away; the struct rows asserted only "not nil and not a map", which a scalar or a raw
  protobuf also satisfies; and the map rows compared values via `fmt.Sprint`, so a value arriving
  as the STRING "1" would have passed as "values intact". All three closed, and the per-element
  expected values are now written out per row rather than derived from the index, because a
  derived expectation is how a wrong value passes unnoticed — which is how the first draft of
  this very table went red against 2.5.

## Folds at r40

- **A third refutation, from the dimension the table held fixed** (Torvalds NAK): all nine rows
  wrapped the array in a record. Dropping that wrapper makes the SAME array ANSWER, with one
  element a message and one a raw map — a ragged array nothing books, and worse than the failure
  because it is silent. The same two literals through a CASE coerce and answer cleanly; through
  a UNION they draw a loud 42F65. So the outcome is not a property of the literal, and the
  three-condition account was describing one site as if it were the mechanism. The table now
  covers all four, and the head comment says what the rows show and stops there.
- **The second booking named the wrong site** (Graefe NAK, measured): the numeric-width failure
  is not a descriptor-synthesis refusal. Both descriptors synthesise — the error names them
  both, which it could not otherwise — and the guard that refuses is `copyFieldsByNumber`'s
  kind check at EVALUATION. `cannot synthesise a protobuf descriptor for` is `ProtoTypeError`'s
  stock prefix, and reading it as a statement of where the failure happened is what produced the
  wrong entry. The consequence matters more than the correction: the two bookings are one site
  and one cause, a stamped parent handed a child the promote never coerced, so the trie closes
  both arms rather than one. They stay two work items and say so.
- **A redundant condition, and an unmeasured one** (Graefe): "a promotion sits between them" is
  entailed by an unsynthesisable child plus a synthesisable target, so the account has two
  independent conditions rather than three. And the erasure that produces the synthesisable
  target was still prose: the type-level pin hand-built an anonymous record and called it what
  unification produces. It now calls the unifier and asserts both directions — the name erased
  when the two disagree, kept when they agree — which is the whole account in one pair of rows.
- **The closure sentence is contradicted by its own new row** (Torvalds): the booking said Go's
  PromoteValue passes a record child through unchanged. The CASE row coerces, so something on
  that path does not. Written as an open question to answer before the port rather than left as
  a claim, because a port aimed at the wrong node is the expensive kind of wrong.
- **Four assertion gaps codex found in the same pass**: the success rows checked only the outer
  carrier, so a struct with an empty `CH` or with the element's original field name still
  passed — they now walk to the leaves and assert NAMES as well as numbers, which is what makes
  the anonymisation visible end-to-end (`_0` where the element wrote `A`). The type-level rows
  matched only `$lead`, which the error's rendered type already contains, so any unrelated
  refusal for that type satisfied them; they now match the invalid-field-name REASON. The
  booking claimed a differing-name width witness the table did not run; it runs now. And two
  homes still carried the refuted two-factor sentence in the present tense; both corrected in
  place. One of the leaf assertions also caught something nobody had noticed: through a CASE the
  branch's single-field record does not survive as a nested record, it arrives as the value
  under the outer alias. Recorded as measured rather than explained.

## Folds at r41

- **Two of the four sites do not depend on the bad name** (Torvalds NAK, measured; Graefe the
  same for the CASE site): r40 presented four sites as four outcomes of one defect. Running the
  controls kills that. A UNION of two records with SYNTHESISABLE but disagreeing names fails
  identically to the bad-name one, and with agreeing names it answers — so that site refuses the
  anonymised target itself, and is a legal union Go will not run. It is now its own booking. The
  CASE site is vacuous: every variant returns the same value, whose single leaf is a bare number
  under the outer alias, so nothing there measures a record being coerced. Both restated as
  measured, with their controls committed beside them.
- **A vacuous row was carrying a load-bearing conclusion** (Graefe): from that CASE row r40 wrote
  "something on that path already does what the array path does not — find out what before
  porting anything", and put it in the booking's closure. It aimed the next engineer away from
  the element promote, which is exactly the node Java coerces at. Removing it does not weaken
  the trie closure, it removes the only evidence against it.
- **The leaf walker decided what it was meant to measure** (Torvalds): its default arm silently
  dropped every leaf it could not read as a number, so an unexpected NULL or string was
  invisible; and a row that declared no leaf expectation passed vacuously, since comparing two
  empty slices succeeds. Both closed — unreadable leaves are now reported, and a struct row
  without an expectation is an error — in the file whose own comment is about false greens.
- **A missing diagonal and two stale counts** (Torvalds and Graefe): the unification pin tested
  names-disagree with types-agree and names-agree with types-disagree, and not the cell the
  table's newest row sits on. Added. And two homes said the table has twelve rows when it had
  thirteen, in the same clause that told the reader to trust the table over the summary. r41
  stops quoting a count at all: a number in a summary of a table is one more thing to go stale,
  and this one had already done it twice.

## Folds at r42

- **Field count was the untried dimension, and it reverses a withdrawal** (Torvalds NAK): a CASE
  branch with TWO fields coerces a record and anonymises the disagreeing one to `_1`. r41 read the
  ONE-field row, saw a bare leaf under the outer alias, concluded no record is ever coerced there,
  and on that basis deleted "something on that path already does what the array path does not —
  find out what before porting". That sentence was right. It is restored, scoped to what the rows
  now show, and the flattening of a single-field branch is recorded as unexplained rather than as
  evidence for anything. Withdrawing a claim needs the same standard as making one, and r41
  applied a lower one.
- **The union refusal is about an ANONYMOUS FIELD, not about records** (Torvalds NAK): agreeing
  names promote, including across differing WIDTHS, both legs answering. And a two-field union
  where only ONE field disagrees is still refused, against a partially anonymised target. So the
  booking's "no record coercion on the Go promote" was a third wrong mechanism for this site. One
  anonymous field in the common type is enough, which is narrow enough to act on.
- **The withdrawn claim survived in the strings the failure prints** (Graefe NAK): the CASE rows'
  `why` text still said the site COERCES and that the row existed to show it, contradicting the
  header 130 lines above — and a `why` is what a reader is handed at the moment the row reddens,
  which makes it the worst place for a retracted claim to sit. Both corrected.
- **A multi-row assertion that examined one row** (Torvalds and Graefe): the agreeing-name union
  declared two rows and walked only the first, so the second leg's value was never checked. Every
  row is walked now, and the leaves are sorted as PAIRS — sorting the two slices independently
  would have reordered names against values and asserted a pairing the query never produced,
  which is a bug this round wrote and caught before committing.

## Folds at r43

- **The union's direction was the fourth wrong mechanism, and the cause is readable** (Graefe
  NAK): r42 got the observable right and the direction wrong, sending the fix at the coercion
  trie. The actual cause is two lines apart in one file: the common row is computed with
  `MaximumType` and each leg is then gated on `MaximumType(leg, target).Equals(target)`, which
  that common row can never satisfy — the record arm keeps the NAMED side when two names
  disagree, so the maximum of a named source and an anonymous target is the named type again.
  A gate built from a function that is not idempotent under its own erasure. One anonymous field
  is enough because record equality is all-or-nothing, which is what makes the booking's
  "whenever" a statement rather than an induction.
- **Java accepts the union, so this is a conformance divergence** (Graefe): `isPromotionNeeded`
  recurses records positionally over element types and never reads a name; `inject` never
  consults a maximum type. Verified in the Java source rather than taken on report. That closes
  the "check whether Java accepts this" the booking had been carrying as an open question, and
  it changes the fix: positional promotability, not the trie.
- **The union title was refuted by a row this very PR builds** (Torvalds NAK): r42 titled the
  booking "refused whenever the common type carries an ANONYMOUS field". Feed the union two
  legs that are ALREADY anonymous — which the two-field CASE row added the same round produces
  — and it ANSWERS, promoting through the anonymous slot. The machine that builds the
  counterexample was sitting in the table and was never pointed at the other site. What is
  refused is a LEG that still NAMES a field the common type anonymised, at any depth, which is
  exactly what the traced cause above predicts and what the nested row confirms. The title, the
  header and the booking now say that, and the rows are committed at r44 — r43 CLAIMED them and shipped none, which the next bullet is about.
- **Guards nothing drove, and an error family standing in for an error** (codex P2 x3): the union
  rows matched only `is not promotable to`, so a run where alignment picked a NAMED common type
  would have passed the row that exists to attribute the failure to an ANONYMOUS one. They now
  assert the target by name and the 42F65 slot wrapper. And the three checks the leaf walker
  gained — unreadable leaves reported, empty expectations refused, pairs kept together in the
  sort — were reachable by no test, so deleting any of them left the suite green. One is
  extracted to be callable, and all three have Docker-free unit pins.

## Folds at r44

- **Three rows were claimed and none was committed** (Graefe NAK and Torvalds NAK, both by
  counting): r43 retitled the union booking on the strength of measurements that were run in a
  probe and then never typed into the table. The edit that would have added them reported "1
  (want 2)", and that was read as the OTHER hunk having failed; the header had in fact applied
  and the rows had not. The evidence was in the file the whole time — a constant declared for the
  missing row and referenced nowhere — and Go compiles an unused local const without complaint,
  so nothing anywhere went red. This is the mutation-never-applied failure with a `const` block
  standing in for the mutation. r44 types the rows and verifies them the way the reviewers caught
  it: counting the table's rows before and after, and asserting the constant is now referenced.
- **A second call site for the same gate** (Torvalds): `exactUnionSlotValue` is also called from
  recursive-CTE output alignment, which wraps the identical refusal in `0AF00` rather than
  `42F65`. Nothing pinned it, and a closure written against the UNION spelling would have left
  recursive CTEs refusing the same shapes. Now a row, and named in the booking's closure.
- **Two explanations still carried the refuted title, and one comment contradicted its own code**
  (Graefe and Torvalds): the target-based wording survived in two `why` strings, which are what a
  reader is handed when the row reddens. And the sort helper still said leg order is not asserted
  anywhere, in the round whose per-row comparison had begun asserting it. The dependency is real,
  so it is now stated rather than denied, with the remedy named if it ever breaks.

## Folds at r45

- **A fresh miscount, in the sentence built to be countable** (Graefe NAK, Torvalds the same as
  a condition): the union booking's pinned list said NINE rows and the table has TEN. The
  enumeration folded the promote row into the both-anonymous row's parenthetical, so the two
  measurements that together refute the target-based title read as one. Corrected, and the
  promote row is listed on its own because the two REFUTE TOGETHER: the both-anonymous row
  shows an anonymous target can answer, and the promote row shows the gate is genuinely reached,
  since equal leg types skip leg normalisation and never call it. Crediting either alone was the
  r45 error, and it sat in two homes.
- **A positional pointer into a growing table** (Torvalds): the head comment still said the
  three-condition account is broken by "the last four rows below", written when four were last.
  Five have been appended since, and the PAIR that kills the target-based title sits in the
  middle of them. (r45 wrote "the row" and pointed at it by position; r46 corrects both.) Positions are not stable in a table that has grown every round, so the
  comment names what the rows show instead of where they sit.
- **The second call site guards a local patch, not a second fix** (Graefe): both callers funnel
  through one gate, and the recursive-CTE target is derived with the same per-ordinal maximum, so
  the direction this booking prescribes covers both by construction. The row's value is that it
  would catch a patch written at one call site. Said that way now, rather than implying the
  closure has two halves.

## Folds at r46

- **The refuting-row attribution was split three ways** (Graefe NAK, codex P2): the table's row
  said one thing and the booking and an RFC fold said another, in the delta whose whole purpose
  was to make that attribution countable. codex supplied what settles it rather than a
  preference: equal leg types skip leg normalisation and never call the gate, so the
  both-anonymous row shows an anonymous target can answer and the PROMOTE row shows the gate is
  genuinely reached. Neither alone does the work. All three homes now say they refute together,
  and each says which half it is.
- **Two more numbers in the comment being edited** (Torvalds NAK): it opened by saying THREE
  successive rounds named a mechanism and then listed a fourth in the same sentence, and it
  described the table as including TWO shapes that fail when twelve rows fail under five
  distinct refusals. r45 removed one stale count from that paragraph and left both of these,
  which is the sweep failure at its smallest scale: the fix was applied to the copy that had
  been complained about. The comment names what the rows show rather than where they sit, and
  says so about ITSELF rather than about the file: the row prose below it does still
  cross-reference by position, and that is written down instead of denied. (r46 wrote "no count of
  anything", which its own next sentence broke by counting rounds; r47 narrowed it to the table's
  rows and positions, which the row prose still broke; r48 bounds it to the comment. The count of
  those pointers, and the command that takes it, are in the r48 fold rather than here.)

## Folds at r47

- **The no-counts claim counted** (Graefe as his one condition, Torvalds NAK, codex P3): r46's
  parenthetical said the comment quotes no count of rows "or of anything else", and the same
  paragraph then said FOUR rounds, a three-condition account, a fourth title. Two RFC copies
  repeated it. That is a scope sentence exceeding what the text does, and the self-disarming
  kind: it tells a later reader nothing needs updating while `FOUR` waits to go stale. Scoped at
  r47 to the table's rows and positions, and at r48 to the COMMENT — because the row prose points
  by position in several places, including in the very string r47 edited, so even the narrowed
  claim was wider than the file (the count and its producing command are in the r48 fold). Three attempts to state one paragraph's scope, each wider than the text. And
  codex's fix for the count is better than incrementing it: the opening is unnumbered, so the
  next refutation cannot rot it. Incrementing THREE to FOUR had only reset the clock.
- **The superseded singular attribution, three more copies** (Torvalds NAK, codex P2): the
  booking still said the both-anonymous row refutes the target-based title on its own, twenty-four
  lines above the list r46 had just corrected to say the pair refute together; and two RFC copies
  said the same. r46 fixed the copies it had been shown and left the ones it had merely written,
  which is this document's own standing rule about sweeps, failing at its smallest scale. Swept
  to zero with a positive control; the only surviving occurrence is this section quoting the
  retired wording as history.

## Folds at r48

- **The third attempt to state one paragraph's scope, and the third that was too wide** (Graefe
  NAK, Torvalds NAK, codex P2 — identical finding): r46 said the comment quotes no count of
  anything, which its own next sentence broke. r47 narrowed that to the table's rows and
  positions, which the ROW PROSE breaks — and one of them sits fourteen words after a
  positional pointer r47 had just deleted from the same string, which is the sweep failure inside
  the fix for a sweep failure. "Read the next two rows" breaks both halves at once.
  r48 stops trying to make the file fit the sentence. The sentence is bounded to the comment,
  and the comment now says the row prose DOES point by position, why that is worth keeping for a
  reader walking the rows in order, and that an insertion can silently retarget it. A disclaimer
  that overstates is worse than none, because it tells the next reader there is nothing to audit
  — which is the whole reason this took three rounds to get right.

  How many pointers the row prose carries is a fact about a file that grows a row at a time, so
  r49 took the figure out of the three sentences r48 had written it into and left it here, once,
  as the command that takes it — over the region below the first `func`, which is what makes the
  number mean anything:

  ```sh
  f=pkg/relational/sqldriver/wrapper_hidden_child_fdb_test.go
  for sha in e1b4ed839 13ba74d63 725164d72 b91c5c86c; do
    git show $sha:$f | tail -n +$(git show $sha:$f | grep -n '^func ' | head -1 | cut -d: -f1) |
      grep -cE 'row above|rows above|row below|rows below|failure above|CASE above|CASE below|next two rows|next row|two-field row'
  done   # 9, 9, 9, 9
  ```

  Drop the `tail` and the same pattern over the WHOLE file answers 9, 11, 14, 16 across those
  four heads, because each round's comment quotes more of the pointers it disclaims. That
  column is reported precisely because it MOVES while the claimed figure does not: same trees,
  same pattern, two series, and the region is what makes one of them mean something. It is part
  of the claim, not an implementation detail of how the claim was taken.

  This list is STALE BY CONSTRUCTION and always will be: it can never name the head it ships
  at, because that head does not exist until the commit is written. Two rounds running, a
  reviewer supplied the missing figure. That is the argument for the moving column rather than
  against it — a reader who wants the current number runs the command, and a reader who wants
  to know whether the number is stable reads the two series.

  It is a LINE count. `grep -o | wc -l` counts OCCURRENCES and is a different measurement, and
  here it happens to answer nine as well, at all three heads — which is worth writing down
  rather than leaving as agreement, because a pair that agrees by coincidence is the shape that
  validated a bad method once already in `CLAUDE.md`. No line in this region carries two of
  these phrasings today; the moment one does, the two numbers part and only the one with a
  stated unit stays checkable.

  r49 also wrote a THIRD counting into this paragraph, attributed to a reviewer, and r50 deleted
  it rather than correcting it, because it was never run here. Run: the bare-token pattern
  `above|below|next two rows` over the same region gives ELEVEN occurrences on TEN lines, not
  nine — it also catches "every row below succeeds vacuously", "nothing above the array is
  stamped" (which points into the expression tree, not the table) and "the comparison below
  succeeds" inside a helper. Reporting a figure somebody else took, without taking it, is the
  precise move this paragraph exists to warn against, committed inside the paragraph that warns
  against it.

## Folds at r49

- **The pointer count, written into prose three times** (Torvalds nit, Graefe nit — the same
  one): r48 fixed a scope claim and, in the sentences explaining the fix, wrote "nine times"
  into the status line and two fold bullets. That is an unscoped number over a file whose whole
  point is that rows get inserted into it — the failure the paragraph it describes exists to
  prevent, committed in the description of the prevention. It is now one figure, in the r48
  fold, with the command that produces it, the region it is taken over, and both heads it was
  taken at; the other three places point at it. The region matters and is stated: the same
  pattern answers nine below the first `func` and eleven over the whole file at `13ba74d63`,
  because the bounded comment quotes two of the pointers it disclaims.
- **A count in the paragraph that refuses to count** (Graefe nit): the comment's opening says no
  count of the refuted accounts is given, deliberately, because the list grows every round — and
  four lines later it said THREE successive attempts to state the paragraph's scope. Both
  reviewers noted it was currently true and the same shape as the `FOUR` r47 had to delete. It
  reads "every earlier attempt" now, which is immune the way the list above it is immune: a
  fourth attempt would make it more true rather than stale.
- **The pointers are not equally dangerous, and the disclaimer treated them as one class**
  (@claude nit, raised as "not a defect in what's shipped" and folded because r49 was reopening
  the comment anyway): most of the row prose's pointers DIRECT a reader at supporting evidence,
  so an insertion that retargets one makes it unhelpful, and several would read as visibly
  self-contradictory to anyone who followed them. Two ASSERT something about the row they point
  at instead — the comparative "worse than the failure above", whose truth value depends on
  which row that is, and the forward "the two-field row below shows one is", which attributes a
  finding to a row not yet read. Those flip from true to false leaving nothing inconsistent on
  the page, which is strictly worse than going stale. The comment now separates the two and says
  to re-read the neighbours of an assertion after inserting beside it.
- **The nightly's red, pinned** (not a review finding — found while re-measuring the bookings
  this PR's final report was going to quote): the engine fuzz nightly's run 33955019772
  (2026-09-05) failed on `FuzzPlanner_PlanFullPipeline` and on nothing else — one failure
  across the 44 target/function pairs that rotation ran — a different target from the
  `FuzzPlanner_WithBatchA_NoPanic` that opened this RFC, and the same defect. Six bytes,
  `Distinct(Union(UnsortedSort(Scan), Projection(Scan)))`, rediscovered at the merge-base
  within the first second of fuzzing (`go test -fuzz='^FuzzPlanner_PlanFullPipeline$'`, 24
  workers, `fuzz: elapsed: 1s, minimizing`). Committed as corpus entry `05cb6c3c202ef15e` with
  both measurements in § Test plan item 1. It reaches the rule from the arm the first entry does not exercise — the
  bare `Scan` terminal, where the walker folds the record type — and it asserts through `Plan`
  returning an unexpected error rather than through a panic. Pinning the shape the nightly is
  actually red on, rather than a cousin of it, is the rule this file keeps: a test that cannot
  express the defect is not coverage.

## Folds at r61-r72

r72 is the round that closed a NAK, and both of its code findings were in the fix written the
round before — which is the shape this branch has repeated often enough to state as a property of
the work rather than an accident.

- **The re-issue added at r71 turned a daemon outage into a reported REMOVAL** (Torvalds, and
  codex independently). `if out=$(…) && [ -n "$out" ]` collapses "daemon unreachable" into "not
  present", so a first answer of rc 0 + empty followed by the daemon going down inside the
  one-second confirmation window fell straight through to `gone_reason=removed`. The main loop
  then writes the `GONE (removed…)` line a reader uses to DATE the cluster's death — the exact
  defect `gone_reason` was introduced to prevent, reached through the re-issue added to prevent a
  different one. Driven by extracting `still_there` and running it against a scripted `docker`
  whose answers are a fixed sequence: the failing case is a race between two calls and no phase
  fixture can reach it. Four arms now pin the decision table, and the shipped bug reddens exactly
  the one that names it — `got [1 removed stopped], want [0 none started]`.
- **`misses` had become dead, and two comments still described it as live** (Torvalds; codex
  reached the same place by asking what the pollers skip on). Switching the budget from polls to
  seconds left `misses` assigned at three sites, read at two, and incremented nowhere — so both
  `if [ "$misses" -gt 0 ]` skip branches were unreachable, and a `docker cp` issued during an
  outage failed and logged `periodic copy FAILING (inspect still succeeds)`, which is false in
  exactly the state it prints in. The signal is set in the outage path again. Driving it needed
  the stub to model an outage as GLOBAL — with `cp` still working, skipping and not skipping are
  indistinguishable — which is itself the point: a fixture that is too kind cannot tell them apart.

And two more corrections to this file's own record, both from codex and both second attempts:

- **The hygiene-gate booking got its measurement wrong twice.** The first version invented a
  population (a glob that excluded the file it named as an example); the second kept the
  population and invented the PATTERN — three of the eight regexes the gate actually uses. Third
  version reads `bannedCommentPatterns` and reports 57 files. The error is the same both times:
  writing a check from memory of the RULE instead of from the GATE.
- **The status chain's restoration was itself misaligned.** Each round's summary is terminated by
  the count current when the NEXT round wrote it, and the r71 restoration put the newest figure
  one position too far left, leaving r70's summary with no count at all. Realigned against each
  commit's own line 3; the chain now reads 45, 43, 41, 36, 33, 33, 30, 28, 28 descending.

r71 is the round where the reviewers stopped finding bugs in the watcher and started finding them
in the RECORD of the watcher — and both were mine.

- **The arm counts in this file had been corrupted by the tool used to maintain them** (codex).
  Every round ended with a blanket `sed s/NN arms/<new>/g` to keep the status line current, and
  that swept the fold sections and the whole per-round status chain, rewriting each past
  measurement's population to today's. A five-run result taken at 28 arms read "43 arms"; two
  mutation results taken at 24 and 28 said the same; all eight round summaries claimed the current
  size. The claims were true when made and false as written. Restored by re-running the suite at
  each historical commit and recovering each sentence's number from the commit that introduced it;
  the note at the top of this section records the mechanism, because the tool was the failure.
- **The TODO entry booking the hygiene-gate gap was wrong three ways** (codex). It claimed 29 files
  and ZERO violations — a free ratchet. The glob was `'*.sh' '*.yml'`, which does not match
  `*.yaml`, so it excluded `infra/cloud-init.yaml`, the file the same sentence named as an example;
  the pattern listed `graefe|torvalds` but not `codex`, the commonest of the three here. Corrected:
  **429 files, 48 with attribution, 86 lines**, of which 41 are `yamsql/testdata` and genuine
  ("Subquery false-positive guard (Torvalds review)"). So it is a gate extension plus a 48-file
  cleanup, and the cleanup is the larger half — which is the thing worth knowing before starting.

The code findings, all from the same two reviewers:

- **`maxOutagePolls` was the wrong unit and my own comment said so** (Torvalds). The three callers
  poll at 60s, 30s and 2s, so 20 polls was 20 minutes, 10 minutes and **40 seconds** — and the
  main loop, where a false removal costs most, got the shortest, inside an ordinary `systemctl
  restart docker`. The sentence condemning the count it replaced applied verbatim; only the
  magnitude had moved. It is `maxOutageSeconds` now.
- **The backstop reinstated the defect it was guarding** (Torvalds, and codex independently). When
  it tripped, the main loop wrote `GONE (removed…)`, because `still_there` returned 1 for both
  reasons and the caller could not tell them apart — one line below a message correctly calling it
  an outage. `gone_reason` is returned, and `GONE` is written only for a proven removal. Driven:
  making the caller ignore the reason reddens the new arm with `outage lines=2, GONE lines=2`.
- **The outage window reset only on SELECTION** (codex). A successful `docker inspect --format`
  bypasses `still_there`, so isolated outages accumulated across a lane until the backstop tripped
  on a live container. Reset after every successful inspect. It is in the QUEUE half of the
  not-covered list rather than driven, with the flapping knob it needs named — every case here
  produces one continuous outage, and this needs many separated by successes.
- **Removal was decided on a single sample** (Torvalds). rc 0 with empty output is the only reading
  that means removed, and a daemon answering mid-reload produces it. Re-issued once before acting.
- **The alarm's false-positive guard was unpinned** (Graefe and Torvalds, independently, with the
  same two-line fix). `alarm_case` wiped its workspace and created no `fdb-logs-*`, so
  `have_traces` was 0 in every arm and mutating that conjunct out reddened nothing — on the one
  change in this branch that can fail a night which previously passed. A 7th parameter and one arm
  bound it: mutating the conjunct out now reddens `an inspect WITH traces on a failed night is
  evidence`.
- **The alarm's message named one cause of three** (both). The same state arises from a copier that
  stopped early, a copier whose every copy failed, and a container that never survived its first
  cycle — and it sent the reader to look for a line that exists in only one of them. All three are
  named, with the line that separates them.

> **A NOTE ON THE ARM COUNTS BELOW, because they were corrupted once and the mechanism is worth
> naming.** Every round here ended with a blanket `sed s/NN arms/<new>/g` over this file to keep
> the status line honest — and that swept the FOLD SECTIONS too, rewriting each past measurement's
> population to the current one. A five-run measurement taken at 28 arms was reading "43 arms"; two
> mutation results taken at 24 and 28 said the same. The claims were true when made and false as
> written, which is precisely the failure this RFC spends several sections on, committed by the
> tool used to maintain it. Restored by re-running the suite at each historical commit
> (`791e33408` → 26, `df8a24834` → 28, `0b464be55` → 30, `b72ac247b` → 33) and recovering each
> sentence's original number from the commit that introduced it. Only the STATUS LINE tracks the
> current count now; a number inside a fold belongs to the round that measured it and must not be
> swept.
>
> The STATUS LINE was corrupted the same way and is restored the same way: it is a chain of
> per-round summaries, each ending in that round's own suite size, and the blanket sweep set all
> eight to the current figure. Recovered by reading the FIRST such figure on line 3 at each
> commit — the value that round claimed while it was current — giving 28 (r63), 30 (r65), 33
> (r66), 36 (r68), 41 (r69), 43 (r70). Two adjacent pairs legitimately repeat, since r66/r67 and
> r63/r64 shipped no new arms between them.

r70 replaced the test rather than tuning the constant, which is the only one of these rounds where
the fix got SMALLER than the thing it replaced.

Torvalds measured what `docker inspect` can and cannot distinguish, and the answer explains every
defect in this area at once:

    docker ps -aq --filter id=<id>     present -> rc 0, prints the id
                                       removed -> rc 0, EMPTY
                            daemon unreachable -> rc 1, empty

    docker inspect <id>                removed -> rc 1
                            daemon unreachable -> rc 1

`inspect` collapses removal and outage into one status, which is exactly WHY a tolerance count was
needed — the code could not see the difference, so it waited to see whether the failure persisted.
With the filter, a removal is decided by the OUTPUT and an outage by the STATUS: a genuine removal
ends the poller immediately and loudly, and only an unreachable daemon is tolerated. The arbitrary
`3` is deleted rather than tuned; what remains is a generous backstop against a daemon that never
returns, and it can be generous precisely because no removal depends on it. Verified independently
against a real container before building on it.

That also retires the unit problem Graefe raised in the same lap — a blip has a DURATION, a count
is that duration over a poll interval, and three pollers poll at different rates, so one number
meant three tolerances. His arithmetic correction stands and is worth recording because it was
mine that was wrong: the tolerance was 2 survivable intervals, so 120s and 60s, not the 180s/90s I
had written, and the asymmetry between pollers was not stated at all.

- **There were THREE `inspect` callers and only two had been converted** (Torvalds). The main loop
  still declared removal on one failure — the `GONE` line a triaging reader uses to DATE the
  cluster's death — so a blip produced a false removal at a false time and cleared `seen`, and the
  same live container was "re-selected" a tick later. My own earlier measurement said "main blip →
  capture continues", which was right about capture and never looked at the log.
- **The blip knob separated 2 ways over 3 callers** (Torvalds, and codex independently). All three
  issue the same command, so the knob hit whichever polled first; with the df sampler sped up it
  was `MISS by df sampling` 3/3 and the suite still said `ALL OK`. It is aimed by an exported
  `POLLER` now, and the arms name which poller they blip.
- **The df sampler had no observable at all** (codex). Its retry wiring could be reverted with no
  arm reddening, because the stub refused `docker exec` and its `sleep 30` sat outside the suite's
  interval override — so it never polled twice in a case's lifetime. Both fixed; reverting it now
  reddens its own arm.
- **An arm asserted a MESSAGE and reported it as TERMINATION** (codex). Changing `still_there`'s
  final `return 1` to `return 0` makes the copier spin forever after removal, emitting the same
  line every poll — and the arm greping for that line passed. Counting it separates them: a copier
  that ends logs once, one that spins logs four times in the fixture.
- **The alarm accepted evidence written by the wrong author** (Graefe). Its test was `have_inspect`
  over `fdb-last-inspect-*.txt`, which the MAIN loop writes, while `fdb-logs-*` comes from the
  COPIER — so a dead copier left the alarm green with no traces at all. It now also requires a
  trace directory, scoped to the case where the copier's snapshot is the only possible evidence: a
  LIVE container in the dump was read directly and is evidence on its own. One existing arm changed
  its expectation as a result, deliberately, and says why.

Two of my own, both process rather than code: reviewer attribution had reached two comments
("three reviewers", "a review pointed out") and shipped through a green `just test`, because
`TestSourceCommentHygiene` scans only `*.go` while this branch's load-bearing comments are in shell
and YAML. Removed, and the gate gap is booked in `TODO.md` with the measurement that makes it a
free ratchet — 29 shell/YAML files, 0 current violations — and with the honest reason it is not
done here. And the platform provenance was corrected in the workflow but not in the suite's copy of
it, which is the fix-only-the-copy-you-remember failure inside the round that fixed it elsewhere.

r69 is the round where the reviewers stopped finding faults in the fix and started finding the same
fault underneath it, three times independently.

**A failed `docker inspect` is not a removed container**, and both pollers used their loop
CONDITION as the liveness test. Graefe injected one blip with the container alive and `cp` still
working: the last generation stayed at `1-5` while the control went `1-5 -> 1-13`, with nothing
logged anywhere. codex reached it from the other side — the launch guard means a worker that dies
this way is never restored. I reproduced it before changing anything, separating the two `inspect`
callers (the copier's has no `--format`, the main loop's does): copier blip → `advanced=NO`, main
blip → capture continues. One blip permanently ended trace capture, silently, on exactly the
overloaded box this watcher exists to explain.

Both pollers share one `still_there` helper now: tolerate up to three consecutive misses, then END
LOUDLY. Both directions are driven, which matters more than usual here — a tolerance is one edit
from becoming a loop that never stops, which is this file's signature failure. Zero tolerance
reddens two arms (`stuck at fdb-logs-c1.1-4`); unbounded tolerance reddens the arm requiring a
genuinely removed container to end the copier and say so.

The refactor itself then produced a small lesson worth keeping: folding the tolerance sleep into
its `if` put a REAL 60-second wait inside the suite, because the interval override rewrites a line
that is exactly `sleep 60`. The arms timed out and caught it. The sleep is on its own line, and the
comment says why.

**The fifth hole had a fifth consequence.** Torvalds drove it: `exited` was a single flag reset on
every re-selection, so a container re-selected AFTER it had already exited had its transition logged
twice — and the second pass finds the exit destination occupied, so the log then says the trace was
NOT copied for a container whose trace is sitting right there. Both halves false, on the line a
triaging reader trusts most; data loss prevented only by `mv -T` refusing, on a path nobody had
identified. It is a per-container list now, like `launched`. Two arms, and the fixture had to be
rebuilt so the exit precedes the re-selection — with the bug present they report `got 2` and
`got 1`, and defeating the guard reddens three arms including a pre-existing one.

**And the stub was half-inert.** `id="${@: -1}"` took the LAST argument under a comment claiming
the id was last; every real call is id-FIRST, so for every `--format` call the stub used the format
string as the container id and fell back to the global phase with nothing reporting it. The
per-container fixture had been lying since it was written. `id="$2"`.

Smaller, all measured: `$(grep -c … || echo 0)` yields `0\n0` on a zero match, because `grep -c`
prints 0 AND exits 1; the `launched` pattern is literal because its `" $c "` segment is quoted, and
an EMPTY `$c` would match and skip every launch forever — unreachable only because of an
`[ -n "$c" ]` written elsewhere for another reason, which is now said at the guard; and the bash
measurement names its two VERSIONS as the claim, with the provenance stated exactly — 5.2.21 was
measured in the `ubuntu:24.04` container, while the fleet boots Hetzner's same-release rolling
label, which is not the same artifact.

**The not-covered list is two lists now.** It was 4 entries when this file was committed and reached
10; all four originals survived verbatim, while every entry anyone picked up became an arm on the
first attempt. Three of those four are in the second half. The risk was never length — it is that
"cannot be driven from here" and "has not been driven yet" look identical, so the closable ones
inherit the permanence of the structural ones. Five STRUCTURAL, five in a QUEUE framed as next arms.

The fifth hole had THREE consequences and r67 fixed one of them. Torvalds reduced the launch branch
and ran it: with the container list going `A A B B A A`, container A gets launch #1 and launch #3
while its first copier is still looping — and the same branch also re-runs `docker logs -f` (two
followers appending one stream to one file, no `--since`, so the log is replayed on top of itself)
and a second `df` sampler (two O_TRUNC writers on `fdb-df-$c.txt.new`, each failure branch `rm -f`ing
the other's file between its write and its rename). codex, reviewing in parallel, found a fourth:
the STAGING path was still shared, so two copiers `rm -rf` and fill one directory and either can
publish what the other half-wrote — the target being launch-scoped does nothing about that.

Scoping the generation name addressed one consequence of four. What ships is a guard at the launch
site — an id list, so a container's background jobs start at most once — which closes all four at
the cause and demotes `$launch` to defence in depth. The staging path is launch-scoped too, since
defence in depth that only covers the published name is not defence in depth.

The arm changed with it, and this is the part worth keeping: it asserted TWO distinct launch ids,
which was asserting the MITIGATION while the cause was still there. It now asserts ONE, with a
companion arm requiring that the case actually re-selected the container (`2 selections`) so it
cannot pass by never reaching the path. Defeating the guard reddens it with `launch ids seen: '1 3'`.

Three more from the same lap:

- **`[ "$gens" -le 1 ]` accepted ZERO**, inside the arm added to close a real defect. Zero means
  the copier published nothing — the empty-set false green this file is built around. `= 1` now.
- **The two copy-failure log lines were undriven and unlisted**, the third instance of that pattern
  this branch has produced. Three arms now, and the one that matters is the count: dropping the
  `[ -z "$copyfail" ]` guard gives FIVE lines where a presence check would still be green.
- **The bash measurement was taken on the laptop.** "5 times out of 5 on bash 5.3.15" is the dev
  box; the fleet runs `ubuntu-24.04` per `infra/main.tf`. Re-measured there: bash 5.2.21, 5 of 5.
  Measuring instead of reading the manual was right; measuring on the wrong machine is the same
  substitution one step smaller.

The orphan-retire log line is undriven and now SAYS so in the not-covered list, with the shim that
would drive it named — which is the difference between an entry and an excuse.

r67 closes the entry r66 added rather than defending it — and then Graefe showed the first attempt
at closing it had shipped an arm that could not fail, so this is the version after that too.

The entry said two copiers for one container were "reasoned, not driven", because the stub served
exactly one. That is the shape r66 had just caught twice: a not-covered entry retiring a case that
is hard rather than impossible. Per-container phase in the stub was all it needed:

    only c1                -> head -1 = c1, copier #1 for c1
    c2 appears (newer)     -> head -1 = c2, copier for c2; c1's is STILL LOOPING
    c2 removed             -> head -1 = c1 again, and c1 != seen, so a SECOND copier starts

The arm reads the launch prefixes off the published generations and requires two distinct ones,
reporting `launches: 1 3`; pinning `launch` to a constant reddens it with `launches seen: '1 '`.
Graefe drove the same path independently and published
`fdb-logs-c1.1-16`, `fdb-logs-c1.3-6`, `fdb-logs-c2.2-4` — two copiers on one container, launch
ids 1 and 3.

The arm shipped BESIDE it asserted that no published generation contains a nested directory, and
it cannot fail. Two copiers only collide if their counters CROSS, and they cannot: both sleep the
same interval and the second starts later, so the first leads by a fixed margin forever. Crossing
needs asymmetric cycle times, which a stub cannot produce without telling the two copiers apart.
Measured, and it generalises to the periodic path's own nesting arm: `mv -T` -> `mv` there reddens
NOTHING. Both were deleted rather than kept, and the not-covered list now says exactly which half
is driven — the scoping that makes a crossing harmless — and which is not, and why the EXIT path
is different (a fixed destination makes a collision reachable there, so nesting IS driven on it).

That is two arms removed for being green-by-construction in a round whose own subject was arms
that cannot fail. The lesson that keeps recurring is not "mutate the code" — it is that an
assertion added to guard an invariant is exactly where a vacuous arm hides, because it looks like
diligence.

All 33 pre-existing arms passed through the per-container stub change unchanged before any new
one was added, which is the regression check that change needed.

Both gates reviewed `0b464be55` and both returned ACK-conditional again, and this round the
findings are of a different kind: they are about the NOT-COVERED LIST. Two of its entries claimed
something was undrivable, and both reviewers independently built the arm that drives it. A
not-covered list is supposed to be the honest half of a coverage claim; used to retire a hard case
it becomes the same over-claim wearing the opposite sign, and that is what had happened.

- **`mv -T` was recorded as undriven and is drivable.** The entry reasoned that the generation
  counter already makes the target unique, so nothing could reach the nesting case. True of the
  PERIODIC path and false of the EXIT one, whose destination is a fixed name: occupy it and the
  publish must refuse. Two arms now, and with plain `mv` both redden — the copy lands at
  `fdb-logs-c1-exit/.fdb-logs-c1-exit.new` and the log still says "copied complete at exit".
- **The retire RENAME was recorded as covered and is not the same mutation.** The fold said
  "dropping the rename reddens the generation-count arm". Dropping the retire ENTIRELY reddens it;
  dropping only the rename — deleting `$prevgen` directly, the pre-r65 shape — reddens nothing,
  because no case interrupts a deletion. A slow-`rm` shim makes the interrupt land inside one
  every time, and the arm then reports `found 2`. The r65 fix had no arm and the list said it did.
- **A fifth hole in the publish, and it is the same trade as the other four.** `gen`/`prevgen`
  were per-subshell-LAUNCH, not per container. `$c` is `docker ps -aq … | head -1`, and a copier
  launches whenever `$c` differs from `$seen`; with two containers in a lane, `head -1` reverts to
  the older when the newer goes, that one is still alive with its first copier looping, and a
  second starts at `gen=0`. Both then mint the same names. The timestamp scheme was immune to
  this exact case; the counter bought uniqueness within a loop and lost it across launches. Both
  are composed now — a launch counter in the parent shell, a cycle counter in the loop — rather
  than one traded for the other, which is the mistake the previous four shapes each made.
- **A failed retire orphaned a generation silently**, and a failed periodic copy said nothing at
  all while the exit branch logged its own. Both speak now; the periodic one logs the TRANSITION
  into failing and back out, because logging every cycle is the burial this file already warns
  about one section down.
- **An arm asserted a header and reported it as content.** "The dump reads the per-container last
  inspect" grepped for the FILENAME, which the loop prints as `--- $i` before `cat`ing the file —
  so deleting the `cat` reddened nothing. It asserts the fixture's `exit=1` now. Its neighbour
  used `ls` where the alarm it is testing uses `[ -s ]`, so the suite accepted a zero-byte inspect
  the instrument rejects; it uses `[ -s ]` too.
- **And a comment justified a guard from a manual page.** "An untrapped SIGTERM kills the shell
  without running EXIT at all" is documented behaviour that was never run here, and it is false on
  this bash: with an EXIT trap set and no TERM trap, a group SIGTERM ran the trap 5 times out of
  5. The arm is kept for promptness; the justification is now the measurement.

codex reviewed `df8a24834` in parallel and reached the generation collision independently — the
third route to the same finding, which is worth noting because it was invisible to the arm written
to watch that code. He also found one nobody else did, folded at r65:

- **`rm -rf` on the retired generation is interruptible.** The stop signal reaches this group at
  an arbitrary point, and a deletion cut in half leaves a partially dismantled `fdb-logs-*`
  directory — which the dump lists and the upload ships as a published snapshot. Measured on a
  deep tree: an interrupted `rm -rf` left 6170 of 8000 files with the directory still matching the
  glob. So a generation is now RETIRED BY RENAME first, onto a dot-prefixed trash name, which is
  atomic — the moment it returns the old generation is invisible to that glob regardless of when
  the signal lands — and only then deleted. Dropping the rename reddens the generation-count arm;
  dropping the trash deletion reddens nothing, because the watcher's trap removes every
  dot-prefixed leftover on the way out, and the not-covered list says so rather than counting it.

His other two were already closed at r64 by the counter and `mv -T`: the exit path fails closed on
a pre-existing snapshot instead of removing it first, and the generation key cannot collide.

One arm added beside them, because the count arm alone accepts a husk: the surviving generation
must still HOLD its traces. "Present, therefore captured" is the reading this entire dump exists
to make impossible, and the directory-count assertion was quietly making it again.

Both gates then reviewed `df8a24834` and both returned **ACK-conditional**, having re-driven every
mutation claim in frozen worktrees. Their conditions, and two findings each made independently
that the other did not, are folded at r64.

- **`mv` onto a pre-existing generation NESTS and returns 0.** Both found it; so did I, before
  either reported. The timestamp was not unique — two cycles inside one second give `mv` a target
  that already exists, and the copy then sits one level below where the dump reads while the exit
  status says success and a generation COUNT still reads one. Silent and green. Two changes, not
  one, because they fail differently: the generation name is a per-cycle COUNTER, which cannot
  repeat, and the rename is `mv -T`, which refuses to nest structurally (measured: rc=1,
  `Directory not empty`). The counter is what the arms drive — pinning `gen` at a constant reddens
  two trace-capture arms — and `-T` is a second, undriven refusal, recorded as such in the
  not-covered list rather than counted as coverage.
- **`ls | sort | head -n -1` ordered lexicographically.** Correct only while every stamp is the
  same width: `.9` sorts after `.10`. Epoch seconds are 10 digits until 2286, so the bug was real
  and dormant, inside a prune whose job is deleting. Gone: the loop REMEMBERS the previous
  generation instead of re-deriving it by listing, which removes the ordering question rather
  than answering it, and removes parsing `ls` with it.
- **"At every instant the glob sees exactly one complete snapshot" was false** (Graefe, measured):
  widening the gap between rename and prune and letting the stop land in it shows two. The true
  claim — and the one the dump needs — is at least one complete snapshot and never a partial or
  absent one. The count was never the guarantee.
- **A SIGTERM mid-copy left staging on the runner** (Graefe, measured by widening the stub's copy
  until the stop was guaranteed to land inside it; the arm failed). So "no staging outlives the
  copier" held only because the window is narrow — five clean runs cannot separate p=0 from
  p≈10⁻³. One `trap` at the top of the watcher fixes it, and the suite now drives that window
  DETERMINISTICALLY with a slow-copy knob rather than hoping to hit it. A per-subshell trap as
  well was measured redundant — removing either alone changed nothing and only removing both
  reddened the arm — so there is one, which is the point of measuring instead of adding.
- **The exit path was still `rm` then `mv`** (Torvalds), shape (2), one line under the comment
  outlawing it, and dropping the pre-`rm` reddened no arm. `mv -T` replaces it: no window, and the
  nest is refused rather than prevented.
- **Two staging arms could not tell which loop leaked** (Torvalds): both loops stage under
  `.fdb-logs-*`, so a shared glob fired whichever arm ran — a periodic leak was reported as an
  exit one. Each arm names its own loop now.
- **`stop_watcher` slept once and assumed** (Torvalds): it polls `kill -0` and escalates, the way
  the workflow's own stop step does, since three arms read the workspace after it returns.
- **And an arm of mine still could not fail.** The periodic case ended on a SUCCESSFUL cycle, and
  a successful publish consumes its staging by renaming it — so the periodic failure branch's
  cleanup could be deleted with the arm still green. The case ends on a failing cycle now. This is
  the third vacuous arm found this round by mutating rather than reading, which is the argument
  for doing it every time and not only when something looks suspicious.

Torvalds reviewed the same two heads: **NAK on `8dc1aef2a`** for three defects — the promote
window, the exit copy's `mv -f`, and the "18 arms" sentence — and **ACK on `791e33408`**, having
re-driven every mutation claim himself. He measured r60 at 14 arms, matching Graefe. He also
recorded a process hazard worth keeping: mid-review the worktree briefly held `: # MUTATED` in
place of the forensics tag guard, and a `git add -A` there would have shipped precisely the defect
`fa9d74dca` is titled after. Staging by path is what prevented it.

codex reviewed the delta and returned two more, both real and both folded here:

- **`cp -a … || true` put a swallowed failure back into the loop it had been flagged for.** A host
  copy interrupted mid-file publishes a truncated snapshot and the complete staging copy is then
  deleted. Fixed by removing `cp -a` — see the publish bullet below — rather than by writing down
  a reason the `|| true` was acceptable, which was the alternative on the table.
- **The staging assertions raced the copier they were asserting about.** The periodic loop
  recreates its staging directory every cycle, so "no staging directory exists" reports a failure
  whenever it lands between the `mkdir` and the rename. That is a flake, and this repo does not
  get to call one unrelated. Every staging assertion now runs after `stop_watcher`. Confirmed by
  running the suite five consecutive times: 28 arms, 0 failures, every run.

Driving codex's second finding turned up a vacuous arm of my own: the exit path's staging-cleanup
arm did not discriminate. On a copy that SUCCEEDS the staging directory is consumed by the rename,
so deleting the failure branch's `rm -rf` changed nothing and the arm stayed green — a test
written for exactly one mutation, and blind to it. The failure is now driven directly with
`cpfail` held across the transition, and both halves are asserted: nothing staged is left behind,
and the watcher SAYS the copy did not happen rather than leaving a reader to infer it from an
absent directory. Each of those reddens alone.

Graefe NAKed the first r61 head (`8dc1aef2a`) with five findings; all five are folded below and
the NAK's own blocker was already fixed in `791e33408` before he reported. He re-drove every
mutation claim in a frozen worktree and they all held — which is the part worth recording, since
the findings are about what the prose CLAIMED, not about what the code does.

- **F1 (his blocker), the promote window.** Already the subject of `791e33408`; see the last
  bullet in this section. He independently reached the same reading, including that both the
  dump's glob and the artifact upload skip the dot-prefixed staging, so the window loses the
  snapshot rather than merely hiding it.
- **F2, a population no revision ever had.** The suite said "measured on this file at 18 arms".
  18 was an uncommitted intermediate: r60 RUNS 14 arms and contains no `bazelrc` string at all,
  and the committed head ran 24. The "6" an earlier draft of this bullet gave for r60 was itself
  the same error one level down — it counted `ok`/`bad` CALL SITES, and a loop or a helper emits
  several arms from one site. Both reviewers measured 14 independently by running it, which is
  the only way a count of what a suite REPORTS can be taken. A stated population that matches nothing is worse than none, because it
  cannot be seen to go stale — the whole point of stating one. Re-measured and restated at the
  committed count, with the retraction left in place rather than silently corrected.
- **F3, wrong provenance.** Two places credited the tag-guard coverage to r60. r60's folds have
  four bullets and none is about a tag guard. Torvalds named the gap during that lap; the code
  landed here. Corrected in the status line and the bullet.
- **F4, a number that is not reproducible.** The fold quoted the failure's aliases as `q$4`/`q$3`;
  he measured a different pair, because quantifier aliases come from a process-global counter and
  move with test ordering and parallelism. The claim is now the shape — a fresh correlation where
  the quantifier's was required — and says why the numbers are omitted.
- **F5, the exit copy disagreed with the periodic one.** It still did `mv -f` into its
  destination. `mv -f` of a directory ONTO an existing directory does not replace it, it nests the
  source inside and returns 0 — so the "logs copied complete at exit" line would be written for a
  copy that landed one level too deep. Measured directly rather than reasoned about. It uses the
  same publish as the periodic copy now — a rename, not the merge, which this round deleted — and
  its staging cleanup has an arm, which it did not: the periodic path's
  cleanup was covered and was vouching for a sibling that was not. Dropping the exit cleanup
  reddens that arm alone, at 28.

codex reviewed `bb231d345..fa9d74dca` and returned four findings, all real and all folded. Three
are the same family this branch keeps landing in — an instrument that cannot tell "looked and
found nothing" from "never looked" — and the fourth is prose that outlived what it described.

- **A trace copy that FAILED published a directory anyway** (codex P2, `nightly-rowdiff.yml:234`):
  the periodic loop ran `mkdir -p "fdb-logs-$c"` and then swallowed the copy's failure with
  `|| true`, so a container removed between the loop's `inspect` and its `cp` left an EMPTY
  directory. The dump cannot tell that from a healthy cluster that wrote no fatal trace, and the
  alarm below passes on a last-inspect, so the trace section reads as examined while holding
  nothing. The `df` sampler eleven lines down already used promote-only-on-success and says why —
  the copier just never adopted it. Both copies (periodic and exit-transition) now stage into a
  dot-prefixed directory the dump's `fdb-logs-*` glob cannot pick up mid-copy, and promote only
  when `docker cp` returns zero. Pinned by a `cpfail` knob in the stub — the removal window
  between inspect and copy, which no container phase can express on its own — driving three arms:
  a failed copy publishes no directory, leaves no staging directory, and the same copy publishes
  once it succeeds. Reverting the fix reddens the first of those and only it, at 24 arms.
- **The missing-trace fallback could not fire** (codex P2, `nightly-rowdiff.yml:590`): `|| echo`
  hung on the `for` is unreachable when the glob matches nothing — the name stays literal,
  `[ -d ]` is false, `continue` is the last command, and the loop exits 0. Measured in a bare
  directory: the section prints NOTHING, neither a directory nor the fallback. This is the r58
  defect one loop over, and it survived r58's fix because the case added then supplied a
  directory. The sibling `fdb-df-*` loop fired its fallback from the identical shape, but only
  because `[ -e ] &&` leaves a non-zero status — correct by accident of the last command, which
  is why the working sibling could not vouch for the broken one and both are counted explicitly
  now. Three arms drive the three fallbacks; restoring the shipped shape reddens one.
- **The dynamic-carrier test could not see the defect it named** (codex P2,
  `erased_record_result_base_test.go`): it asserted the published type and nil ordinal properties,
  and `newPlanExprBaseForType(owner, resultValue.Type())` over the same erased type produces both.
  That fallback mints a fresh `UniqueCorrelationIdentifier`, so what actually separates the two
  exits is WHOSE value comes back, and nothing asserted it — a test claiming the route "hands back
  the quantifier's own flowed value untouched" while being green if it handed back someone else's.
  The correlation is now the assertion and the type only its shape. Driven: the substitution
  reddens that test alone of the four, reporting that the published correlation is not the
  quantifier's own. The two alias NUMBERS the failure prints are deliberately not quoted here:
  quantifier aliases are minted from a process-global counter, so they move with test ordering
  and parallelism — a run of the same mutation reported a different pair. The reproducible claim
  is the shape, a fresh correlation where the quantifier's was required.
- **The blanket-skip description outlived the blanket skip** (codex P3): the sweep stopped skipping
  during in-flight jobs when the start-time comparison landed, and two places still said it did —
  `nightly-rowdiff.yml` and the `TODO.md` booking, which also still described a three-arm hand test
  that a committed six-arm suite replaced. Both corrected. The repo-wide re-check has to be
  multi-line: the workflow's copy wraps mid-sentence, so a line-oriented `git grep` reported it
  absent from the very file it was in. Over all tracked files, joined and comment-stripped, the
  superseded framing now matches 0 files, against 10 for `orphan-fdb-sweep` as the control.

Three things not from codex, all from driving the above — and the last of them is a defect the
fix for codex's first finding INTRODUCED:

- **The tag guard was covered for one of its two steps.** Deleting the forensics copy reddens
  exactly one arm; deleting the WATCHER copy reddened NONE, because the extraction named a step.
  It takes the step as a parameter now and both are driven. Provenance, since an earlier draft of
  this bullet got it wrong: this is NEW work in r61 and not a r60 regression — r60's folds have
  four bullets and none is about a tag guard, and the suite at `30229785a` contains no `bazelrc`
  string at all. Torvalds named the gap during the r60 lap; the code closing it landed here.
  Re-measured at 28 arms, each deletion reddens its own step's arm and no other.
- **The `docker` stub mismodelled `docker logs`.** It slept for every invocation, which is right
  for the watcher's `-f` stream and wrong for the dump's `--tail` read, so the first case to leave
  a container running wedged the dump's live-container loop and hung the suite. Found by that
  hang, not by reasoning. The stub distinguishes the two now, and the case hands the container
  back GONE rather than leaving the sections below it retargeted.
- **The publish took three attempts, and each of the first two traded the defect for the same
  defect one step over.** This is the whole shape of the round, so it is written out rather than
  summarised. (1) `mkdir -p` then `docker cp … || true` publishes the directory whether or not the
  copy worked — codex's first finding. (2) Staging plus `rm -rf` + `mv` fixes that and opens an
  instant in which NEITHER directory exists; the stop step signals that loop's process group at an
  arbitrary point, so landing there destroys the last good snapshot — Graefe's blocker, and mine
  before he reported it. (3) Staging plus `cp -a` INTO the published directory closes that and
  opens a third: a host copy interrupted mid-file, by that same signal or by the runner's disk
  filling, leaves a TRUNCATED file published while the complete staging copy is then deleted, and
  `|| true` hides it — codex's finding on the delta, in the very loop he had flagged for a
  swallowed failure one round earlier.
  What ships is (4): publish by renaming staging onto a FRESH timestamped name. `rename(2)` on one
  filesystem is atomic and the target never pre-exists, so no signal can catch a half-written
  published directory; the previous generation is pruned only after the new one is in place, so at
  every instant the glob sees AT LEAST ONE complete snapshot and never a partial or absent one,
  which is what the dump needs. "Exactly one" was the first phrasing and it is false: widening the
  gap between the rename and the prune and letting the stop signal land in it shows two, measured.
  The count is not the guarantee — the completeness of whatever is there is. It also removes `cp -a` and its
  swallowed failure entirely rather than justifying them. Three arms drive what the construction is
  for — a failed copy publishes nothing, exactly one generation survives the prune, no staging
  outlives the copier — and the not-covered list says the ATOMICITY itself is relied on and not
  driven, because no arm here lands a signal inside a rename.

## Folds at r60

- **The dump extraction anchored on position, in a suite about not doing that** (Torvalds nit):
  it took the FIRST ten-space brace in the file, of which there are three, so a block added above
  the forensics step would silently reroute it while both guards still passed. Anchored on the
  step name now, with a second guard on a string unique to that block. Driven: inserting a decoy
  ten-space block ABOVE the forensics step leaves the suite green, where before it would have
  extracted the decoy.
- **The alarm below the dump was unpinned, and it is the piece that had already been wrong**
  (Graefe): the empty-capture check writes an `::error::` and exits non-zero, and an earlier
  version wrote the annotation WITHOUT the exit, so the step stayed green while announcing it had
  captured nothing. Five arms now: empty capture with the deep sweep failed, with the PAGING
  sweep failed, with both green, and evidence present from either source. Mutation-verified —
  removing `exit 1` reddens both failure arms, and reading only the deep sweep's outcome reddens
  the paging arm alone.
- **A message assertion that belonged to no case** (Graefe): the sweeper suite's disarm check read
  the shared output file after the loop, so appending a case would retarget it — the positional
  hazard this branch has spent several rounds on in prose, in a test. It is a parameter of the
  case now.
- **"The suite covers the dump block" covered two of its arms** (Graefe): the not-covered list is
  written down, first and from what was actually run — the live-container loop iterates an empty
  set under the stub, `docker exec` is refused entirely so the `df` sampler is unpinned, the
  deadline and process-group teardown are driven by hand against real processes rather than here,
  and every trace is a fixture, so this pins the plumbing and not the format it carries.

## Folds at r59

- **A leftover mutation shipped in the nightly, and it was the empty-set false green in its
  purest form** (Torvalds NAK, Graefe NAK, independently): r58 left `[ -d "" ] || continue` in
  the forensics dump's loop over the copied trace directories, where `[ -d "$d" ]` belonged.
  Always false, so the body never ran — and the `|| echo "(no fdb-logs-* directories)"` fallback
  never fired either, because the `for` still exits 0. A night with a dead cluster then reads
  exactly like a clean one. It shipped inside the commit whose entire point was letting the dump
  read that directory, and no gate caught it because the suite covered the WATCHER heredoc and
  not the dump block.

  So the suite covers the dump block now, extracted the same way, and the first thing it did was
  fail: the extracted block ends with `} > fdb-forensics.txt`, so its output lands in that file
  rather than on stdout — which is the artifact a night is actually read from, and therefore the
  right thing to assert against. Re-introducing `[ -d "" ]` reddens the trace-directory case
  alone.
- **The redundant upload glob** (both): `fdb-logs-*-exit/**` is already matched by
  `fdb-logs-*/**`. Dropped.
- **The disarm message had no arm** (Torvalds): failing closed is half of it — a permanently
  disarmed sweep that says nothing looks exactly like a healthy one, which is the shape this
  whole change is a record of. The sweeper suite asserts the string now, and silencing the echo
  reddens that arm alone.

## Folds at r58

- **The trace measurements lived in a scratch directory** (Graefe conditional, Torvalds the same
  as a nit): the numbers behind the trace rewrite — a late first file never captured, a rotated
  file never followed, the terminal line lost — rested on fixtures that no longer existed. That
  is exactly what Torvalds had just rejected for the sweeper arms, one instrument over. They are
  a committed suite now, `pkg/docscheck/rowdiff_watcher_suite.sh`, built the same way: it
  EXTRACTS `fdb-watch.sh` out of the workflow heredoc, so it cannot drift from what the nightly
  runs, and stubs `docker` so it needs no daemon.

  The suite caught something about ITSELF that matters more than the cases. Its first mutation
  run deleted the exit-transition copy and NOTHING reddened — because at the 1s interval the
  cases used, the periodic loop reached the same file, `docker inspect` succeeding on a stopped
  container. The suite was pinning "the trace is captured", not "each path captures what it
  claims". So the copy interval is now a parameter, and one case sets it longer than the case
  lives and removes the container immediately after it stops — the shape where only the exit
  copy can be the reader. Re-run: deleting the exit copy reddens that case ALONE; deleting the
  periodic copy reddens the late-file and rotation cases alone. Two paths, two mutations, no
  overlap.
- **The exit copy was described as exclusive when it is prompt** (Graefe): `docker inspect`
  succeeds on a stopped container, so the periodic loop keeps copying after exit and would reach
  the same lines within a cycle. The status line said "the only window that catches a fatal line
  written as the container dies"; the body already said the weakness is staleness. Both say
  promptness now, which is what it actually buys — and what makes it matter is that the next
  thing to happen may be a removal.
- **Two copies could write the same paths concurrently** (Torvalds): the periodic loop is still
  running when the exit transition fires, so both `docker cp` into `fdb-logs-<id>/`. The exit
  copy has its own directory now.
- **Failing closed silently is still failing silently** (Torvalds): a worker whose age cannot be
  read disarms the FDB sweep entirely, and looked exactly like a healthy run. It says so on
  stdout now, which the unit's journal keeps.
- **The clock seam, recorded rather than smoothed over** (Torvalds' verification): `ps -o etimes=`
  is boottime-derived on both sides, so a suspend is safe — but `now` is CLOCK_REALTIME, so an
  NTP STEP skews the comparison. Sweeping a live container needs `age > 1800` and
  `sepoch < wstart` together, i.e. a forward step of roughly 26 minutes mid-job, and post-boot
  chrony slews rather than steps. `docker inspect` reports `StartedAt` in realtime regardless, so
  no formulation avoids mixing the two clocks. In `infra/README.md`.

## Folds at r57

- **The worker's start time was not the worker's start time** (Torvalds NAK, and it partly
  reinstated the bug it was fixing): `stat -c %Y /proc/<pid>` reads when the procfs inode was
  allocated — first lookup after a cache miss — not when the process began, and the bias is
  late and unbounded. Measured here: `/proc/1` three seconds late, a fresh process one to two.
  Two consequences, both restoring the original incident. If the first lookup is the sweep's own
  tick, the worker reads minutes younger than it is and the job's OWN container becomes "older
  than the worker". And a dentry evicted under memory pressure and re-looked-up at hour three of
  a four-hour lane moves the apparent start to hour three, at which point the live container is
  swept. `ps -o etimes=` is the real clock. The old `|| echo 0` also failed OPEN — a worker
  present with an unreadable age disabled the guard entirely — and now fails closed.
- **The arms were driven by hand and never committed** (Torvalds): they are a test now,
  `infra/orphan_fdb_sweep_test.sh`, following the repository's own precedent for cloud-init
  shell code — it EXTRACTS the shipped script out of `cloud-init.yaml`, so it cannot drift from
  what the boxes run, and stubs `docker`, `ps` and `pgrep` so it needs neither a daemon nor a
  runner. Five cases including the fail-closed one, wired into `//infra:infra_test`.
  Two things that test caught before it was committed. Its first version of arm A put the live
  container UNDER the age threshold, where it would survive with or without the guard — a
  vacuous case, and it failed for the right reason. And the first mutation used to prove the
  suite discriminates was malformed: `perl` interpolated `$wstart` as one of its own variables
  and shipped `[ "" -gt 0 ]`, so the script never took the branch at all and TWO cases reddened
  for a reason that had nothing to do with the mutation. Re-run with the replacement escaped,
  exactly one case reddens — arm B, the orphan a blanket skip would strand — which is the
  regression the guard exists to prevent.



- **The streamed trace lost the terminal line too, and had two failure modes beyond it** (found
  by testing the fix instead of shipping it on the argument for it; Graefe then measured the
  other two independently and refused the sentence). r55 replaced a 30-second sampler with
  `tail -F` on the grounds that "an open `tail -F` is already inside the container when the line
  is written". Three measurements, each with its control:

  - Against a container that appends `Severity="40"` and exits in the same breath, the stream
    captured EIGHT of nine lines and dropped the fatal one — `docker exec`'s pipe is torn down
    with the container, so the race moved from 30 seconds to milliseconds and kept the same
    victim. A sampler that misses the event and a stream that misses the event differ only in
    how confident the comment sounds.
  - The glob expands ONCE, at exec time, and the exec fires once under the new-container branch.
    A `.xml` that does not exist yet at that instant is never captured — 0 bytes, permanently,
    for a file created one second later, against a control with the file pre-existing that
    captured 30.
  - A second trace file created after the attach is not followed at all, which is what
    ROTATION does.

  So the replacement is neither a grep nor a stream: the watcher COPIES the trace directory,
  periodically while the container lives and once more on the exit transition. Copying re-globs
  every cycle, so a late first file and rotation are handled by construction, and a STOPPED
  container still exists until something removes it, which is the window `docker cp` reads and
  a live-container read cannot. Both paths driven separately: a fixture with an empty logs
  directory for four seconds, a rotated second file, and a fatal line written immediately before
  exit is captured completely on all three counts; and a container that keeps RUNNING has its
  trace captured with zero exit-transition copies having occurred, which is what says the
  periodic path works rather than the other one covering for it.

  The one weakness is stated rather than papered over: on the path where something
  `docker rm -f`s the container outright there is no stopped state to copy from, and the record
  is then as fresh as the last periodic copy. That is precisely the path this pull request's
  sweeper fix removes — and precisely the one a future cause could restore.

## Folds at r56

- **The fourth route is pinned, not just named** (@claude, who found it independently of Graefe
  and traced further): naming `OrdinalLayoutDynamicCarrier` in the RFC left it with zero tests,
  positive or negative — and it is a different FUNCTION from the three the new pins drive, so
  neither `newPlanExprBaseForType` test touches it. It is production-reachable, since
  `proto_field_type.go` constructs an erased record type outside any test, and a leaf carrying
  one can be a union leg's child. `TestResultBaseForAQuantifierOverAnErasedChildKeepsTheFlowedValue`
  drives it, asserting both preconditions that make the route identifiable — the child's layout
  error really is `OrdinalLayoutDynamicCarrier`, and the quantifier really has a selected child
  — because a carrierless base looks the same whichever exit produced it. A concrete-child
  control takes the pass-through and publishes a layout.
  Mutation-verified, and the FIRST attempt at that verification proved nothing: removing the
  route's guard outright left `unavailable` unused, so the target FAILED TO BUILD and a grep for
  `--- FAIL` would have read as silence. Re-done as a mutation that compiles — selecting the
  route on the wrong error code — which reddens the pin and leaves the control green.
- **@claude's gate residual was already closed by r55.** It found that a job cancelled during
  the start step's ten-second pid wait yields `outcome: cancelled`, which `== 'success'` would
  have skipped. r55 had widened the same gate to `!= 'skipped'` for Graefe's version of the same
  hole, which covers this one too — three reviewers converging on one predicate from three
  different paths.

## Folds at r55

- **The sweeper guard traded one leak for another** (codex P1-class, and Torvalds had named the
  sharper rule a round earlier as "two lines you cannot afford"): a blanket "skip everything
  while a job runs" also exempts an ORPHAN left by a previous cancelled job. The timer samples
  every five minutes and a nightly lane runs for four hours, so back-to-back work need never
  expose an idle tick, and a ~700 MB container survives the whole night — which is the leak the
  sweeper exists to prevent. It compares start times now: a container newer than the running
  `Runner.Worker` belongs to the job in flight, one older than it is a real orphan and stays
  eligible. The two lines were affordable after moving prose to `infra/README.md`, which is what
  the budget gate's own failure message tells you to do. Four arms driven, including the mixed
  live-and-orphan case that motivated it.
- **The trace sampler could not capture the event it exists for** (codex): when fdbserver dies
  on the fatal I/O failure, the image's foreground process exits and the container stops at
  essentially the same instant — so every `docker exec` after that boundary fails, the last
  promoted snapshot is the PRE-crash one, and ENOSPC stays indistinguishable from any other I/O
  error. That is the whole question the dump was added to answer. It streams now
  (`tail -F` opened while the container is alive) instead of sampling every 30 seconds.
- **The gate had a hole in the OTHER direction, and the looser version covered it** (Graefe
  conditional, codex independently, and both were right): the start step launches the watcher
  with `setsid` and only THEN can fail its pid handshake or be cancelled, so
  `outcome == 'success'` skips cleanup for a process that is demonstrably running.
  `!= 'skipped'` is exact rather than lucky — `skipped` is the only outcome meaning no line of
  that step ran. All six paths walked, and the docscheck gate's `want` moved with it.
- **The gate's own classifier had load-bearing, undocumented ordering** (Torvalds): the START
  step's body also contains `fdb-watch.pid` and `kill`, so a first-match-wins switch put it in
  the stop arm if the cases were ever reordered. Keyed on markers unique to each step now, with
  a population guard demanding EXACTLY one of each rather than at least one — a second match
  means the marker stopped identifying what it names. The `/proc` ownership check is pinned too;
  it had none.
- **Two sweeps and two message fixes**: the retracted "prints the figure on every run" survived
  in two RFC copies after only the README was corrected — swept to zero with a control; the
  ownership check's comment claimed to prove the pid IS the watcher when it proves it is A
  watcher (a sibling lane's would match, which is fine for what it is for); the start step's
  failure message said "do not leave it running" while leaving it running, and now says what is
  true — the watcher is already detached, its deadline bounds it, and the cleanup step gates on
  this step not being SKIPPED precisely so it still gets a chance; and the erased-record pin's
  type mismatch rendered "RECORD, want RECORD" because `%s` carries neither the Go type nor
  nullability.
- **The route list is short for the third lap** (Graefe): `OrdinalLayoutDynamicCarrier`'s early
  return hands back `RequireFlowedObjectValue()` itself. It satisfies the invariant trivially,
  which is exactly why the invariant was the right thing to state — so the list now says
  "instances include" and stops pretending to be closed.

## Folds at r54

- **"Stop what this job started", said correctly on the third attempt** (Torvalds and Graefe
  independently, and the same path found by walking them here before either landed): a plain
  `if:` is implicitly `success() && …`, so on a Checkout FAILURE with the window open the start
  step is skipped while an `always() && window.ok` stop step still runs — and `git clean` has
  not run either, so last night's pid file is sitting there to be signalled as a process group
  on a persistent box. The predicate the first two attempts were approximating is the start
  step's own outcome, and the gate says that now. Belt to those braces: before signalling,
  the step reads that one `/proc/<pid>/cmdline` and declines if it is not the watcher — a
  question about a process we named, not the `pkill -f` self-match this branch has already been
  bitten by twice. Driven: the real watcher, a stranger process, and a dead pid.
- **The gate is pinned where the next reader will trip over it** (Graefe's suggestion):
  `pkg/docscheck` already parses this workflow, and `TestRowdiffWatcherStopIsGatedOnTheStartStep`
  now fails the build if the stop step stops gating on the start step's outcome, if it loses
  `always()`, if the start step loses its `id`, or if either step is renamed out of recognition
  — that last one being the population guard, without which the test would pass over an empty
  set, which is the failure it exists to prevent wearing a different hat. All four arms driven
  by mutation, each with the mutation proven present first and each producing its own message.
  This is worth a gate rather than a comment because the failure has no symptom in the run that
  causes it: the damage lands on a different job, on a different night, on the same box.
- **An untested branch, found by an enumeration being wrong twice** (Torvalds): the carrier
  substitution had a third route — `newPlanExprBaseForType`'s erased-record fallback, reached
  when the identity layout REFUSES the type, minting a bare QOV whose identity rests on
  `SnapshotExactType`. `isExactErasedRecord` had four call sites in production and zero in any
  test. It has two now, mutation-verified: inverting the predicate reddens the erased pin and
  leaves its concrete control green, which is what says the control separates the exits rather
  than echoing them. § Test plan item 1 stops enumerating routes and states the invariant they
  share instead, since two laps enumerated and both were short by one.
- **A visibility claim about a test's own output** (Torvalds nit): the headroom figure is
  computed on every run but printed only under `--test_output=all`, a passing Go test's `t.Logf`
  being hidden otherwise. Said properly now, in the one place that tells a reader where to look.

## Folds at r53

- **The stale-pid fix was in the wrong step, and was a no-op in the right one** (Torvalds NAK,
  Graefe the same as a residual): r52 put `rm -f fdb-watch.pid` in the step that STARTS the
  watcher, which is gated on the window being open — so on the closed-window path, the one the
  comment beside it cites as what makes the hazard reachable, it never runs. And on the open
  path it is redundant: `actions/checkout@v5` defaults to `clean: true`, a `git clean -ffdx`
  that has already removed the untracked file. A fix that executes only where it is unnecessary.
  The real fix is one line on the STOP step — `always() && steps.window.outputs.ok == 'true'` —
  so there is nothing to stop on a night nothing was started. The pre-launch `rm` is gone and
  the reason it would be a no-op is written where it was.
- **A byte count that went stale inside the round that wrote it** (Graefe): `infra/README.md`
  and the RFC both said the guard left 13 bytes of `user_data` headroom. A later trim in the
  same round moved it to 30 and neither copy was re-measured — and the README's derived
  instruction, that the next added comment would fail the test, became false with it. Both now
  point at `//infra:infra_test`, which computes the figure on every run and prints it under
  `--test_output=all`, and say what does not
  change: the budget has about one line left in it.
- **The carrier substitution under-enumerated by one** (Graefe half-nit): there is a second
  route, `newPlanExprBaseForType` → `newIdentityOutputLayout`, type-identical by construction
  rather than by an `Equals` check. The conclusion survives both routes; "that branch" named
  only the first.

## Folds at r52

- **The deadline bounded the parent and leaked its children** (Torvalds NAK, measured by him
  and re-measured here): `docker logs -f` and the trace poller are the watcher's children with
  no deadline of their own, so a deadline expiry with no cleanup step left them running — the
  same leak one level down, in the fix for the leak. The loop now signals its own process group
  on the way out. Driven: three children alive before a 12-second deadline, all gone after,
  with no cleanup step and each checked by `kill -0` on its own pid.
- **The deadline outlived the job it claimed to be inside** (Torvalds): 18600s is 310 minutes
  against a `timeout-minutes: 300` cap, so the sentence "cannot outlive the job" was false by
  ten minutes plus however late the watcher starts. 17400s now, which is inside the cap with
  the start offset on the right side of it.
- **A stale pid file on the skipped-window path** (Torvalds): the stop step is `always()` and
  ungated, and when the nightly window is closed `Checkout` is skipped — so the workspace still
  holds LAST night's `fdb-watch.pid`, and the step would signal a recycled process group on a
  persistent box. Removed before launch and after stopping.
- **"One copy of the tag, not a third" was one copy short** (Graefe): the forensics loop twenty
  lines below it still hardcoded `:7.3.77`. A stale ancestor filter emptying a loop is the
  precise failure that step exists to report, so it now derives the tag the same way and fails
  loudly when `.bazelrc` carries none.
- **The accessor claim over-reached by one hop** (Graefe): legs 1..n call
  `GetFlowedObjectType()` directly, leg 0 reaches it through `RequireFlowedObjectValue` and may
  have the provided layout's carrier substituted — under that branch's own `Equals`. The
  conclusion stands; the hop is named now instead of glossed as "the same accessor".
- **The guard nearly shipped matching argv instead of the process** (self-caught, then
  confirmed by both reviewers): `tools/bazelscaleset`'s cmdline matcher carries a
  `dotnet Runner.Worker.dll` case, which reads like a description of the fleet and is a
  defensive case in a test table. Cloud-init installs the official `actions-runner-linux-x64`
  tarball, which ships `Runner.Worker` as an apphost, so `comm` is `Runner.Worker` and `-x` is
  right. An intermediate version used `-f` on the strength of that table — and while it was
  being tested, `pgrep -f 'Runner[.]Worker'` on this dev box reported a worker present with no
  runner on it at all, because an unrelated agent's own `grep -E 'Runner\.(Listener|Worker)'`
  was in its argv. The hazard is not theoretical and the `README` records it. What the three
  arms establish is the guard's LOGIC against a stand-in; that a real worker's `comm` matches
  rests on the packaging, and the timer has still never been caught firing — one
  `journalctl -u orphan-fdb-sweep.service` at a recorded death timestamp turns the inference
  into an observation.

## Folds at r51

- **The thing this RFC's own investigation was looking for, found by a gate reading `infra/`**
  (codex P1). r50 said the rowdiff forensics step captured nothing because "the sweep binary's
  own testcontainers cleanup" had removed the container. That is wrong, and the right answer is
  the root cause of a defect booked for weeks as `NO MECHANISM IS ESTABLISHED` with two refuted
  hypotheses and a STOP saying nothing in this repository could observe or change it.
  `orphan-fdb-sweep.sh` — written by `infra/cloud-init.yaml`, enabled on every provisioned box
  — runs every five minutes and `docker rm -fv`s any FoundationDB container older than 1800s,
  on the premise that "no per-test container lives that long". Rowdiff, stress and factory each
  hold one for hours. 1800s plus the timer's phase IS the measured 30–35 minute band, and it
  accounts for every observation the memory hypotheses could not: the container GONE rather
  than exited, `OOMKilled=false`, a fresh dial blackholing instead of being refused, tracking
  elapsed time rather than work done, 2.8% across five hosts, and never reproducing on the dev
  box — which was read for weeks as "more RAM" and was really "no sweeper". Corroborated by a
  boundary nobody had looked for: green for its first eleven nights, red every night from
  2026-08-01, the day the pool was provisioned. Fixed at the source with a start-time comparison
  against the running worker, every arm driven and committed as a test
  (`infra/orphan_fdb_sweep_test.sh`); the reasoning is in `infra/README.md` because the 32 KiB `user_data`
  budget is enforced by a test and the guard left a line or so of it — `//infra:infra_test`
  computes the exact figure on every run and prints it under `--test_output=all`, which is where
  to read it, since it moved twice inside this round and
  both prose copies went stale for a round. The STOP is withdrawn, and what remains is a
  deployment rather than a diagnosis.
- **Three mechanical defects in the watcher** (Torvalds NAK): it leaked — `while true` plus
  `nohup` on a PERSISTENT box, with "the runner reaps it when the job ends" asserted rather
  than measured, in a delta whose thesis is that unmeasured claims rot. It now records its own
  pid (a session leader under `setsid`, so pid == pgid), a cleanup step kills that group on
  `always()`, and a deadline bounds it if the step never runs. The empty-capture alarm read
  `steps.sweep.outcome` only, so a death in the PAGING lane with the un-paged lane green
  printed "nothing to explain tonight" — the failure the alarm exists to kill, one lane over;
  it reads both now. And `fdb-last-inspect.txt` was clobbered by the paging lane's healthy
  container, replacing the dead one's inspect and then silencing the alarm with a non-empty
  file; it is per-container, as the log file already was.
- **The alarm annotated and stayed green** (codex P3): an `::error::` workflow command writes
  an annotation and changes nothing about the exit status, so the step reported success while
  announcing it had captured nothing. It exits non-zero now.
- **The trace evidence was not preserved** (codex P2): `exit=1 oom=false` cannot separate
  ENOSPC from any other I/O failure — the fdbserver trace XML can, and it cannot be read after
  removal. The watcher now snapshots `Severity="40"` events and `/var/fdb/data` usage while the
  container is alive, promoting a snapshot only when the exec SUCCEEDED, so an empty result
  from a healthy read stays distinguishable from a failed read.
- **A whole analysis of undeployed code** (codex P2, and it is the sharpest one): r50's
  coverage-nightly booking traced the "runner has received a shutdown signal" kills to
  `Scaler.shutdown()` — the only SIGTERM path, no busy check, grace flag reading "before
  SIGKILLing in-flight runners". It fits perfectly and it cannot be the sender, because
  `tools/bazelscaleset` is not deployed: `runner_mode` defaults to `classic`, cloud-init
  disables, deletes and masks the unit, and `infra/README.md` records zero units fleet-wide.
  Withdrawn, with the reasoning kept so it is not re-derived, and the DONE criterion rewritten
  to name the service that IS deployed. `infra/README.md` warns one line above that this
  fleet's naming has already cost one investigation an evening; this is that mistake wearing a
  different label. Read the deployment before reading the code.
- **The superseded coverage diagnosis, at the site a triager reads** (codex P2):
  `nightly-coverage.yml` still said those runs were "cancelled from outside, the same
  runner-host class that kills the FDB container". Both halves are now false. Corrected there,
  not only in `TODO.md`.
- **Graefe's sharpening, and one more count** — § Test plan item 1 now says that
  `plannerFuzzSameResultType` and the union constructor use the SAME accessor and the SAME
  predicate (`GetFlowedObjectType().Equals`), which closes the argument instead of making it
  analogical; and the comment's "turned up the rest" became "the others below", a completeness
  claim one sentence after "a sample and not a census". The pin loop gains `b91c5c86c` (region
  9, whole file 16) and says that it is stale by construction — it can never name the head it
  ships at, which two rounds of reviewers have now supplied.

## Folds at r50

- **The fix for a scope claim became one** (@claude NAK, and the only NAK of the round): r49
  added a paragraph separating positional pointers that merely DIRECT a reader from those that
  ASSERT something about the row they point at — the second kind flipping from true to false on
  an insertion with nothing inconsistent left on the page. It then said there were TWO such
  kinds and named the comparative and the forward-attributing. That sentence was written by
  describing the two examples in hand rather than by asking the question of every row, which is
  the failure mode this whole document is a record of, committed inside the fold that exists to
  fix it. Asking the question finds an interpretive third ("the contrast that shows the row
  above is really measuring the erasure") and, once looked for, a backward-attributing fourth in
  three spellings — causal, by origin, and by equality. r50 stops classifying: the paragraph now
  leads with the QUESTION ("would this sentence still be true if the row it points at were a
  different row?"), gives the spellings as worked examples of applying it, and says in the text
  that they are a sample and why. A question cannot be undercounted; a taxonomy invites a count,
  and every count this comment has carried has gone stale.
- **A figure reported without being taken** (Torvalds NAK-worthy nit, Graefe the same finding):
  r49's own paragraph about not trusting an unscoped count wrote a THIRD counting into itself,
  attributed to a reviewer and never run here. Run: the bare-token pattern
  `above|below|next two rows` over the same region gives eleven occurrences on ten lines, not
  nine. Deleted rather than corrected, with the reason kept — because the failure is not the
  wrong number, it is having published someone else's measurement as if it were one of ours,
  inside the paragraph that exists to forbid exactly that.
- **The pin omitted the head it shipped at** (Torvalds): the loop now runs over three SHAs, and
  the whole-file column is reported beside the region one precisely because it moves — 9/11/14
  against 9/9/9 — as each round's comment quotes more of the pointers it disclaims.
- **An unresolvable pointer, added by the commit that documents the hazard** (Torvalds): r49
  wrote "the table above's last row" pointing at the `physicalPlanColumnNames` arms; the nearest
  table above is the corpus census, whose last row is `**total**`. It was wrong the day it was
  written, and it is an ASSERT-class pointer of exactly the kind that same commit had just
  taught the reader to distrust. The arm is named now, which an insertion cannot retarget.
- **A truncated transcript** (Graefe): the quoted failure dropped the trailing ` NOT NULL` from
  quantifier 1's type. Restored — a transcript that has been tidied is not a transcript.
- **The entry proves more than it claimed** (Graefe, offered and taken): `buildFuzzExpression`
  only builds the union when `plannerFuzzSameResultType(ql, qr)` holds, so the legs' types are
  already `Equals` before any rule runs. The second alignment is therefore not re-deriving a
  property and agreeing with itself — it MANUFACTURES a disagreement that did not exist. That is
  this RFC's thesis as an experiment rather than an argument, and § Test plan item 1 says so now.
- **Run-specific figures presented as facts about the target** (Graefe): the input sizes. The
  nightly minimised 113 bytes, the run behind the committed entry 43, an independent reproduction
  51. Only the six committed bytes are a fact about the target; the rest are named so a later
  run reporting a different one is not read as a discrepancy. The same treatment is applied to
  "the nightly is red on X", which is now the run id and the date it was true of.
- **A retry broader than its fault** (Torvalds): the pin-bump remedy removed the whole
  `cache/vcs` directory to discard one poisoned clone, charging every other module a re-clone.
  `go`'s own error names the path, so only that path is removed — and when it names none, this
  is not the shallow-clone fault and the step fails rather than widening the delete.

## Rides alongside, not part of this RFC

The engine fuzz nightly was red for a second, unrelated reason: `FuzzRebaseValue_NoPanic` built
its alias map from two fuzzed strings and `t.Fatal`ed when `NewAliasMap` refused an empty name
as a zero correlation — reporting a guard as a crash. That fixture fix and its corpus entry
`d442d027a0e3b992` land in the same pull request as their own commit; nothing in this RFC's
mechanism depends on them.

## What this does not close

- `planColumnNamesWithMD`'s descriptor-name fold and the aggregate-index / streaming-aggregate
  output-name arms are deleted here only because their sole caller is. Whether the aggregate
  cursors' `CanonicalAggColumnName` is itself an RFC-237 second fold is a separate question with
  its own census; it does not compare legs and is out of scope.
- The two `EqualFold` lookups in `rule_implement_in_union.go:130` and `physical_key_types.go:295`
  are identifier LOOKUPS against physical field names, the class RFC-237 §Scope permits, not
  presentations. Not touched.
- Two reads of a gathered multi-source unnest star body (`SELECT * FROM a, b, a.arr AS x`),
  both pre-existing at the merge-base in the same form: an aggregate over the DERIVED spelling
  (`… FROM (…) d GROUP BY d.aid`) is refused at execution as an undeclared binding of `D`, and
  a WHERE over the CTE spelling (`WITH d AS (…) SELECT d.aid FROM d WHERE d.aid = 1`) does not
  plan. The CTE spelling stays out of the global scope by shape (§ Fix F) and an aggregate over
  it answers through the translator's seed bake; the derived spelling has no such arm, and the
  translator already books the projected-output-layout ordinalization it needs
  (`translateAggregate`, the positional-gather comment). A projection or a WHERE over the
  derived spelling answers, as before. Recorded in `TODO.md` ("Exact quantifier binding over a
  CTE or derived body") with the measurements. Two more of the same class, over row-versioned
  tables and pre-existing at the merge-base in the same form: a WHERE over the derived star
  join (`edge lookup D: read as RECORD(ID,Y,ID,Z), declared RECORD(AA.ID,Y,BB.ID,Z)` — the
  row-version rewrite has already produced Java's explicit projection, and that projection
  names its slots by the leg-qualified datum key while the scope's walk publishes bare names),
  and a star over a lateral unnest (the rewritten projection carries the outer column beside
  the element, as the top-level star does, while the derived unnest scope shadows it). Both
  are in the same `TODO.md` entry with their measurements.
- A sort over a derived table's or CTE's column is still never answered by an index, nor a DESC
  over its primary key by a reverse scan: the constraint now crosses the projection (§ Third
  adjacent finding) but the scan group's leaf match never climbs to the candidate's sort
  (`correlatedToEquals`), so the data-access rule has no matched ordering parts to satisfy it
  with. `TODO.md`, "Ordering through a projection reaches the
  child group but not the index", with the measurement and the Java mechanism that closes it.
- A struct member declared with a dot in its name labels as `b`, not `a.b`, over the base table
  and through a derived table alike: RFC-238 §2's `qualifierStrippedLabel` residual, pinned on
  the derived shape this RFC admits (`TestFDB_QuotedDotNestedMemberLabel`).
- An array literal with a NULL element (`[x.id, NULL]`, Java's `maximumType(LONG, NULL)`: a
  NULLABLE element) cannot be published by the exact derivation, because `semantic.Column`
  carries the array container's nullability and no element bit; a join-bodied CTE projecting
  one has no publisher and every read of it hits the loud floor (0AF00), identical at the
  merge-base. It is the specimen the loud-floor pins use now that a nominal record publishes
  (`TestOrderByExactMetadata_UnderivableCTEComputedProjectionStaysLoud`). RFC-232's bridge
  residual; `TODO.md`, "An array literal with a NULL element cannot be read through a CTE or
  derived table", with the two closures.
- A table with a fieldless nested-message column (Java-authored metadata; this DDL cannot
  declare one) cannot be queried at all — `SELECT t.sk FROM t` is 42703 with the column `e`
  present and plans without it — because `expr.structColumnType` turns a fieldless record into
  UNKNOWN and the flowed row then resolves nothing. Identical at the merge-base; surfaced by the
  r15 measurement of an exact-derivation decline beside a nested path. `TODO.md`, "A table with
  a fieldless nested-message column cannot be queried at all", with the reproducer and the
  closure.
- An enum field is typed STRING by the exact derivation (`sqlTypeToCascadesType("ENUM")`), one
  layer before RFC-232's carrier gap; `TODO.md`, "The exact derivation types an enum field as
  STRING", pointing at the pin that goes red when it closes.
- The nightlies red for a runner-host reason — the FDB container disappearing about thirty
  minutes into every Docker-backed job — need host access and are escalated to the owner as a
  STOP, not filed. Re-measured for r50 rather than repeated: RowDiff T+30m48s (run
  34011921618), Stress T+33m13s (run 33955095551), Factory's growth lane T+33m47s, each first
  announced by `WARN fdbgo: connection to server failed`, all beside the booked eight-night
  band — beside rather than inside, because only RowDiff has a `random_seeds` RUN line to time
  from and the other two are timed from job start, so they are upper bounds on that quantity
  rather than further samples of it. `TODO.md` carries the same distinction. Two claims that rode along with this bullet did NOT survive re-measurement and are
  corrected in `TODO.md` with their run ids:

  - The Coverage lane is **not** the same class. Six for six since 2026-08-31 it ends with "The
    runner has received a shutdown signal" — the RUNNER stopped, not a container dying and not
    an external cancel — and runner lifetime is owned by `tools/bazelscaleset`, which is in
    this repository. So that half of the STOP is withdrawn: it may well be ours. (An earlier
    draft blamed the lane's job timeout; Torvalds's delta lap measured the durations and
    refuted it, and the cap is back at its value with the measurement in its comment. This is
    the same claim measured one step further.)
  - "The ONE repository-editable cause" is one short. Besides the bot pin-bump PR's held runs,
    #745 (`factory/batch`, also bot-authored) is DIRTY as read on 2026-09-06, so GitHub cannot
    compute its merge ref and its `pull_request` workflows cannot fire while it stays that way
    (an earlier draft said "since 2026-08-12" and "never fire at all"; 2026-08-12 is the PR's
    createdAt, and six checks did run on 2026-08-29 with `Build, Lint & Test` failing) —
    a different face from #769's, and Reconcile stays red on it even after the dispatch fix
    here lands.

  The dispatch fix itself has never executed and cannot yet be judged:
  `git show origin/master:.github/workflows/frl-pin-bump.yml | grep -c "Dispatch the pull
  request's checks"` is 0, and the last nightly pin-bump run carried no such step. Reconcile
  still listing the bot PR ABSENT is therefore not evidence against it.
