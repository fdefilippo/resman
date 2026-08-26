package mcp

import (
	"strconv"

	"github.com/fdefilippo/resman/cgroup"
)

func newCgroupInfoResult(info cgroup.CgroupInfo) GetCgroupInfoResult {
	return GetCgroupInfoResult{
		Path:                           info.Path,
		CPUQuota:                       availableCgroupValue(info.CPUQuota),
		CPUQuotaAvailable:              info.CPUQuota.Available,
		CPUQuotaUnavailableReason:      unavailableCgroupReason(info.CPUQuota),
		CPUWeight:                      availableCgroupValue(info.CPUWeight),
		CPUWeightAvailable:             info.CPUWeight.Available,
		CPUWeightUnavailableReason:     unavailableCgroupReason(info.CPUWeight),
		MemoryCurrent:                  availableCgroupValue(info.MemoryCurrent),
		MemoryCurrentAvailable:         info.MemoryCurrent.Available,
		MemoryCurrentUnavailableReason: unavailableCgroupReason(info.MemoryCurrent),
		MemoryMax:                      availableCgroupValue(info.MemoryMax),
		MemoryMaxAvailable:             info.MemoryMax.Available,
		MemoryMaxUnavailableReason:     unavailableCgroupReason(info.MemoryMax),
		MemoryHigh:                     availableCgroupValue(info.MemoryHigh),
		MemoryHighAvailable:            info.MemoryHigh.Available,
		MemoryHighUnavailableReason:    unavailableCgroupReason(info.MemoryHigh),
	}
}

func availableCgroupValue(value cgroup.CgroupFileValue) string {
	if !value.Available {
		return ""
	}
	return value.Value
}

func unavailableCgroupReason(value cgroup.CgroupFileValue) cgroup.CgroupFileUnavailableReason {
	if value.Available {
		return ""
	}
	return value.UnavailableReason
}

func extractCgroupMemoryMetrics(info cgroup.CgroupInfo) (uint64, bool, string, string) {
	currentValue := availableCgroupValue(info.MemoryCurrent)
	current, err := strconv.ParseUint(currentValue, 10, 64)
	return current, info.MemoryCurrent.Available && err == nil,
		availableCgroupValue(info.MemoryMax), availableCgroupValue(info.MemoryHigh)
}
