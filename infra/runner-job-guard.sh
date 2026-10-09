#!/bin/bash
# runner-job-guard.sh — ACTIONS_RUNNER_HOOK_JOB_STARTED on the self-hosted runners.
#
# This repo is public and its runners are persistent, with sudo, Docker and the
# fleet's shared Bazel cache. A pull_request job runs the PR's own workflow file,
# so a guard in the workflow is no guard at all: it has to live on the box. This
# hook runs before every job and fails the job unless it comes from this
# repository itself. Fork pull requests get GitHub-hosted CI only
# (hosted-smoke.yml); to run the full suite on a fork's change, a maintainer
# pushes the reviewed commit to a branch of this repository.
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
	# @claude on a pull request from a fork: the agent would act on fork content.
	if jq -e '.issue.pull_request' "${GITHUB_EVENT_PATH:?}" >/dev/null; then
		echo "runner-job-guard: refusing issue_comment on a pull request on a self-hosted runner" >&2
		exit 1
	fi
	;;
esac
exit 0
