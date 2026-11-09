package metrics

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/fdefilippo/resman/config"
	"github.com/fdefilippo/resman/internal/processactivity"
	"github.com/shirou/gopsutil/v3/cpu"
	"github.com/shirou/gopsutil/v3/host"
	"github.com/shirou/gopsutil/v3/process"
)

func processFixture(tb testing.TB, count int) string {
	tb.Helper()
	root := tb.TempDir()
	write := func(path, body string) {
		tb.Helper()
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			tb.Fatal(err)
		}
	}
	write(filepath.Join(root, "uptime"), "1000000.00 200000.00\n")
	write(filepath.Join(root, "stat"), "btime 1700000000\n")
	for i := 0; i < count; i++ {
		pid := 10000 + i
		dir := filepath.Join(root, strconv.Itoa(pid))
		if err := os.Mkdir(dir, 0700); err != nil {
			tb.Fatal(err)
		}
		fields := make([]string, 52)
		for j := range fields {
			fields[j] = "0"
		}
		fields[0] = "S"
		fields[1] = "1"
		fields[11] = "50000"
		fields[12] = "10000"
		fields[17] = "1"
		fields[19] = "10000"
		fields[20] = "4096000"
		fields[21] = "100"
		write(filepath.Join(dir, "stat"), fmt.Sprintf("%d (fixture job) %s\n", pid, strings.Join(fields, " ")))
		uid := 1000 + i%45
		write(filepath.Join(dir, "status"), fmt.Sprintf("Name:\tfixture\nState:\tS (sleeping)\nUid:\t%d\t%d\t%d\t%d\nGid:\t%d\t%d\t%d\t%d\nVmRSS:\t400 kB\nThreads:\t1\n", uid, uid, uid, uid, uid, uid, uid, uid))
		write(filepath.Join(dir, "statm"), "1000 100 50 10 0 20 0\n")
		write(filepath.Join(dir, "comm"), "fixture\n")
		write(filepath.Join(dir, "smaps_rollup"), "Pss: 200 kB\n")
		write(filepath.Join(dir, "io"), "read_bytes: 1000\nwrite_bytes: 1000\nsyscr: 10\nsyscw: 10\n")
		if err := os.Symlink("/usr/bin/fixture", filepath.Join(dir, "exe")); err != nil {
			tb.Fatal(err)
		}
	}
	return root
}
func fixtureCollector(tb testing.TB, root string) *Collector {
	tb.Helper()
	cfg := config.DefaultConfig()
	c, err := NewCollector(cfg)
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(c.Stop)
	c.processRoot = root
	c.processBootID = "fixture-boot"
	for i := 0; i < 45; i++ {
		c.usernameCache[1000+i] = fmt.Sprintf("fixture-%d", i)
		c.usernameCacheTime[1000+i] = time.Now()
	}
	return c
}

// BenchmarkProcessReadPath240 compares the removed legacy read sequence against
// the actual shared stat sequence. Fixture timings are not production qualification.
func BenchmarkProcessReadPath240(b *testing.B) {
	root := processFixture(b, 240)
	b.Setenv("HOST_PROC", root)
	for _, mode := range []string{"legacy", "shared_guarded"} {
		b.Run(mode, func(b *testing.B) {
			c := fixtureCollector(b, root)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				for j := 0; j < 240; j++ {
					pid := 10000 + j
					if mode == "legacy" {
						// A fixture PID is not a host process. Reproduce NewProcess's
						// eager CreateTime read directly, excluding its host-only
						// signal(0) liveness check from both measured sequences.
						p := &process.Process{Pid: int32(pid)}
						if _, err := p.CreateTime(); err != nil {
							b.Fatal(err)
						}
						_ = c.getProcessCPUUsageSimpleWithHandle(c.decisionState, p)
						_ = c.getProcessCPUAverage(p, 1000000)
						mem, err := p.MemoryInfo()
						if err != nil {
							b.Fatal(err)
						}
						_, _ = c.getProcessMemorySample(pid, mem.RSS, true)
					} else {
						stat, err := c.readProcessStat(root, pid, c.processClockTicks)
						if err != nil {
							b.Fatal(err)
						}
						_ = updateProcessCPUSample(c.decisionState, int32(pid), int64(stat.StartTicks), cpu.TimesStat{User: stat.UserSeconds, System: stat.SystemSeconds}, time.Now())
						_ = processCPUAverageFromSample(stat.UserSeconds+stat.SystemSeconds, 1000000-float64(stat.StartTicks)/c.processClockTicks, 60)
						_, _ = c.getProcessMemorySample(pid, stat.RSSBytes, true)
						guard, err := c.readProcessStat(root, pid, c.processClockTicks)
						if err != nil || guard.StartTicks != stat.StartTicks {
							b.Fatalf("guard=%+v err=%v", guard, err)
						}
					}
					if _, err := c.getProcessIO(pid); err != nil {
						b.Fatal(err)
					}
				}
			}
			b.ReportMetric(240, "processes/scan")
			if mode == "legacy" {
				b.ReportMetric(3, "stat_reads/process")
			} else {
				b.ReportMetric(2, "stat_reads/process")
			}
		})
	}
}
func BenchmarkCollectAllUserMetrics240(b *testing.B) {
	root := processFixture(b, 240)
	b.Setenv("HOST_PROC", root)
	c := fixtureCollector(b, root)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if got := c.collectAllUserMetrics(c.decisionState); len(got) != 45 {
			b.Fatalf("users=%d want45", len(got))
		}
	}
	b.ReportMetric(240, "processes/scan")
	b.ReportMetric(45, "users/scan")
}
func TestCollectorStatReadBudgetAndDiagnostics(t *testing.T) {
	for _, fallback := range []bool{false, true} {
		for _, failureAt := range []int{0, 1, 2} {
			t.Run(fmt.Sprintf("fallback=%v/fail=%d", fallback, failureAt), func(t *testing.T) {
				root := processFixture(t, 1)
				t.Setenv("HOST_PROC", root)
				c := fixtureCollector(t, root)
				var reads int
				c.decisionState.ema.values[1000] = 123
				c.decisionState.ema.enforceableValues[1000] = 123
				c.processStatReader = func(root string, pid int, ticks float64) (processactivity.ProcStat, error) {
					reads++
					if reads == failureAt {
						return processactivity.ProcStat{}, os.ErrPermission
					}
					return processactivity.ReadStat(root, pid, ticks)
				}
				if fallback {
					c.getAllUserMetricsFallback(c.decisionState)
				} else {
					c.collectAllUserMetrics(c.decisionState)
				}
				if failureAt == 0 {
					if reads != 2 {
						t.Fatalf("stat reads=%d want2", reads)
					}
					if err := c.DecisionProcessScanError(); err != nil {
						t.Fatal(err)
					}
				} else {
					if c.decisionState.ema.values[1000] != 123 || c.decisionState.ema.enforceableValues[1000] != 123 {
						t.Fatal("unreadable scan advanced EMA with a fabricated zero")
					}
					if c.DecisionProcessScanError() == nil {
						t.Fatal("unreadable source represented as complete zero")
					}
					if len(c.decisionState.process.prevProcCPU) != 0 || len(c.decisionState.process.prevProcIO) != 0 {
						t.Fatal("failed identity kept a stale baseline")
					}
				}
			})
		}
	}
}

// Legacy routines from baseline 0ed2ce4 are retained only to benchmark its read sequence.
// getProcessCPUAverage calculates the average CPU usage of a process since its start.
// Uses total CPU time (user + system) divided by process lifetime.
func (c *Collector) getProcessCPUAverage(p *process.Process, systemUptimeSeconds float64) float64 {
	// Get CPU times (total user + system time in seconds)
	times, err := p.Times()
	if err != nil || times == nil {
		return 0
	}
	totalCPUSeconds := times.User + times.System

	// Get process creation time (milliseconds since epoch)
	createTime, err := p.CreateTime()
	if err != nil || createTime == 0 {
		return 0
	}

	// Get system boot time (seconds since epoch)
	bootTime, err := host.BootTime()
	if err != nil || bootTime == 0 {
		return 0
	}

	// Process age in seconds = current uptime - (createTime/1000 - bootTime)
	processAgeSeconds := systemUptimeSeconds - (float64(createTime)/1000.0 - float64(bootTime))

	if processAgeSeconds <= 0 {
		return 0
	}
	minAgeSeconds := c.getConfig().GetProcessMinAgeSeconds()
	if minAgeSeconds > 0 && processAgeSeconds < float64(minAgeSeconds) {
		return 0
	}

	return calculateProcessCPUAverage(totalCPUSeconds, processAgeSeconds)
}

// getProcessCPUUsageSimpleWithHandle calculates CPU usage from consecutive samples.
// Every process state contributes after the initial baseline sample.
func (c *Collector) getProcessCPUUsageSimpleWithHandle(state *userMetricsSamplingState, proc *process.Process) float64 {
	pid32 := proc.Pid

	// Read current process CPU times.
	times, err := proc.Times()
	if err != nil || state == nil || state.process == nil {
		return 0
	}
	startTime, _ := proc.CreateTime()
	return updateProcessCPUSample(state, pid32, startTime, *times, time.Now())
}

// BenchmarkCollectCurrentProcReadOnly reads the current host process population.
// It performs no enforcement, database writes, or workload creation. Population
// and permission failures vary by host; this is not the representative fixture.
func BenchmarkCollectCurrentProcReadOnly(b *testing.B) {
	for _, workers := range []int{1, 4} {
		b.Run(strconv.Itoa(workers), func(b *testing.B) {
			cfg := config.DefaultConfig()
			c, err := NewCollector(cfg)
			if err != nil {
				b.Fatal(err)
			}
			b.Cleanup(c.Stop)
			c.processScanWorkers = workers
			pids, err := process.Pids()
			if err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.ResetTimer()
			var users, processes int
			for i := 0; i < b.N; i++ {
				values := c.collectAllUserMetrics(c.decisionState)
				users = len(values)
				processes = 0
				for _, v := range values {
					processes += v.ProcessCount
				}
			}
			b.ReportMetric(float64(len(pids)), "host_pids_at_start")
			b.ReportMetric(float64(users), "observed_users/scan")
			b.ReportMetric(float64(processes), "observed_processes/scan")
			if err := c.DecisionProcessScanError(); err != nil {
				b.Logf("source availability: %v", err)
			}
		})
	}
}

func BenchmarkCollectProcessPool240(b *testing.B) {
	root := processFixture(b, 240)
	b.Setenv("HOST_PROC", root)
	for _, workers := range []int{1, 4} {
		b.Run(strconv.Itoa(workers), func(b *testing.B) {
			c := fixtureCollector(b, root)
			c.processScanWorkers = workers
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if got := c.collectAllUserMetrics(c.decisionState); len(got) != 45 {
					b.Fatalf("users=%d", len(got))
				}
			}
			b.ReportMetric(240, "processes/scan")
			b.ReportMetric(45, "users/scan")
		})
	}
}
