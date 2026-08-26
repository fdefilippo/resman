package mcp

import (
	"strconv"

	"github.com/fdefilippo/resman/cgroup"
)

type cgroupResourcePayload struct {
	Path                   string `json:"path"`
	CPUQuota               string `json:"cpu.max,omitempty"`
	CPUQuotaAvailable      bool   `json:"cpu_max_available"`
	CPUWeight              string `json:"cpu.weight,omitempty"`
	CPUWeightAvailable     bool   `json:"cpu_weight_available"`
	MemoryCurrent          string `json:"memory.current,omitempty"`
	MemoryCurrentAvailable bool   `json:"memory_current_available"`
	MemoryMax              string `json:"memory.max,omitempty"`
	MemoryMaxAvailable     bool   `json:"memory_max_available"`
	MemoryHigh             string `json:"memory.high,omitempty"`
	MemoryHighAvailable    bool   `json:"memory_high_available"`
}

func newCgroupInfoResult(info cgroup.CgroupInfo) GetCgroupInfoResult {
	return GetCgroupInfoResult{
		Path:                   info.Path,
		CPUQuota:               availableCgroupValue(info.CPUQuota),
		CPUQuotaAvailable:      info.CPUQuota.Available,
		CPUWeight:              availableCgroupValue(info.CPUWeight),
		CPUWeightAvailable:     info.CPUWeight.Available,
		MemoryCurrent:          availableCgroupValue(info.MemoryCurrent),
		MemoryCurrentAvailable: info.MemoryCurrent.Available,
		MemoryMax:              availableCgroupValue(info.MemoryMax),
		MemoryMaxAvailable:     info.MemoryMax.Available,
		MemoryHigh:             availableCgroupValue(info.MemoryHigh),
		MemoryHighAvailable:    info.MemoryHigh.Available,
	}
}

func newCgroupResourcePayload(info cgroup.CgroupInfo) cgroupResourcePayload {
	return cgroupResourcePayload{
		Path:                   info.Path,
		CPUQuota:               availableCgroupValue(info.CPUQuota),
		CPUQuotaAvailable:      info.CPUQuota.Available,
		CPUWeight:              availableCgroupValue(info.CPUWeight),
		CPUWeightAvailable:     info.CPUWeight.Available,
		MemoryCurrent:          availableCgroupValue(info.MemoryCurrent),
		MemoryCurrentAvailable: info.MemoryCurrent.Available,
		MemoryMax:              availableCgroupValue(info.MemoryMax),
		MemoryMaxAvailable:     info.MemoryMax.Available,
		MemoryHigh:             availableCgroupValue(info.MemoryHigh),
		MemoryHighAvailable:    info.MemoryHigh.Available,
	}
}

func availableCgroupValue(value cgroup.CgroupFileValue) string {
	if !value.Available {
		return ""
	}
	return value.Value
}

func extractCgroupMemoryMetrics(info cgroup.CgroupInfo) (uint64, bool, string, string) {
	currentValue := availableCgroupValue(info.MemoryCurrent)
	current, err := strconv.ParseUint(currentValue, 10, 64)
	return current, info.MemoryCurrent.Available && err == nil,
		availableCgroupValue(info.MemoryMax), availableCgroupValue(info.MemoryHigh)
}
