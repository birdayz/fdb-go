#!/bin/bash
# Three fdbservers under fdbmonitor, so a killed process restarts in place with
# its data. Only 4500 is a coordinator. The timeout reaps a leaked container.
set -euo pipefail
ip=$(hostname -i | awk '{print $1}')
echo "recovery:recovery@$ip:4500" > /var/fdb/fdb.cluster
cat > /var/fdb/fdbmonitor.conf <<EOF
[fdbmonitor]
[general]
restart-delay = 1
initial-restart-delay = 1
[fdbserver]
command = /usr/bin/fdbserver
cluster-file = /var/fdb/fdb.cluster
listen-address = 0.0.0.0:\$ID
public-address = $ip:\$ID
datadir = /var/fdb/data/\$ID
logdir = /var/fdb/logs
knob_disable_posix_kernel_aio = 1
EOF
for port in 4500 4501 4502; do
    mkdir -p "/var/fdb/data/$port"
    cat >> /var/fdb/fdbmonitor.conf <<EOF
[fdbserver.$port]
locality-machineid = machine-$port
locality-zoneid = zone-$port
EOF
done
exec timeout -s KILL 600 /usr/bin/fdbmonitor --conffile /var/fdb/fdbmonitor.conf --lockfile /var/fdb/fdbmonitor.pid
