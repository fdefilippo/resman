#!/usr/bin/env bash

set -euo pipefail

if (( $# != 2 )); then
	echo "usage: $0 RPMBUILD_DIR OUTPUT_DIR" >&2
	exit 2
fi

rpm_build_dir=$1
output_dir=$2
binary_dir=$rpm_build_dir/RPMS
source_dir=$rpm_build_dir/SRPMS

if [[ ! -d $binary_dir || ! -d $source_dir ]]; then
	echo "RPM build did not produce both RPMS and SRPMS below $rpm_build_dir" >&2
	exit 1
fi

shopt -s globstar nullglob
binary_packages=()
for package in "$binary_dir"/**/resman-*.rpm; do
	if [[ -f $package && $package != *.src.rpm ]]; then
		binary_packages+=("$package")
	fi
done
source_packages=()
for package in "$source_dir"/**/resman-*.src.rpm; do
	if [[ -f $package ]]; then
		source_packages+=("$package")
	fi
done

if (( ${#binary_packages[@]} != 1 || ${#source_packages[@]} != 1 )); then
	echo "expected exactly one binary RPM and one source RPM below $rpm_build_dir; found ${#binary_packages[@]} binary and ${#source_packages[@]} source packages" >&2
	exit 1
fi

if [[ -e $output_dir ]]; then
	if [[ ! -d $output_dir ]]; then
		echo "release output path is not a directory: $output_dir" >&2
		exit 1
	fi
	shopt -s dotglob
	output_entries=("$output_dir"/*)
	if (( ${#output_entries[@]} != 0 )); then
		echo "release output directory is not empty: $output_dir" >&2
		exit 1
	fi
fi

mkdir -p "$output_dir"
cp -- "${binary_packages[0]}" "${source_packages[0]}" "$output_dir/"

printf 'Collected binary RPM: %s\n' "${binary_packages[0]}"
printf 'Collected source RPM: %s\n' "${source_packages[0]}"
