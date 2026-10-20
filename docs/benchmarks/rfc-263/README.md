# RFC-263 evidence

Local screens behind RFC-263 (transaction timer and incarnation). The design is
under review; the RFC text is on its own branch. Evidence for later revisions is
added to this directory as it is produced.

| Path | Content |
|---|---|
| [rev4-screen.md](rev4-screen.md) | Selected variant (b), its acceptance screen, rejected screens and CPU/GC attribution. |
| [rev4-gc-and-selection.md](rev4-gc-and-selection.md) | Why fixing contention alone failed; GC cost of the candidates; selection. |
| [rev4-reclaim.patch](rev4-reclaim.patch), [rev4-reclaim-mutant-no-cleanup.patch](rev4-reclaim-mutant-no-cleanup.patch) | Source of the selected prototype and its cleanup-removal mutant. |
| [rev4/](rev4/) | Analysis summaries cited by the write-ups (`screen*-analysis.json`, `range*-analysis.json`, `queue-wait-analysis.json`, populations, exclusive CPU partition). |
| [phase1/](phase1/) | Phase 1 primary and contention analysis summaries. |
| [RAW-ARCHIVES.sha256](RAW-ARCHIVES.sha256) | sha256 of the source archives and benchmark binaries of each arm (not in Git). |

Raw samples, pprof profiles, gctrace dumps and build logs are not in Git. The
analysis scripts are in
[`tools/benchcampaign/campaigns/rfc-263`](../../../tools/benchcampaign/campaigns/rfc-263/);
they read the evidence directory layout described in the write-ups.
