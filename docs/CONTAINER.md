# Supported container deployment

resman supports one container runtime contract: a rootful Podman container that
manages the host. It is not an isolated or rootless workload. The daemon must see
host PIDs, resolve host users through NSS, read trustworthy `/proc/PID/exe` and
`/proc/PID/io` entries for foreign users, and write the host cgroup v2 hierarchy.

The image is built with CGO on Oracle Linux 9 and includes the SSSD NSS client.
The process runs as UID 0. Rootless Podman, a private PID or cgroup namespace,
an unprivileged user, and a read-only cgroup mount are unsupported because they
cannot satisfy the resource-manager contract.

## Prepare the host

Create a configuration whose persistent paths point at the mounted directories:

```ini
CGROUP_ROOT=/sys/fs/cgroup
LOG_FILE=/var/log/resman/resman.log
CREATED_CGROUPS_FILE=/var/lib/resman/cgroups.txt
METRICS_DB_PATH=/var/lib/resman/metrics.db
```

Then create the state directories and build the image:

```bash
sudo install -d -m 0700 /var/lib/resman
sudo install -d -m 0750 /var/log/resman
make container-build
```

## Run it

This is the complete supported invocation for local host accounts:

```bash
sudo podman run --rm --name resman \
  --privileged \
  --pid=host \
  --cgroupns=host \
  --network=host \
  --security-opt label=disable \
  -v /sys/fs/cgroup:/sys/fs/cgroup:rw \
  -v /etc/resman.conf:/etc/resman.conf:ro \
  -v /etc/passwd:/etc/passwd:ro \
  -v /etc/group:/etc/group:ro \
  -v /etc/nsswitch.conf:/etc/nsswitch.conf:ro \
  -v /var/lib/resman:/var/lib/resman:rw \
  -v /var/log/resman:/var/log/resman:rw \
  resman:1.25.1
```

`--pid=host` makes `/proc` describe the processes resman controls.
`--cgroupns=host` and the writable `/sys/fs/cgroup` bind expose the hierarchy
where those processes live. `--privileged` supplies cgroup administration and
the ptrace-style read access required by `/proc/PID/exe` and `/proc/PID/io` for
other users. `--network=host` preserves the configured listener addresses; use
the Prometheus and MCP authentication/TLS settings exactly as on a native host.

The `/etc/passwd`, `/etc/group`, and `/etc/nsswitch.conf` binds make local host
accounts visible through the same CGO/NSS path used by native deployments. For
SSSD-backed accounts, also bind the host NSS socket:

```bash
  -v /var/lib/sss/pipes:/var/lib/sss/pipes:ro
```

Another NSS provider requires its matching client library in a derived image
and its host socket or configuration mounts. Do not claim LDAP/NIS resolution
from the three local-account mounts alone.

## Security and failure contract

This container has host-wide administrative access by design. Treat its image,
configuration, bearer tokens, TLS keys, and state directories like the native
root daemon.

`PROCESS_EXCLUDE_LIST` matches only the basename obtained from
`/proc/PID/exe`. `/proc/PID/comm` is retained only as a display name because a
process can rewrite it. If the executable identity cannot be read, resman keeps
that process in decision accounting and cgroup enforcement and emits an explicit
error; it never lets the writable `comm` value match an exclusion.

Verify a deployment before relying on it:

```bash
sudo podman exec resman getent passwd 1000
sudo podman exec resman readlink /proc/1234/exe
sudo podman exec resman cat /proc/1234/io
sudo podman exec resman test -w /sys/fs/cgroup/cgroup.subtree_control
```

Use a real foreign-user PID in both `/proc` commands and verify that the I/O
output contains `read_bytes`, `write_bytes`, `syscr`, and `syscw`. Failure of any check means
the runtime does not meet the supported contract and must not be treated as an
enforcement-capable deployment.

The reproducible product gate is:

```bash
make test-functional-smolvm-container-runtime
```

It runs the shipped image under rootful Podman inside a disposable SmolVM guest,
resolves a host fixture user, reads the foreign-user executable identity, and
observes a real finite `cpu.max` lifecycle. Exit status 77 is blocked evidence,
not a pass.
