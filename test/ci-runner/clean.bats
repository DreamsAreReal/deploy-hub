load helpers

setup() {
  setup_world "${BATS_TEST_DIRNAME}" "${BATS_TEST_TMPDIR}"
  CLEAN=$(patched ci-runner-clean -e "s#/usr/local/lib/ci-runner/ci-runner-reap#${STUBS}/reap#")
  stub reap
  stub id 'echo ci-tasks'
  stub docker '[ "$1 $2" = "ps -aq" ] && printf "c1\nc2\n"; [ "$1 $2" = "image ls" ] && printf "i2\ni1\ni2\n"; exit 0'
  stub df "$(declare -f df_fake)
df_fake \"\$@\""
  disk 40
  export PATH="${STUBS}:${PATH}" HOME="${WORLD}/home" GITHUB_REPOSITORY_OWNER=DreamsAreReal
  export GITHUB_WORKSPACE="${WORLD}/work/tasks/tasks" RUNNER_TEMP="${WORLD}/work/_temp" TMPDIR="${WORLD}/tmp" RUNNER_TOOL_CACHE="${WORLD}/tool"
  unset CI_DISK_MAX_PERCENT CI_BUILD_CACHE_KEEP DOCKER_HOST
  for d in "$GITHUB_WORKSPACE" "$RUNNER_TEMP" "$TMPDIR" "$RUNNER_TOOL_CACHE" "${WORLD}/work/_actions" "$HOME/.cache" "$HOME/go" "$HOME/.npm" "$HOME/.nuget" "$HOME/runner"; do
    mkdir -p "$d/sub"
    touch "$d/sub/file"
  done
}

df_fake() {
  local pct
  pct=$(head -1 "${WORLD}/disk")
  [ "$(wc -l <"${WORLD}/disk")" -gt 1 ] && sed -i 1d "${WORLD}/disk"
  case "$*" in
  --output=pcent*) printf ' Use%%\n %s%%\n' "$pct" ;;
  *) printf ' Used Size\n 14G 40G\n' ;;
  esac
}

disk() {
  printf '%s\n' "$@" >"${WORLD}/disk"
}

emptied() {
  [ -d "$1" ] && [ -z "$(ls -A "$1")" ]
}

docker_calls() {
  grep '^docker ' "$CALLS"
}

@test "a job leaves no containers, own images or files behind" {
  run bash "$CLEAN"
  [ "$status" -eq 0 ]
  [ "$output" = "$(printf 'ci-runner-clean: disk before: 40%%\nci-runner-clean: disk after: 40%% (14G of 40G GiB)')" ]
  [ "$(docker_calls)" = "$(cat <<'CALLS'
docker ps -aq
docker rm -f c1 c2
docker network prune -f
docker volume prune -af
docker image ls --filter reference=ghcr.io/dreamsarereal/* --format {{.ID}}
docker image rm -f i1 i2
docker image prune -f
docker builder prune -f --max-used-space 15gb
CALLS
)" ]
  for d in "$GITHUB_WORKSPACE" "$RUNNER_TEMP" "$TMPDIR" "$RUNNER_TOOL_CACHE" "${WORLD}/work/_actions"; do
    emptied "$d"
  done
  [ -f "$HOME/.cache/sub/file" ]
  [ -f "$HOME/runner/sub/file" ]
  [ "$(head -1 "$CALLS")" = "reap " ]
}

@test "without an owner no image is removed by name" {
  stub docker 'exit 0'
  unset GITHUB_REPOSITORY_OWNER
  run bash "$CLEAN"
  [ "$status" -eq 0 ]
  [ -z "$(grep 'docker image ls\|docker image rm\|docker rm' "$CALLS")" ]
}

@test "read-only files and unset or missing directories do not stop the cleanup" {
  chmod -R a-w "$GITHUB_WORKSPACE/sub"
  export TMPDIR="${WORLD}/missing"
  unset RUNNER_TOOL_CACHE RUNNER_TEMP
  run bash "$CLEAN"
  [ "$status" -eq 0 ]
  [ "${#lines[@]}" -eq 2 ]
  emptied "$GITHUB_WORKSPACE"
  [ -f "${WORLD}/work/_actions/sub/file" ]
  [ -f "${WORLD}/tool/sub/file" ]
}

@test "a full disk drops every image and the language caches" {
  disk 40 75 50
  run bash "$CLEAN"
  [ "$status" -eq 0 ]
  [ "${lines[1]}" = "ci-runner-clean: disk over 70%: dropping every image, the build cache and the language caches" ]
  [ "${lines[2]}" = "ci-runner-clean: disk after: 50% (14G of 40G GiB)" ]
  grep -qx 'docker system prune -af --volumes' "$CALLS"
  for d in .cache go .npm .nuget; do
    emptied "$HOME/$d"
  done
  [ -f "$HOME/runner/sub/file" ]
}

@test "the disk limit and the build cache size can be tuned" {
  disk 40 60 50
  CI_DISK_MAX_PERCENT=60 CI_BUILD_CACHE_KEEP=5gb run bash "$CLEAN"
  [ "${lines[1]}" = "ci-runner-clean: disk over 60%: dropping every image, the build cache and the language caches" ]
  grep -qx 'docker builder prune -f --max-used-space 5gb' "$CALLS"
  disk 40 69
  : >"$CALLS"
  run bash "$CLEAN"
  [ -z "$(grep 'system prune' "$CALLS")" ]
}

@test "an exhausted quota is reported as an error of the job" {
  disk 98 40
  run bash "$CLEAN"
  [ "$status" -eq 0 ]
  [ "${lines[1]}" = "::error::ci-runner-clean: the disk quota of ci-tasks is exhausted (14G of 40G GiB): writes failed with 'Disk quota exceeded'. The cleanup below drops the caches; the quotas live in server/ci-runner/ci-disk-quota of deploy-hub." ]
  disk 97 40
  run bash "$CLEAN"
  [ "${lines[1]}" != "${lines[1]#ci-runner-clean: disk after}" ]
}

@test "a failed reap does not stop the cleanup" {
  stub reap 'exit 1'
  run bash "$CLEAN"
  [ "$status" -eq 0 ]
  emptied "$GITHUB_WORKSPACE"
}

@test "a finished job lifts the flag that holds back the full mutation run" {
  mkdir -p "${WORLD}/run/ci/tasks"
  touch "${WORLD}/run/ci/tasks/job-running"
  DOCKER_HOST="unix://${WORLD}/run/ci/tasks/docker.sock" run bash "$CLEAN"
  [ "$status" -eq 0 ]
  [ ! -e "${WORLD}/run/ci/tasks/job-running" ]
}
