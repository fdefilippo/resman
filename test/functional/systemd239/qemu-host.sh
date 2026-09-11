#!/usr/bin/env bash
set -Eeuo pipefail

run_id=${1:?run id is required}
qualification_revision=${2:?qualification revision is required}
package_source_revision=${3:?package source revision is required}
script_dir=$(CDPATH='' cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
evidence_dir=$script_dir/evidence
package=$script_dir/package.rpm
build_manifest=$script_dir/build-manifest.txt
base_image=${RESMAN_EL8_BASE_IMAGE:-/var/lib/libvirt/images/OL8U10-base.qcow2}
base_url=https://yum.oracle.com/templates/OracleLinux/OL8/u10/x86_64/OL8U10_x86_64-kvm-b287.qcow2
base_sha256=cf9eb243b7390311f1e2896e3e6849241e521e6592c8be9907956e3a6cee1f0c
vm_name=resman-systemd239-$run_id
work_dir=/var/lib/libvirt/images/$vm_name
overlay=$work_dir/root.qcow2
private_key=$script_dir/guest-key
known_hosts=$script_dir/known_hosts
address=
vm_defined=0
result=FAIL
detail="EL8 QEMU qualification did not complete"

[[ $run_id =~ ^r[0-9]{14}-[0-9]+$ ]] || { echo "unsafe run id: $run_id" >&2; exit 2; }
[[ $qualification_revision =~ ^[0-9a-f]{40}$ ]] \
	|| { echo "a full qualification revision is required" >&2; exit 2; }
[[ $package_source_revision =~ ^[0-9a-f]{40}$ ]] \
	|| { echo "a full package source revision is required" >&2; exit 2; }
[[ $work_dir == /var/lib/libvirt/images/resman-systemd239-* ]] \
	|| { echo "unsafe work directory: $work_dir" >&2; exit 2; }

mkdir -p "$evidence_dir"
chmod 0700 "$evidence_dir"

ssh_options=(-i "$private_key" -o BatchMode=yes -o StrictHostKeyChecking=yes \
	-o UserKnownHostsFile="$known_hosts" -o LogLevel=ERROR -o ConnectTimeout=5)

guest() {
	# The arguments form one deliberately caller-supplied command for the guest shell.
	# shellcheck disable=SC2029
	ssh "${ssh_options[@]}" root@"$address" "$@"
}

cleanup() {
	local status=$? cleanup_status=PASS
	trap - EXIT INT TERM
	set +e
	if [[ $vm_defined -eq 1 ]] && virsh dominfo "$vm_name" >/dev/null 2>&1; then
		virsh destroy "$vm_name" >>"$evidence_dir/cleanup.log" 2>&1 || true
		virsh undefine "$vm_name" --nvram >>"$evidence_dir/cleanup.log" 2>&1 \
			|| virsh undefine "$vm_name" >>"$evidence_dir/cleanup.log" 2>&1 \
			|| cleanup_status=FAIL
	fi
	if [[ -d $work_dir ]]; then
		rm -f -- "$overlay"
		rmdir -- "$work_dir" >>"$evidence_dir/cleanup.log" 2>&1 || cleanup_status=FAIL
	fi
	rm -f -- "$private_key" "$private_key.pub" "$known_hosts" "$known_hosts.tmp"
	if [[ $cleanup_status != PASS ]]; then
		status=1
		result=FAIL
		detail="QEMU resource cleanup failed"
	fi
	printf 'cleanup=%s\nresult=%s\ndetail=%s\nexit_code=%d\n' \
		"$cleanup_status" "$result" "$detail" "$status" >>"$evidence_dir/environment.txt"
	printf '%s\n' "$result" >"$evidence_dir/result"
	(
		cd "$evidence_dir"
		manifest_tmp=$script_dir/evidence-SHA256SUMS.tmp
		find . -type f ! -name SHA256SUMS -print0 | sort -z \
			| xargs -0 sha256sum -- >"$manifest_tmp"
		mv -- "$manifest_tmp" SHA256SUMS
	)
	exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

exec 9>/run/lock/resman-systemd239-qemu.lock
flock -n 9 || { echo "another systemd 239 QEMU qualification owns terra" >&2; exit 75; }

for command in qemu-img qemu-system-x86_64 virt-customize virt-install virsh ssh ssh-keygen ssh-keyscan scp rpm sha256sum; do
	command -v "$command" >/dev/null 2>&1 || { echo "missing host command: $command" >&2; exit 77; }
done
[[ -f $base_image && ! -L $base_image ]] || { echo "EL8 base image is unavailable" >&2; exit 77; }
[[ -f $package && ! -L $package ]] || { echo "exact EL8 RPM is unavailable" >&2; exit 77; }
[[ -f $build_manifest && ! -L $build_manifest ]] \
	|| { echo "EL8 build manifest is unavailable" >&2; exit 77; }
[[ $(sha256sum "$base_image" | awk '{print $1}') == "$base_sha256" ]] \
	|| { echo "EL8 base image does not match the reviewed Oracle digest" >&2; exit 1; }
package_identity=$(rpm -qp --qf '%{NAME}-%{VERSION}-%{RELEASE}.%{ARCH}' "$package")
[[ $package_identity == resman-1.36.6-1.el8.x86_64 ]] \
	|| { echo "unexpected package identity: $package_identity" >&2; exit 1; }
[[ $(awk -F= '$1 == "source_revision" {print $2}' "$build_manifest") == "$package_source_revision" ]] \
	|| { echo "build manifest revision differs from the requested revision" >&2; exit 1; }
[[ $(awk -F= '$1 == "package_sha256" {print $2}' "$build_manifest") == "$(sha256sum "$package" | awk '{print $1}')" ]] \
	|| { echo "build manifest does not identify the supplied RPM" >&2; exit 1; }
[[ ! -e $work_dir ]] || { echo "QEMU work directory already exists: $work_dir" >&2; exit 75; }
! virsh dominfo "$vm_name" >/dev/null 2>&1 \
	|| { echo "QEMU domain already exists: $vm_name" >&2; exit 75; }

{
	printf 'run_id=%s\nqualification_revision=%s\nsource_revision=%s\n' \
		"$run_id" "$qualification_revision" "$package_source_revision"
	printf 'base_image=%s\nbase_url=%s\nbase_sha256=%s\n' "$base_image" "$base_url" "$base_sha256"
	printf 'package_identity=%s\npackage_sha256=%s\n' "$package_identity" "$(sha256sum "$package" | awk '{print $1}')"
	printf 'build_manifest_sha256=%s\n' "$(sha256sum "$build_manifest" | awk '{print $1}')"
	printf 'qemu_version=%s\n' "$(qemu-system-x86_64 --version | head -n 1)"
	printf 'libvirt_version=%s\n' "$(virsh version --daemon 2>/dev/null | tr '\n' ' ')"
} >"$evidence_dir/environment.txt"
install -m 0600 "$build_manifest" "$evidence_dir/build-manifest.txt"

install -d -o qemu -g qemu -m 0710 "$work_dir"
qemu-img create -q -f qcow2 -F qcow2 -b "$base_image" "$overlay"
chown qemu:qemu "$overlay"
chmod 0600 "$overlay"
ssh-keygen -q -t ed25519 -N '' -f "$private_key"
virt-customize -q -a "$overlay" --ssh-inject "root:file:$private_key.pub" --selinux-relabel
vm_defined=1
virt-install --connect qemu:///system --name "$vm_name" --memory 3072 --vcpus 2 \
	--cpu host-passthrough --import --disk "path=$overlay,format=qcow2,bus=sata" \
	--network network=default,model=virtio --graphics none --noautoconsole \
	--os-variant ol8.9 --boot uefi

for _ in $(seq 1 120); do
	address=$(virsh domifaddr "$vm_name" --source lease 2>/dev/null \
		| awk '/ipv4/ {sub("/.*", "", $4); print $4; exit}')
	[[ -n $address ]] || { sleep 2; continue; }
	ssh-keyscan -T 2 -H "$address" >"$known_hosts.tmp" 2>/dev/null || { sleep 2; continue; }
	mv "$known_hosts.tmp" "$known_hosts"
	guest true 2>/dev/null && break
	sleep 2
done
if [[ -z $address ]] || ! guest true; then
	echo "EL8 guest SSH did not become ready" >&2
	exit 1
fi

mkdir -p "$evidence_dir/initial-boot" "$evidence_dir/bootloader-before" \
	"$evidence_dir/bootloader-after" "$evidence_dir/qualified-boot"
guest 'systemctl --version' >"$evidence_dir/initial-boot/systemd-version.txt"
guest 'cat /etc/os-release' >"$evidence_dir/initial-boot/os-release.txt"
guest 'uname -r' >"$evidence_dir/initial-boot/kernel.txt"
guest 'cat /proc/cmdline' >"$evidence_dir/initial-boot/cmdline.txt"
guest 'stat -fc %T /sys/fs/cgroup' >"$evidence_dir/initial-boot/cgroup-filesystem.txt"
guest 'test -d /sys/firmware/efi && printf "uefi\n" || printf "bios\n"' \
	>"$evidence_dir/initial-boot/firmware.txt"
initial_boot_id=$(guest 'cat /proc/sys/kernel/random/boot_id')
printf '%s\n' "$initial_boot_id" >"$evidence_dir/initial-boot/boot-id.txt"
guest 'dnf install -y cpio cronie python3 util-linux && command -v python3 >/dev/null && systemctl enable --now crond' \
	>"$evidence_dir/provision.log"

guest 'grubby --info=ALL' >"$evidence_dir/bootloader-before/grubby-info.txt"
guest 'grub2-editenv /boot/grub2/grubenv list' \
	>"$evidence_dir/bootloader-before/grubenv.txt"
guest 'ls -l /boot/grub2/grubenv' >"$evidence_dir/bootloader-before/grubenv-link.txt"
guest 'grep "^GRUB_ENABLE_BLSCFG=" /etc/default/grub' \
	>"$evidence_dir/bootloader-before/bls-config.txt"
guest 'grep -H -E "^(title|version|options)" /boot/loader/entries/*.conf' \
	>"$evidence_dir/bootloader-before/entries.txt"
guest 'grubby --update-kernel=ALL --args="systemd.unified_cgroup_hierarchy=1 psi=1"'
guest 'grubby --info=ALL' >"$evidence_dir/bootloader-after/grubby-info.txt"
guest 'grub2-editenv /boot/grub2/grubenv list' \
	>"$evidence_dir/bootloader-after/grubenv.txt"
guest 'ls -l /boot/grub2/grubenv' >"$evidence_dir/bootloader-after/grubenv-link.txt"
guest 'grep "^GRUB_ENABLE_BLSCFG=" /etc/default/grub' \
	>"$evidence_dir/bootloader-after/bls-config.txt"
guest 'grep -H -E "^(title|version|options)" /boot/loader/entries/*.conf' \
	>"$evidence_dir/bootloader-after/entries.txt"
for argument in systemd.unified_cgroup_hierarchy=1 psi=1; do
	if ! grep -qw "$argument" "$evidence_dir/bootloader-after/grubby-info.txt"; then
		echo "grubby did not publish the required kernel argument: $argument" >&2
		exit 1
	fi
	if ! grep -qw "$argument" "$evidence_dir/bootloader-after/grubenv.txt"; then
		echo "grubenv does not contain the required kernel argument: $argument" >&2
		exit 1
	fi
done
guest 'sync; systemctl reboot' >/dev/null 2>&1 || true

for _ in $(seq 1 150); do
	sleep 2
	boot_id=$(guest 'cat /proc/sys/kernel/random/boot_id' 2>/dev/null || true)
	[[ -n $boot_id && $boot_id != "$initial_boot_id" ]] && break
done
[[ -n ${boot_id:-} && $boot_id != "$initial_boot_id" ]] \
	|| { echo "EL8 guest did not complete the cgroup v2 reboot" >&2; exit 1; }

guest 'systemctl --version' >"$evidence_dir/qualified-boot/systemd-version.txt"
guest 'rpm -q systemd' >"$evidence_dir/qualified-boot/systemd-package.txt"
guest 'cat /etc/os-release' >"$evidence_dir/qualified-boot/os-release.txt"
guest 'uname -r' >"$evidence_dir/qualified-boot/kernel.txt"
guest 'cat /proc/cmdline' >"$evidence_dir/qualified-boot/cmdline.txt"
guest 'cat /proc/sys/kernel/random/boot_id' >"$evidence_dir/qualified-boot/boot-id.txt"
guest 'stat -fc %T /sys/fs/cgroup' >"$evidence_dir/qualified-boot/cgroup-filesystem.txt"
guest 'test -d /sys/firmware/efi && printf "uefi\n" || printf "bios\n"' \
	>"$evidence_dir/qualified-boot/firmware.txt"
guest 'cat /sys/fs/cgroup/cgroup.controllers' >"$evidence_dir/qualified-boot/controllers.txt"
guest 'find /proc/pressure -maxdepth 1 -type f -printf "%f\n" | sort' >"$evidence_dir/qualified-boot/psi-files.txt"
guest 'systemctl show user.slice -p ControlGroup -p ControlGroupId -p InvocationID' \
	>"$evidence_dir/qualified-boot/user-slice.txt"
guest 'busctl introspect --no-pager --no-legend org.freedesktop.systemd1 /org/freedesktop/systemd1/unit/user_2eslice org.freedesktop.systemd1.Slice' \
	>"$evidence_dir/qualified-boot/slice-interface.txt"
guest 'lsblk -o NAME,MAJ:MIN,TYPE,MOUNTPOINT' >"$evidence_dir/qualified-boot/block-devices.txt"
grep -q '^systemd 239 ' "$evidence_dir/qualified-boot/systemd-version.txt"
[[ $(<"$evidence_dir/qualified-boot/cgroup-filesystem.txt") == cgroup2fs ]]
grep -qw 'systemd.unified_cgroup_hierarchy=1' "$evidence_dir/qualified-boot/cmdline.txt"
grep -qw 'psi=1' "$evidence_dir/qualified-boot/cmdline.txt"
grep -Eq '^sda[[:space:]]+8:0[[:space:]]+disk' "$evidence_dir/qualified-boot/block-devices.txt"
if grep -q '^ControlGroupId=' "$evidence_dir/qualified-boot/user-slice.txt"; then
	echo "systemd show unexpectedly exposes ControlGroupId" >&2
	exit 1
fi
if grep -Eq '^[[:space:]]*ControlGroupId[[:space:]]' \
	"$evidence_dir/qualified-boot/slice-interface.txt"; then
	echo "systemd Slice unexpectedly exposes ControlGroupId" >&2
	exit 1
fi

# This program is evaluated by the guest shell, not expanded on terra.
# shellcheck disable=SC2016
guest 'for entry in "resman-t1:1006" "resman-t2:1007" "resman-t3:1008"; do name=${entry%:*}; uid=${entry#*:}; id "$name" >/dev/null 2>&1 || useradd --uid "$uid" --create-home "$name"; done'
guest 'install -d -m 0700 /root/resman-systemd239'
scp "${ssh_options[@]}" "$package" "$script_dir/native_gate.py" "$script_dir/native_package.py" \
	"$script_dir/native-workload.py" "$script_dir/el8_package.py" \
	root@"$address":/root/resman-systemd239/ >"$evidence_dir/transfer.log" 2>&1
guest 'dnf install -y /root/resman-systemd239/package.rpm' >"$evidence_dir/package-install.log"
set +e
guest "python3 /root/resman-systemd239/el8_package.py '$run_id' '$package_source_revision'" \
	>"$evidence_dir/guest-run.log" 2>&1
guest_status=$?
set -e
mkdir -p "$evidence_dir/guest"
if guest 'test -d /root/resman-systemd239/evidence'; then
	guest 'tar -C /root/resman-systemd239 -cf - evidence' \
		| tar --no-same-owner -C "$evidence_dir/guest" --strip-components=1 -xf -
else
	printf 'guest_evidence=absent\nguest_exit_code=%d\n' "$guest_status" \
		>"$evidence_dir/guest-evidence-status.txt"
	result=FAIL
	detail="guest package gate exited before producing evidence"
	[[ $guest_status -ne 0 ]] || guest_status=1
	exit "$guest_status"
fi

if [[ $guest_status -ne 0 ]]; then
	result=FAIL
	detail="guest package gate failed after producing evidence"
	exit "$guest_status"
fi
[[ $(<"$evidence_dir/guest/result") == PASS ]]
# This program is evaluated by the guest shell, not expanded on terra.
# shellcheck disable=SC2016
guest 'test ! -e /var/lib/resman/systemd-property-leases.json && test -z "$(find /run/systemd/system.control -mindepth 1 -print -quit)" && test "$(systemctl show resman -p ActiveState --value)" = inactive && { test ! -e /sys/fs/cgroup/user.slice/cpu.max || test "$(awk '\''{print $1}'\'' /sys/fs/cgroup/user.slice/cpu.max)" = max; } && test "$(systemctl show user.slice -p CPUQuotaPerSecUSec)" = CPUQuotaPerSecUSec=infinity'
guest 'systemctl show resman -p ActiveState -p SubState -p Result; if test -e /sys/fs/cgroup/user.slice/cpu.max; then cat /sys/fs/cgroup/user.slice/cpu.max; else printf "cpu.max=unavailable\n"; fi; systemctl show user.slice -p CPUQuotaPerSecUSec; find /run/systemd/system.control -mindepth 1 -maxdepth 3 -print; test ! -e /var/lib/resman/systemd-property-leases.json' \
	>"$evidence_dir/post-run.txt"

result=PASS
detail="exact EL8 RPM passed systemd 239 native lifecycle qualification"
