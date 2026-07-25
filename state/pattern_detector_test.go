package state

import (
	"testing"
	"time"

	"github.com/fdefilippo/resman/config"
)

func TestClassifyPatternBatchNightWithoutDaySamples(t *testing.T) {
	stats := &UserHourlyStats{}
	for day := 1; day <= 30; day++ {
		stats.Buckets = append(stats.Buckets, hourlyPatternBucket{
			Hour:        time.Date(2026, time.July, day, 23, 0, 0, 0, time.Local),
			CPUSum:      100,
			SampleCount: 1,
		})
	}

	result := classifyPattern(stats, 0.7)

	if result.Pattern != PatternBatchNight {
		t.Fatalf("pattern = %q, want %q", result.Pattern, PatternBatchNight)
	}
	if result.Confidence < 0.7 {
		t.Fatalf("confidence = %f, want at least 0.7", result.Confidence)
	}
}

func TestPatternMinimumSamplesCountsDistinctHours(t *testing.T) {
	cfg := configForPatternTest(2)
	detector := NewPatternDetector(nil)
	start := time.Date(2026, time.July, 24, 22, 0, 0, 0, time.Local)

	for i := 0; i < 100; i++ {
		detector.updateAt(1000, 100, start.Add(30*time.Second))
	}
	if result := detector.Analyze(cfg)[1000]; result.Pattern != PatternUnknown {
		t.Fatalf("one hourly bucket produced pattern %q, want unknown", result.Pattern)
	}

	detector.updateAt(1000, 100, start.Add(time.Hour))
	if result := detector.Analyze(cfg)[1000]; result.Pattern != PatternBatchNight {
		t.Fatalf("two hourly buckets produced pattern %q, want %q", result.Pattern, PatternBatchNight)
	}
}

func TestPatternCleanupPrunesExpiredBucketsForActiveUser(t *testing.T) {
	detector := NewPatternDetector(nil)
	now := time.Date(2026, time.July, 25, 12, 30, 0, 0, time.Local)
	detector.updateAt(1000, 10, now.Add(-3*time.Hour))
	detector.updateAt(1000, 20, now.Add(-time.Hour))
	detector.updateAt(1000, 30, now)

	if removed := detector.cleanupAt(2*time.Hour, now); len(removed) != 0 {
		t.Fatalf("active user was removed: %v", removed)
	}

	stats := detector.userStats[1000]
	if len(stats.Buckets) != 2 {
		t.Fatalf("retained buckets = %d, want 2", len(stats.Buckets))
	}
	if !stats.Buckets[0].Hour.Equal(patternHourStart(now.Add(-time.Hour))) {
		t.Fatalf("oldest retained bucket = %v", stats.Buckets[0].Hour)
	}
}

func TestPatternHourStartUsesLocalWallClock(t *testing.T) {
	location := time.FixedZone("offset-0330", 3*60*60+30*60)
	value := time.Date(2026, time.July, 25, 12, 47, 31, 123, location)

	got := patternHourStart(value)

	if got.Hour() != 12 || got.Minute() != 0 || got.Second() != 0 || got.Nanosecond() != 0 {
		t.Fatalf("hour start = %v, want 12:00:00 local", got)
	}
}

func TestHourlyBucketsHaveEqualWeightAcrossHours(t *testing.T) {
	stats := &UserHourlyStats{Buckets: []hourlyPatternBucket{
		{
			Hour:        time.Date(2026, time.July, 24, 22, 0, 0, 0, time.Local),
			CPUSum:      100000,
			SampleCount: 1000,
		},
		{
			Hour:        time.Date(2026, time.July, 24, 23, 0, 0, 0, time.Local),
			CPUSum:      0,
			SampleCount: 1,
		},
	}}

	hourlyCPU, hourlyCount := aggregateHourlyBuckets(stats.Buckets)
	average, samples := weightedAverage(hourlyCPU[:], hourlyCount[:], 22, 23)
	if samples != 2 {
		t.Fatalf("hourly samples = %d, want 2", samples)
	}
	if average != 50 {
		t.Fatalf("night average = %f, want 50", average)
	}
}

func TestWeightedAverageUsesSampleCounts(t *testing.T) {
	var values [24]float64
	var counts [24]int
	values[22], counts[22] = 100, 1
	values[23], counts[23] = 10, 9

	got, samples := weightedAverage(values[:], counts[:], 22, 23)

	if samples != 10 {
		t.Fatalf("samples = %d, want 10", samples)
	}
	if got != 19 {
		t.Fatalf("average = %f, want 19", got)
	}
}

func configForPatternTest(minSamples int) *config.Config {
	cfg := config.DefaultConfig()
	cfg.PatternMinSamples = minSamples
	cfg.PatternConfidenceThreshold = 0.7
	return cfg
}
