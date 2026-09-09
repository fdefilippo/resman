# Limit-hook execution

ResMan can dispatch a script, an HTTP request, or both after a user is newly
limited. Hook delivery is an observation and integration boundary: a failed,
timed-out, cancelled, or saturated delivery never rolls back an applied resource
limit.

## Executor bounds

`LIMIT_HOOK_MAX_CONCURRENCY` creates a fixed worker pool (default `2`) and
`LIMIT_HOOK_QUEUE_CAPACITY` bounds pending deliveries (default `64`). Admission is
non-blocking. A full queue rejects the new delivery with the terminal `saturated`
outcome; ResMan does not retry or persist it, and the control cycle continues.

Script and HTTP mechanisms are separate jobs. Each job receives its own complete
`LIMIT_HOOK_TIMEOUT` deadline, so time consumed by one mechanism is never deducted
from the other. The HTTP client is private to the hook executor, performs no retry,
and bounds connection, TLS-handshake, response-header, and complete-request time.

The worker count and queue capacity are restart-required. Hook enablement, script
path, URL, and per-delivery timeout are dynamic. Script user and group are also
restart-required: a dynamic script-path change is accepted only when the running
instance already has a valid script identity.

## Script identity and trust boundary

When `LIMIT_HOOK_SCRIPT` is set, both `LIMIT_HOOK_SCRIPT_USER` and
`LIMIT_HOOK_SCRIPT_GROUP` are required. They must resolve exactly through NSS and
must not select UID 0 or GID 0. The two identity keys are rejected when no script is
configured, because an inert security setting is misleading.

The script path must be absolute, clean, regular, and executable by that identity.
Neither the script nor an ancestor may be a symbolic link. Every component must be
owned by root or the configured script user and must not be writable by group or
other users; a root-owned sticky ancestor such as `/tmp` is the only writable
ancestor exception. ResMan checks the path during configuration validation and again
immediately before execution.

The script starts with the configured UID and GID and an empty supplementary-group
list. Its working directory is `/`. ResMan supplies only this base environment:

- `PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin`
- `LANG=C`, `LC_ALL=C`, `HOME=/`, `PWD=/`
- `USER` and `LOGNAME`, both set to `LIMIT_HOOK_SCRIPT_USER`
- the documented `RESMAN_LIMIT_*` event variables listed in `resman(8)`

The daemon environment is not inherited. A shell interpreter can create its own
shell-local variables after startup; they do not originate from ResMan's environment.
Script stdout and stderr are discarded.

## Constraints inherited from the service sandbox

The script runs inside the mount namespace of `resman.service`, so the packaged unit
constrains it as well as the daemon:

- `ProtectHome=yes` hides `/home`, `/root`, and `/run/user`. A hook script placed
  below any of them is unreachable; install it under a system path such as
  `/usr/local/libexec`.
- `PrivateTmp=yes` gives the service its own `/tmp` and `/var/tmp`. A hook can still
  use them, but nothing it writes there is visible to other processes on the host,
  and files another process left there are not visible to the hook.
- `ProtectSystem=full` mounts `/usr`, the boot directories, and `/etc` read-only,
  with `/etc/resman` as the single writable exception. A hook that writes below
  `/etc` outside that directory fails.
- The capability bounding set of the service is the upper bound for the hook. It
  carries no `CAP_SYS_ADMIN`, `CAP_NET_ADMIN`, `CAP_SYS_MODULE`, or `CAP_SYS_TIME`.

`NoNewPrivileges` is deliberately not set, so a hook may still invoke a setuid or
setgid helper. The shipped `resman-sendmail-hook.sh` example depends on that,
because mail submission on Enterprise Linux passes through a setgid helper.

Each script starts in a new process group. On timeout or daemon shutdown, ResMan
sends `TERM` to the group, waits for a bounded grace period, escalates to `KILL`,
waits for the direct child, and verifies that the group has drained before releasing
the worker. A script that exits while leaving ordinary descendants in the group is a
failed delivery and those descendants are terminated. This is not hard containment
of a malicious program that deliberately escapes its process group with `setsid` or
an equivalent mechanism; that guarantee would require a separate systemd transient
unit or cgroup/`TasksMax` design.

## HTTP delivery

`LIMIT_HOOK_URL` receives one JSON `POST` per accepted HTTP job. The request body is
the typed event described in `resman(8)`. ResMan uses no proxy inherited from the
daemon environment, never uses `http.DefaultClient`, and never retries. Logs retain
only the URL scheme and host (including an explicit port); user information, path,
query, and fragment are excluded.

## Shutdown and observability

Shutdown first closes admission, then cancels in-flight and queued jobs and drains
the fixed worker pool. Every accepted or rejected delivery records exactly one
terminal value in:

```text
resman_limit_hook_executions_total{hook_type,outcome}
```

The bounded outcomes are `success`, `failure`, `timeout`, `cancelled`, and
`saturated`. Current executor occupancy is exported through:

```text
resman_limit_hook_in_flight
resman_limit_hook_queue_depth
resman_limit_hook_queue_capacity
```

Saturation warnings are rate-limited and contain neither a user identifier nor an
endpoint. Size `DAEMON_SHUTDOWN_TIMEOUT` to include the hook cancellation and process-
group drain budget, and keep systemd's `TimeoutStopSec` larger than that daemon-wide
deadline.

## Example

```ini
LIMIT_HOOK_ENABLED=true
LIMIT_HOOK_SCRIPT=/usr/local/libexec/resman/user-limited
LIMIT_HOOK_SCRIPT_USER=resman-hook
LIMIT_HOOK_SCRIPT_GROUP=resman-hook
LIMIT_HOOK_URL=https://hooks.example.internal/resman
LIMIT_HOOK_TIMEOUT=10
LIMIT_HOOK_MAX_CONCURRENCY=2
LIMIT_HOOK_QUEUE_CAPACITY=64
```

Install the script and all of its non-system ancestors under the ownership and mode
rules above before starting ResMan. A URL-only configuration needs no script identity.

## Packaged email example

The packages include `resman-sendmail-hook.sh`, a no-argument adapter for this hook
contract, and `sendmail.sh`, a separate generic SMTP helper. ResMan must invoke the
adapter, not the generic helper. The adapter reads the fixed `RESMAN_LIMIT_*`
environment and converts it into a subject and text body before delegating delivery.

Copy the adapter from `/usr/share/doc/resman/scripts/` to a trusted executable path
and configure that copy; never edit the package-owned example in place. See
`/usr/share/doc/resman/scripts/README.md` for the complete procedure and the explicit
restriction against placing SMTP passwords on a command line.
