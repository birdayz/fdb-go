#!/usr/bin/env bash
# Wire goldens: run the wgscen scenarios on libfdb_c and on the Go client under
# a packet capture, render each client's requests as a normalized transcript
# (cmd/wiregolden), and compare both with the libfdb_c golden.
#
# Usage: wiregolden.sh [-update] OUTDIR CLUSTER_FILE PORTS
#   PORTS: the cluster's fdbserver ports, comma-separated. Use an idle cluster:
#   the transcript holds every request to those ports.
#   -update runs libfdb_c twice, requires identical transcripts, and rewrites
#   the golden from them and the known Go divergences from the Go run.
#
# Fails when libfdb_c's transcript differs from the golden (a scenario that is
# not deterministic, or a changed libfdb_c), or when the Go client's
# differences from the golden are not exactly the known ones
# (wiregolden/go-divergences.diff). Requires docker (wirecapture.sh), Bazel and
# LIBFDB_C_DIR (default ~/.local/share/foundationdb/7.3.77/lib). BAZEL overrides
# the Bazel command (default bazelisk).
set -euo pipefail
update=0
[[ ${1:-} == -update ]] && { update=1; shift; }
(($# == 3)) || { echo "usage: wiregolden.sh [-update] OUTDIR CLUSTER_FILE PORTS" >&2; exit 2; }
mkdir -p "$1"
out=$(cd "$1" && pwd)
cluster=$(cd "$(dirname "$2")" && pwd)/$(basename "$2")
ports=$3
here=$(cd "$(dirname "$0")" && pwd)
root=$(cd "$here/../../.." && pwd)
data=$here/../wiregolden
lib=${LIBFDB_C_DIR:-$HOME/.local/share/foundationdb/7.3.77/lib}
[[ -f $cluster ]] || { echo "wiregolden: no cluster file $cluster" >&2; exit 2; }
bazel=${BAZEL:-bazelisk}
cd "$root"
# Build outside command substitution, so a failed build stops the script
# instead of installing an older output.
for t in //tools/clientbench/cmd/wgscen:wgscen_go //tools/clientbench/cmd/wgscen:wgscen_cgo //tools/clientbench/cmd/wiregolden; do
  if ! "$bazel" build "$t" >&2; then
    echo "wiregolden: building $t failed" >&2
    exit 2
  fi
  f=$("$bazel" cquery --output=files "$t" 2>/dev/null | head -1)
  if [[ -z $f || ! -x $f ]]; then
    echo "wiregolden: no executable for $t" >&2
    exit 2
  fi
  install -m755 "$f" "$out/$(basename "${t##*:}")"
done
"$out/wgscen_go" -list >"$out/scenarios.txt"
# diffu A B LABEL_A LABEL_B > OUT: 0 equal, 1 different; anything else is a failure.
diffu() {
  local rc=0
  diff -u --label "$3" --label "$4" "$1" "$2" || rc=$?
  ((rc <= 1)) || { echo "wiregolden: diff failed ($rc)" >&2; exit 2; }
  return $rc
}
run() { # client name
  rm -rf "$out/$2"
  LD_LIBRARY_PATH=$lib WIRETRACE=true "$here/wirecapture.sh" "$out/$2" "$ports" -- \
    "$out/wgscen_$1" -cluster-file "$cluster" >/dev/null
  "$out/wiregolden" -pcap "$out/$2/capture.pcap" -server-ports "$ports" -expect "$out/scenarios.txt" >"$out/$2.transcript"
}
run cgo cgo
run go go
if ((update)); then
  run cgo cgo-rerun
  if ! diffu "$out/cgo.transcript" "$out/cgo-rerun.transcript" libfdb_c libfdb_c-rerun >"$out/cgo-rerun.diff"; then
    cat "$out/cgo-rerun.diff" >&2
    echo "wiregolden: libfdb_c is not deterministic across runs; golden not updated" >&2
    exit 1
  fi
  cp "$out/cgo.transcript" "$data/libfdbc.golden"
fi
diffu "$data/libfdbc.golden" "$out/go.transcript" golden go >"$out/go.diff" || true
if ((update)); then
  cp "$out/go.diff" "$data/go-divergences.diff"
  echo "wiregolden: golden and known Go divergences updated"
  exit 0
fi
status=0
if ! diffu "$data/libfdbc.golden" "$out/cgo.transcript" golden libfdb_c; then
  echo "wiregolden: libfdb_c differs from the golden" >&2
  status=1
fi
if ! diffu "$data/go-divergences.diff" "$out/go.diff" known now; then
  echo "wiregolden: the Go client's divergences changed (see $out/go.diff)" >&2
  status=1
fi
((status == 0)) && echo "wiregolden: libfdb_c matches the golden; Go differs only by the known divergences"
exit $status
