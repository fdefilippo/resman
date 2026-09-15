#!/usr/bin/env bash
set -Eeuo pipefail

run_id=${1:?run ID is required}
image_root=${RESMAN_IO_EFFECT_IMAGE_ROOT:-/var/lib/libvirt/images}
vm_name=resman-iow-effect-$run_id
work_dir=$image_root/$vm_name

[[ $run_id =~ ^r[0-9]{14}-[0-9]+$ ]] || { echo "unsafe run ID: $run_id" >&2; exit 2; }
[[ $image_root == /* && $image_root != / ]] || { echo "unsafe image root: $image_root" >&2; exit 2; }
[[ $work_dir == "$image_root"/resman-iow-effect-r*-* ]] \
	|| { echo "unsafe work directory: $work_dir" >&2; exit 2; }

if virsh dominfo "$vm_name" >/dev/null 2>&1; then
	state=$(virsh domstate "$vm_name" 2>/dev/null || true)
	if [[ $state != 'shut off' ]]; then
		virsh destroy "$vm_name" >/dev/null 2>&1 || true
	fi
	virsh undefine "$vm_name" --nvram >/dev/null 2>&1 \
		|| virsh undefine "$vm_name" >/dev/null 2>&1
fi

if [[ -d $work_dir ]]; then
	for name in root.qcow2 effect-a.raw effect-b.raw; do
		path=$work_dir/$name
		[[ ! -e $path && ! -L $path ]] || unlink -- "$path"
	done
	rmdir -- "$work_dir"
fi

[[ -z $(virsh list --all --name | grep -Fx "$vm_name" || true) ]] \
	|| { echo "owned domain survived cleanup: $vm_name" >&2; exit 1; }
[[ ! -e $work_dir && ! -L $work_dir ]] \
	|| { echo "owned work directory survived cleanup: $work_dir" >&2; exit 1; }

printf 'PASS: removed exact weighted-I/O QEMU run %s\n' "$run_id"
