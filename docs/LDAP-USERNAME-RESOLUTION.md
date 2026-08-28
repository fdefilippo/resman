# LDAP/NIS Username Resolution Guide

## Overview

ResMan resolves UIDs through the operating system NSS configuration. With a CGO-enabled
build, `os/user.LookupId` uses libc and therefore supports LDAP, NIS, SSSD, and other
NSS providers. A local `/etc/passwd` lookup is retained as a fallback.

The shipped builds use `CGO_ENABLED=1`. A static `CGO_ENABLED=0` binary cannot satisfy
this contract and is not a supported production build for directory-backed users.

## Prerequisites

1. Configure the host's NSS provider and authentication service.
2. Confirm that NSS can resolve the target users before starting ResMan.
3. Build ResMan with CGO and a working C toolchain.

Typical `/etc/nsswitch.conf` entries are:

```text
passwd: files sss
group:  files sss
shadow: files sss
```

For another NSS provider, replace `sss` with the module configured by the operating
system. ResMan does not connect directly to an LDAP server and has no LDAP URL,
bind-DN, or password setting of its own.

Verify the host first:

```bash
getent passwd ldap-user-01
id ldap-user-01
getent passwd 10001
```

These commands must return the expected account and UID. If they fail, correct NSS
before investigating ResMan.

## Building with CGO

Install a compiler and libc development headers appropriate for the distribution.

```bash
# Oracle Linux, RHEL, or compatible distributions
sudo dnf install gcc glibc-devel

# Debian or Ubuntu
sudo apt-get install build-essential libc6-dev
```

Build with the project toolchain:

```bash
CGO_ENABLED=1 /usr/local/go/bin/go build -o resman .
```

Confirm that the executable is dynamically linked to libc:

```bash
ldd ./resman
./resman --version
```

`ldd` should list libc. A result such as “not a dynamic executable” usually means the
binary was built without CGO and will not use the configured LDAP/NIS NSS module.

## ResMan configuration

No directory-service-specific ResMan key is required. Username resolution is automatic.
The relevant general settings are:

```ini
# Cache successful UID-to-name resolutions for this many minutes.
USERNAME_CACHE_TTL=60

# Only UIDs at or above the configured system boundary are observed as users.
SYSTEM_UID_MIN=1000
```

The exact public key set and defaults are in
[`config/resman.conf.example`](../config/resman.conf.example). Unknown keys are rejected,
so do not add ad-hoc LDAP settings to the ResMan file.

When resolution succeeds, metrics use the directory username:

```promql
resman_user_cpu_usage_percent{uid="10001",username="ldap-user-01"}
```

If both NSS and `/etc/passwd` resolution fail, ResMan uses the numeric UID string as the
display name. Eligibility based on username patterns may then differ, so numeric output
must be treated as an observable degradation rather than proof that the account is local.

## SSSD example

Install and configure SSSD according to the identity provider. A minimal shape is:

```ini
# /etc/sssd/sssd.conf
[sssd]
services = nss, pam
domains = example

[domain/example]
id_provider = ldap
auth_provider = ldap
ldap_uri = ldaps://ldap.example.com
ldap_search_base = dc=example,dc=com
cache_credentials = true
```

Protect the SSSD configuration, enable the service, and verify NSS:

```bash
sudo chmod 0600 /etc/sssd/sssd.conf
sudo systemctl enable --now sssd
getent passwd ldap-user-01
```

Then start or restart ResMan and query a per-user metric.

## Performance and caching

Directory lookups can block on DNS, TLS, or the identity provider. Use the provider's
own cache and timeout controls and keep `USERNAME_CACHE_TTL` high enough to avoid a
lookup on every collection. ResMan bounds its username cache and removes expired entries.

For SSSD, provider-specific settings such as `entry_cache_timeout` and LDAP operation
timeouts belong in `sssd.conf`, not in ResMan configuration.

## Troubleshooting

### Metrics show a numeric username

1. Run `getent passwd <uid>` as the same user and service context that runs ResMan.
2. Confirm the binary was built with `CGO_ENABLED=1` and links to libc.
3. Check `/etc/nsswitch.conf` for the configured provider.
4. Check SSSD, LDAP, NIS, DNS, and certificate logs.
5. Confirm the UID is at or above `SYSTEM_UID_MIN`.
6. Restart ResMan or wait for the username cache entry to expire after fixing NSS.

### Resolution is slow

- Measure `getent passwd <uid>` latency outside ResMan.
- Enable and tune the provider cache.
- Configure bounded DNS and directory-service timeouts.
- Verify that unreachable providers are not queried before the authoritative source.

### `user: unknown userid` appears

The NSS stack could not resolve the UID at that moment. Confirm that the user exists,
that the ResMan service account can read the necessary NSS configuration, and that the
identity provider is reachable. ResMan falls back to `/etc/passwd` and then to the UID
string; it does not invent a username.

## Final verification

```bash
getent passwd 10001
curl --fail --silent https://127.0.0.1:1974/metrics \
  --cacert /etc/resman/tls/ca.crt \
  | grep 'uid="10001"'
journalctl -u resman --since '10 minutes ago'
```

Adjust the Prometheus authentication arguments to match the deployment. The important
evidence is agreement between NSS, the metric's `uid`, and its `username` label.
