"""Authoritative nq6 acceptance rows; a supported subset is never the final gate."""
from dataclasses import dataclass


@dataclass(frozen=True)
class Row:
    scenario: str
    criteria: tuple
    checks: tuple = ()
    artifact: str = "source-binary"
    remote: bool = True


ROWS = (
    Row("systemd-native-lifecycle", (1, 2, 4, 6, 7, 8, 9), (
        "full-budget-rejection", "pam-sessions", "blackout-suppression", "blackout-observation",
        "native-plan", "resource-properties", "authority-split", "crash-reclaim", "graceful-stop", "root-only-release")),
    Row("systemd-native-proportional", (2, 5, 8), (
        "full-budget-rejection", "pam-sessions", "native-plan", "full-contention", "stale-control",
        "work-conserving-lending", "root-progress", "unchanged-membership", "unchanged-weights", "graceful-stop")),
    Row("systemd-native-reference-pinned-six", (5,)),
    Row("systemd-native-reference", (5,)),
    Row("systemd-native-coverage", (1, 2, 3, 4, 8, 12), (
        "ssh-pam-root-no-pty", "ssh-pam-root-pty", "ssh-pam-user-no-pty", "ssh-pam-user-pty", "child-units",
        "resource-properties", "rootless-envelope", "nspawn-machine-split", "nspawn-machine-only", "nspawn-keep-unit",
        "nspawn-shifted-bounded", "hard-io-delivery")),
    Row("systemd-native-weighted-io-adapter", (4,), (
        "weighted-io-pam-sessions", "owned-null-block-device", "bfq-device-capability",
        "non-bfq-negative-capability", "adapter-weight-roundtrip", "exact-weight-restoration",
        "owned-device-cleanup"), "adapter-probe"),
    Row("systemd-native-recovery", (4, 8), (
        "recovery-pam-session", "crash-before-reload", "automatic-crash-recovery", "persistent-operator-conflict",
        "runtime-operator-conflict", "recreated-unit-recovery", "exact-recovery-cleanup"), artifact="adapter-probe"),
    Row("systemd-native-reconciliation", (2, 8), (
        "policy-reload", "concurrent-reconciliation", "topology-turnover", "cardinality-refusal", "online-cpu-change")),
    Row("missing-io-startup", (9,), ("missing-io-startup",), remote=False),
    Row("mcp-filter-reload", (9,), ("mcp-filter-reload",), remote=False),
    Row("psi-refresh-neutrality", (7, 9), ("psi-refresh-neutrality",)),
    Row("non-systemd-migration", (9,), ("non-systemd-namespace", "legacy-cpu-ingress", "legacy-live-restoration", "legacy-cleanup"), remote=False),
    Row("native-package-acceptance", (6, 10), (
        "installed-identity", "shipped-defaults", "schema-reset", "upgrade-750-rejected", "graceful-stop"),
        artifact="package"),
)

# Criterion 11 remains an independent approval after this executable matrix.
AUTOMATED_CRITERIA = frozenset(range(1, 13)) - {11}


def validate_catalog():
    if len({row.scenario for row in ROWS}) != len(ROWS):
        raise ValueError("duplicate catalog row")
    if {criterion for row in ROWS for criterion in row.criteria} != AUTOMATED_CRITERIA:
        raise ValueError("acceptance coverage gap")
    if not all(len(row.checks) == len(set(row.checks)) for row in ROWS):
        raise ValueError("duplicate required check")
