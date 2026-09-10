#!/usr/bin/env bash
set -Eeuo pipefail

output_dir=${1:?usage: collect.sh OUTPUT_DIRECTORY}
capture_image=${RESMAN_CAPTURE_IMAGE:?RESMAN_CAPTURE_IMAGE is required}
capture_image_id=${RESMAN_CAPTURE_IMAGE_ID:?RESMAN_CAPTURE_IMAGE_ID is required}
capture_image_digest=${RESMAN_CAPTURE_IMAGE_DIGEST:?RESMAN_CAPTURE_IMAGE_DIGEST is required}
capture_source_revision=${RESMAN_CAPTURE_SOURCE_REVISION:?RESMAN_CAPTURE_SOURCE_REVISION is required}
capture_environment=${RESMAN_CAPTURE_ENVIRONMENT:?RESMAN_CAPTURE_ENVIRONMENT is required}
captured_on=$(date -u +%F)
[[ -d $output_dir ]] || {
	echo "output directory does not exist: $output_dir" >&2
	exit 2
}
[[ -w $output_dir ]] || {
	echo "output directory is not writable: $output_dir" >&2
	exit 2
}
[[ -z $(find "$output_dir" -mindepth 1 -maxdepth 1 -print -quit) ]] || {
	echo "output directory must be empty: $output_dir" >&2
	exit 2
}
[[ $capture_image_id =~ ^sha256:[[:xdigit:]]{64}$ ]] || {
	echo "RESMAN_CAPTURE_IMAGE_ID must be a complete sha256 digest" >&2
	exit 2
}
[[ $capture_image_digest =~ ^sha256:[[:xdigit:]]{64}$ ]] || {
	echo "RESMAN_CAPTURE_IMAGE_DIGEST must be a complete sha256 digest" >&2
	exit 2
}
[[ $capture_source_revision =~ ^[[:xdigit:]]{40}$ ]] || {
	echo "RESMAN_CAPTURE_SOURCE_REVISION must be a full Git revision" >&2
	exit 2
}

unit_id=$(systemctl show user.slice -p Id --value)
[[ $unit_id == user.slice ]] || {
	echo "user.slice is unavailable" >&2
	exit 1
}
[[ $(systemctl show user.slice -p ActiveState --value) == active ]] || {
	echo "user.slice must already be active; this read-only collector will not start it" >&2
	exit 1
}

{
	printf 'capture_schema=1\n'
	printf 'capture_kind=dbus_introspection\n'
	printf 'source_revision=%s\n' "$capture_source_revision"
	printf 'captured_on=%s\n' "$captured_on"
	printf 'environment=%s\n' "$capture_environment"
	printf 'image=%s\n' "$capture_image"
	printf 'image_id=%s\n' "$capture_image_id"
	printf 'image_digest=%s\n' "$capture_image_digest"
} >"$output_dir/environment.txt"

systemctl --version >"$output_dir/systemd-version.txt"
rpm -q systemd >"$output_dir/systemd-package.txt"
cat /etc/os-release >"$output_dir/os-release.txt"
uname -r >"$output_dir/kernel.txt"
stat -fc 'type=%T' /sys/fs/cgroup >"$output_dir/cgroup-filesystem.txt"
cat /sys/fs/cgroup/cgroup.controllers >"$output_dir/cgroup-controllers.txt"
stat -Lc 'dev=%d ino=%i' /sys/fs/cgroup/user.slice >"$output_dir/user-slice-identity.txt"

systemctl show user.slice --all \
	-p Id \
	-p ActiveState \
	-p InvocationID \
	-p ControlGroup \
	-p ControlGroupId \
	-p CPUWeight \
	-p CPUQuotaPerSecUSec \
	-p CPUQuotaPeriodUSec \
	-p MemoryHigh \
	-p MemoryMax \
	-p MemorySwapMax \
	-p IOWeight \
	-p IOReadBandwidthMax \
	-p IOWriteBandwidthMax \
	-p IOReadIOPSMax \
	-p IOWriteIOPSMax \
	-p FragmentPath \
	-p DropInPaths >"$output_dir/user-slice-values.txt"

busctl introspect --no-pager --no-legend \
	org.freedesktop.systemd1 \
	/org/freedesktop/systemd1/unit/user_2eslice \
	org.freedesktop.systemd1.Unit >"$output_dir/unit-interface.txt"
busctl introspect --no-pager --no-legend \
	org.freedesktop.systemd1 \
	/org/freedesktop/systemd1/unit/user_2eslice \
	org.freedesktop.systemd1.Slice >"$output_dir/slice-interface.txt"
busctl introspect --no-pager --no-legend \
	org.freedesktop.systemd1 \
	/org/freedesktop/systemd1 \
	org.freedesktop.systemd1.Manager >"$output_dir/manager-interface.txt"

printf 'PASS\n' >"$output_dir/result.txt"
(
	cd "$output_dir"
	sha256sum -- ./*.txt >SHA256SUMS
)
