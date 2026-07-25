/*
 * Copyright (C) 2026 Francesco Defilippo
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU General Public License as published by
 * the Free Software Foundation, either version 3 of the License, or
 * (at your option) any later version.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
 * GNU General Public License for more details.
 *
 * You should have received a copy of the GNU General Public License
 * along with this program. If not, see <https://www.gnu.org/licenses/>.
 */
// state/pattern_detector.go
package state

import (
	"math"
	"sync"
	"time"

	"github.com/fdefilippo/resman/config"
	"github.com/fdefilippo/resman/logging"
)

// WorkloadPattern rappresenta il tipo di pattern riconosciuto per un utente.
type WorkloadPattern string

const (
	PatternUnknown        WorkloadPattern = "unknown"
	PatternBatchNight     WorkloadPattern = "batch_night"
	PatternInteractiveDay WorkloadPattern = "interactive_day"
	PatternMixed          WorkloadPattern = "mixed"
	PatternAlwaysOn       WorkloadPattern = "always_on"
	PatternSporadic       WorkloadPattern = "sporadic"
)

type hourlyPatternBucket struct {
	Hour        time.Time
	CPUSum      float64
	SampleCount int
}

// UserHourlyStats contains the retained hourly observations for a user.
type UserHourlyStats struct {
	Buckets []hourlyPatternBucket
}

// PatternResult contiene il risultato della classificazione.
type PatternResult struct {
	Pattern    WorkloadPattern
	Confidence float64
}

// PatternDetector rileva i pattern di utilizzo per ogni utente.
type PatternDetector struct {
	mu        sync.RWMutex
	logger    *logging.Logger
	userStats map[int]*UserHourlyStats // uid -> statistiche orarie
}

// NewPatternDetector crea un nuovo PatternDetector.
func NewPatternDetector(logger *logging.Logger) *PatternDetector {
	return &PatternDetector{
		logger:    logger,
		userStats: make(map[int]*UserHourlyStats),
	}
}

// Update aggiorna le statistiche per un utente con un nuovo campione.
func (pd *PatternDetector) Update(uid int, cpuUsage float64) {
	pd.updateAt(uid, cpuUsage, time.Now())
}

func (pd *PatternDetector) updateAt(uid int, cpuUsage float64, now time.Time) {
	pd.mu.Lock()
	defer pd.mu.Unlock()

	stats, exists := pd.userStats[uid]
	if !exists {
		stats = &UserHourlyStats{}
		pd.userStats[uid] = stats
	}

	hour := patternHourStart(now)
	for i := len(stats.Buckets) - 1; i >= 0; i-- {
		if stats.Buckets[i].Hour.Equal(hour) {
			stats.Buckets[i].CPUSum += cpuUsage
			stats.Buckets[i].SampleCount++
			return
		}
	}
	stats.Buckets = append(stats.Buckets, hourlyPatternBucket{
		Hour:        hour,
		CPUSum:      cpuUsage,
		SampleCount: 1,
	})
}

// Analyze analizza i pattern per tutti gli utenti e restituisce i risultati.
func (pd *PatternDetector) Analyze(cfg *config.Config) map[int]PatternResult {
	pd.mu.RLock()
	defer pd.mu.RUnlock()

	results := make(map[int]PatternResult)
	minSamples := cfg.GetPatternMinSamples()
	confidenceThreshold := cfg.GetPatternConfidenceThreshold()

	for uid, stats := range pd.userStats {
		if len(stats.Buckets) < minSamples {
			results[uid] = PatternResult{Pattern: PatternUnknown, Confidence: 0}
			continue
		}

		result := classifyPattern(stats, confidenceThreshold)
		results[uid] = result
	}

	return results
}

// classifyPattern classifica il pattern di un utente basandosi sulle statistiche orarie.
func classifyPattern(stats *UserHourlyStats, confidenceThreshold float64) PatternResult {
	hourlyCPU, hourlyCount := aggregateHourlyBuckets(stats.Buckets)

	// Calcola varianza oraria
	nightAvg, nightSamples := weightedAverage(hourlyCPU[:], hourlyCount[:], 22, 23, 0, 1, 2, 3, 4, 5, 6)
	dayAvg, daySamples := weightedAverage(hourlyCPU[:], hourlyCount[:], 8, 9, 10, 11, 12, 13, 14, 15, 16, 17)
	overallAvg := overallAverage(hourlyCPU[:], hourlyCount[:])
	variance := calculateVariance(hourlyCPU[:], hourlyCount[:])

	// Normalizza varianza (0-1)
	normalizedVariance := math.Min(variance/50.0, 1.0)

	// Ratio notte/giorno
	nightDayRatio := 0.0
	if dayAvg > 0 {
		nightDayRatio = nightAvg / dayAvg
	} else if nightAvg > 0 {
		nightDayRatio = math.Inf(1)
	}

	// Classificazione
	var pattern WorkloadPattern
	var confidence float64

	switch {
	case nightSamples > 0 && daySamples == 0 && nightAvg > 5:
		// No daytime samples and sustained night usage are strong batch evidence.
		pattern = PatternBatchNight
		confidence = math.Min(0.8+math.Min(nightAvg/100.0, 1.0)*0.2, 1.0)

	case nightDayRatio > 1.5 && normalizedVariance > 0.4:
		// Alta CPU di notte, bassa di giorno, alta varianza
		pattern = PatternBatchNight
		confidence = math.Min(nightDayRatio/3.0*0.6+normalizedVariance*0.4, 1.0)

	case nightDayRatio < 0.5 && normalizedVariance < 0.3 && overallAvg > 5:
		// CPU moderata di giorno, bassa di notte, varianza bassa
		pattern = PatternInteractiveDay
		confidence = math.Min((1.0-nightDayRatio)*0.5+(1.0-normalizedVariance)*0.5, 1.0)

	case normalizedVariance > 0.5 && nightDayRatio > 0.8 && nightDayRatio < 1.2:
		// Alta varianza ma uso simile giorno/notte
		pattern = PatternMixed
		confidence = math.Min(normalizedVariance, 1.0)

	case normalizedVariance < 0.2 && overallAvg > 10:
		// Bassa varianza, uso costante alto
		pattern = PatternAlwaysOn
		confidence = math.Min(1.0-normalizedVariance, 1.0)

	case overallAvg < 5 && normalizedVariance > 0.6:
		// Uso medio basso ma con picchi
		pattern = PatternSporadic
		confidence = math.Min(normalizedVariance*0.7+(1.0-overallAvg/5.0)*0.3, 1.0)

	default:
		pattern = PatternUnknown
		confidence = 0
	}

	if confidence < confidenceThreshold {
		pattern = PatternUnknown
		confidence = 0
	}

	return PatternResult{Pattern: pattern, Confidence: confidence}
}

func aggregateHourlyBuckets(buckets []hourlyPatternBucket) ([24]float64, [24]int) {
	var hourlyCPU [24]float64
	var hourlyCount [24]int
	for _, bucket := range buckets {
		if bucket.SampleCount <= 0 {
			continue
		}
		hour := bucket.Hour.Hour()
		bucketAverage := bucket.CPUSum / float64(bucket.SampleCount)
		hourlyCPU[hour] = (hourlyCPU[hour]*float64(hourlyCount[hour]) + bucketAverage) / float64(hourlyCount[hour]+1)
		hourlyCount[hour]++
	}
	return hourlyCPU, hourlyCount
}

// weightedAverage calculates the sample-weighted mean for selected hours.
func weightedAverage(values []float64, counts []int, hours ...int) (float64, int) {
	total := 0.0
	samples := 0
	for _, h := range hours {
		if h >= 0 && h < len(values) && h < len(counts) && counts[h] > 0 {
			total += values[h] * float64(counts[h])
			samples += counts[h]
		}
	}
	if samples == 0 {
		return 0, 0
	}
	return total / float64(samples), samples
}

// overallAverage calcola la media ponderata su tutte le ore.
func overallAverage(values []float64, counts []int) float64 {
	totalSum := 0.0
	totalCount := 0
	for i := 0; i < len(values) && i < len(counts); i++ {
		totalSum += values[i] * float64(counts[i])
		totalCount += counts[i]
	}
	if totalCount == 0 {
		return 0
	}
	return totalSum / float64(totalCount)
}

// calculateVariance calcola la varianza ponderata delle CPU orarie.
func calculateVariance(values []float64, counts []int) float64 {
	mean := overallAverage(values, counts)
	if mean == 0 {
		return 0
	}

	totalWeight := 0
	sumSquaredDiff := 0.0
	for i := 0; i < len(values) && i < len(counts); i++ {
		if counts[i] > 0 {
			diff := values[i] - mean
			sumSquaredDiff += diff * diff * float64(counts[i])
			totalWeight += counts[i]
		}
	}

	if totalWeight == 0 {
		return 0
	}
	return sumSquaredDiff / float64(totalWeight)
}

// Cleanup removes statistics older than the configured history window.
func (pd *PatternDetector) Cleanup(maxAge time.Duration) []int {
	return pd.cleanupAt(maxAge, time.Now())
}

func (pd *PatternDetector) cleanupAt(maxAge time.Duration, now time.Time) []int {
	pd.mu.Lock()
	defer pd.mu.Unlock()

	historyHours := int(maxAge / time.Hour)
	if maxAge%time.Hour != 0 {
		historyHours++
	}
	if historyHours < 1 {
		historyHours = 1
	}
	oldestHour := patternHourStart(now).Add(-time.Duration(historyHours-1) * time.Hour)

	var removed []int
	for uid, stats := range pd.userStats {
		retained := stats.Buckets[:0]
		for _, bucket := range stats.Buckets {
			if !bucket.Hour.Before(oldestHour) {
				retained = append(retained, bucket)
			}
		}
		stats.Buckets = retained
		if len(stats.Buckets) == 0 {
			delete(pd.userStats, uid)
			removed = append(removed, uid)
		}
	}
	return removed
}

func patternHourStart(value time.Time) time.Time {
	return value.Add(
		-time.Duration(value.Minute())*time.Minute -
			time.Duration(value.Second())*time.Second -
			time.Duration(value.Nanosecond()),
	)
}

// RetainUsers removes statistics for users that are no longer eligible.
func (pd *PatternDetector) RetainUsers(eligible map[int]bool) {
	pd.mu.Lock()
	defer pd.mu.Unlock()

	for uid := range pd.userStats {
		if !eligible[uid] {
			delete(pd.userStats, uid)
		}
	}
}

// UserIDs returns a snapshot of users with retained pattern history.
func (pd *PatternDetector) UserIDs() []int {
	pd.mu.RLock()
	defer pd.mu.RUnlock()

	uids := make([]int, 0, len(pd.userStats))
	for uid := range pd.userStats {
		uids = append(uids, uid)
	}
	return uids
}
