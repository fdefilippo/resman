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
	"fmt"
	//    "io"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/fdefilippo/resman/config"
	"github.com/fdefilippo/resman/logging"
	"github.com/shirou/gopsutil/v3/cpu"
	"github.com/shirou/gopsutil/v3/host"
	"github.com/shirou/gopsutil/v3/mem"
	"github.com/shirou/gopsutil/v3/process"
)

const (
	cpuPercentMultiplier = 100.0
)

// UserMetrics contains metrics for a single user.
type UserMetrics struct {
	UID             int
	Username        string
	CPUUsage        float64 // CPU percentage (instantaneous, last cycle)
	CPUUsageAverage float64 // CPU percentage average since process start
	CPUUsageEMA     float64 // CPU percentage exponential moving average (α=0.3)
	MemoryUsage     uint64  // Memory in bytes (VmRSS)
	ProcessCount    int     // Number of processes
	IsLimited       bool    // Whether user has CPU limits applied
	IOReadBytes     uint64  // Total bytes read from block devices
	IOWriteBytes    uint64  // Total bytes written to block devices
	IOReadOps       uint64  // Total read operations
	IOWriteOps      uint64  // Total write operations
}

// procCache holds CPU timing data for all PIDs.
// Uses single mutex instead of sharding for simplicity and deadlock safety.
type procCache struct {
	mu           sync.RWMutex
	prevProcCPU  map[int32]cpu.TimesStat
	prevProcTime map[int32]time.Time
}

// userData is a temporary structure for accumulating data per UID during /proc scan.
type userData struct {
	cpuUsage     float64
	cpuUsageAvg  float64
	processCount int
	memoryUsage  uint64
	ioReadBytes  uint64
	ioWriteBytes uint64
	ioReadOps    uint64
	ioWriteOps   uint64
}

// emaCache stores EMA values per UID between cycles.
type emaCache struct {
	mu     sync.RWMutex
	values map[int]float64 // uid -> EMA value
}

// Collector collects system metrics.
type Collector struct {
	cfg    *config.Config
	logger *logging.Logger
	mu     sync.RWMutex

	// Cache per le metriche
	cache           map[string]interface{}
	cacheTimestamps map[string]time.Time
	cacheMutex      sync.RWMutex

	// Stato precedente per calcolo delta CPU
	prevCPUStats cpu.TimesStat
	prevCPUTime  time.Time

	// Cache per CPU usage per processo (necessaria per calcolo delta)
	procCache *procCache // Single cache instead of sharding

	// EMA cache for CPU usage smoothing between cycles
	emaCache *emaCache

	// Database writer (opzionale)
	dbWriter *DBWriter

	// Cache per risoluzione UID -> username
	usernameCache      map[int]string    // UID -> username
	usernameCacheTime  map[int]time.Time // Timestamp ultima risoluzione
	usernameCacheMutex sync.RWMutex
	usernameCacheTTL   time.Duration // TTL della cache

	// Cleanup goroutine control
	stopCleanup chan struct{}
	cleanupDone chan struct{}
}

// Default Username Cache TTL
const (
	DEFAULT_USERNAME_CACHE_TTL = 60 * time.Minute
	MAX_CACHE_SIZE             = 10000 // Maximum number of entries in general cache
	MAX_PROC_CACHE_SIZE        = 5000  // Maximum number of entries in process CPU cache
	MAX_USERNAME_CACHE_SIZE    = 10000 // Maximum number of entries in username cache
)

// NewCollector crea un nuovo collettore di metriche.
func NewCollector(cfg *config.Config) (*Collector, error) {
	logger := logging.GetLogger()

	collector := &Collector{
		cfg:               cfg,
		logger:            logger,
		cache:             make(map[string]interface{}),
		cacheTimestamps:   make(map[string]time.Time),
		prevCPUTime:       time.Now(),
		usernameCache:     make(map[int]string),
		usernameCacheTime: make(map[int]time.Time),
		usernameCacheTTL:  DEFAULT_USERNAME_CACHE_TTL,
		stopCleanup:       make(chan struct{}),
		cleanupDone:       make(chan struct{}),
		procCache: &procCache{
			prevProcCPU:  make(map[int32]cpu.TimesStat),
			prevProcTime: make(map[int32]time.Time),
		},
		emaCache: &emaCache{
			values: make(map[int]float64),
		},
	}

	go collector.periodicCleanup()

	// Inizializza le statistiche CPU precedenti
	if stats, err := cpu.Times(false); err == nil && len(stats) > 0 {
		collector.prevCPUStats = stats[0]
	}

	logger.Info("Metrics collector initialized")
	return collector, nil
}

// SetDBWriter imposta il DBWriter per la persistenza delle metriche
func (c *Collector) SetDBWriter(writer *DBWriter) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.dbWriter = writer
	c.logger.Info("Database writer configured", "enabled", writer != nil)
}

// GetDBWriter restituisce il DBWriter corrente
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

// GetTotalCores restituisce il numero totale di core CPU.
func (c *Collector) GetTotalCores() int {
	cacheKey := "total_cores"
	if val, valid := c.getFromCache(cacheKey, 3600*time.Second); valid { // Cache lunga per questa metrica
		return val.(int)
	}

	cores, err := cpu.Counts(true)
	if err != nil {
		c.logger.Warn("Failed to get CPU core count via gopsutil, using /proc/cpuinfo fallback",
			"error", err,
		)
		// Fallback: leggi da /proc/cpuinfo
		cores = c.getTotalCoresFallback()
	}

	c.setInCache(cacheKey, cores)
	return cores
}

// getTotalCoresFallback è un fallback per ottenere il numero di core.
func (c *Collector) getTotalCoresFallback() int {
	file, err := os.Open("/proc/cpuinfo")
	if err != nil {
		c.logger.Error("Failed to open /proc/cpuinfo to read CPU cores",
			"error", err,
			"fallback", "returning 1 core",
		)
		return 1
	}
	defer file.Close()

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

// GetTotalCPUUsage restituisce l'uso totale della CPU in percentuale.
func (c *Collector) GetTotalCPUUsage() float64 {
	cacheKey := "total_cpu_usage"
	if val, valid := c.getFromCache(cacheKey, c.metricsCacheTTL()); valid {
		return val.(float64)
	}

	// Usa gopsutil per ottenere l'uso CPU con un intervallo breve
	percentages, err := cpu.Percent(100*time.Millisecond, false)
	if err != nil || len(percentages) == 0 {
		c.logger.Warn("Failed to get CPU usage via gopsutil, using /proc/stat fallback",
			"error", err,
			"percentages_empty", len(percentages) == 0,
		)
		// Fallback al metodo manuale
		return c.getTotalCPUUsageFallback()
	}

	usage := percentages[0]
	c.setInCache(cacheKey, usage)
	return usage
}

// getTotalCPUUsageFallback calcola l'uso CPU manualmente da /proc/stat.
func (c *Collector) getTotalCPUUsageFallback() float64 {
	file, err := os.Open("/proc/stat")
	if err != nil {
		c.logger.Error("Failed to open /proc/stat for CPU usage calculation",
			"error", err,
			"fallback", "returning 0.0",
		)
		return 0.0
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	if !scanner.Scan() {
		return 0.0
	}

	line := scanner.Text()
	if !strings.HasPrefix(line, "cpu ") {
		return 0.0
	}

	// Parse della linea CPU
	fields := strings.Fields(line)
	if len(fields) < 8 {
		return 0.0
	}

	// Calcola i tempi totali
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

	c.mu.Lock()
	defer c.mu.Unlock()

	// Calcola la differenza dal precedente campione
	if c.prevCPUStats.User == 0 && c.prevCPUStats.System == 0 && c.prevCPUStats.Idle == 0 {
		// Primo campione, salva e ritorna 0
		c.prevCPUStats = cpu.TimesStat{
			User:    float64(user),
			Nice:    float64(nice),
			System:  float64(system),
			Idle:    float64(idle),
			Iowait:  float64(iowait),
			Irq:     float64(irq),
			Softirq: float64(softirq),
			Steal:   float64(steal),
		}
		c.prevCPUTime = time.Now()
		return 0.0
	}

	// Calcola delta
	totalDelta := total - (uint64(c.prevCPUStats.User) + uint64(c.prevCPUStats.Nice) +
		uint64(c.prevCPUStats.System) + uint64(c.prevCPUStats.Idle) +
		uint64(c.prevCPUStats.Iowait) + uint64(c.prevCPUStats.Irq) +
		uint64(c.prevCPUStats.Softirq) + uint64(c.prevCPUStats.Steal))
	idleDelta := idle - uint64(c.prevCPUStats.Idle)

	// Aggiorna lo stato precedente
	c.prevCPUStats = cpu.TimesStat{
		User:    float64(user),
		Nice:    float64(nice),
		System:  float64(system),
		Idle:    float64(idle),
		Iowait:  float64(iowait),
		Irq:     float64(irq),
		Softirq: float64(softirq),
		Steal:   float64(steal),
	}

	if totalDelta == 0 {
		return 0.0
	}

	usage := cpuPercentMultiplier * float64(totalDelta-idleDelta) / float64(totalDelta)

	// Cache il risultato
	c.setInCache("total_cpu_usage", usage)

	return usage
}

// GetUserCPUUsage restituisce l'uso CPU per un utente specifico.
// Esclude i processi di sistema dalla blacklist
func (c *Collector) GetUserCPUUsage(uid int) float64 {
	if !c.isValidUserUID(uid) {
		return 0.0
	}

	cacheKey := fmt.Sprintf("cpu_usage_uid_%d", uid)
	if val, valid := c.getFromCache(cacheKey, c.metricsCacheTTL()); valid {
		return val.(float64)
	}

	// Use data already collected by GetAllUserMetrics to avoid redundant scans
	allMetrics := c.GetAllUserMetrics()
	var totalUsage float64
	if metrics, exists := allMetrics[uid]; exists {
		totalUsage = metrics.CPUUsage
	}

	c.setInCache(cacheKey, totalUsage)
	return totalUsage
}

// getUIDFromStatusFile legge l'UID da /proc/[pid]/status.
// Used by fallback functions when gopsutil is unavailable.
func (c *Collector) getUIDFromStatusFile(statusFile string) (int, error) {
	file, err := os.Open(statusFile)
	if err != nil {
		return 0, err
	}
	defer file.Close()

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

// GetAllUsersCPUUsage restituisce l'uso CPU totale di TUTTI gli utenti (UID >= SYSTEM_UID_MIN).
// NON applica filtri USER_INCLUDE_LIST o USER_EXCLUDE_LIST
func (c *Collector) GetAllUsersCPUUsage() float64 {
	cacheKey := "all_users_cpu_usage"
	if val, valid := c.getFromCache(cacheKey, c.metricsCacheTTL()); valid {
		return val.(float64)
	}

	var totalUsage float64

	// Utilizza i dati già raccolti da GetAllUserMetrics per evitare scansioni ridondanti
	allMetrics := c.GetAllUserMetrics()
	for _, metrics := range allMetrics {
		totalUsage += metrics.CPUUsage
	}

	c.setInCache(cacheKey, totalUsage)
	return totalUsage
}

// GetLimitedUsersCPUUsage restituisce l'uso CPU totale solo degli utenti che passano i filtri.
// Applica USER_INCLUDE_LIST e USER_EXCLUDE_LIST
func (c *Collector) GetLimitedUsersCPUUsage() float64 {
	cacheKey := "limited_users_cpu_usage"
	if val, valid := c.getFromCache(cacheKey, c.metricsCacheTTL()); valid {
		return val.(float64)
	}

	var totalUsage float64

	// Utilizza i dati già raccolti da GetAllUserMetrics e filtra per utenti limitabili
	allMetrics := c.GetAllUserMetrics()
	for _, metrics := range allMetrics {
		if metrics.IsLimited {
			totalUsage += metrics.CPUUsage
		}
	}

	c.setInCache(cacheKey, totalUsage)
	return totalUsage
}

// GetAllUsers restituisce la lista di TUTTI gli UID attivi non di sistema (UID >= SYSTEM_UID_MIN).
// NON applica filtri USER_INCLUDE_LIST o USER_EXCLUDE_LIST
// Usato per metriche "all_users" (monitoraggio completo)
func (c *Collector) GetAllUsers() []int {
	cacheKey := "all_users"
	if val, valid := c.getFromCache(cacheKey, c.metricsCacheTTL()); valid {
		return val.([]int)
	}

	// Utilizza i dati già raccolti da GetAllUserMetrics per evitare scansioni ridondanti
	allMetrics := c.GetAllUserMetrics()
	users := make([]int, 0, len(allMetrics))
	for uid := range allMetrics {
		users = append(users, uid)
	}

	c.setInCache(cacheKey, users)
	return users
}

// GetLimitedUsers restituisce la lista degli UID che passano i filtri per i limiti CPU.
// Applica USER_INCLUDE_LIST e USER_EXCLUDE_LIST
// Usato per metriche "limited_users" (sottoinsieme limitabile)
func (c *Collector) GetLimitedUsers() []int {
	cacheKey := "limited_users"
	if val, valid := c.getFromCache(cacheKey, c.metricsCacheTTL()); valid {
		return val.([]int)
	}

	// Utilizza i dati già raccolti da GetAllUserMetrics e filtra per utenti limitabili
	allMetrics := c.GetAllUserMetrics()
	users := make([]int, 0, len(allMetrics))
	for uid, metrics := range allMetrics {
		if metrics.IsLimited {
			users = append(users, uid)
		}
	}

	c.setInCache(cacheKey, users)
	return users
}

// getUsername ritorna la username dato un UID
// Usa os/user.LookupId() che supporta LDAP/NIS quando CGO è abilitato
// Implementa cache con TTL per migliorare le performance
func (c *Collector) getUsername(uid int) string {
	// Controllo cache prima di tutto
	if cachedUsername, valid := c.getCachedUsername(uid); valid {
		return cachedUsername
	}

	// Metodo 1: Usa os/user.LookupId() (supporta LDAP/NIS con CGO)
	// Questo funziona solo se compilato con CGO_ENABLED=1
	u, err := user.LookupId(fmt.Sprintf("%d", uid))
	if err == nil && u.Username != "" {
		c.cacheUsername(uid, u.Username) // Cache il risultato
		return u.Username
	}

	// Metodo 2: Fallback su /etc/passwd (solo utenti locali)
	username, err := c.getUsernameFromPasswd(uid)
	if err == nil && username != "" {
		c.cacheUsername(uid, username) // Cache il risultato
		return username
	}

	// Fallback finale: ritorna l'UID come stringa
	return fmt.Sprintf("%d", uid)
}

// getCachedUsername restituisce lo username dalla cache se valido
func (c *Collector) getCachedUsername(uid int) (string, bool) {
	c.usernameCacheMutex.RLock()
	defer c.usernameCacheMutex.RUnlock()

	username, exists := c.usernameCache[uid]
	if !exists {
		return "", false
	}

	// Controllo se la cache è scaduta
	timestamp, exists := c.usernameCacheTime[uid]
	if !exists || time.Since(timestamp) > c.usernameCacheTTL {
		return "", false
	}

	return username, true
}

// cacheUsername memorizza lo username nella cache con LRU eviction.
func (c *Collector) cacheUsername(uid int, username string) {
	c.usernameCacheMutex.Lock()
	defer c.usernameCacheMutex.Unlock()

	// If cache is full, remove oldest entry (LRU eviction)
	if len(c.usernameCache) >= MAX_USERNAME_CACHE_SIZE {
		oldestUID := 0
		oldestTime := time.Now()

		for uid, ts := range c.usernameCacheTime {
			if ts.Before(oldestTime) {
				oldestTime = ts
				oldestUID = uid
			}
		}

		if oldestUID != 0 {
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

// SetUsernameCacheTTL imposta il TTL della cache username
func (c *Collector) SetUsernameCacheTTL(ttl time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.usernameCacheTTL = ttl
	c.logger.Debug("Username cache TTL updated", "ttl", ttl)
}

// GetUsernameCacheTTL restituisce il TTL corrente della cache username
func (c *Collector) GetUsernameCacheTTL() time.Duration {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.usernameCacheTTL
}

// getUsernameFromPasswd legge il username da /etc/passwd senza usare CGO
func (c *Collector) getUsernameFromPasswd(uid int) (string, error) {
	file, err := os.Open("/etc/passwd")
	if err != nil {
		return "", err
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "#") {
			continue // Salta commenti
		}

		fields := strings.Split(line, ":")
		if len(fields) >= 3 {
			// Campo 0: username
			// Campo 2: UID (come stringa)
			fileUID, err := strconv.Atoi(fields[2])
			if err == nil && fileUID == uid {
				return fields[0], nil
			}
		}
	}

	return "", fmt.Errorf("UID %d not found in /etc/passwd", uid)
}

// GetUsernameFromUID ritorna la username dato un UID (public alias)
func (c *Collector) GetUsernameFromUID(uid int) string {
	return c.getUsername(uid)
}

// GetMemoryUsage restituisce l'uso della memoria in MB.
func (c *Collector) GetMemoryUsage() float64 {
	cacheKey := "memory_usage"
	if val, valid := c.getFromCache(cacheKey, c.metricsCacheTTL()); valid {
		return val.(float64)
	}

	vm, err := mem.VirtualMemory()
	if err != nil {
		c.logger.Warn("Failed to get memory info via gopsutil, using /proc/meminfo fallback",
			"error", err,
		)
		return c.getMemoryUsageFallback()
	}

	// Converti da byte a MB
	usageMB := float64(vm.Used) / 1024 / 1024
	c.setInCache(cacheKey, usageMB)
	return usageMB
}

// getMemoryUsageFallback legge l'uso memoria da /proc/meminfo.
func (c *Collector) getMemoryUsageFallback() float64 {
	file, err := os.Open("/proc/meminfo")
	if err != nil {
		c.logger.Error("Failed to open /proc/meminfo for memory calculation",
			"error", err,
			"fallback", "returning 0.0",
		)
		return 0.0
	}
	defer file.Close()

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

	// Se memAvailable non è stato trovato, usa MemFree come fallback
	if memAvailable == 0 {
		// Dovremmo rileggere il file per MemFree, ma per semplicità usiamo 0
		memAvailable = 0
	}

	// MemTotal e MemAvailable sono in KB, converti a MB
	usageMB := (memTotal - memAvailable) / 1024
	return usageMB
}

// GetTotalMemoryMB restituisce la RAM fisica totale del sistema in MB.
func (c *Collector) GetTotalMemoryMB() float64 {
	cacheKey := "total_memory"
	if val, valid := c.getFromCache(cacheKey, c.metricsCacheTTL()); valid {
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
	c.setInCache(cacheKey, totalMB)
	return totalMB
}

// getTotalMemoryFallback legge MemTotal da /proc/meminfo.
func (c *Collector) getTotalMemoryFallback() float64 {
	file, err := os.Open("/proc/meminfo")
	if err != nil {
		c.logger.Error("Failed to open /proc/meminfo for total memory",
			"error", err,
			"fallback", "returning 0.0",
		)
		return 0.0
	}
	defer file.Close()

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

// GetCachedMemoryMB restituisce la memoria cache del sistema in MB.
func (c *Collector) GetCachedMemoryMB() float64 {
	cacheKey := "cached_memory"
	if val, valid := c.getFromCache(cacheKey, c.metricsCacheTTL()); valid {
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
	c.setInCache(cacheKey, cachedMB)
	return cachedMB
}

// getCachedMemoryFallback legge Cached da /proc/meminfo.
func (c *Collector) getCachedMemoryFallback() float64 {
	file, err := os.Open("/proc/meminfo")
	if err != nil {
		c.logger.Error("Failed to open /proc/meminfo for cached memory",
			"error", err,
			"fallback", "returning 0.0",
		)
		return 0.0
	}
	defer file.Close()

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

// IsSystemUnderLoad determina se il sistema è sotto carico.
func (c *Collector) IsSystemUnderLoad() bool {
	cacheKey := "system_under_load"
	if val, valid := c.getFromCache(cacheKey, 10*time.Second); valid { // Cache breve
		return val.(bool)
	}

	// Calcola load average
	load, cores, err := c.getLoadAverage()
	if err != nil {
		c.logger.Warn("Failed to get load average, assuming system not under load",
			"error", err,
		)
		return false
	}

	// Sistema è sotto carico se load > 0.7 * cores
	underLoad := load > float64(cores)*0.7

	c.setInCache(cacheKey, underLoad)
	return underLoad
}

// getLoadAverage restituisce load average e numero di core.
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

// isValidUserUID verifica se un UID è un utente non di sistema.
func (c *Collector) isValidUserUID(uid int) bool {
	cfg := c.getConfig()
	return uid >= cfg.GetSystemUIDMin() && uid <= cfg.GetSystemUIDMax()
}

// getFromCache recupera un valore dalla cache se non è scaduto.
func (c *Collector) getFromCache(key string, ttl time.Duration) (interface{}, bool) {
	c.cacheMutex.RLock()
	defer c.cacheMutex.RUnlock()

	val, exists := c.cache[key]
	if !exists {
		return nil, false
	}

	timestamp, timestampExists := c.cacheTimestamps[key]
	if !timestampExists {
		return nil, false
	}

	if time.Since(timestamp) > ttl {
		return nil, false
	}

	return val, true
}

// setInCache memorizza un valore nella cache con LRU eviction.
func (c *Collector) setInCache(key string, value interface{}) {
	c.cacheMutex.Lock()
	defer c.cacheMutex.Unlock()

	// If cache is full, remove oldest entries (LRU eviction)
	if len(c.cache) >= MAX_CACHE_SIZE {
		oldestKey := ""
		oldestTime := time.Now()

		for k, ts := range c.cacheTimestamps {
			if ts.Before(oldestTime) {
				oldestTime = ts
				oldestKey = k
			}
		}

		if oldestKey != "" {
			delete(c.cache, oldestKey)
			delete(c.cacheTimestamps, oldestKey)
			c.logger.Debug("Cache full - evicted oldest entry",
				"evicted_key", oldestKey,
				"cache_size", len(c.cache))
		}
	}

	c.cache[key] = value
	c.cacheTimestamps[key] = time.Now()
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

// cleanupCache rimuove le voci scadute dalla cache.
func (c *Collector) cleanupCache() {
	c.cacheMutex.Lock()
	defer c.cacheMutex.Unlock()

	now := time.Now()
	for key, timestamp := range c.cacheTimestamps {
		if now.Sub(timestamp) > 5*time.Minute {
			delete(c.cache, key)
			delete(c.cacheTimestamps, key)
		}
	}

	// Pulisci anche la cache dei processi CPU (processi vecchi > 5 minuti)
	if c.procCache != nil {
		c.procCache.mu.Lock()
		for pid, timestamp := range c.procCache.prevProcTime {
			if now.Sub(timestamp) > 5*time.Minute {
				delete(c.procCache.prevProcCPU, pid)
				delete(c.procCache.prevProcTime, pid)
			}
		}
		c.procCache.mu.Unlock()
	}

	// Pulisci anche la cache username (utenti non risolti da > TTL)
	c.usernameCacheMutex.Lock()
	cleanedCount := 0
	for uid, timestamp := range c.usernameCacheTime {
		if now.Sub(timestamp) > c.usernameCacheTTL {
			delete(c.usernameCache, uid)
			delete(c.usernameCacheTime, uid)
			cleanedCount++
		}
	}
	c.usernameCacheMutex.Unlock()

	if cleanedCount > 0 {
		c.logger.Debug("Username cache cleanup completed",
			"cleaned_entries", cleanedCount,
			"remaining", len(c.usernameCache),
		)
	}
}

// ClearCache svuota la cache.
func (c *Collector) ClearCache() {
	c.cacheMutex.Lock()
	defer c.cacheMutex.Unlock()

	c.cache = make(map[string]interface{})
	c.cacheTimestamps = make(map[string]time.Time)
}

// UpdateConfig aggiorna la configurazione del collector
func (c *Collector) UpdateConfig(newConfig *config.Config) {
	c.mu.Lock()
	c.cfg = newConfig
	c.mu.Unlock()
	c.logger.Info("Metrics collector configuration updated",
		"metrics_cache_ttl", newConfig.MetricsCacheTTL,
		"system_uid_min", newConfig.SystemUIDMin,
		"system_uid_max", newConfig.SystemUIDMax,
		"user_exclude_list", newConfig.UserExcludeList,
	)
	// Pulisci la cache per applicare immediatamente i cambiamenti
	c.ClearCache()
}

// GetDetailedMetrics restituisce metriche dettagliate per debugging.
func (c *Collector) GetDetailedMetrics() map[string]interface{} {
	metrics := make(map[string]interface{})

	metrics["total_cores"] = c.GetTotalCores()
	metrics["total_cpu_usage"] = c.GetTotalCPUUsage()

	// ALL USERS metrics
	metrics["all_users_cpu_usage"] = c.GetAllUsersCPUUsage()
	metrics["all_users_memory_usage"] = c.GetAllUsersMemoryUsage()
	allUsers := c.GetAllUsers()
	metrics["all_users_count"] = len(allUsers)

	// LIMITED USERS metrics
	metrics["limited_users_cpu_usage"] = c.GetLimitedUsersCPUUsage()
	metrics["limited_users_memory_usage"] = c.GetLimitedUsersMemoryUsage()
	limitedUsers := c.GetLimitedUsers()
	metrics["limited_users_count"] = len(limitedUsers)

	metrics["memory_usage_mb"] = c.GetMemoryUsage()
	metrics["total_memory_mb"] = c.GetTotalMemoryMB()
	metrics["cached_memory_mb"] = c.GetCachedMemoryMB()
	metrics["system_under_load"] = c.IsSystemUnderLoad()

	// Uso CPU per utente (per ALL users)
	userCPU := make(map[int]float64)
	for _, uid := range allUsers {
		userCPU[uid] = c.GetUserCPUUsage(uid)
	}
	metrics["user_cpu_usage"] = userCPU

	// Informazioni sulla cache
	c.cacheMutex.RLock()
	metrics["cache_size"] = len(c.cache)
	c.cacheMutex.RUnlock()

	return metrics
}

// GetSystemLoad restituisce il load average di 1 minuto.
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

// GetAllUserMetrics returns metrics (CPU, memory, processes) for all active users.
// Uses gopsutil for efficient process discovery with single-pass aggregation.
func (c *Collector) GetAllUserMetrics() map[int]*UserMetrics {
	cacheKey := "all_user_metrics"
	if val, valid := c.getFromCache(cacheKey, c.metricsCacheTTL()); valid {
		if metrics, ok := val.(map[int]*UserMetrics); ok {
			return metrics
		}
	}

	userMetrics := make(map[int]*UserMetrics)

	// Use gopsutil for efficient process discovery
	procs, err := process.Processes()
	if err != nil {
		c.logger.Warn("Failed to get processes via gopsutil, falling back to /proc scan",
			"error", err,
		)
		return c.getAllUserMetricsFallback()
	}

	c.logger.Debug("GetAllUserMetrics: using gopsutil path",
		"process_count", len(procs),
	)

	// Pre-allocate with estimated capacity
	tempData := make(map[int]*userData, len(procs)/50)

	// Read system uptime once (needed for CPU average calculation)
	systemUptimeSeconds := c.getSystemUptimeSeconds()

	// Second pass for IO: collect for ALL visible PIDs (including system users like mysql)
	ioData := make(map[int]*userData)

	for _, p := range procs {
		// Get process UID
		uids, err := p.Uids()
		if err != nil || len(uids) == 0 {
			continue
		}
		uid := int(uids[0])

		if !c.isValidUserUID(uid) {
			continue
		}

		// Initialize structure if it doesn't exist
		if tempData[uid] == nil {
			tempData[uid] = &userData{}
		}

		// Count process
		tempData[uid].processCount++

		// Read CPU usage using gopsutil proc.Times()
		cpuUsage := c.getProcessCPUUsageSimpleWithHandle(p)
		tempData[uid].cpuUsage += cpuUsage

		// Read memory usage (RSS)
		memInfo, err := p.MemoryInfo()
		if err == nil && memInfo != nil {
			tempData[uid].memoryUsage += memInfo.RSS
		}

		// Calculate CPU average since process start
		cpuAvg := c.getProcessCPUAverage(p, systemUptimeSeconds)
		tempData[uid].cpuUsageAvg += cpuAvg
	}

	// Collect IO for ALL visible processes (including system users like mysql, root, etc.)
	// IO is useful for all processes, not just non-system users
	for _, p := range procs {
		uids, err := p.Uids()
		if err != nil || len(uids) == 0 {
			continue
		}
		uid := int(uids[0])

		if ioData[uid] == nil {
			ioData[uid] = &userData{}
		}

		rB, wB, rO, wO := c.getProcessIO(int(p.Pid))
		ioData[uid].ioReadBytes += rB
		ioData[uid].ioWriteBytes += wB
		ioData[uid].ioReadOps += rO
		ioData[uid].ioWriteOps += wO
	}

	// Merge IO data into main tempData
	for uid, ioD := range ioData {
		if !c.isValidUserUID(uid) {
			continue
		}
		if tempData[uid] == nil {
			tempData[uid] = &userData{}
		}
		tempData[uid].ioReadBytes += ioD.ioReadBytes
		tempData[uid].ioWriteBytes += ioD.ioWriteBytes
		tempData[uid].ioReadOps += ioD.ioReadOps
		tempData[uid].ioWriteOps += ioD.ioWriteOps
	}

	// Convert to UserMetrics with username
	for uid, data := range tempData {
		username := c.GetUsernameFromUID(uid)

		cpuUsage := data.cpuUsage

		// Calculate EMA for this user
		ema := c.calculateEMA(uid, cpuUsage)

		userMetrics[uid] = &UserMetrics{
			UID:             uid,
			Username:        username,
			CPUUsage:        cpuUsage,
			CPUUsageAverage: data.cpuUsageAvg,
			CPUUsageEMA:     ema,
			MemoryUsage:     data.memoryUsage,
			ProcessCount:    data.processCount,
			IsLimited:       c.getConfig().IsUserWhitelisted(username),
			IOReadBytes:     data.ioReadBytes,
			IOWriteBytes:    data.ioWriteBytes,
			IOReadOps:       data.ioReadOps,
			IOWriteOps:      data.ioWriteOps,
		}
	}

	c.setInCache(cacheKey, userMetrics)
	return userMetrics
}

// getAllUserMetricsFallback scans /proc manually if gopsutil fails.
func (c *Collector) getAllUserMetricsFallback() map[int]*UserMetrics {
	userMetrics := make(map[int]*UserMetrics)
	procDir := "/proc"

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
	ioData := make(map[int]*userData)

	// Read system uptime once
	systemUptimeSeconds := c.getSystemUptimeSeconds()

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
		if err != nil || !c.isValidUserUID(uid) {
			continue
		}

		if tempData[uid] == nil {
			tempData[uid] = &userData{}
		}

		tempData[uid].processCount++
		cpuUsage := c.getProcessCPUUsageSimple(pid)
		tempData[uid].cpuUsage += cpuUsage
		memoryUsage := c.getProcessMemoryUsage(pid)
		tempData[uid].memoryUsage += memoryUsage

		// CPU average
		proc, err := process.NewProcess(int32(pid))
		if err == nil {
			cpuAvg := c.getProcessCPUAverage(proc, systemUptimeSeconds)
			tempData[uid].cpuUsageAvg += cpuAvg
		}

		// IO
		if ioData[uid] == nil {
			ioData[uid] = &userData{}
		}
		rB, wB, rO, wO := c.getProcessIO(pid)
		ioData[uid].ioReadBytes += rB
		ioData[uid].ioWriteBytes += wB
		ioData[uid].ioReadOps += rO
		ioData[uid].ioWriteOps += wO
	}

	// Merge IO
	for uid, ioD := range ioData {
		if tempData[uid] == nil {
			tempData[uid] = &userData{}
		}
		tempData[uid].ioReadBytes += ioD.ioReadBytes
		tempData[uid].ioWriteBytes += ioD.ioWriteBytes
		tempData[uid].ioReadOps += ioD.ioReadOps
		tempData[uid].ioWriteOps += ioD.ioWriteOps
	}

	for uid, data := range tempData {
		username := c.GetUsernameFromUID(uid)
		ema := c.calculateEMA(uid, data.cpuUsage)
		userMetrics[uid] = &UserMetrics{
			UID:             uid,
			Username:        username,
			CPUUsage:        data.cpuUsage,
			CPUUsageAverage: data.cpuUsageAvg,
			CPUUsageEMA:     ema,
			MemoryUsage:     data.memoryUsage,
			ProcessCount:    data.processCount,
			IsLimited:       c.getConfig().IsUserWhitelisted(username),
			IOReadBytes:     data.ioReadBytes,
			IOWriteBytes:    data.ioWriteBytes,
			IOReadOps:       data.ioReadOps,
			IOWriteOps:      data.ioWriteOps,
		}
	}

	return userMetrics
}

// GetUserMemoryUsage returns total memory used by a user in bytes.
// Uses gopsutil for efficient process discovery and memory reading.
func (c *Collector) GetUserMemoryUsage(uid int) uint64 {
	if !c.isValidUserUID(uid) {
		return 0
	}

	cacheKey := fmt.Sprintf("memory_usage_uid_%d", uid)
	if val, valid := c.getFromCache(cacheKey, c.metricsCacheTTL()); valid {
		return val.(uint64)
	}

	// Use data already collected by GetAllUserMetrics to avoid redundant scans
	allMetrics := c.GetAllUserMetrics()
	var totalMemory uint64
	if metrics, exists := allMetrics[uid]; exists {
		totalMemory = metrics.MemoryUsage
	}

	c.setInCache(cacheKey, totalMemory)
	return totalMemory
}

// getUserMemoryUsageFallback scans /proc manually if gopsutil fails.
func (c *Collector) getUserMemoryUsageFallback(uid int) uint64 {
	var totalMemory uint64

	procDir := "/proc"
	entries, err := os.ReadDir(procDir)
	if err != nil {
		return 0
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}

		statusFile := filepath.Join(procDir, entry.Name(), "status")
		procUID, err := c.getUIDFromStatusFile(statusFile)
		if err != nil || procUID != uid {
			continue
		}

		memoryUsage := c.getProcessMemoryUsage(pid)
		totalMemory += memoryUsage
	}

	return totalMemory
}

// GetAllUsersMemoryUsage restituisce la memoria totale usata da TUTTI gli utenti (UID >= SYSTEM_UID_MIN).
// NON applica filtri USER_INCLUDE_LIST o USER_EXCLUDE_LIST
func (c *Collector) GetAllUsersMemoryUsage() uint64 {
	cacheKey := "all_users_memory_usage"
	if val, valid := c.getFromCache(cacheKey, c.metricsCacheTTL()); valid {
		return val.(uint64)
	}

	var totalMemory uint64

	// Utilizza i dati già raccolti da GetAllUserMetrics per evitare scansioni ridondanti
	allMetrics := c.GetAllUserMetrics()
	for _, metrics := range allMetrics {
		totalMemory += metrics.MemoryUsage
	}

	c.setInCache(cacheKey, totalMemory)
	return totalMemory
}

// GetLimitedUsersMemoryUsage restituisce la memoria totale usata solo dagli utenti che passano i filtri.
// Applica USER_INCLUDE_LIST e USER_EXCLUDE_LIST
func (c *Collector) GetLimitedUsersMemoryUsage() uint64 {
	cacheKey := "limited_users_memory_usage"
	if val, valid := c.getFromCache(cacheKey, c.metricsCacheTTL()); valid {
		return val.(uint64)
	}

	var totalMemory uint64

	// Utilizza i dati già raccolti da GetAllUserMetrics e filtra per utenti limitabili
	allMetrics := c.GetAllUserMetrics()
	for _, metrics := range allMetrics {
		if metrics.IsLimited {
			totalMemory += metrics.MemoryUsage
		}
	}

	c.setInCache(cacheKey, totalMemory)
	return totalMemory
}

// getProcessMemoryUsage restituisce la memoria RSS di un processo in bytes.
// Legge VmRSS da /proc/[pid]/status.
func (c *Collector) getProcessMemoryUsage(pid int) uint64 {
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
				// VmRSS è in kB, converti in bytes
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

// getProcessIO reads /proc/[pid]/io and returns readBytes, writeBytes, readOps, writeOps.
// Returns 0 for all values if the file doesn't exist or can't be read.
func (c *Collector) getProcessIO(pid int) (readBytes, writeBytes, readOps, writeOps uint64) {
	ioFile := fmt.Sprintf("/proc/%d/io", pid)
	data, err := os.ReadFile(ioFile)
	if err != nil {
		// Common errors: EACCES (ptrace restriction), ENOENT (process exited)
		// Silently ignore - process may have exited or ptrace restriction applies
		return 0, 0, 0, 0
	}

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
		val, parseErr := strconv.ParseUint(parts[1], 10, 64)
		if parseErr != nil {
			continue
		}

		switch key {
		case "read_bytes":
			readBytes = val
		case "write_bytes":
			writeBytes = val
		case "syscr":
			readOps = val
		case "syscw":
			writeOps = val
		}
	}

	return readBytes, writeBytes, readOps, writeOps
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

	// Average CPU% = (total CPU seconds / process age seconds) * 100
	avgCPU := (totalCPUSeconds / processAgeSeconds) * 100.0
	if avgCPU < 0 {
		return 0
	}
	if avgCPU > 100 {
		return 100
	}

	return avgCPU
}

// calculateEMA calculates exponential moving average for CPU usage.
// alpha = 0.3 (weight for new value, rest for previous EMA)
func (c *Collector) calculateEMA(uid int, currentValue float64) float64 {
	const alpha = 0.3

	c.emaCache.mu.Lock()
	defer c.emaCache.mu.Unlock()

	prevEMA, exists := c.emaCache.values[uid]
	if !exists {
		// First value: EMA = currentValue
		c.emaCache.values[uid] = currentValue
		return currentValue
	}

	ema := alpha*currentValue + (1-alpha)*prevEMA
	c.emaCache.values[uid] = ema
	return ema
}

// getProcessCPUUsageSimple calcola l'uso CPU di un processo usando il delta tra due letture.
func (c *Collector) getProcessCPUUsageSimple(pid int) float64 {
	proc, err := process.NewProcess(int32(pid))
	if err != nil {
		return 0
	}
	return c.getProcessCPUUsageSimpleWithHandle(proc)
}

// isProcessRunningLongEnough checks if a process is in "running" state (R)
// for at least 60 seconds. Returns true if stable, false if transient.
// For transient processes, returns 1% to avoid skewing metrics.
func (c *Collector) isProcessRunningLongEnough(proc *process.Process) bool {
	// Check process state
	statuses, err := proc.Status()
	if err != nil || len(statuses) == 0 {
		return false // Can't determine state, skip
	}

	// Check if process is in running state (R)
	isRunning := false
	for _, s := range statuses {
		if s == "R" || s == "running" {
			isRunning = true
			break
		}
	}

	if !isRunning {
		return false // Not in running state
	}

	// Check how long the process has been alive
	createTime, err := proc.CreateTime()
	if err != nil || createTime == 0 {
		return false // Can't determine age
	}

	// createTime is milliseconds since epoch
	processAgeSeconds := (float64(time.Now().UnixMilli()) - float64(createTime)) / 1000.0

	return processAgeSeconds >= 60
}

// getProcessCPUUsageSimpleWithHandle calcola l'uso CPU usando un handle gopsutil esistente.
// Più efficiente quando l'handle è già disponibile (evita chiamata a process.NewProcess).
// Se il processo non è in stato "running" da almeno 60 secondi, ritorna 0% per evitare
// di sfalsare le metriche con letture instabili (es. processi multithread appena avviati).
func (c *Collector) getProcessCPUUsageSimpleWithHandle(proc *process.Process) float64 {
	pid32 := proc.Pid

	// Check if process is stable (running for at least 60 seconds)
	if !c.isProcessRunningLongEnough(proc) {
		// Process is transient or not running - return 0% to avoid skewing metrics
		// The 1% floor will be applied at the user level, not per-process
		return 0
	}

	// Ottieni tempi CPU attuali
	times, err := proc.Times()
	if err != nil || c.procCache == nil {
		return 0
	}

	now := time.Now()

	c.procCache.mu.Lock()
	defer c.procCache.mu.Unlock()

	// Controlla se abbiamo un campione precedente
	if prevTimes, ok := c.procCache.prevProcCPU[pid32]; ok {
		if prevTime, ok := c.procCache.prevProcTime[pid32]; ok {
			// Calcola tempo trascorso in secondi
			elapsed := now.Sub(prevTime).Seconds()
			if elapsed > 0 {
				// Calcola delta CPU (user + system)
				delta := (times.User - prevTimes.User) + (times.System - prevTimes.System)
				// Converti in percentuale
				cpuPercent := (delta / elapsed) * cpuPercentMultiplier

				// Aggiorna campione corrente
				c.procCache.prevProcCPU[pid32] = *times
				c.procCache.prevProcTime[pid32] = now

				return cpuPercent
			}
		}
	}

	// Primo campione: salva e ritorna 0
	// Se cache è piena, rimuovi entry più vecchia (LRU)
	if len(c.procCache.prevProcCPU) >= MAX_PROC_CACHE_SIZE {
		// Find and remove oldest entry
		var oldestPID int32
		var oldestTime time.Time
		first := true
		for pid, ts := range c.procCache.prevProcTime {
			if first || ts.Before(oldestTime) {
				oldestTime = ts
				oldestPID = pid
				first = false
			}
		}
		if !first {
			delete(c.procCache.prevProcCPU, oldestPID)
			delete(c.procCache.prevProcTime, oldestPID)
		}
	}

	// Aggiungi nuova entry
	c.procCache.prevProcCPU[pid32] = *times
	c.procCache.prevProcTime[pid32] = now
	return 0
}

// GetUserProcessCount returns the number of processes for a user.
// Uses gopsutil for efficient process discovery.
func (c *Collector) GetUserProcessCount(uid int) int {
	if !c.isValidUserUID(uid) {
		return 0
	}

	cacheKey := fmt.Sprintf("process_count_uid_%d", uid)
	if val, valid := c.getFromCache(cacheKey, c.metricsCacheTTL()); valid {
		return val.(int)
	}

	// Use data already collected by GetAllUserMetrics to avoid redundant scans
	allMetrics := c.GetAllUserMetrics()
	count := 0
	if metrics, exists := allMetrics[uid]; exists {
		count = metrics.ProcessCount
	}

	c.setInCache(cacheKey, count)
	return count
}

// getUserProcessCountFallback scans /proc manually if gopsutil fails.
func (c *Collector) getUserProcessCountFallback(uid int) int {
	count := 0
	procDir := "/proc"

	entries, err := os.ReadDir(procDir)
	if err != nil {
		return 0
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		_, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}

		statusFile := filepath.Join(procDir, entry.Name(), "status")
		procUID, err := c.getUIDFromStatusFile(statusFile)
		if err == nil && procUID == uid {
			count++
		}
	}

	return count
}

// WriteMetricsToDatabase scrive le metriche nel database se il DBWriter è configurato
func (c *Collector) WriteMetricsToDatabase(userMetrics map[int]*UserMetrics, totalCPUUsage float64, totalCores int, systemLoad float64, limitsActive bool, limitedUsersCount int) {
	c.mu.RLock()
	writer := c.dbWriter
	c.mu.RUnlock()

	if writer == nil {
		return
	}

	// Scrivi metriche di sistema
	writer.WriteSystemMetrics(totalCPUUsage, totalCores, systemLoad, limitsActive, limitedUsersCount)

	// Scrivi metriche per ogni utente
	for uid, metrics := range userMetrics {
		writer.WriteUserMetrics(
			uid,
			metrics.Username,
			metrics.CPUUsage,
			metrics.MemoryUsage,
			metrics.ProcessCount,
			false, // isLimited verrà impostato dallo state manager
			"",    // cgroupPath
			"",    // cpuQuota
		)
	}

	writer.MarkWritten()
}

// Stop stops the collector and its background goroutines
func (c *Collector) Stop() {
	close(c.stopCleanup)
	<-c.cleanupDone
}
