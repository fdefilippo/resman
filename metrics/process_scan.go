package metrics

import (
	"fmt"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"time"

	"github.com/fdefilippo/resman/config"
	"github.com/fdefilippo/resman/internal/processactivity"
	"github.com/fdefilippo/resman/internal/processidentity"
)

const maxProcessScanWorkers = 4

// processScanSample contains fresh I/O only. Workers never mutate a sampling stream.
type processScanSample struct {
	pid          int32
	uid          int
	valid        bool
	identity     processidentity.Identity
	identityErr  error
	stat         processactivity.ProcStat
	sampledAt    time.Time
	memoryBytes  uint64
	memorySource processactivity.MemorySource
	io           processIOCounters
	ioErr        error
	err          error
}

func (c *Collector) processWorkerCount() int {
	count := min(maxProcessScanWorkers, runtime.GOMAXPROCS(0))
	if c.processScanWorkers > 0 {
		count = min(maxProcessScanWorkers, c.processScanWorkers)
	}
	return max(1, count)
}

// readProcessSamples bounds simultaneous kernel work and joins every worker
// before the owner consumes results in the original enumeration order.
func (c *Collector) readProcessSamples(pids []int32, cfg *config.Config) []processScanSample {
	results := make([]processScanSample, len(pids))
	count := min(len(pids), c.processWorkerCount())
	if count == 0 {
		return results
	}
	if count == 1 {
		for i, pid := range pids {
			results[i] = c.readProcessSample(pid, cfg)
		}
		return results
	}
	jobs := make(chan int)
	var workers sync.WaitGroup
	workers.Add(count)
	for range count {
		go func() {
			defer workers.Done()
			for i := range jobs {
				results[i] = c.readProcessSample(pids[i], cfg)
			}
		}()
	}
	for i := range pids {
		jobs <- i
	}
	close(jobs)
	workers.Wait()
	return results
}

func (c *Collector) readProcessSample(pid int32, cfg *config.Config) processScanSample {
	sample := processScanSample{pid: pid}
	root := c.procRoot()
	// Bracket UID and executable too: a recycled PID must not mix old metadata
	// with a new lifetime. Unmonitored PIDs need only this initial stat read.
	var statErr error
	sample.stat, statErr = c.readProcessStat(root, int(pid), c.processClockTicks)
	sample.sampledAt = c.currentTime()
	uid, err := c.getUIDFromStatusFile(filepath.Join(root, strconv.Itoa(int(pid)), "status"))
	if err != nil {
		if !processidentity.Vanished(err) {
			sample.err = fmt.Errorf("read process UID: %w", err)
		}
		return sample
	}
	sample.uid = uid
	if uid < cfg.GetSystemUIDMin() || uid > cfg.GetSystemUIDMax() || uid > config.MaxValidUID {
		return sample
	}
	if statErr != nil {
		if !processidentity.Vanished(statErr) {
			sample.err = fmt.Errorf("read authoritative process stat: %w", statErr)
		}
		return sample
	}
	sample.identity, sample.identityErr = processidentity.Read(root, int(pid))
	if processidentity.Vanished(sample.identityErr) {
		return sample
	}
	sample.memoryBytes, sample.memorySource = c.getProcessMemorySample(int(pid), sample.stat.RSSBytes, true)
	sample.io, sample.ioErr = c.getProcessIO(int(pid))
	guard, err := c.readProcessStat(root, int(pid), c.processClockTicks)
	if err != nil {
		if !processidentity.Vanished(err) {
			sample.err = fmt.Errorf("guard authoritative process stat: %w", err)
		}
		return sample
	}
	if guard.StartTicks != sample.stat.StartTicks {
		sample.err = fmt.Errorf("process lifetime changed during authoritative sample")
		return sample
	}
	sample.valid = true
	return sample
}
