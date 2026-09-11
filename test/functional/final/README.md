# Native enforcement final gate

`make test-functional-final` runs the revision-bound `resman-nq6.9` matrix.
`catalog.py` is the authoritative set of required scenarios and check names.
The former observation-only eleven-row gate is not current native acceptance.
Its historical containment dispositions remain in the adjacent TSV as historical
inventory, not as an executable claim that native enforcement passed.

Every applicable row is mandatory. An absent capability or unavailable fixture is
`BLOCKED` (exit 77), an invalid proof or failed check is `FAIL` (exit 1), and only
a complete matrix can be `PASS`. Independent analyst review, criterion 11, remains
pending even after an automated PASS; the gate never closes an issue or epic.

There is **no overall nq6.9 PASS yet**. The new coverage, recovery,
reconciliation and package producers have local regression tests, but this does
not establish their field acceptance. Weighted I/O is explicitly **adapter-only**
under the approved `resman-nq6.39` decision. The daemon has no weighted-I/O policy;
that future product contract is deferred to `resman-nq6.40`. An adapter PASS cannot
be reported as a daemon delivery PASS.

## Evidence contract

Use a clean, frozen checkout throughout collection. A JSON manifest identifies the
exact current commit and expected artifacts, with supplied evidence directories:

```json
{
  "schema": 1,
  "source_revision": "FULL_CURRENT_GIT_COMMIT",
  "artifacts": {
    "source-binary": {
      "source_revision": "FULL_CURRENT_GIT_COMMIT",
      "binary_sha256": "SHA256_OF_THE_TESTED_SOURCE_BINARY"
    },
    "adapter-probe": {
      "source_revision": "FULL_CURRENT_GIT_COMMIT",
      "binary_sha256": "SHA256_OF_THE_ADAPTER_PROBE"
    },
    "package": {
      "source_revision": "FULL_CURRENT_GIT_COMMIT",
      "kind": "rpm",
      "identity": "EXACT_PACKAGE_IDENTITY",
      "sha256": "SHA256_OF_THE_RPM",
      "binary_sha256": "SHA256_OF_ITS_EXTRACTED_BINARY"
    }
  },
  "evidence": {
    "systemd-native-lifecycle": ["/absolute/path/to/lifecycle/evidence"],
    "systemd-native-reference-pinned-six": [
      "/absolute/path/to/replica-1/evidence",
      "/absolute/path/to/replica-2/evidence",
      "/absolute/path/to/replica-3/evidence"
    ]
  }
}
```

The example is intentionally incomplete and cannot pass. Supply every catalog row,
or let the configured disposable-host runner collect supported remote scenarios.
Unknown scenario names, stale revisions, unknown statuses, duplicate JSON keys,
missing check observations and artifact mismatches are errors. If separate builds
of one source revision have distinct Go build IDs, an artifact entry may use
`binaries`, a map from absolute evidence-directory paths to their exact binary
SHA-256, instead of one `binary_sha256`. Never infer a binary hash from the version
string. A package proof must exercise the package binary and bind both its package
identity and bytes; a source build or adapter test cannot satisfy that row.

```bash
FINAL_GATE_MANIFEST=/absolute/path/to/manifest.json \
RESMAN_REAL_KERNEL_HOST=root@terra make test-functional-final
```

Without a host, supplied evidence is validated read-only and unavailable rows are
recorded as BLOCKED. No package is built, installed or upgraded implicitly. Before
any new RPM/DEB build, follow the repository's fresh VERSION-RELEASE rule.

## Collecting individual rows

Run these commands sequentially against an explicitly disposable host; the runner
serializes host ownership. Keep the checkout frozen until each command finishes.

```bash
GO_BIN=/usr/local/go/bin/go test/functional/real-kernel/remote.sh systemd-native-coverage root@terra
GO_BIN=/usr/local/go/bin/go test/functional/real-kernel/remote.sh systemd-native-recovery root@terra
GO_BIN=/usr/local/go/bin/go test/functional/real-kernel/remote.sh systemd-native-reconciliation root@terra
```

The separate adapter row creates only its own disposable null_blk device, uses
cron/PAM rather than SSH authentication, and retains the exact compiled probe:

```bash
GO_BIN=/usr/local/go/bin/go test/functional/real-kernel/remote.sh systemd-native-weighted-io-adapter root@terra
```

Its required proof is IOWeight 100/2300 programmed through the real adapter,
BFQ kernel readback 100/300, per-device BFQ capability, a non-BFQ negative phase
without enabled per-device io.cost, and exact restoration. The matrix validates
`adapter_exercised=true`, `daemon_exercised=false`, probe identity and raw operation
evidence. Direct-read traffic shares are recomputed but remain CHARACTERIZATION,
without CPU tolerances or a ratio verdict. A prior standalone null_blk script
cannot satisfy the adapter row. See [NULL-BLOCK.md](../real-kernel/NULL-BLOCK.md).

The reconciliation command deliberately leaves `online-cpu-change` BLOCKED. Only after
explicit approval to change the disposable host's CPU topology, use:

```bash
REAL_KERNEL_ALLOW_CPU_HOTPLUG=1 GO_BIN=/usr/local/go/bin/go \
  test/functional/real-kernel/remote.sh systemd-native-reconciliation root@terra
```

Only `0` and `1` are accepted. The fixture records this intent, offlines only the
highest online nonzero CPU, and verifies restoration of the original online set
even after failure. It does not change CPU placement policy in ResMan.

Coverage requires working, pre-existing localhost SSH authentication and trusted
host keys for both root and the non-root fixture account, with and without PTY.
It never installs credentials or changes SSH/PAM. Prepared container images,
the nspawn root and usable I/O capabilities are separate prerequisites; see
[native fixture requirements](../real-kernel/NATIVE.md).

Package acceptance requires a freshly built RPM identity already explicitly
installed on the test host, plus the exact matching RPM file:

```bash
REAL_KERNEL_PACKAGE=/absolute/path/to/resman-1.36.6-2.el8.x86_64.rpm \
GO_BIN=/usr/local/go/bin/go \
  test/functional/real-kernel/remote.sh native-package-acceptance root@terra
```

This example names the current `1.36.6-2` release identity. Increment RELEASE before
any later build of the same VERSION, and do not treat this command as build or
installation authorization. The fixture verifies the installed identity and actual binary bytes
against the retained RPM payload, then exercises that package binary with isolated
configuration. It never substitutes a freshly compiled source binary.

The required non-systemd observation row uses a disposable SmolVM guest:

```bash
SMOLVM_SCENARIO=non-systemd-observation test/functional/smolvm/run.sh run
```

Its scope is a private PID/mount namespace with a non-systemd PID 1 inside the
guest, not proof from a physical host booted without systemd. The existing
`missing-io-startup` and `mcp-filter-reload` SmolVM rows remain independently
required, as does the real-host `psi-refresh-neutrality` row.

Each directory retains `result`, `environment.txt`, `environment.json`, and, for
check-bearing rows, `checks.json` plus a nonempty `<check>.json` observation for
every exact required check. Environment fields bind scenario, revision, running
kernel, host machine identity, run ID, tested artifact and verified cleanup.
The consumer recomputes file hashes into `matrix.json`; failures and blocked
attempts are never discarded or replaced by a successful subset.

## Measurement scope

The three fresh six-window reference runs and daemon run must share the same host,
running kernel and exact source revision. The consumer reopens raw frames, verifies
the runnable-leaf CPU-assignment multisets, checks every adjacent identity/counter
interval, and recomputes six 60-second windows and the lending interval. It retains
the original 0.5-point aggregate, 1.0-point leaf and 2.0-point stale-control bounds.
Root response and unchanged-journal observations are required as well.

A controlled-placement PASS is **not evidence for unbound workloads**. The unbound
reference row is mandatory `CHARACTERIZATION`, not delivery PASS, and its numerical
dispersion is reported without a delivery verdict. This scope is carried in both
the machine-readable matrix and summary, not just issue notes.

## Required coverage

Besides the lifecycle and proportional rows, the catalog requires real SSH/PAM
root and non-root sessions with and without PTY, child units, excluded and rootless
workloads, all three nspawn placements, hard I/O delivery, adapter-only weighted I/O,
adapter crash/conflict/recreation recovery, concurrent policy/topology reconciliation,
non-systemd migration, package upgrade/default/schema behavior, and the existing
missing-controller/MCP-reload/PSI-observation regression contracts. Missing runtime
images, genuine SSH sessions, package bytes or guest capabilities remain BLOCKED.
Weighted-I/O delivery by the daemon is not claimed or implied by its adapter's
programmed-property proof. Missing dedicated null_blk/BFQ capabilities leave the
adapter row BLOCKED; no existing system-disk scheduler is changed to obtain PASS.

Local whole-package tests use Go JSON events and must include actual passing test
events; a command that exits zero after selecting no tests cannot pass.
`make test-functional-final-unit` runs complete synthetic success plus mutations of
every required check, scope, artifacts, cleanup, status, raw frames and replicas,
without making any host changes.
