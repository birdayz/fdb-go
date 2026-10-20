# PREPARED ONLY. Local key generation/rendering; no hcloud commands. Not executed during preparation.
set -euo pipefail
umask 077
: "${SSH_PUBLIC_KEY:?Owner supplies public .pub path outside Git; never private key}"
python3 -B ${FDB_PERF_HOME}/campaigns/fdbmn-20261020/tooling/tools/benchcampaign/multinode/render_cloud_init.py keys --host-key-dir ${FDB_PERF_HOME}/campaigns/fdbmn-20261020/private/hostkeys --host fdbmn-20261020-NONCE-client --host fdbmn-20261020-NONCE-node1 --host fdbmn-20261020-NONCE-node2 --host fdbmn-20261020-NONCE-node3
python3 -B ${FDB_PERF_HOME}/campaigns/fdbmn-20261020/tooling/tools/benchcampaign/multinode/render_cloud_init.py render --host-key-dir ${FDB_PERF_HOME}/campaigns/fdbmn-20261020/private/hostkeys --host fdbmn-20261020-NONCE-client --host fdbmn-20261020-NONCE-node1 --host fdbmn-20261020-NONCE-node2 --host fdbmn-20261020-NONCE-node3 --output-dir ${FDB_PERF_HOME}/campaigns/fdbmn-20261020/private/cloud-init --ssh-public-key "$SSH_PUBLIC_KEY"
