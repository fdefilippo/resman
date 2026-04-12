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

// UserHourlyStats contiene le statistiche aggregate per fascia oraria.
type UserHourlyStats struct {
	HourlyCPU    [24]float64 // Media CPU per ora (0-23)
	HourlyCount  [24]int     // Numero di campioni per ora
	TotalSamples int
	FirstSample  time.Time
	LastSample   time.Time
}

// PatternResult contiene il risultato della classificazione.
type PatternResult struct {
	Pattern    WorkloadPattern
	Confidence float64
}

// PatternDetector rileva i pattern di utilizzo per ogni utente.
type PatternDetector struct {
	mu           sync.RWMutex
	logger       *logging.Logger
	userStats    map[int]*UserHourlyStats // uid -> statistiche orarie
	lastAnalysis time.Time
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
	pd.mu.Lock()
	defer pd.mu.Unlock()

	stats, exists := pd.userStats[uid]
	if !exists {
		stats = &UserHourlyStats{
			FirstSample: time.Now(),
		}
		pd.userStats[uid] = stats
	}

	now := time.Now()
	hour := now.Hour()
	stats.HourlyCPU[hour] = (stats.HourlyCPU[hour]*float64(stats.HourlyCount[hour]) + cpuUsage) / float64(stats.HourlyCount[hour]+1)
	stats.HourlyCount[hour]++
	stats.TotalSamples++
	stats.LastSample = now
}

// Analyze analizza i pattern per tutti gli utenti e restituisce i risultati.
func (pd *PatternDetector) Analyze(cfg *config.Config) map[int]PatternResult {
	pd.mu.RLock()
	defer pd.mu.RUnlock()

	results := make(map[int]PatternResult)
	minSamples := cfg.GetPatternMinSamples()
	confidenceThreshold := cfg.GetPatternConfidenceThreshold()

	for uid, stats := range pd.userStats {
		if stats.TotalSamples < minSamples {
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
	// Calcola varianza oraria
	nightAvg := average(stats.HourlyCPU[:], 22, 23, 0, 1, 2, 3, 4, 5, 6)
	dayAvg := average(stats.HourlyCPU[:], 8, 9, 10, 11, 12, 13, 14, 15, 16, 17)
	overallAvg := overallAverage(stats.HourlyCPU[:], stats.HourlyCount[:])
	variance := calculateVariance(stats.HourlyCPU[:], stats.HourlyCount[:])

	// Normalizza varianza (0-1)
	normalizedVariance := math.Min(variance/50.0, 1.0)

	// Ratio notte/giorno
	nightDayRatio := 0.0
	if dayAvg > 0 {
		nightDayRatio = nightAvg / dayAvg
	}

	// Classificazione
	var pattern WorkloadPattern
	var confidence float64

	switch {
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

// average calcola la media dei valori per le ore specificate.
func average(values []float64, hours ...int) float64 {
	sum := 0.0
	count := 0
	for _, h := range hours {
		if h >= 0 && h < len(values) {
			sum += values[h]
			count++
		}
	}
	if count == 0 {
		return 0
	}
	return sum / float64(count)
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

// Cleanup rimuove statistiche vecchie oltre la finestra storica.
func (pd *PatternDetector) Cleanup(maxAge time.Duration) {
	pd.mu.Lock()
	defer pd.mu.Unlock()

	now := time.Now()
	for uid, stats := range pd.userStats {
		if now.Sub(stats.LastSample) > maxAge {
			delete(pd.userStats, uid)
		}
	}
}
