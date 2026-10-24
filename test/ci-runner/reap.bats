load helpers

setup() {
  setup_world "${BATS_TEST_DIRNAME}" "${BATS_TEST_TMPDIR}"
  REAP=$(patched ci-runner-reap -e "s#^PATH=.*#PATH=$STUBS:/usr/bin:/bin#" -e "s#/proc/#$WORLD/proc/#g" -e "s#/sys/fs/cgroup#$WORLD/cg#g" -e 's#\$\$#100#g')
  stub kill "$(declare -f kill_fake)
kill_fake \"\$@\""
}

kill_fake() {
  local signal=$1 pid
  shift
  for pid in "$@"; do
    if [ "$signal" = -KILL ]; then
      sed -i "/^$pid\$/d" "$WORLD"/cg/*/cgroup.procs
    else
      sed -i 's/) [A-Z] /) T /' "$WORLD/proc/$pid/stat"
    fi
  done
}

proc() {
  mkdir -p "$WORLD/proc/$1" "$WORLD/cg$4"
  printf '%s (%s) x) %s %s 0 0\n' "$1" "$5" "$3" "$2" >"$WORLD/proc/$1/stat"
  printf '0::%s\n' "$4" >"$WORLD/proc/$1/cgroup"
  printf '%s\n' "$1" >>"$WORLD/cg$4/cgroup.procs"
}

job_tree() {
  proc 1 0 S /init.scope systemd
  proc 10 1 S /ci.slice runsvc
  proc 11 10 S /ci.slice listener
  proc 12 11 R /ci.slice worker
  proc 100 12 R /ci.slice hook
}

@test "a job tree without strays is left alone" {
  job_tree
  run bash "$REAP"
  [ "$status" -eq 0 ]
  [ -z "$output" ]
  [ -z "$(calls)" ]
}

@test "strays are stopped first and killed once all of them stand still" {
  job_tree
  proc 50 1 S /ci.slice daemon
  proc 51 50 R /ci.slice child
  proc 52 1 Z /ci.slice zombie
  proc 60 1 S /other.slice neighbour
  run bash "$REAP"
  [ "$status" -eq 0 ]
  [ "$output" = "ci-runner-reap: killing 2 leftover processes in /ci.slice" ]
  [ "$(sed -n 1p "$CALLS" | tr ' ' '\n' | sort | tr '\n' ' ')" = "-STOP 50 51 kill " ]
  [ "$(sed -n 2p "$CALLS" | tr ' ' '\n' | sort | tr '\n' ' ')" = "-KILL 50 51 kill " ]
  [ "$(wc -l <"$CALLS")" -eq 2 ]
  grep -qx 60 "$WORLD/cg/other.slice/cgroup.procs"
  grep -qx 52 "$WORLD/cg/ci.slice/cgroup.procs"
}

@test "strays that are already stopped or traced are killed at once" {
  job_tree
  proc 50 1 T /ci.slice stopped
  proc 51 1 t /ci.slice traced
  run bash "$REAP"
  [ "$status" -eq 0 ]
  [ "$(sed -n 1p "$CALLS" | tr ' ' '\n' | sort | tr '\n' ' ')" = "-KILL 50 51 kill " ]
  [ "$(wc -l <"$CALLS")" -eq 1 ]
}

@test "a stray that survives every round fails the reap" {
  job_tree
  proc 50 1 T /ci.slice immortal
  stub kill
  run bash "$REAP"
  [ "$status" -eq 1 ]
  [ "$(grep -c 'killing 1 leftover processes in /ci.slice' <<<"$output")" -eq 50 ]
  [ "${lines[-1]}" = "ci-runner-reap: processes still left in /ci.slice after 50 rounds" ]
  [ "$(grep -c '^kill -KILL 50$' "$CALLS")" -eq 50 ]
}

@test "the job tree stops at the first parent outside the cgroup" {
  proc 1 0 S /init.scope systemd
  proc 5 1 S /system.slice manager
  proc 10 5 S /ci.slice runsvc
  proc 100 10 R /ci.slice hook
  proc 70 5 T /ci.slice sibling
  run bash "$REAP"
  [ "$status" -eq 0 ]
  [ "$(calls)" = "kill -KILL 70" ]
}

@test "without a cgroup v2 there is nothing to reap" {
  run bash "$REAP"
  [ "$status" -eq 0 ]
  [ "$output" = "ci-runner-reap: no cgroup v2 to reap" ]
  proc 100 1 R /gone.slice hook
  rm "$WORLD/cg/gone.slice/cgroup.procs"
  run bash "$REAP"
  [ "$status" -eq 0 ]
  [ "$output" = "ci-runner-reap: no cgroup v2 to reap" ]
  [ -z "$(calls)" ]
}
