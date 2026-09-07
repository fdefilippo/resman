#!/usr/bin/env bash
set -Eeuo pipefail

script_dir=$(CDPATH='' cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
repo_root=$(CDPATH='' cd -- "$script_dir/../../.." && pwd)
smolvm_bin=${SMOLVM_BIN:-smolvm}
sg_bin=${SG_BIN:-sg}
kvm_device=${KVM_DEVICE:-/dev/kvm}
cpus=${SMOLVM_CPUS:-2}
memory_mib=${SMOLVM_MEMORY_MIB:-2048}
storage_gib=${SMOLVM_STORAGE_GIB:-8}
overlay_gib=${SMOLVM_OVERLAY_GIB:-2}
require_psi=${SMOLVM_REQUIRE_PSI:-0}
scenario=${SMOLVM_SCENARIO:-resource-only}
if [[ $scenario == container-runtime && -z ${SMOLVM_OVERLAY_GIB+x} ]]; then
	# Nested Podman uses the vfs driver because SmolVM does not expose /dev/fuse.
	# Its full layer copies need more writable guest storage than other scenarios.
	overlay_gib=8
fi
evidence_root=${SMOLVM_EVIDENCE_ROOT:-$repo_root/build/functional/smolvm}
command_log=
evidence_dir=
run_id=
scratch_dir=
image_archive=
image_ref=
fixture_image_ref=
container_image_archive=
container_image_ref=
container_image_cache_ref=${RESMAN_CONTAINER_IMAGE_CACHE_REF:-localhost/resman-container-cache:latest}
vm_name=
vm_started=0
image_built=0
container_image_built=0

blocked() {
    echo "BLOCKED: $1" >&2
    exit 77
}

record_command() {
    [[ -n $command_log ]] || return 0
    printf '%q ' "$@" >>"$command_log"
    printf '\n' >>"$command_log"
}

run_kvm() {
    local command_string
    printf -v command_string '%q ' "$smolvm_bin" "$@"
    record_command "$sg_bin" kvm -c "$command_string"
    "$sg_bin" kvm -c "$command_string"
}

require_host_capabilities() {
    command -v "$smolvm_bin" >/dev/null 2>&1 || blocked "smolvm is not installed"
    command -v "$sg_bin" >/dev/null 2>&1 || blocked "sg is not installed"
    command -v sudo >/dev/null 2>&1 || blocked "sudo is not installed"
    command -v sha256sum >/dev/null 2>&1 || blocked "sha256sum is not installed"

    local probe
    printf -v probe 'test -c %q && test -r %q && test -w %q' \
        "$kvm_device" "$kvm_device" "$kvm_device"
    if ! "$sg_bin" kvm -c "$probe"; then
        blocked "$kvm_device is not a readable and writable KVM character device inside sg kvm"
    fi

    if ! sudo podman version >/dev/null 2>&1; then
        blocked "sudo podman is unavailable"
    fi

    "$smolvm_bin" --version
    echo "KVM access: sg kvm"
    echo "Container engine: sudo podman"
}

safe_remove_scratch() {
    [[ -n $scratch_dir ]] || return 0
    case "$scratch_dir" in
        "${TMPDIR:-/tmp}"/resman-smolvm.*)
            rm -rf -- "$scratch_dir"
            ;;
        *)
            echo "refusing to remove unexpected scratch path: $scratch_dir" >&2
            return 1
            ;;
    esac
}

cleanup() {
    local status=$?
    local cleanup_status=PASS
    trap - EXIT INT TERM
    set +e
    if ! cleanup_resources; then
        cleanup_status=FAIL
        status=1
    fi
    if [[ -n $evidence_dir ]]; then
        [[ $status -ne 0 || -f $evidence_dir/result ]] || status=1
        python3 "$script_dir/guest/evidence-metadata.py" finalize "$evidence_dir" "$cleanup_status" "$status" \
            || status=1
    fi
    exit "$status"
}

interrupt() {
    local signal_status=$1
    trap - INT TERM
    exit "$signal_status"
}

cleanup_resources() {
    local failed=0
    local cleanup_log=/dev/null
    local remove_output=
    [[ -n $evidence_dir ]] && cleanup_log=$evidence_dir/cleanup.log

    if [[ $vm_started -eq 1 ]]; then
        run_kvm_cleanup machine stop --name "$vm_name" || failed=1
        run_kvm_cleanup machine delete --force --name "$vm_name" || failed=1
        vm_started=0
    fi
    if [[ $image_built -eq 1 && -n $image_ref ]]; then
        record_command sudo podman image rm --force "$image_ref"
        if ! remove_output=$(sudo podman image rm --force "$image_ref" 2>&1); then
            failed=1
        fi
        [[ -z $remove_output ]] || printf '%s\n' "$remove_output" >>"$cleanup_log"
        image_built=0
    fi
	if [[ $container_image_built -eq 1 && -n $container_image_ref ]]; then
		record_command sudo podman image rm --force "$container_image_ref"
		if ! remove_output=$(sudo podman image rm --force "$container_image_ref" 2>&1); then
			failed=1
		fi
		[[ -z $remove_output ]] || printf '%s\n' "$remove_output" >>"$cleanup_log"
		container_image_built=0
	fi
    safe_remove_scratch || failed=1
    scratch_dir=
    return "$failed"
}

missing_machine_error() {
    # A launch can fail before SmolVM creates its database record. Only an explicit
    # absence diagnosis is an idempotent cleanup success; every other error remains fatal.
    local output=${1,,}
    [[ $output =~ machine.*not[[:space:]]+found \
        || $output =~ machine.*does[[:space:]]+not[[:space:]]+exist \
        || $output =~ no[[:space:]]+such[[:space:]]+machine \
        || $output =~ unknown[[:space:]]+machine ]]
}

run_kvm_cleanup() {
    local cleanup_log=/dev/null
    local output=
    [[ -n $evidence_dir ]] && cleanup_log=$evidence_dir/cleanup.log

    if output=$(run_kvm "$@" 2>&1); then
        [[ -z $output ]] || printf '%s\n' "$output" >>"$cleanup_log"
        return 0
    fi
    [[ -z $output ]] || printf '%s\n' "$output" >>"$cleanup_log"
    missing_machine_error "$output"
}

initialize_evidence() {
    # Evidence and traps precede host capability checks so BLOCKED and early FAIL
    # outcomes are durable even when no scratch directory or VM ever exists.
    run_id=r$(date -u +%Y%m%d%H%M%S)-$$
    evidence_dir=$evidence_root/$run_id
    mkdir -p "$evidence_dir"
    chmod 0700 "$evidence_dir"
    command_log=$evidence_dir/commands.log
    vm_name=resman-functional-$run_id
    trap cleanup EXIT
    trap 'interrupt 130' INT
    trap 'interrupt 143' TERM

    {
        printf 'started_at=%s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)"
        printf 'run_id=%s\n' "$run_id"
        printf 'requested_cpus=%s\n' "$cpus"
        printf 'requested_memory_mib=%s\n' "$memory_mib"
        printf 'requested_psi_required=%s\n' "$require_psi"
        printf 'requested_scenario=%s\n' "$scenario"
        printf 'source_revision=%s\n' "$(git -C "$repo_root" rev-parse HEAD)"
        printf 'network=disabled\n'
        printf 'host_ports=none\n'
    } >"$evidence_dir/environment.txt"
}

run_harness() {
    initialize_evidence
    require_host_capabilities

	[[ $require_psi == 0 || $require_psi == 1 ]] \
		|| blocked "SMOLVM_REQUIRE_PSI must be 0 or 1"
	[[ $scenario == resource-only || $scenario == memory-only || $scenario == process-membership \
		|| $scenario == cpu-without-cpuset || $scenario == missing-io-startup \
		|| $scenario == mcp-filter-reload || $scenario == container-runtime \
		|| $scenario == block-iops || $scenario == psi-refresh-neutrality \
		|| $scenario == limit-hook-executor || $scenario == host-cpu-sampling-cadence \
		|| $scenario == non-systemd-migration ]] \
		|| blocked "SMOLVM_SCENARIO must name a documented functional scenario"

    local smolvm_version base_image_id base_image_digest fixture_image_id
    local fixture_hash fixture_reused image_id container_image_id guest_status source_revision
    source_revision=$(git -C "$repo_root" rev-parse HEAD)
    [[ -z $(git -C "$repo_root" status --porcelain --untracked-files=no) ]] \
        || blocked "source tree must be committed before a revision-bound SmolVM build"
    scratch_dir=$(mktemp -d "${TMPDIR:-/tmp}/resman-smolvm.XXXXXX")
    image_archive=$scratch_dir/resman-functional.tar
    image_ref=localhost/resman-functional:$run_id
	container_image_archive=$scratch_dir/resman-container.tar
	container_image_ref=localhost/resman-container:$run_id
    fixture_hash=$(sha256sum "$script_dir/Containerfile.base")
    fixture_hash=${fixture_hash%% *}
    fixture_image_ref=localhost/resman-functional-base:${fixture_hash:0:16}

    smolvm_version=$($smolvm_bin --version)
    {
        printf 'smolvm_version=%s\n' "$smolvm_version"
        printf 'image_reference=%s\n' "$image_ref"
        printf 'fixture_image_reference=%s\n' "$fixture_image_ref"
    } >>"$evidence_dir/environment.txt"

    fixture_reused=true
    record_command sudo podman image exists "$fixture_image_ref"
    if ! sudo podman image exists "$fixture_image_ref"; then
        fixture_reused=false
        record_command sudo podman build --layers --file "$script_dir/Containerfile.base" \
            --tag "$fixture_image_ref" "$script_dir"
        sudo podman build --layers --file "$script_dir/Containerfile.base" \
            --tag "$fixture_image_ref" "$script_dir"
    fi
    record_command sudo podman build --layers --build-arg \
        "FUNCTIONAL_BASE_IMAGE=$fixture_image_ref" --file "$script_dir/Containerfile" \
        --tag "$image_ref" "$repo_root"
    sudo podman build --layers --build-arg "FUNCTIONAL_BASE_IMAGE=$fixture_image_ref" \
        --file "$script_dir/Containerfile" --tag "$image_ref" "$repo_root"
    image_built=1
	if [[ $scenario == container-runtime ]]; then
		record_command sudo podman build --layers --file "$repo_root/packaging/docker/Dockerfile" \
			--tag "$container_image_ref" "$repo_root"
		sudo podman build --layers --file "$repo_root/packaging/docker/Dockerfile" \
			--tag "$container_image_ref" "$repo_root"
		container_image_built=1
		record_command sudo podman tag "$container_image_ref" "$container_image_cache_ref"
		sudo podman tag "$container_image_ref" "$container_image_cache_ref"
		record_command sudo podman save --output "$container_image_archive" "$container_image_ref"
		sudo podman save --output "$container_image_archive" "$container_image_ref"
	fi
    record_command sudo podman image inspect --format '{{.Id}}' docker.io/amd64/oraclelinux:9
    base_image_id=$(sudo podman image inspect --format '{{.Id}}' docker.io/amd64/oraclelinux:9)
    record_command sudo podman image inspect --format '{{.Digest}}' docker.io/amd64/oraclelinux:9
    base_image_digest=$(sudo podman image inspect --format '{{.Digest}}' docker.io/amd64/oraclelinux:9)
    record_command sudo podman image inspect --format '{{.Id}}' "$fixture_image_ref"
    fixture_image_id=$(sudo podman image inspect --format '{{.Id}}' "$fixture_image_ref")
    record_command sudo podman image inspect --format '{{.Id}}' "$image_ref"
    image_id=$(sudo podman image inspect --format '{{.Id}}' "$image_ref")
	container_image_id=not-requested
	if [[ $scenario == container-runtime ]]; then
		record_command sudo podman image inspect --format '{{.Id}}' "$container_image_ref"
		container_image_id=$(sudo podman image inspect --format '{{.Id}}' "$container_image_ref")
	fi
    {
        printf 'base_image=%s\n' 'docker.io/amd64/oraclelinux:9'
        printf 'base_image_id=%s\n' "$base_image_id"
        printf 'base_image_digest=%s\n' "$base_image_digest"
        printf 'fixture_image_id=%s\n' "$fixture_image_id"
        printf 'fixture_image_reused=%s\n' "$fixture_reused"
        printf 'image_id=%s\n' "$image_id"
		printf 'container_image_reference=%s\n' "$container_image_ref"
		printf 'container_image_id=%s\n' "$container_image_id"
		printf 'container_image_cache_reference=%s\n' "$container_image_cache_ref"
    } >>"$evidence_dir/environment.txt"

    record_command sudo podman save --output "$image_archive" "$image_ref"
    sudo podman save --output "$image_archive" "$image_ref"

    vm_started=1
	machine_args=(machine run --detach --name "$vm_name" --cpus "$cpus" --mem "$memory_mib"
		--storage "$storage_gib" --overlay "$overlay_gib"
		--volume "$evidence_dir:/mnt/resman-artifacts")
	if [[ $scenario == container-runtime ]]; then
		machine_args+=(--volume "$scratch_dir:/mnt/resman-input")
	fi
	run_kvm "${machine_args[@]}" --image "$image_archive" -- /sbin/init

    run_kvm machine exec --name "$vm_name" --timeout 60s -- \
        /opt/resman-functional/wait-systemd.sh

    set +e
	run_kvm machine exec --stream --name "$vm_name" --timeout 4m -- \
		/opt/resman-functional/run-functional.sh "$run_id" "$cpus" "$memory_mib" "$require_psi" "$scenario" "$source_revision"
    guest_status=$?
    set -e

    if [[ $guest_status -eq 77 ]]; then
        echo "BLOCKED: guest capability preflight failed; evidence: $evidence_dir" >&2
        return 77
    fi
    if [[ $guest_status -ne 0 ]]; then
        echo "FAIL: functional guest failed; evidence: $evidence_dir" >&2
        return "$guest_status"
    fi
    if [[ $(< "$evidence_dir/result") != PASS ]]; then
        echo "FAIL: guest returned success without PASS evidence; evidence: $evidence_dir" >&2
        return 1
    fi

    if ! cleanup_resources; then
        python3 "$script_dir/guest/evidence-metadata.py" finalize "$evidence_dir" FAIL 1
        trap - EXIT INT TERM
        echo "FAIL: deterministic cleanup failed; evidence: $evidence_dir" >&2
        return 1
    fi
    python3 "$script_dir/guest/evidence-metadata.py" finalize "$evidence_dir" PASS 0
    trap - EXIT INT TERM

    echo "PASS: SmolVM functional harness"
    echo "Evidence: $evidence_dir"
}

main() {
    case ${1:-run} in
        run)
            run_harness
            ;;
        preflight)
            require_host_capabilities
            ;;
        *)
            echo "usage: $0 {run|preflight}" >&2
            return 2
            ;;
    esac
}

if [[ ${RESMAN_SMOLVM_LIBRARY_ONLY:-0} != 1 ]]; then
    main "$@"
fi
