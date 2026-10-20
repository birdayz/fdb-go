# Authorized already-frozen attempt1 continuation

Root4218/4221 authorizes abandoning the **added nonprotocol full-ballast census** after it read small successfully but split failed with transaction_too_old1007 on roughly368MiB under the5s MVCC horizon. The original120s healthy-zero settle **already completed123.849s before its original1200s deadline**, DD was disabled, and **no ballast clear occurred**. This is not a failed canonical placement probe and does not authorize reseeding, mixed read versions, new PP/binaries, or a restarted settle clock.

## Ready client command

Copy the reviewed current `p0.py`, `resume_p0.py`, and `frozen-command.sh` together at the existing client paths. Parent-controlled read-only preflight supplies the two preserved metadata literals:

```sh
PRIOR_INVOCATION_SHA256=<original-provenance/p0/invocation.json-literal> \
PRIOR_CONTINUITY_SHA256=<provenance/p0-continuation-1/continuity.json-literal> \
  bash /srv/fdb-bench/execution/placement/frozen-command.sh
```

The script has the exact original four F0 literals, all original/failed/current p0 source ACKs and failed resume source ACK. New output is the fresh sibling **`R/provenance/p0-continuation-2`**. Current reviewed code:

- p0.py: `f40d150614377f9a896b95cc0973cbad8d5c8bf17bb22a3e5565f0bec2939c84`
- resume_p0.py: `a40c682317556bbf42936d5ee9f0c7464bd92eadeec94f3f530355e835f295af`

The explicit `--frozen-after-preclear-1007` entry requires the preserved original root and first-continuation root, literal continuity/source acknowledgements, completed on-time settle/DD-off receipts, valid small census, exact split read-only scan1007 failure, and no prior clear/canonical/reenable/later work. Both old trees are hash-pinned/read-only. It does **not** require120s remaining now: the interval was already completed. It does not rerun settle or toggle DD.

A small empty owned control-small PP read establishes **DD/provenance only**: complete transactional snapshot at positive V1/V2, mode0 present, owner/write token matching the successful small census, nondecreasing versions, and unchanged role/generation. It is explicitly labelled not-placement and is never used as a canonical receipt. No scratch writes occur. The original sequence then resumes: clear only each store's ballast, verify empty at positive RV, strict canonical PP/receipts/offline controls for every fixture, and final stable receipt sweep. All canonical/final probes must share the global lock epoch/role identity; the successful old small keyServers digest is compared **only** with benchmark small, never control-small or other stores. Split/planskip extra preclear censuses are recorded as abandoned, not completed.

If attempt1 is integral partial, original conditional attempts2/3 remain authorized at512/1024MiB with first all-pass else attempt3. Integrity errors stop; no restart of attempt1 or hand-picked best result is permitted. Final DD remains off through M2. Use **`--p0-root R/provenance/p0-continuation-2`** for `seal_live.py` and that root's `placement-command.json` for R2/controller.

## Verification

The actual empty-snapshot method, changed-epoch refusal, no-settle/no-DD-toggle frozen branch, original hard guards/timer, physical census validators, immutable-history checks, and conditional attempts are exercised offline. The nominal5s poll interval is **not** converted into an invented minimum25 polls: real command/hash overhead may yield fewer polls while the authenticated120s interval remains valid. No real subprocess/cluster/SSH/cloud commands ran during generation/tests. Independent reviewer ACKed exact current hashes and fixed the empty-probe parser finding before use.
