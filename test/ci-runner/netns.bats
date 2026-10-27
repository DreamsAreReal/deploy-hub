load helpers

setup() {
  setup_world "${BATS_TEST_DIRNAME}" "${BATS_TEST_TMPDIR}"
  NETNS=$(patched ci-netns -e "s#/run/netns/#${WORLD}/netns/#")
  mkdir -p "${WORLD}/netns" "${WORLD}/rules"
  stub id '[ "$2" = ci-tasks ] && echo "${UID_OF_TASKS:-1001}"'
  stub ip '
case "$*" in
"-4 route get 1.1.1.1") echo "1.1.1.1 via 203.0.113.1 dev eth0 src 203.0.113.10 uid 0" ;;
"-br link show "*) cat "${WORLD}/link-$4" 2>/dev/null ;;
"-n "*" -br link show "*) cat "${WORLD}/link-$6" 2>/dev/null ;;
"link show "*) [ -e "${WORLD}/link-$3" ] ;;
esac'
  stub nft '[ "$1" = -f ] && cat >"${WORLD}/ruleset"; [ "$1" != list ] || [ -e "${WORLD}/table" ]'
  stub sysctl
  stub iptables '[ "$3" != -C ] || [ -e "${WORLD}/rules/$4" ]'
  export PATH="${STUBS}:${PATH}"
  unset UID_OF_TASKS
}

ruleset() {
  cat <<'NFT'
table inet ci_netns
delete table inet ci_netns
table inet ci_netns {
  chain forward {
    type filter hook forward priority -10; policy accept;
    oifname "cih*" ct state established,related accept
    oifname "cih*" drop
    iifname "cih*" meta nfproto ipv6 drop
    iifname "cih*" ip daddr { 10.0.0.0/8, 172.16.0.0/12, 192.168.0.0/16, 100.64.0.0/10, 169.254.0.0/16, 127.0.0.0/8 } drop
    iifname "cih*" oifname != "eth0" drop
  }
  chain input {
    type filter hook input priority -10; policy accept;
    iifname != "cih*" accept
    ct state established,related accept
    ip daddr 203.0.113.10 tcp dport { 22, 80, 443 } accept
    ip protocol icmp accept
    drop
  }
}
NFT
}

guard_calls() {
  cat <<'CALLS'
ip -4 route get 1.1.1.1
ip -4 route get 1.1.1.1
nft -f -
sysctl -qw net.ipv4.ip_forward=1
iptables -t filter -C FORWARD -i cih+ -o eth0 -j ACCEPT
iptables -t filter -A FORWARD -i cih+ -o eth0 -j ACCEPT
iptables -t filter -C FORWARD -o cih+ -m conntrack --ctstate ESTABLISHED,RELATED -j ACCEPT
iptables -t filter -A FORWARD -o cih+ -m conntrack --ctstate ESTABLISHED,RELATED -j ACCEPT
iptables -t nat -C POSTROUTING -s 10.79.0.0/16 -o eth0 -j MASQUERADE
iptables -t nat -A POSTROUTING -s 10.79.0.0/16 -o eth0 -j MASQUERADE
CALLS
}

@test "up gives the repository its own namespace, veth pair and /30" {
  touch "${WORLD}/link-cih1001" "${WORLD}/netns/ci-tasks"
  run bash "$NETNS" up tasks
  [ "$status" -eq 0 ]
  [ "$(calls)" = "$(cat <<CALLS
id -u ci-tasks
id -u ci-tasks
ip link show cih1001
ip link del cih1001
ip netns del ci-tasks
$(guard_calls)
ip netns add ci-tasks
ip -n ci-tasks link set lo up
ip link add cih1001 type veth peer name cin1001
ip link set cin1001 netns ci-tasks
ip addr add 10.79.15.165/30 dev cih1001
ip link set cih1001 up
ip -n ci-tasks addr add 10.79.15.166/30 dev cin1001
ip -n ci-tasks link set cin1001 up
ip -n ci-tasks route add default via 10.79.15.165
ip netns exec ci-tasks sysctl -qw net.ipv6.conf.all.disable_ipv6=1
CALLS
)" ]
  [ "$(cat "${WORLD}/ruleset")" = "$(ruleset)" ]
}

@test "the guard adds each iptables rule only once" {
  touch "${WORLD}/rules/FORWARD" "${WORLD}/rules/POSTROUTING"
  run bash "$NETNS" guard
  [ "$status" -eq 0 ]
  [ -z "$(grep -- ' -A ' "$CALLS")" ]
  [ "$(grep -c -- ' -C ' "$CALLS")" -eq 3 ]
  [ "$(cat "${WORLD}/ruleset")" = "$(ruleset)" ]
}

@test "down removes the veth pair and the namespace that are there" {
  run bash "$NETNS" down tasks
  [ "$status" -eq 0 ]
  [ "$(calls)" = "$(printf 'id -u ci-tasks\nip link show cih1001')" ]
  touch "${WORLD}/link-cih1001"
  : >"$CALLS"
  run bash "$NETNS" down tasks
  [ "$(calls)" = "$(printf 'id -u ci-tasks\nip link show cih1001\nip link del cih1001')" ]
  rm "${WORLD}/link-cih1001"
  touch "${WORLD}/netns/ci-tasks"
  : >"$CALLS"
  run bash "$NETNS" down tasks
  [ "$(calls)" = "$(printf 'id -u ci-tasks\nip link show cih1001\nip netns del ci-tasks')" ]
}

@test "the uid of the repository user must fit the address pool" {
  UID_OF_TASKS=16383 run bash "$NETNS" up tasks
  [ "$status" -eq 0 ]
  grep -qx 'ip -n ci-tasks addr add 10.79.255.254/30 dev cin16383' "$CALLS"
  grep -qx 'ip addr add 10.79.255.253/30 dev cih16383' "$CALLS"
  for bad in 16384 0; do
    UID_OF_TASKS=$bad run bash "$NETNS" down tasks
    [ "$status" -eq 1 ]
    [ "$output" = "uid $bad of ci-tasks does not fit a /30 of 10.79.0.0/16" ]
  done
  UID_OF_TASKS=1 run bash "$NETNS" down tasks
  [ "$status" -eq 0 ]
  grep -qx 'ip link show cih1' "$CALLS"
}

@test "an unknown repository user stops every command" {
  for cmd in up down check; do
    : >"$CALLS"
    run bash "$NETNS" "$cmd" router-bot
    [ "$status" -eq 1 ]
    [ "$(calls)" = "id -u ci-router-bot" ]
  done
}

@test "check needs the namespace, both veth ends up and the nftables table" {
  touch "${WORLD}/netns/ci-tasks" "${WORLD}/table"
  echo 'cih1001@if2      UP             10.79.15.165/30 ' >"${WORLD}/link-cih1001"
  echo 'cin1001@if3      UP             10.79.15.166/30 ' >"${WORLD}/link-cin1001"
  run bash "$NETNS" check tasks
  [ "$status" -eq 0 ]
  grep -qx 'ip -n ci-tasks -br link show cin1001' "$CALLS"
  rm "${WORLD}/table"
  run bash "$NETNS" check tasks
  [ "$status" -eq 1 ]
  touch "${WORLD}/table"
  echo 'cin1001@if3      DOWN           ' >"${WORLD}/link-cin1001"
  run bash "$NETNS" check tasks
  [ "$status" -eq 1 ]
  echo 'cin1001@if3      UP             ' >"${WORLD}/link-cin1001"
  echo 'cih1001@if2      DOWN           ' >"${WORLD}/link-cih1001"
  run bash "$NETNS" check tasks
  [ "$status" -eq 1 ]
  echo 'cih1001@if2      UP             ' >"${WORLD}/link-cih1001"
  rm "${WORLD}/netns/ci-tasks"
  run bash "$NETNS" check tasks
  [ "$status" -eq 1 ]
}

@test "a wrong command or a missing repository prints the usage" {
  for args in "" "start tasks"; do
    run bash "$NETNS" $args
    [ "$status" -eq 2 ]
    [ "$output" = "usage: $NETNS up|down|check <repo> | guard" ]
  done
  run bash "$NETNS" up
  [ "$status" -ne 0 ]
  [[ "$output" == *"repo"* ]]
  [ -z "$(calls)" ]
}
