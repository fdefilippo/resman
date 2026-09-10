#!/usr/bin/env bash
set -Eeuo pipefail

output_dir=${1:?usage: collect.sh OUTPUT_DIRECTORY}
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

unit_id=$(systemctl show user.slice -p Id --value)
[[ $unit_id == user.slice ]] || {
	echo "user.slice is unavailable" >&2
	exit 1
}
[[ $(systemctl show user.slice -p ActiveState --value) == active ]] || {
	echo "user.slice must already be active; this read-only collector will not start it" >&2
	exit 1
}

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

(
	cd "$output_dir"
	sha256sum -- ./*.txt >SHA256SUMS
)
