# Read-only configuration and prerequisite checks

Run the check with the same environment and user identity intended for the daemon:

```sh
resman --check --config /etc/resman/resman.conf
# Equivalent Go flag forms:
resman -config=/etc/resman/resman.conf -check=true
```

The selected configuration must exist and be a non-symlink regular file. The
command shares startup defaults, file parsing, environment precedence, validation,
CPU Points map safety and exact NSS identity resolution, the CPU capacity
invariant, and the enabled weighted-I/O map loader. It also reads the online CPU
source and cgroup v2 controller inventory, checks existing SQLite storage metadata
with the startup rules, parses enabled TLS credentials, and reads enabled
Prometheus credential files. Native mode checks whether the systemd private socket
accepts a connection; it does not prove D-Bus authorization or enforcement.

Each result is explicitly classified:

- `verified`: the described read-only inspection succeeded in this process context.
- `invalid`: the inspection detected a configuration or prerequisite failure.
- `unverified`: proving the property would require operational work that this
  command deliberately does not perform.

Exit status is **0** when all performed inspections pass, **78** when a configuration
or inspected prerequisite is invalid, and **1** if the report cannot be written.
A successful check can contain unverified results; it is not a promise that startup
will pass every operational probe. Running from a shell does not reproduce the
systemd unit's capability restrictions, mount namespace or SELinux context.

No file logger is initialized, no database is created or opened, no schema or
retention work runs, no listener is bound, no systemd property or lease is changed,
and no PSI trigger or BPF program is loaded. Existing database metadata is inspected
without opening SQLite; schema compatibility, creation, locking and writes remain
unverified. Missing database storage is reported as unverified rather than created.
The command never repairs permissions or recovers a durable property lease.

The final list of restart-required keys comes directly from the configuration
lifecycle table. It contains key names only, not values, and describes which fields
require component reconstruction. It does not compare the file with a running
instance and therefore cannot say that a restart is currently necessary.
