#!/usr/bin/env bash
# Reproduce the EL8 cpu-controller delegation refusal on one disposable guest.
set -Eeuo pipefail

run_id=${1:?run id is required}
qualification_revision=${2:?qualification revision is required}
package_source_revision=${3:?package source revision is required}
script_dir=$(CDPATH='' cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
evidence_dir=$script_dir/evidence
package=$script_dir/package.rpm
build_manifest=$script_dir/build-manifest.txt
base_image=${RESMAN_CPUDEL_BASE_IMAGE:-/var/lib/libvirt/images/OL8U10-base.qcow2}
base_url=https://yum.oracle.com/templates/OracleLinux/OL8/u10/x86_64/OL8U10_x86_64-kvm-b287.qcow2
base_sha256=cf9eb243b7390311f1e2896e3e6849241e521e6592c8be9907956e3a6cee1f0c
package_identity_expected=resman-1.38.0-10.el8.x86_64
fixture_unit=resman-cpudel-fixture
vm_name=resman-cpudel-$run_id
work_dir=/var/lib/libvirt/images/$vm_name
overlay=$work_dir/root.qcow2
private_key=$work_dir/guest-key
known_hosts=$work_dir/known-hosts
address=
vm_defined=0
guest_status=not-run
result=FAIL
detail="cpu delegation reproduction did not complete"

[[ $run_id =~ ^r[0-9]{14}-[0-9]+$ ]] || { echo "unsafe run id: $run_id" >&2; exit 2; }
[[ $qualification_revision =~ ^[0-9a-f]{40}$ ]] \
	|| { echo "a full qualification revision is required" >&2; exit 2; }
[[ $package_source_revision =~ ^[0-9a-f]{40}$ ]] \
	|| { echo "a full package source revision is required" >&2; exit 2; }
[[ $work_dir =~ ^/var/lib/libvirt/images/resman-cpudel-r[0-9]{14}-[0-9]+$ ]] \
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

wait_for_guest() {
	local candidate stable=0
	for _ in $(seq 1 150); do
		candidate=$(virsh domifaddr "$vm_name" --source lease 2>/dev/null \
			| awk '/ipv4/ {sub("/.*", "", $4); print $4; exit}')
		if [[ -z $candidate ]]; then
			stable=0
			sleep 2
			continue
		fi
		if ! ssh-keyscan -T 2 -H "$candidate" >"$known_hosts.tmp" 2>/dev/null; then
			stable=0
			sleep 2
			continue
		fi
		mv "$known_hosts.tmp" "$known_hosts"
		address=$candidate
		if guest true 2>/dev/null; then
			stable=$((stable + 1))
			[[ $stable -ge 3 ]] && return 0
		else
			stable=0
		fi
		sleep 2
	done
	return 1
}

# Invoked by the EXIT/INT/TERM traps below.
# shellcheck disable=SC2329
cleanup() {
	local status=$? cleanup_status=PASS
	trap - EXIT INT TERM
	set +e
	if [[ $status -eq 75 || $status -eq 77 ]] && [[ $result == FAIL ]]; then
		result=BLOCKED
		detail="QEMU infrastructure or guest evidence was unavailable"
	fi
	if [[ $vm_defined -eq 1 ]] && virsh dominfo "$vm_name" >/dev/null 2>&1; then
		virsh destroy "$vm_name" >>"$evidence_dir/cleanup.log" 2>&1 || true
		virsh undefine "$vm_name" --nvram >>"$evidence_dir/cleanup.log" 2>&1 \
			|| virsh undefine "$vm_name" >>"$evidence_dir/cleanup.log" 2>&1 \
			|| cleanup_status=FAIL
	fi
	if [[ -d $work_dir ]]; then
		rm -f -- "$overlay" "$private_key" "$private_key.pub" "$known_hosts" "$known_hosts.tmp"
		rmdir -- "$work_dir" >>"$evidence_dir/cleanup.log" 2>&1 || cleanup_status=FAIL
	fi
	if [[ $cleanup_status != PASS ]]; then
		status=1
		result=FAIL
		detail="QEMU resource cleanup failed"
	fi
	printf 'guest_exit_code=%s\ncleanup=%s\nresult=%s\ndetail=%s\nexit_code=%d\n' \
		"$guest_status" "$cleanup_status" "$result" "$detail" "$status" \
		>>"$evidence_dir/environment.txt"
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

exec 9>/run/lock/resman-cpudel-qemu.lock
flock -n 9 || { echo "another cpu delegation reproduction owns this host" >&2; exit 75; }

for command in curl qemu-img qemu-system-x86_64 virt-customize virt-install virsh \
	ssh ssh-keygen ssh-keyscan scp rpm sha256sum; do
	command -v "$command" >/dev/null 2>&1 || { echo "missing host command: $command" >&2; exit 77; }
done
[[ -f $package && ! -L $package ]] || { echo "the EL8 package is unavailable" >&2; exit 77; }
[[ -f $build_manifest && ! -L $build_manifest ]] \
	|| { echo "the EL8 build manifest is unavailable" >&2; exit 77; }
if [[ ! -e $base_image ]]; then
	partial=$base_image.download-$run_id
	[[ ! -e $partial ]] || { echo "base-image download collision: $partial" >&2; exit 75; }
	curl -fL --retry 3 --output "$partial" "$base_url"
	printf '%s  %s\n' "$base_sha256" "$partial" | sha256sum --check -
	chown qemu:qemu "$partial"
	chmod 0644 "$partial"
	mv "$partial" "$base_image"
fi
[[ -f $base_image && ! -L $base_image ]] || { echo "EL8 base image is unavailable" >&2; exit 77; }
[[ $(sha256sum "$base_image" | awk '{print $1}') == "$base_sha256" ]] \
	|| { echo "EL8 base image does not match the reviewed Oracle digest" >&2; exit 1; }
package_identity=$(rpm -qp --qf '%{NAME}-%{VERSION}-%{RELEASE}.%{ARCH}' "$package")
[[ $package_identity == "$package_identity_expected" ]] \
	|| { echo "unexpected package identity: $package_identity" >&2; exit 1; }
package_sha256=$(sha256sum "$package" | awk '{print $1}')
[[ $(awk -F= '$1 == "source_revision" {print $2}' "$build_manifest") == "$package_source_revision" ]] \
	|| { echo "build manifest revision differs from the requested revision" >&2; exit 1; }
[[ $(awk -F= '$1 == "package_sha256" {print $2}' "$build_manifest") == "$package_sha256" ]] \
	|| { echo "build manifest does not identify the supplied package" >&2; exit 1; }
[[ ! -e $work_dir ]] || { echo "QEMU work directory already exists: $work_dir" >&2; exit 75; }
! virsh dominfo "$vm_name" >/dev/null 2>&1 \
	|| { echo "QEMU domain already exists: $vm_name" >&2; exit 75; }

{
	printf 'run_id=%s\nqualification_revision=%s\nsource_revision=%s\n' \
		"$run_id" "$qualification_revision" "$package_source_revision"
	printf 'base_image=%s\nbase_url=%s\nbase_sha256=%s\n' "$base_image" "$base_url" "$base_sha256"
	printf 'kernel_family=rhck\nfixture_unit=%s\n' "$fixture_unit"
	printf 'package_identity=%s\npackage_sha256=%s\n' "$package_identity" "$package_sha256"
	printf 'build_manifest_sha256=%s\n' "$(sha256sum "$build_manifest" | awk '{print $1}')"
	printf 'qemu_version=%s\n' "$(qemu-system-x86_64 --version | head -n 1)"
	printf 'libvirt_version=%s\n' "$(virsh version --daemon 2>/dev/null | tr '\n' ' ')"
} >"$evidence_dir/environment.txt"
install -m 0600 "$build_manifest" "$evidence_dir/build-manifest.txt"
install -m 0600 "$script_dir/guest_probe.py" "$evidence_dir/guest-probe.py"

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
	--os-variant ol8.10 --boot uefi

wait_for_guest || { echo "EL8 guest SSH did not become ready" >&2; exit 77; }
mkdir -p "$evidence_dir/initial-boot" "$evidence_dir/qualified-boot"
guest 'systemctl --version' >"$evidence_dir/initial-boot/systemd-version.txt"
guest 'cat /etc/os-release' >"$evidence_dir/initial-boot/os-release.txt"
guest 'uname -r' >"$evidence_dir/initial-boot/kernel.txt"
guest 'cat /proc/cmdline' >"$evidence_dir/initial-boot/cmdline.txt"
guest 'cat /proc/sys/kernel/random/boot_id' >"$evidence_dir/initial-boot/boot-id.txt"
guest 'stat -fc %T /sys/fs/cgroup' >"$evidence_dir/initial-boot/cgroup-filesystem.txt"
guest 'rpm -q systemd kernel-core kernel-uek-core 2>&1 || true' \
	>"$evidence_dir/initial-boot/packages.txt"
guest 'dnf install -y python3 util-linux procps-ng' >"$evidence_dir/provision.log"
initial_boot_id=$(<"$evidence_dir/initial-boot/boot-id.txt")

# The field nodes run the distribution kernel, not UEK, and only that kernel
# family carries the CONFIG_RT_GROUP_SCHED build this reproduction depends on.
guest 'grubby --info=ALL' >"$evidence_dir/grubby-before.txt"
guest 'dnf install -y --setopt=install_weak_deps=False kernel' >"$evidence_dir/rhck-install.log"
# Expand the package and boot-image expressions inside the guest shell.
# shellcheck disable=SC2016
guest 'set -eu; release=$(rpm -q --qf "%{VERSION}-%{RELEASE}.%{ARCH}\n" kernel-core | sort -V | tail -n 1); test -n "$release"; image=/boot/vmlinuz-$release; test -f "$image"; grubby --set-default "$image"; grubby --update-kernel="$image" --args="systemd.unified_cgroup_hierarchy=1 psi=1"; printf "release=%s\nimage=%s\n" "$release" "$image"' \
	>"$evidence_dir/rhck-selection.txt"
selected_image=$(awk -F= '$1 == "image" {print $2}' "$evidence_dir/rhck-selection.txt")
[[ $selected_image == /boot/vmlinuz-* && $selected_image != *uek* ]] \
	|| { echo "the guest did not select an RHCK image" >&2; exit 77; }
guest 'grubby --info=ALL' >"$evidence_dir/grubby-after.txt"
grep -Fq "kernel=\"$selected_image\"" "$evidence_dir/grubby-after.txt" \
	|| { echo "the grubby inventory lacks the selected kernel image" >&2; exit 77; }
guest 'sync; systemctl reboot' >/dev/null 2>&1 || true
address=
wait_for_guest || { echo "the guest did not return after the qualified boot" >&2; exit 77; }

qualified_boot_id=$(guest 'cat /proc/sys/kernel/random/boot_id')
[[ $qualified_boot_id != "$initial_boot_id" ]] \
	|| { echo "the guest did not complete a distinct qualified boot" >&2; exit 77; }
running_kernel=$(guest 'uname -r')
[[ $running_kernel != *uek* && "/boot/vmlinuz-$running_kernel" == "$selected_image" ]] \
	|| { echo "the guest did not boot the selected RHCK kernel" >&2; exit 77; }
guest 'systemctl --version' >"$evidence_dir/qualified-boot/systemd-version.txt"
guest 'rpm -q systemd' >"$evidence_dir/qualified-boot/systemd-package.txt"
guest 'cat /etc/os-release' >"$evidence_dir/qualified-boot/os-release.txt"
guest 'uname -r' >"$evidence_dir/qualified-boot/kernel.txt"
guest 'cat /proc/cmdline' >"$evidence_dir/qualified-boot/cmdline.txt"
guest 'cat /proc/sys/kernel/random/boot_id' >"$evidence_dir/qualified-boot/boot-id.txt"
guest 'stat -fc %T /sys/fs/cgroup' >"$evidence_dir/qualified-boot/cgroup-filesystem.txt"
guest 'cat /sys/fs/cgroup/cgroup.controllers' >"$evidence_dir/qualified-boot/controllers.txt"
guest 'cat /sys/fs/cgroup/cgroup.subtree_control' \
	>"$evidence_dir/qualified-boot/subtree-control.txt"
# Expand uname inside the guest shell.
# shellcheck disable=SC2016
guest 'rpm -q --whatprovides "/boot/vmlinuz-$(uname -r)"' \
	>"$evidence_dir/qualified-boot/running-kernel-package.txt"
# Expand uname inside the guest shell.
# shellcheck disable=SC2016
guest 'grep -E "^CONFIG_RT_GROUP_SCHED" "/boot/config-$(uname -r)"' \
	>"$evidence_dir/qualified-boot/rt-group-sched.txt"
guest 'find /proc/pressure -maxdepth 1 -type f -printf "%f\n" | sort' \
	>"$evidence_dir/qualified-boot/psi-files.txt"
grep -q '^systemd 239 ' "$evidence_dir/qualified-boot/systemd-version.txt" \
	|| { echo "the qualified guest does not run systemd 239" >&2; exit 77; }
[[ $(<"$evidence_dir/qualified-boot/cgroup-filesystem.txt") == cgroup2fs ]] \
	|| { echo "the qualified guest does not use cgroup v2" >&2; exit 77; }
grep -q '^kernel-core-' "$evidence_dir/qualified-boot/running-kernel-package.txt" \
	|| { echo "the running kernel is not owned by kernel-core" >&2; exit 77; }
grep -q '^CONFIG_RT_GROUP_SCHED=y$' "$evidence_dir/qualified-boot/rt-group-sched.txt" \
	|| { echo "the selected kernel lacks CONFIG_RT_GROUP_SCHED=y" >&2; exit 77; }

guest 'install -d -m 0700 /root/resman-cpudel'
scp "${ssh_options[@]}" "$package" "$script_dir/guest_probe.py" \
	root@"$address":/root/resman-cpudel/ >"$evidence_dir/transfer.log" 2>&1
guest 'dnf install -y /root/resman-cpudel/package.rpm' >"$evidence_dir/package-install.log"
guest 'systemctl stop resman || true; systemctl reset-failed resman || true'
guest 'rpm -q resman; systemctl show resman -p UnitFileState -p ActiveState -p Result' \
	>"$evidence_dir/qualified-boot/package-state.txt"
set +e
guest "python3 /root/resman-cpudel/guest_probe.py --evidence /root/resman-cpudel/evidence --run-id '$run_id' --source-revision '$package_source_revision' --unit '$fixture_unit'" \
	>"$evidence_dir/guest-run.log" 2>&1
guest_status=$?
set -e
mkdir -p "$evidence_dir/guest"
if guest 'test -d /root/resman-cpudel/evidence'; then
	guest 'tar -C /root/resman-cpudel -cf - evidence' \
		| tar --no-same-owner -C "$evidence_dir/guest" --strip-components=1 -xf -
else
	detail="the guest exited before producing evidence"
	[[ $guest_status -ne 0 ]] || guest_status=1
	exit "$guest_status"
fi

case "$guest_status" in
	0)
		[[ $(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["result"])' \
			"$evidence_dir/guest/result.json") == MEASURED ]] \
			|| { echo "the guest did not publish a complete measurement" >&2; exit 1; }
		result=PASS
		detail="EL8 RHCK guest completed the cpu delegation measurement"
		;;
	77)
		detail="the guest could not provide valid delegation evidence"
		exit 77
		;;
	*)
		detail="the guest probe failed after producing evidence"
		exit "$guest_status"
		;;
esac
