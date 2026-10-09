# RFC-257: A Bazel cache shared by the CI fleet

Status: proposed (needs an owner `tofu apply`). Follow-up to RFC-108's "Cache
provenance", which requires an RFC before any non-local cache.

## Problem

Each CI runner has its own `--disk_cache`. A job is cache-warm only on the box
that ran its predecessor:

- Run 37876412173 changed only `ci.yml`. Its main job landed on
  `gh-runner-drain-0`, whose cache did not hold the previous run's results, so it
  re-ran 135 test targets (~5350 s of test time, 33 min).
- In the same run, the Java conformance job landed on `gh-runner-fdb` and hit the
  cache in 40 s.

Commit `69392239c` pins jobs to boxes (`fdb-ci-main`, `fdb-ci-aux`) as a
stopgap. It works, but it has costs:

- A pinned job waits while its box is down or busy with another PR's run.
- The two boxes never share work. For example, both compile the same packages.
- Nightly jobs still land anywhere.

## Proposal

1. **Network.** A Hetzner private network (`hcloud_network` with one subnet in
   `eu-central`, which spans nbg1 and fsn1). Both runners attach to it via
   `hcloud_server_network`, which does not replace the servers.
2. **Cache server.** One `bazel-remote` container (pinned by digest, the same
   supply-chain rule as RFC-108 §1) on `gh-runner-fdb`:
   - storage on its data volume: `--max_size` 40 GiB, LRU;
   - gRPC bound to the private IP only;
   - no public port; the Hetzner firewall drops 9092 on the public interface.
3. **Bazel config.** Each box's `/etc/bazel.bazelrc` (not the committed
   `.bazelrc`, so developers are unaffected) gets
   `build --remote_cache=grpc://<private-ip>:9092` and
   `build --remote_local_fallback`. The local `--disk_cache` stays as a first tier.
4. **Live boxes.** They have `ignore_changes[user_data]`, so a one-shot script
   (`infra/enable-shared-cache.sh`, run over SSH) writes the rc lines and starts
   the container. `cloud-init.yaml` carries the same for re-provisioned boxes.
5. **Unpin.** Once the shared cache is live, revert the pinning in `ci.yml` back
   to `hetzner-fdb-vm`.

## Trust boundary (RFC-108's question)

Who can write to the cache, and therefore poison it?

- **Writers today:** exactly the processes that can already write each box's local
  disk cache, which are the CI jobs of this repository on these two boxes. The
  repository runs no fork PRs on self-hosted runners.
- **What the shared cache adds:** a poisoned entry written on box A is read on
  box B. It adds no new writer.
- **Network reach:** the cache is reachable only from the private network, which
  has only the fleet on it.
- **Integrity:** Bazel content-addresses outputs (CAS digests are checked on read).
  Action-cache entries are trusted as they are today for the disk cache.

So the boundary is the fleet, the same as now. External adopters are unaffected:
the committed `.bazelrc` still configures no cache.

## Expected effect

- A job is cache-warm on whichever box runs it, so pinning goes away.
- A push that changes no test inputs re-runs zero tests on every job.
- Work done by one job serves the other: the race job and the main lane share
  every non-race action; Java conformance and the main lane share the whole
  build.
- Measure on the first five runs after enabling, against the pinned baseline.
  The measures are the cache hit ratio (BEP `ActionCacheStatistics`) and job wall
  time.

## Rollback

Delete the two rc lines on each box; the local disk cache remains.
