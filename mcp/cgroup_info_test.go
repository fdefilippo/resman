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
		name              string
		info              cgroup.CgroupInfo
		wantResult        GetCgroupInfoResult
		wantPayloadValues map[string]any
		absentPayloadKeys []string
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
			wantResult: GetCgroupInfoResult{
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
			wantPayloadValues: map[string]any{
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
			absentPayloadKeys: []string{
				"cpu_max_unavailable_reason",
				"cpu_weight_unavailable_reason",
				"memory_current_unavailable_reason",
				"memory_max_unavailable_reason",
				"memory_high_unavailable_reason",
			},
		},
		{
			name: "unavailable values carry bounded reasons",
			info: cgroup.CgroupInfo{
				Path: "/sys/fs/cgroup/resman/user_1001",
				CPUQuota: cgroup.CgroupFileValue{
					Value: "max 100000", Available: true, UnavailableReason: cgroup.CgroupFileReadError,
				},
				CPUWeight: cgroup.CgroupFileValue{
					Value: "stale", UnavailableReason: cgroup.CgroupFileNotPresent,
				},
				MemoryCurrent: cgroup.CgroupFileValue{
					Value: "stale", UnavailableReason: cgroup.CgroupFilePermissionDenied,
				},
				MemoryMax: cgroup.CgroupFileValue{
					Value: "max", Available: true, UnavailableReason: cgroup.CgroupFilePermissionDenied,
				},
				MemoryHigh: cgroup.CgroupFileValue{
					Value: "stale", UnavailableReason: cgroup.CgroupFileReadError,
				},
			},
			wantResult: GetCgroupInfoResult{
				Path:                           "/sys/fs/cgroup/resman/user_1001",
				CPUQuota:                       "max 100000",
				CPUQuotaAvailable:              true,
				CPUWeightUnavailableReason:     cgroup.CgroupFileNotPresent,
				MemoryCurrentUnavailableReason: cgroup.CgroupFilePermissionDenied,
				MemoryMax:                      "max",
				MemoryMaxAvailable:             true,
				MemoryHighUnavailableReason:    cgroup.CgroupFileReadError,
			},
			wantPayloadValues: map[string]any{
				"path":                              "/sys/fs/cgroup/resman/user_1001",
				"cpu_max":                           "max 100000",
				"cpu_max_available":                 true,
				"cpu_weight_available":              false,
				"cpu_weight_unavailable_reason":     "not_present",
				"memory_current_available":          false,
				"memory_current_unavailable_reason": "permission_denied",
				"memory_max":                        "max",
				"memory_max_available":              true,
				"memory_high_available":             false,
				"memory_high_unavailable_reason":    "read_error",
			},
			absentPayloadKeys: []string{
				"cpu_weight", "memory_current", "memory_high",
				"cpu_max_unavailable_reason", "memory_max_unavailable_reason",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := &Server{cgroupManager: cgroupInfoReaderStub{info: tt.info}}
			_, gotTool, err := server.handleGetCgroupInfo(context.Background(), nil, GetCgroupInfoArgs{UID: 1000})
			if err != nil {
				t.Fatalf("handleGetCgroupInfo() error = %v", err)
			}
			if !reflect.DeepEqual(gotTool, tt.wantResult) {
				t.Errorf("tool result = %#v, want %#v", gotTool, tt.wantResult)
			}
			assertCgroupPayload(t, "tool", gotTool, tt.wantPayloadValues, tt.absentPayloadKeys)

			gotResource, err := server.handleCgroupResource(context.Background(), &sdkmcp.ReadResourceRequest{
				Params: &sdkmcp.ReadResourceParams{URI: "resman://cgroups/1000"},
			})
			if err != nil {
				t.Fatalf("handleCgroupResource() error = %v", err)
			}
			assertCgroupPayload(t, "resource", json.RawMessage(gotResource.Contents[0].Text),
				tt.wantPayloadValues, tt.absentPayloadKeys)
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
