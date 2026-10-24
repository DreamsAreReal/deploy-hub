load helpers

setup() {
  setup_world "${BATS_TEST_DIRNAME}" "${BATS_TEST_TMPDIR}"
  STATUS=$(patched ci-runner-status \
    -e "s#/etc/systemd/system/multi-user.target.wants#${WORLD}/wants#" \
    -e "s#/sys/fs/cgroup#${WORLD}/cg#" -e "s#/proc/#${WORLD}/proc/#" -e "s#/run/netns/#${WORLD}/netns/#g" \
    -e "s#/usr/local/lib/ci-runner/ci-netns#${STUBS}/ci-netns#")
  mkdir -p "${WORLD}/wants" "${WORLD}/netns" "${WORLD}/units" "${WORLD}/uid" "${WORLD}/ok" "${WORLD}/project" "${WORLD}/ss"
  stub systemctl '
case "$1 $2 $3" in
"show -p ControlGroup") cat "${WORLD}/units/$5/cg" 2>/dev/null ;;
"show -p MainPID") cat "${WORLD}/units/$5/pid" 2>/dev/null || echo 0 ;;
"-q is-active "*) [ -e "${WORLD}/units/$3/active" ] ;;
esac'
  stub ip 'cat "${WORLD}/ss/$3" 2>/dev/null'
  stub mountpoint '[ -e "${WORLD}/mounted" ]'
  stub nft '[ -e "${WORLD}/nft" ]'
  stub id 'cat "${WORLD}/uid/$2" 2>/dev/null'
  stub ci-netns '[ -e "${WORLD}/ok/$2" ]'
  stub findmnt 'cat "${WORLD}/options"'
  stub lsattr 'printf "%5s --------------e-----P--- %s\n" "$(cat "${WORLD}/project/${2##*/}")" "$2"'
  stub df 'case "$3" in /var/lib/ci) printf "Used Size\n50 100\n" ;; *) printf "Used Size\n25 100\n" ;; esac'
  stub curl 'cat >"${WORLD}/pushed"'
  touch "${WORLD}/mounted" "${WORLD}/nft"
  echo rw,relatime,prjquota >"${WORLD}/options"
  export PATH="${STUBS}:${PATH}"
  unset DEPLOY_HUB_METRICS_URL
}

proc() {
  mkdir -p "${WORLD}/proc/$2/ns" "${WORLD}/units/$1"
  echo "$2" >"${WORLD}/units/$1/pid"
  ln "$3" "${WORLD}/proc/$2/ns/net"
}

runner() {
  local repo=$1 uid=$2 base=$3 ns=${WORLD}/netns/ci-$1
  touch "$ns" "${WORLD}/ok/${repo}"
  touch "${WORLD}/ci-runner@.service"
  ln -sf "${WORLD}/ci-runner@.service" "${WORLD}/wants/ci-runner@${repo}.service"
  echo "$uid" >"${WORLD}/uid/ci-${repo}"
  echo "$uid" >"${WORLD}/project/${repo}"
  proc "ci-runner@${repo}.service" "$base" "$ns"
  proc "ci-docker@${repo}.service" "$((base + 1))" "$ns"
  proc "user@${uid}.service" "$((base + 2))" "$ns"
  touch "${WORLD}/units/ci-runner@${repo}.service/active"
  echo "/ci.slice/${repo}" >"${WORLD}/units/ci-runner@${repo}.service/cg"
  mkdir -p "${WORLD}/cg/ci.slice/${repo}"
  printf '%s\n%s\n' "$base" "$((base + 5))" >"${WORLD}/cg/ci.slice/${repo}/cgroup.procs"
  printf 'ESTAB 0 0 10.79.0.2:5000 140.82.112.21:443 users:(("Runner.Listener",pid=%s,fd=3))\n' "$((base + 5))" >"${WORLD}/ss/ci-${repo}"
}

healthy() {
  printf 'deploy_hub_ci_runner_up{runner="%s"} %s\ndeploy_hub_ci_quota_on{runner="%s"} 1\ndeploy_hub_ci_quota_used_ratio{runner="%s"} 0.2500\n' "$1" "$2" "$1" "$1"
}

stops() {
  grep '^systemctl stop' "$CALLS" || true
}

@test "an isolated connected runner is up and the metrics are pushed" {
  runner tasks 1001 100
  run bash "$STATUS"
  [ "$status" -eq 0 ]
  expected="$(printf 'deploy_hub_ci_isolation_ok 1\n%s\ndeploy_hub_ci_disk_used_ratio 0.5000' "$(healthy tasks 1)")"
  [ "$output" = "$expected" ]
  [ "$(cat "${WORLD}/pushed")" = "$expected" ]
  grep -qx 'curl -fsS --max-time 10 --data-binary @- http://127.0.0.1:8428/api/v1/import/prometheus' "$CALLS"
  grep -qx 'ip netns exec ci-tasks ss -Htnp state established ( dport = :443 )' "$CALLS"
  [ -z "$(stops)" ]
}

@test "a runner without a connection of its own processes to GitHub is down" {
  runner tasks 1001 100
  printf 'ESTAB 0 0 10.79.0.2:5000 1.2.3.4:443 users:(("x",pid=1050,fd=3))\n' >"${WORLD}/ss/ci-tasks"
  run bash "$STATUS"
  [ "${lines[1]}" = 'deploy_hub_ci_runner_up{runner="tasks"} 0' ]
  rm "${WORLD}/units/ci-runner@tasks.service/active"
  run bash "$STATUS"
  [ "${lines[1]}" = 'deploy_hub_ci_runner_up{runner="tasks"} 0' ]
  [ "${lines[0]}" = 'deploy_hub_ci_isolation_ok 1' ]
}

@test "a runner without a cgroup or processes is down" {
  runner tasks 1001 100
  touch "${WORLD}/units/ci-runner@tasks.service/active"
  : >"${WORLD}/cg/ci.slice/tasks/cgroup.procs"
  run bash "$STATUS"
  [ "${lines[1]}" = 'deploy_hub_ci_runner_up{runner="tasks"} 0' ]
  rm "${WORLD}/cg/ci.slice/tasks/cgroup.procs"
  run bash "$STATUS"
  [ "${lines[1]}" = 'deploy_hub_ci_runner_up{runner="tasks"} 0' ]
  rm "${WORLD}/units/ci-runner@tasks.service/cg"
  run bash "$STATUS"
  [ "${lines[1]}" = 'deploy_hub_ci_runner_up{runner="tasks"} 0' ]
  [ -z "$(grep '^ip ' "$CALLS")" ]
}

@test "a runner whose namespace check fails is stopped" {
  runner tasks 1001 100
  runner router-bot 1002 200
  rm "${WORLD}/ok/tasks"
  run bash "$STATUS"
  [ "${lines[0]}" = 'deploy_hub_ci_isolation_ok 0' ]
  [ "${lines[1]}" = 'deploy_hub_ci_runner_up{runner="router-bot"} 1' ]
  [ "${lines[4]}" = 'deploy_hub_ci_runner_up{runner="tasks"} 0' ]
  [ "$(stops)" = "systemctl stop ci-runner@tasks.service" ]
}

@test "a runner, dockerd or user manager outside the namespace stops the runner" {
  for unit in ci-runner@tasks.service ci-docker@tasks.service user@1001.service; do
    rm -rf "${WORLD}"/*/ci-runner@tasks.service "${WORLD}/netns" "${WORLD}/proc" "${WORLD}/units"
    mkdir -p "${WORLD}/netns" "${WORLD}/units"
    : >"$CALLS"
    runner tasks 1001 100
    pid=$(cat "${WORLD}/units/${unit}/pid")
    rm "${WORLD}/proc/${pid}/ns/net"
    touch "${WORLD}/proc/${pid}/ns/net"
    run bash "$STATUS"
    [ "${lines[0]}" = 'deploy_hub_ci_isolation_ok 0' ]
    [ "$(stops)" = "systemctl stop ci-runner@tasks.service" ]
  done
}

@test "a unit without a main process and a missing namespace file" {
  runner tasks 1001 100
  rm "${WORLD}/units/ci-docker@tasks.service/pid"
  run bash "$STATUS"
  [ "${lines[0]}" = 'deploy_hub_ci_isolation_ok 1' ]
  rm "${WORLD}/proc/100/ns/net" "${WORLD}/netns/ci-tasks"
  run bash "$STATUS"
  [ "${lines[0]}" = 'deploy_hub_ci_isolation_ok 0' ]
}

@test "a runner whose user is gone is stopped" {
  runner tasks 1001 100
  rm "${WORLD}/uid/ci-tasks"
  run bash "$STATUS"
  [ "${lines[0]}" = 'deploy_hub_ci_isolation_ok 0' ]
  [ "${lines[2]}" = 'deploy_hub_ci_quota_on{runner="tasks"} 0' ]
  [ "$(stops)" = "systemctl stop ci-runner@tasks.service" ]
}

@test "without the CI disk or the nftables table every runner stops" {
  runner tasks 1001 100
  rm "${WORLD}/nft"
  run bash "$STATUS"
  [ "$output" = "$(printf 'deploy_hub_ci_isolation_ok 0\n%s\ndeploy_hub_ci_disk_used_ratio 0.5000' "$(healthy tasks 0)")" ]
  [ "$(stops)" = "systemctl stop ci-runner@*.service" ]
  [ -z "$(grep '^ci-netns ' "$CALLS")" ]
  touch "${WORLD}/nft"
  rm "${WORLD}/mounted"
  : >"$CALLS"
  run bash "$STATUS"
  [ "${lines[0]}" = 'deploy_hub_ci_isolation_ok 0' ]
  [ "${#lines[@]}" -eq 4 ]
  [ "$(stops)" = "systemctl stop ci-runner@*.service" ]
}

@test "the quota counts only with prjquota and the runner's project" {
  runner tasks 1001 100
  echo rw,relatime >"${WORLD}/options"
  run bash "$STATUS"
  [ "${lines[2]}" = 'deploy_hub_ci_quota_on{runner="tasks"} 0' ]
  [ "${lines[3]}" = 'deploy_hub_ci_disk_used_ratio 0.5000' ]
  echo rw,prjquota >"${WORLD}/options"
  echo 7 >"${WORLD}/project/tasks"
  run bash "$STATUS"
  [ "${lines[2]}" = 'deploy_hub_ci_quota_on{runner="tasks"} 0' ]
}

@test "metrics go to the configured address or nowhere" {
  runner tasks 1001 100
  DEPLOY_HUB_METRICS_URL= run bash "$STATUS"
  [ "$status" -eq 0 ]
  [ -z "$(grep '^curl' "$CALLS")" ]
  DEPLOY_HUB_METRICS_URL=http://vm:8428/x run bash "$STATUS"
  grep -qx 'curl -fsS --max-time 10 --data-binary @- http://vm:8428/x' "$CALLS"
}

@test "dangling or missing runner links are skipped" {
  run bash "$STATUS"
  [ "$output" = "$(printf 'deploy_hub_ci_isolation_ok 1\ndeploy_hub_ci_disk_used_ratio 0.5000')" ]
  ln -s "${WORLD}/nowhere" "${WORLD}/wants/ci-runner@gone.service"
  run bash "$STATUS"
  [ "$output" = "$(printf 'deploy_hub_ci_isolation_ok 1\ndeploy_hub_ci_disk_used_ratio 0.5000')" ]
}
