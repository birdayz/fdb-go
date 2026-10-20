# fdb-go CI on Bazel remote execution: design recommendation (2026-10-11)

**Verified state.** Bazel 9.0.1, with 151 test targets: 66 depend on `//pkg/testcontainers/foundationdb`, 39 are tagged `test-full` and 7 `conformance_java`. CI has 7 jobs. The bazel-remote cache is set in each box's `/etc/bazel.bazelrc` (RFC-257), not in `.bazelrc`. The C++ oracle is three `genrule(local = True)` in `cmd/{fdb-schema-extract,fdb-diff-oracle}/BUILD.bazel`; there is no `fdb_cpp_build.bzl`. `local` turns off remote **and** disk caching [B], so the oracle only survives in a warm output_base. libfdbc is not in Bazel; it runs as a `go test -tags libfdbc` job. RFC-108 requires an RFC before any cache outside the fleet.

**The hard part.** fdbserver advertises `containerIP:4500`, and the fixture README says "bridge IP connectivity required". So the test must share a kernel and network with the Docker daemon. Two options fail:
- A remote `DOCKER_HOST` does not work without routing to the bridge.
- An FDB sidecar breaks `Pause/Unpause`, multi-process clusters, `fdbcli configure`, knobs and chaos, and it makes cached results depend on shared state.

Two options work: Docker inside the action's VM, or actions running bare on a Docker host. `no-remote-exec` also works, but it keeps most test time on the 4-vCPU client. It is a migration crutch only.

| Option | Cost/month (900–1800 runs) | Docker / testcontainers | C++ Docker genrule |
|---|---|---|---|
| **BuildBuddy Cloud** (Personal) | $0 up to 80 cores and 100 GB transfer; above that, Enterprise by quote [1] | Firecracker VM with `init-dockerd` + `recycle-runner` on shared executors [2]. Sibling Docker is Enterprise-only [1]. `dockerNetwork` only sets the action's own network [3] | Works: `-v` paths are inside the VM |
| BuildBuddy + own executors | $0 for BuildBuddy + €159 (AX102 [7]); executor↔cache traffic probably exceeds 100 GB | `workload-isolation-type=none` pool on host Docker, selected per target via `use-self-hosted-executors`/`Pool` [3]. Same as today | Bare pool |
| EngFlow | Free tier is self-hosted, 1 machine, ≤32 cores; managed by quote [4] | `dockerSiblingContainers` + `$HOST_NETWORK_NAME`, or Sysbox DinD on request [5]. Needs fixture changes (join that network, use direct IP) | Breaks: sibling `-v` sees host paths |
| NativeLink | OSS free, ~€195 hardware; managed by quote [6] | No per-action containers; a worker on a Docker host uses host Docker, with no isolation [6b] | Docker-host pool |
| Buildbarn on Hetzner | €0 software, ~€195 hardware, most ops work | `bb_runner` in our image with `docker.sock` + host network [8] | Same |

Hetzner net prices [7]: cpx32 €35.49 (today's 2 runners ≈ €71), AX42 (8c/64 GB) €79, AX102 (16c/128 GB) €159. Hetzner Cloud has no nested KVM [9], so self-hosted Firecracker needs bare metal. The ~€195 is one AX102 plus one cpx32 (scheduler, cache, client). For BuildBuddy we assume "transfer" counts client bytes. With `--remote_download_minimal` plus fetching only `test.xml`/`test.log` (which `cmd/test-report` and `cmd/test-budget` need), that is 15–70 MB per run, or 15–125 GB a month: borderline.

**Design.**
- **Lanes.** Untagged means fast and is the default. `slow` replaces `test-full`, `conformance_java` and `stress`; keep a descriptive second tag. Each PR runs `bazelisk test --config=ci //...` (both lanes); `just test` filters out `slow`.
- **Placement tags.** `docker` and `libfdbc` decide where a test runs, not which lane it is in. An `fdb_go_test` macro (gazelle `map_kind`) maps `docker` and `resources:*` to `test.workload-isolation-type=firecracker`, `init-dockerd`, `recycle-runner`, `runner-recycling-key=fdb-7.3.77` and `EstimatedCPU/EstimatedMemory` [3].
  - A query test asserts that every test depending on the fixture has the `docker` tag.
  - Under `--config=ci`, the fixture's "no Docker" skip becomes a failure. Otherwise a target that lands on an executor without Docker passes green.
- **Java conformance.** `remotejdk_21` is hermetic, so the server and FDB share one VM of about 4 GB.
- **libfdbc.** rules_go `go_test` has `gotags`/`race` attributes. Add `gotags=["libfdbc"]` targets, get `libfdb_c.x86_64.so` 7.3.77 via `http_file` with a sha256 (cloud-init already pins it), and register a hermetic CC toolchain. `race="on"` targets bring the race job into the same invocation.
- **C++ oracle.** Replace `local = True` with the `docker` properties, pin `foundationdb/build` by digest, and merge the two stages so only the binary enters the CAS. It is then built once per FDB version.
- **Cache.** BuildBuddy's CAS replaces bazel-remote; CI gets a read-write key and developers read-only. Fix `--test_env=HOME` to a constant (the client's HOME otherwise leaks into action keys). Mirror the FDB image to GHCR by digest, because of Docker Hub limits on shared IPs. Notices, codegen, vulnerability and go-install checks stay in one small job outside Bazel.

**Risks.**
- A free tier with no SLA would back a required check.
- FDB-in-microVM performance is unmeasured. On the plus side, a kernel per VM ends the `fs.aio-max-nr` and OOM interference between tests.
- The `test-budget` thresholds need re-baselining.

**Recommendation.** Use BuildBuddy Cloud Personal, with FDB tests in Firecracker+dockerd. This puts the complexity on their side at €0, and one client box suffices (saves €35/month). If the pilot breaches the limits or FDB is flaky, move only the `docker` pool to self-hosted BuildBuddy executors on an AX102; the tags stay the same. Choose Buildbarn or NativeLink only for zero vendor dependence. EngFlow does not fit.

**First 3 steps.**
1. Write an RFC on the cache trust boundary (RFC-108/257). Add an opt-in `build:rbe`: BuildBuddy executor and BES endpoints, minimal download plus a regex for test outputs, `--jobs=80`, and a fixed `HOME`.
2. Land the `fdb_go_test` macro, the `docker` tag and its query gate, and the fatal no-Docker check. Mirror and pin the FDB image, and convert the three genrules.
3. Run `bazelisk test --config=ci --config=rbe //...` as a non-required job on PRs for 2 weeks. Go if p50 wall time ≤15 min, Docker skips are zero, projected transfer is under 100 GB/month and flakes are no worse than today. Otherwise take the AX102 path.

**Sources.**
- [1] https://www.buildbuddy.io/pricing/
- [2] https://www.buildbuddy.io/docs/rbe-microvms/
- [3] https://www.buildbuddy.io/docs/rbe-platforms/
- [4] https://www.engflow.com/product/pricing
- [5] https://docs.engflow.com/re/client/platform-options-reference.html and https://docs.engflow.com/re/client/sysbox.html
- [6] https://nativelink.com/pricing
- [6b] https://docs.nativelink.com/remote-execution/containers-and-images
- [7] Hetzner Cloud API (`hcloud server-type describe`) and https://www.hetzner.com/dedicated-rootserver/matrix-ax/
- [8] https://github.com/buildbarn/bb-remote-execution#readme
- [9] https://docs.hetzner.com/cloud/servers/faq/
- [B] https://bazel.build/reference/be/common-definitions#common.tags
