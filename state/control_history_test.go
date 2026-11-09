package state

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/fdefilippo/resman/config"
	"github.com/fdefilippo/resman/database"
	"github.com/fdefilippo/resman/internal/controlhistory"
	"github.com/fdefilippo/resman/metrics"
)

type historyCollector struct {
	mockMetricsCollector
	writer *metrics.DBWriter
}

func (c *historyCollector) GetDBWriter() *metrics.DBWriter { return c.writer }

func historyManager(t *testing.T, cfg *config.Config, collector *historyCollector) *Manager {
	t.Helper()
	m, err := NewManager(cfg, collector, &moveResultCgroupManager{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestTerminalHistoryPersistsAcrossManagersAndRetriesWithoutDuplicate(t *testing.T) {
	base := t.TempDir()
	if err := os.Chmod(base, 0700); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(base, "private")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	db, err := database.NewDatabaseManager(filepath.Join(dir, "history.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			t.Errorf("close resource: %v", err)
		}
	}()
	writer := metrics.NewDBWriter(db, 30)
	collector := &historyCollector{writer: writer}
	cfg := config.DefaultConfig()
	cfg.MetricsDBEnabled = true
	m := historyManager(t, cfg, collector)
	if err := m.RunControlCycleWithTrigger(t.Context(), ControlCycleTriggerInitial); err != nil {
		t.Fatal(err)
	}
	writer.SetEnabled(false)
	if err := m.RunControlCycle(t.Context()); err == nil {
		t.Fatal("unavailable sink failure hidden")
	}
	result := m.QueryControlHistory(t.Context(), 100)
	if result.Persistence != controlhistory.PersistenceUnavailable || result.Pending != 1 || len(result.Entries) != 2 || result.Entries[0].Trigger != ControlCycleTriggerInitial || !result.Entries[0].SampleAvailable || !result.Entries[0].HostCPUUsageAvailable {
		t.Fatalf("unavailable result=%+v", result)
	}
	writer.SetEnabled(true)
	if err := m.RunControlCycle(t.Context()); err != nil {
		t.Fatal(err)
	}
	result = m.QueryControlHistory(t.Context(), 100)
	if result.Persistence != controlhistory.PersistenceAvailable || result.Pending != 0 || len(result.Entries) != 3 {
		t.Fatalf("recovered result=%+v", result)
	}
	next := historyManager(t, cfg, collector)
	if err := next.RunControlCycle(t.Context()); err != nil {
		t.Fatal(err)
	}
	result = next.QueryControlHistory(t.Context(), 100)
	if len(result.Entries) != 4 || m.historySessionID == next.historySessionID {
		t.Fatalf("restart result=%+v", result)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := next.RunControlCycle(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel error=%v", err)
	}
	result = next.QueryControlHistory(t.Context(), 1)
	if len(result.Entries) != 1 || result.Entries[0].Outcome != controlhistory.Cancelled || result.Entries[0].SampleAvailable || result.Entries[0].ExecutionAttempted {
		t.Fatalf("cancelled attempt=%+v", result)
	}
}

func TestTerminalHistoryClassifiesUnknownSampleAndBoundedRetryLoss(t *testing.T) {
	for _, tt := range []struct {
		name      string
		err       error
		suspended bool
		want      controlhistory.Outcome
	}{
		{"collection failed", errors.New("sensitive source error"), false, controlhistory.Failed},
		{"blackout", nil, true, controlhistory.Suspended},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg := config.DefaultConfig()
			m := historyManager(t, cfg, &historyCollector{})
			run := &controlCycleContext{cfg: cfg, ctx: t.Context(), cycleID: 1, startTime: time.Now(), stopWithoutError: tt.suspended}
			if err := m.finishControlCycle(run, tt.err); err != nil {
				t.Fatal(err)
			}
			got := m.QueryControlHistory(t.Context(), 1)
			if got.Persistence != controlhistory.PersistenceDisabled || len(got.Entries) != 1 || got.Entries[0].Outcome != tt.want || got.Entries[0].SampleAvailable {
				t.Fatalf("result=%+v", got)
			}
			if got.Entries[0].Reason != "" {
				t.Fatal("raw failure copied into policy reason")
			}
		})
	}
	cfg := config.DefaultConfig()
	cfg.MetricsDBEnabled = true
	m := historyManager(t, cfg, &historyCollector{})
	for i := int64(1); i <= 102; i++ {
		run := &controlCycleContext{cfg: cfg, ctx: t.Context(), cycleID: i, startTime: time.Now()}
		if err := m.finishControlCycle(run, nil); err == nil {
			t.Fatal("missing writer accepted")
		}
	}
	got := m.QueryControlHistory(t.Context(), 1000)
	if got.Pending != 100 || got.Dropped != 2 || len(got.Entries) != 100 {
		t.Fatalf("bounded result: pending=%d dropped=%d entries=%d", got.Pending, got.Dropped, len(got.Entries))
	}
}

func TestTerminalHistoryRetainsHostCPUSourceAvailability(t *testing.T) {
	cfg := config.DefaultConfig()
	m := historyManager(t, cfg, &historyCollector{})
	for _, available := range []bool{false, true} {
		run := &controlCycleContext{cfg: cfg, ctx: t.Context(), cycleID: 1, startTime: time.Now(), metrics: &SystemMetrics{HostCPUUsageAvailable: available}}
		if !available {
			run.metrics.HostCPUUsageUnavailableReason = metrics.HostCPUUsageUnavailableRead
		}
		if err := m.finishControlCycle(run, nil); err != nil {
			t.Fatal(err)
		}
		entries := m.GetControlHistory(1)
		if len(entries) != 1 || !entries[0].SampleAvailable || entries[0].HostCPUUsageAvailable != available || entries[0].HostCPUUnavailableReason != string(run.metrics.HostCPUUsageUnavailableReason) {
			t.Fatalf("host availability=%v history=%+v", available, entries)
		}
	}
}
