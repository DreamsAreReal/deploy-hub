setup_world() {
	CI_RUNNER_DIR=$1/../../server/ci-runner
	WORLD=$2/world
	STUBS=${WORLD}/stubs
	CALLS=${WORLD}/calls
	mkdir -p "${STUBS}"
	: >"${CALLS}"
	export WORLD STUBS CALLS
}

stub() {
	local name=$1 body=${2:-}
	cat >"${STUBS}/${name}" <<STUB
#!/bin/bash
printf '%s\n' "${name} \$*" >>"\${CALLS}"
${body}
STUB
	chmod +x "${STUBS}/${name}"
}

patched() {
	local name=$1 copy=${WORLD}/tree/server/ci-runner/$1
	shift
	mkdir -p "${copy%/*}"
	sed "$@" "${CI_RUNNER_DIR}/${name}" >"${copy}"
	chmod +x "${copy}"
	printf '%s' "${copy}"
}

calls() {
	cat "${CALLS}"
}
