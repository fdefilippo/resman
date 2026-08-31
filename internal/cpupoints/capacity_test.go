package cpupoints

import (
	"errors"
	"fmt"
	"os"
	"sync/atomic"
	"testing"
)

func TestParseOnlineCPUList(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  uint64
		ok    bool
	}{
		{name: "singleton", value: "0\n", want: 1, ok: true},
		{name: "range", value: "0-7\n", want: 8, ok: true},
		{name: "disjoint ranges", value: "0-3,8-11", want: 8, ok: true},
		{name: "large identifiers without expansion", value: "1000000-1000009", want: 10, ok: true},
		{name: "empty", value: "", ok: false},
		{name: "embedded whitespace", value: "0-3, 8-11", ok: false},
		{name: "empty segment", value: "0-3,,8", ok: false},
		{name: "leading zero", value: "00-3", ok: false},
		{name: "signed", value: "+0-3", ok: false},
		{name: "descending", value: "3-0", ok: false},
		{name: "overlap", value: "0-3,3-4", ok: false},
		{name: "duplicate", value: "0,0", ok: false},
		{name: "unordered", value: "4-5,0-1", ok: false},
		{name: "multiple separators", value: "0-1-2", ok: false},
		{name: "number overflow", value: "18446744073709551616", ok: false},
		{name: "count overflow", value: "0-18446744073709551615", ok: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseOnlineCPUList(tt.value)
			if tt.ok {
				if err != nil {
					t.Fatalf("ParseOnlineCPUList(%q): %v", tt.value, err)
				}
				if got.Value() != tt.want {
					t.Errorf("count = %d, want %d", got.Value(), tt.want)
				}
				return
			}
			if err == nil {
				t.Fatalf("ParseOnlineCPUList(%q) accepted count %d", tt.value, got.Value())
			}
		})
	}
}

func TestSysfsOnlineCPUSourceClassifiesReadAndParseFailures(t *testing.T) {
	tests := []struct {
		name    string
		data    string
		readErr error
		want    CapacityUnavailableReason
	}{
		{name: "read", readErr: os.ErrPermission, want: CapacityReadError},
		{name: "parse", data: "0-3,broken", want: CapacityParseError},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			source := SysfsOnlineCPUSource{readFile: func(path string) ([]byte, error) {
				if path != OnlineCPUListPath {
					t.Fatalf("path = %q, want %q", path, OnlineCPUListPath)
				}
				return []byte(tt.data), tt.readErr
			}}
			_, err := source.OnlineCPUs()
			var sourceErr *CapacitySourceError
			if !errors.As(err, &sourceErr) {
				t.Fatalf("error = %v, want CapacitySourceError", err)
			}
			if sourceErr.Reason() != tt.want {
				t.Errorf("reason = %q, want %q", sourceErr.Reason(), tt.want)
			}
		})
	}
}

func TestLiveCapacityProviderRequiresTrustworthyInitialRead(t *testing.T) {
	pool, _ := NewParentPoolPoints(900)
	source := OnlineCPUSourceFunc(func() (OnlineCPUCount, error) {
		return OnlineCPUCount{}, &CapacitySourceError{reason: CapacityReadError, err: os.ErrNotExist}
	})
	if _, err := NewLiveCapacityProvider(source, pool); err == nil {
		t.Fatal("provider started without a trustworthy denominator")
	}
}

func TestLiveCapacityProviderSeesTopologyChangesWithoutObservationCache(t *testing.T) {
	pool, _ := NewParentPoolPoints(900)
	var cpus atomic.Uint64
	cpus.Store(2)
	source := OnlineCPUSourceFunc(func() (OnlineCPUCount, error) {
		return NewOnlineCPUCount(cpus.Load())
	})
	provider, err := NewLiveCapacityProvider(source, pool)
	if err != nil {
		t.Fatal(err)
	}
	if got := provider.State().LastVerified.QuotaMicroseconds(); got != 180000 {
		t.Fatalf("initial quota = %d, want 180000", got)
	}

	cpus.Store(4)
	state, err := provider.Refresh(pool)
	if err != nil {
		t.Fatal(err)
	}
	if got := state.LastVerified.QuotaMicroseconds(); got != 360000 {
		t.Errorf("refreshed quota = %d, want 360000", got)
	}
	if got := state.LastVerified.OnlineCPUs().Value(); got != 4 {
		t.Errorf("refreshed denominator = %d, want 4", got)
	}
}

func TestLiveCapacityProviderRetainsExactPlanAcrossFailureAndRetries(t *testing.T) {
	pool, _ := NewParentPoolPoints(900)
	var call atomic.Int32
	source := OnlineCPUSourceFunc(func() (OnlineCPUCount, error) {
		switch call.Add(1) {
		case 1:
			return NewOnlineCPUCount(2)
		case 2:
			return OnlineCPUCount{}, &CapacitySourceError{reason: CapacityParseError, err: errors.New("truncated range")}
		default:
			return NewOnlineCPUCount(3)
		}
	})
	provider, err := NewLiveCapacityProvider(source, pool)
	if err != nil {
		t.Fatal(err)
	}

	failed, err := provider.Refresh(pool)
	if err == nil {
		t.Fatal("runtime source failure returned nil")
	}
	if failed.Available {
		t.Fatal("failed refresh remained available")
	}
	if failed.UnavailableReason != CapacityParseError {
		t.Errorf("reason = %q, want %q", failed.UnavailableReason, CapacityParseError)
	}
	if got := failed.LastVerified.OnlineCPUs().Value(); got != 2 {
		t.Errorf("retained denominator = %d, want 2", got)
	}
	if got := failed.LastVerified.QuotaMicroseconds(); got != 180000 {
		t.Errorf("retained quota = %d, want 180000", got)
	}

	recovered, err := provider.Refresh(pool)
	if err != nil {
		t.Fatalf("retry failed: %v", err)
	}
	if !recovered.Available {
		t.Fatal("successful retry remained unavailable")
	}
	if got := recovered.LastVerified.OnlineCPUs().Value(); got != 3 {
		t.Errorf("recovered denominator = %d, want 3", got)
	}
}

func TestLiveCapacityProviderDoesNotPublishAnOlderConcurrentRead(t *testing.T) {
	pool, _ := NewParentPoolPoints(900)
	firstRefreshStarted := make(chan struct{})
	releaseFirstRefresh := make(chan struct{})
	var call atomic.Int32
	source := OnlineCPUSourceFunc(func() (OnlineCPUCount, error) {
		switch call.Add(1) {
		case 1:
			return NewOnlineCPUCount(1)
		case 2:
			close(firstRefreshStarted)
			<-releaseFirstRefresh
			return NewOnlineCPUCount(2)
		case 3:
			return NewOnlineCPUCount(4)
		default:
			return OnlineCPUCount{}, fmt.Errorf("unexpected source call")
		}
	})
	provider, err := NewLiveCapacityProvider(source, pool)
	if err != nil {
		t.Fatal(err)
	}

	firstDone := make(chan error, 1)
	go func() {
		_, refreshErr := provider.Refresh(pool)
		firstDone <- refreshErr
	}()
	<-firstRefreshStarted
	newer, err := provider.Refresh(pool)
	if err != nil {
		t.Fatal(err)
	}
	if got := newer.LastVerified.OnlineCPUs().Value(); got != 4 {
		t.Fatalf("newer denominator = %d, want 4", got)
	}
	close(releaseFirstRefresh)
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
	if got := provider.State().LastVerified.OnlineCPUs().Value(); got != 4 {
		t.Errorf("older concurrent read replaced denominator with %d", got)
	}
}

func FuzzParseOnlineCPUList(f *testing.F) {
	for _, seed := range []string{"0", "0-7", "0-3,8-11", "", "0-3,,8", "0-18446744073709551615"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, value string) {
		count, err := ParseOnlineCPUList(value)
		if err == nil && count.Value() == 0 {
			t.Fatal("successful parse returned zero online CPUs")
		}
	})
}
