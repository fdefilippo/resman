package state

import (
	"fmt"
	"math/big"
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
	return evaluateIOBounds(policy, metrics, threshold, false)
}

func evaluateIOReleasePressure(policy config.IODecisionPolicy, metrics *SystemMetrics, threshold int) ioPressure {
	return evaluateIOBounds(policy, metrics, threshold, true)
}

func evaluateIOBounds(policy config.IODecisionPolicy, metrics *SystemMetrics, threshold int, release bool) ioPressure {
	pressure := ioPressure{}
	if !policy.Enabled || threshold <= 0 {
		return pressure
	}

	dimensions := []struct {
		name         string
		rate         float64
		perUserLimit float64
		iopsQuota    int
	}{
		{name: "read_bps", rate: metrics.IOEligibleReadBPS, perUserLimit: byteRateLimit(policy.ReadBPS)},
		{name: "write_bps", rate: metrics.IOEligibleWriteBPS, perUserLimit: byteRateLimit(policy.WriteBPS)},
		{name: "read_iops", rate: metrics.IOEligibleReadBlockIOPS, perUserLimit: float64(policy.ReadIOPS)},
		{name: "write_iops", rate: metrics.IOEligibleWriteBlockIOPS, perUserLimit: float64(policy.WriteIOPS)},
	}

	if metrics.IOBlockIOPSDevices != nil {
		dimensions = dimensions[:2]
		for device, bounds := range metrics.IOBlockIOPSDevices {
			read, write := bounds.Read.ActivationMinimum, bounds.Write.ActivationMinimum
			if release {
				read, write = bounds.Read.ReleaseMaximum, bounds.Write.ReleaseMaximum
			}
			dimensions = append(dimensions, struct {
				name         string
				rate         float64
				perUserLimit float64
				iopsQuota    int
			}{fmt.Sprintf("read_iops[%d:%d]", device.Major, device.Minor), read, float64(policy.ReadIOPS), policy.ReadIOPS}, struct {
				name         string
				rate         float64
				perUserLimit float64
				iopsQuota    int
			}{fmt.Sprintf("write_iops[%d:%d]", device.Major, device.Minor), write, float64(policy.WriteIOPS), policy.WriteIOPS})
		}
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
		exceeded := percent >= float64(threshold)
		if dimension.iopsQuota > 0 {
			exceeded = blockIOPSAtOrAbove(dimension.rate, dimension.iopsQuota, metrics.IOEligibleUsersCount, threshold)
		}
		if exceeded {
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

// blockIOPSAtOrAbove compares a finite certified rate against the exact integer
// quota ratio. Floating-point percentages are for diagnostics only: rounding a
// large quota or user count must not reverse an activation/release proof.
func blockIOPSAtOrAbove(rate float64, quota, users, threshold int) bool {
	if quota <= 0 || users <= 0 || threshold <= 0 {
		return false
	}
	target := new(big.Int).SetInt64(int64(quota))
	target.Mul(target, big.NewInt(int64(users)))
	target.Mul(target, big.NewInt(int64(threshold)))
	actual := new(big.Rat).SetFloat64(rate)
	actual.Mul(actual, big.NewRat(100, 1))
	return actual.Cmp(new(big.Rat).SetInt(target)) >= 0
}
