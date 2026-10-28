load helpers

setup() {
  setup_world "${BATS_TEST_DIRNAME}" "${BATS_TEST_TMPDIR}"
  HOOK=$(patched ci-job-started -e "s#/usr/local/lib/ci-runner/ci-runner-reap#${STUBS}/reap#")
  stub reap
  stub id 'echo ci-tasks'
  disk 50%
  export PATH="${STUBS}:${PATH}" HOME="${WORLD}/home"
  export GITHUB_REPOSITORY=DreamsAreReal/tasks GITHUB_EVENT_NAME=push GITHUB_REF=refs/heads/main
  export GITHUB_WORKFLOW_REF=DreamsAreReal/tasks/.github/workflows/deploy.yml@refs/heads/main
  unset GITHUB_BASE_REF GITHUB_EVENT_PATH DOCKER_HOST
}

disk() {
  stub df "printf 'Used Size Use%%\n 12G 40G %s\n' '$1'"
}

pull_request() {
  printf '{"pull_request":{"head":{"repo":{"full_name":"%s"}}}}\n' "$1" >"${WORLD}/event.json"
  export GITHUB_EVENT_NAME=pull_request GITHUB_BASE_REF=main GITHUB_REF=refs/pull/7/merge GITHUB_EVENT_PATH="${WORLD}/event.json"
  export GITHUB_WORKFLOW_REF=DreamsAreReal/tasks/.github/workflows/deploy.yml@refs/pull/7/merge
}

refused() {
  [ "$status" -eq 1 ]
  [ "$output" = "ci-job-started: $1" ]
}

@test "a push to main runs and reports the disk quota" {
  run bash "$HOOK"
  [ "$status" -eq 0 ]
  [ "$output" = "ci-job-started: disk quota of ci-tasks: 12G of 40G GiB (50%)" ]
  [ "$(calls)" = "$(printf 'reap \nid -un\ndf -B1G --output=used,size,pcent %s' "${WORLD}/home")" ]
}

@test "manual and scheduled runs of main are taken" {
  for event in workflow_dispatch schedule; do
    GITHUB_EVENT_NAME=$event run bash "$HOOK"
    [ "$status" -eq 0 ]
  done
}

@test "a job is refused when a process of an earlier job survived" {
  stub reap 'exit 1'
  run bash "$HOOK"
  refused "a process of an earlier job survived, refusing the job"
  [ "$(calls)" = "reap " ]
}

@test "other events are refused" {
  GITHUB_EVENT_NAME=pull_request_target run bash "$HOOK"
  refused "this runner takes no pull_request_target events"
  GITHUB_EVENT_NAME= run bash "$HOOK"
  refused "this runner takes no unknown events"
}

@test "only main is taken on push" {
  GITHUB_REF=refs/heads/dev run bash "$HOOK"
  refused "this runner takes only refs/heads/main, got refs/heads/dev"
  GITHUB_REF= run bash "$HOOK"
  refused "this runner takes only refs/heads/main, got ?"
}

@test "a workflow from another repository or ref is refused" {
  GITHUB_WORKFLOW_REF=Evil/tasks/.github/workflows/deploy.yml@refs/heads/main run bash "$HOOK"
  refused "workflow Evil/tasks/.github/workflows/deploy.yml@refs/heads/main is not from refs/heads/main of DreamsAreReal/tasks"
  GITHUB_WORKFLOW_REF=DreamsAreReal/tasks/.github/workflows/deploy.yml@refs/heads/dev run bash "$HOOK"
  refused "workflow DreamsAreReal/tasks/.github/workflows/deploy.yml@refs/heads/dev is not from refs/heads/main of DreamsAreReal/tasks"
  GITHUB_WORKFLOW_REF=DreamsAreReal/tasks/other/deploy.yml@refs/heads/main run bash "$HOOK"
  refused "workflow DreamsAreReal/tasks/other/deploy.yml@refs/heads/main is not from refs/heads/main of DreamsAreReal/tasks"
  unset GITHUB_WORKFLOW_REF GITHUB_REPOSITORY
  run bash "$HOOK"
  refused "workflow ? is not from refs/heads/main of ?"
}

@test "a pull request into main from the repository itself runs" {
  pull_request DreamsAreReal/tasks
  run bash "$HOOK"
  [ "$status" -eq 0 ]
}

@test "a pull request into another branch is refused" {
  pull_request DreamsAreReal/tasks
  GITHUB_BASE_REF=release run bash "$HOOK"
  refused "pull request into release, not main"
  unset GITHUB_BASE_REF
  run bash "$HOOK"
  refused "pull request into ?, not main"
}

@test "a pull request from a fork or an unknown repository is refused" {
  pull_request Evil/tasks
  run bash "$HOOK"
  refused "pull request from Evil/tasks, not DreamsAreReal/tasks"
  unset GITHUB_EVENT_PATH GITHUB_REPOSITORY
  run bash "$HOOK"
  refused "pull request from an unknown repo, not ?"
}

@test "a pull request job must run the merge ref" {
  pull_request DreamsAreReal/tasks
  GITHUB_REF=refs/heads/feature run bash "$HOOK"
  refused "pull request ref refs/heads/feature is not refs/pull/<n>/merge"
  GITHUB_REF= run bash "$HOOK"
  refused "pull request ref ? is not refs/pull/<n>/merge"
}

@test "a full disk quota refuses the job" {
  for full in 95% 99% 100%; do
    disk "$full"
    run bash "$HOOK"
    [ "$status" -eq 1 ]
    [ "${lines[1]}" = "ci-job-started: the disk quota of ci-tasks is full even after the last cleanup, refusing the job; free ${WORLD}/home or raise its line in server/ci-runner/ci-disk-quota of deploy-hub" ]
  done
  for fine in 94% 9% 10%; do
    disk "$fine"
    run bash "$HOOK"
    [ "$status" -eq 0 ]
  done
}

@test "an unreadable quota does not block the job" {
  stub df 'exit 1'
  run bash "$HOOK"
  [ "$status" -eq 0 ]
  [ "$output" = "ci-job-started: disk quota of ci-tasks: unknown" ]
  unset HOME
  run bash "$HOOK"
  [ "$status" -eq 0 ]
  grep -qx 'df -B1G --output=used,size,pcent /nonexistent' "$CALLS"
}

@test "a taken job raises the flag that holds back the full mutation run" {
  mkdir -p "${WORLD}/run/ci/tasks"
  DOCKER_HOST="unix://${WORLD}/run/ci/tasks/docker.sock" run bash "$HOOK"
  [ "$status" -eq 0 ]
  [ -e "${WORLD}/run/ci/tasks/job-running" ]
}

@test "a refused job raises no flag" {
  mkdir -p "${WORLD}/run/ci/tasks"
  GITHUB_REF=refs/heads/dev DOCKER_HOST="unix://${WORLD}/run/ci/tasks/docker.sock" run bash "$HOOK"
  [ "$status" -eq 1 ]
  [ ! -e "${WORLD}/run/ci/tasks/job-running" ]
}
