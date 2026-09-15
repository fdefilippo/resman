#!/usr/bin/env bash
set -Eeuo pipefail

remote_host=${1:?usage: remote-qemu.sh ROOT_AT_QEMU_HOST RPM BUILD_MANIFEST}
package=${2:?usage: remote-qemu.sh ROOT_AT_QEMU_HOST RPM BUILD_MANIFEST}
build_manifest=${3:?usage: remote-qemu.sh ROOT_AT_QEMU_HOST RPM BUILD_MANIFEST}
script_dir=$(CDPATH='' cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
repo_root=$(CDPATH='' cd -- "$script_dir/../../.." && pwd)
evidence_root=${IODEVICEWEIGHT_EFFECT_EVIDENCE_ROOT:-$repo_root/build/functional/io-device-weight-effect}
run_id=r$(date -u +%Y%m%d%H%M%S)-$$
source_revision=$(git -C "$repo_root" rev-parse HEAD)
source_tree=$(git -C "$repo_root" rev-parse 'HEAD^{tree}')
remote_root=/tmp/resman-iow-effect-$run_id
evidence_dir=$evidence_root/$run_id
scratch_dir=
remote_started=0

case "$remote_host" in root@*[A-Za-z0-9.-]) ;; *) echo "a root SSH target is required" >&2; exit 2 ;; esac
[[ -f $package && ! -L $package && -f $build_manifest && ! -L $build_manifest ]] \
	|| { echo "regular RPM and build manifest are required" >&2; exit 2; }
[[ -z $(git -C "$repo_root" status --porcelain) ]] \
	|| { echo "the worktree must be clean" >&2; exit 1; }
[[ $(awk -F= '$1 == "source_revision" {print $2}' "$build_manifest") == "$source_revision" && \
	$(awk -F= '$1 == "source_tree" {print $2}' "$build_manifest") == "$source_tree" && \
	$(awk -F= '$1 == "package_sha256" {print $2}' "$build_manifest") == "$(sha256sum "$package" | awk '{print $1}')" ]] \
	|| { echo "build manifest does not identify HEAD and the supplied RPM" >&2; exit 1; }
[[ $remote_root == /tmp/resman-iow-effect-r*-* ]] || { echo "unsafe remote root" >&2; exit 2; }

cleanup() {
	local status=$? cleanup_status=PASS final_result=FAIL
	trap - EXIT INT TERM
	set +e
	if [[ $remote_started -eq 1 ]]; then
		mkdir -p "$evidence_dir/remote"
		ssh -q -o BatchMode=yes "$remote_host" \
			"test ! -d '$remote_root/evidence' || tar -C '$remote_root/evidence' -cf - ." \
			| tar --no-same-owner -C "$evidence_dir/remote" -xf - \
			>>"$evidence_dir/collect.log" 2>&1 || { status=1; cleanup_status=FAIL; }
		ssh -q -o BatchMode=yes "$remote_host" "rm -rf -- '$remote_root'" \
			>>"$evidence_dir/collect.log" 2>&1 || { status=1; cleanup_status=FAIL; }
	fi
	if [[ -n $scratch_dir ]]; then
		case "$scratch_dir" in
			"${TMPDIR:-/tmp}"/resman-iow-effect.*) rm -rf -- "$scratch_dir" ;;
			*) status=1; cleanup_status=FAIL ;;
		esac
	fi
	if [[ $status -eq 0 ]]; then
		python3 "$script_dir/validate_evidence.py" "$evidence_dir/remote/guest" \
			--revision "$source_revision" --package-sha "$(sha256sum "$package" | awk '{print $1}')" \
			>>"$evidence_dir/collect.log" 2>&1 && final_result=PASS || status=1
	fi
	printf 'cleanup=%s\nresult=%s\nexit_code=%d\n' "$cleanup_status" "$final_result" "$status" \
		>>"$evidence_dir/request.txt"
	printf '%s\n' "$final_result" >"$evidence_dir/result"
	(
		cd "$evidence_dir"
		manifest_tmp=$evidence_root/effect-SHA256SUMS.tmp
		find . -type f ! -name SHA256SUMS -print0 | sort -z | xargs -0 sha256sum -- >"$manifest_tmp"
		mv "$manifest_tmp" SHA256SUMS
	)
	exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

scratch_dir=$(mktemp -d "${TMPDIR:-/tmp}/resman-iow-effect.XXXXXX")
mkdir -p "$evidence_dir" "$scratch_dir/bundle"
chmod 0700 "$evidence_dir" "$scratch_dir/bundle"
install -m 0600 "$package" "$scratch_dir/bundle/package.rpm"
install -m 0600 "$build_manifest" "$scratch_dir/bundle/build-manifest.txt"
install -m 0755 "$script_dir/qemu-host.sh" "$scratch_dir/bundle/qemu-host.sh"
install -m 0755 "$script_dir/guest_effect.py" "$scratch_dir/bundle/guest_effect.py"
install -m 0755 "$script_dir/validate_evidence.py" "$scratch_dir/bundle/validate_evidence.py"

{
	printf 'run_id=%s\nsource_revision=%s\nsource_tree=%s\nremote_host=%s\n' \
		"$run_id" "$source_revision" "$source_tree" "$remote_host"
	printf 'package_sha256=%s\n' "$(sha256sum "$package" | awk '{print $1}')"
} >"$evidence_dir/request.txt"

tar -C "$scratch_dir/bundle" -cf - . | ssh -q -o BatchMode=yes "$remote_host" \
	"test ! -e '$remote_root' && install -d -m 0700 '$remote_root' && tar --no-same-owner -C '$remote_root' -xf -"
remote_started=1
ssh -q -o BatchMode=yes "$remote_host" \
	"'$remote_root/qemu-host.sh' '$run_id' '$source_revision' '$source_tree'"
