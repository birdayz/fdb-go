# PREPARED ONLY. Render locally after actual private-interface name is verified on all new nodes.
set -euo pipefail
umask 077
: "${CTL_CIDR:?Controller public /32}"
: "${PRIVATE_IFACE:?Verified campaign private interface name}"
python3 -B ${FDB_PERF_HOME}/campaigns/fdbmn-20261020/tooling/tools/benchcampaign/multinode/fdb_cluster.py render --vpc-cidr 10.89.20.0/24 --client-ip 10.89.20.10 --node-ip 10.89.20.11 --node-ip 10.89.20.12 --node-ip 10.89.20.13 --description fdbmn20261020 --cluster-id mn20261020NONCE --loggroup fdbmn-20261020-NONCE --engine ssd-2 --output-dir ${FDB_PERF_HOME}/campaigns/fdbmn-20261020/cluster-ssd --controller-cidr "$CTL_CIDR" --private-iface "$PRIVATE_IFACE"
