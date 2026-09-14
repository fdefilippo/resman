#!/usr/bin/env bash
set -Eeuo pipefail

platform=${1:?platform is required}
run_id=${2:?run ID is required}
source_revision=${3:?source revision is required}
kernel_family=${4:-rhck}
script_dir=$(CDPATH='' cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
evidence_dir=$script_dir/evidence/$platform
serial=resmaniow${kernel_family}${platform}
vm_name=resman-iow-$run_id-$platform-$kernel_family
work_dir=/var/lib/libvirt/images/$vm_name
overlay=$work_dir/root.qcow2
probe_disk=$work_dir/probe.raw
private_key=$work_dir/guest-key
known_hosts=$work_dir/known-hosts
address=
vm_defined=0
result=FAIL
detail="platform characterization did not complete"

case "$platform" in
	el8)
		base_image=${RESMAN_IOW_EL8_BASE_IMAGE:-/var/lib/libvirt/images/OL8U10-base.qcow2}
		base_url=https://yum.oracle.com/templates/OracleLinux/OL8/u10/x86_64/OL8U10_x86_64-kvm-b287.qcow2
		base_sha256=cf9eb243b7390311f1e2896e3e6849241e521e6592c8be9907956e3a6cee1f0c
		os_variant=ol8.10
		;;
	el9)
		base_image=${RESMAN_IOW_EL9_BASE_IMAGE:-/var/lib/libvirt/images/OL9U8-base.qcow2}
		base_url=https://yum.oracle.com/templates/OracleLinux/OL9/u8/x86_64/OL9U8_x86_64-kvm-b293.qcow2
		base_sha256=b12103391327abee8090686759c0d62dac9a7af2bf0f45fdf6b0d085a0fbb52b
		os_variant=ol9.4
		;;
	el10)
		base_image=${RESMAN_IOW_EL10_BASE_IMAGE:-/var/lib/libvirt/images/OL10U1-base.qcow2}
		base_url=https://yum.oracle.com/templates/OracleLinux/OL10/u1/x86_64/OL10U1_x86_64-kvm-b291.qcow2
		base_sha256=8e59326c4bf7cfa58a6cac404db8ed583fe3a5f4c460e2b73c64988785bb4f0f
		os_variant=ol9-unknown
		;;
	*) echo "unsupported platform label: $platform" >&2; exit 2 ;;
esac

[[ $run_id =~ ^r[0-9]{14}-[0-9]+$ ]] || { echo "unsafe run ID: $run_id" >&2; exit 2; }
[[ $source_revision =~ ^[0-9a-f]{40}$ ]] || { echo "full source revision required" >&2; exit 2; }
[[ $kernel_family == rhck ]] || { echo "unsupported kernel family: $kernel_family" >&2; exit 2; }
[[ $work_dir == /var/lib/libvirt/images/resman-iow-r*-*-el*-rhck ]] \
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
		ssh-keyscan -T 2 -H "$candidate" >"$known_hosts.tmp" 2>/dev/null || {
			stable=0
			sleep 2
			continue
		}
		mv "$known_hosts.tmp" "$known_hosts"
		address=$candidate
		if guest true 2>/dev/null; then
			stable=$((stable + 1))
			if [[ $stable -ge 3 ]]; then
				return 0
			fi
		else
			stable=0
		fi
		sleep 2
	done
	return 1
}

write_manifest() {
	(
		cd "$evidence_dir"
		manifest_tmp=$script_dir/evidence/$platform-SHA256SUMS.tmp
		find . -type f ! -name SHA256SUMS -print0 | sort -z \
			| xargs -0 sha256sum -- >"$manifest_tmp"
		mv "$manifest_tmp" SHA256SUMS
	)
}

cleanup() {
	local status=$? cleanup_status=PASS
	trap - EXIT INT TERM
	set +e
	if [[ $status -eq 75 || $status -eq 77 ]] && [[ $result == FAIL ]]; then
		result=BLOCKED
		detail="QEMU platform infrastructure was unavailable or invalid"
	fi
	if [[ $vm_defined -eq 1 ]] && virsh dominfo "$vm_name" >/dev/null 2>&1; then
		virsh destroy "$vm_name" >>"$evidence_dir/cleanup.log" 2>&1 || true
		virsh undefine "$vm_name" --nvram >>"$evidence_dir/cleanup.log" 2>&1 \
			|| virsh undefine "$vm_name" >>"$evidence_dir/cleanup.log" 2>&1 \
			|| cleanup_status=FAIL
	fi
	if [[ -d $work_dir ]]; then
		rm -f -- "$overlay" "$probe_disk" "$private_key" "$private_key.pub" \
			"$known_hosts" "$known_hosts.tmp"
		rmdir -- "$work_dir" >>"$evidence_dir/cleanup.log" 2>&1 || cleanup_status=FAIL
	fi
	if [[ $cleanup_status != PASS ]]; then
		status=1
		result=FAIL
		detail="QEMU resource cleanup failed"
	fi
	printf 'guest_exit_code=%s\ncleanup=%s\nresult=%s\ndetail=%s\nexit_code=%d\n' \
		"${guest_status:-not-run}" "$cleanup_status" "$result" "$detail" "$status" \
		>>"$evidence_dir/environment.txt"
	printf '%s\n' "$result" >"$evidence_dir/result"
	write_manifest
	exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

for command in curl qemu-img qemu-system-x86_64 virt-customize virt-install virsh \
	ssh ssh-keygen ssh-keyscan scp sha256sum; do
	command -v "$command" >/dev/null 2>&1 \
		|| { echo "missing host command: $command" >&2; exit 77; }
done

if [[ ! -e $base_image ]]; then
	partial=$base_image.download-$run_id
	[[ ! -e $partial ]] || { echo "base-image download collision: $partial" >&2; exit 75; }
	curl -fL --retry 3 --output "$partial" "$base_url"
	printf '%s  %s\n' "$base_sha256" "$partial" | sha256sum --check -
	chown qemu:qemu "$partial"
	chmod 0644 "$partial"
	mv "$partial" "$base_image"
fi
[[ -f $base_image && ! -L $base_image ]] || { echo "base image is unavailable" >&2; exit 77; }
[[ $(sha256sum "$base_image" | awk '{print $1}') == "$base_sha256" ]] \
	|| { echo "base image does not match the pinned Oracle digest" >&2; exit 1; }
[[ ! -e $work_dir ]] || { echo "QEMU work directory already exists: $work_dir" >&2; exit 75; }
! virsh dominfo "$vm_name" >/dev/null 2>&1 \
	|| { echo "QEMU domain already exists: $vm_name" >&2; exit 75; }

{
	printf 'platform=%s\nkernel_family=%s\nrun_id=%s\nsource_revision=%s\n' \
		"$platform" "$kernel_family" "$run_id" "$source_revision"
	printf 'base_image=%s\nbase_url=%s\nbase_sha256=%s\n' "$base_image" "$base_url" "$base_sha256"
	printf 'probe_serial=%s\nos_variant=%s\n' "$serial" "$os_variant"
	printf 'qemu_version=%s\n' "$(qemu-system-x86_64 --version | head -n 1)"
	printf 'libvirt_version=%s\n' "$(virsh version --daemon 2>/dev/null | tr '\n' ' ')"
} >"$evidence_dir/environment.txt"
install -m 0600 "$script_dir/guest_probe.py" "$evidence_dir/guest-probe.py"

install -d -o qemu -g qemu -m 0710 "$work_dir"
qemu-img create -q -f qcow2 -F qcow2 -b "$base_image" "$overlay"
qemu-img create -q -f raw "$probe_disk" 512M
chown qemu:qemu "$overlay" "$probe_disk"
chmod 0600 "$overlay" "$probe_disk"
ssh-keygen -q -t ed25519 -N '' -f "$private_key"
virt-customize -q -a "$overlay" --ssh-inject "root:file:$private_key.pub" --selinux-relabel
vm_defined=1
virt-install --connect qemu:///system --name "$vm_name" --memory 3072 --vcpus 2 \
	--cpu host-passthrough --import \
	--disk "path=$overlay,format=qcow2,bus=sata" \
	--disk "path=$probe_disk,format=raw,bus=virtio,serial=$serial,cache=none" \
	--network network=default,model=virtio --graphics none --noautoconsole \
	--os-variant "$os_variant" --boot uefi

wait_for_guest || { echo "$platform guest SSH did not become ready" >&2; exit 77; }
mkdir -p "$evidence_dir/initial-boot" "$evidence_dir/qualified-boot"
guest 'systemctl --version' >"$evidence_dir/initial-boot/systemd-version.txt"
guest 'rpm -q systemd kernel-uek-core kernel-core 2>&1 || true' >"$evidence_dir/initial-boot/packages.txt"
guest 'cat /etc/os-release' >"$evidence_dir/initial-boot/os-release.txt"
guest 'uname -r' >"$evidence_dir/initial-boot/kernel.txt"
guest 'cat /proc/cmdline' >"$evidence_dir/initial-boot/cmdline.txt"
guest 'cat /proc/sys/kernel/random/boot_id' >"$evidence_dir/initial-boot/boot-id.txt"
guest 'stat -fc %T /sys/fs/cgroup' >"$evidence_dir/initial-boot/cgroup-filesystem.txt"
guest 'lsblk -o NAME,MAJ:MIN,SIZE,TYPE,FSTYPE,MOUNTPOINT,SERIAL' >"$evidence_dir/initial-boot/block-devices.txt"
guest 'dnf install -y python3 util-linux systemd-udev' >"$evidence_dir/provision.log"

guest 'grubby --info=ALL' >"$evidence_dir/grubby-before.txt"
guest 'dnf install -y kernel' >"$evidence_dir/rhck-install.log"
# Expand the package and boot-image expressions inside the guest shell.
# shellcheck disable=SC2016
guest 'set -eu; release=$(rpm -q --qf "%{VERSION}-%{RELEASE}.%{ARCH}\n" kernel-core | sort -V | tail -n 1); test -n "$release"; image=/boot/vmlinuz-$release; test -f "$image"; grubby --set-default "$image"; grubby --update-kernel="$image" --args="systemd.unified_cgroup_hierarchy=1"; printf "release=%s\nimage=%s\n" "$release" "$image"' \
	>"$evidence_dir/rhck-selection.txt"
guest 'grubby --info=ALL' >"$evidence_dir/grubby-after.txt"
selected_image=$(awk -F= '$1 == "image" {print $2}' "$evidence_dir/rhck-selection.txt")
[[ $selected_image == /boot/vmlinuz-* && $selected_image != *uek* ]] \
	|| { echo "$platform did not select an RHCK image" >&2; exit 77; }
grep -Fq "kernel=\"$selected_image\"" "$evidence_dir/grubby-after.txt" \
	|| { echo "$platform grubby inventory lacks the selected RHCK image" >&2; exit 77; }
initial_boot_id=$(<"$evidence_dir/initial-boot/boot-id.txt")
guest 'sync; systemctl reboot' >/dev/null 2>&1 || true
address=
wait_for_guest || { echo "$platform guest did not return after the RHCK boot" >&2; exit 77; }
qualified_boot_id=$(guest 'cat /proc/sys/kernel/random/boot_id')
[[ $qualified_boot_id != "$initial_boot_id" ]] \
	|| { echo "$platform did not complete a distinct RHCK boot" >&2; exit 77; }
running_kernel=$(guest 'uname -r')
[[ $running_kernel != *uek* && "/boot/vmlinuz-$running_kernel" == "$selected_image" ]] \
	|| { echo "$platform did not boot the selected RHCK kernel" >&2; exit 77; }
# Expand uname inside the guest shell.
# shellcheck disable=SC2016
guest 'rpm -q --whatprovides "/boot/vmlinuz-$(uname -r)"' >"$evidence_dir/rhck-running-package.txt"
grep -q '^kernel-core-' "$evidence_dir/rhck-running-package.txt" \
	|| { echo "$platform running kernel is not owned by kernel-core" >&2; exit 77; }

if [[ $(<"$evidence_dir/initial-boot/cgroup-filesystem.txt") != cgroup2fs ]]; then
	[[ $platform == el8 ]] || { echo "$platform RHCK boot did not materialize cgroup v2" >&2; exit 77; }
fi

guest 'systemctl --version' >"$evidence_dir/qualified-boot/systemd-version.txt"
guest 'rpm -q systemd kernel-uek-core kernel-core 2>&1 || true' >"$evidence_dir/qualified-boot/packages.txt"
guest 'cat /etc/os-release' >"$evidence_dir/qualified-boot/os-release.txt"
guest 'uname -r' >"$evidence_dir/qualified-boot/kernel.txt"
guest 'cat /proc/cmdline' >"$evidence_dir/qualified-boot/cmdline.txt"
guest 'cat /proc/sys/kernel/random/boot_id' >"$evidence_dir/qualified-boot/boot-id.txt"
guest 'stat -fc %T /sys/fs/cgroup' >"$evidence_dir/qualified-boot/cgroup-filesystem.txt"
guest 'cat /sys/fs/cgroup/cgroup.controllers' >"$evidence_dir/qualified-boot/controllers.txt"
guest 'lsblk -o NAME,MAJ:MIN,SIZE,TYPE,FSTYPE,MOUNTPOINT,SERIAL' >"$evidence_dir/qualified-boot/block-devices.txt"
[[ $(<"$evidence_dir/qualified-boot/cgroup-filesystem.txt") == cgroup2fs ]] \
	|| { echo "$platform qualified boot is not cgroup v2" >&2; exit 77; }
grep -q "$serial" "$evidence_dir/qualified-boot/block-devices.txt" \
	|| { echo "$platform guest lacks the owned virtual device" >&2; exit 77; }

guest 'install -d -m 0700 /root/resman-iow'
scp "${ssh_options[@]}" "$script_dir/guest_probe.py" \
	root@"$address":/root/resman-iow/guest_probe.py >"$evidence_dir/transfer.log" 2>&1
set +e
guest "python3 /root/resman-iow/guest_probe.py --evidence /root/resman-iow/evidence --platform '$platform' --run-id '$run_id' --source-revision '$source_revision' --device '/dev/disk/by-id/virtio-$serial'" \
	>"$evidence_dir/guest-run.log" 2>&1
guest_status=$?
set -e
mkdir -p "$evidence_dir/guest"
if guest 'test -d /root/resman-iow/evidence'; then
	guest 'tar -C /root/resman-iow -cf - evidence' \
		| tar --no-same-owner -C "$evidence_dir/guest" --strip-components=1 -xf -
else
	detail="guest exited before producing evidence"
	result=FAIL
	[[ $guest_status -ne 0 ]] || guest_status=1
	exit "$guest_status"
fi

case "$guest_status" in
	0)
		[[ $(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["result"])' \
			"$evidence_dir/guest/result.json") == CHARACTERIZED ]]
		result=PASS
		detail="$platform IODeviceWeight characterization completed"
		;;
	77)
		result=BLOCKED
		detail="$platform guest could not provide valid characterization"
		exit 77
		;;
	*)
		result=FAIL
		detail="$platform guest characterization failed"
		exit "$guest_status"
		;;
esac
