package processactivity

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/tklauser/go-sysconf"
)

// ProcStat is one coherent kernel stat read; start ticks never pass through float64.
type ProcStat struct {
	StartTicks    uint64
	UserSeconds   float64
	SystemSeconds float64
	RSSBytes      uint64
}

// ClockTicks reads the platform USER_HZ contract rather than assuming the kernel HZ.
func ClockTicks() (float64, error) {
	ticks, err := sysconf.Sysconf(sysconf.SC_CLK_TCK)
	if err != nil || ticks <= 0 {
		return 0, fmt.Errorf("process clock ticks unavailable: %v", err)
	}
	return float64(ticks), nil
}

// ReadStat reads exact process lifetime identity and CPU/RSS from one procfs file.
func ReadStat(procRoot string, pid int, ticks float64) (ProcStat, error) {
	body, err := os.ReadFile(filepath.Join(procRoot, strconv.Itoa(pid), "stat"))
	if err != nil {
		return ProcStat{}, fmt.Errorf("read process stat: %w", err)
	}
	return ParseStat(string(body), ticks)
}

// ParseStat handles embedded spaces and parentheses in the process comm field.
func ParseStat(body string, ticks float64) (ProcStat, error) {
	if ticks <= 0 || math.IsNaN(ticks) || math.IsInf(ticks, 0) {
		return ProcStat{}, fmt.Errorf("invalid process clock ticks")
	}
	end := strings.LastIndex(body, ")")
	if end < 0 {
		return ProcStat{}, fmt.Errorf("missing process stat comm delimiter")
	}
	fields := strings.Fields(body[end+1:])
	if len(fields) < 22 {
		return ProcStat{}, fmt.Errorf("truncated process stat")
	}
	// fields starts at field 3 (state): utime14, stime15, starttime22, rss24.
	values := make([]uint64, 4)
	for i, index := range []int{11, 12, 19, 21} {
		v, err := strconv.ParseUint(fields[index], 10, 64)
		if err != nil {
			return ProcStat{}, fmt.Errorf("invalid process stat field %d: %w", index+3, err)
		}
		values[i] = v
	}
	if values[3] > math.MaxUint64/uint64(os.Getpagesize()) {
		return ProcStat{}, fmt.Errorf("process RSS overflows bytes")
	}
	return ProcStat{StartTicks: values[2], UserSeconds: float64(values[0]) / ticks, SystemSeconds: float64(values[1]) / ticks, RSSBytes: values[3] * uint64(os.Getpagesize())}, nil
}
