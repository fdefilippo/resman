# Installing ResMan

This guide covers the supported platforms, building ResMan and its packages,
installation, service behaviour and host preparation. Configuration keys, defaults
and reload rules are in [CONFIGURATION.md](CONFIGURATION.md); breaking changes
between releases are in [UPGRADING.md](UPGRADING.md).

## Requirements

- Linux kernel 4.18 or later with the unified cgroup v2 hierarchy
- systemd; Enterprise Linux 8 with systemd 239 is supported
- Enterprise Linux 8 or later for the published RPM packages
- Go 1.27.1 or later, with CGO enabled for LDAP/NIS username resolution, to build
  from source
- Debian package tools (`dpkg-dev`) when building `.deb` packages

## Build

```bash
# Development build
make build

# RPM package
make rpm

# Native Debian/Ubuntu package (amd64 or arm64)
make deb
# Creates build/deb/resman_1.41.2-1_<architecture>.deb

# All packages
make all-with-packages
```

CGO must be enabled for LDAP/NIS support:

```bash
CGO_ENABLED=1 go build -v -ldflags="-s -w" -o resman .
```

Release RPMs are native CGO builds produced separately on Enterprise Linux 8,
9, and 10. Each package carries its matching distribution tag and is checked
against that platform's glibc baseline. The RPMs are not static binaries,
because static builds cannot preserve the required NSS/LDAP/SSSD user resolution
behavior.

The `.deb` is a native CGO build. `dpkg-shlibdeps` records the actual minimum
runtime library versions, so release artifacts should be built on the oldest
Debian or Ubuntu baseline that the release intends to support.

## Enterprise Linux 8 and systemd 239

EL8 enforcement requires the unified cgroup v2 hierarchy. ResMan supports the
systemd 239 contract in which `ControlGroup` and `InvocationID` are available but
`ControlGroupId` is absent: it derives the cgroup identity from the validated
kernel path and confirms it around every unit read. Before announcing
`systemd_native`, startup proves apply, kernel readback, and exact restoration on
a disposable slice for every enabled resource. See
[Cgroup v2 technical reference](CGROUP-V2-TECHNICAL.md) for the complete
capability contract and [Upgrading](UPGRADING.md) for host preparation.

The evidence boundaries are intentionally separate. The EL8 container build
proves the glibc and package baseline; the EL8 QEMU qualification proves the exact
RPM against systemd 239 and genuine PAM sessions; the Oracle Linux 9 SmolVM and
real-kernel rows prove the newer-systemd behavior. Passing one row never
substitutes for another.

## Install

```bash
# From packages
sudo rpm -ivh resman-*.rpm
# or
sudo apt install ./resman_*.deb

# From source
sudo cp resman /usr/bin/
sudo install -d -m 0700 /etc/resman /var/lib/resman
sudo install -m 0600 config/resman.conf.example /etc/resman/resman.conf
sudo install -m 0600 config/cpu-points.map.example /etc/resman/cpu-points.map
sudo cp packaging/systemd/resman.service /usr/lib/systemd/system/
sudo systemctl enable --now resman
```

Package installation does not enable or start the service automatically. Review
`/etc/resman/resman.conf`, then use `systemctl enable --now resman`. During an upgrade,
an already active service is restarted after the new package is configured.

Before installing a new release over an older one, complete the filesystem, database,
configuration, MCP, Prometheus, hook, capability, and container actions in
[UPGRADING.md](UPGRADING.md).

RPM and Debian packages also ship a no-argument email adapter example at
`/usr/share/doc/resman/scripts/resman-sendmail-hook.sh`. Copy and configure it before
use; the adjacent generic `sendmail.sh` requires command-line arguments and is not
itself a ResMan hook. See [scripts/README.md](scripts/README.md).

## Service behaviour

The packaged unit does not retry configuration, required cgroup-capability, or MCP TLS
credential rejections: these exit with status 78 and remain failed until the operator
fixes the cause. Failures reading or writing setup state for an otherwise present
cgroup capability, including `cgroup.subtree_control`, remain transient. They and other
failures are retried after 10 seconds, with at most three starts per minute; after the
start limit is reached, use `systemctl reset-failed resman` after correcting the cause.
See `man resman` and `journalctl -u resman` for diagnostics.

Dynamic fields are reloaded automatically. Restart the service after changing a field
that `config/resman.conf.example` marks "Requires restart" and
[CONFIGURATION.md](CONFIGURATION.md) lists as `restart-required`, such as the cgroup
observation root, the Prometheus listener, TLS and authentication settings, logging
backend settings, or MCP listener and security settings:

```bash
sudo systemctl restart resman
```

Monitor the service:

```bash
sudo systemctl status resman
journalctl -u resman -f
curl -s http://localhost:1974/metrics | grep resman
```

## Enabling PSI

Event-driven control cycles (`PSI_EVENT_DRIVEN=true`) need Pressure Stall Information,
available from Linux 4.20 with `CONFIG_PSI=y`. CPU pressure files that expose only the
`some` line, as on older kernels with PSI backports, are supported; the `full` line is
treated as optional.

On Red Hat Enterprise Linux 8 and later, PSI may be compiled in but disabled at
boot. Enable it with:

```bash
sudo grubby --update-kernel=ALL --args="systemd.unified_cgroup_hierarchy=1 psi=1"
sudo reboot
```

Red Hat documents the performance impact of enabling PSI as slight (<1%).
After reboot, verify that PSI is active with `ls /proc/pressure`. If PSI files
are still unavailable, resman falls back to the normal polling loop.
