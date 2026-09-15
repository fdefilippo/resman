#!/usr/bin/env bash
set -Eeuo pipefail

base_image=${1:?Oracle Linux base image is required}
prepared_image=${2:?prepared RHCK image is required}
base_sha256=${RESMAN_IO_EFFECT_BASE_SHA256:-b12103391327abee8090686759c0d62dac9a7af2bf0f45fdf6b0d085a0fbb52b}
kernel_release=${RESMAN_IO_EFFECT_KERNEL_RELEASE:-5.14.0-687.46.1.el9_8.x86_64}
prepared_owner=${RESMAN_IO_EFFECT_PREPARED_OWNER-qemu:qemu}
manifest=$prepared_image.manifest
parent=$(dirname -- "$prepared_image")
temporary_dir=

[[ $base_image == /* && $prepared_image == /* && $base_image != "$prepared_image" ]] \
	|| { echo "base and prepared image paths must be distinct absolute paths" >&2; exit 2; }
[[ $(basename -- "$prepared_image") == resman-iow-effect-prepared-*.qcow2 ]] \
	|| { echo "unsafe prepared image name: $prepared_image" >&2; exit 2; }
[[ $base_sha256 =~ ^[0-9a-f]{64}$ ]] || { echo "invalid base digest" >&2; exit 2; }
[[ $kernel_release =~ ^[A-Za-z0-9._+-]+$ ]] || { echo "invalid kernel release" >&2; exit 2; }

cleanup() {
	local status=$?
	trap - EXIT
	if [[ -n $temporary_dir && $temporary_dir == "$parent"/.resman-iow-effect-prepare.* ]]; then
		for path in "$temporary_dir/image.qcow2" "$temporary_dir/manifest"; do
			[[ ! -e $path && ! -L $path ]] || unlink -- "$path"
		done
		rmdir -- "$temporary_dir" 2>/dev/null || true
	fi
	exit "$status"
}
trap cleanup EXIT

for command in qemu-img virt-customize sha256sum; do
	command -v "$command" >/dev/null 2>&1 || { echo "missing preparation command: $command" >&2; exit 77; }
done
[[ -f $base_image && ! -L $base_image ]] || { echo "base image is unavailable" >&2; exit 77; }
[[ $(sha256sum "$base_image" | awk '{print $1}') == "$base_sha256" ]] \
	|| { echo "base image differs from the reviewed Oracle digest" >&2; exit 1; }
install -d -m 0710 "$parent"

cache_is_valid() {
	[[ -f $prepared_image && ! -L $prepared_image && -f $manifest && ! -L $manifest ]] || return 1
	[[ $(awk -F= '$1 == "schema" {print $2}' "$manifest") == 1 ]] || return 1
	[[ $(awk -F= '$1 == "base_image" {print $2}' "$manifest") == "$base_image" ]] || return 1
	[[ $(awk -F= '$1 == "base_sha256" {print $2}' "$manifest") == "$base_sha256" ]] || return 1
	[[ $(awk -F= '$1 == "kernel_release" {print $2}' "$manifest") == "$kernel_release" ]] || return 1
	local declared
	declared=$(awk -F= '$1 == "prepared_sha256" {print $2}' "$manifest")
	[[ $declared =~ ^[0-9a-f]{64}$ && $(sha256sum "$prepared_image" | awk '{print $1}') == "$declared" ]]
}

if cache_is_valid; then
	printf '%s\n' "$prepared_image"
	exit 0
fi

for path in "$prepared_image" "$manifest"; do
	[[ ! -e $path && ! -L $path ]] || unlink -- "$path"
done
temporary_dir=$(mktemp -d "$parent/.resman-iow-effect-prepare.XXXXXX")
temporary_image=$temporary_dir/image.qcow2
temporary_manifest=$temporary_dir/manifest

qemu-img create -q -f qcow2 -F qcow2 -b "$base_image" "$temporary_image"
if [[ -n $prepared_owner ]]; then
	chown "$prepared_owner" "$temporary_dir" "$temporary_image"
	chmod 0710 "$temporary_dir"
	chmod 0600 "$temporary_image"
fi
LIBGUESTFS_BACKEND=direct virt-customize -q -a "$temporary_image" --network \
	--install "cpio,curl,python3,util-linux,systemd-udev,kernel-$kernel_release" \
	--run-command "test -f /boot/vmlinuz-$kernel_release" \
	--run-command "grubby --set-default /boot/vmlinuz-$kernel_release" \
	--run-command "grubby --update-kernel=/boot/vmlinuz-$kernel_release --args=systemd.unified_cgroup_hierarchy=1" \
	--selinux-relabel
qemu-img check -q "$temporary_image"
prepared_sha256=$(sha256sum "$temporary_image" | awk '{print $1}')
printf 'schema=1\nbase_image=%s\nbase_sha256=%s\nkernel_release=%s\nprepared_sha256=%s\n' \
	"$base_image" "$base_sha256" "$kernel_release" "$prepared_sha256" >"$temporary_manifest"
chmod 0600 "$temporary_image" "$temporary_manifest"
if [[ -n $prepared_owner ]]; then
	chown "$prepared_owner" "$temporary_image"
fi
mv -- "$temporary_image" "$prepared_image"
mv -- "$temporary_manifest" "$manifest"
rmdir -- "$temporary_dir"
temporary_dir=
printf '%s\n' "$prepared_image"
