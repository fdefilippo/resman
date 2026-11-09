package processactivity

import (
	"fmt"
	"math"
	"strings"
	"testing"
	"time"
)

func activitySample(at time.Time, cpu float64) Sample {
	return Sample{Identity: Identity{BootID: "boot", PID: 42, StartTicks: "123"}, UID: 1000, Executable: "/bin/job", ExecutableAvailable: true, SampledAt: at, RuntimeAvailable: true, RuntimeSeconds: 604800, CPUSeconds: cpu, CPUAvailable: true, MemoryBytes: 100, MemorySource: MemoryPSS}
}
func TestTrackerCountsOnlyAdjacentCoveredIntervals(t *testing.T) {
	now := time.Now()
	for _, tt := range []struct {
		name     string
		mutate   func(*Sample)
		gap      time.Duration
		wantCPU  float64
		wantWall float64
	}{
		{"covered", func(*Sample) {}, time.Second, 1, 1},
		{"long gap", func(*Sample) {}, 3 * time.Second, 0, 0},
		{"counter reset", func(s *Sample) { s.CPUSeconds = 0 }, time.Second, 0, 0},
		{"unavailable", func(s *Sample) { s.CPUAvailable = false }, time.Second, 0, 0},
		{"exec", func(s *Sample) { s.Executable = "/bin/other" }, time.Second, 0, 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			tr := NewTracker()
			first := activitySample(now, 1)
			tr.Update([]Sample{first}, now, 2*time.Second, true, 0)
			second := activitySample(now.Add(tt.gap), 2)
			tt.mutate(&second)
			tr.Update([]Sample{second}, second.SampledAt, 2*time.Second, true, 0)
			got := tr.Snapshot().Entries[0]
			if got.CoveredCPUSeconds != tt.wantCPU || got.CPUCoveredWallSeconds != tt.wantWall {
				t.Fatalf("coverage=%+v", got)
			}
			if got.RuntimeSeconds != 604800 || got.Samples != 2 {
				t.Fatalf("lifetime fabricated coverage: %+v", got)
			}
		})
	}
}
func TestTrackerIdentityExitRestartAndUnavailableScan(t *testing.T) {
	now := time.Now()
	tr := NewTracker()
	s := activitySample(now, 1)
	tr.Update([]Sample{s}, now, 2*time.Second, true, 0)
	tr.Update(nil, now.Add(time.Second), 2*time.Second, true, 0)
	if tr.Snapshot().Entries[0].Active {
		t.Fatal("missing task still observed")
	}
	s.SampledAt = now.Add(2 * time.Second)
	s.CPUSeconds = 3
	tr.Update([]Sample{s}, s.SampledAt, 2*time.Second, true, 0)
	if tr.Snapshot().Entries[0].CPUAverageAvailable {
		t.Fatal("gap was bridged")
	}
	s.SampledAt = now.Add(3 * time.Second)
	s.CPUSeconds = 4
	tr.Update([]Sample{s}, s.SampledAt, 2*time.Second, true, 0)
	old := tr.Snapshot().Entries[0]
	restored := NewTracker()
	restored.Restore([]Entry{old})
	s.SampledAt = now.Add(4 * time.Second)
	s.CPUSeconds = 5
	restored.Update([]Sample{s}, s.SampledAt, 2*time.Second, true, 0)
	if e := restored.Snapshot().Entries[0]; e.CPUCoveredWallSeconds != old.CPUCoveredWallSeconds || e.GapCount <= old.GapCount {
		t.Fatalf("daemon gap bridged: %+v", e)
	}
	reused := s
	reused.StartTicks = "124"
	reused.CPUSeconds = 999
	restored.Update([]Sample{reused}, now.Add(5*time.Second), 2*time.Second, true, 0)
	entries := restored.Snapshot().Entries
	if len(entries) != 2 {
		t.Fatalf("PID reuse inherited identity: %+v", entries)
	}
	for _, e := range entries {
		if e.StartTicks == reused.StartTicks && e.CPUAverageAvailable {
			t.Fatal("PID reuse inherited baseline")
		}
	}
	restored.Update(nil, now.Add(6*time.Second), 2*time.Second, false, 1)
	if result := restored.Snapshot(); result.ScanComplete || result.UnavailableIdentities != 1 {
		t.Fatalf("failed scan=%+v", result)
	}
}
func TestTrackerBoundsOverflowAndPrunesOldBaseline(t *testing.T) {
	tr := NewTracker()
	at := time.Now()
	samples := make([]Sample, MaxEntries+500)
	for i := range samples {
		samples[i] = activitySample(at, 1)
		samples[i].PID = i + 1
	}
	tr.Update(samples, at, time.Minute, true, 0)
	result := tr.Snapshot()
	if len(result.Entries) != MaxEntries || result.EvictedEntries != 500 {
		t.Fatalf("entries=%d dropped=%d", len(result.Entries), result.EvictedEntries)
	}
	next := activitySample(at.Add(time.Second), 2)
	next.PID = MaxEntries + 1000
	tr.Update([]Sample{next}, next.SampledAt, time.Minute, true, 0)
	if len(tr.previous) != 1 {
		t.Fatalf("inactive baseline leak: %d", len(tr.previous))
	}
}
func TestParseStatPreservesExactIdentity(t *testing.T) {
	fields := make([]string, 22)
	for i := range fields {
		fields[i] = "0"
	}
	fields[0] = "S"
	fields[11] = "100"
	fields[12] = "50"
	fields[19] = "9007199254740993"
	fields[21] = "2"
	body := "42 (job (with) spaces) " + strings.Join(fields, " ")
	stat, err := ParseStat(body, 100)
	if err != nil || stat.StartTicks != 9007199254740993 || stat.UserSeconds != 1 || stat.SystemSeconds != 0.5 {
		t.Fatalf("stat=%+v err=%v", stat, err)
	}
	for _, bad := range []string{"", "42 (bad) S 1", strings.Replace(body, "9007199254740993", "-1", 1)} {
		if _, err := ParseStat(bad, 100); err == nil {
			t.Fatalf("invalid stat accepted: %s", bad)
		}
	}
	if zero, err := ParseStat(strings.Replace(body, "9007199254740993", "0", 1), 100); err != nil || zero.StartTicks != 0 {
		t.Fatalf("kernel start0 rejected: %+v %v", zero, err)
	}
	if _, err := ParseStat(body, math.NaN()); err == nil {
		t.Fatal("nonfinite ticks accepted")
	}
}
func TestQueryRequiresCoveredCPUAndFiniteThresholds(t *testing.T) {
	e := Entry{RuntimeAvailable: true, RuntimeSeconds: 604800, AverageCPUPercent: 99}
	if (Query{MinAverageCPUPercent: 80}).Matches(e) {
		t.Fatal("lifetime scalar fabricated observed CPU")
	}
	for _, v := range []float64{-1, math.NaN(), math.Inf(1)} {
		if err := (Query{MinRuntimeSeconds: v}).Validate(); err == nil {
			t.Fatalf("threshold %v accepted", v)
		}
	}
	if got := (Query{Limit: 99999}).BoundedLimit(); got != 1000 {
		t.Fatalf("limit=%d", got)
	}
}
func BenchmarkTracker240Processes(b *testing.B) {
	samples := make([]Sample, 240)
	now := time.Now()
	for i := range samples {
		samples[i] = activitySample(now, float64(i))
		samples[i].PID = i + 1
		samples[i].UID = 1000 + i%45
	}
	tr := NewTracker()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		at := now.Add(time.Duration(i) * time.Second)
		for j := range samples {
			samples[j].SampledAt = at
			samples[j].CPUSeconds = float64(i + j)
		}
		tr.Update(samples, at, time.Minute, true, 0)
	}
	b.ReportMetric(240, "processes/scan")
	b.ReportMetric(45, "users/scan")
}
func ExampleQuery() {
	q := Query{MinRuntimeSeconds: 7 * 24 * 3600, MinAverageCPUPercent: 80, MinCoveredSeconds: 3600}
	fmt.Println(q.BoundedLimit()) // Output: 100
}

func TestTrackerMemorySourceChangesStartIndependentCoverage(t *testing.T) {
	tr := NewTracker()
	now := time.Now()
	for i := 0; i < 4; i++ {
		s := activitySample(now.Add(time.Duration(i)*time.Second), float64(i))
		if i >= 2 {
			s.MemorySource = MemoryRSS
			s.MemoryBytes = 1000
		}
		tr.Update([]Sample{s}, s.SampledAt, 2*time.Second, true, 0)
		if i == 2 {
			e := tr.Snapshot().Entries[0]
			if e.MemoryAverageAvailable || e.MemoryCoveredSeconds != 0 {
				t.Fatalf("PSS and RSS averaged together: %+v", e)
			}
		}
	}
	e := tr.Snapshot().Entries[0]
	if e.MemorySource != MemoryRSS || e.AverageMemoryBytes != 1000 || e.MemoryCoveredSeconds != 1 || e.CPUCoveredWallSeconds != 3 {
		t.Fatalf("source coverage=%+v", e)
	}
}
func TestIdentityRejectsNoncanonicalTicks(t *testing.T) {
	for _, ticks := range []string{"", "-1", "00", "0123", "1.0", "18446744073709551616"} {
		if (Identity{BootID: "boot", PID: 1, StartTicks: ticks}).Valid() {
			t.Fatalf("accepted %q", ticks)
		}
	}
}

func TestTrackerAcceptsZeroStartTicks(t *testing.T) {
	tr := NewTracker()
	at := time.Now()
	s := activitySample(at, 1)
	s.StartTicks = "0"
	if !s.Valid() {
		t.Fatal("canonical kernel start tick zero rejected")
	}
	tr.Update([]Sample{s}, at, 2*time.Second, true, 0)
	s.SampledAt = at.Add(time.Second)
	s.CPUSeconds = 2
	tr.Update([]Sample{s}, s.SampledAt, 2*time.Second, true, 0)
	if e := tr.Snapshot().Entries[0]; e.StartTicks != "0" || e.CoveredCPUSeconds != 1 {
		t.Fatalf("zero start coverage: %+v", e)
	}
}
