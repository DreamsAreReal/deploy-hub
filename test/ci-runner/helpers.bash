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
[ "\${STUB_DEPTH:-0}" -lt 3 ] || exit 125
export STUB_DEPTH=\$((\${STUB_DEPTH:-0} + 1))
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
