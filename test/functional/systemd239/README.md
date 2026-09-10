# EL8 systemd 239 contract characterization

This fixture records the D-Bus and cgroup surface that the native enforcement
adapter consumes on Enterprise Linux 8. It is separate from binary ABI checks
and from the Oracle Linux 9 SmolVM functional suite.

`collect.sh` is deliberately read-only with respect to systemd and cgroups. It
requires an already active `user.slice`, writes only to a caller-owned output
directory, and refuses to start units or change properties. Run it inside a
disposable EL8 systemd 239 guest:

```bash
install -d -m 0700 /tmp/resman-systemd239-contract
RESMAN_CAPTURE_IMAGE=registry.access.redhat.com/ubi8/ubi:8.10 \
RESMAN_CAPTURE_IMAGE_ID=sha256:... \
RESMAN_CAPTURE_IMAGE_DIGEST=sha256:... \
RESMAN_CAPTURE_SOURCE_REVISION="$(git rev-parse HEAD)" \
	RESMAN_CAPTURE_ENVIRONMENT='disposable SmolVM guest' \
	test/functional/systemd239/collect.sh /tmp/resman-systemd239-contract
sha256sum -c /tmp/resman-systemd239-contract/SHA256SUMS
```

The checked contract at
`internal/systemdunit/testdata/el8-systemd239-contract.json` was captured on
2026-09-10 from a disposable SmolVM guest built from Red Hat UBI 8.10, with
`systemd-239-82.el8_10.19.x86_64` and a cgroup v2 kernel. The capture proves
that all scalar and per-device properties used by the adapter are present with
the expected D-Bus signatures, while `ControlGroupId` is absent. It also pins
the manager method signatures used for discovery, runtime property writes,
the bounded startup probe, restoration, and reload.

The raw collector output is retained below `evidence/`. Its manifest covers
the environment, package and kernel identity, cgroup capabilities, property
values, complete interface introspection, and terminal result. The Go contract
test recalculates that manifest and derives every checked signature from the
raw `busctl introspect` output before comparing it with the JSON fixture.

The production adapter must discover capabilities from the D-Bus members it
actually receives. The systemd package version is provenance, not a capability
switch.
