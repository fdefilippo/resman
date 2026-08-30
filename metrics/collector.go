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
// metrics/collector.go
package metrics

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/fdefilippo/resman/config"
	"github.com/fdefilippo/resman/internal/operationgate"
	"github.com/fdefilippo/resman/internal/processidentity"
	"github.com/fdefilippo/resman/internal/processpolicy"
	"github.com/fdefilippo/resman/logging"
	"github.com/shirou/gopsutil/v3/cpu"
	"github.com/shirou/gopsutil/v3/host"
	"github.com/shirou/gopsutil/v3/mem"
	"github.com/shirou/gopsutil/v3/process"
)

const (
	cpuPercentMultiplier                  = 100.0
	defaultFallbackCPUSamplingInterval    = 30 * time.Second
	fallbackCPUSampleIntervalsBeforeStale = 2
)

// UserMetrics contains metrics for a single user.
type UserMetrics struct {
	UID                                    int
	Username                               string
	CPUUsage                               float64 // CPU percentage (instantaneous, last cycle)
	CPUUsageAverage                        float64 // CPU percentage average since process start
	CPUUsageEMA                            float64 // CPU percentage exponential moving average (α=0.3)
	MemoryUsage                            uint64  // Memory in bytes (PSS when available, RSS fallback)
	ProcessCount                           int     // Number of processes
	EligibleForCPU                         bool    // Whether CPU policy may limit the user
	EligibleForRAM                         bool    // Whether RAM policy may limit the user
	EligibleForIO                          bool    // Whether I/O policy may limit the user
	CPULimitRequested                      bool    // Whether the control cycle currently requests a CPU limit
	CPULimitActive                         bool    // Whether CPU cgroup enforcement is observed as active
	RAMLimitRequested                      bool    // Whether the control cycle currently requests a RAM limit
	RAMLimitActive                         bool    // Whether RAM cgroup enforcement is observed as active
	IOLimitRequested                       bool    // Whether the control cycle currently requests an I/O limit
	IOLimitActive                          bool    // Whether I/O cgroup enforcement is observed as active
	IOReadBytes                            uint64  // Total bytes read from block devices
	IOWriteBytes                           uint64  // Total bytes written to block devices
	IOReadOps                              uint64  // Total read-family syscalls reported by /proc/PID/io syscr
	IOWriteOps                             uint64  // Total write-family syscalls reported by /proc/PID/io syscw
	ExecutableIdentityUnavailableProcesses int     // Processes without a trustworthy /proc/PID/exe identity
	IOUnavailableProcesses                 int     // Processes without a trustworthy I/O decision sample
	EnforceableUsage                       ProcessSetMetrics
}

// ProcessSetMetrics contains usage from processes selected for cgroup
// enforcement. Observed UserMetrics fields continue to describe every process.
type ProcessSetMetrics struct {
	CPUUsage                               float64
	CPUUsageAverage                        float64
	CPUUsageEMA                            float64
	MemoryUsage                            uint64
	ProcessCount                           int
	IOReadBytes                            uint64
	IOWriteBytes                           uint64
	IOReadOps                              uint64
	IOWriteOps                             uint64
	IODelta                                ProcessIODelta
	ExecutableIdentityUnavailableProcesses int
	IOUnavailableProcesses                 int
}

// ProcessIODelta contains the sum of per-process counter growth observed since
// the previous sample from the same sampling stream.
type ProcessIODelta struct {
	ReadBytes  uint64
	WriteBytes uint64
}

// ObservationMetrics is the typed observation snapshot consumed by external
// status surfaces. It does not contain policy eligibility or runtime state.
type ObservationMetrics struct {
	TotalCores            int
	TotalCPUUsage         float64
	ObservedUsersCPUUsage float64
	ObservedUsersCount    int
	MemoryUsageMB         float64
	TotalMemoryMB         float64
	SystemUnderLoad       bool
}

type processIOCounters struct {
	readBytes  uint64
	writeBytes uint64
	readOps    uint64
	writeOps   uint64
}

type processIOSample struct {
	startTime int64
	counters  processIOCounters
	sampledAt time.Time
}

// procCache holds temporal CPU and I/O data for all PIDs.
// Uses single mutex instead of sharding for simplicity and deadlock safety.
type procCache struct {
	mu            sync.RWMutex
	prevProcCPU   map[int32]cpu.TimesStat
	prevProcTime  map[int32]time.Time
	procStartTime map[int32]int64
	prevProcIO    map[int32]processIOSample
}

// userData is a temporary structure for accumulating data per UID during /proc scan.
type userData struct {
	observed    processUsage
	enforceable processUsage
}

type processUsage struct {
	cpuUsage                               float64
	cpuUsageAvg                            float64
	processCount                           int
	memoryUsage                            uint64
	ioReadBytes                            uint64
	ioWriteBytes                           uint64
	ioReadOps                              uint64
	ioWriteOps                             uint64
	ioDelta                                ProcessIODelta
	executableIdentityUnavailableProcesses int
	ioUnavailableProcesses                 int
}

func (u *processUsage) add(sample processUsage) {
	u.cpuUsage += sample.cpuUsage
	u.cpuUsageAvg += sample.cpuUsageAvg
	u.processCount += sample.processCount
	u.memoryUsage += sample.memoryUsage
	u.ioReadBytes += sample.ioReadBytes
	u.ioWriteBytes += sample.ioWriteBytes
	u.ioReadOps += sample.ioReadOps
	u.ioWriteOps += sample.ioWriteOps
	u.ioDelta.ReadBytes += sample.ioDelta.ReadBytes
	u.ioDelta.WriteBytes += sample.ioDelta.WriteBytes
	u.executableIdentityUnavailableProcesses += sample.executableIdentityUnavailableProcesses
	u.ioUnavailableProcesses += sample.ioUnavailableProcesses
}

func addProcessSample(data *userData, cfg *config.Config, executable, comm string, sample processUsage) processpolicy.Selection {
	selection := processpolicy.Evaluate(cfg, executable, comm)
	data.observed.add(sample)
	if selection.Enforceable {
		data.enforceable.add(sample)
	}
	return selection
}

func processSetMetrics(usage processUsage, ema float64) ProcessSetMetrics {
	return ProcessSetMetrics{
		CPUUsage:                               usage.cpuUsage,
		CPUUsageAverage:                        usage.cpuUsageAvg,
		CPUUsageEMA:                            ema,
		MemoryUsage:                            usage.memoryUsage,
		ProcessCount:                           usage.processCount,
		IOReadBytes:                            usage.ioReadBytes,
		IOWriteBytes:                           usage.ioWriteBytes,
		IOReadOps:                              usage.ioReadOps,
		IOWriteOps:                             usage.ioWriteOps,
		IODelta:                                usage.ioDelta,
		ExecutableIdentityUnavailableProcesses: usage.executableIdentityUnavailableProcesses,
		IOUnavailableProcesses:                 usage.ioUnavailableProcesses,
	}
}

type procFSFailureSummary struct {
	access   string
	policy   string
	count    int
	firstPID int
	firstErr error
}

func (s *procFSFailureSummary) record(pid int, err error) {
	s.count++
	if s.firstPID == 0 {
		s.firstPID = pid
		s.firstErr = err
	}
}

func boolCount(value bool) int {
	if value {
		return 1
	}
	return 0
}

func reportableProcFSFailure(err error) bool {
	return err != nil && !errors.Is(err, os.ErrNotExist)
}

// emaCache stores EMA values per UID between cycles.
type emaCache struct {
	mu                sync.RWMutex
	values            map[int]float64 // uid -> observed EMA value
	enforceableValues map[int]float64 // uid -> enforceable-process EMA value
}

// userMetricsSamplingState owns temporal data for one sampling purpose. The
// observation and decision streams must never share baselines or smoothing.
type userMetricsSamplingState struct {
	process *procCache
	ema     *emaCache
}

// metricCacheEntry binds a cached value to the retention contract that owned
// the read which populated it.
type metricCacheEntry struct {
	value    interface{}
	storedAt time.Time
	ttl      time.Duration
}

func newUserMetricsSamplingState() *userMetricsSamplingState {
	return &userMetricsSamplingState{
		process: &procCache{
			prevProcCPU:   make(map[int32]cpu.TimesStat),
			prevProcTime:  make(map[int32]time.Time),
			procStartTime: make(map[int32]int64),
			prevProcIO:    make(map[int32]processIOSample),
		},
		ema: &emaCache{
			values:            make(map[int]float64),
			enforceableValues: make(map[int]float64),
		},
	}
}

type cpuJiffySample struct {
	total     uint64
	idle      uint64
	sampledAt time.Time
	valid     bool
}

// Collector collects system metrics.
type Collector struct {
	cfg    *config.Config
	logger *logging.Logger
	mu     sync.RWMutex

	// Shared metric-value cache.
	cache           map[string]metricCacheEntry
	cacheMutex      sync.RWMutex
	userMetricsScan operationgate.Gate
	now             func() time.Time

	// Previous /proc/stat sample. Values are raw kernel jiffies.
	prevFallbackCPU             cpuJiffySample
	fallbackCPUSamplingInterval time.Duration

	// Observation refreshes and control decisions own independent temporal
	// state so changing observability cadence cannot change enforcement.
	observationState *userMetricsSamplingState
	decisionState    *userMetricsSamplingState

	// Optional database writer.
	dbWriter *DBWriter

	// UID-to-username resolution cache.
	usernameCache      map[int]string    // UID -> username
	usernameCacheTime  map[int]time.Time // Last resolution timestamp
	usernameCacheMutex sync.RWMutex
	usernameCacheTTL   time.Duration // Cache TTL

	// Cleanup goroutine control
	stopCleanup chan struct{}
	cleanupDone chan struct{}
}

// Default username cache TTL.
const (
	DEFAULT_USERNAME_CACHE_TTL = 60 * time.Minute
	MAX_CACHE_SIZE             = 10000 // Maximum number of entries in general cache
	MAX_USERNAME_CACHE_SIZE    = 10000 // Maximum number of entries in username cache
)

// NewCollector creates a metrics collector using the initial configuration.
func NewCollector(cfg *config.Config) (*Collector, error) {
	logger := logging.GetLogger()
	usernameCacheTTL := DEFAULT_USERNAME_CACHE_TTL
	if cfg != nil && cfg.UsernameCacheTTL > 0 {
		usernameCacheTTL = time.Duration(cfg.UsernameCacheTTL) * time.Minute
	}

	collector := &Collector{
		cfg:                         cfg,
		logger:                      logger,
		cache:                       make(map[string]metricCacheEntry),
		usernameCache:               make(map[int]string),
		usernameCacheTime:           make(map[int]time.Time),
		usernameCacheTTL:            usernameCacheTTL,
		stopCleanup:                 make(chan struct{}),
		cleanupDone:                 make(chan struct{}),
		observationState:            newUserMetricsSamplingState(),
		decisionState:               newUserMetricsSamplingState(),
		fallbackCPUSamplingInterval: configuredPollingInterval(cfg),
		now:                         time.Now,
	}

	go collector.periodicCleanup()

	logger.Info("Metrics collector initialized")
	return collector, nil
}

// SetDBWriter replaces the optional metrics database writer.
func (c *Collector) SetDBWriter(writer *DBWriter) {
	c.mu.Lock()
	c.dbWriter = writer
	c.mu.Unlock()
	c.logger.Info("Database writer configured", "enabled", writer != nil)
}

// GetDBWriter returns the current DBWriter.
func (c *Collector) GetDBWriter() *DBWriter {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.dbWriter
}

func (c *Collector) getConfig() *config.Config {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.cfg
}

func (c *Collector) metricsCacheTTL() time.Duration {
	return time.Duration(c.getConfig().GetMetricsCacheTTL()) * time.Second
}

func (c *Collector) currentTime() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now()
}

// GetTotalCores returns the total number of CPU cores.
func (c *Collector) GetTotalCores() int {
	cacheKey := "total_cores"
	if val, valid := c.getFromCache(cacheKey); valid { // Core count changes rarely, so it owns a long TTL.
		return val.(int)
	}

	cores, err := cpu.Counts(true)
	if err != nil {
		c.logger.Warn("Failed to get CPU core count via gopsutil, using /proc/cpuinfo fallback",
			"error", err,
		)
		// Fall back to /proc/cpuinfo.
		cores = c.getTotalCoresFallback()
	}

	c.setInCache(cacheKey, cores, time.Hour)
	return cores
}

// getTotalCoresFallback returns the core count without gopsutil.
func (c *Collector) getTotalCoresFallback() int {
	file, err := os.Open("/proc/cpuinfo")
	if err != nil {
		c.logger.Error("Failed to open /proc/cpuinfo to read CPU cores",
			"error", err,
			"fallback", "returning 1 core",
		)
		return 1
	}
	defer func() { _ = file.Close() }()

	cores := 0
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "processor") {
			cores++
		}
	}

	if cores == 0 {
		c.logger.Warn("/proc/cpuinfo returned 0 cores, using fallback of 1")
		cores = 1
	}

	return cores
}

// GetTotalCPUUsage returns host-wide CPU usage as a percentage.
func (c *Collector) GetTotalCPUUsage() float64 {
	cacheKey := "total_cpu_usage"
	if val, valid := c.getFromCache(cacheKey); valid {
		return val.(float64)
	}

	return c.getTotalCPUUsageFallback()
}

// getTotalCPUUsageFallback calculates host CPU usage from /proc/stat jiffies.
func (c *Collector) getTotalCPUUsageFallback() float64 {
	file, err := os.Open("/proc/stat")
	if err != nil {
		c.logger.Error("Failed to open /proc/stat for CPU usage calculation",
			"error", err,
			"fallback", "returning 0.0",
		)
		return 0.0
	}
	defer func() { _ = file.Close() }()

	scanner := bufio.NewScanner(file)
	if !scanner.Scan() {
		return 0.0
	}

	line := scanner.Text()
	if !strings.HasPrefix(line, "cpu ") {
		return 0.0
	}

	// Parse the aggregate CPU line.
	fields := strings.Fields(line)
	if len(fields) < 8 {
		return 0.0
	}

	// Calculate total jiffies.
	user, _ := strconv.ParseUint(fields[1], 10, 64)
	nice, _ := strconv.ParseUint(fields[2], 10, 64)
	system, _ := strconv.ParseUint(fields[3], 10, 64)
	idle, _ := strconv.ParseUint(fields[4], 10, 64)
	iowait, _ := strconv.ParseUint(fields[5], 10, 64)
	irq, _ := strconv.ParseUint(fields[6], 10, 64)
	softirq, _ := strconv.ParseUint(fields[7], 10, 64)
	steal := uint64(0)
	if len(fields) > 8 {
		steal, _ = strconv.ParseUint(fields[8], 10, 64)
	}

	total := user + nice + system + idle + iowait + irq + softirq + steal

	usage := c.updateFallbackCPUSample(total, idle)

	// Cache the result under the configured value-reuse TTL.
	c.setInCache("total_cpu_usage", usage, c.metricsCacheTTL())

	return usage
}

func (c *Collector) updateFallbackCPUSample(total, idle uint64) float64 {
	return c.updateFallbackCPUSampleAt(total, idle, time.Now())
}

func (c *Collector) updateFallbackCPUSampleAt(total, idle uint64, now time.Time) float64 {
	maxGap := c.fallbackCPUSampleMaxGap()

	c.mu.Lock()
	defer c.mu.Unlock()

	previous := c.prevFallbackCPU
	c.prevFallbackCPU = cpuJiffySample{total: total, idle: idle, sampledAt: now, valid: true}
	if !previous.valid || now.Before(previous.sampledAt) || now.Sub(previous.sampledAt) > maxGap ||
		total < previous.total || idle < previous.idle {
		return 0
	}

	totalDelta := total - previous.total
	idleDelta := idle - previous.idle
	if totalDelta == 0 || idleDelta > totalDelta {
		return 0
	}
	return cpuPercentMultiplier * float64(totalDelta-idleDelta) / float64(totalDelta)
}

// SetFallbackCPUSamplingInterval records the control loop's effective runtime
// cadence. Configuration intent alone cannot determine whether PSI monitoring
// actually started.
func (c *Collector) SetFallbackCPUSamplingInterval(interval time.Duration) {
	if interval <= 0 {
		interval = defaultFallbackCPUSamplingInterval
	}
	c.mu.Lock()
	c.fallbackCPUSamplingInterval = interval
	c.mu.Unlock()
}

func (c *Collector) fallbackCPUSampleMaxGap() time.Duration {
	c.mu.RLock()
	samplingInterval := c.fallbackCPUSamplingInterval
	c.mu.RUnlock()
	if samplingInterval <= 0 {
		samplingInterval = configuredPollingInterval(c.getConfig())
	}
	return fallbackCPUSampleIntervalsBeforeStale * samplingInterval
}

func configuredPollingInterval(cfg *config.Config) time.Duration {
	if cfg != nil && cfg.GetPollingInterval() > 0 {
		return time.Duration(cfg.GetPollingInterval()) * time.Second
	}
	return defaultFallbackCPUSamplingInterval
}

// GetUserCPUUsage returns CPU usage for a user.
// It excludes configured system processes.
func (c *Collector) GetUserCPUUsage(uid int) float64 {
	if !c.isMonitoredUserUID(uid) {
		return 0.0
	}

	// Use data already collected by GetAllUserMetrics to avoid redundant scans
	allMetrics := c.GetAllUserMetrics()
	if metrics, exists := allMetrics[uid]; exists {
		return metrics.CPUUsage
	}
	return 0
}

// getUIDFromStatusFile reads the UID from /proc/[pid]/status.
// Used by fallback functions when gopsutil is unavailable.
func (c *Collector) getUIDFromStatusFile(statusFile string) (int, error) {
	file, err := os.Open(statusFile)
	if err != nil {
		return 0, err
	}
	defer func() { _ = file.Close() }()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "Uid:") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				uid, err := strconv.Atoi(fields[1])
				if err != nil {
					return 0, err
				}
				return uid, nil
			}
		}
	}

	return 0, fmt.Errorf("UID not found")
}

// GetAllUsersCPUUsage returns total CPU usage for all non-system users.
// It does not apply USER_INCLUDE_LIST or USER_EXCLUDE_LIST filters.
func (c *Collector) GetAllUsersCPUUsage() float64 {
	var totalUsage float64

	// Reuse data already collected by GetAllUserMetrics to avoid redundant scans.
	allMetrics := c.GetAllUserMetrics()
	for _, metrics := range allMetrics {
		totalUsage += metrics.CPUUsage
	}

	return totalUsage
}

// GetAllUsers returns all active non-system UIDs.
// It does not apply USER_INCLUDE_LIST or USER_EXCLUDE_LIST filters.
// It supports complete all_users monitoring.
func (c *Collector) GetAllUsers() []int {
	// Reuse data already collected by GetAllUserMetrics to avoid redundant scans.
	allMetrics := c.GetAllUserMetrics()
	users := make([]int, 0, len(allMetrics))
	for uid := range allMetrics {
		users = append(users, uid)
	}

	return users
}

// getUsername returns the username for a UID.
// It uses os/user.LookupId, which supports LDAP and NIS when CGO is enabled,
// and caches successful results for the configured TTL.
func (c *Collector) getUsername(uid int) string {
	// Check the cache first.
	if cachedUsername, valid := c.getCachedUsername(uid); valid {
		return cachedUsername
	}

	// Method 1 uses os/user.LookupId for LDAP and NIS support.
	// This requires a build with CGO_ENABLED=1.
	u, err := user.LookupId(fmt.Sprintf("%d", uid))
	if err == nil && u.Username != "" {
		c.cacheUsername(uid, u.Username) // Cache the result.
		return u.Username
	}

	// Method 2 falls back to /etc/passwd for local users.
	username, err := c.getUsernameFromPasswd(uid)
	if err == nil && username != "" {
		c.cacheUsername(uid, username) // Cache the result.
		return username
	}

	// Finally, use the UID as a string.
	username = strconv.Itoa(uid)
	c.cacheUsername(uid, username)
	return username
}

// getCachedUsername returns a valid cached username.
func (c *Collector) getCachedUsername(uid int) (string, bool) {
	c.usernameCacheMutex.RLock()
	defer c.usernameCacheMutex.RUnlock()

	username, exists := c.usernameCache[uid]
	if !exists {
		return "", false
	}

	// Check whether the cache entry has expired.
	timestamp, exists := c.usernameCacheTime[uid]
	if !exists || time.Since(timestamp) > c.usernameCacheTTL {
		return "", false
	}

	return username, true
}

// cacheUsername stores a username in the cache with LRU eviction.
func (c *Collector) cacheUsername(uid int, username string) {
	c.usernameCacheMutex.Lock()
	defer c.usernameCacheMutex.Unlock()

	// If cache is full, remove oldest entry (LRU eviction)
	if len(c.usernameCache) >= MAX_USERNAME_CACHE_SIZE {
		oldestUID := 0
		var oldestTime time.Time
		found := false

		for uid, ts := range c.usernameCacheTime {
			if !found || ts.Before(oldestTime) {
				oldestTime = ts
				oldestUID = uid
				found = true
			}
		}

		if found {
			delete(c.usernameCache, oldestUID)
			delete(c.usernameCacheTime, oldestUID)
			c.logger.Debug("Username cache full - evicted oldest entry",
				"evicted_uid", oldestUID,
				"cache_size", len(c.usernameCache))
		}
	}

	c.usernameCache[uid] = username
	c.usernameCacheTime[uid] = time.Now()
}

// SetUsernameCacheTTL updates the username cache lifetime.
func (c *Collector) SetUsernameCacheTTL(ttl time.Duration) {
	c.usernameCacheMutex.Lock()
	c.usernameCacheTTL = ttl
	c.usernameCacheMutex.Unlock()
	c.logger.Debug("Username cache TTL updated", "ttl", ttl)
}

// GetUsernameCacheTTL returns the current username cache TTL.
func (c *Collector) GetUsernameCacheTTL() time.Duration {
	c.usernameCacheMutex.RLock()
	defer c.usernameCacheMutex.RUnlock()
	return c.usernameCacheTTL
}

// getUsernameFromPasswd reads a username from /etc/passwd without CGO.
func (c *Collector) getUsernameFromPasswd(uid int) (string, error) {
	file, err := os.Open("/etc/passwd")
	if err != nil {
		return "", err
	}
	defer func() { _ = file.Close() }()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "#") {
			continue // Skip comments.
		}

		fields := strings.Split(line, ":")
		if len(fields) >= 3 {
			// Field 0 is the username; field 2 is the UID string.
			fileUID, err := strconv.Atoi(fields[2])
			if err == nil && fileUID == uid {
				return fields[0], nil
			}
		}
	}

	return "", fmt.Errorf("UID %d not found in /etc/passwd", uid)
}

// GetUsernameFromUID returns the username for a UID.
func (c *Collector) GetUsernameFromUID(uid int) string {
	return c.getUsername(uid)
}

// GetMemoryUsage returns memory usage in MB.
func (c *Collector) GetMemoryUsage() float64 {
	cacheKey := "memory_usage"
	if val, valid := c.getFromCache(cacheKey); valid {
		return val.(float64)
	}

	vm, err := mem.VirtualMemory()
	if err != nil {
		c.logger.Warn("Failed to get memory info via gopsutil, using /proc/meminfo fallback",
			"error", err,
		)
		return c.getMemoryUsageFallback()
	}

	// Convert bytes to MB.
	usageMB := float64(vm.Used) / 1024 / 1024
	c.setInCache(cacheKey, usageMB, c.metricsCacheTTL())
	return usageMB
}

// getMemoryUsageFallback reads memory usage from /proc/meminfo.
func (c *Collector) getMemoryUsageFallback() float64 {
	file, err := os.Open("/proc/meminfo")
	if err != nil {
		c.logger.Error("Failed to open /proc/meminfo for memory calculation",
			"error", err,
			"fallback", "returning 0.0",
		)
		return 0.0
	}
	defer func() { _ = file.Close() }()

	var memTotal, memAvailable float64
	scanner := bufio.NewScanner(file)

	for scanner.Scan() {
		line := scanner.Text()
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}

		switch fields[0] {
		case "MemTotal:":
			memTotal, _ = strconv.ParseFloat(fields[1], 64)
		case "MemAvailable:":
			memAvailable, _ = strconv.ParseFloat(fields[1], 64)
		}

		//if memTotal > 0 && memAvailable > 0 {
		//    break
		//}
	}

	if memTotal == 0 {
		return 0.0
	}

	// Fall back to MemFree when MemAvailable was not found.
	if memAvailable == 0 {
		// Avoid a second read and report zero when MemFree was not retained.
		memAvailable = 0
	}

	// MemTotal and MemAvailable are in KiB; convert them to MiB.
	usageMB := (memTotal - memAvailable) / 1024
	return usageMB
}

// GetTotalMemoryMB returns total physical RAM in MB.
func (c *Collector) GetTotalMemoryMB() float64 {
	cacheKey := "total_memory"
	if val, valid := c.getFromCache(cacheKey); valid {
		return val.(float64)
	}

	vm, err := mem.VirtualMemory()
	if err != nil {
		c.logger.Warn("Failed to get total memory via gopsutil, using /proc/meminfo fallback",
			"error", err,
		)
		return c.getTotalMemoryFallback()
	}

	totalMB := float64(vm.Total) / 1024 / 1024
	c.setInCache(cacheKey, totalMB, c.metricsCacheTTL())
	return totalMB
}

// getTotalMemoryFallback reads MemTotal from /proc/meminfo.
func (c *Collector) getTotalMemoryFallback() float64 {
	file, err := os.Open("/proc/meminfo")
	if err != nil {
		c.logger.Error("Failed to open /proc/meminfo for total memory",
			"error", err,
			"fallback", "returning 0.0",
		)
		return 0.0
	}
	defer func() { _ = file.Close() }()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "MemTotal:") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				kb, _ := strconv.ParseFloat(fields[1], 64)
				return kb / 1024
			}
		}
	}
	return 0.0
}

// GetCachedMemoryMB returns cached system memory in MB.
func (c *Collector) GetCachedMemoryMB() float64 {
	cacheKey := "cached_memory"
	if val, valid := c.getFromCache(cacheKey); valid {
		return val.(float64)
	}

	vm, err := mem.VirtualMemory()
	if err != nil {
		c.logger.Warn("Failed to get cached memory via gopsutil, using /proc/meminfo fallback",
			"error", err,
		)
		return c.getCachedMemoryFallback()
	}

	cachedMB := float64(vm.Cached) / 1024 / 1024
	c.setInCache(cacheKey, cachedMB, c.metricsCacheTTL())
	return cachedMB
}

// getCachedMemoryFallback reads Cached from /proc/meminfo.
func (c *Collector) getCachedMemoryFallback() float64 {
	file, err := os.Open("/proc/meminfo")
	if err != nil {
		c.logger.Error("Failed to open /proc/meminfo for cached memory",
			"error", err,
			"fallback", "returning 0.0",
		)
		return 0.0
	}
	defer func() { _ = file.Close() }()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "Cached:") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				kb, _ := strconv.ParseFloat(fields[1], 64)
				return kb / 1024
			}
		}
	}
	return 0.0
}

// IsSystemUnderLoad reports whether the system load exceeds the configured heuristic.
func (c *Collector) IsSystemUnderLoad() bool {
	cacheKey := "system_under_load"
	if val, valid := c.getFromCache(cacheKey); valid { // Load detection owns a short TTL.
		return val.(bool)
	}

	// Calculate the load average.
	load, cores, err := c.getLoadAverage()
	if err != nil {
		c.logger.Warn("Failed to get load average, assuming system not under load",
			"error", err,
		)
		return false
	}

	// Treat the system as loaded when load exceeds 0.7 per core.
	underLoad := load > float64(cores)*0.7

	c.setInCache(cacheKey, underLoad, 10*time.Second)
	return underLoad
}

// getLoadAverage returns the load average and core count.
func (c *Collector) getLoadAverage() (float64, int, error) {
	data, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return 0.0, 0, fmt.Errorf("failed to read /proc/loadavg: %w", err)
	}

	fields := strings.Fields(string(data))
	if len(fields) == 0 {
		return 0.0, 0, fmt.Errorf("invalid loadavg format (empty file)")
	}

	load1, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return 0.0, 0, fmt.Errorf("failed to parse load average value '%s': %w", fields[0], err)
	}

	cores := c.GetTotalCores()
	return load1, cores, nil
}

// isMonitoredUserUID checks if a UID falls within the monitored range.
func (c *Collector) isMonitoredUserUID(uid int) bool {
	cfg := c.getConfig()
	return uid >= cfg.GetSystemUIDMin() && uid <= cfg.GetSystemUIDMax()
}

// getFromCache returns a cached value while its owning TTL remains valid.
func (c *Collector) getFromCache(key string) (interface{}, bool) {
	c.cacheMutex.RLock()
	defer c.cacheMutex.RUnlock()

	entry, exists := c.cache[key]
	if !exists {
		return nil, false
	}
	if entry.ttl <= 0 || c.currentTime().Sub(entry.storedAt) > entry.ttl {
		return nil, false
	}
	return entry.value, true
}

// setInCache stores a value with the TTL of the read that populated it.
func (c *Collector) setInCache(key string, value interface{}, ttl time.Duration) {
	now := c.currentTime()
	c.cacheMutex.Lock()
	defer c.cacheMutex.Unlock()

	if _, replacing := c.cache[key]; !replacing && len(c.cache) >= MAX_CACHE_SIZE {
		oldestKey := ""
		oldestTime := now
		found := false

		for k, entry := range c.cache {
			if !found || entry.storedAt.Before(oldestTime) {
				oldestTime = entry.storedAt
				oldestKey = k
				found = true
			}
		}

		if found {
			delete(c.cache, oldestKey)
			c.logger.Debug("Cache full - evicted oldest entry",
				"evicted_key", oldestKey,
				"cache_size", len(c.cache))
		}
	}

	c.cache[key] = metricCacheEntry{value: value, storedAt: now, ttl: ttl}
}

// periodicCleanup runs cleanup periodically until stopped
func (c *Collector) periodicCleanup() {
	defer close(c.cleanupDone)
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			c.cleanupCache()
		case <-c.stopCleanup:
			c.cleanupCache()
			return
		}
	}
}

// cleanupCache removes expired metric values and username resolutions.
func (c *Collector) cleanupCache() {
	c.cleanupCacheAt(c.currentTime())
}

func (c *Collector) cleanupCacheAt(now time.Time) {
	c.cacheMutex.Lock()
	for key, entry := range c.cache {
		if entry.ttl <= 0 || now.Sub(entry.storedAt) > entry.ttl {
			delete(c.cache, key)
		}
	}
	c.cacheMutex.Unlock()

	// Remove username cache entries that have exceeded their independent TTL.
	c.usernameCacheMutex.Lock()
	cleanedCount := 0
	for uid, timestamp := range c.usernameCacheTime {
		if now.Sub(timestamp) > c.usernameCacheTTL {
			delete(c.usernameCache, uid)
			delete(c.usernameCacheTime, uid)
			cleanedCount++
		}
	}
	remainingUsernames := len(c.usernameCache)
	c.usernameCacheMutex.Unlock()

	if cleanedCount > 0 {
		c.logger.Debug("Username cache cleanup completed",
			"cleaned_entries", cleanedCount,
			"remaining", remainingUsernames,
		)
	}
}

// ClearCache removes every cached metric value.
func (c *Collector) ClearCache() {
	c.cacheMutex.Lock()
	defer c.cacheMutex.Unlock()

	c.cache = make(map[string]metricCacheEntry)
}

// UpdateConfig replaces the collector configuration used by subsequent scans.
func (c *Collector) UpdateConfig(newConfig *config.Config) {
	leaveScan := c.userMetricsScan.Enter()

	c.mu.Lock()
	oldConfig := c.cfg
	c.cfg = newConfig
	c.mu.Unlock()
	processPolicyChanged := oldConfig == nil || !slices.Equal(
		oldConfig.GetProcessExcludeList(),
		newConfig.GetProcessExcludeList(),
	)
	if processPolicyChanged {
		resetEnforceableEMA(c.observationState)
		resetEnforceableEMA(c.decisionState)
	}
	c.usernameCacheMutex.Lock()
	c.usernameCacheTTL = time.Duration(newConfig.UsernameCacheTTL) * time.Minute
	c.usernameCacheMutex.Unlock()
	// Clear cached values so the new configuration takes effect immediately.
	c.ClearCache()
	leaveScan()

	c.logger.Info("Metrics collector configuration updated",
		"metrics_cache_ttl", newConfig.MetricsCacheTTL,
		"username_cache_ttl_minutes", newConfig.UsernameCacheTTL,
		"system_uid_min", newConfig.SystemUIDMin,
		"system_uid_max", newConfig.SystemUIDMax,
		"user_exclude_list", newConfig.GetUserExcludeList(),
	)
}

func resetEnforceableEMA(state *userMetricsSamplingState) {
	if state == nil || state.ema == nil {
		return
	}
	state.ema.mu.Lock()
	state.ema.enforceableValues = make(map[int]float64)
	state.ema.mu.Unlock()
}

// GetObservationMetrics returns a typed observation snapshot for diagnostics
// and external status surfaces.
func (c *Collector) GetObservationMetrics() ObservationMetrics {
	allUsers := c.GetAllUsers()

	return ObservationMetrics{
		TotalCores:            c.GetTotalCores(),
		TotalCPUUsage:         c.GetTotalCPUUsage(),
		ObservedUsersCPUUsage: c.GetAllUsersCPUUsage(),
		ObservedUsersCount:    len(allUsers),
		MemoryUsageMB:         c.GetMemoryUsage(),
		TotalMemoryMB:         c.GetTotalMemoryMB(),
		SystemUnderLoad:       c.IsSystemUnderLoad(),
	}
}

// GetSystemLoad returns the one-minute load average.
func (c *Collector) GetSystemLoad() (float64, error) {
	data, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return 0.0, err
	}

	fields := strings.Fields(string(data))
	if len(fields) == 0 {
		return 0.0, fmt.Errorf("invalid loadavg format")
	}

	load1, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return 0.0, err
	}

	return load1, nil
}

const (
	observationUserMetricsCacheKey = "all_user_metrics_observation"
	decisionUserMetricsCacheKey    = "all_user_metrics_decision"
)

// GetAllUserMetrics returns an observation sample for all active users.
// Observation samples never advance decision baselines or smoothing.
func (c *Collector) GetAllUserMetrics() map[int]*UserMetrics {
	return c.getAllUserMetricsCached(
		observationUserMetricsCacheKey,
		c.observationState,
		c.collectAllUserMetrics,
	)
}

// GetAllUserMetricsForDecision returns the control cycle's authoritative sample.
// Its temporal state and cache are independent from observation refreshes.
func (c *Collector) GetAllUserMetricsForDecision() map[int]*UserMetrics {
	return c.getAllUserMetricsCached(
		decisionUserMetricsCacheKey,
		c.decisionState,
		c.collectAllUserMetrics,
	)
}

func (c *Collector) getAllUserMetricsCached(
	cacheKey string,
	state *userMetricsSamplingState,
	collect func(*userMetricsSamplingState) map[int]*UserMetrics,
) map[int]*UserMetrics {
	if val, valid := c.getFromCache(cacheKey); valid {
		if metrics, ok := val.(map[int]*UserMetrics); ok {
			return metrics
		}
	}

	leaveScan := c.userMetricsScan.Enter()
	defer leaveScan()

	// Another caller may have populated the cache while this caller waited.
	if val, valid := c.getFromCache(cacheKey); valid {
		if metrics, ok := val.(map[int]*UserMetrics); ok {
			return metrics
		}
	}

	userMetrics := collect(state)
	c.setInCache(cacheKey, userMetrics, c.metricsCacheTTL())
	return userMetrics
}

func (c *Collector) collectAllUserMetrics(state *userMetricsSamplingState) map[int]*UserMetrics {
	userMetrics := make(map[int]*UserMetrics)
	sampleTime := time.Now()

	// Use gopsutil for efficient process discovery
	procs, err := process.Processes()
	if err != nil {
		c.logger.Warn("Failed to get processes via gopsutil, falling back to /proc scan",
			"error", err,
		)
		return c.getAllUserMetricsFallback(state)
	}

	c.logger.Debug("GetAllUserMetrics: using gopsutil path",
		"process_count", len(procs),
	)

	// Pre-allocate with estimated capacity
	tempData := make(map[int]*userData, len(procs)/50)

	// Read system uptime once (needed for CPU average calculation)
	systemUptimeSeconds := c.getSystemUptimeSeconds()
	cfg := c.getConfig()

	seenPIDs := make(map[int32]struct{}, len(procs))
	identityFailures := procFSFailureSummary{
		access: procFSAccessExecutableIdentity,
		policy: "process_remains_enforceable",
	}
	ioFailures := procFSFailureSummary{
		access: procFSAccessIODecision,
		policy: "unknown_is_not_zero",
	}

	for _, p := range procs {
		// Get process UID
		uids, err := p.Uids()
		if err != nil || len(uids) == 0 {
			continue
		}
		uid := int(uids[0])

		if !c.isMonitoredUserUID(uid) {
			continue
		}
		identity, identityErr := processidentity.Read("/proc", int(p.Pid))
		if errors.Is(identityErr, os.ErrNotExist) {
			continue
		}
		seenPIDs[p.Pid] = struct{}{}

		// Initialize structure if it doesn't exist
		if tempData[uid] == nil {
			tempData[uid] = &userData{}
		}

		// Read CPU usage using gopsutil proc.Times()
		cpuUsage := c.getProcessCPUUsageSimpleWithHandle(state, p)

		// Prefer PSS so shared pages are divided among mappings instead of
		// counted once per process. Fall back to RSS when smaps is unavailable.
		var rss uint64
		memInfo, err := p.MemoryInfo()
		if err == nil && memInfo != nil {
			rss = memInfo.RSS
		}
		memoryUsage := c.getProcessMemoryUsageWithFallback(int(p.Pid), rss)

		// Calculate CPU average since process start
		cpuAvg := c.getProcessCPUAverage(p, systemUptimeSeconds)

		ioCounters, ioErr := c.getProcessIO(int(p.Pid))
		var ioDelta ProcessIODelta
		ioUnavailable := false
		if ioErr == nil {
			if startTime, startErr := p.CreateTime(); startErr == nil {
				ioDelta = updateProcessIOSample(state, p.Pid, startTime, ioCounters, sampleTime)
			} else {
				ioErr = fmt.Errorf("read process start time: %w", startErr)
			}
		}
		if ioErr != nil {
			if reportableProcFSFailure(ioErr) {
				ioUnavailable = true
				ioFailures.record(int(p.Pid), ioErr)
			}
			discardProcessIOBaseline(state, p.Pid)
		}
		sample := processUsage{
			cpuUsage:                               cpuUsage,
			cpuUsageAvg:                            cpuAvg,
			processCount:                           1,
			memoryUsage:                            memoryUsage,
			ioReadBytes:                            ioCounters.readBytes,
			ioWriteBytes:                           ioCounters.writeBytes,
			ioReadOps:                              ioCounters.readOps,
			ioWriteOps:                             ioCounters.writeOps,
			ioDelta:                                ioDelta,
			executableIdentityUnavailableProcesses: boolCount(identityErr != nil),
			ioUnavailableProcesses:                 boolCount(ioUnavailable),
		}
		selection := addProcessSample(tempData[uid], cfg, identity.Executable, identity.Comm, sample)
		if !selection.IdentityTrusted {
			identityFailures.record(int(p.Pid), identityErr)
		}
	}
	c.reportProcFSFailures(identityFailures, ioFailures)

	// Convert to UserMetrics with username
	for uid, data := range tempData {
		username := c.GetUsernameFromUID(uid)
		eligibility := cfg.EvaluateUserEligibility(username)

		cpuUsage := data.observed.cpuUsage

		// Calculate observed and enforceable EMA values independently.
		ema := calculateEMA(state, uid, cpuUsage)
		enforceableEMA := calculateEnforceableEMA(state, uid, data.enforceable.cpuUsage)

		userMetrics[uid] = &UserMetrics{
			UID:                                    uid,
			Username:                               username,
			CPUUsage:                               cpuUsage,
			CPUUsageAverage:                        data.observed.cpuUsageAvg,
			CPUUsageEMA:                            ema,
			MemoryUsage:                            data.observed.memoryUsage,
			ProcessCount:                           data.observed.processCount,
			EligibleForCPU:                         eligibility.EligibleForCPU,
			EligibleForRAM:                         eligibility.EligibleForRAM,
			EligibleForIO:                          eligibility.EligibleForIO,
			IOReadBytes:                            data.observed.ioReadBytes,
			IOWriteBytes:                           data.observed.ioWriteBytes,
			IOReadOps:                              data.observed.ioReadOps,
			IOWriteOps:                             data.observed.ioWriteOps,
			ExecutableIdentityUnavailableProcesses: data.observed.executableIdentityUnavailableProcesses,
			IOUnavailableProcesses:                 data.observed.ioUnavailableProcesses,
			EnforceableUsage:                       processSetMetrics(data.enforceable, enforceableEMA),
		}
	}

	retainProcessBaselines(state, seenPIDs)
	retainEMAUsers(state, userMetrics)
	return userMetrics
}

// getAllUserMetricsFallback scans /proc manually if gopsutil fails.
func (c *Collector) getAllUserMetricsFallback(state *userMetricsSamplingState) map[int]*UserMetrics {
	userMetrics := make(map[int]*UserMetrics)
	procDir := "/proc"
	sampleTime := time.Now()

	entries, err := os.ReadDir(procDir)
	if err != nil {
		c.logger.Warn("Failed to read /proc directory for user metrics",
			"error", err,
			"fallback", "returning empty metrics",
		)
		return userMetrics
	}

	estimatedUIDs := len(entries) / 50
	tempData := make(map[int]*userData, estimatedUIDs)
	seenPIDs := make(map[int32]struct{}, len(entries))
	identityFailures := procFSFailureSummary{
		access: procFSAccessExecutableIdentity,
		policy: "process_remains_enforceable",
	}
	ioFailures := procFSFailureSummary{
		access: procFSAccessIODecision,
		policy: "unknown_is_not_zero",
	}

	// Read system uptime once
	systemUptimeSeconds := c.getSystemUptimeSeconds()
	cfg := c.getConfig()

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}

		statusFile := filepath.Join(procDir, entry.Name(), "status")
		uid, err := c.getUIDFromStatusFile(statusFile)
		if err != nil || !c.isMonitoredUserUID(uid) {
			continue
		}
		identity, identityErr := processidentity.Read(procDir, pid)
		if errors.Is(identityErr, os.ErrNotExist) {
			continue
		}
		seenPIDs[int32(pid)] = struct{}{}

		if tempData[uid] == nil {
			tempData[uid] = &userData{}
		}

		cpuUsage := c.getProcessCPUUsageSimple(state, pid)
		memoryUsage := c.getProcessMemoryUsage(pid)

		// CPU average
		cpuAvg := 0.0
		proc, err := process.NewProcess(int32(pid))
		var startTime int64
		var startTimeErr error
		if err == nil {
			cpuAvg = c.getProcessCPUAverage(proc, systemUptimeSeconds)
			startTime, startTimeErr = proc.CreateTime()
		} else {
			startTimeErr = err
		}

		// IO
		ioCounters, ioErr := c.getProcessIO(pid)
		var ioDelta ProcessIODelta
		ioUnavailable := false
		if ioErr == nil && startTimeErr == nil && startTime != 0 {
			ioDelta = updateProcessIOSample(state, int32(pid), startTime, ioCounters, sampleTime)
		} else {
			if ioErr == nil {
				if startTimeErr != nil {
					ioErr = fmt.Errorf("read process start time: %w", startTimeErr)
				} else {
					ioErr = fmt.Errorf("read process start time: unavailable zero value")
				}
			}
			if reportableProcFSFailure(ioErr) {
				ioUnavailable = true
				ioFailures.record(pid, ioErr)
			}
			discardProcessIOBaseline(state, int32(pid))
		}
		sample := processUsage{
			cpuUsage:                               cpuUsage,
			cpuUsageAvg:                            cpuAvg,
			processCount:                           1,
			memoryUsage:                            memoryUsage,
			ioReadBytes:                            ioCounters.readBytes,
			ioWriteBytes:                           ioCounters.writeBytes,
			ioReadOps:                              ioCounters.readOps,
			ioWriteOps:                             ioCounters.writeOps,
			ioDelta:                                ioDelta,
			executableIdentityUnavailableProcesses: boolCount(identityErr != nil),
			ioUnavailableProcesses:                 boolCount(ioUnavailable),
		}
		selection := addProcessSample(tempData[uid], cfg, identity.Executable, identity.Comm, sample)
		if !selection.IdentityTrusted {
			identityFailures.record(pid, identityErr)
		}
	}
	c.reportProcFSFailures(identityFailures, ioFailures)

	for uid, data := range tempData {
		username := c.GetUsernameFromUID(uid)
		ema := calculateEMA(state, uid, data.observed.cpuUsage)
		enforceableEMA := calculateEnforceableEMA(state, uid, data.enforceable.cpuUsage)
		eligibility := cfg.EvaluateUserEligibility(username)
		userMetrics[uid] = &UserMetrics{
			UID:                                    uid,
			Username:                               username,
			CPUUsage:                               data.observed.cpuUsage,
			CPUUsageAverage:                        data.observed.cpuUsageAvg,
			CPUUsageEMA:                            ema,
			MemoryUsage:                            data.observed.memoryUsage,
			ProcessCount:                           data.observed.processCount,
			EligibleForCPU:                         eligibility.EligibleForCPU,
			EligibleForRAM:                         eligibility.EligibleForRAM,
			EligibleForIO:                          eligibility.EligibleForIO,
			IOReadBytes:                            data.observed.ioReadBytes,
			IOWriteBytes:                           data.observed.ioWriteBytes,
			IOReadOps:                              data.observed.ioReadOps,
			IOWriteOps:                             data.observed.ioWriteOps,
			ExecutableIdentityUnavailableProcesses: data.observed.executableIdentityUnavailableProcesses,
			IOUnavailableProcesses:                 data.observed.ioUnavailableProcesses,
			EnforceableUsage:                       processSetMetrics(data.enforceable, enforceableEMA),
		}
	}

	retainProcessBaselines(state, seenPIDs)
	retainEMAUsers(state, userMetrics)
	return userMetrics
}

func (c *Collector) reportProcFSFailures(summaries ...procFSFailureSummary) {
	for _, summary := range summaries {
		if summary.count == 0 {
			continue
		}
		c.logger.Error("Required procfs access unavailable; decision input remains conservative",
			"access", summary.access,
			"affected_processes", summary.count,
			"first_pid", summary.firstPID,
			"first_error", summary.firstErr,
			"policy", summary.policy,
		)
	}
}

// GetUserMemoryUsage returns total memory used by a user in bytes.
// Uses gopsutil for efficient process discovery and memory reading.
func (c *Collector) GetUserMemoryUsage(uid int) uint64 {
	if !c.isMonitoredUserUID(uid) {
		return 0
	}

	// Use data already collected by GetAllUserMetrics to avoid redundant scans
	allMetrics := c.GetAllUserMetrics()
	if metrics, exists := allMetrics[uid]; exists {
		return metrics.MemoryUsage
	}
	return 0
}

// GetAllUsersMemoryUsage returns total memory used by all non-system users.
// It does not apply USER_INCLUDE_LIST or USER_EXCLUDE_LIST filters.
func (c *Collector) GetAllUsersMemoryUsage() uint64 {
	var totalMemory uint64

	// Reuse data already collected by GetAllUserMetrics to avoid redundant scans.
	allMetrics := c.GetAllUserMetrics()
	for _, metrics := range allMetrics {
		totalMemory += metrics.MemoryUsage
	}

	return totalMemory
}

// getProcessMemoryUsage returns PSS when available and falls back to VmRSS.
func (c *Collector) getProcessMemoryUsage(pid int) uint64 {
	return c.getProcessMemoryUsageWithFallback(pid, c.getProcessRSS(pid))
}

func (c *Collector) getProcessMemoryUsageWithFallback(pid int, rss uint64) uint64 {
	file, err := os.Open(fmt.Sprintf("/proc/%d/smaps_rollup", pid))
	if err != nil {
		return rss
	}
	defer func() { _ = file.Close() }()

	pss, err := parseProcessPSS(file)
	if err != nil {
		return rss
	}
	return pss
}

func parseProcessPSS(r io.Reader) (uint64, error) {
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "Pss:") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return 0, fmt.Errorf("invalid Pss line %q", line)
		}
		kb, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			return 0, fmt.Errorf("invalid Pss value %q: %w", fields[1], err)
		}
		return kb * 1024, nil
	}
	if err := scanner.Err(); err != nil {
		return 0, fmt.Errorf("failed to read smaps_rollup: %w", err)
	}
	return 0, fmt.Errorf("pss not found in smaps_rollup")
}

func (c *Collector) getProcessRSS(pid int) uint64 {
	statusFile := fmt.Sprintf("/proc/%d/status", pid)
	data, err := os.ReadFile(statusFile)
	if err != nil {
		return 0
	}

	lines := strings.Split(string(data), "\n")
	for _, line := range lines {
		if strings.HasPrefix(line, "VmRSS:") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				// VmRSS is measured in KiB; convert it to bytes.
				kb, err := strconv.ParseUint(fields[1], 10, 64)
				if err != nil {
					return 0
				}
				return kb * 1024
			}
		}
	}

	return 0
}

// getProcessIO reads /proc/[pid]/io. Byte counters describe storage traffic,
// while syscall counters are syscr/syscw and do not represent block-device IOPS.
// An error means no reliable sample is available; callers must discard the
// temporal baseline instead of advancing it.
func (c *Collector) getProcessIO(pid int) (processIOCounters, error) {
	ioFile := fmt.Sprintf("/proc/%d/io", pid)
	data, err := os.ReadFile(ioFile)
	if err != nil {
		return processIOCounters{}, fmt.Errorf("read %s: %w", ioFile, err)
	}

	counters, err := parseProcessIO(data)
	if err != nil {
		return processIOCounters{}, fmt.Errorf("parse %s: %w", ioFile, err)
	}
	return counters, nil
}

func parseProcessIO(data []byte) (processIOCounters, error) {
	var counters processIOCounters
	seen := make(map[string]bool, 4)
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		parts := strings.Fields(line)
		if len(parts) != 2 {
			continue
		}

		key := strings.TrimSuffix(parts[0], ":")
		switch key {
		case "read_bytes":
			value, err := strconv.ParseUint(parts[1], 10, 64)
			if err != nil {
				return processIOCounters{}, fmt.Errorf("parse read_bytes: %w", err)
			}
			counters.readBytes = value
			seen[key] = true
		case "write_bytes":
			value, err := strconv.ParseUint(parts[1], 10, 64)
			if err != nil {
				return processIOCounters{}, fmt.Errorf("parse write_bytes: %w", err)
			}
			counters.writeBytes = value
			seen[key] = true
		case "syscr":
			value, err := strconv.ParseUint(parts[1], 10, 64)
			if err != nil {
				return processIOCounters{}, fmt.Errorf("parse syscr: %w", err)
			}
			counters.readOps = value
			seen[key] = true
		case "syscw":
			value, err := strconv.ParseUint(parts[1], 10, 64)
			if err != nil {
				return processIOCounters{}, fmt.Errorf("parse syscw: %w", err)
			}
			counters.writeOps = value
			seen[key] = true
		}
	}

	for _, key := range []string{"read_bytes", "write_bytes", "syscr", "syscw"} {
		if !seen[key] {
			return processIOCounters{}, fmt.Errorf("required counter %q is missing", key)
		}
	}
	return counters, nil
}

// getSystemUptimeSeconds reads /proc/uptime and returns system uptime in seconds.
// Returns 0 on error.
func (c *Collector) getSystemUptimeSeconds() float64 {
	data, err := os.ReadFile("/proc/uptime")
	if err != nil {
		c.logger.Debug("Failed to read /proc/uptime", "error", err)
		return 0
	}
	fields := strings.Fields(string(data))
	if len(fields) < 1 {
		return 0
	}
	uptime, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return 0
	}
	return uptime
}

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

func calculateProcessCPUAverage(totalCPUSeconds, processAgeSeconds float64) float64 {
	avgCPU := (totalCPUSeconds / processAgeSeconds) * 100.0
	if avgCPU < 0 {
		return 0
	}
	return avgCPU
}

// calculateEMA calculates exponential moving average for CPU usage.
// alpha = 0.3 (weight for new value, rest for previous EMA)
func calculateEMA(state *userMetricsSamplingState, uid int, currentValue float64) float64 {
	const alpha = 0.3

	state.ema.mu.Lock()
	defer state.ema.mu.Unlock()

	prevEMA, exists := state.ema.values[uid]
	if !exists {
		// First value: EMA = currentValue
		state.ema.values[uid] = currentValue
		return currentValue
	}

	ema := alpha*currentValue + (1-alpha)*prevEMA
	state.ema.values[uid] = ema
	return ema
}

func calculateEnforceableEMA(state *userMetricsSamplingState, uid int, currentValue float64) float64 {
	const alpha = 0.3

	state.ema.mu.Lock()
	defer state.ema.mu.Unlock()
	if state.ema.enforceableValues == nil {
		state.ema.enforceableValues = make(map[int]float64)
	}
	prevEMA, exists := state.ema.enforceableValues[uid]
	if !exists {
		state.ema.enforceableValues[uid] = currentValue
		return currentValue
	}
	ema := alpha*currentValue + (1-alpha)*prevEMA
	state.ema.enforceableValues[uid] = ema
	return ema
}

func retainEMAUsers(state *userMetricsSamplingState, active map[int]*UserMetrics) {
	state.ema.mu.Lock()
	defer state.ema.mu.Unlock()

	for uid := range state.ema.values {
		if _, exists := active[uid]; !exists {
			delete(state.ema.values, uid)
		}
	}
	for uid := range state.ema.enforceableValues {
		if _, exists := active[uid]; !exists {
			delete(state.ema.enforceableValues, uid)
		}
	}
}

// getProcessCPUUsageSimple calculates process CPU usage between two samples.
func (c *Collector) getProcessCPUUsageSimple(state *userMetricsSamplingState, pid int) float64 {
	proc, err := process.NewProcess(int32(pid))
	if err != nil {
		return 0
	}
	return c.getProcessCPUUsageSimpleWithHandle(state, proc)
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

func updateProcessCPUSample(state *userMetricsSamplingState, pid int32, startTime int64, times cpu.TimesStat, now time.Time) float64 {
	state.process.mu.Lock()
	defer state.process.mu.Unlock()

	if previousStart := state.process.procStartTime[pid]; previousStart != 0 && startTime != 0 && previousStart != startTime {
		delete(state.process.prevProcCPU, pid)
		delete(state.process.prevProcTime, pid)
	}

	// Calculate usage only when a previous sample exists.
	if prevTimes, ok := state.process.prevProcCPU[pid]; ok {
		if prevTime, ok := state.process.prevProcTime[pid]; ok {
			elapsed := now.Sub(prevTime).Seconds()
			if elapsed < 1 {
				return 0
			}

			delta := (times.User - prevTimes.User) + (times.System - prevTimes.System)

			state.process.prevProcCPU[pid] = times
			state.process.prevProcTime[pid] = now
			if startTime != 0 {
				state.process.procStartTime[pid] = startTime
			}

			if delta <= 0 {
				return 0
			}
			return (delta / elapsed) * cpuPercentMultiplier
		}
	}

	// Keep every process baseline until a completed scan proves that its PID disappeared.
	state.process.prevProcCPU[pid] = times
	state.process.prevProcTime[pid] = now
	state.process.procStartTime[pid] = startTime
	return 0
}

func updateProcessIOSample(
	state *userMetricsSamplingState,
	pid int32,
	startTime int64,
	counters processIOCounters,
	now time.Time,
) ProcessIODelta {
	if state == nil || state.process == nil || startTime == 0 {
		return ProcessIODelta{}
	}

	state.process.mu.Lock()
	defer state.process.mu.Unlock()

	previous, exists := state.process.prevProcIO[pid]
	state.process.prevProcIO[pid] = processIOSample{
		startTime: startTime,
		counters:  counters,
		sampledAt: now,
	}
	if !exists || previous.startTime != startTime {
		return ProcessIODelta{}
	}

	return ProcessIODelta{
		ReadBytes:  monotonicCounterDelta(counters.readBytes, previous.counters.readBytes),
		WriteBytes: monotonicCounterDelta(counters.writeBytes, previous.counters.writeBytes),
	}
}

func discardProcessIOBaseline(state *userMetricsSamplingState, pid int32) {
	if state == nil || state.process == nil {
		return
	}
	state.process.mu.Lock()
	delete(state.process.prevProcIO, pid)
	state.process.mu.Unlock()
}

func monotonicCounterDelta(current, previous uint64) uint64 {
	if current < previous {
		return 0
	}
	return current - previous
}

type processBaselinePruneResult struct {
	cpu int
	io  int
}

// retainProcessBaselines removes samples for PIDs absent from a completed scan.
// Rebuilding the maps also releases bucket capacity retained after PID churn.
func retainProcessBaselines(state *userMetricsSamplingState, seen map[int32]struct{}) processBaselinePruneResult {
	if state == nil || state.process == nil {
		return processBaselinePruneResult{}
	}

	state.process.mu.Lock()
	defer state.process.mu.Unlock()

	needsRebuild := len(state.process.prevProcCPU) != len(state.process.prevProcTime) ||
		len(state.process.prevProcCPU) != len(state.process.procStartTime)
	if !needsRebuild {
		for pid := range state.process.prevProcCPU {
			if _, ok := seen[pid]; !ok {
				needsRebuild = true
				break
			}
		}
	}
	if !needsRebuild {
		for pid := range state.process.prevProcIO {
			if _, ok := seen[pid]; !ok {
				needsRebuild = true
				break
			}
		}
	}
	if !needsRebuild {
		return processBaselinePruneResult{}
	}

	oldCPUSize := len(state.process.prevProcCPU)
	oldIOSize := len(state.process.prevProcIO)
	cpuCapacity := min(oldCPUSize, len(seen))
	prevProcCPU := make(map[int32]cpu.TimesStat, cpuCapacity)
	prevProcTime := make(map[int32]time.Time, cpuCapacity)
	procStartTime := make(map[int32]int64, cpuCapacity)
	for pid, times := range state.process.prevProcCPU {
		if _, ok := seen[pid]; !ok {
			continue
		}
		sampledAt, ok := state.process.prevProcTime[pid]
		if !ok {
			continue
		}
		prevProcCPU[pid] = times
		prevProcTime[pid] = sampledAt
		procStartTime[pid] = state.process.procStartTime[pid]
	}
	ioCapacity := min(oldIOSize, len(seen))
	prevProcIO := make(map[int32]processIOSample, ioCapacity)
	for pid, sample := range state.process.prevProcIO {
		if _, ok := seen[pid]; ok {
			prevProcIO[pid] = sample
		}
	}

	state.process.prevProcCPU = prevProcCPU
	state.process.prevProcTime = prevProcTime
	state.process.procStartTime = procStartTime
	state.process.prevProcIO = prevProcIO
	return processBaselinePruneResult{
		cpu: oldCPUSize - len(prevProcCPU),
		io:  oldIOSize - len(prevProcIO),
	}
}

// GetUserProcessCount returns the number of processes for a user.
// Uses gopsutil for efficient process discovery.
func (c *Collector) GetUserProcessCount(uid int) int {
	if !c.isMonitoredUserUID(uid) {
		return 0
	}

	// Use data already collected by GetAllUserMetrics to avoid redundant scans
	allMetrics := c.GetAllUserMetrics()
	if metrics, exists := allMetrics[uid]; exists {
		return metrics.ProcessCount
	}
	return 0
}

// WriteMetricsToDatabase writes one typed metrics batch when a database writer is configured.
func (c *Collector) WriteMetricsToDatabase(userMetrics map[int]*UserMetrics, system SystemPersistenceMetrics) error {
	c.mu.RLock()
	writer := c.dbWriter
	c.mu.RUnlock()

	if writer == nil {
		return nil
	}

	if err := writer.WriteMetricsBatch(userMetrics, system); err != nil {
		return fmt.Errorf("write metrics to database: %w", err)
	}

	writer.MarkWritten()
	return nil
}

// Stop stops the collector and its background goroutines
func (c *Collector) Stop() {
	close(c.stopCleanup)
	<-c.cleanupDone
}
