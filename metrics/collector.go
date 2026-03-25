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
	"github.com/shirou/gopsutil/v3/mem"
	"github.com/shirou/gopsutil/v3/process"
)

const (
	cpuPercentMultiplier   = 100.0
	jiffiesPerSecond       = 100.0
	cpuUsageEstimateFactor = 0.1
	pageSizeBytes          = 4096
)

// UserMetrics contains metrics for a single user.
type UserMetrics struct {
	UID          int
	Username     string
	CPUUsage     float64 // CPU percentage
	MemoryUsage  uint64  // Memory in bytes (VmRSS)
	ProcessCount int     // Number of processes
	IsLimited    bool    // Whether user has CPU limits applied
}

// userData is a temporary structure for accumulating data per UID during /proc scan.
type userData struct {
	cpuUsage     float64
	memoryUsage  uint64
	processCount int
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
	prevProcCPU  map[int32]cpu.TimesStat
	prevProcTime map[int32]time.Time
	procCPUMutex sync.RWMutex

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
	MAX_CACHE_SIZE            = 10000  // Maximum number of entries in general cache
	MAX_PROC_CACHE_SIZE       = 5000   // Maximum number of entries in process CPU cache
	MAX_USERNAME_CACHE_SIZE   = 10000  // Maximum number of entries in username cache
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
		prevProcCPU:       make(map[int32]cpu.TimesStat),
		prevProcTime:      make(map[int32]time.Time),
		usernameCache:     make(map[int]string),
		usernameCacheTime: make(map[int]time.Time),
		usernameCacheTTL:  DEFAULT_USERNAME_CACHE_TTL,
		stopCleanup:       make(chan struct{}),
		cleanupDone:       make(chan struct{}),
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

// GetTotalCores restituisce il numero totale di core CPU.
func (c *Collector) GetTotalCores() int {
	cacheKey := "total_cores"
	if val, valid := c.getFromCache(cacheKey, 3600*time.Second); valid { // Cache lunga per questa metrica
		return val.(int)
	}

	cores, err := cpu.Counts(true)
	if err != nil {
		c.logger.Warn("Failed to get CPU core count, using fallback", "error", err)
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
		c.logger.Error("Failed to open /proc/cpuinfo", "error", err)
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
		cores = 1
	}

	return cores
}

// GetTotalCPUUsage restituisce l'uso totale della CPU in percentuale.
func (c *Collector) GetTotalCPUUsage() float64 {
	cacheKey := "total_cpu_usage"
	if val, valid := c.getFromCache(cacheKey, time.Duration(c.cfg.MetricsCacheTTL)*time.Second); valid {
		return val.(float64)
	}

	// Usa gopsutil per ottenere l'uso CPU con un intervallo breve
	percentages, err := cpu.Percent(100*time.Millisecond, false)
	if err != nil || len(percentages) == 0 {
		c.logger.Warn("Failed to get CPU usage via gopsutil, using fallback", "error", err)
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
		c.logger.Error("Failed to open /proc/stat", "error", err)
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
	if val, valid := c.getFromCache(cacheKey, time.Duration(c.cfg.MetricsCacheTTL)*time.Second); valid {
		return val.(float64)
	}

	var totalUsage float64
	var processCount int

	// Metodo: Usa gopsutil/process.CPUPercent() che gestisce internamente il delta
	processes, err := process.Processes()
	if err == nil {
		for _, p := range processes {
			// Ottieni l'UID del processo
			if uids, err := p.Uids(); err == nil && len(uids) > 0 {
				if int(uids[0]) == uid { // UID reale
					// Escludi processi di sistema
					pname, _ := p.Name()

					processCount++
					// CPUPercent() fa due letture internamente
					// La prima volta restituisce 0, ma le successive funzionano
					if cpuPercent, err := p.CPUPercent(); err == nil {
						c.logger.Debug("Process CPU usage",
							"pid", p.Pid,
							"uid", uid,
							"name", pname,
							"cpu_percent", cpuPercent,
						)
						totalUsage += cpuPercent
					}
				}
			}
		}
	}

	c.logger.Debug("User CPU usage calculated",
		"user", fmt.Sprintf("%s(%d)", c.getUsername(uid), uid),
		"process_count", processCount,
		"total_usage", totalUsage,
	)

	c.setInCache(cacheKey, totalUsage)
	return totalUsage
}

// getUserCPUUsageFallback usa ps per ottenere l'uso CPU (simile allo script Bash).
func (c *Collector) getUserCPUUsageFallback(uid int) float64 {
	// Costruisci il comando ps
	// cmd := fmt.Sprintf("ps -U %d -o pcpu=", uid)

	// Esegui il comando e parsa l'output
	// Nota: In produzione, useremmo os/exec invece di eseguire shell commands
	// Per ora implementiamo una versione semplificata
	return c.getUserCPUUsageFromProc(uid)
}

// getUserCPUUsageFromProc calcola l'uso CPU leggendo da /proc.
func (c *Collector) getUserCPUUsageFromProc(uid int) float64 {
	var totalUsage float64

	// Itera su tutte le directory in /proc
	procDir := "/proc"
	entries, err := os.ReadDir(procDir)
	if err != nil {
		c.logger.Warn("Failed to read /proc directory", "error", err)
		return 0.0
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		// Verifica se è una directory PID
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}

		// Leggi l'UID del processo
		statusFile := filepath.Join(procDir, entry.Name(), "status")
		procUID, err := c.getUIDFromStatusFile(statusFile)
		if err != nil || procUID != uid {
			continue
		}

		// Leggi l'uso CPU del processo
		cpuUsage, err := c.getProcessCPUUsage(pid)
		if err == nil {
			totalUsage += cpuUsage
		}
	}

	return totalUsage
}

// getUIDFromStatusFile legge l'UID da /proc/[pid]/status.
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

// getProcessCPUUsage calcola l'uso CPU di un singolo processo.
func (c *Collector) getProcessCPUUsage(pid int) (float64, error) {
	statFile := fmt.Sprintf("/proc/%d/stat", pid)

	// Leggi il file stat del processo
	content, err := os.ReadFile(statFile)
	if err != nil {
		return 0.0, err
	}

	// Parse dei dati del processo per calcolare l'uso CPU
	// Il formato di /proc/[pid]/stat è complesso
	// Per una implementazione semplifica, leggiamo il tempo CPU
	stats := strings.Fields(string(content))
	if len(stats) < 15 {
		return 0.0, fmt.Errorf("invalid stat format for PID %d", pid)
	}

	// Tempo CPU speso in user mode (jiffies) - campo 13
	// Tempo CPU speso in kernel mode (jiffies) - campo 14
	utime, err1 := strconv.ParseUint(stats[13], 10, 64)
	stime, err2 := strconv.ParseUint(stats[14], 10, 64)

	if err1 != nil || err2 != nil {
		return 0.0, fmt.Errorf("failed to parse CPU times for PID %d", pid)
	}

	// Per calcolare la percentuale CPU, dovremmo:
	// 1. Salvare i valori precedenti
	// 2. Calcolare la differenza tra due letture
	// 3. Dividere per il tempo trascorso

	// Per ora, restituiamo una stima molto semplificata
	// In una implementazione completa, dovremmo implementare la cache
	// e il calcolo delle differenze

	// Stima semplificata: (utime + stime) in jiffies
	// 1 jiffy = tipicamente 10ms = 0.01s
	totalJiffies := float64(utime + stime)

	// Converti in secondi (assumendo 100 jiffies/secondo)
	cpuSeconds := totalJiffies / 100.0

	// Per ottenere una percentuale, dovremmo dividere per il tempo di esecuzione
	// del processo. Per semplicità, restituiamo un valore basso.
	// In produzione, implementeremmo la logica completa.

	return cpuSeconds * 0.1, nil // Stima molto approssimativa
}

// GetAllUsersCPUUsage restituisce l'uso CPU totale di TUTTI gli utenti (UID >= SYSTEM_UID_MIN).
// NON applica filtri USER_INCLUDE_LIST o USER_EXCLUDE_LIST
func (c *Collector) GetAllUsersCPUUsage() float64 {
	cacheKey := "all_users_cpu_usage"
	if val, valid := c.getFromCache(cacheKey, time.Duration(c.cfg.MetricsCacheTTL)*time.Second); valid {
		return val.(float64)
	}

	var totalUsage float64

	// Ottieni TUTTI gli utenti (senza filtri)
	allUsers := c.GetAllUsers()
	for _, uid := range allUsers {
		totalUsage += c.GetUserCPUUsage(uid)
	}

	c.setInCache(cacheKey, totalUsage)
	return totalUsage
}

// GetLimitedUsersCPUUsage restituisce l'uso CPU totale solo degli utenti che passano i filtri.
// Applica USER_INCLUDE_LIST e USER_EXCLUDE_LIST
func (c *Collector) GetLimitedUsersCPUUsage() float64 {
	cacheKey := "limited_users_cpu_usage"
	if val, valid := c.getFromCache(cacheKey, time.Duration(c.cfg.MetricsCacheTTL)*time.Second); valid {
		return val.(float64)
	}

	var totalUsage float64

	// Ottieni solo utenti che passano i filtri
	limitedUsers := c.GetLimitedUsers()
	for _, uid := range limitedUsers {
		totalUsage += c.GetUserCPUUsage(uid)
	}

	c.setInCache(cacheKey, totalUsage)
	return totalUsage
}

// GetAllUsers restituisce la lista di TUTTI gli UID attivi non di sistema (UID >= SYSTEM_UID_MIN).
// NON applica filtri USER_INCLUDE_LIST o USER_EXCLUDE_LIST
// Usato per metriche "all_users" (monitoraggio completo)
func (c *Collector) GetAllUsers() []int {
	cacheKey := "all_users"
	if val, valid := c.getFromCache(cacheKey, time.Duration(c.cfg.MetricsCacheTTL)*time.Second); valid {
		return val.([]int)
	}

	uidMap := make(map[int]bool)

	// Metodo 1: Usa gopsutil
	processes, err := process.Processes()
	if err == nil {
		for _, p := range processes {
			if uids, err := p.Uids(); err == nil && len(uids) > 0 {
				uid := int(uids[0])
				if c.isValidUserUID(uid) {
					uidMap[uid] = true
				}
			}
		}
	} else {
		// Fallback: legge da /proc
		uidMap = c.getActiveUsersFromProc()
	}

	// Converti la mappa in slice
	users := make([]int, 0, len(uidMap))
	for uid := range uidMap {
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
	if val, valid := c.getFromCache(cacheKey, time.Duration(c.cfg.MetricsCacheTTL)*time.Second); valid {
		return val.([]int)
	}

	uidMap := make(map[int]bool)

	// Metodo 1: Usa gopsutil
	processes, err := process.Processes()
	if err == nil {
		for _, p := range processes {
			if uids, err := p.Uids(); err == nil && len(uids) > 0 {
				uid := int(uids[0])
				if c.isValidUserUID(uid) {
					// Check se l'utente è incluso nella include list
					username := c.getUsername(uid)
					if c.cfg.IsUserIncluded(username) {
						// Check se l'utente è escluso dalla exclude list
						if !c.cfg.IsUserExcluded(username) {
							uidMap[uid] = true
						}
					}
				}
			}
		}
	} else {
		// Fallback: legge da /proc
		uidMap = c.getActiveUsersFromProc()
	}

	// Converti la mappa in slice
	users := make([]int, 0, len(uidMap))
	for uid := range uidMap {
		users = append(users, uid)
	}

	c.setInCache(cacheKey, users)
	return users
}

// previousUsers memorizza la lista precedente di utenti per il confronto
var (
	previousUsers      []int
	previousUsersMutex sync.RWMutex
)

// areUsersEqual verifica se la lista degli utenti è cambiata rispetto al ciclo precedente
func (c *Collector) areUsersEqual(current []int) bool {
	previousUsersMutex.RLock()
	defer previousUsersMutex.RUnlock()

	if len(previousUsers) != len(current) {
		return false
	}

	// Crea mappe per confronto veloce
	currentMap := make(map[int]bool)
	for _, uid := range current {
		currentMap[uid] = true
	}

	for _, uid := range previousUsers {
		if !currentMap[uid] {
			return false
		}
	}

	return true
}

// setPreviousUsers memorizza la lista corrente per il prossimo confronto
func (c *Collector) setPreviousUsers(users []int) {
	previousUsersMutex.Lock()
	defer previousUsersMutex.Unlock()

	// Crea una copia per evitare race condition
	previousUsers = make([]int, len(users))
	copy(previousUsers, users)
}

// formatActiveUsers formatta una lista di UID come lista di "username(uid)"
func (c *Collector) formatActiveUsers(uids []int) []string {
	formatted := make([]string, len(uids))
	for i, uid := range uids {
		formatted[i] = fmt.Sprintf("%s(%d)", c.getUsername(uid), uid)
	}
	return formatted
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

// clearUsernameCache rimuove un entry dalla cache (utile se l'utente cambia)
func (c *Collector) clearUsernameCache(uid int) {
	c.usernameCacheMutex.Lock()
	defer c.usernameCacheMutex.Unlock()

	delete(c.usernameCache, uid)
	delete(c.usernameCacheTime, uid)
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

// getUsernameFromUID è un alias per coerenza con il resto del codice
func (c *Collector) getUsernameFromUID(uid int) string {
	return c.getUsername(uid)
}

// getActiveUsersFromProc legge gli utenti attivi da /proc.
func (c *Collector) getActiveUsersFromProc() map[int]bool {
	uidMap := make(map[int]bool)

	procDir := "/proc"
	entries, err := os.ReadDir(procDir)
	if err != nil {
		c.logger.Warn("Failed to read /proc directory", "error", err)
		return uidMap
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		// Verifica se è una directory PID
		if _, err := strconv.Atoi(entry.Name()); err != nil {
			continue
		}

		// Leggi l'UID
		statusFile := filepath.Join(procDir, entry.Name(), "status")
		if uid, err := c.getUIDFromStatusFile(statusFile); err == nil {
			if c.isValidUserUID(uid) {
				uidMap[uid] = true
			}
		}
	}

	return uidMap
}

// GetMemoryUsage restituisce l'uso della memoria in MB.
func (c *Collector) GetMemoryUsage() float64 {
	cacheKey := "memory_usage"
	if val, valid := c.getFromCache(cacheKey, time.Duration(c.cfg.MetricsCacheTTL)*time.Second); valid {
		return val.(float64)
	}

	vm, err := mem.VirtualMemory()
	if err != nil {
		c.logger.Warn("Failed to get memory info via gopsutil, using fallback", "error", err)
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
		c.logger.Error("Failed to open /proc/meminfo", "error", err)
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

// IsSystemUnderLoad determina se il sistema è sotto carico.
func (c *Collector) IsSystemUnderLoad() bool {
	cacheKey := "system_under_load"
	if val, valid := c.getFromCache(cacheKey, 10*time.Second); valid { // Cache breve
		return val.(bool)
	}

	// Calcola load average
	load, cores, err := c.getLoadAverage()
	if err != nil {
		c.logger.Warn("Failed to get load average", "error", err)
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
		return 0.0, 0, err
	}

	fields := strings.Fields(string(data))
	if len(fields) == 0 {
		return 0.0, 0, fmt.Errorf("invalid loadavg format")
	}

	load1, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return 0.0, 0, err
	}

	cores := c.GetTotalCores()
	return load1, cores, nil
}

// isValidUserUID verifica se un UID è un utente non di sistema.
func (c *Collector) isValidUserUID(uid int) bool {
	return uid >= c.cfg.SystemUIDMin && uid <= c.cfg.SystemUIDMax
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
	c.procCPUMutex.Lock()
	for pid, timestamp := range c.prevProcTime {
		if now.Sub(timestamp) > 5*time.Minute {
			delete(c.prevProcCPU, pid)
			delete(c.prevProcTime, pid)
		}
	}
	c.procCPUMutex.Unlock()

	// Pulisci anche la cache username (utenti non risolti da > TTL)
	c.usernameCacheMutex.Lock()
	for uid, timestamp := range c.usernameCacheTime {
		if now.Sub(timestamp) > c.usernameCacheTTL {
			delete(c.usernameCache, uid)
			delete(c.usernameCacheTime, uid)
		}
	}
	c.usernameCacheMutex.Unlock()
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
	c.cfg = newConfig
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
// Optimization: single /proc scan with pre-allocation and combined UID+name reading.
func (c *Collector) GetAllUserMetrics() map[int]*UserMetrics {
	cacheKey := "all_user_metrics"
	if val, valid := c.getFromCache(cacheKey, time.Duration(c.cfg.MetricsCacheTTL)*time.Second); valid {
		if metrics, ok := val.(map[int]*UserMetrics); ok {
			return metrics
		}
	}

	userMetrics := make(map[int]*UserMetrics)
	procDir := "/proc"

	entries, err := os.ReadDir(procDir)
	if err != nil {
		c.logger.Warn("Failed to read /proc directory for user metrics", "error", err)
		return userMetrics
	}

	// Pre-allocate with estimated capacity (reduces dynamic allocations)
	estimatedUIDs := len(entries) / 50  // Estimate: ~50 processes per average UID
	tempData := make(map[int]*userData, estimatedUIDs)

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}

		// Read process UID
		statusFile := filepath.Join(procDir, entry.Name(), "status")
		uid, err := c.getUIDFromStatusFile(statusFile)
		if err != nil || !c.isValidUserUID(uid) {
			continue
		}

		// Initialize structure if it doesn't exist
		if tempData[uid] == nil {
			tempData[uid] = &userData{}
		}

		// Count process (all processes, including those excluded from limits)
		tempData[uid].processCount++

		// Read CPU usage (all processes, including those excluded from limits)
		cpuUsage := c.getProcessCPUUsageSimple(pid)
		tempData[uid].cpuUsage += cpuUsage

		// Read memory usage (VmRSS in bytes) (all processes)
		memoryUsage := c.getProcessMemoryUsage(pid)
		tempData[uid].memoryUsage += memoryUsage
	}

	// Convert to UserMetrics with username
	for uid, data := range tempData {
		username := c.getUsernameFromUID(uid)
		userMetrics[uid] = &UserMetrics{
			UID:          uid,
			Username:     username,
			CPUUsage:     data.cpuUsage,
			MemoryUsage:  data.memoryUsage,
			ProcessCount: data.processCount,
			IsLimited:    c.cfg.IsUserWhitelisted(username),
		}
	}

	c.setInCache(cacheKey, userMetrics)
	return userMetrics
}

// GetUserMemoryUsage returns total memory used by a user in bytes.
func (c *Collector) GetUserMemoryUsage(uid int) uint64 {
	if !c.isValidUserUID(uid) {
		return 0
	}

	cacheKey := fmt.Sprintf("memory_usage_uid_%d", uid)
	if val, valid := c.getFromCache(cacheKey, time.Duration(c.cfg.MetricsCacheTTL)*time.Second); valid {
		return val.(uint64)
	}

	var totalMemory uint64

	procDir := "/proc"
	entries, err := os.ReadDir(procDir)
	if err != nil {
		c.logger.Warn("Failed to read /proc for memory stats", "error", err)
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

	c.setInCache(cacheKey, totalMemory)
	return totalMemory
}

// GetAllUsersMemoryUsage restituisce la memoria totale usata da TUTTI gli utenti (UID >= SYSTEM_UID_MIN).
// NON applica filtri USER_INCLUDE_LIST o USER_EXCLUDE_LIST
func (c *Collector) GetAllUsersMemoryUsage() uint64 {
	cacheKey := "all_users_memory_usage"
	if val, valid := c.getFromCache(cacheKey, time.Duration(c.cfg.MetricsCacheTTL)*time.Second); valid {
		return val.(uint64)
	}

	var totalMemory uint64

	allUsers := c.GetAllUsers()
	for _, uid := range allUsers {
		totalMemory += c.GetUserMemoryUsage(uid)
	}

	c.setInCache(cacheKey, totalMemory)
	return totalMemory
}

// GetLimitedUsersMemoryUsage restituisce la memoria totale usata solo dagli utenti che passano i filtri.
// Applica USER_INCLUDE_LIST e USER_EXCLUDE_LIST
func (c *Collector) GetLimitedUsersMemoryUsage() uint64 {
	cacheKey := "limited_users_memory_usage"
	if val, valid := c.getFromCache(cacheKey, time.Duration(c.cfg.MetricsCacheTTL)*time.Second); valid {
		return val.(uint64)
	}

	var totalMemory uint64

	limitedUsers := c.GetLimitedUsers()
	for _, uid := range limitedUsers {
		totalMemory += c.GetUserMemoryUsage(uid)
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

// getProcessCPUUsageSimple calcola l'uso CPU di un processo usando il delta tra due letture.
func (c *Collector) getProcessCPUUsageSimple(pid int) float64 {
	proc, err := process.NewProcess(int32(pid))
	if err != nil {
		return 0
	}

	// Ottieni tempi CPU attuali
	times, err := proc.Times()
	if err != nil {
		return 0
	}

	now := time.Now()
	pid32 := int32(pid)

	c.procCPUMutex.Lock()
	defer c.procCPUMutex.Unlock()

	// Controlla se abbiamo un campione precedente
	if prevTimes, ok := c.prevProcCPU[pid32]; ok {
		if prevTime, ok := c.prevProcTime[pid32]; ok {
			// Calcola tempo trascorso in secondi
			elapsed := now.Sub(prevTime).Seconds()
			if elapsed > 0 {
				// Calcola delta CPU (user + system)
				delta := (times.User - prevTimes.User) + (times.System - prevTimes.System)
				// Converti in percentuale
				cpuPercent := (delta / elapsed) * cpuPercentMultiplier

				// Aggiorna campione corrente
				c.prevProcCPU[pid32] = *times
				c.prevProcTime[pid32] = now

				return cpuPercent
			}
		}
	}

	// Primo campione: salva e ritorna 0
	// Se cache è piena, rimuovi entry più vecchia (LRU)
	if len(c.prevProcCPU) >= MAX_PROC_CACHE_SIZE {
		oldestPID := int32(0)
		oldestTime := time.Now()
		
		for pid, ts := range c.prevProcTime {
			if ts.Before(oldestTime) {
				oldestTime = ts
				oldestPID = pid
			}
		}
		
		if oldestPID != 0 {
			delete(c.prevProcCPU, oldestPID)
			delete(c.prevProcTime, oldestPID)
		}
	}
	
	c.prevProcCPU[pid32] = *times
	c.prevProcTime[pid32] = now
	return 0
}

// GetUserProcessCount restituisce il numero di processi di un utente.
func (c *Collector) GetUserProcessCount(uid int) int {
	if !c.isValidUserUID(uid) {
		return 0
	}

	cacheKey := fmt.Sprintf("process_count_uid_%d", uid)
	if val, valid := c.getFromCache(cacheKey, time.Duration(c.cfg.MetricsCacheTTL)*time.Second); valid {
		return val.(int)
	}

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

	c.setInCache(cacheKey, count)
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
