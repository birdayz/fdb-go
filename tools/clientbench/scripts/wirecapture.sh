#!/usr/bin/env bash
# Capture one clientbench run's loopback traffic to the fdbserver ports and
# summarize its GRV and commit batching with wiretrace.
#
# Usage: wirecapture.sh OUTDIR PORTS -- CLIENTBENCH [ARGS...]
#   PORTS is comma-separated, e.g. 4721,4722,4723. Fails if tcpdump does not
#   start, drops packets, or the capture is incomplete. tcpdump runs in a container
#   (tools/clientbench/wiretrace-image) with host networking on CPUs 12-15;
#   set WIRECAPTURE_CPUS to change them. Requires docker and a built
#   //tools/clientbench/cmd/wiretrace (WIRETRACE=path). WIRETRACE_ROUND=N reports
#   whole and split commit batches of a closed loop with N workers.
set -euo pipefail
out=$1 ports=$2
shift 2
[[ ${1:-} == -- ]] && shift
here=$(cd "$(dirname "$0")" && pwd)
image=fdbperf-wiretrace:local
cpus=${WIRECAPTURE_CPUS:-12-15}
wiretrace=${WIRETRACE:-wiretrace}
mkdir -p "$out"
out=$(cd "$out" && pwd)
docker build -q -t "$image" "$here/../wiretrace-image" >/dev/null
filter=$(echo "$ports" | sed 's/,/ or tcp port /g; s/^/tcp port /')
name=wirecapture-$$
rm -f "$out/capture.pcap"
# No --rm: tcpdump's packet statistics are read from the container log after it stops.
docker run -d --name "$name" --net=host --cap-add NET_RAW --cap-add NET_ADMIN \
  --cpuset-cpus "$cpus" -v "$out:/out" "$image" \
  -i lo -s 0 -B 262144 -U --time-stamp-precision=nano -w /out/capture.pcap "$filter" >/dev/null
trap 'docker rm -f "$name" >/dev/null 2>&1 || true' EXIT
# Wait until tcpdump has written the capture file header.
for _ in $(seq 100); do [[ -s $out/capture.pcap ]] && break; sleep 0.1; done
if [[ ! -s $out/capture.pcap ]]; then
  docker logs "$name" >&2 || true
  echo "wirecapture: tcpdump did not start capturing" >&2
  exit 1
fi
sleep 1
# Wall clock just before the client starts, to place its gctrace times
# (relative to process start) on the capture's clock; see gcalign.py.
date +%s%N >"$out/start_ns"
"$@" >"$out/clientbench.log" 2>&1
sleep 1
docker stop -t 5 "$name" >/dev/null
docker logs "$name" >"$out/tcpdump.log" 2>&1
docker rm "$name" >/dev/null
trap - EXIT
# tcpdump reports "N packets dropped by kernel" on exit; any drop loses data.
dropped=$(sed -n 's/^\([0-9]*\) packets dropped by kernel$/\1/p' "$out/tcpdump.log")
if [[ -z $dropped ]]; then
  echo "wirecapture: no tcpdump statistics in $out/tcpdump.log" >&2
  exit 1
fi
if [[ $dropped != 0 ]]; then
  echo "wirecapture: tcpdump dropped $dropped packets" >&2
  exit 1
fi
"$wiretrace" -pcap "$out/capture.pcap" -server-ports "$ports" -events "$out/events.csv" ${WIRETRACE_ROUND:+-round "$WIRETRACE_ROUND"} >"$out/summary.json"
cat "$out/summary.json"
