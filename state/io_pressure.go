package state

import (
	"fmt"
	"strings"

	"github.com/fdefilippo/resman/config"
)

type ioPressure struct {
	maxPercent         float64
	exceededDimensions []string
}

func (p ioPressure) exceeded() bool {
	return len(p.exceededDimensions) > 0
}

func (p ioPressure) reason(threshold int) string {
	return fmt.Sprintf(
		"IO %s >= %d%% (peak %.1f%%)",
		strings.Join(p.exceededDimensions, ", "),
		threshold,
		p.maxPercent,
	)
}

// evaluateIOPressure applies an any-of rule across configured I/O dimensions.
// Activation or maintenance requires any dimension at or above the supplied
// threshold. Consequently, release requires every configured dimension below
// its release threshold. Disabled dimensions are omitted independently.
func evaluateIOPressure(policy config.IODecisionPolicy, metrics *SystemMetrics, threshold int) ioPressure {
	pressure := ioPressure{}
	if !policy.Enabled || threshold <= 0 {
		return pressure
	}

	dimensions := []struct {
		name         string
		rate         float64
		perUserLimit float64
	}{
		{name: "read_bps", rate: metrics.IOEligibleReadBPS, perUserLimit: byteRateLimit(policy.ReadBPS)},
		{name: "write_bps", rate: metrics.IOEligibleWriteBPS, perUserLimit: byteRateLimit(policy.WriteBPS)},
		{name: "read_iops", rate: metrics.IOEligibleReadBlockIOPS, perUserLimit: float64(policy.ReadIOPS)},
		{name: "write_iops", rate: metrics.IOEligibleWriteBlockIOPS, perUserLimit: float64(policy.WriteIOPS)},
	}

	for _, dimension := range dimensions {
		if dimension.perUserLimit <= 0 {
			continue
		}
		percent := 0.0
		if metrics.IOEligibleUsersCount > 0 {
			totalLimit := dimension.perUserLimit * float64(metrics.IOEligibleUsersCount)
			percent = dimension.rate / totalLimit * 100
		}
		if percent > pressure.maxPercent {
			pressure.maxPercent = percent
		}
		if percent >= float64(threshold) {
			pressure.exceededDimensions = append(pressure.exceededDimensions, dimension.name)
		}
	}
	return pressure
}

func byteRateLimit(value string) float64 {
	if value == "" || value == "max" {
		return 0
	}
	limit, err := config.ParseByteQuota(value)
	if err != nil {
		return 0
	}
	return float64(limit)
}
