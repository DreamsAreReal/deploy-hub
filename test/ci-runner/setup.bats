load helpers

setup() {
  setup_world "${BATS_TEST_DIRNAME}" "${BATS_TEST_TMPDIR}"
  mkdir -p "${WORLD}/tree/server/ci-runner" "${WORLD}/etc" "${WORLD}/units/multi-user.target.wants" "${WORLD}/ci" "${WORLD}/run/netns" "${WORLD}/apparmor" "${WORLD}/pid" "${WORLD}/fixture/runner"
  cp "${CI_RUNNER_DIR}"/* "${WORLD}/tree/server/ci-runner/"
  printf '#!/bin/sh\necho configured "$@" >"${WORLD}/configured"\n' >"${WORLD}/fixture/runner/config.sh"
  chmod +x "${WORLD}/fixture/runner/config.sh"
  tar -czf "${WORLD}/fixture.tar.gz" -C "${WORLD}/fixture/runner" .
  sha=$(sha256sum "${WORLD}/fixture.tar.gz")
  SETUP=$(patched ci-runner-setup \
    -e "s#^LIB=.*#LIB=${WORLD}/lib#" -e "s#^UNITS=.*#UNITS=${WORLD}/units#" -e "s#^CACHE=.*#CACHE=${WORLD}/cache#" \
    -e "s#^CI_ROOT=.*#CI_ROOT=${WORLD}/ci#" -e "s#^DISK_IMAGE=.*#DISK_IMAGE=${WORLD}/ci.img#" \
    -e "s#^DEPLOY_LOCK=.*#DEPLOY_LOCK=${WORLD}/deploy.lock#" -e "s#^RUNNER_SHA256=.*#RUNNER_SHA256=${sha%% *}#" \
    -e "s#/etc/sub#${WORLD}/etc/sub#g" -e "s#/proc/#${WORLD}/proc/#" -e "s#/etc/apparmor.d/#${WORLD}/apparmor/#g" \
    -e "/^\t/s#/etc/netns/#${WORLD}/etc/netns/#g" -e "/^\t/s#/run/netns/#${WORLD}/run/netns/#g")
  printf 'deploy:100000:65536\n' >"${WORLD}/etc/subuid"
  printf 'deploy:100000:65536\nci-old:300000:65536\n' >"${WORLD}/etc/subgid"
  printf 'root 0\n' >"${WORLD}/users"
  printf 'loop,nodev,noatime,discard\n' >"${WORLD}/options"
  stubs
  export PATH="${STUBS}:${PATH}"
  unset CI_DISK_SIZE
}

stubs() {
  local s
  for s in apt-get apparmor_parser loginctl userdel mkfs.ext4 mount chown chattr setquota find sysctl usermod; do
    stub "$s"
  done
  stub useradd 'echo "${@: -1} $((1000 + $(wc -l <"${WORLD}/users")))" >>"${WORLD}/users"'
  stub id 'u=${@: -1}; line=$(grep "^${u} " "${WORLD}/users") || exit 1; [ "$1" = -u ] && echo "${line#* }"; exit 0'
  stub install "$(declare -f fake_install)
fake_install \"\$@\""
  stub stat '[ "$1 $2" = "-c %U" ] && { cat "${WORLD}/owner-${3##*/}" 2>/dev/null || echo "ci-${3##*/}"; exit 0; }; /usr/bin/stat "$@"'
  stub runuser 'shift 3; "$@"'
  stub systemctl '[ "$1 $2" = "show -p" ] && { cat "${WORLD}/pid/$5" 2>/dev/null || echo 0; }; exit 0'
  stub ip 'case "$1 $2" in "-4 route") echo "1.1.1.1 via 203.0.113.1 dev eth0 src 203.0.113.10" ;; "link show") [ -e "${WORLD}/link-$3" ] ;; "netns exec") shift 3; "$@" ;; esac'
  stub nft 'cat >/dev/null'
  stub iptables 'case " $* " in *" -C "*) exit 0 ;; *" -D "*) n=$(cat "${WORLD}/deletes" 2>/dev/null || echo 0); [ "$n" -gt 0 ] || exit 1; echo $((n - 1)) >"${WORLD}/deletes" ;; esac'
  stub truncate 'touch "${@: -1}"'
  stub tune2fs '[ "$1" = -l ] && { [ -e "${WORLD}/project" ] && echo "Filesystem features:      has_journal ext_attr quota project"; exit 0; }; touch "${WORLD}/project"'
  stub findmnt 'cat "${WORLD}/options"'
  stub lsattr 'printf "%5s ---- %s\n" "$(cat "${WORLD}/proj-${2##*/}" 2>/dev/null || echo 0)" "$2"'
  stub pgrep '[ -e "${WORLD}/busy" ]'
  stub pkill 'exit "$(cat "${WORLD}/pkill" 2>/dev/null || echo 0)"'
  stub flock '[ ! -e "${WORLD}/locked" ]'
  stub mountpoint '[ -e "${WORLD}/mounted" ]'
  stub date 'echo 2026-10-25T03:00:00Z'
  stub curl 'cp "${WORLD}/fixture.tar.gz" "$3"'
}

fake_install() {
  local dir=0 files=()
  while (($#)); do
    case $1 in
    -d) dir=1 ;;
    -m | -o | -g) shift ;;
    *) files+=("$1") ;;
    esac
    shift
  done
  if ((dir)); then mkdir -p "${files[@]}"; else cp "${files[0]}" "${files[1]}"; fi
}

user() {
  printf '%s %s\n' "$1" "$2" >>"${WORLD}/users"
}

repo() {
  user "ci-$1" "$2"
  mkdir -p "${WORLD}/ci/$1/runner"
  touch "${WORLD}/ci/$1/runner/.runner"
  printf 'ci-%s:%s:65536\n' "$1" "$3" | tee -a "${WORLD}/etc/subuid" >>"${WORLD}/etc/subgid"
}

quotas_on() {
  touch "${WORLD}/project"
  echo loop,nodev,noatime,discard,prjquota >"${WORLD}/options"
}

called() {
  grep -qxF -- "$1" "$CALLS"
}

not_called() {
  [ -z "$(grep -F -- "$1" "$CALLS")" ]
}

@test "host prepares a fresh machine" {
  run bash "$SETUP" host
  [ "$status" -eq 0 ]
  called 'apt-get install -y -q uidmap slirp4netns nftables quota linux-image-extra-virtual libicu74 libkrb5-3 liblttng-ust1t64 libssl3t64 zlib1g'
  called "apparmor_parser -r ${WORLD}/apparmor/rootlesskit"
  cmp "${WORLD}/apparmor/rootlesskit" "${CI_RUNNER_DIR}/rootlesskit.apparmor"
  for f in ci-netns ci-runner-status ci-runner-reap; do cmp "${WORLD}/lib/$f" "${CI_RUNNER_DIR}/$f"; done
  cmp "${WORLD}/lib/ci-job-started.sh" "${CI_RUNNER_DIR}/ci-job-started"
  cmp "${WORLD}/lib/ci-runner-clean.sh" "${CI_RUNNER_DIR}/ci-runner-clean"
  for f in ci-netns@.service ci-docker@.service ci-runner@.service ci-runner-status.service ci-runner-status.timer; do cmp "${WORLD}/units/$f" "${CI_RUNNER_DIR}/$f"; done
  called 'nft -f -'
  called "truncate -s 280G ${WORLD}/ci.img"
  called "mkfs.ext4 -q -m 0 -L ci -O quota,project -E quotatype=prjquota ${WORLD}/ci.img"
  [ "$(/usr/bin/stat -c %a "${WORLD}/ci.img")" = 600 ]
  [ "$(sed -n 's/^Options=//p' "${WORLD}/units/var-lib-ci.mount")" = loop,nodev,noatime,discard ]
  called 'systemctl enable --now var-lib-ci.mount'
  not_called 'mount -o remount'
  called "chown root:root ${WORLD}/ci"
  [ "$(/usr/bin/stat -c %a "${WORLD}/ci")" = 755 ]
  [ "$(tail -1 "$CALLS")" = 'systemctl enable --now ci-runner-status.timer' ]
  not_called userdel
  not_called 'iptables -D'
}

@test "host replaces the old clean hook and keeps an existing image" {
  mkdir -p "${WORLD}/lib" "${WORLD}/units/ci-docker@.service.d" "${WORLD}/units/ci-runner@.service.d"
  touch "${WORLD}/lib/ci-runner-clean" "${WORLD}/ci.img" "${WORLD}/units/ci-docker@.service.d/50-ci-user.conf" "${WORLD}/units/ci-runner@.service.d/50-ci-user.conf"
  echo loop,nodev >"${WORLD}/options"
  CI_DISK_SIZE=10G run bash "$SETUP" host
  [ "$status" -eq 0 ]
  [ ! -e "${WORLD}/lib/ci-runner-clean" ]
  [ ! -e "${WORLD}/units/ci-docker@.service.d/50-ci-user.conf" ] && [ ! -e "${WORLD}/units/ci-runner@.service.d/50-ci-user.conf" ]
  not_called truncate
  not_called mkfs.ext4
  called "mount -o remount,discard ${WORLD}/ci"
}

@test "the mount unit asks for project quotas once the image has them" {
  quotas_on
  run bash "$SETUP" host
  [ "$status" -eq 0 ]
  cmp "${WORLD}/units/var-lib-ci.mount" "${CI_RUNNER_DIR}/var-lib-ci.mount"
}

@test "host jails every registered repository and applies its quota" {
  quotas_on
  repo tasks 1001 165536
  repo observability 1003 231072
  touch "${WORLD}/ci/stray"
  mkdir -p "${WORLD}/ci/half/runner"
  echo 1003 >"${WORLD}/proj-observability"
  run bash "$SETUP" host
  [ "$status" -eq 0 ]
  called "systemctl start ci-netns@tasks.service"
  called "loginctl enable-linger ci-tasks"
  [ "$(cat "${WORLD}/etc/netns/ci-tasks/resolv.conf")" = "$(printf 'nameserver 1.1.1.1\nnameserver 9.9.9.9')" ]
  cmp "${WORLD}/units/user-1001.slice.d/50-ci-limits.conf" "${CI_RUNNER_DIR}/ci-limits.conf"
  [ "$(cat "${WORLD}/units/user@1001.service.d/50-ci-jail.conf")" = "$(cat <<UNIT
[Unit]
Requires=ci-netns@tasks.service var-lib-ci.mount
After=ci-netns@tasks.service var-lib-ci.mount

[Service]
Delegate=cpu cpuset io memory pids
NetworkNamespacePath=/run/netns/ci-tasks
BindReadOnlyPaths=/etc/netns/ci-tasks/resolv.conf:/etc/resolv.conf
NoNewPrivileges=yes
ProtectSystem=strict
ReadWritePaths=${WORLD}/ci/tasks /run/user/1001
InaccessiblePaths=/opt /root /home /etc/srv-backup /etc/wireguard /etc/deploy-hub /var/backups /var/lib/docker /var/lib/containerd -/var/lib/app-backup -/run/docker.sock -/var/run/docker.sock -/run/containerd
UNIT
)" ]
  [ "$(cat "${WORLD}/units/ci-docker@tasks.service.d/50-ci-user.conf")" = "$(printf '[Unit]\nRequires=user@1001.service\nAfter=user@1001.service\n\n[Service]\nSlice=user-1001.slice\nEnvironment=DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/1001/bus')" ]
  [ "$(cat "${WORLD}/units/ci-runner@tasks.service.d/50-ci-user.conf")" = "$(printf '[Service]\nSlice=user-1001.slice')" ]
  called "find ${WORLD}/ci/tasks -xdev -type d -exec chattr +P -p 1001 {} +"
  called "find ${WORLD}/ci/tasks -xdev -type f -exec chattr -p 1001 {} +"
  called "setquota -P 1001 0 146800640 0 0 ${WORLD}/ci"
  not_called "find ${WORLD}/ci/observability"
  called "setquota -P 1003 0 20971520 0 0 ${WORLD}/ci"
  not_called useradd
  not_called usermod
  not_called half
}

@test "host warns about repositories without a quota while quotas are off" {
  repo tasks 1001 165536
  run bash "$SETUP" host
  [ "$status" -eq 0 ]
  [ "${lines[-1]}" = "warning: no disk quota for tasks until '$SETUP quota-enable' runs" ]
  not_called setquota
}

@test "a user manager outside the namespace of its repository is restarted" {
  quotas_on
  repo tasks 1001 165536
  touch "${WORLD}/run/netns/ci-tasks"
  mkdir -p "${WORLD}/proc/77/ns"
  echo 77 >"${WORLD}/pid/user@1001.service"
  touch "${WORLD}/proc/77/ns/net"
  run bash "$SETUP" host
  called "systemctl restart user@1001.service"
  rm "${WORLD}/proc/77/ns/net"
  ln "${WORLD}/run/netns/ci-tasks" "${WORLD}/proc/77/ns/net"
  : >"$CALLS"
  run bash "$SETUP" host
  [ "$status" -eq 0 ]
  not_called restart
  rm "${WORLD}/proc/77/ns/net"
  : >"$CALLS"
  run bash "$SETUP" host
  called "systemctl restart user@1001.service"
}

@test "a home owned by someone else stops the setup" {
  quotas_on
  repo tasks 1001 165536
  echo root >"${WORLD}/owner-tasks"
  run bash "$SETUP" host
  [ "$status" -eq 1 ]
  [ "${lines[-1]}" = "${WORLD}/ci/tasks exists but is not owned by ci-tasks: remove it first" ]
}

@test "the shared runner user is removed once the CI disk is mounted" {
  user ci 999
  mkdir -p "${WORLD}/units/user-999.slice.d" "${WORLD}/units/user@999.service.d"
  ln -s "${WORLD}/units/ci-runner@.service" "${WORLD}/units/multi-user.target.wants/ci-runner@tasks.service"
  touch "${WORLD}/units/multi-user.target.wants/ci-runner@plain.service"
  run bash "$SETUP" host
  [ "$status" -eq 1 ]
  [ "${lines[-1]}" = "${WORLD}/ci is not mounted: mount it before removing the user ci" ]
  not_called userdel
  touch "${WORLD}/mounted"
  echo 1 >"${WORLD}/pkill"
  : >"$CALLS"
  run bash "$SETUP" host
  [ "$status" -eq 0 ]
  [ "${lines[0]}" = "removing the shared runner user ci: register every repository again" ]
  [ "$(grep -A6 '^systemctl disable --now ci-runner@tasks.service$' "$CALLS")" = "$(cat <<CALLS
systemctl disable --now ci-runner@tasks.service
systemctl stop ci-runner@*.service ci-docker@*.service
loginctl disable-linger ci
systemctl stop user@999.service
pkill -KILL -U 999
find ${WORLD}/ci -mindepth 1 -maxdepth 1 -user ci -exec rm -rf {} +
userdel ci
CALLS
)" ]
  not_called plain
  [ ! -e "${WORLD}/units/user-999.slice.d" ] && [ ! -e "${WORLD}/units/user@999.service.d" ]
}

@test "a failing kill of the shared user stops the setup" {
  user ci 999
  touch "${WORLD}/mounted"
  echo 2 >"${WORLD}/pkill"
  run bash "$SETUP" host
  [ "$status" -eq 1 ]
  not_called userdel
}

@test "the shared namespace ci is moved to one per repository" {
  touch "${WORLD}/units/ci-netns.service" "${WORLD}/link-ci-host"
  echo 4 >"${WORLD}/deletes"
  mkdir -p "${WORLD}/etc/netns/ci"
  run bash "$SETUP" host
  [ "$status" -eq 0 ]
  [ "${lines[0]}" = "moving the runners from the shared network namespace ci to one per repository" ]
  called 'systemctl disable --now ci-netns.service'
  [ ! -e "${WORLD}/units/ci-netns.service" ] && [ ! -e "${WORLD}/etc/netns/ci" ]
  [ "$(grep -c '^iptables -D FORWARD -i ci-host -o eth0 -j ACCEPT$' "$CALLS")" -eq 5 ]
  [ "$(grep -c '^iptables -D FORWARD -o ci-host -m conntrack --ctstate ESTABLISHED,RELATED -j ACCEPT$' "$CALLS")" -eq 1 ]
  [ "$(grep -c '^iptables -t nat -D POSTROUTING -s 10.79.0.0/30 -o eth0 -j MASQUERADE$' "$CALLS")" -eq 1 ]
  called 'ip link del ci-host'
  not_called 'ip netns del'
  rm "${WORLD}/link-ci-host"
  touch "${WORLD}/run/netns/ci"
  : >"$CALLS"
  run bash "$SETUP" host
  called 'systemctl stop ci-runner@*.service ci-docker@*.service'
  called 'ip netns del ci'
  not_called 'ip link del'
  not_called 'disable --now ci-netns'
}

@test "register installs a runner for a new repository" {
  quotas_on
  run bash "$SETUP" register router-bot <<<"token-123"
  [ "$status" -eq 0 ]
  called "useradd --system --user-group --home-dir ${WORLD}/ci/router-bot --no-create-home --shell /usr/sbin/nologin ci-router-bot"
  called 'usermod --add-subuids 165536-231071 ci-router-bot'
  called 'usermod --add-subgids 365536-431071 ci-router-bot'
  called "curl -fsSL -o ${WORLD}/cache/actions-runner-2.338.1.tar.gz.part https://github.com/actions/runner/releases/download/v2.338.1/actions-runner-linux-x64-2.338.1.tar.gz"
  [ -f "${WORLD}/cache/actions-runner-2.338.1.tar.gz" ] && [ ! -e "${WORLD}/cache/actions-runner-2.338.1.tar.gz.part" ]
  called "runuser -u ci-router-bot -- mkdir -p -m 700 ${WORLD}/ci/router-bot/runner ${WORLD}/ci/router-bot/tmp ${WORLD}/ci/router-bot/docker"
  called "setquota -P 1001 0 41943040 0 0 ${WORLD}/ci"
  [ "$(cat "${WORLD}/configured")" = "configured --unattended --url https://github.com/DreamsAreReal/router-bot --token token-123 --name eu-router-bot --labels eu --work _work --replace" ]
  called "ip netns exec ci-router-bot runuser -u ci-router-bot -- env HOME=${WORLD}/ci/router-bot ${WORLD}/ci/router-bot/runner/config.sh --unattended --url https://github.com/DreamsAreReal/router-bot --token token-123 --name eu-router-bot --labels eu --work _work --replace"
  [ "$(tail -1 "$CALLS")" = 'systemctl enable --now ci-runner@router-bot.service' ]
}

@test "register reuses the cached runner and keeps a configured one" {
  quotas_on
  repo tasks 1001 165536
  mkdir -p "${WORLD}/cache"
  cp "${WORLD}/fixture.tar.gz" "${WORLD}/cache/actions-runner-2.338.1.tar.gz"
  run bash "$SETUP" register tasks <<<"token"
  [ "$status" -eq 0 ]
  not_called curl
  not_called config.sh
  [ ! -e "${WORLD}/configured" ]
  called 'systemctl enable --now ci-runner@tasks.service'
}

@test "register refuses a runner archive with the wrong checksum" {
  quotas_on
  mkdir -p "${WORLD}/cache"
  echo forged >"${WORLD}/cache/actions-runner-2.338.1.tar.gz"
  run bash "$SETUP" register tasks <<<"token"
  [ "$status" -ne 0 ]
  not_called useradd
}

@test "register needs a valid name, a quota line and quotas on" {
  quotas_on
  for name in Tasks -x "a b" abcdefghijklmnopqrstuvwxyz0123; do
    run bash "$SETUP" register "$name" <<<"token"
    [ "$status" -eq 2 ]
    [ "$output" = "bad repository name: $name (lowercase letters, digits, - and _, at most 29)" ]
  done
  run bash "$SETUP" register unknown <<<"token"
  [ "$status" -eq 1 ]
  [ "$output" = "no disk quota for unknown in ${WORLD}/tree/server/ci-runner/ci-disk-quota: add a line 'unknown <GiB>' first" ]
  echo "badline x" >>"${WORLD}/tree/server/ci-runner/ci-disk-quota"
  run bash "$SETUP" register badline <<<"token"
  [ "$status" -eq 1 ]
  rm "${WORLD}/project"
  run bash "$SETUP" register tasks <<<"token"
  [ "$status" -eq 1 ]
  [ "$output" = "project quotas are off on ${WORLD}/ci: run '$SETUP quota-enable' while no job runs" ]
  quotas_on
  echo loop,nodev >"${WORLD}/options"
  run bash "$SETUP" register tasks <<<"token"
  [ "$status" -eq 1 ]
  not_called useradd
  quotas_on
  run bash "$SETUP" register a_b-c-d-e-f-g-h-i-j-k-l-m-n-o <<<"token"
  [ "$status" -eq 1 ]
  [[ "$output" == "no disk quota for a_b-c-d-e-f-g-h-i-j-k-l-m-n-o "* ]]
}

@test "quota-enable switches the image to project quotas while the runners are idle" {
  repo tasks 1001 165536
  echo 1001 >"${WORLD}/proj-tasks"
  touch "${WORLD}/deploy.lock" "${WORLD}/units/ci-runner@.service"
  ln -s "${WORLD}/units/ci-runner@.service" "${WORLD}/units/multi-user.target.wants/ci-runner@tasks.service"
  touch "${WORLD}/units/multi-user.target.wants/ci-runner@plain.service"
  echo loop,nodev,noatime,discard,prjquota >"${WORLD}/options"
  run bash "$SETUP" quota-enable
  [ "$status" -eq 0 ]
  [ "$output" = "$(printf 'stopping the runners to enable project quotas on %s: 2026-10-25T03:00:00Z\nrunners started again: 2026-10-25T03:00:00Z' "${WORLD}/ci.img")" ]
  [ "$(grep -v '^id \|^lsattr \|^findmnt \|^tune2fs -l \|^date \|^pgrep \|^flock ' "$CALLS")" = "$(cat <<CALLS
systemctl stop ci-runner@*.service ci-docker@*.service
systemctl stop user@1001.service
systemctl stop var-lib-ci.mount
tune2fs -O quota,project -Q prjquota ${WORLD}/ci.img
install -m 644 ${WORLD}/tree/server/ci-runner/var-lib-ci.mount ${WORLD}/units/var-lib-ci.mount
systemctl daemon-reload
systemctl start var-lib-ci.mount
setquota -P 1001 0 146800640 0 0 ${WORLD}/ci
systemctl start user@1001.service
systemctl start ci-runner@tasks.service
CALLS
)" ]
}

@test "quota-enable does nothing when the quotas are on" {
  quotas_on
  run bash "$SETUP" quota-enable
  [ "$status" -eq 0 ]
  [ -z "$output" ]
  not_called systemctl
}

@test "quota-enable waits for running jobs and deploys" {
  touch "${WORLD}/busy"
  run bash "$SETUP" quota-enable
  [ "$status" -eq 1 ]
  [ "$output" = "a CI job is running: enable the quotas when the runners are idle" ]
  rm "${WORLD}/busy"
  touch "${WORLD}/deploy.lock" "${WORLD}/locked"
  run bash "$SETUP" quota-enable
  [ "$status" -eq 1 ]
  [ "$output" = "a deploy holds ${WORLD}/deploy.lock: enable the quotas when it is done" ]
  rm "${WORLD}/deploy.lock"
  run bash "$SETUP" quota-enable
  [ "$status" -eq 1 ]
  [ "${lines[-1]}" = "project quotas are still off on ${WORLD}/ci" ]
  called 'systemctl start var-lib-ci.mount'
  not_called 'systemctl start user@'
  not_called setquota
}

@test "a quota that cannot follow every file is reported" {
  quotas_on
  repo tasks 1001 165536
  stub find 'exit 1'
  run bash "$SETUP" host
  [ "$status" -eq 0 ]
  [ "${lines[-1]}" = "warning: some files of ${WORLD}/ci/tasks keep another project and count outside the quota of tasks" ]
  stub find '[ "$4" = f ] && exit 1; exit 0'
  run bash "$SETUP" host
  [ "${lines[-1]}" = "warning: some files of ${WORLD}/ci/tasks keep another project and count outside the quota of tasks" ]
  stub find
  run bash "$SETUP" host
  [ -z "$output" ]
}

@test "a wrong command prints the usage" {
  run bash "$SETUP"
  [ "$status" -eq 2 ]
  [ "$output" = "usage: $SETUP host | quota-enable | register <repo> < registration-token" ]
  run bash "$SETUP" register
  [ "$status" -ne 0 ]
  [[ "$output" == *repository* ]]
}
