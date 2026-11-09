# Kernel pressure observations and optional memory wakeups

ResMan reads `/proc/pressure/cpu`, `/proc/pressure/io` and
`/proc/pressure/memory` during each decision sample, whether or not
`PSI_EVENT_DRIVEN` is enabled. These are observational kernel values, not workload
attribution or an additional activation/release dimension. Host-only refreshes do
not replace the pressure snapshot or renew its timestamp. During blackout the
previous snapshot ages; no decision samples are invented.

The [kernel PSI interface](https://docs.kernel.org/accounting/psi.html) reports
`some` pressure when at least some tasks stall and `full` when all non-idle tasks
stall together, with percent averages over 10, 60 and 300 seconds. Host CPU `full`
is undefined, even on kernels that report a compatibility zero, so ResMan omits it.
An absent optional `full` line is also unavailable rather than a measured zero.

The shared parser validates all required fields, rejects duplicate fields or lines,
nonfinite/out-of-range averages and invalid totals, and retains exact `uint64`
stall totals. An unreadable or malformed source does not stop enforcement; the
failed observation is explicit and its prior percentage series are removed.

## Metrics

All labels have a fixed inventory; `scope` is `system`:

- `resman_psi_pressure_percent{type,scope,stall,window_seconds}`: measured percent,
  `type=cpu|io|memory`, `stall=some|full`, `window_seconds=10|60|300`.
- `resman_psi_pressure_available{type,scope,stall}`: whether that observation/mode
  is available. CPU full always reports unavailable.
- `resman_psi_pressure_sample_timestamp_seconds{type,scope}`: time of the last
  decision-owned sampling attempt. Use it with availability to judge freshness.
- `resman_psi_pressure_source_status{type,scope,reason}`: bounded one-hot status
  `none`, `read_error` or `invalid_data` for the last attempt.

No percentage series means unavailable or not yet sampled. It does not mean zero.
These gauges are independent of the existing `resman_psi_events_total` counter:
events describe registered threshold wakeups, not periodic pressure measurements.
The operations dashboard provides both, with source availability and sample age.

## Optional memory wakeups

```ini
PSI_EVENT_DRIVEN=true
PSI_MEMORY_EVENT_DRIVEN=true
PSI_MEMORY_STALL_THRESHOLD=50000
PSI_WINDOW_US=1000000
```

Memory wakeups are disabled by default. When enabled, the daemon registers an owned
`some` trigger on root `memory.pressure` or the `/proc/pressure/memory` fallback,
using the same watcher, window and cancellation lifecycle as CPU and I/O. The
memory threshold must be positive and not exceed the window. Changes to mode,
threshold or window rebuild monitors during configuration reload. A missing
optional wake source leaves the available resources monitored and readiness
explicitly degraded; if no source can be monitored, regular polling continues.

The event trigger is `psi_system_memory`. It wakes the existing control cycle;
resource eligibility, attribution, thresholds, hold times and authority still
control every RAM action. Pressure alone neither requests a limit nor selects a
victim. ResMan remains complementary to `systemd-oomd`, which owns its configured
emergency victim-selection policy.

PSI registration uses the existing kernel capability contract, including
`CAP_SYS_RESOURCE` in the packaged service. No extra privilege is introduced.
Fixture tests cover parsing, publication, reload and watcher lifecycle; a fixture
is not evidence that a particular deployed kernel or sandbox supports triggers.
