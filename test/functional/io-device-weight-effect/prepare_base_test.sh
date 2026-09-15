#!/usr/bin/env bash
set -Eeuo pipefail

script_dir=$(CDPATH='' cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
test_root=$(mktemp -d "${TMPDIR:-/tmp}/resman-iow-effect-base.XXXXXX")
base=$test_root/base.qcow2
prepared=$test_root/resman-iow-effect-prepared-test.qcow2
calls=$test_root/calls

cleanup_test() {
	local status=$?
	trap - EXIT
	find "$test_root" -depth -delete
	exit "$status"
}
trap cleanup_test EXIT

mkdir -p "$test_root/bin"
printf 'reviewed base\n' >"$base"
base_sha=$(sha256sum "$base" | awk '{print $1}')
cat >"$test_root/bin/qemu-img" <<'EOF'
#!/usr/bin/env bash
set -Eeuo pipefail
printf 'qemu-img %s\n' "$*" >>"$RESMAN_IO_EFFECT_TEST_CALLS"
if [[ $1 == create ]]; then
	printf 'prepared\n' >"${!#}"
fi
EOF
cat >"$test_root/bin/virt-customize" <<'EOF'
#!/usr/bin/env bash
set -Eeuo pipefail
printf 'virt-customize %s\n' "$*" >>"$RESMAN_IO_EFFECT_TEST_CALLS"
printf 'customized\n' >>"$3"
EOF
chmod 0755 "$test_root/bin/qemu-img" "$test_root/bin/virt-customize"

prepare() {
	PATH=$test_root/bin:/usr/bin:/bin \
	RESMAN_IO_EFFECT_BASE_SHA256=$base_sha \
	RESMAN_IO_EFFECT_KERNEL_RELEASE=1.2.3-test \
	RESMAN_IO_EFFECT_PREPARED_OWNER='' \
	RESMAN_IO_EFFECT_TEST_CALLS=$calls \
	"$script_dir/prepare-base.sh" "$base" "$prepared"
}

[[ $(prepare) == "$prepared" ]]
grep -q '^virt-customize ' "$calls"
first_calls=$(wc -l <"$calls")
[[ $(prepare) == "$prepared" ]]
[[ $(wc -l <"$calls") -eq $first_calls ]]
printf 'corrupt\n' >>"$prepared"
[[ $(prepare) == "$prepared" ]]
[[ $(wc -l <"$calls") -gt $first_calls ]]

printf 'PASS: weighted-I/O reusable RHCK base preparation\n'
