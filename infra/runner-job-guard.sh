#!/bin/bash
# Host-side job hook: a PR can edit its workflow, but not this baked policy.
# Reject foreign repositories, fork PR events, and all PR issue_comments
# (those payloads lack the head repository). Ordinary issue events are allowed.
# Keep the cloud-init.yaml payload identical; TestCloudInitInstallsRunnerJobGuard pins it.
set -euo pipefail

REPO="birdayz/fdb-go"

if [ "${GITHUB_REPOSITORY:-}" != "$REPO" ]; then
  echo "runner-job-guard: refusing job for repository '${GITHUB_REPOSITORY:-unset}'" >&2
  exit 1
fi

case "${GITHUB_EVENT_NAME:-}" in
pull_request | pull_request_target | pull_request_review | pull_request_review_comment)
  head=$(jq -r '.pull_request.head.repo.full_name // empty' "${GITHUB_EVENT_PATH:?}")
  if [ "$head" != "$REPO" ]; then
    echo "runner-job-guard: refusing ${GITHUB_EVENT_NAME} from fork '${head:-unknown}' on a self-hosted runner" >&2
    exit 1
  fi
  ;;
issue_comment)
  # A PR issue_comment does not identify the head repository; fail closed.
  is_pr=$(jq -r '.issue | if type == "object" then has("pull_request") else error("missing issue object") end' "${GITHUB_EVENT_PATH:?}")
  if [ "$is_pr" != false ]; then
    echo "runner-job-guard: refusing issue_comment on a pull request on a self-hosted runner" >&2
    exit 1
  fi
  ;;
esac
exit 0
