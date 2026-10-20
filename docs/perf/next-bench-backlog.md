# Next benchmark campaign: backlog

## Mass multi-tenant (requested 2026-10-20, after fdbmn-20261020)

Not part of fdbmn-20261020; that campaign is sealed and preregistered.

Tenant kind (decided by the lead while the owner was away, 2026-10-20):
- First: (a) FDB native tenants (7.3 tenant API), Go vs libfdb_c.
- Later, as a separate track: (b) record-layer / SQL many-store multi-tenancy, Go only.

Planned shape:
- Tenant counts: 1k / 10k / 100k.
- Access: uniform and skewed (hot tenant).
- Operations: get / set / range per tenant.
- Arms: Go master vs Apple binding over libfdb_c 7.3.77.
- Measure: client CPU, latency, memory growth of the tenant location and metadata caches.

Order of work:
1. A reviewed harness PR adding the workload to tools/benchcampaign/multinode.
2. A local prototype on the tmpfs cluster with tenant mode on; include it in RFC-263's screen.
3. A short preregistered Hetzner follow-up campaign, reusing the fdbmn teardown and inventory tooling.

## Carry-over from fdbmn-20261020

- M1 at 1 worker lost all write cases: set100 failed the pre-arm client CPU gate, and the preregistered stop-on-runner-failure policy left batchset50 and batchset50x1000 unreached. Re-measure them.
- Find and fix the cause of the pre-arm client CPU bursts (5 gate failures; a suspected cause is the controller's 10 s SSH status polls landing on unpinned CPUs) before sealing the next protocol.
- Preregister an outcome-blind re-attempt rule: a case that fails the pre-arm gate before any arm starts may be re-attempted once at the end of the schedule.
