package state

import "testing"

func TestClassifyPatternBatchNightWithoutDaySamples(t *testing.T) {
	stats := &UserHourlyStats{TotalSamples: 30}
	stats.HourlyCPU[23] = 100
	stats.HourlyCount[23] = 30

	result := classifyPattern(stats, 0.7)

	if result.Pattern != PatternBatchNight {
		t.Fatalf("pattern = %q, want %q", result.Pattern, PatternBatchNight)
	}
	if result.Confidence < 0.7 {
		t.Fatalf("confidence = %f, want at least 0.7", result.Confidence)
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
