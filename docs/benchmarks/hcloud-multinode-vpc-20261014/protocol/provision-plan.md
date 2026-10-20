# hcloud-multinode-vpc-20261014: provisioning plan and runbook

**Status: design, not executed.** No cloud resource exists for this campaign.
Every command below is parameterised and has been run only in dry-run mode
(`provision.py` without `--execute`). Protocol: `protocol.json` v2, design-ACKed by pr3-sol on repo 9f31262cd (no runtime/timing ACK); see `revision_history` for the post-ACK clarifications (v1 in `protocol-v1.json`; pr3-sol: ACK on direction, NAK on timing and on the unqualified placement proof. v2 addresses each NAK point; see its `revision_history`). Orders:
`orders.json`, sealed by `seal_multinode.py orders` and checked by
`seal_multinode.py check-orders`.

Tooling: `tools/benchcampaign/multinode/` in `${FDB_GO_WORKTREES}/fdb-go-bench267`
(uncommitted, branch `bench/rfc258-campaign`). Copy it into this campaign
directory (`tools/`) and record its SHA-256s before launch, so the scripts that
run are the scripts on record.

---

## 0. Decisions root must make before anything is created

| # | Decision | Recommendation | Deadline |
|---|---|---|---|
| D1 | **Binary reuse or rebuild** (see §4) | Archive the 14 binaries from VM <server-id> now. Reuse them for the reuse-only cells. Rebuild only for new-harness cells. | **Before 2026-10-15T00:19Z**: the single-node campaign's own collection cutoff. Hard delete is 00:29:01Z. |
| D2 | **Scope**: `reuse_only` or `full` (full needs a reviewed harness commit H1–H4, §7). The choice is a protocol amendment that fixes the phase caps from `budget.scope_variants`. | Run `full` only after H1–H4 pass review. `reuse_only` is valid and smaller. | **before any timing**; cannot change after the first timed run |
| D3 | **Lifetime cap**: 4 h or 6 h | 6 h. **Not everything is guaranteed to fit.** Worst case at caps: `full` 28,600 s, `reuse_only` 22,400 s, against 21,600 s for 6 h. The skip-and-continue lifetime gate (cap + 600 + 300 s) decides deterministically. Guaranteed at worst case within 6 h: `full` M2rl, M2skip, M2sql, M1w1; `reuse_only` M2rl, M2skip, M1w1, M1w64, M1w8. Within 4 h, only M2rl and M2skip in either scope. Everything else runs only if earlier phases finish under cap; expected totals are unmeasured guesses (about 19,000 s full, 15,100 s reuse). | at launch |
| D4 | **Server type** | 4 × ccx43. The 7-process node1 layout needs 7 sibling pairs plus a housekeeping pair, which ccx33 (4 cores) cannot provide. | at launch |
| D5 | Add before-Go as a third M1 arm | No by default. It costs about +50 % of M1 time. If added, M1 becomes a 3-arm, 6-block protocol amendment sealed before timing. | before sealing |
| D6 | Optional M2mem (memory engine anchor) | No by default. It needs a cluster recreate, a new placement gate and about 1.3 h. | before sealing |
| D7 | Phase order | M2 first, then M1w1, M1w64, M1w8, M1w32 (protocol `phase_order`). | before sealing |
| D8 | Controller SSH: source CIDR and which public key; an existing hcloud SSH key or a new campaign-labelled one | A new campaign-labelled key from the controller `.pub`, unless that fingerprint already exists in the project (hcloud rejects duplicates). In that case pass `--ssh-key-name`. Always pass one: without an SSH key Hetzner e-mails a root password. | at launch |
| D9 | Project quota for **+64 dedicated vCPUs** while <server-id> (16) and the <existing-ssh-key> fleet exist | Root checks the limit in the console (no read-only CLI exposes it). Or launch after <server-id> is deleted. | at launch |
| D10 | Placement mechanism: ballast-driven data distribution, then `datadistribution off` for M2 (protocol `placement`). Label rule: stop at the first passing attempt, else use attempt 3 (never the best-looking one). Labels are sealed before timing. | Approve, together with review of H5/H6 and their on-cluster original and mutation controls. | before P0 |
| D11 | Reviewers for H1–H4 (harness), H5/H6/R1–R3 (campaign tools) | Same gate as d94b4c6: Java/Graefe + Linus ACK for the harness; methodology reviewer for the tools. | before sealing |

---

## 1. Pricing (read-only `hcloud server-type describe -o json`, 2026-10-14 ~20:25 UTC)

| Type | vCPU (dedicated) | RAM | Disk | fsn1 net €/h | fsn1 gross €/h | gross €/month cap |
|---|---|---|---|---|---|---|
| ccx33 | 8 | 32 GB | 240 GB | 0.2219 | 0.264061 | 164.8031 |
| ccx43 | 16 | 64 GB | 360 GB | 0.4423 | 0.526337 | 328.4281 |

Prices are the same in nbg1 and hel1.

| Fleet | gross €/h | net €/h | 4 h gross | 6 h gross | 4 h net | 6 h net |
|---|---|---|---|---|---|---|
| **4 × ccx43 (recommended)** | **2.105348** | 1.769200 | **8.421392** | **12.632088** | 7.076800 | 10.615200 |
| 3 × ccx33 + 1 × ccx43 (not viable for this layout) | 1.318520 | 1.108000 | 5.274080 | 7.911120 | 4.432000 | 6.648000 |
| + optional separate build VM ccx43, 2 started hours | 0.526337 | 0.4423 | +1.052674 | | | |

Billing notes, not verified from the CLI:

- Hetzner bills per started hour, up to the monthly cap.
- Primary IPv4 addresses are billed separately (the price is not in `server-type describe`).
- Private networks and firewalls carry no charge.
- Private traffic does not count against the included traffic (ccx43: 40 TiB/month).

A staged launch saves node-hours: provision the client first (`--role client`), build or verify binaries on it, then add the nodes (`--role node1 --role node2 --role node3 --skip-shared`). A 1.5 h client-only build window saves about 3 × 0.526337 × 2 = 3.16 € gross compared with all four running.

---

## 2. Topology and security

- Network `fdb-bench-vpc` 10.88.0.0/24 with one cloud subnet 10.88.0.0/24 in `eu-central` (fsn1). It does not overlap <existing-network> (<existing-cidr>); `provision.py --protected-cidr <existing-cidr>` enforces this and preflight refuses any existing overlapping network.
- Hosts: `fdb-bench-vpc-client` 10.88.0.10 (no fdbserver process), `fdb-bench-vpc-node1..3` 10.88.0.11–13. All are ccx43, ubuntu-24.04, `--without-ipv6`, and labelled `campaign=hcloud-multinode-vpc-20261014`, `deadline=<epoch>`, `role`, `node`.
- Firewall `fdb-bench-vpc-fw` is attached at server creation, so a server is never unprotected. Its only inbound rules are TCP 22 and ICMP echo from the controller CIDR. **Hetzner Cloud Firewalls do not filter private networks.** Each host therefore loads `nftables.conf` from `fdb_cluster.py`: input policy drop, then accept loopback, established, source 10.88.0.0/24 (optionally restricted to the private interface), controller SSH/ICMP, and DHCP replies.
- FDB binds and advertises only 10.88.0.x addresses.
- SSH host keys are pinned. Host keys are generated on the controller before boot (`render_cloud_init.py keys`) and injected by cloud-init as each machine's only host key; other key types are not generated. The controller connects only with `StrictHostKeyChecking=yes` and the campaign `known_hosts`, keyed by `HostKeyAlias`. Password authentication is off. There is no SSH between VMs.
- **Zero secrets on VMs.** A VM receives:
  - its own host key pair;
  - the controller's public key;
  - pinned public `.deb` files, binaries and source archives;
  - the cluster file, generated configs and scripts.

  A VM never receives the hcloud token (the CLI context stays on the controller and the scripts never read or pass it), the controller's private key, Git/GitHub credentials or deploy keys. A rebuild on the client needs only public downloads (Bazel dependencies) and `git archive` tarballs copied from the controller.
- The cloud-init user-data (that host's own private host key) is readable from the metadata service on that VM. This is acceptable: it is the host's own key, and the VM is deleted at teardown.
- The FDB cluster file is not an authentication secret. FDB without TLS relies on network isolation, which the firewall and nftables provide.
- Private campaign state lives under `private/` in this directory (mode 0700): host keys, cloud-init and `known_hosts`. This directory is not in any Git repository, and `.gitignore` here lists `private/` and `work/` as a guard. Never include either in a published bundle. **No real IP of any existing host appears in this campaign's files or in the repository tooling.**

---

## 3. Hard teardown guard

`teardown_guard.py` deletes a resource only if all of these hold:

- the server-side selector `campaign=<label>` returns it;
- its labels contain that value (re-checked on the client side);
- its name is `fdb-bench-vpc` or `fdb-bench-vpc-*`;
- its name matches no `--protect-name` glob and its ID is no `--protect-id`.

It deletes in this order: servers, firewalls, SSH keys, networks. Deletes are by numeric ID, and firewall/network deletes are retried. It then lists again, and any surviving owned resource fails the run. A labelled resource outside the prefix is reported as REFUSED, never deleted, and makes the exit status nonzero.

The guard is armed **before** `provision.py --execute`, with the same deadline that `provision.py` writes into each resource's `deadline` label:

```sh
python3 -B "$T/teardown_guard.py" --label "$LABEL" --prefix fdb-bench-vpc \
  --protect-name '<existing-ssh-key>-*' --protect-name <existing-server> --protect-name <existing-network> \
  --protect-id <server-id> \
  --deadline "$DEADLINE" --execute --receipt-dir "$C/work/teardown"
```

Run it as a supervised foreground background-job (`bash` `background: true`, `timeout_ms` = lifetime + 15 min) and record its future id in `state.json`. A dry run (no `--execute`, no `--deadline`) beforehand must list nothing. Re-run that dry run after provisioning: it must list exactly 4 servers, 1 firewall, 1 network and the optional SSH key.

---

## 4. Binary reuse decision (D1), stated plainly

The compiled binaries this campaign could reuse exist **only on VM <server-id>**, under `/srv/fdb-bench/bin/`. Their hashes are in `../hcloud-rebench-20261014/provenance/all-binaries.sha256` (sha256 of that file: `8dbd9b7020b1a0e62d19042745281bf13e5160625f5e629ea6007c26f7c49630`) and `client-binaries.sha256`. That VM is **hard-deleted at its lifetime deadline, 2026-10-15T00:29:01Z**, and its campaign plans final collection before 00:19Z. Nothing is on the controller.

- **Option A, archive (recommended, cheap):** before deletion, root, or the agent that holds that VM's SSH (its `state.json` `ssh` entry), copies `/srv/fdb-bench/bin/` to the controller. No new credential goes anywhere: the controller pulls over the existing pinned SSH.

  ```sh
  # on the controller; $SSH is the exact pinned ssh command from ../hcloud-rebench-20261014/state.json
  mkdir -p "$C/binaries/rebench-20261014"
  $SSH 'cd /srv/fdb-bench && sha256sum -c provenance/all-binaries.sha256 >&2 && tar -cf - bin' \
    > "$C/binaries/rebench-20261014/bin.tar"
  tar -xf "$C/binaries/rebench-20261014/bin.tar" -C "$C/binaries/rebench-20261014"
  sed "s#/srv/fdb-bench/bin/#$C/binaries/rebench-20261014/bin/#" \
    ../hcloud-rebench-20261014/provenance/all-binaries.sha256 | sha256sum -c -   # must print 14 x OK
  ```

  Also keep `provenance/runtime.sha256` (libfdb_c/fdbserver/fdbcli identities). Those come from the public 7.3.77 `.deb` files with pinned hashes, so they are reproducible. The reused binaries cover only the reuse cells (protocol `harness_requirements.reuse_frozen_binaries`). All arms of one campaign must come from this one archive.
- **Option B, rebuild:** rebuild from the frozen source archives (`../hcloud-rebench-20261014/sources/*.tar.gz` and `overlays/`, hashes in `provenance/source-receipt.json`) on the client VM before the nodes exist, following `build-client-vm.sh`/`build-harness-vm.sh`. Go builds are not guaranteed byte-identical, so rebuilt binaries get new recorded identities and are a new build, not the 2026-10-14 binaries. Budget about 1–1.5 h client-only.
- **Either way**, the new-harness cells (range1000, mixed80_20, rows100, all SQL) need a rebuild from a reviewed harness commit (§7). Every arm of an affected campaign is rebuilt from it.

If nobody archives by 00:19Z, Option B is the only path.

---

## 5. Launch runbook (parameterised; nothing here has run)

Variables (values come from root; none are committed anywhere):

```sh
C=${FDB_PERF_HOME}/hcloud-multinode-vpc-20261014
T=$C/tools                     # frozen copy of tools/benchcampaign/multinode (sha256 recorded)
LABEL=campaign=hcloud-multinode-vpc-20261014
CTL_CIDR=<controller public IPv4>/32
PUBKEY=<controller public key .pub>; PRIVKEY=<matching private key path, never copied>
HOSTS=(fdb-bench-vpc-client fdb-bench-vpc-node1 fdb-bench-vpc-node2 fdb-bench-vpc-node3)
DEADLINE=$(( $(date -u +%s) + 6*3600 ))     # D3; recorded in state.json
SSHX() { ssh -i "$PRIVKEY" -o IdentitiesOnly=yes -o BatchMode=yes -o StrictHostKeyChecking=yes \
             -o UserKnownHostsFile="$C/private/cloud-init/known_hosts" -o HostKeyAlias="$1" "root@$2" "${@:3}"; }
```

1. **Approvals:** record D1–D11 in `provenance/` and verify the quota (D9). Copy the tooling to `$T` and run `sha256sum "$T"/* > provenance/tools.sha256`.
2. **Host keys and cloud-init:**
   ```sh
   python3 -B "$T/render_cloud_init.py" keys --host-key-dir "$C/private/hostkeys" $(printf -- '--host %s ' "${HOSTS[@]}")
   python3 -B "$T/render_cloud_init.py" render --host-key-dir "$C/private/hostkeys" --ssh-public-key "$PUBKEY" \
     $(printf -- '--host %s ' "${HOSTS[@]}") --output-dir "$C/private/cloud-init"
   ```
3. **Arm the teardown guard** (§3), then dry-run it.
4. **Provision: dry run, review, then execute** (staged; drop `--role` for all at once):
   ```sh
   P=(--label "$LABEL" --controller-cidr "$CTL_CIDR" --ssh-public-key "$PUBKEY" \
      --protected-cidr <existing-cidr> --protected-name <existing-network> --protected-name <existing-server> \
      --cloud-init-dir "$C/private/cloud-init" --work-dir "$C/work/provision" --teardown-deadline "$DEADLINE")
   python3 -B "$T/provision.py" "${P[@]}" --role client                      # prints only
   python3 -B "$T/provision.py" "${P[@]}" --role client --execute
   # ... client-only build/verify window (§4) ...
   python3 -B "$T/provision.py" "${P[@]}" --role node1 --role node2 --role node3 --skip-shared --execute
   ```
   `work/provision/hosts-*.json` holds the public IPs, which stay out of the repository. If `provision.py` fails at any step, run the guard with `--execute` and no `--deadline` immediately.
5. **First contact** on each host: `SSHX <alias> <ip> 'test -f /srv/fdb-bench/cloud-init-done && ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub'`. It must match `private/hostkeys/<host>/…pub`. A host-key mismatch aborts the campaign: tear down, no override.
6. **Packages** (each host): use the `setup-vm.sh` `fetch()` pattern (curl, then `sha256sum -c`) with the pinned FDB 7.3.77 clients `b6c263723009bddbc30b6ae8cf9854340bd35cef14152abb0da8fb5af2b91254` and server `789f1ed4cbacc0fd7043da2217c9e57516e4ec94fa7140853ec22a7e715785cc` `.deb` files (and bazelisk `5a408715…` on the client only if building). Then `systemctl unmask foundationdb; dpkg -i …; systemctl disable --now foundationdb; systemctl mask foundationdb`. Record `runtime.sha256`, `lscpu -J`, the CPU sibling map (`/sys/devices/system/cpu/cpu*/topology/thread_siblings_list`), `uname`, `ip -j addr` and `chronyc tracking`.
7. **Render the cluster** (on the controller), passing the private interface name observed in step 6:
   ```sh
   python3 -B "$T/fdb_cluster.py" render --node-ip 10.88.0.11 --node-ip 10.88.0.12 --node-ip 10.88.0.13 \
     --client-ip 10.88.0.10 --controller-cidr "$CTL_CIDR" --private-iface <iface> \
     --description fdbbenchvpc --cluster-id mn20261014 --engine ssd-2 --output-dir "$C/cluster-ssd"
   ```
   If the sibling map differs from (2k,2k+1), stop: regenerate the layout through a reviewed change. Never hand-edit the pinning.
8. **Install:** scp `cluster-ssd/nodeN/` and `cluster-ssd/fdb.cluster` to each node and run `install-units.sh` (it refuses if any fdbserver runs). On the client, load `client/nftables.conf` and install `client/fdb.cluster`. Record `taskset -pc <MainPID>` for every unit.
9. **Configure** from the client: `fdbcli -C <cluster file> --exec "$(cat configure.fdbcli)"`. Wait until available, save `status json`, then run `fdb_cluster.py check-roles --layout cluster-ssd/layout.json --status … --output provenance/roles-configure.json` (must pass).
10. **Binaries:** copy them to the client and run `sha256sum -c` against the archive (Option A) or the build receipts (Option B). Verify CGo linkage to the pinned `libfdb_c`.
11. **Gate F0:** protocol `gates.F0_cluster` receipts, including 100-packet ping client→each node (p50/p99, 0 % loss) and MTU.
11b. **F0 status-json paths (R1/R2 `cluster_gate.py`).** Save one real 7.3.77 `status json` as `provenance/status-f0.json`, and confirm every path below on it before sealing. Paths and semantics were checked against the 7.3.77 source (`fdbclient/Schemas.cpp` status schema; `fdbserver/Status.actor.cpp`):
    - `cluster.recovery_state.name`: an enum that includes `fully_recovered` (`statusObj["recovery_state"]`).
    - `cluster.generation`: an int, `db->get().recoveryCount`.
    - `cluster.qos.worst_durability_lag_storage_server.seconds`: from `getLagObject`, versions divided by `VERSIONS_PER_SECOND`. The `qos` object exists only when the ratekeeper answered.
    - `cluster.qos.worst_queue_bytes_log_server`: a raw number, present only when tlogCount > 0.
    - `cluster.data.moving_data.in_flight_bytes` / `in_queue_bytes`: **present only when the data distributor's `MovingData` event has `AverageShardSize >= 0`.**
      - With DD off (`DataDistribution.actor.cpp` disabled branch, `AverageShardSize = -1`) `moving_data` is **absent**. M2's gate must accept its absence when `data_distribution_disabled` is true.
      - As written, `cluster_gate.server_findings` reports "missing" and would abort every M2 case. sn-port's fix is pending.
      - While the distributor is (re)initialising, `data.state.name` is `initializing` and `moving_data` is absent.
    - `cluster.data_distribution_disabled`: present (`true`) only when `\xff/dataDistributionMode` is present and 0.
    - `cluster.database_available`, `client.database_status.available`, `client.coordinators.coordinators[].address`, and `cluster.configuration.{redundancy_mode,storage_engine,commit_proxies,grv_proxies,resolvers,logs}`: present in the schema.
    - `cluster.processes.<id>.{address,class_type,locality.machineid,roles[].role,roles[].id}`: `class_type` is `processClass.toString()`. That emits `coordinator` for the `coordinator` class even though the schema's documentation enum omits it.
    - The role names used by `check-roles` are all in the schema's role enum.

11c. **placementprobe_cgo build gate (blocks P0).** `just test` never compiles the CGo probe, so build and smoke it on the client VM before any probe. Use the tree with the reviewed placementprobe commit, and the same bazelisk/cache flags as `build-client-vm.sh` uses for `clientbench_cgo`:
    ```sh
    T=//tools/benchcampaign/multinode/placementprobe/cmd/placementprobe:placementprobe_cgo
    bazelisk --output_user_root="$root/cache/bazel" build --disk_cache="$root/cache/disk" \
      --repository_cache="$root/cache/repository" "$T" > "$root/results/build/placementprobe-cgo.log" 2>&1
    install -m755 "$(bazelisk --output_user_root="$root/cache/bazel" cquery --output=files "$T")" "$root/bin/placementprobe_cgo"
    bazelisk --output_user_root="$root/cache/bazel" shutdown
    sha256sum "$root/bin/placementprobe_cgo" >> "$root/provenance/client-binaries.sha256"
    ldd "$root/bin/placementprobe_cgo" > "$root/provenance/placementprobe-cgo-linkage.txt"   # must resolve libfdb_c.so to the pinned 7.3.77 library
    # refusal smoke: both must exit 2 and write nothing
    rc=0; "$root/bin/placementprobe_cgo" || rc=$?; test "$rc" -eq 2
    rc=0; "$root/bin/placementprobe_cgo" probe --cluster-file "$CF" --store benchfetch/nope \
      --fixture-receipt /dev/null --output "$root/results/smoke/pp-refusal.json" || rc=$?; test "$rc" -eq 2
    test ! -e "$root/results/smoke/pp-refusal.json"
    ```
    - Pin the binary's SHA-256 in every P0 command record and in R2's placement-command template.
    - Any failure (build, linkage, or a refusal not exiting 2) blocks P0. `placementprobe_go` never substitutes for it.

12. **Gate P0 (M2 only; needs reviewed H5/H6 = `placementprobe`; protocol `H5_H6_implementation`).** `PP` is the placementprobe binary on the client and `CF` the client cluster file.
    - **Qualification (once).** Run `fdbcli --exec 'datadistribution off'` and keep the receipt. Then, for `S` in `benchfetch/control-small` and `benchfetch/control-split`:
      ```sh
      $PP control-write --cluster-file $CF --store S --rows 1000 --fixture-receipt-out fx-S.json --output cw-S.json
      $PP probe --cluster-file $CF --store S --fixture-receipt fx-S.json --output probe-S.json
      placement_receipt.py receipt ... --fixture-receipt fx-S.json --cluster-file cluster-ssd/fdb.cluster --layout recordlayer --record-format unsplit|split \
        --expect-prefix-tuple benchfetch,control,small|split          # must be integrity_ok (exit 0 or 3)
      ```
      For `D` in `delete-chunk`, `stray-suffix` and `version-key`, on the split control store only:
      ```sh
      $PP control-mutate --cluster-file $CF --store benchfetch/control-split --defect D --output m-D.json
      $PP probe ...    # then the receipt must have integrity_ok false, naming D
      ```
      Clear before the next defect: `control-clear` both control stores, then `control-write` and re-probe the split store. Finish with `control-clear` on both; its receipt proves each store empty. Then `fdbcli --exec 'datadistribution on'`.
    - **Per attempt k, per store**, strictly in this order (protocol `placement.mechanism` (a)–(g)):
      1. DD on; run the benchmark's own untimed fixture step (it clears the store) and keep its log as `fixture-<store>.log`. Extract its single `FDB_BENCH_AUDIT` fixture object into `fixture-<store>.json`, and use that one file for every later `--fixture-receipt`.
      2. `$PP ballast-write --cluster-file $CF --store <store> --fixture-receipt fixture-<store>.json --ballast-mib $((256 * 2**(k-1))) --output bw.json`. This refuses unless DD is on and the ballast range is empty.
      3. Wait for `moving_data` in-flight and in-queue 0 for 120 s.
      4. **Freeze:** `fdbcli --exec 'datadistribution off'`; keep the receipt and a status json.
      5. `$PP ballast-clear ... --output bc.json`. This clears only the ballast range, and refuses unless DD is off (checked inside the clearing transaction).
      6. `$PP ballast-verify-empty ... --output bv.json` must show 0 keys at a recorded read version.
      7. `$PP probe --cluster-file $CF --store <store> --fixture-receipt fixture-<store>.json --output probe-<store>-attempt<k>.json` (`$PP` = `placementprobe_cgo`). It refuses unless DD mode is 0 (key present) at both V1 and V2, and the moveKeysLock owner, write token, keyServers bytes and boundaries are identical at both. The owner may be any value: a DD restart under mode 0 leaves a random owner. Then:
         ```sh
         python3 -B "$T/placement_receipt.py" receipt --status status.json --probe probe-<store>-attempt<k>.json \
           --fixture-receipt fixture-<store>.json --cluster-file cluster-ssd/fdb.cluster --preset benchfetch|planskip|sql \
           --layout recordlayer --record-format unsplit|split --expect-prefix-tuple benchfetch,<payload> \
           --output receipts/placement-<store>-attempt<k>.json      # SQL: --layout opaque --records-per-id <n>
         # exit 0 = integral and fully disjoint; 3 = integral, partially colocated; 1 = integrity failed
         python3 -B "$T/placement_receipt.py" controls <same arguments> --output receipts/controls-<store>-attempt<k>.json
         ```
         `fixture-<store>.json` is the single FDB_BENCH_AUDIT fixture object extracted from the log; `placement_receipt.py` reads JSON only.
    - **P0 commands are sealed with the cluster identity.**
      - Before P0, record the SHA-256 of the generated `cluster-ssd/fdb.cluster` (pinned in the manifests) in `provenance/p0-commands.json`, together with the exact P0 argv lines.
      - Every probe and H6 receipt records the cluster file SHA-256 and description:id it ran against. `placement_receipt.py receipt --cluster-file <pinned file>` fails integrity on a mismatch, and `seal_multinode.py testingb-plans --cluster-file` refuses one.
      - R2's placement command carries `--expect-cluster-sha256 <that hash>` as a sealed literal.
      - Use `placementprobe_cgo` for every probe. Pass the same extracted `fixture-<store>.json` to the probe and to the receipt, so the recorded fixture hash matches.
    - **Integrity failure** of a receipt or of its controls stops P0 for investigation. It is not a placement outcome and is not retried as an attempt.
    - Otherwise, stop at the first attempt that passes for all cells of all stores; if none does, use attempt 3. A later attempt needs DD back on and restarts at step 1.
    - Labels come from the used attempt's receipts at seal time (step 13).
13. **Capacity smokes**, then a protocol amendment if anything is unexecutable for budget, then **seal**:
    ```sh
    python3 -B "$T/seal_multinode.py" check-orders --protocol protocol.json --orders orders.json
    python3 -B "$T/seal_multinode.py" testingb-plans --protocol protocol.json --orders orders.json \
      --cluster-file <client cluster file> --binary fetch=<…/fetch-final> --binary planskip=<…/planskip-final> \
      --placement-receipt benchfetch/small=<receipt> --placement-receipt benchfetch/split=<receipt> \
      --placement-receipt benchfetch/planskip=<receipt> \
      --output-dir plans/testingb [--allow-new-harness rows100 --binary sqlfetch=<…> --allow-new-harness M2sql \
      --placement-receipt sql/IDX_A=<receipt>]
    # refuses receipts without integrity_ok, with another store/layout/record format/preset, or whose cell bounds are not
    # exactly the scanned preset bounds; accepts integral partial receipts and writes each case's fixed label
    # (cross-server | partially colocated (f = x.xxxx))
    python3 -B "$T/seal_multinode.py" client-plans --protocol protocol.json --orders orders.json \
      --scripts-dir <frozen tools/clientbench/scripts on the client> --cluster-file <…> --native-library /usr/lib/libfdb_c.so \
      --server-binary /usr/sbin/fdbserver --harness-archive <client-harness.tar.gz> \
      --binary final=<…/client-final-go> --binary cgo=<…/client-final-cgo> \
      --results-dir results/client --output-dir plans/client [--allow-new-harness range1000 --allow-new-harness mixed80_20]
    ```
    Plans are sealed on the client VM, where the pinned paths exist (as in the single-node campaign). Get the methodology-review ACK on the exact manifests and runtime before timing.
14. **Timing**, phase by phase in `phase_order`, each behind the lifetime gate (cap + 600 + 300 s; skip-and-continue).
    - Server samplers (R3) run during each phase, with unit, chrony and status receipts before and after.
    - Placement receipt and `compare` before the first and after the last process of every M2 case.
    - After M2: `datadistribution on`, wait for `moving_data` 0, run check-roles, then M1.
    - **No ratio or summary for a phase before its R4 reconciliation audit is complete** (protocol `environment_policy.server_telemetry_reconciliation`). A contaminated or unverifiable case is omitted entirely.
14b. **R4 reconciliation, per phase, before any ratio or summary.** `$T` is the frozen tools copy. It must include `reconcile_telemetry.py`, `build_reconcile_input.py`, `cluster_gate.py`, `fdb_cluster.py`, `mn_common.py` and `../singlenode/{bench_runner,client_campaign}.py`, all recorded in `provenance/tools.sha256`.
    ```sh
    python3 -B "$T/build_reconcile_input.py" --manifest plans/<testingb|client>/manifest.json \
      --manifest-sha256 <acknowledged manifest SHA-256> --phase <PHASE> --protocol protocol.json \
      --results results/<PHASE> --layout cluster-ssd/layout.json \
      --client-receipt receipts/<PHASE>/client-before.json --client-receipt receipts/<PHASE>/client-after.json \
      --node node1=telemetry/<PHASE>/node1 --node node2=telemetry/<PHASE>/node2 --node node3=telemetry/<PHASE>/node3 \
      --output reconcile/<PHASE>/input.json
    python3 -B "$T/reconcile_telemetry.py" --input reconcile/<PHASE>/input.json --output reconcile/<PHASE>/report.json
    ```
    - `--results` is `bench_runner_mn.py`'s `--output` for testing.B, or the phase directory of `client_campaign_mn.py` outputs for client phases.
    - R4 re-judges each case's status with `cluster_gate.status_verdict` under the phase's `expected_dd_state`, and requires the runner's verdict.
    - Sample errors follow protocol `sample_error_scope`. Any malformed sample, or a missing or unsealed `samples.jsonl` or receipt, makes **every** case of the phase unverifiable. Gaps over 2 s, clock steps, invalid deltas, steal, OOM and a vanished MainPID affect only the run windows they touch.
    - Only cases R4 reports clean enter the analysis. For M2sql, rows are paired with their own measured invocation's audit, and differing prefixes are disclosed as `physical_prefix_confound`.
15. **Collect:** results, provenance and FDB trace logs from all four hosts go to the controller with sha256 manifests. Verify, then tear down now (§6), and only after confirmed deletion cancel the deadline guard job.

---

## 6. Abort and teardown

- **Abort at any time:** stop the running phase's job; it retains partial artifacts by design. If time allows (≤ 10 min), pull artifacts. Then run:
  ```sh
  python3 -B "$T/teardown_guard.py" --label "$LABEL" --prefix fdb-bench-vpc \
    --protect-name '<existing-ssh-key>-*' --protect-name <existing-server> --protect-name <existing-network> --protect-id <server-id> \
    --execute --receipt-dir "$C/work/teardown"
  ```
- **Verify:** `hcloud server list -o json --selector campaign=hcloud-multinode-vpc-20261014`, and the same for `network`, `firewall` and `ssh-key`, must all be empty. Compare `hcloud server list` and `hcloud network list` before and after: <existing-network>, <existing-ssh-key>-* and <existing-server> are unchanged.
- **If the guard job dies,** re-arm it immediately with the same `$DEADLINE`. Each resource's `deadline` label preserves it.
- **If a REFUSED item appears** (a labelled resource outside the prefix), do not touch it. Report it to root.
- **Never** delete by name pattern alone, by `hcloud ... delete --all`, or anything not returned by the guard.

---

## 7. Proposed harness changes (not made; frozen harness untouched)

These belong in one separately reviewed commit on top of `d94b4c6`, overlaid byte-identically into every compared tree. Affected arms are rebuilt.

- **H1 `range1000`** (clientbench): `Dataset` gains a 1000-row range (8-byte values) next to the 100-row one. `Operation` reads `tx.Range(begin, end, 1001)` and checks exactly 1000 rows with `CheckRange`. Same API and streaming mode as range100. Add it to the smoke test and the workload list in the README/`run.py` validation.
- **H2 `mixed80_20`** (clientbench): worker *w*'s *i*-th transaction is `set100` (overwrite of its prepared key) iff `(i + w) % 5 == 4`, otherwise a `get100` read-only transaction. Raw rows gain `class=read|write`. Persisted-value validation covers the write class. `analyze.py` reports per-class quantiles and combined throughput. Warmup and measurement use the same rule. The name fits matrix_plan's 16-character padding.
- **H3 `rows100` cells:** benchfetch `cell=rows100` = `TupleRangeBetween((100),(200))`, IDs 100..199 forward and 199..100 reverse, `SourceExhausted`, same audits/receipts. planskip `cell=rows100` = `[100,203)` with Skip 3, returning 100. fetchmethod `query=rows100` = `a >= 100 AND a < 200`.
- **H4 persistent SQL fixture** (fetchmethod): a fixed per-campaign database path, a seeding step (`-test.run=TestSeedFixture`), and `FDB_BENCH_REUSE_FIXTURE=1`, which audits row count, checksum and EXPLAIN instead of dropping and recreating. Log a `store_prefix_hex` receipt. Without H4 the SQL store moves on every process and cannot be placed.

Campaign tools (they do not change timed binaries, but need review):

- **H5** `placementprobe_cgo` (Go + libfdb_c, build tag `libfdbc`, built on the client VM like the CGo arm), emitting `fdb-bench-placement-probe/v3`.
  - One transaction at V1 reads the DD mode, the moveKeysLock owner and write token, the raw `\xff/keyServers` entries of the store range, every key of both data subspaces, and every key's addresses. libfdb_c `getAddressesForKey` is transactional (NativeAPI `getAddressesForKeyActor`).
  - A V2 ≥ V1 recheck must find DD mode 0 and the same owner (any value), write token and keyServers bytes. A toggle writes a new token, a DD restart a new owner, and any shard-map commit a new token with new keyServers bytes (7.3.77 `setDDMode`/`DataDistributionImpl`, `takeMoveKeysLock`, `checkMoveKeysLock`).
  - `placementprobe_go` (pure-Go location cache) is diagnostic only.
- **H6** a ballast writer/clearer restricted to the owned store prefixes.
- **R1** the multi-node client adapter derived from `client_campaign.py`. It replaces the 172.31.99.1 single-coordinator cluster guard with the generated 3-coordinator file in 10.88.0.0/24, takes the engine guard from status json, drops the netns check, adds the DD in-flight pre-arm gate and runs check-roles before and after.
- **R2** the testing.B runner derived from `bench_runner.py`, executing `seal_multinode` testing.B plans with per-case placement receipt and compare.
- **R3** a per-node 1 Hz `/proc/stat` + `/proc/vmstat` sampler on CPUs 0-1 with realtime and monotonic stamps.
- **R4** a server-telemetry reconciliation tool. It merges all hosts' samples, unit receipts (MainPID, NRestarts), chrony receipts and status/role receipts per case, using conservatively widened windows (widened by the sealed bound `u = max_phase(|off_c| + disp_c + 0.5*delay_c) + max_phase(|off_n| + disp_n + 0.5*delay_n) + 1 s + 0.001 s`. Each host's term is chrony's full root distance (`chronyc tracking` System time offset, Root dispersion, Root delay), maximised over that host's receipts for the phase. The two hosts' bounds add because their offsets can have opposite signs, and 0.001 s is the timestamp precision. Every intersecting interval is counted; a missing chrony field makes the phase unverifiable). It emits clean, contaminated or unverifiable per case. Missing data means unverifiable, which omits the case.
- **H5/H6 qualification:** the original and mutation controls of protocol `placement.controls`. H5 must emit probe format v2: one read version for boundaries and addresses, complete boundaries, every physical key of the record and index data subspaces (`keys_complete`, `scanned_subspaces_hex`; metadata out of scope), and the prefix from the owned fixture receipt. H6 clears only the ballast range, refuses to clear while DD is on, and writes a verify-empty receipt.

### Claim scope (protocol v2)

M1 worker levels and all phases run in a fixed order that is not counterbalanced, and per-level iteration/warmup totals give finite closed-loop runs different measurement windows. Results are therefore arm contrasts within each worker level, and within each M2 cell under its sealed placement label. No scaling curve, saturation, cross-level, cross-phase or single-node-versus-multi-node effect is claimed.

"Server-disjoint" means storage-process (address) disjoint. With double replication on three machines any two teams share a machine, so machine-level disjointness is structurally 0 and is only descriptive.

---

## 8. What was verified and what was not

Verified:

- hcloud prices, types and locations via read-only `describe`/`list`.
- The `hcloud` 1.70.1 flags used by the scripts, via `--help`.
- Orders are reproducible from the seeds and balanced, and use the same derivation as `run.py`.
- The real frozen `matrix_plan.make_plan` + `run.make_schedule` accept the M1 plan shape (900 s cap, idle sentinel CPU group, worker override). This was a unit test with temporary files.
- Unit tests cover all script logic (48 tests). These include:
  - placement rejection of a duplicate key, an omitted second chunk, a stray suffix, a version key, suffix-less keys, foreign IDs, missing/zero/non-integer read versions and incomplete boundaries;
  - mutation controls on canonical probes;
  - seal refusal of unproven placement receipts and of same-named cells with non-preset bounds;
  - integral partial receipts passing compare, controls and seal with a 'partially colocated' label;
  - an integral shared-shard fixture;
  - probe-scope violations (metadata key, leftover ballast, partial key scan);
  - `.pub` symlink redirection in host-key generation.

Not verified (each needs a check on real infrastructure):

- `coordinator`, `commit_proxy`, `grv_proxy`, `resolution` and `transaction` class names and the role placement on 7.3.77. Startup errors and check-roles will show this.
- The exact status-json field paths on 7.3.77.
- The CCX SMT sibling map.
- Hetzner cloud-init handling of `ssh_keys` and root `ssh_authorized_keys` on ubuntu-24.04.
- Whether `datadistribution off` freezes split/merge as assumed. Before/after receipts detect a violation.
- How quickly DD splits the ballast, and whether 256 MiB suffices.
- Resolved from source: libfdb_c 7.3.77 `getAddressesForKey` is transactional at the read version; the pure-Go client's answers from its location cache (a divergence, reported separately). Production probes therefore use libfdb_c.
- Still unverified on a cluster: that `placementprobe_cgo` reads the whole split store (about 100 MB) plus all address lookups inside the ~5 s MVCC window. If it doesn't, the probe fails with `transaction_too_old`; rerun it.
- That `//tools/clientbench/scripts:*.py` can be used as data from another package under Bazel 9.0.1. The BUILD file of `tools/clientbench/scripts` has no `exports_files`, so this depends on implicit source-file export. It was not run under Bazel.
- Server knob names (none are used by default).
- The primary-IPv4 price and the dedicated-vCPU quota.
- The size of the binary archive.
- All throughput and latency estimates in `budget`, which only the capacity smokes can replace.

## Reporting

Owner instruction: all results of this campaign are PR'd into
`docs/benchmarks/hcloud-multinode-vpc-20261014/` (README.md as the index, one
report per phase), following `docs/benchmarks/hcloud-rebench-20261014/`. Raw
data, receipts and the analysis provenance stay here, outside the repository;
the docs carry no credentials, keys, IP addresses or local paths.
