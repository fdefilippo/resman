package cgroup

import (
	"errors"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// FuzzReadIOStatsFile exercises the io.stat reader whose output feeds the
// monotonic logical counters in io_accounting.go. Those counters are consumed
// as deltas, so a silently wrapped sum would surface as an enormous phantom
// delta rather than as an error.
func FuzzReadIOStatsFile(f *testing.F) {
	for _, seed := range []string{
		"8:0 rbytes=104857600 wbytes=52428800 rios=1234 wios=567\n",
		"8:0 rbytes=0 wbytes=0 rios=0 wios=0\n",
		"",
		"\n\n\n",
		"8:0\n",
		"8:0 rbytes=\n",
		"8:0 rbytes=-1\n",
		"8:0 rbytes=notanumber\n",
		"8:0 rbytes=18446744073709551615\n8:1 rbytes=18446744073709551615\n",
		"8:0 rbytes=1 rbytes=2 rbytes=3\n",
		"8:0 rbytes=1=2\n",
		"   8:0    rbytes=7   \n",
		strings.Repeat("8:0 rbytes=1\n", 256),
	} {
		f.Add(seed)
	}

	dir := f.TempDir()
	f.Fuzz(func(t *testing.T, content string) {
		path := filepath.Join(dir, "io.stat")
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Skip()
		}

		readBytes, writeBytes, readOps, writeOps, err := readIOStatsFile(path)

		overflow := false
		expected := make(map[string]uint64, 4)
		for _, key := range []string{"rbytes", "wbytes", "rios", "wios"} {
			sum, sumOverflow := exactIOStatSum(content, key)
			expected[key] = sum
			overflow = overflow || sumOverflow
		}
		if overflow {
			if !errors.Is(err, errCounterOverflow) {
				t.Fatalf("readIOStatsFile overflow error = %v, want %v", err, errCounterOverflow)
			}
			return
		}
		if err != nil {
			t.Fatalf("readIOStatsFile on representable input returned error %v", err)
		}

		for _, check := range []struct {
			key   string
			total uint64
		}{
			{"rbytes", readBytes},
			{"wbytes", writeBytes},
			{"rios", readOps},
			{"wios", writeOps},
		} {
			if check.total != expected[check.key] {
				t.Fatalf("readIOStatsFile reported %s=%d, want the exact sum %d", check.key, check.total, expected[check.key])
			}
		}
	})
}

// exactIOStatSum recomputes one io.stat key independently of the reader under
// test and reports when the mathematical result cannot fit in uint64.
func exactIOStatSum(content, key string) (sum uint64, overflow bool) {
	for _, line := range strings.Split(content, "\n") {
		for _, field := range strings.Fields(strings.TrimSpace(line)) {
			pair := strings.SplitN(field, "=", 2)
			if len(pair) != 2 || pair[0] != key {
				continue
			}
			value, err := strconv.ParseUint(pair[1], 10, 64)
			if err != nil {
				continue
			}
			if sum > math.MaxUint64-value {
				overflow = true
				continue
			}
			sum += value
		}
	}
	return sum, overflow
}
