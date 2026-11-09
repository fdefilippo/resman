// Package processactivity records sampled process activity without making policy decisions.
package processactivity

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"sync"
	"time"
)

const (
	MaxEntries      = 10000
	MaxQueryEntries = 1000
)

// Identity distinguishes PID reuse and host reboots without relying on wall time.
type Identity struct {
	BootID     string `json:"boot_id"`
	PID        int    `json:"pid"`
	StartTicks string `json:"start_ticks"`
}

// Valid accepts only the canonical exact kernel start-tick representation.
func (id Identity) Valid() bool {
	ticks, err := strconv.ParseUint(id.StartTicks, 10, 64)
	return id.BootID != "" && id.PID > 0 && err == nil && strconv.FormatUint(ticks, 10) == id.StartTicks
}

// MemorySource identifies the actual fresh memory observation, not an inferred value.
type MemorySource string

const (
	MemoryUnknown MemorySource = "unknown"
	MemoryPSS     MemorySource = "pss"
	MemoryRSS     MemorySource = "rss"
)

// Sample is captured inside the same completed scan as the decision observations.
type Sample struct {
	Identity
	UID                 int
	Executable          string
	ExecutableAvailable bool
	SampledAt           time.Time
	RuntimeAvailable    bool
	RuntimeSeconds      float64
	CPUSeconds          float64
	CPUAvailable        bool
	MemoryBytes         uint64
	MemorySource        MemorySource
}

// Entry distinguishes process lifetime from covered sampled activity intervals.
type Entry struct {
	Identity
	UID                    int          `json:"uid"`
	Executable             string       `json:"executable"`
	ExecutableAvailable    bool         `json:"executable_available"`
	ExecutableChanges      uint64       `json:"executable_changes"`
	FirstObservedAt        time.Time    `json:"first_observed_at"`
	ActivitySince          time.Time    `json:"activity_since"`
	LastSeenAt             time.Time    `json:"last_seen_at"`
	Active                 bool         `json:"active"`
	RuntimeAvailable       bool         `json:"runtime_available"`
	RuntimeSeconds         float64      `json:"runtime_seconds"`
	Samples                uint64       `json:"samples"`
	GapCount               uint64       `json:"gap_count"`
	CoveredCPUSeconds      float64      `json:"covered_cpu_seconds"`
	CPUCoveredWallSeconds  float64      `json:"cpu_covered_wall_seconds"`
	CPUAverageAvailable    bool         `json:"cpu_average_available"`
	AverageCPUPercent      float64      `json:"average_cpu_percent"`
	CPUAvailable           bool         `json:"cpu_available"`
	MemoryAvailable        bool         `json:"memory_available"`
	MemorySource           MemorySource `json:"memory_source"`
	MemoryBytes            uint64       `json:"memory_bytes"`
	MemoryByteSeconds      float64      `json:"memory_byte_seconds"`
	MemoryCoveredSeconds   float64      `json:"memory_covered_seconds"`
	MemoryAverageAvailable bool         `json:"memory_average_available"`
	AverageMemoryBytes     float64      `json:"average_memory_bytes"`
}

// Query selects a bounded observational result. CPU percentage is per-core (100 = one CPU).
type Query struct {
	UID                  *int    `json:"uid,omitempty"`
	MinRuntimeSeconds    float64 `json:"min_runtime_seconds,omitempty"`
	MinAverageCPUPercent float64 `json:"min_average_cpu_percent,omitempty"`
	MinCoveredSeconds    float64 `json:"min_covered_seconds,omitempty"`
	ActiveOnly           bool    `json:"active_only,omitempty"`
	Limit                int     `json:"limit,omitempty"`
}

func (q Query) Validate() error {
	if q.UID != nil && *q.UID < 0 {
		return fmt.Errorf("uid must be nonnegative")
	}
	for _, v := range []float64{q.MinRuntimeSeconds, q.MinAverageCPUPercent, q.MinCoveredSeconds} {
		if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
			return fmt.Errorf("activity thresholds must be finite and nonnegative")
		}
	}
	return nil
}
func (q Query) BoundedLimit() int {
	if q.Limit <= 0 {
		return 100
	}
	if q.Limit > MaxQueryEntries {
		return MaxQueryEntries
	}
	return q.Limit
}
func (q Query) Matches(e Entry) bool {
	return (q.UID == nil || e.UID == *q.UID) && (!q.ActiveOnly || e.Active) && (q.MinRuntimeSeconds == 0 || e.RuntimeAvailable && e.RuntimeSeconds >= q.MinRuntimeSeconds) && e.CPUCoveredWallSeconds >= q.MinCoveredSeconds && (q.MinAverageCPUPercent == 0 || e.CPUAverageAvailable && e.AverageCPUPercent >= q.MinAverageCPUPercent)
}

// Result reports storage and scan coverage independently of returned process values.
// PersistenceStatus describes access to the optional activity store.
type PersistenceStatus string

const (
	PersistenceDisabled    PersistenceStatus = "disabled"
	PersistenceAvailable   PersistenceStatus = "available"
	PersistenceUnavailable PersistenceStatus = "unavailable"
)

type Result struct {
	Entries               []Entry           `json:"entries"`
	Persistence           PersistenceStatus `json:"persistence_status"`
	SampledAt             time.Time         `json:"sampled_at"`
	UnavailableIdentities int               `json:"unavailable_identities"`
	EvictedEntries        uint64            `json:"evicted_entries"`
	ScanComplete          bool              `json:"scan_complete"`
}

type baseline struct {
	sample Sample
	valid  bool
}

// Tracker owns only observational state. Its mutex is never held across I/O.
type Tracker struct {
	mu          sync.RWMutex
	entries     map[Identity]Entry
	previous    map[Identity]baseline
	sampledAt   time.Time
	unavailable int
	complete    bool
	evicted     uint64
}

func NewTracker() *Tracker {
	return &Tracker{entries: make(map[Identity]Entry), previous: make(map[Identity]baseline)}
}

// Restore retains durable covered totals but never bridges daemon downtime.
func (t *Tracker) Restore(entries []Entry) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, e := range entries {
		if current, ok := t.entries[e.Identity]; ok {
			// No new snapshot is persisted until this restore succeeds. Covered
			// intervals collected during a temporary read failure are disjoint
			// from the durable daemon episode and may be added without bridging it.
			current.Samples += e.Samples
			current.GapCount += e.GapCount + 1
			if e.FirstObservedAt.Before(current.FirstObservedAt) {
				current.FirstObservedAt = e.FirstObservedAt
			}
			if current.UID == e.UID && current.ExecutableAvailable == e.ExecutableAvailable && current.Executable == e.Executable {
				if e.ActivitySince.Before(current.ActivitySince) {
					current.ActivitySince = e.ActivitySince
				}
				current.CoveredCPUSeconds += e.CoveredCPUSeconds
				current.CPUCoveredWallSeconds += e.CPUCoveredWallSeconds
				if current.MemorySource == e.MemorySource {
					current.MemoryByteSeconds += e.MemoryByteSeconds
					current.MemoryCoveredSeconds += e.MemoryCoveredSeconds
				}
				current.ExecutableChanges += e.ExecutableChanges
			} else {
				current.ExecutableChanges += e.ExecutableChanges + 1
			}
			current.CPUAverageAvailable = current.CPUCoveredWallSeconds > 0
			if current.CPUAverageAvailable {
				current.AverageCPUPercent = current.CoveredCPUSeconds / current.CPUCoveredWallSeconds * 100
			}
			current.MemoryAverageAvailable = current.MemoryCoveredSeconds > 0
			if current.MemoryAverageAvailable {
				current.AverageMemoryBytes = current.MemoryByteSeconds / current.MemoryCoveredSeconds
			}
			t.entries[e.Identity] = current
			continue
		}
		if len(t.entries) >= MaxEntries {
			continue
		}
		e.Active = false
		t.entries[e.Identity] = e
	}
}

// Update accepts one completed scan. Missing identities mean unobserved, not proven dead.
func (t *Tracker) Update(samples []Sample, sampledAt time.Time, maxGap time.Duration, complete bool, unavailable int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.sampledAt, t.complete, t.unavailable = sampledAt, complete, unavailable
	for id, e := range t.entries {
		e.Active = false
		t.entries[id] = e
	}
	seen := make(map[Identity]bool, len(samples))
	incoming := make(map[Identity]bool, len(samples))
	for _, sample := range samples {
		incoming[sample.Identity] = true
	}
	// Sort eviction candidates once per scan, never once per incoming process.
	candidates := make([]Identity, 0)
	for id := range t.entries {
		if !incoming[id] {
			candidates = append(candidates, id)
		}
	}
	sort.Slice(candidates, func(i, j int) bool {
		return t.entries[candidates[i]].LastSeenAt.Before(t.entries[candidates[j]].LastSeenAt)
	})
	candidateIndex := 0
	for _, s := range samples {
		if !s.Valid() || s.SampledAt.IsZero() {
			continue
		}
		seen[s.Identity] = true
		e, exists := t.entries[s.Identity]
		if !exists {
			if len(t.entries) >= MaxEntries {
				if candidateIndex >= len(candidates) {
					t.evicted++
					continue
				}
				evicted := candidates[candidateIndex]
				candidateIndex++
				delete(t.entries, evicted)
				delete(t.previous, evicted)
				t.evicted++
			}
			e = Entry{Identity: s.Identity, UID: s.UID, FirstObservedAt: s.SampledAt, ActivitySince: s.SampledAt}
		}
		prev := t.previous[s.Identity]
		execChanged := exists && (e.UID != s.UID || e.ExecutableAvailable != s.ExecutableAvailable || e.Executable != s.Executable)
		if execChanged {
			e.ExecutableChanges++
			e.ActivitySince = s.SampledAt
			e.CoveredCPUSeconds = 0
			e.CPUCoveredWallSeconds = 0
			e.MemoryByteSeconds = 0
			e.MemoryCoveredSeconds = 0
			e.GapCount++
			prev.valid = false
		}
		elapsed := s.SampledAt.Sub(prev.sample.SampledAt).Seconds()
		contiguous := prev.valid && elapsed > 0 && elapsed <= maxGap.Seconds() && complete
		if exists && !contiguous {
			e.GapCount++
		}
		if contiguous && s.CPUAvailable && prev.sample.CPUAvailable && s.CPUSeconds >= prev.sample.CPUSeconds {
			e.CoveredCPUSeconds += s.CPUSeconds - prev.sample.CPUSeconds
			e.CPUCoveredWallSeconds += elapsed
		} else if contiguous {
			e.GapCount++
		}
		if exists && s.MemorySource != e.MemorySource {
			e.MemoryByteSeconds = 0
			e.MemoryCoveredSeconds = 0
		}
		if contiguous && s.MemorySource != MemoryUnknown && prev.sample.MemorySource == s.MemorySource {
			e.MemoryByteSeconds += (float64(prev.sample.MemoryBytes) + float64(s.MemoryBytes)) / 2 * elapsed
			e.MemoryCoveredSeconds += elapsed
		}
		e.UID = s.UID
		e.Executable = s.Executable
		e.ExecutableAvailable = s.ExecutableAvailable
		e.LastSeenAt = s.SampledAt
		e.Active = true
		e.RuntimeAvailable = s.RuntimeAvailable
		e.RuntimeSeconds = s.RuntimeSeconds
		e.Samples++
		e.CPUAvailable = s.CPUAvailable
		e.MemorySource = s.MemorySource
		e.MemoryAvailable = s.MemorySource != MemoryUnknown
		e.MemoryBytes = s.MemoryBytes
		e.CPUAverageAvailable = e.CPUCoveredWallSeconds > 0
		if e.CPUAverageAvailable {
			e.AverageCPUPercent = e.CoveredCPUSeconds / e.CPUCoveredWallSeconds * 100
		} else {
			e.AverageCPUPercent = 0
		}
		e.MemoryAverageAvailable = e.MemoryCoveredSeconds > 0
		if e.MemoryCoveredSeconds > 0 {
			e.AverageMemoryBytes = e.MemoryByteSeconds / e.MemoryCoveredSeconds
		} else {
			e.AverageMemoryBytes = 0
		}
		t.entries[s.Identity] = e
		t.previous[s.Identity] = baseline{sample: s, valid: complete}
	}
	for id := range t.previous {
		if !seen[id] {
			delete(t.previous, id)
		}
	}
}
func (t *Tracker) Snapshot() Result {
	t.mu.RLock()
	defer t.mu.RUnlock()
	result := Result{Entries: make([]Entry, 0, len(t.entries)), SampledAt: t.sampledAt, UnavailableIdentities: t.unavailable, EvictedEntries: t.evicted, ScanComplete: t.complete}
	for _, e := range t.entries {
		result.Entries = append(result.Entries, e)
	}
	return result
}

// HasObservations distinguishes an unused tracker from a completed empty scan.
// Restored entries also represent observations owed to the persistence path.
func (t *Tracker) HasObservations() bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return !t.sampledAt.IsZero() || len(t.entries) > 0
}

// Merge applies filters after identity deduplication, preferring newer memory evidence.
func Merge(durable, memory []Entry, q Query) []Entry {
	byID := make(map[Identity]Entry, len(durable)+len(memory))
	for _, src := range [][]Entry{durable, memory} {
		for _, e := range src {
			old, ok := byID[e.Identity]
			if !ok || !e.LastSeenAt.Before(old.LastSeenAt) {
				byID[e.Identity] = e
			}
		}
	}
	entries := make([]Entry, 0, len(byID))
	for _, e := range byID {
		if q.Matches(e) {
			entries = append(entries, e)
		}
	}
	sort.Slice(entries, func(i, j int) bool {
		if !entries[i].LastSeenAt.Equal(entries[j].LastSeenAt) {
			return entries[i].LastSeenAt.After(entries[j].LastSeenAt)
		}
		if entries[i].BootID != entries[j].BootID {
			return entries[i].BootID < entries[j].BootID
		}
		if entries[i].PID != entries[j].PID {
			return entries[i].PID < entries[j].PID
		}
		return entries[i].StartTicks < entries[j].StartTicks
	})
	if len(entries) > q.BoundedLimit() {
		entries = entries[:q.BoundedLimit()]
	}
	return entries
}

// Prune removes only inactive historical cache entries outside the durable retention.
func (t *Tracker) Prune(cutoff time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for id, e := range t.entries {
		if !e.Active && e.LastSeenAt.Before(cutoff) {
			delete(t.entries, id)
			delete(t.previous, id)
		}
	}
}
