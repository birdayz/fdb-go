# RFC-263 evidence

Evidence for [RFC-263](../../../rfcs/263-transaction-timer-and-incarnation.md), transaction timeout queues and read incarnations. Revision 6's design is accepted and its B1 implementation is complete, pending merge gates. The fixed confirmation cleared the recorded 8-worker p999 blocker. The separate A1/A2 CPU qualification and queue-wait diagnostics also pass their recorded rules; full/full-race, all five final-HEAD reviews, CI and owner approval remain open. Historical prototypes, exploratory timing, instrumented diagnostics and confirmation are separate populations.

## Revisions 5 and 6

All ten revision 5/6 write-ups are indexed below. Raw artifacts remain outside Git; these reports record their identities and reproduction commands.

| Path | Content |
|---|---|
| [rev5-draft-history.md](rev5-draft-history.md) | Historical pre-pricing renewal draft, retained as review history rather than current approval. |
| [rev5-evidence.md](rev5-evidence.md) | Disposable weak-association renewal proof, mutation, allocation cost and 42-run pricing screen. |
| [rev5-historical-tail.md](rev5-historical-tail.md) | Retained-data checks of earlier range-tail populations; no new benchmark or pooled timing. |
| [rev5-tail-plan.md](rev5-tail-plan.md) | Historical diagnosis plan separating timing, traces and detector-only intervention. |
| [rev5-tail-results.md](rev5-tail-results.md) | Proven R5 weak-promotion GC gate, detector-only intervention and unresolved historical residual. |
| [rev5-trace-attribution.md](rev5-trace-attribution.md) | Separate 32-worker diagnostic mechanism and bounded per-sample trace alignment. |
| [rev6-tail-hypotheses.md](rev6-tail-hypotheses.md) | Historical source-based 8-worker hypotheses and callback-exposure bounds, not attribution. |
| [rev6-production-evidence.md](rev6-production-evidence.md) | Production contract, canonical regressions, compiled mutations and actual/cached fast-lane results. |
| [rev6-production-tail-results.md](rev6-production-tail-results.md) | Exclusive production screen, original adverse get/set findings and actual 8-worker range traces. |
| [rev6-confirm-results.md](rev6-confirm-results.md) | Fixed-population p999 clearance, power/epoch/order limitations and sealed artifacts; other gates remain open. |
| [pr-body.md](pr-body.md) | PR description with separate findings, scoped p999 clearance and open merge gates. |

### CPU qualification

| Path | Status |
|---|---|
| [rev6-cpu-plan.md](rev6-cpu-plan.md) | Fixed dual-endpoint CPU rules and separate queue-profile protocol; completed results are reported separately. |
| [rev6-cpu-a2-grv-confirmation.md](rev6-cpu-a2-grv-confirmation.md) | Outcome-informed grv-w8 confirmation, sealed before sampling; supersedes only two A1 bounds, without pooling. |
| [rev6-cpu-results.md](rev6-cpu-results.md) | All sixteen original A1 bounds, superseding A2 result, descriptive A/A controls, passing queue-wait diagnostics, sealed receipts and retained caveats; not merge approval. |

## Earlier evidence

| Path | Content |
|---|---|
| [rev4-screen.md](rev4-screen.md) | Selected variant (b), its acceptance screen, rejected screens and CPU/GC attribution. |
| [rev4-gc-and-selection.md](rev4-gc-and-selection.md) | Why fixing contention alone failed; GC cost of the candidates; selection. |
| [rev4-reclaim.patch](rev4-reclaim.patch), [rev4-reclaim-mutant-no-cleanup.patch](rev4-reclaim-mutant-no-cleanup.patch) | Source of the selected prototype and its cleanup-removal mutant. |
| [rev4/](rev4/) | Analysis summaries cited by the write-ups (`screen*-analysis.json`, `range*-analysis.json`, `queue-wait-analysis.json`, populations, exclusive CPU partition). |
| [phase1/](phase1/) | Phase 1 primary and contention analysis summaries. |
| [RAW-ARCHIVES.sha256](RAW-ARCHIVES.sha256) | sha256 of the source archives and benchmark binaries of each arm (not in Git). |

Raw samples, pprof profiles, gctrace dumps and build logs are not in Git. The analysis scripts are in [`tools/benchcampaign/campaigns/rfc-263`](../../../tools/benchcampaign/campaigns/rfc-263/); they read the evidence directory layout described in the write-ups.

[Reproduction and source identities](reproduction.md) distinguish unchanged historical as-run sources from the portable entrypoints and pin analysis/decoder commands.
