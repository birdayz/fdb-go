#!/bin/bash
# A cancelled job's runner kills Bazel but not the containers its actions started
# (a leaked 5 GB FDB C++ build OOM-killed the next job); remove them at job start.
set -uo pipefail
command -v docker >/dev/null 2>&1 || exit 0
now=$(date -u +%s)

# Older than the oldest in-flight job means owned by none. pgrep -x and etimes, as
# in cloud-init's orphan-fdb-sweep.sh; infra/README.md says why.
oldest=
for pid in $(pgrep -x Runner.Worker); do
  et=$(ps -o etimes= -p "$pid" 2>/dev/null | tr -d ' ')
  case "$et" in
  '' | *[!0-9]*)
    echo "::warning::Runner.Worker $pid age unreadable: reaping nothing"
    exit 0
    ;;
  esac
  start=$((now - et))
  if [ -z "$oldest" ] || [ "$start" -lt "$oldest" ]; then oldest=$start; fi
done
if [ -z "$oldest" ]; then
  echo "::warning::no Runner.Worker visible: reaping nothing"
  exit 0
fi

docker ps --format '{{.ID}}' 2>/dev/null | while read -r id; do
  # Policy last: an empty one (API-created containers) would otherwise shift the fields.
  read -r started name image policy < <(docker inspect -f '{{.State.StartedAt}} {{.Name}} {{.Config.Image}} {{.HostConfig.RestartPolicy.Name}}' "$id" 2>/dev/null) || continue
  # Persistent infra (bazel-remote) runs under a restart policy; job containers never do.
  case "$policy" in '' | no) ;; *) continue ;; esac
  sepoch=$(date -u -d "$started" +%s 2>/dev/null) || continue
  [ "$sepoch" -lt "$oldest" ] || continue
  echo "::warning::removing ${name#/} ($image), left running $((now - sepoch))s by an earlier job"
  # -v: an orphan's anonymous volumes would otherwise be stranded.
  docker rm -fv "$id" >/dev/null 2>&1 || echo "::warning::could not remove $id"
done
exit 0
