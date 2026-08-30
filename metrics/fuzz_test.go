package metrics

import (
	"strconv"
	"strings"
	"testing"
)

// FuzzParseCPUQuota exercises the cpu.max reader that feeds the exported
// resman_cgroup_cpu_quota and resman_cgroup_cpu_period series. The function
// already owns a sentinel for "no usable value" (-1, which the caller skips
// with quota >= 0), so any field it cannot parse must carry that sentinel
// rather than a value a dashboard would read as a real limit.
func FuzzParseCPUQuota(f *testing.F) {
	for _, seed := range []string{
		"max 100000",
		"100000 100000",
		"50000 100000",
		"",
		"max",
		"abc 100000",
		"100000 abc",
		"abc def",
		"max max",
		"-5 100000",
		"0 100000",
		"99999999999999999999 100000",
		"100000 0",
		"100000 -1",
		"  100000   100000  ",
		"100000 100000 100000",
	} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, value string) {
		quota, period := parseCPUQuota(value)

		fields := strings.Fields(value)
		if len(fields) != 2 {
			if quota != -1 || period != -1 {
				t.Fatalf("parseCPUQuota(%q) = (%d, %d), want the (-1, -1) sentinel for a malformed pair", value, quota, period)
			}
			return
		}

		parsedPeriod, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil || parsedPeriod <= 0 {
			if quota != -1 || period != -1 {
				t.Fatalf("parseCPUQuota(%q) = (%d, %d), want the atomic (-1, -1) sentinel", value, quota, period)
			}
			return
		}

		if fields[0] == "max" {
			if quota != -1 || period != parsedPeriod {
				t.Fatalf("parseCPUQuota(%q) = (%d, %d), want (-1, %d)", value, quota, period, parsedPeriod)
			}
			return
		}
		parsedQuota, err := strconv.ParseInt(fields[0], 10, 64)
		if err != nil || parsedQuota <= 0 {
			if quota != -1 || period != -1 {
				t.Fatalf("parseCPUQuota(%q) = (%d, %d), want the atomic (-1, -1) sentinel", value, quota, period)
			}
			return
		}
		if quota != parsedQuota || period != parsedPeriod {
			t.Fatalf("parseCPUQuota(%q) = (%d, %d), want (%d, %d)", value, quota, period, parsedQuota, parsedPeriod)
		}
	})
}
