package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/fdefilippo/resman/cgroup"
)

type cgroupInfoReaderStub struct {
	info cgroup.CgroupInfo
	err  error
}

func (s cgroupInfoReaderStub) GetCgroupInfo(int) (cgroup.CgroupInfo, error) {
	return s.info, s.err
}

func (cgroupInfoReaderStub) GetMemoryHighEvents(int) (uint64, error) {
	return 0, errors.New("not implemented by cgroup info test stub")
}

func (cgroupInfoReaderStub) GetIOStats(int) (uint64, uint64, uint64, uint64, error) {
	return 0, 0, 0, 0, errors.New("not implemented by cgroup info test stub")
}

func TestTypedCgroupInfoContractAcrossMCPSurfaces(t *testing.T) {
	tests := []struct {
		name               string
		info               cgroup.CgroupInfo
		wantTool           GetCgroupInfoResult
		wantToolValues     map[string]any
		absentToolKeys     []string
		wantResourceValues map[string]any
		absentResourceKeys []string
	}{
		{
			name: "all interfaces available",
			info: cgroup.CgroupInfo{
				Path:          "/sys/fs/cgroup/resman/user_1000",
				CPUQuota:      cgroup.CgroupFileValue{Value: "50000 100000", Available: true},
				CPUWeight:     cgroup.CgroupFileValue{Value: "100", Available: true},
				MemoryCurrent: cgroup.CgroupFileValue{Value: "1048576", Available: true},
				MemoryMax:     cgroup.CgroupFileValue{Value: "max", Available: true},
				MemoryHigh:    cgroup.CgroupFileValue{Value: "2097152", Available: true},
			},
			wantTool: GetCgroupInfoResult{
				Path:                   "/sys/fs/cgroup/resman/user_1000",
				CPUQuota:               "50000 100000",
				CPUQuotaAvailable:      true,
				CPUWeight:              "100",
				CPUWeightAvailable:     true,
				MemoryCurrent:          "1048576",
				MemoryCurrentAvailable: true,
				MemoryMax:              "max",
				MemoryMaxAvailable:     true,
				MemoryHigh:             "2097152",
				MemoryHighAvailable:    true,
			},
			wantToolValues: map[string]any{
				"path":                     "/sys/fs/cgroup/resman/user_1000",
				"cpu_max":                  "50000 100000",
				"cpu_max_available":        true,
				"cpu_weight":               "100",
				"cpu_weight_available":     true,
				"memory_current":           "1048576",
				"memory_current_available": true,
				"memory_max":               "max",
				"memory_max_available":     true,
				"memory_high":              "2097152",
				"memory_high_available":    true,
			},
			wantResourceValues: map[string]any{
				"path":                     "/sys/fs/cgroup/resman/user_1000",
				"cpu.max":                  "50000 100000",
				"cpu_max_available":        true,
				"cpu.weight":               "100",
				"cpu_weight_available":     true,
				"memory.current":           "1048576",
				"memory_current_available": true,
				"memory.max":               "max",
				"memory_max_available":     true,
				"memory.high":              "2097152",
				"memory_high_available":    true,
			},
		},
		{
			name: "unavailable values remain absent",
			info: cgroup.CgroupInfo{
				Path:          "/sys/fs/cgroup/resman/user_1001",
				CPUQuota:      cgroup.CgroupFileValue{Value: "max 100000", Available: true},
				CPUWeight:     cgroup.CgroupFileValue{Value: "stale", Available: false},
				MemoryCurrent: cgroup.CgroupFileValue{Value: "stale", Available: false},
				MemoryMax:     cgroup.CgroupFileValue{Value: "max", Available: true},
				MemoryHigh:    cgroup.CgroupFileValue{Value: "stale", Available: false},
			},
			wantTool: GetCgroupInfoResult{
				Path:               "/sys/fs/cgroup/resman/user_1001",
				CPUQuota:           "max 100000",
				CPUQuotaAvailable:  true,
				MemoryMax:          "max",
				MemoryMaxAvailable: true,
			},
			wantToolValues: map[string]any{
				"path":                     "/sys/fs/cgroup/resman/user_1001",
				"cpu_max":                  "max 100000",
				"cpu_max_available":        true,
				"cpu_weight_available":     false,
				"memory_current_available": false,
				"memory_max":               "max",
				"memory_max_available":     true,
				"memory_high_available":    false,
			},
			absentToolKeys: []string{"cpu_weight", "memory_current", "memory_high"},
			wantResourceValues: map[string]any{
				"path":                     "/sys/fs/cgroup/resman/user_1001",
				"cpu.max":                  "max 100000",
				"cpu_max_available":        true,
				"cpu_weight_available":     false,
				"memory_current_available": false,
				"memory.max":               "max",
				"memory_max_available":     true,
				"memory_high_available":    false,
			},
			absentResourceKeys: []string{"cpu.weight", "memory.current", "memory.high"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := &Server{cgroupManager: cgroupInfoReaderStub{info: tt.info}}
			_, gotTool, err := server.handleGetCgroupInfo(context.Background(), nil, GetCgroupInfoArgs{UID: 1000})
			if err != nil {
				t.Fatalf("handleGetCgroupInfo() error = %v", err)
			}
			if !reflect.DeepEqual(gotTool, tt.wantTool) {
				t.Errorf("tool result = %#v, want %#v", gotTool, tt.wantTool)
			}
			assertCgroupPayload(t, "tool", gotTool, tt.wantToolValues, tt.absentToolKeys)

			gotResource, err := server.handleCgroupResource(context.Background(), &sdkmcp.ReadResourceRequest{
				Params: &sdkmcp.ReadResourceParams{URI: "resman://cgroups/1000"},
			})
			if err != nil {
				t.Fatalf("handleCgroupResource() error = %v", err)
			}
			assertCgroupPayload(t, "resource", json.RawMessage(gotResource.Contents[0].Text),
				tt.wantResourceValues, tt.absentResourceKeys)
		})
	}
}

func assertCgroupPayload(t *testing.T, surface string, value any, wantValues map[string]any, absentKeys []string) {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("encode %s payload: %v", surface, err)
	}
	var payload map[string]any
	if err := json.Unmarshal(encoded, &payload); err != nil {
		t.Fatalf("decode %s payload: %v", surface, err)
	}
	for key, want := range wantValues {
		if got := payload[key]; got != want {
			t.Errorf("%s %q = %#v, want %#v", surface, key, got, want)
		}
	}
	for _, key := range absentKeys {
		if got, exists := payload[key]; exists {
			t.Errorf("%s contains unavailable %q = %#v", surface, key, got)
		}
	}
}
