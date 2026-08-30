#!/bin/sh
set -eu

if [ "$#" -ne 5 ]; then
    echo "usage: $0 BINARY VERSION RELEASE ARCH PACKAGE_DIR" >&2
    exit 2
fi

binary=$1
version=$2
release=$3
arch=$4
package_dir=$5

script_dir=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
project_dir=$(CDPATH='' cd -- "$script_dir/../.." && pwd)

case "$arch" in
    amd64|arm64) ;;
    *)
        echo "unsupported Debian architecture: $arch" >&2
        exit 1
        ;;
esac

if [ ! -x "$binary" ]; then
    echo "resman binary not found or not executable: $binary" >&2
    exit 1
fi

case "$package_dir" in
    ""|"/")
        echo "refusing unsafe package directory: $package_dir" >&2
        exit 1
        ;;
esac

mkdir -p "$(dirname -- "$package_dir")"
package_parent=$(CDPATH='' cd -- "$(dirname -- "$package_dir")" && pwd)
package_dir="$package_parent/$(basename -- "$package_dir")"
shlibdeps_dir="${package_dir}.shlibdeps"

rm -rf -- "$package_dir" "$shlibdeps_dir"

install -d -m 0755 \
    "$package_dir/DEBIAN" \
    "$package_dir/etc/logrotate.d" \
    "$package_dir/etc/rsyslog.d" \
    "$package_dir/usr/bin" \
    "$package_dir/usr/lib/resman" \
    "$package_dir/usr/lib/systemd/system" \
    "$package_dir/usr/share/doc/resman" \
    "$package_dir/usr/share/lintian/overrides" \
    "$package_dir/usr/share/man/man8"
install -d -m 0700 \
    "$package_dir/etc/resman" \
    "$package_dir/etc/resman/tls" \
    "$package_dir/var/lib/resman"

install -m 0755 "$binary" "$package_dir/usr/bin/resman"
install -m 0600 "$project_dir/config/resman.conf.example" "$package_dir/etc/resman/resman.conf"
install -m 0644 "$project_dir/packaging/systemd/resman.service" \
    "$package_dir/usr/lib/systemd/system/resman.service"
install -m 0644 "$project_dir/packaging/syslog/resman" \
    "$package_dir/etc/logrotate.d/resman"
install -m 0644 "$project_dir/packaging/syslog/resman.conf" \
    "$package_dir/etc/rsyslog.d/resman.conf"

install -m 0644 "$project_dir/README.md" "$package_dir/usr/share/doc/resman/README.md"
install -m 0644 "$project_dir/docs/CONFIGURATION.md" \
    "$package_dir/usr/share/doc/resman/CONFIGURATION.md"
install -m 0644 "$project_dir/docs/UPGRADING.md" \
    "$package_dir/usr/share/doc/resman/UPGRADING.md"
install -m 0644 "$script_dir/copyright" "$package_dir/usr/share/doc/resman/copyright"
install -m 0644 "$project_dir/docs/alerting-rules.yml" \
    "$package_dir/usr/share/doc/resman/alerting-rules.yml"
install -m 0644 "$project_dir/docs/dashboard-grafana-operations.json" \
    "$package_dir/usr/share/doc/resman/dashboard-grafana-operations.json"
install -m 0755 "$project_dir/docs/generate-tls-certs.sh" \
    "$package_dir/usr/lib/resman/generate-tls-certs"
install -m 0644 "$script_dir/lintian-overrides" \
    "$package_dir/usr/share/lintian/overrides/resman"

install -m 0644 "$project_dir/docs/resman.8" "$package_dir/usr/share/man/man8/resman.8"
gzip -n -9 "$package_dir/usr/share/man/man8/resman.8"
install -m 0644 "$script_dir/changelog" "$package_dir/usr/share/doc/resman/changelog.Debian"
gzip -n -9 "$package_dir/usr/share/doc/resman/changelog.Debian"

install -m 0644 "$script_dir/conffiles" "$package_dir/DEBIAN/conffiles"
install -m 0755 "$script_dir/postinst" "$package_dir/DEBIAN/postinst"
install -m 0755 "$script_dir/prerm" "$package_dir/DEBIAN/prerm"
install -m 0755 "$script_dir/postrm" "$package_dir/DEBIAN/postrm"

mkdir -p "$shlibdeps_dir/debian"
cat >"$shlibdeps_dir/debian/control" <<EOF
Source: resman
Section: admin
Priority: optional
Maintainer: Francesco Defilippo <francesco@defilippo.org>
Standards-Version: 4.6.2

Package: resman
Architecture: any
Description: resman dependency calculation
EOF

shlibs=$(
    cd "$shlibdeps_dir"
    dpkg-shlibdeps -O -S"$package_dir" -e"$package_dir/usr/bin/resman"
)
rm -rf -- "$shlibdeps_dir"
depends=${shlibs#shlibs:Depends=}
if [ -z "$depends" ] || [ "$depends" = "$shlibs" ]; then
    echo "unable to determine shared library dependencies" >&2
    exit 1
fi

installed_size=$(du -sk "$package_dir" | awk '{print $1}')
sed \
    -e "s/@VERSION@/$version-$release/" \
    -e "s/@ARCH@/$arch/" \
    -e "s/@INSTALLED_SIZE@/$installed_size/" \
    -e "s/@DEPENDS@/$depends, procps, systemd/" \
    "$script_dir/control.in" >"$package_dir/DEBIAN/control"

(
    cd "$package_dir"
    find . -type f ! -path './DEBIAN/*' -printf '%P\0' |
        sort -z |
        xargs -0 md5sum >DEBIAN/md5sums
)

if [ -n "${SOURCE_DATE_EPOCH:-}" ]; then
    case "$SOURCE_DATE_EPOCH" in
        *[!0-9]*)
            echo "SOURCE_DATE_EPOCH must be an integer" >&2
            exit 1
            ;;
    esac
    find "$package_dir" -print0 |
        xargs -0 touch --no-dereference --date="@$SOURCE_DATE_EPOCH"
fi
