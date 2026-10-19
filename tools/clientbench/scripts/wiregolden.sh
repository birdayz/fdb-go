#!/usr/bin/env bash
# Wire goldens: run the wgscen scenarios on libfdb_c and on the Go client under
# a packet capture, render each client's requests as a normalized transcript
# (cmd/wiregolden), and compare both with the libfdb_c golden.
#
# Usage: wiregolden.sh [-update] OUTDIR CLUSTER_FILE PORTS
#   PORTS: the cluster's fdbserver ports, comma-separated. Use an idle cluster:
#   the transcript holds every request to those ports.
#   -update rewrites the golden from this libfdb_c run and the known Go
#   divergences from this Go run.
#
# Fails when libfdb_c's transcript differs from the golden (a scenario that is
# not deterministic, or a changed libfdb_c), or when the Go client's
# differences from the golden are not exactly the known ones
# (wiregolden/go-divergences.diff). Requires docker (wirecapture.sh), Bazel and
# LIBFDB_C_DIR (default ~/.local/share/foundationdb/7.3.77/lib).
set -euo pipefail
update=0
[[ ${1:-} == -update ]] && { update=1; shift; }
out=$1 cluster=$2 ports=$3
here=$(cd "$(dirname "$0")" && pwd)
root=$(cd "$here/../../.." && pwd)
data=$here/../wiregolden
lib=${LIBFDB_C_DIR:-$HOME/.local/share/foundationdb/7.3.77/lib}
mkdir -p "$out"
out=$(cd "$out" && pwd)
cd "$root"
bin() {
  bazelisk build "$1" >/dev/null 2>&1 || bazelisk build "$1"
  bazelisk cquery --output=files "$1" 2>/dev/null | head -1
}
for t in //tools/clientbench/cmd/wgscen:wgscen_go //tools/clientbench/cmd/wgscen:wgscen_cgo //tools/clientbench/cmd/wiregolden; do
  install -m755 "$(bin "$t")" "$out/$(basename "${t##*:}")"
done
for c in cgo go; do
  rm -rf "$out/$c"
  LD_LIBRARY_PATH=$lib WIRETRACE=true "$here/wirecapture.sh" "$out/$c" "$ports" -- \
    "$out/wgscen_$c" -cluster-file "$cluster" >/dev/null
  "$out/wiregolden" -pcap "$out/$c/capture.pcap" -server-ports "$ports" >"$out/$c.transcript"
done
if ((update)); then
  cp "$out/cgo.transcript" "$data/libfdbc.golden"
fi
diff -u --label golden --label go "$data/libfdbc.golden" "$out/go.transcript" >"$out/go.diff" || true
if ((update)); then
  cp "$out/go.diff" "$data/go-divergences.diff"
  echo "wiregolden: golden and known Go divergences updated"
  exit 0
fi
status=0
if ! diff -u --label golden --label libfdb_c "$data/libfdbc.golden" "$out/cgo.transcript"; then
  echo "wiregolden: libfdb_c differs from the golden" >&2
  status=1
fi
if ! diff -u --label known --label now "$data/go-divergences.diff" "$out/go.diff"; then
  echo "wiregolden: the Go client's divergences changed (see $out/go.diff)" >&2
  status=1
fi
((status == 0)) && echo "wiregolden: libfdb_c matches the golden; Go differs only by the known divergences"
exit $status
