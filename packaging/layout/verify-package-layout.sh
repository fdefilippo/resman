#!/bin/sh
set -eu

if [ "$#" -ne 1 ]; then
	echo "usage: $0 PACKAGE.deb|PACKAGE.rpm" >&2
	exit 2
fi

package=$1
if [ ! -f "$package" ]; then
	echo "package does not exist: $package" >&2
	exit 1
fi

assert_entry() {
	path=$1
	mode=$2
	owner=$3
	listing=$4
	if ! awk -F '\t' -v path="$path" -v mode="$mode" -v owner="$owner" \
		'$1 == path && $2 == mode && $3 == owner { found = 1 } END { exit !found }' "$listing"; then
		echo "package layout is missing: $path $mode $owner" >&2
		exit 1
	fi
}

assert_absent_path() {
	path=$1
	paths=$2
	if grep -Fqx -- "$path" "$paths"; then
		echo "package layout retained legacy path: $path" >&2
		exit 1
	fi
}

assert_absent_prefix() {
	prefix=$1
	paths=$2
	if grep -F -- "$prefix" "$paths" >/dev/null; then
		echo "package layout retained a legacy path beginning with: $prefix" >&2
		exit 1
	fi
}

listing=$(mktemp "${TMPDIR:-/tmp}/resman-package-layout.XXXXXX")
paths=$(mktemp "${TMPDIR:-/tmp}/resman-package-paths.XXXXXX")
trap 'rm -f -- "$listing" "$paths"' EXIT HUP INT TERM

case "$package" in
	*.deb)
		command -v dpkg-deb >/dev/null 2>&1 || {
			echo "dpkg-deb is required to verify a Debian package" >&2
			exit 1
		}
		dpkg-deb --contents "$package" |
			awk '{ path = $6; sub(/^\./, "", path); sub(/\/$/, "", path); print path "\t" $1 "\t" $2 }' >"$listing"
		dpkg-deb --fsys-tarfile "$package" |
			tar -tf - |
			awk '{ path = $0; sub(/^\./, "", path); sub(/\/$/, "", path); print path }' >"$paths"
		;;
	*.rpm)
		command -v rpm >/dev/null 2>&1 || {
			echo "rpm is required to verify an RPM package" >&2
			exit 1
		}
		rpm --query --package \
			--queryformat '[%{FILENAMES}\t%{FILEMODES:perms}\t%{FILEUSERNAME}/%{FILEGROUPNAME}\n]' \
			"$package" >"$listing"
		cut -f 1 "$listing" >"$paths"
		;;
	*)
		echo "unsupported package type: $package" >&2
		exit 2
		;;
esac

assert_entry '/etc/resman' 'drwx------' 'root/root' "$listing"
assert_entry '/etc/resman/resman.conf' '-rw-------' 'root/root' "$listing"
assert_entry '/etc/resman/tls' 'drwx------' 'root/root' "$listing"
assert_entry '/var/lib/resman' 'drwx------' 'root/root' "$listing"
assert_entry '/usr/share/doc/resman/UPGRADING.md' '-rw-r--r--' 'root/root' "$listing"

assert_absent_path '/etc/resman.conf' "$paths"
assert_absent_path '/etc/resman.conf.rpmsave' "$paths"
assert_absent_path '/etc/resman.conf.backup' "$paths"
assert_absent_path '/etc/resman.conf.tmp' "$paths"
assert_absent_prefix '/etc/resman.conf.backup_' "$paths"
assert_absent_path '/etc/resman/metrics.db' "$paths"

echo "package layout verified: $package"
