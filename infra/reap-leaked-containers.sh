#!/bin/bash
# Removes containers an earlier job left running on this runner, so each job
# starts with the whole box its Bazel resource budget assumes. A cancelled job's
# runner kills Bazel but not the containers its actions started: a timed-out
# job's FDB C++ build (3 CPUs, 5 GB) ran on into the next job, which swapped,
# slowed 20x and had factory_test OOM-killed. ci.yml runs this first in every
# self-hosted job; infra/reap_leaked_containers_test.sh pins it.
set -uo pipefail
command -v docker >/dev/null 2>&1 || exit 0
now=$(date -u +%s)

# Containers older than the oldest in-flight job belong to no job. pgrep -x and
# ps etimes, as in cloud-init's orphan-fdb-sweep.sh; infra/README.md says why.
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
  read -r started policy name image < <(docker inspect -f '{{.State.StartedAt}} {{.HostConfig.RestartPolicy.Name}} {{.Name}} {{.Config.Image}}' "$id" 2>/dev/null) || continue
  # Persistent infra (bazel-remote) runs under a restart policy; job containers never do.
  case "$policy" in '' | no) ;; *) continue ;; esac
  sepoch=$(date -u -d "$started" +%s 2>/dev/null) || continue
  [ "$sepoch" -lt "$oldest" ] || continue
  echo "::warning::removing ${name#/} ($image), left running $((now - sepoch))s by an earlier job"
  # -v: an orphan's anonymous volumes would otherwise be stranded.
  docker rm -fv "$id" >/dev/null 2>&1 || echo "::warning::could not remove $id"
done

free -m 2>/dev/null | awk '/^Mem:/ { print "memory at job start: " $3 " MB used, " $7 " MB available" }'
exit 0
