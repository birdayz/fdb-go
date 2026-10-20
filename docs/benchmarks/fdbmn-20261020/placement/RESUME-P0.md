# Same-attempt1 P0 settle continuation

Owner ACKs4200/4204 preserve the interrupted attempt and original settle clock; ACK4207/4211 retain the original conditional attempts2/3. This is an external orchestration correction only, not a harness/production/binary or timing change.

## Corrected loop

`p0.py` now treats the explicit QoS backlog/missing-moving-data errors as **pending**, never as zero. Every such sample resets the continuous healthy-zero interval. Role identity/generation, role/configuration, DD mode and recovery errors still abort. Success requires120 continuous seconds of valid health and known zero movement, before the original deadline. `p0-settle-correction.diff` records the change against the original deployed script.

## Client invocation

Keep `p0.py`, `resume_p0.py` and unchanged `placement_checked.py` together at their original client paths. Review the literal script hashes in `resume-command.sh`; retrieve/hash the preserved original `R/provenance/p0/invocation.json` in the parent-controlled read-only preflight, and pass that literal as `PRIOR_INVOCATION_SHA256`. The ready command is:

```sh
PRIOR_INVOCATION_SHA256=<preflight-literal> bash /srv/fdb-bench/execution/placement/resume-command.sh
```

The script contains the exact four original F0 literals from `provenance/p0-literal-inputs.json`, the acknowledged original/new p0.py hashes, original root `R/provenance/p0`, and fresh sibling output `R/provenance/p0-continuation-1`. It never executes qualification, fixture seeding, or ballast writes for attempt1 again. Existing output is refused.

The deadline is **original attempt-1/settle-0000/command.json.started_epoch + original1200 seconds**, not continuation start+1200. Fewer than120 seconds remaining refuses before any cluster command; expiry during settling stops instead of resetting/retrying. Resume reads the historical state, validates exact original top/attempt/store boundaries with no later DD-off/clear/canonical/attempt, validates complete qualification+controls+defects/clears, original seed argv/environment/audits, all three exact256MiB completed writes and command literal pins, and allows only the acknowledged p0.py hash transition. The original output tree is hashed and checked read-only throughout.

After successful settling, DD is frozen once. Before clearing **any** store, existing CGo PP performs frozen read-only physical censuses of **all3** stores: exact canonical fixture keys plus all2685 raw ballast keys each, positive version, complete boundaries/scans, correct fixture hash/cluster, transactional addresses, two known storage-role addresses and preserved DD/lock/keyServers provenance. These continuity diagnostics are not canonical placement receipts or selection evidence. The full raw probes are retained. Only then does original ballast clear/verify/canonical logic continue. Lock owner/token, per-store raw keyServers digest, role identity and generation must match from preclear census through canonical/final probes.

If attempt1 fully passes, it is selected. If it is integral partial, the ordinary original attempts2/3 run at512/1024MiB, first all-pass else attempt3. No manual best-of selection, replay of attempt1, or integrity-error retry occurs. Final exit0 is all-pass; exit3 is integral partial selected attempt3; exit2 is refusal/failure. No DD-on cleanup trap exists.

`continuity.json` records the old tree hashes, invocation/script literal hashes, original deadline and correction. Final `seal-pins.json` includes continuity/resume code and all historical artifacts. Use **`--p0-root R/provenance/p0-continuation-1`** for later `seal_live.py` calls and pass the new root's `placement-command.json` to R2/controller. The failed original root remains historical evidence, not selected output.

## External verification

- `verification-resume-final.log`:8 passing fake-clock/archive tests, including transient missing telemetry/QoS resets, original-deadline exhaustion, hard guard failures, exact fixture+ballast census, immutable oldtree, DD-off/clear/later-attempt boundary refusal, preclear→canonical epoch matching, explicit Resume.execute first-pass/second-pass/all-partial/integrity-stop branches.
- `verification-p0-settle-correction.log`: all12 prior external P0 checks pass after the loop/refactor.
- `resume-command.sh` passes shell syntax validation.

No cluster/cloud/SSH commands or real benchmark were executed during generation/testing. Actual continuation is parent-controlled under the original operational/provider deadlines.
