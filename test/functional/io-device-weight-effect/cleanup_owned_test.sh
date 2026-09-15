#!/usr/bin/env bash
set -Eeuo pipefail

script_dir=$(CDPATH='' cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
test_root=$(mktemp -d "${TMPDIR:-/tmp}/resman-iow-effect-cleanup.XXXXXX")
run_id=r20260915170000-12345
image_root=$test_root/images
work_dir=$image_root/resman-iow-effect-$run_id
state_file=$test_root/domain.state
calls_file=$test_root/virsh.calls

cleanup_test() {
	local status=$?
	trap - EXIT
	find "$test_root" -depth -delete
	exit "$status"
}
trap cleanup_test EXIT

mkdir -p "$test_root/bin" "$work_dir"
touch "$work_dir/root.qcow2" "$work_dir/effect-a.raw" "$work_dir/effect-b.raw" "$state_file"
cat >"$test_root/bin/virsh" <<'EOF'
#!/usr/bin/env bash
set -Eeuo pipefail
printf '%s\n' "$*" >>"$RESMAN_IO_EFFECT_TEST_CALLS"
case "$1" in
dominfo) [[ -e $RESMAN_IO_EFFECT_TEST_STATE ]] ;;
domstate) printf 'running\n' ;;
destroy) printf 'stopped\n' >"$RESMAN_IO_EFFECT_TEST_STATE" ;;
undefine) unlink "$RESMAN_IO_EFFECT_TEST_STATE" ;;
list) [[ ! -e $RESMAN_IO_EFFECT_TEST_STATE ]] || printf '%s\n' "resman-iow-effect-r20260915170000-12345" ;;
*) exit 2 ;;
esac
EOF
chmod 0755 "$test_root/bin/virsh"

PATH=$test_root/bin:/usr/bin:/bin \
RESMAN_IO_EFFECT_IMAGE_ROOT=$image_root \
RESMAN_IO_EFFECT_TEST_STATE=$state_file \
RESMAN_IO_EFFECT_TEST_CALLS=$calls_file \
"$script_dir/cleanup-owned.sh" "$run_id"

[[ ! -e $state_file && ! -e $work_dir ]]
grep -q '^destroy ' "$calls_file"
grep -q '^undefine ' "$calls_file"

printf 'PASS: weighted-I/O interrupted-run cleanup\n'
