#!/usr/bin/env bash
# Qualify ResMan's early-boot start on one disposable Enterprise Linux guest:
# three cold boots with a vcs.service fixture that requests SCHED_FIFO itself.
set -Eeuo pipefail

platform=${1:?platform is required}
run_id=${2:?run id is required}
qualification_revision=${3:?qualification revision is required}
package_source_revision=${4:?package source revision is required}
script_dir=$(CDPATH='' cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
evidence_dir=$script_dir/evidence
package=$script_dir/package.rpm
build_manifest=$script_dir/build-manifest.txt
vm_name=resman-eboot-$run_id-$platform
work_dir=/var/lib/libvirt/images/$vm_name
overlay=$work_dir/root.qcow2
private_key=$work_dir/guest-key
known_hosts=$work_dir/known-hosts
address=
vm_defined=0
result=FAIL
detail="early-boot qualification did not complete"

case "$platform" in
	el8)
		base_image=${RESMAN_EBOOT_EL8_BASE_IMAGE:-/var/lib/libvirt/images/OL8U10-base.qcow2}
		base_url=https://yum.oracle.com/templates/OracleLinux/OL8/u10/x86_64/OL8U10_x86_64-kvm-b287.qcow2
		base_sha256=cf9eb243b7390311f1e2896e3e6849241e521e6592c8be9907956e3a6cee1f0c
		os_variant=ol8.10
		boot_args="systemd.unified_cgroup_hierarchy=1 psi=1"
		;;
	el9)
		base_image=${RESMAN_EBOOT_EL9_BASE_IMAGE:-/var/lib/libvirt/images/OL9U8-base.qcow2}
		base_url=https://yum.oracle.com/templates/OracleLinux/OL9/u8/x86_64/OL9U8_x86_64-kvm-b293.qcow2
		base_sha256=b12103391327abee8090686759c0d62dac9a7af2bf0f45fdf6b0d085a0fbb52b
		os_variant=ol9.4
		boot_args="psi=1"
		;;
	el10)
		base_image=${RESMAN_EBOOT_EL10_BASE_IMAGE:-/var/lib/libvirt/images/OL10U1-base.qcow2}
		base_url=https://yum.oracle.com/templates/OracleLinux/OL10/u1/x86_64/OL10U1_x86_64-kvm-b291.qcow2
		base_sha256=8e59326c4bf7cfa58a6cac404db8ed583fe3a5f4c460e2b73c64988785bb4f0f
		os_variant=ol9-unknown
		boot_args="psi=1"
		;;
	*) echo "unsupported platform: $platform" >&2; exit 2 ;;
esac

[[ $run_id =~ ^r[0-9]{14}-[0-9]+$ ]] || { echo "unsafe run id: $run_id" >&2; exit 2; }
[[ $qualification_revision =~ ^[0-9a-f]{40}$ ]] \
	|| { echo "a full qualification revision is required" >&2; exit 2; }
[[ $package_source_revision =~ ^[0-9a-f]{40}$ ]] \
	|| { echo "a full package source revision is required" >&2; exit 2; }
[[ $work_dir =~ ^/var/lib/libvirt/images/resman-eboot-r[0-9]{14}-[0-9]+-el(8|9|10)$ ]] \
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

reboot_guest() {
	local before after
	before=$(guest 'cat /proc/sys/kernel/random/boot_id')
	guest 'sync; systemctl reboot' >/dev/null 2>&1 || true
	address=
	sleep 5
	wait_for_guest || { echo "the guest did not return after a reboot" >&2; exit 77; }
	after=$(guest 'cat /proc/sys/kernel/random/boot_id')
	[[ $after != "$before" ]] || { echo "the guest did not complete a distinct boot" >&2; exit 77; }
	# Let the boot settle so every unit has a final state to record.
	# Expand the loop inside the guest shell.
	# shellcheck disable=SC2016
	guest 'for _ in $(seq 1 60); do state=$(systemctl is-system-running 2>/dev/null || true); case "$state" in running|degraded) exit 0 ;; esac; sleep 2; done; exit 1' \
		|| { echo "the guest did not finish booting" >&2; exit 77; }
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

exec 9>/run/lock/resman-eboot-qemu.lock
flock -n 9 || { echo "another early-boot qualification owns this host" >&2; exit 75; }

for command in curl qemu-img qemu-system-x86_64 virt-customize virt-install virsh \
	ssh ssh-keygen ssh-keyscan scp rpm sha256sum; do
	command -v "$command" >/dev/null 2>&1 || { echo "missing host command: $command" >&2; exit 77; }
done
[[ -f $package && ! -L $package ]] || { echo "the package is unavailable" >&2; exit 77; }
[[ -f $build_manifest && ! -L $build_manifest ]] || { echo "the build manifest is unavailable" >&2; exit 77; }
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
	|| { echo "base image does not match the reviewed Oracle digest" >&2; exit 1; }
package_identity=$(rpm -qp --qf '%{NAME}-%{VERSION}-%{RELEASE}.%{ARCH}' "$package")
package_sha256=$(sha256sum "$package" | awk '{print $1}')
[[ $(awk -F= '$1 == "source_revision" {print $2}' "$build_manifest") == "$package_source_revision" ]] \
	|| { echo "build manifest revision differs from the requested revision" >&2; exit 1; }
[[ $(awk -F= '$1 == "package_sha256" {print $2}' "$build_manifest") == "$package_sha256" ]] \
	|| { echo "build manifest does not identify the supplied package" >&2; exit 1; }
[[ ! -e $work_dir ]] || { echo "QEMU work directory already exists: $work_dir" >&2; exit 75; }
! virsh dominfo "$vm_name" >/dev/null 2>&1 || { echo "QEMU domain already exists: $vm_name" >&2; exit 75; }

{
	printf 'platform=%s\nrun_id=%s\nqualification_revision=%s\nsource_revision=%s\n' \
		"$platform" "$run_id" "$qualification_revision" "$package_source_revision"
	printf 'base_image=%s\nbase_url=%s\nbase_sha256=%s\n' "$base_image" "$base_url" "$base_sha256"
	printf 'kernel_family=rhck\nboot_args=%s\n' "$boot_args"
	printf 'package_identity=%s\npackage_sha256=%s\n' "$package_identity" "$package_sha256"
	printf 'build_manifest_sha256=%s\n' "$(sha256sum "$build_manifest" | awk '{print $1}')"
	printf 'qemu_version=%s\n' "$(qemu-system-x86_64 --version | head -n 1)"
	printf 'libvirt_version=%s\n' "$(virsh version --daemon 2>/dev/null | tr '\n' ' ')"
	printf 'vcpus=2\nmemory_mib=3072\n'
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
	--os-variant "$os_variant" --boot uefi

wait_for_guest || { echo "guest SSH did not become ready" >&2; exit 77; }
guest 'dnf install -y python3 procps-ng util-linux' >"$evidence_dir/provision.log" 2>&1

# The field nodes run the distribution kernel; only that family is known to
# carry CONFIG_RT_GROUP_SCHED=y, which the realtime scenarios depend on.
guest 'dnf install -y --setopt=install_weak_deps=False kernel' >"$evidence_dir/rhck-install.log" 2>&1
# Expand the package and boot-image expressions inside the guest shell.
# shellcheck disable=SC2016
guest "set -eu; release=\$(rpm -q --qf '%{VERSION}-%{RELEASE}.%{ARCH}\n' kernel-core | sort -V | tail -n 1); image=/boot/vmlinuz-\$release; test -f \"\$image\"; grubby --set-default \"\$image\"; grubby --update-kernel=\"\$image\" --args='$boot_args'; printf 'release=%s\nimage=%s\n' \"\$release\" \"\$image\"" \
	>"$evidence_dir/rhck-selection.txt"
selected_image=$(awk -F= '$1 == "image" {print $2}' "$evidence_dir/rhck-selection.txt")
[[ $selected_image == /boot/vmlinuz-* && $selected_image != *uek* ]] \
	|| { echo "the guest did not select a distribution kernel" >&2; exit 77; }

# The fixture stands in for Veritas Cluster Server: like had, it requests
# SCHED_FIFO itself, so its unit file declares no CPUSchedulingPolicy.
guest 'install -d -m 0755 /usr/local/libexec && cat >/usr/local/libexec/resman-eboot-fixture.py' <<'FIXTURE'
import json
import os
import time

try:
    os.sched_setscheduler(0, os.SCHED_FIFO, os.sched_param(10))
    outcome = {"result": "granted", "policy": os.sched_getscheduler(0)}
except OSError as error:
    outcome = {"result": "refused", "errno": error.errno, "error": os.strerror(error.errno)}
with open("/proc/self/cgroup", encoding="utf-8") as handle:
    outcome["cgroup"] = handle.read().strip()
with open("/run/resman-eboot-fixture.json", "w", encoding="utf-8") as handle:
    json.dump(outcome, handle)
while True:
    time.sleep(3600)
FIXTURE
guest 'cat >/etc/systemd/system/vcs.service' <<'UNIT'
[Unit]
Description=ResMan early-boot fixture standing in for Veritas Cluster Server

[Service]
Type=simple
ExecStart=/usr/bin/python3 /usr/local/libexec/resman-eboot-fixture.py

[Install]
WantedBy=multi-user.target
UNIT
guest 'install -d -m 0700 /root/resman-eboot'
scp "${ssh_options[@]}" "$package" root@"$address":/root/resman-eboot/package.rpm \
	>"$evidence_dir/transfer.log" 2>&1
guest 'dnf install -y /root/resman-eboot/package.rpm' >"$evidence_dir/package-install.log" 2>&1
guest 'systemctl daemon-reload && systemctl enable resman.service vcs.service' \
	>"$evidence_dir/enable.log" 2>&1

# Rewrite only the declaration keys the scenarios vary; the rest of the
# packaged configuration stays as installed.
configure() {
	local mode=$1 protected=$2
	guest "set -eu; conf=/etc/resman/resman.conf
		sed -i -e '/^ENFORCEMENT_MODE=/d' -e '/^REALTIME_PROTECTED_UNITS=/d' \
			-e '/^RAM_LIMIT_ENABLED=/d' -e '/^IO_LIMIT_ENABLED=/d' \"\$conf\"
		printf 'ENFORCEMENT_MODE=%s\nREALTIME_PROTECTED_UNITS=%s\nRAM_LIMIT_ENABLED=false\nIO_LIMIT_ENABLED=false\n' '$mode' '$protected' >>\"\$conf\"
		: >/var/log/resman.log
		systemctl reset-failed resman.service 2>/dev/null || true"
}

collect() {
	local scenario=$1 out=$evidence_dir/$1
	mkdir -p "$out"
	guest 'cat /proc/sys/kernel/random/boot_id' >"$out/boot-id.txt"
	guest 'uname -r' >"$out/kernel.txt"
	guest 'systemctl --version | head -n 1' >"$out/systemd-version.txt"
	guest 'stat -fc %T /sys/fs/cgroup' >"$out/cgroup-filesystem.txt"
	# Expand uname inside the guest shell.
	# shellcheck disable=SC2016
	guest 'grep -E "^(CONFIG_RT_GROUP_SCHED|# CONFIG_RT_GROUP_SCHED)" "/boot/config-$(uname -r)" || true' >"$out/rt-group-sched.txt"
	guest 'cat /proc/sys/kernel/sched_rt_runtime_us' >"$out/sched-rt-runtime-us.txt"
	guest 'grep -E "^(ENFORCEMENT_MODE|REALTIME_PROTECTED_UNITS|RAM_LIMIT_ENABLED|IO_LIMIT_ENABLED)=" /etc/resman/resman.conf' >"$out/declaration.txt"
	guest 'systemctl show resman.service -p ActiveState -p SubState -p Result -p ExecMainStatus -p NRestarts -p InactiveExitTimestampMonotonic -p ActiveEnterTimestampMonotonic' >"$out/resman-unit.txt"
	guest 'systemctl show vcs.service -p ActiveState -p ActiveEnterTimestampMonotonic' >"$out/vcs-unit.txt"
	guest 'systemctl show sysinit.target -p ActiveEnterTimestampMonotonic' >"$out/sysinit-target.txt"
	guest 'systemctl show dbus.socket -p ActiveEnterTimestampMonotonic' >"$out/dbus-socket.txt"
	guest 'cat /run/resman-eboot-fixture.json' >"$out/fixture.json"
	guest 'cat /sys/fs/cgroup/cgroup.subtree_control' >"$out/root-subtree-control.txt"
	guest 'cat /sys/fs/cgroup/user.slice/cgroup.subtree_control' >"$out/user-slice-subtree-control.txt"
	guest 'journalctl -b -u resman.service --no-pager -o short-monotonic' >"$out/resman-journal.log"
	guest 'cat /var/log/resman.log' >"$out/resman.log"
	guest 'journalctl -b --no-pager | grep -c -e bus_unavailable -e system_bus_socket || true' >"$out/bus-failures.txt"
	guest 'systemd-analyze critical-chain sysinit.target --no-pager || true' >"$out/critical-chain-sysinit.txt" 2>&1
	guest 'systemd-analyze critical-chain resman.service --no-pager || true' >"$out/critical-chain-resman.txt" 2>&1
	guest 'ps -eLo pid,tid,cls,rtprio,cgroup,comm' >"$out/threads.txt"
}

run_scenario() {
	local scenario=$1 mode=$2 protected=$3
	configure "$mode" "$protected"
	printf 'scenario=%s\nenforcement_mode=%s\nrealtime_protected_units=%s\n' "$scenario" "$mode" "$protected" \
		>>"$evidence_dir/scenarios.txt"
	reboot_guest
	collect "$scenario"
}

# Each scenario is one cold boot; the kernel cgroup tree starts empty each time.
run_scenario protected-refusal systemd_native vcs.service
run_scenario protected-observation auto vcs.service
run_scenario consented-enforcement systemd_native ''

result=PASS
detail="three cold boots measured on $platform"
