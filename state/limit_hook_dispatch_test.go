package state

import (
	"context"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/fdefilippo/resman/metrics"
)

// limitHookRecorder captures the events delivered to the script hook.
type limitHookRecorder struct {
	mu     sync.Mutex
	events []limitHookEvent
	signal chan struct{}
}

func newLimitHookRecorder() *limitHookRecorder {
	return &limitHookRecorder{signal: make(chan struct{}, 16)}
}

func (r *limitHookRecorder) execute(_ context.Context, _ hookScriptInvocation, event limitHookEvent) error {
	r.mu.Lock()
	r.events = append(r.events, event)
	r.mu.Unlock()
	r.signal <- struct{}{}
	return nil
}

func (r *limitHookRecorder) waitFor(t *testing.T, count int) []limitHookEvent {
	t.Helper()
	for received := 0; received < count; received++ {
		select {
		case <-r.signal:
		case <-time.After(5 * time.Second):
			t.Fatalf("limit hook deliveries = %d, want %d", received, count)
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]limitHookEvent(nil), r.events...)
}

func (r *limitHookRecorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.events)
}

func TestSystemdNativeActivationDispatchesOneLimitHookPerNewlyLimitedUser(t *testing.T) {
	policy := testCPUPointsPolicy(t, map[string]struct{ uid, points int }{"alice": {1000, 300}})
	adapter := &fakeSystemdCPUUnitAdapter{topology: testSystemdTopology(0, 1000, 1001)}
	manager := testSystemdCPUPointsManager(t, policy, adapter, &forbiddenSystemdNativeCgroupManager{}, 4)
	cfg := manager.cfg
	cfg.UserIncludeList = []string{"^alice$", "^bob$"}
	cfg.CPUThreshold = 2
	cfg.CPUReleaseThreshold = 1
	cfg.CPUThresholdDuration = 0
	cfg.IgnoreSystemLoad = true
	cfg.LimitHookEnabled = true
	cfg.LimitHookScript = "/test/hook"
	recorder := newLimitHookRecorder()
	manager.executeHookScript = recorder.execute
	t.Cleanup(manager.stopLimitHooks)

	collector := manager.metricsCollector.(*mockMetricsCollector)
	collector.allUserMetrics = map[int]*metrics.UserMetrics{
		1000: {UID: 1000, Username: "alice", CPUUsage: 300, CPUUsageEMA: 300, CPUUsageAverage: 300, ProcessCount: 3},
		1001: {UID: 1001, Username: "bob", CPUUsage: 10, CPUUsageEMA: 10, CPUUsageAverage: 10, ProcessCount: 1},
	}

	if err := manager.RunControlCycle(context.Background()); err != nil {
		t.Fatalf("activation cycle: %v", err)
	}
	if !manager.limitsActive {
		t.Fatal("activation cycle did not activate limits")
	}
	events := recorder.waitFor(t, 2)
	uids := make([]int, 0, len(events))
	for _, event := range events {
		uids = append(uids, event.UID)
		if event.Username == "" || event.LimitHookSource != "resman" {
			t.Fatalf("limit hook event = %+v, want a resolved username and source", event)
		}
	}
	sort.Ints(uids)
	if len(uids) != 2 || uids[0] != 1000 || uids[1] != 1001 {
		t.Fatalf("limit hook UIDs = %v, want the two newly limited eligible users [1000 1001]", uids)
	}

	// A maintained episode notifies nobody again.
	if err := manager.RunControlCycle(context.Background()); err != nil {
		t.Fatalf("maintain cycle: %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	if got := recorder.count(); got != 2 {
		t.Fatalf("limit hook deliveries after a maintained episode = %d, want 2", got)
	}
}

func TestSystemdNativeLimitHookIsSilentWhileHooksAreDisabled(t *testing.T) {
	policy := testCPUPointsPolicy(t, map[string]struct{ uid, points int }{"alice": {1000, 300}})
	adapter := &fakeSystemdCPUUnitAdapter{topology: testSystemdTopology(0, 1000)}
	manager := testSystemdCPUPointsManager(t, policy, adapter, &forbiddenSystemdNativeCgroupManager{}, 4)
	cfg := manager.cfg
	cfg.UserIncludeList = []string{"^alice$"}
	cfg.CPUThreshold = 2
	cfg.CPUReleaseThreshold = 1
	cfg.CPUThresholdDuration = 0
	cfg.IgnoreSystemLoad = true
	cfg.LimitHookScript = "/test/hook"
	recorder := newLimitHookRecorder()
	manager.executeHookScript = recorder.execute
	t.Cleanup(manager.stopLimitHooks)

	collector := manager.metricsCollector.(*mockMetricsCollector)
	collector.allUserMetrics = map[int]*metrics.UserMetrics{
		1000: {UID: 1000, Username: "alice", CPUUsage: 300, CPUUsageEMA: 300, CPUUsageAverage: 300, ProcessCount: 3},
	}
	if err := manager.RunControlCycle(context.Background()); err != nil {
		t.Fatalf("activation cycle: %v", err)
	}
	if !manager.limitsActive {
		t.Fatal("activation cycle did not activate limits")
	}
	time.Sleep(50 * time.Millisecond)
	if got := recorder.count(); got != 0 {
		t.Fatalf("limit hook deliveries with LIMIT_HOOK_ENABLED=false = %d, want 0", got)
	}
}
