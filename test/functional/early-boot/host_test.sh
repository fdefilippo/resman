#!/usr/bin/env bash
# Exercise the early-boot harness contract without starting a virtual machine.
set -Eeuo pipefail

script_dir=$(CDPATH='' cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)

PYTHONDONTWRITEBYTECODE=1 python3 -I "$script_dir/validate_evidence_test.py"

# The fixture must stand in for Veritas exactly: the name the packaged unit
# orders before, and no CPUSchedulingPolicy, because had requests SCHED_FIFO
# itself and that is what the automatic detection cannot see.
grep -q "cat >/etc/systemd/system/vcs.service" "$script_dir/qemu-host.sh"
if sed -n '/^\[Unit\]$/,/^UNIT$/p' "$script_dir/qemu-host.sh" | grep -q CPUSchedulingPolicy; then
	echo "the vcs.service fixture must not declare a CPUSchedulingPolicy" >&2
	exit 1
fi
grep -q 'os.sched_setscheduler(0, os.SCHED_FIFO' "$script_dir/qemu-host.sh"

# Every scenario the validator expects is the one the guest runs, with the same
# declaration.
for scenario in 'protected-refusal systemd_native vcs.service' \
	'protected-observation auto vcs.service' "consented-enforcement systemd_native ''"; do
	grep -qF "run_scenario $scenario" "$script_dir/qemu-host.sh" \
		|| { echo "qemu-host.sh does not run: $scenario" >&2; exit 1; }
done

# The pinned template digests agree between the host runner and the validator.
while IFS= read -r digest; do
	grep -q "$digest" "$script_dir/validate_evidence.py" \
		|| { echo "validator does not pin template digest $digest" >&2; exit 1; }
done < <(grep -oE 'base_sha256=[0-9a-f]{64}' "$script_dir/qemu-host.sh" | cut -d= -f2)

# Every retained archive must stay intact and keep the verdict it was reviewed
# with, and together they must cover the three platforms.
archives=$(find "$script_dir/evidence" -mindepth 1 -maxdepth 1 -type d | sort)
[[ -n $archives ]] || { echo "no retained early-boot archive" >&2; exit 1; }
platforms=()
while IFS= read -r archive; do
	(cd "$archive" && sha256sum --quiet --check SHA256SUMS)
	(cd "$archive/remote" && sha256sum --quiet --check SHA256SUMS)
	grep -q '"verdict": "QUALIFIED"' "$archive/verdict.json" \
		|| { echo "retained archive is not qualified: $archive" >&2; exit 1; }
	grep -qE '"package_identity": "resman-[0-9.]+-[0-9]+\.el8(\.[a-z0-9]+)?\.x86_64"' "$archive/verdict.json"
	platforms+=("$(sed -n 's/^  "platform": "\(el[0-9]*\)",$/\1/p' "$archive/verdict.json")")
done <<<"$archives"
for platform in el8 el9 el10; do
	printf '%s\n' "${platforms[@]}" | grep -qx "$platform" \
		|| { echo "no retained archive qualifies $platform" >&2; exit 1; }
done

echo "PASS: early-boot harness contract"
