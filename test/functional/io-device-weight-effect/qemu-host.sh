#!/usr/bin/env bash
set -Eeuo pipefail

run_id=${1:?run ID is required}
qualification_revision=${2:?qualification revision is required}
script_dir=$(CDPATH='' cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
evidence_dir=$script_dir/evidence
package=$script_dir/package.rpm
build_manifest=$script_dir/build-manifest.txt
base_image=${RESMAN_IO_EFFECT_OL9_BASE_IMAGE:-/var/lib/libvirt/images/OL9U8-base.qcow2}
base_url=https://yum.oracle.com/templates/OracleLinux/OL9/u8/x86_64/OL9U8_x86_64-kvm-b293.qcow2
base_sha256=b12103391327abee8090686759c0d62dac9a7af2bf0f45fdf6b0d085a0fbb52b
kernel_release=5.14.0-687.46.1.el9_8.x86_64
source_revision=$(awk -F= '$1 == "source_revision" {print $2}' "$build_manifest")
source_tree=$(awk -F= '$1 == "source_tree" {print $2}' "$build_manifest")
qualification_tree=$(awk -F= '$1 == "qualification_tree" {print $2}' "$script_dir/qualification.txt")
vm_name=resman-iow-effect-$run_id
work_dir=/var/lib/libvirt/images/$vm_name
overlay=$work_dir/root.qcow2
device_a=$work_dir/effect-a.raw
device_b=$work_dir/effect-b.raw
private_key=$script_dir/guest-key
known_hosts=$script_dir/known-hosts
address=
vm_defined=0
result=FAIL
detail="effect qualification did not complete"
guest_status=not-run

[[ $run_id =~ ^r[0-9]{14}-[0-9]+$ ]] || { echo "unsafe run ID: $run_id" >&2; exit 2; }
[[ $source_revision =~ ^[0-9a-f]{40}$ && $source_tree =~ ^[0-9a-f]{40}$ &&
	$qualification_revision =~ ^[0-9a-f]{40}$ && $qualification_tree =~ ^[0-9a-f]{40}$ ]] \
	|| { echo "immutable package and qualification source identities are required" >&2; exit 2; }
[[ $work_dir == /var/lib/libvirt/images/resman-iow-effect-r*-* ]] \
	|| { echo "unsafe work directory: $work_dir" >&2; exit 2; }
[[ ! -e $evidence_dir ]] || { echo "evidence directory already exists" >&2; exit 75; }
mkdir -m 0700 "$evidence_dir"

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
			[[ $stable -lt 3 ]] || return 0
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
		manifest_tmp=$script_dir/evidence-SHA256SUMS.tmp
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
		detail="QEMU infrastructure or exact representative was unavailable"
	fi
	if [[ $vm_defined -eq 1 ]] && virsh dominfo "$vm_name" >/dev/null 2>&1; then
		virsh destroy "$vm_name" >>"$evidence_dir/cleanup.log" 2>&1 || true
		virsh undefine "$vm_name" --nvram >>"$evidence_dir/cleanup.log" 2>&1 \
			|| virsh undefine "$vm_name" >>"$evidence_dir/cleanup.log" 2>&1 \
			|| cleanup_status=FAIL
	fi
	if [[ -d $work_dir ]]; then
		rm -f -- "$overlay" "$device_a" "$device_b"
		rmdir -- "$work_dir" >>"$evidence_dir/cleanup.log" 2>&1 || cleanup_status=FAIL
	fi
	rm -f -- "$private_key" "$private_key.pub" "$known_hosts" "$known_hosts.tmp"
	if [[ $cleanup_status != PASS ]]; then
		status=1
		result=FAIL
		detail="QEMU resource cleanup failed"
	fi
	printf 'guest_exit_code=%s\ncleanup=%s\nresult=%s\ndetail=%s\nexit_code=%d\n' \
		"$guest_status" "$cleanup_status" "$result" "$detail" "$status" \
		>>"$evidence_dir/environment.txt"
	printf '%s\n' "$result" >"$evidence_dir/result"
	write_manifest
	exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

exec 9>/run/lock/resman-iodeviceweight-effect-qemu.lock
flock -n 9 || { echo "another weighted-I/O effect campaign owns this host" >&2; exit 75; }

for command in qemu-img qemu-system-x86_64 virt-customize virt-install virsh \
	ssh ssh-keygen ssh-keyscan scp rpm sha256sum; do
	command -v "$command" >/dev/null 2>&1 || { echo "missing host command: $command" >&2; exit 77; }
done
[[ -f $base_image && ! -L $base_image ]] || { echo "OL9 base image is unavailable" >&2; exit 77; }
[[ -f $package && ! -L $package && -f $build_manifest && ! -L $build_manifest ]] \
	|| { echo "exact RPM and build manifest are required" >&2; exit 77; }
[[ $(sha256sum "$base_image" | awk '{print $1}') == "$base_sha256" ]] \
	|| { echo "OL9 base image differs from reviewed Oracle digest" >&2; exit 1; }
package_identity=$(rpm -qp --qf '%{NAME}-%{VERSION}-%{RELEASE}.%{ARCH}' "$package")
package_sha=$(sha256sum "$package" | awk '{print $1}')
[[ $package_identity == resman-1.38.0-1.el9.x86_64 ]] \
	|| { echo "unexpected package identity: $package_identity" >&2; exit 1; }
[[ $(awk -F= '$1 == "source_revision" {print $2}' "$build_manifest") == "$source_revision" && \
	$(awk -F= '$1 == "source_tree" {print $2}' "$build_manifest") == "$source_tree" && \
	$(awk -F= '$1 == "package_sha256" {print $2}' "$build_manifest") == "$package_sha" ]] \
	|| { echo "build manifest does not identify source and package" >&2; exit 1; }
[[ ! -e $work_dir ]] || { echo "QEMU work directory exists: $work_dir" >&2; exit 75; }
! virsh dominfo "$vm_name" >/dev/null 2>&1 \
	|| { echo "QEMU domain exists: $vm_name" >&2; exit 75; }

{
	printf 'run_id=%s\nsource_revision=%s\nsource_tree=%s\n' "$run_id" "$source_revision" "$source_tree"
	printf 'qualification_revision=%s\nqualification_tree=%s\n' "$qualification_revision" "$qualification_tree"
	printf 'base_image=%s\nbase_url=%s\nbase_sha256=%s\n' "$base_image" "$base_url" "$base_sha256"
	printf 'package_identity=%s\npackage_sha256=%s\n' "$package_identity" "$package_sha"
	printf 'qemu_version=%s\n' "$(qemu-system-x86_64 --version | head -n 1)"
	printf 'libvirt_version=%s\n' "$(virsh version --daemon 2>/dev/null | tr '\n' ' ')"
} >"$evidence_dir/environment.txt"
install -m 0600 "$build_manifest" "$evidence_dir/build-manifest.txt"

install -d -o qemu -g qemu -m 0710 "$work_dir"
qemu-img create -q -f qcow2 -F qcow2 -b "$base_image" "$overlay"
qemu-img create -q -f raw "$device_a" 8G
qemu-img create -q -f raw "$device_b" 8G
chown qemu:qemu "$overlay" "$device_a" "$device_b"
chmod 0600 "$overlay" "$device_a" "$device_b"
ssh-keygen -q -t ed25519 -N '' -f "$private_key"
virt-customize -q -a "$overlay" --ssh-inject "root:file:$private_key.pub" --selinux-relabel
vm_defined=1
virt-install --connect qemu:///system --name "$vm_name" --memory 3072 --vcpus 2 \
	--cpu host-passthrough --import \
	--disk "path=$overlay,format=qcow2,bus=sata" \
	--disk "path=$device_a,format=raw,bus=virtio,serial=resmaneffecta,cache=none,io=native" \
	--disk "path=$device_b,format=raw,bus=virtio,serial=resmaneffectb,cache=none,io=native" \
	--network network=default,model=virtio --graphics none --noautoconsole \
	--os-variant ol9.4 --boot uefi

wait_for_guest || { echo "OL9 guest SSH did not become ready" >&2; exit 77; }
initial_boot_id=$(guest 'cat /proc/sys/kernel/random/boot_id')
guest 'cat /etc/os-release; systemctl --version; uname -r; stat -fc %T /sys/fs/cgroup; lsblk -o NAME,MAJ:MIN,SIZE,TYPE,SERIAL' \
	>"$evidence_dir/initial-boot.txt"
guest 'dnf install -y --setopt=install_weak_deps=False cpio curl python3 util-linux systemd-udev kernel-5.14.0-687.46.1.el9_8.x86_64' \
	>"$evidence_dir/provision.log"
guest "test -f /boot/vmlinuz-$kernel_release && grubby --set-default /boot/vmlinuz-$kernel_release && grubby --update-kernel=/boot/vmlinuz-$kernel_release --args=systemd.unified_cgroup_hierarchy=1"
guest 'sync; systemctl reboot' >/dev/null 2>&1 || true
address=
wait_for_guest || { echo "OL9 guest did not return after RHCK boot" >&2; exit 77; }
qualified_boot_id=$(guest 'cat /proc/sys/kernel/random/boot_id')
[[ $qualified_boot_id != "$initial_boot_id" ]] || { echo "guest did not complete a distinct boot" >&2; exit 77; }
[[ $(guest 'uname -r') == "$kernel_release" ]] || { echo "guest did not boot exact RHCK" >&2; exit 77; }
# Evaluated by the guest shell.
# shellcheck disable=SC2016
guest 'rpm -q --whatprovides "/boot/vmlinuz-$(uname -r)"; cat /etc/os-release; systemctl --version; uname -r; cat /proc/cmdline; stat -fc %T /sys/fs/cgroup; cat /sys/fs/cgroup/cgroup.controllers; lsblk -o NAME,MAJ:MIN,SIZE,TYPE,SERIAL' \
	>"$evidence_dir/qualified-boot.txt"
grep -q 'resmaneffecta' "$evidence_dir/qualified-boot.txt"
grep -q 'resmaneffectb' "$evidence_dir/qualified-boot.txt"

# Evaluated by the guest shell.
# shellcheck disable=SC2016
guest 'for entry in resman-t1:1006 resman-t2:1007; do name=${entry%:*}; uid=${entry#*:}; id "$name" >/dev/null 2>&1 || useradd --uid "$uid" --create-home "$name"; done'
guest 'install -d -m 0700 /root/resman-iow-effect'
scp "${ssh_options[@]}" "$package" "$script_dir/guest_effect.py" \
	root@"$address":/root/resman-iow-effect/ >"$evidence_dir/transfer.log" 2>&1
guest 'dnf install -y /root/resman-iow-effect/package.rpm' >"$evidence_dir/package-install.log"
set +e
guest "python3 /root/resman-iow-effect/guest_effect.py --bundle /root/resman-iow-effect --evidence /root/resman-iow-effect/evidence --run-id '$run_id' --revision '$source_revision' --source-tree '$source_tree' --qualification-revision '$qualification_revision' --qualification-tree '$qualification_tree' --package /root/resman-iow-effect/package.rpm --device /dev/disk/by-id/virtio-resmaneffecta --device /dev/disk/by-id/virtio-resmaneffectb" \
	>"$evidence_dir/guest-run.log" 2>&1
guest_status=$?
set -e
mkdir -p "$evidence_dir/guest"
if guest 'test -d /root/resman-iow-effect/evidence'; then
	guest 'tar -C /root/resman-iow-effect/evidence -cf - .' \
		| tar --no-same-owner -C "$evidence_dir/guest" -xf -
else
	detail="guest exited before producing evidence"
	[[ $guest_status -ne 0 ]] || guest_status=1
	exit "$guest_status"
fi
if [[ $guest_status -ne 0 ]]; then
	detail="guest campaign failed after producing evidence"
	exit "$guest_status"
fi

(
	cd "$evidence_dir/guest"
	manifest_tmp=$evidence_dir/guest-SHA256SUMS.tmp
	find . -type f ! -name SHA256SUMS -print0 | sort -z | xargs -0 sha256sum -- >"$manifest_tmp"
	mv "$manifest_tmp" SHA256SUMS
)
python3 "$script_dir/validate_evidence.py" "$evidence_dir/guest" \
	--revision "$source_revision" --package-sha "$package_sha" >"$evidence_dir/validation.log"
result=PASS
detail="exact OL9/RHCK packaged daemon qualified BFQ and io.cost delivery"
